// studio-bench measures how reliably internal/matcher.NewScanner
// recovers the ground-truth studio from filenames / paths.
//
// Each labeled scene has at most one studio in StashDB; we count a hit
// when the scanner's output set contains that studio's stashdb_id.
//
// Run from the forager repo root (daemon must have populated forager.db):
//
//	FORAGER_STASH_URL=... FORAGER_STASH_API_KEY=... \
//	FORAGER_STASHDB_API_KEY=... \
//	  go run ./tools/studio-bench --limit=2000
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
	"github.com/ordureconnoisseur/forager/internal/db"
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

	database, err := db.Open(cfg.DBPath)
	if err != nil {
		log.Error("open db", "err", err, "path", cfg.DBPath)
		os.Exit(1)
	}
	defer database.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	corpus, err := matcher.LoadStudios(ctx, database)
	if err != nil {
		log.Error("load studio corpus", "err", err)
		os.Exit(1)
	}
	if len(corpus) == 0 {
		log.Error("studio_cache is empty — run the daemon and POST /refresh first")
		os.Exit(1)
	}
	known := make(map[string]bool, len(corpus))
	for _, e := range corpus {
		known[e.ID] = true
	}
	log.Info("loaded studio corpus", "count", len(corpus))

	scanner := matcher.NewScanner(corpus, matcher.DefaultScannerOptions())

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

	results := make([]*studioResult, 0, len(modes))
	for _, m := range modes {
		log.Info("running mode", "name", m.Name)
		t0 := time.Now()
		r := scoreMode(m.Name, scanner, scenes, known, m.Haystack)
		r.Elapsed = time.Since(t0)
		results = append(results, r)

		csvPath := filepath.Join(*outputDir, m.Name+".studio.failures.csv")
		if err := writeFailures(csvPath, r.Failures, *maxFailures); err != nil {
			log.Error("write csv", "mode", m.Name, "err", err)
			os.Exit(1)
		}
		log.Info("mode done",
			"name", m.Name,
			"scored", r.ScoredScenes,
			"correct", r.Correct,
			"wrong", r.Wrong,
			"missed", r.Missed,
			"csv", csvPath,
			"elapsed", r.Elapsed,
		)
	}

	printSummary(os.Stdout, results)
}

type studioResult struct {
	Mode         string
	ScoredScenes int // scenes with non-empty studio GT in the corpus
	Correct      int // GT studio was in the scanner's hit set
	Wrong        int // scanner emitted hits but none were the GT studio
	Missed       int // scanner emitted no hits at all (false negative)
	Failures     []studioFailure
	Elapsed      time.Duration
}

type studioFailure struct {
	SceneID   string
	Kind      string // "missed" | "wrong"
	GT        string
	Predicted string // pipe-joined hit IDs
	Input     string
}

func scoreMode(name string, scanner *matcher.Scanner, scenes []stash.LabeledScene, known map[string]bool, haystackFn func(stash.LabeledScene) string) *studioResult {
	r := &studioResult{Mode: name}
	for _, sc := range scenes {
		if sc.StudioStashDBID == "" || !known[sc.StudioStashDBID] {
			continue
		}
		r.ScoredScenes++

		input := haystackFn(sc)
		hits := scanner.Match(input)

		hitSet := make(map[string]bool, len(hits))
		for _, h := range hits {
			hitSet[h] = true
		}
		switch {
		case len(hits) == 0:
			r.Missed++
			r.Failures = append(r.Failures, studioFailure{
				SceneID: sc.ID, Kind: "missed", GT: sc.StudioStashDBID, Input: input,
			})
		case hitSet[sc.StudioStashDBID]:
			r.Correct++
		default:
			r.Wrong++
			r.Failures = append(r.Failures, studioFailure{
				SceneID:   sc.ID,
				Kind:      "wrong",
				GT:        sc.StudioStashDBID,
				Predicted: strings.Join(hits, "|"),
				Input:     input,
			})
		}
	}
	return r
}

func writeFailures(path string, rows []studioFailure, cap int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{"scene_id", "kind", "gt", "predicted", "input"}); err != nil {
		return err
	}
	n := len(rows)
	if cap > 0 && cap < n {
		n = cap
	}
	for i := 0; i < n; i++ {
		r := rows[i]
		if err := w.Write([]string{r.SceneID, r.Kind, r.GT, r.Predicted, r.Input}); err != nil {
			return err
		}
	}
	return nil
}

func printSummary(w *os.File, results []*studioResult) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "mode\tscored\tcorrect\twrong\tmissed\trecall\tprecision\telapsed")
	for _, r := range results {
		recall := 0.0
		if r.ScoredScenes > 0 {
			recall = float64(r.Correct) / float64(r.ScoredScenes)
		}
		precision := 0.0
		predicted := r.Correct + r.Wrong
		if predicted > 0 {
			precision = float64(r.Correct) / float64(predicted)
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%.3f\t%.3f\t%s\n",
			r.Mode, r.ScoredScenes, r.Correct, r.Wrong, r.Missed,
			recall, precision, r.Elapsed.Truncate(time.Millisecond))
	}
	tw.Flush()
}
