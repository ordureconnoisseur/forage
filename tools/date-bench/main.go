// date-bench measures how reliably internal/matcher.TopDate recovers
// the ground-truth scene date from filenames / paths.
//
// Run from the forager repo root:
//
//	FORAGER_STASH_URL=... FORAGER_STASH_API_KEY=... \
//	FORAGER_STASHDB_API_KEY=... \
//	  go run ./tools/date-bench --limit=2000
//
// Reuses internal/config, internal/db (for boot env), and pulls
// labeled scenes directly via internal/stash.
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/matcher"
	"github.com/ordureconnoisseur/forager/internal/stash"
)

func main() {
	var (
		limit       = flag.Int("limit", 2000, "max labeled scenes to fetch (0 = all)")
		outputDir   = flag.String("output-dir", ".", "where to write per-mode *.failures.csv files")
		maxFailures = flag.Int("max-failures", 0, "cap rows written to each CSV (0 = no cap)")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	stashClient := stash.New(cfg.StashURL, cfg.StashAPIKey)
	log.Info("fetching labeled scenes", "limit", *limit)
	t0 := time.Now()
	scenes, err := stashClient.FindLabeledScenes(ctx, *limit)
	if err != nil {
		log.Error("find scenes", "err", err)
		os.Exit(1)
	}
	log.Info("fetched scenes", "count", len(scenes), "elapsed", time.Since(t0))

	if err := os.MkdirAll(*outputDir, 0o755); err != nil {
		log.Error("mkdir output", "err", err)
		os.Exit(1)
	}

	modes := []struct {
		Name     string
		Haystack func(stash.LabeledScene) string
	}{
		{"basename", func(s stash.LabeledScene) string { return s.Basename }},
		{"basename_folder", func(s stash.LabeledScene) string {
			parts := append([]string{s.Basename}, s.Folders...)
			return strings.Join(parts, " ")
		}},
	}

	results := make([]*dateResult, 0, len(modes))
	for _, m := range modes {
		log.Info("running mode", "name", m.Name)
		t0 := time.Now()
		r := scoreMode(m.Name, scenes, m.Haystack)
		r.Elapsed = time.Since(t0)
		results = append(results, r)

		csvPath := filepath.Join(*outputDir, m.Name+".date.failures.csv")
		if err := writeFailures(csvPath, r.Failures, *maxFailures); err != nil {
			log.Error("write csv", "mode", m.Name, "err", err)
			os.Exit(1)
		}
		log.Info("mode done",
			"name", m.Name,
			"scenes_with_gt", r.ScenesWithGT,
			"extracted", r.Extracted,
			"exact", r.Exact,
			"within_1d", r.Within1d,
			"wrong", r.Wrong,
			"missed", r.Missed,
			"csv", csvPath,
			"elapsed", r.Elapsed,
		)
	}

	printSummary(os.Stdout, results)
}

type dateResult struct {
	Mode         string
	ScenesWithGT int
	Extracted    int // scenes where TopDate returned non-empty
	Exact        int // extracted date == GT date
	Within1d     int // extracted within ±1 day of GT (date-only comparison)
	Wrong        int // extracted but >1 day off
	Missed       int // GT present but no extraction (recall failure)
	Failures     []dateFailure
	Elapsed      time.Duration
}

type dateFailure struct {
	SceneID   string
	Input     string
	GT        string
	Predicted string
	AllHits   string
	Kind      string // "missed" | "wrong"
}

func scoreMode(name string, scenes []stash.LabeledScene, haystackFn func(stash.LabeledScene) string) *dateResult {
	r := &dateResult{Mode: name}
	for _, sc := range scenes {
		if sc.Date == "" {
			continue
		}
		r.ScenesWithGT++

		input := haystackFn(sc)
		hits := matcher.ExtractDates(input)
		predicted := matcher.TopDate(input)

		if predicted == "" {
			r.Missed++
			r.Failures = append(r.Failures, dateFailure{
				SceneID:   sc.ID,
				Input:     input,
				GT:        sc.Date,
				Predicted: "",
				AllHits:   "",
				Kind:      "missed",
			})
			continue
		}
		r.Extracted++

		switch dateDelta(predicted, sc.Date) {
		case 0:
			r.Exact++
		case 1:
			r.Within1d++
		default:
			r.Wrong++
			all := make([]string, 0, len(hits))
			for _, h := range hits {
				all = append(all, h.Date+"("+h.Format+")")
			}
			r.Failures = append(r.Failures, dateFailure{
				SceneID:   sc.ID,
				Input:     input,
				GT:        sc.Date,
				Predicted: predicted,
				AllHits:   strings.Join(all, ", "),
				Kind:      "wrong",
			})
		}
	}
	return r
}

// dateDelta returns abs-difference in days between two YYYY-MM-DD
// strings, or -1 if either is unparseable. Used to bucket near-miss
// extractions (off-by-one is usually a timezone artefact between Stash
// and the release-date convention).
func dateDelta(a, b string) int {
	ta, err1 := time.Parse("2006-01-02", a)
	tb, err2 := time.Parse("2006-01-02", b)
	if err1 != nil || err2 != nil {
		return -1
	}
	d := ta.Sub(tb).Hours() / 24
	if d < 0 {
		d = -d
	}
	return int(d + 0.5)
}

func writeFailures(path string, rows []dateFailure, cap int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{"scene_id", "kind", "gt", "predicted", "all_hits", "input"}); err != nil {
		return err
	}
	n := len(rows)
	if cap > 0 && cap < n {
		n = cap
	}
	for i := 0; i < n; i++ {
		r := rows[i]
		if err := w.Write([]string{r.SceneID, r.Kind, r.GT, r.Predicted, r.AllHits, r.Input}); err != nil {
			return err
		}
	}
	return nil
}

func printSummary(w *os.File, results []*dateResult) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "mode\tw_gt\textracted\texact\twithin_1d\twrong\tmissed\trecall\tprecision\telapsed")
	for _, r := range results {
		recall := 0.0
		if r.ScenesWithGT > 0 {
			recall = float64(r.Exact+r.Within1d) / float64(r.ScenesWithGT)
		}
		precision := 0.0
		if r.Extracted > 0 {
			precision = float64(r.Exact+r.Within1d) / float64(r.Extracted)
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%.3f\t%.3f\t%s\n",
			r.Mode, r.ScenesWithGT, r.Extracted, r.Exact, r.Within1d, r.Wrong, r.Missed,
			recall, precision, r.Elapsed.Truncate(time.Millisecond))
	}
	tw.Flush()
}
