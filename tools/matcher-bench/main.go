// matcher-bench measures end-to-end matcher.Match accuracy against the
// user's existing Stash library. For each labeled scene we use its
// basename (+ optionally ancestor folders) as the "release name",
// invoke the matcher, and report whether the correct StashDB scene_id
// is in the top-1, top-3, or top-10 candidates.
//
// Run from the forager repo root (daemon must have populated forager.db):
//
//	FORAGER_STASH_URL=... FORAGER_STASH_API_KEY=... \
//	FORAGER_STASHDB_API_KEY=... \
//	  go run ./tools/matcher-bench --limit=500 --concurrency=4
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
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/db"
	"github.com/ordureconnoisseur/forager/internal/matcher"
	"github.com/ordureconnoisseur/forager/internal/stash"
	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

func main() {
	var (
		limit       = flag.Int("limit", 500, "max labeled scenes to bench (0 = all)")
		concurrency = flag.Int("concurrency", 4, "parallel matcher.Match calls")
		outputDir   = flag.String("output-dir", ".", "where to write *.failures.csv")
		maxFailures = flag.Int("max-failures", 0, "cap rows per CSV (0 = no cap)")
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

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	stashClient := stash.New(cfg.StashURL, cfg.StashAPIKey)
	stashDBClient := stashdb.New(cfg.StashDBURL, cfg.StashDBAPIKey)

	log.Info("constructing matcher")
	t0 := time.Now()
	m, err := matcher.New(ctx, database, stashDBClient)
	if err != nil {
		log.Error("matcher.New", "err", err)
		os.Exit(1)
	}
	log.Info("matcher ready", "setup", time.Since(t0))

	log.Info("fetching labeled scenes", "limit", *limit)
	t0 = time.Now()
	scenes, err := stashClient.FindLabeledScenes(ctx, *limit)
	if err != nil {
		log.Error("find scenes", "err", err)
		os.Exit(1)
	}
	log.Info("fetched scenes", "count", len(scenes), "elapsed", time.Since(t0))

	// Filter to scenes with StashDBID ground truth.
	gt := make([]stash.LabeledScene, 0, len(scenes))
	for _, s := range scenes {
		if s.StashDBID != "" {
			gt = append(gt, s)
		}
	}
	log.Info("with-stashdb-id", "count", len(gt))

	if err := os.MkdirAll(*outputDir, 0o755); err != nil {
		log.Error("mkdir output", "err", err)
		os.Exit(1)
	}

	modes := []struct {
		Name  string
		Input func(stash.LabeledScene) string
	}{
		{"basename", func(s stash.LabeledScene) string { return s.Basename }},
		{"basename_folder", func(s stash.LabeledScene) string {
			parts := append([]string{s.Basename}, s.Folders...)
			return strings.Join(parts, " ")
		}},
	}

	results := make([]*modeResult, 0, len(modes))
	for _, mode := range modes {
		log.Info("running mode", "name", mode.Name, "concurrency", *concurrency)
		t0 := time.Now()
		r := runMode(ctx, log, m, gt, mode.Name, mode.Input, *concurrency)
		r.Elapsed = time.Since(t0)
		results = append(results, r)

		csvPath := filepath.Join(*outputDir, mode.Name+".matcher.failures.csv")
		if err := writeFailures(csvPath, r.Failures, *maxFailures); err != nil {
			log.Error("write csv", "mode", mode.Name, "err", err)
			os.Exit(1)
		}
		log.Info("mode done",
			"name", mode.Name,
			"scenes", r.Scenes,
			"top1", r.Top1,
			"top3", r.Top3,
			"top10", r.Top10,
			"no_candidates", r.NoCandidates,
			"csv", csvPath,
			"elapsed", r.Elapsed,
		)
	}

	printSummary(os.Stdout, results)
}

type modeResult struct {
	Mode         string
	Scenes       int
	Top1         int
	Top3         int
	Top10        int
	NoCandidates int // matcher returned zero results
	Failures     []failureRow
	Elapsed      time.Duration
}

type failureRow struct {
	SceneID     string
	GTStashDBID string
	Kind        string // "no_candidates" | "wrong_top1" | "missing_from_top10"
	TopID       string
	TopConf     float64
	TopReasons  string
	Input       string
}

func runMode(ctx context.Context, log *slog.Logger, m *matcher.Matcher, scenes []stash.LabeledScene, name string, inputFn func(stash.LabeledScene) string, concurrency int) *modeResult {
	r := &modeResult{Mode: name, Scenes: len(scenes)}
	if concurrency < 1 {
		concurrency = 1
	}

	type job struct {
		idx   int
		scene stash.LabeledScene
	}
	type result struct {
		idx        int
		candidates []matcher.Candidate
		err        error
	}

	jobs := make(chan job, concurrency*2)
	results := make(chan result, concurrency*2)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				cands, err := m.Match(ctx, inputFn(j.scene))
				results <- result{idx: j.idx, candidates: cands, err: err}
			}
		}()
	}

	go func() {
		for i, sc := range scenes {
			jobs <- job{idx: i, scene: sc}
		}
		close(jobs)
	}()

	var failuresMu sync.Mutex
	var processed atomic.Int64
	logTicker := time.NewTicker(15 * time.Second)
	defer logTicker.Stop()

	doneCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(results)
		close(doneCh)
	}()

	for {
		select {
		case <-logTicker.C:
			log.Info("progress", "mode", name, "done", processed.Load(), "total", len(scenes))
		case <-doneCh:
			return r
		case res, ok := <-results:
			if !ok {
				return r
			}
			processed.Add(1)
			sc := scenes[res.idx]
			if res.err != nil {
				log.Warn("match error", "scene", sc.ID, "err", res.err)
				continue
			}

			cands := res.candidates
			if len(cands) == 0 {
				r.NoCandidates++
				failuresMu.Lock()
				r.Failures = append(r.Failures, failureRow{
					SceneID:     sc.ID,
					GTStashDBID: sc.StashDBID,
					Kind:        "no_candidates",
					Input:       inputFn(sc),
				})
				failuresMu.Unlock()
				continue
			}

			pos := -1
			for i, c := range cands {
				if c.Scene.ID == sc.StashDBID {
					pos = i
					break
				}
			}

			switch {
			case pos == 0:
				r.Top1++
				r.Top3++
				r.Top10++
			case pos >= 0 && pos < 3:
				r.Top3++
				r.Top10++
			case pos >= 0 && pos < 10:
				r.Top10++
			}

			if pos != 0 {
				kind := "missing_from_top10"
				if pos >= 0 {
					kind = fmt.Sprintf("rank=%d", pos+1)
				}
				top := cands[0]
				failuresMu.Lock()
				r.Failures = append(r.Failures, failureRow{
					SceneID:     sc.ID,
					GTStashDBID: sc.StashDBID,
					Kind:        kind,
					TopID:       top.Scene.ID,
					TopConf:     top.Confidence,
					TopReasons:  strings.Join(top.Reasons, "; "),
					Input:       inputFn(sc),
				})
				failuresMu.Unlock()
			}
		}
	}
}

func writeFailures(path string, rows []failureRow, cap int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{"scene_id", "gt_stashdb_id", "kind", "top_id", "top_confidence", "top_reasons", "input"}); err != nil {
		return err
	}
	n := len(rows)
	if cap > 0 && cap < n {
		n = cap
	}
	for i := 0; i < n; i++ {
		r := rows[i]
		if err := w.Write([]string{r.SceneID, r.GTStashDBID, r.Kind, r.TopID, fmt.Sprintf("%.3f", r.TopConf), r.TopReasons, r.Input}); err != nil {
			return err
		}
	}
	return nil
}

func printSummary(w *os.File, results []*modeResult) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "mode\tscenes\ttop1\ttop3\ttop10\tno_cand\tP@1\tP@3\tP@10\telapsed")
	for _, r := range results {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%.3f\t%.3f\t%.3f\t%s\n",
			r.Mode, r.Scenes, r.Top1, r.Top3, r.Top10, r.NoCandidates,
			pct(r.Top1, r.Scenes), pct(r.Top3, r.Scenes), pct(r.Top10, r.Scenes),
			r.Elapsed.Truncate(time.Millisecond))
	}
	tw.Flush()
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total)
}
