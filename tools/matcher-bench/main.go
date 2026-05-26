// matcher-bench measures end-to-end matcher.Match accuracy.
//
// Two modes:
//
//   - **Stash library mode** (default): pulls labeled scenes from the
//     user's Stash and treats their basenames (and basename+folders)
//     as the "release name". Measures matcher performance on the
//     messy filename input it has to handle in a deduplication
//     context.
//
//   - **Corpus mode** (--corpus path): loads a YAML corpus of real
//     (release, expected_scene_id) pairs (built by tools/build-corpus)
//     and runs Match on each release. Measures matcher performance on
//     the well-formatted Prowlarr-style release names it actually
//     handles in production grab flow.
//
// Run from the forager repo root (daemon must have populated forager.db):
//
//	FORAGER_STASH_URL=... FORAGER_STASH_API_KEY=... \
//	FORAGER_STASHDB_API_KEY=... \
//	  go run ./tools/matcher-bench --limit=500 --concurrency=4
//
// Or in the deployed forager container against the real corpus:
//
//	docker exec forager /matcher-bench --corpus=/data/corpus.yaml --concurrency=8
package main

import (
	"bufio"
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
		corpusPath  = flag.String("corpus", "", "YAML corpus path; when set, runs against the corpus instead of the Stash library")
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

	if err := os.MkdirAll(*outputDir, 0o755); err != nil {
		log.Error("mkdir output", "err", err)
		os.Exit(1)
	}

	var (
		gt    []stash.LabeledScene
		modes []struct {
			Name  string
			Input func(stash.LabeledScene) string
		}
	)

	if *corpusPath != "" {
		log.Info("loading corpus", "path", *corpusPath)
		entries, err := loadCorpus(*corpusPath)
		if err != nil {
			log.Error("load corpus", "err", err)
			os.Exit(1)
		}
		log.Info("corpus loaded", "entries", len(entries))
		for _, e := range entries {
			// Reuse LabeledScene as the test-case shape. Basename =
			// matcher input (the release string); StashDBID = ground
			// truth. ID = corpus row id for failure-CSV identification.
			gt = append(gt, stash.LabeledScene{
				ID:        e.ID,
				Basename:  e.Release,
				StashDBID: e.ExpectedScene,
			})
		}
		// Corpus releases are already the full release-name string —
		// no folder context to add. Single "release" mode.
		modes = []struct {
			Name  string
			Input func(stash.LabeledScene) string
		}{
			{"release", func(s stash.LabeledScene) string { return s.Basename }},
		}
	} else {
		log.Info("fetching labeled scenes", "limit", *limit)
		t0 = time.Now()
		scenes, err := stashClient.FindLabeledScenes(ctx, *limit)
		if err != nil {
			log.Error("find scenes", "err", err)
			os.Exit(1)
		}
		log.Info("fetched scenes", "count", len(scenes), "elapsed", time.Since(t0))

		gt = make([]stash.LabeledScene, 0, len(scenes))
		for _, s := range scenes {
			if s.StashDBID != "" {
				gt = append(gt, s)
			}
		}
		log.Info("with-stashdb-id", "count", len(gt))

		modes = []struct {
			Name  string
			Input func(stash.LabeledScene) string
		}{
			{"basename", func(s stash.LabeledScene) string { return s.Basename }},
			{"basename_folder", func(s stash.LabeledScene) string {
				parts := append([]string{s.Basename}, s.Folders...)
				return strings.Join(parts, " ")
			}},
		}
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
	// Filled when the correct scene appears at rank ≥ 2 (so we can
	// compare what won vs what should have won — drives the
	// profile-and-reweight workflow). Empty for no_candidates /
	// missing_from_top10.
	GTConf    float64
	GTReasons string
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
				row := failureRow{
					SceneID:     sc.ID,
					GTStashDBID: sc.StashDBID,
					Kind:        kind,
					TopID:       top.Scene.ID,
					TopConf:     top.Confidence,
					TopReasons:  strings.Join(top.Reasons, "; "),
					Input:       inputFn(sc),
				}
				// Capture the correct candidate's score + reasons when
				// it's in the pool but not at rank 1 — gives us a
				// side-by-side to spot scoring patterns.
				if pos > 0 {
					gt := cands[pos]
					row.GTConf = gt.Confidence
					row.GTReasons = strings.Join(gt.Reasons, "; ")
				}
				failuresMu.Lock()
				r.Failures = append(r.Failures, row)
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
	if err := w.Write([]string{
		"scene_id", "gt_stashdb_id", "kind",
		"top_id", "top_confidence", "top_reasons",
		"gt_confidence", "gt_reasons",
		"input",
	}); err != nil {
		return err
	}
	n := len(rows)
	if cap > 0 && cap < n {
		n = cap
	}
	for i := 0; i < n; i++ {
		r := rows[i]
		if err := w.Write([]string{
			r.SceneID, r.GTStashDBID, r.Kind,
			r.TopID, fmt.Sprintf("%.3f", r.TopConf), r.TopReasons,
			fmt.Sprintf("%.3f", r.GTConf), r.GTReasons,
			r.Input,
		}); err != nil {
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

// corpusEntry mirrors the shape build-corpus writes. Hand-parsed
// below to avoid pulling in a yaml dependency for one file.
type corpusEntry struct {
	ID            string
	Release       string
	ExpectedScene string
	Source        string
	FilePath      string
}

// loadCorpus parses the YAML format build-corpus emits. The format is
// a simple list of objects, each on consecutive lines with single-
// quoted string values (escape: '' for a literal quote). One pass,
// state-machine style. Doesn't try to be a general YAML parser —
// only handles the shape we control.
func loadCorpus(path string) ([]corpusEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var (
		out []corpusEntry
		cur *corpusEntry
	)
	scanner := bufio.NewScanner(f)
	// Some release names + paths get long; bump the buffer.
	scanner.Buffer(make([]byte, 0, 8*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		stripped := strings.TrimSpace(line)
		if stripped == "" || strings.HasPrefix(stripped, "#") {
			continue
		}
		// New entry header: starts with `- ` at indent zero, value is
		// the first field (id). All later fields share a 2-space indent.
		if strings.HasPrefix(line, "- ") {
			if cur != nil {
				out = append(out, *cur)
			}
			cur = &corpusEntry{}
			rest := strings.TrimPrefix(line, "- ")
			parseField(cur, rest)
			continue
		}
		if cur == nil {
			continue
		}
		parseField(cur, stripped)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if cur != nil {
		out = append(out, *cur)
	}
	return out, nil
}

func parseField(e *corpusEntry, line string) {
	colon := strings.Index(line, ":")
	if colon < 0 {
		return
	}
	key := strings.TrimSpace(line[:colon])
	val := strings.TrimSpace(line[colon+1:])
	val = unquoteYAML(val)
	switch key {
	case "id":
		e.ID = val
	case "release":
		e.Release = val
	case "expected_scene_id":
		e.ExpectedScene = val
	case "source":
		e.Source = val
	case "file_path":
		e.FilePath = val
	}
}

// unquoteYAML strips surrounding single quotes from a YAML scalar and
// unescapes embedded '' → '. Returns plain strings unchanged.
func unquoteYAML(s string) string {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		s = s[1 : len(s)-1]
		s = strings.ReplaceAll(s, "''", "'")
	}
	return s
}
