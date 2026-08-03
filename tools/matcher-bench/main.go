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
//
// With --verify --gate it is the scheduled full-pipeline regression gate: it
// fails when the live run breaches the floors in
// internal/matcher/pipeline_floors.json. That is the half of the pipeline the
// per-push test cannot see, since replaying frozen candidates guards Verify
// alone. Drive it with scripts/matcher-bench.sh; see docs/matcher-accuracy.md.
package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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

// gateBreachExit is the status --gate uses for a breached floor, kept distinct
// from the 1 that every other failure path here exits with. scripts/matcher-
// bench.sh needs to tell "the pipeline regressed" from "ssh could not reach the
// host", and reporting the second as the first is how a green box of a problem
// gets opened at the wrong end.
const gateBreachExit = 3

func main() {
	var (
		limit       = flag.Int("limit", 500, "max labeled scenes to bench (0 = all)")
		concurrency = flag.Int("concurrency", 4, "parallel matcher.Match calls")
		outputDir   = flag.String("output-dir", ".", "where to write *.failures.csv")
		maxFailures = flag.Int("max-failures", 0, "cap rows per CSV (0 = no cap)")
		corpusPath  = flag.String("corpus", "", "YAML corpus path; when set, runs against the corpus instead of the Stash library")
		verify      = flag.Bool("verify", false, "verification mode: per entry assert the expected scene verifies (recall) and other candidates do NOT (precision) — exercises matcher.Verify, the release-page badge logic")
		explain     = flag.String("explain", "", "match ONE release string and dump every candidate (rank, conf, title overlap, reasons, verify outcome) — the failure-CSV microscope")
		expectID    = flag.String("expect", "", "with --explain: the expected StashDB scene id, highlighted and verify-checked")
		dumpPath    = flag.String("dump", "", "with --verify: record every entry's candidates to this path (.gz compresses). Verify is pure given its candidates, so a dump lets threshold sweeps and a CI regression gate replay the corpus offline instead of paying two StashDB round trips per release (26 minutes a run). A <name>.meta.json sidecar is written beside it holding this run's numbers and verifier config, which is what makes refreshing the committed fixture one command.")
		dumpCommit  = flag.String("dump-commit", "", "with --dump: the forage commit being benched, recorded in the sidecar so a surprising number can be traced back to a tree")
		gate        = flag.Bool("gate", false, "with --verify: fail (exit 3) when the run breaches the recorded full-pipeline floors in internal/matcher/pipeline_floors.json. This is the scheduled regression gate; the per-push gate only replays frozen candidates and so guards Verify alone.")
		floorsPath  = flag.String("floors", "", "with --gate: read floors from this JSON file instead of the ones compiled into the binary. For trying a proposed ratchet without rebuilding.")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Both flags are read only inside the verify branch, so without --verify
	// they were accepted and ignored: `matcher-bench --gate` exited 0 having
	// gated nothing, which is the worst possible outcome for a gate. Refuse
	// before touching the config or the database, so the mistake costs a
	// second rather than a matcher construction.
	if *gate && !*verify {
		fmt.Fprintln(os.Stderr, "matcher-bench: --gate needs --verify. The floors describe a Match+Verify run; without --verify this would have exited 0 having gated nothing.")
		os.Exit(2)
	}
	if *dumpPath != "" && !*verify {
		fmt.Fprintln(os.Stderr, "matcher-bench: --dump needs --verify. The replay fixture is recorded from the verify pass.")
		os.Exit(2)
	}

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

	if *explain != "" {
		runExplain(ctx, m, *explain, *expectID)
		return
	}

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
		// --limit applies here too. It did not, so a run asked to bench 400
		// entries quietly benched all 1,570 — the flag reported one thing
		// and the numbers described another, which is the whole failure
		// mode this tool exists to prevent.
		if *limit > 0 && len(entries) > *limit {
			log.Info("corpus truncated by --limit", "entries", len(entries), "limit", *limit)
			entries = entries[:*limit]
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
				Source:    e.Source,
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

	if *verify {
		log.Info("running verify mode", "entries", len(gt), "concurrency", *concurrency)
		t0 := time.Now()
		vr := runVerify(ctx, log, m, gt, *concurrency, *dumpPath != "")
		vr.Elapsed = time.Since(t0)
		csvPath := filepath.Join(*outputDir, "verify.failures.csv")
		if err := writeVerifyFailures(csvPath, vr.Failures, *maxFailures); err != nil {
			log.Error("write verify csv", "err", err)
		}
		// A refresh that could not write its fixture must not exit 0. The
		// driving script decides whether to replace the committed fixture from
		// this process's status, and a warning in a 26-minute log is not
		// something anyone reads.
		dumpFailed := false
		if *dumpPath != "" {
			if err := writeDump(log, *dumpPath, *dumpCommit, describeCorpus(*corpusPath, len(gt)), vr); err != nil {
				log.Error("write replay dump", "err", err)
				dumpFailed = true
			}
		}
		printVerify(os.Stdout, vr, csvPath)
		if *gate && !checkFloors(os.Stdout, log, *floorsPath, vr) {
			os.Exit(gateBreachExit)
		}
		if dumpFailed {
			os.Exit(1)
		}
		return
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
// quoted string values (escape: ” for a literal quote). One pass,
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
// unescapes embedded ” → '. Returns plain strings unchanged.
func unquoteYAML(s string) string {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		s = s[1 : len(s)-1]
		s = strings.ReplaceAll(s, "''", "'")
	}
	return s
}

// ── verification mode ────────────────────────────────────────────────
//
// For each entry (release, expected scene) we run the matcher once, then
// exercise matcher.Verify — the exact logic behind the release page's
// "verified" badge:
//   - recall:    does the EXPECTED scene verify for its real release?
//   - precision: do the OTHER candidate scenes (wrong scenes that share
//                a performer / title token) wrongly verify? Each one that
//                does is a false-green — the "Home And Horny (156)" class.

type verifyResult struct {
	// Replay is the recorded candidate set, populated only when --dump is
	// given. Kept on the result rather than written inline so the writer
	// stays with the other output paths.
	Replay []matcher.ReplayEntry
	// Total is the number of entries SCORED: the ones whose Match call
	// returned. It is incremented as results arrive, alongside the Replay
	// append, which is the point.
	//
	// It used to be set to len(entries) before the run. Two things followed,
	// both silent. The rates below deflated by the error count, because an
	// entry the matcher was never able to answer for cannot be recalled but
	// stayed in the denominator anyway. And --dump wrote a sidecar claiming
	// more entries than the fixture beside it held, which is exactly what
	// TestCorpusFixtureMatchesTheLiveRun rejects, so one transient StashDB 5xx
	// in a 26-minute refresh committed a self-inconsistent pair and left push
	// CI red until somebody spent another 26 minutes.
	Total int
	// MatchErrors is entries whose Match call failed. Counted, never scored:
	// see recordMatchError.
	MatchErrors      int
	Recall           int // expected scene verified
	NoExpectedCand   int // expected scene wasn't even a candidate
	FalseVerifies    int // total wrong-scene verifications across all entries
	EntriesWithFalse int // entries with >=1 false verify
	Failures         []verifyFailure
	Elapsed          time.Duration
	// PerSource breaks recall out by corpus source so the search-input
	// distribution (grabs-search) is visible separately from download-name
	// sources (qbit/sab/torrent), which the matcher never sees in prod.
	PerSource map[string]*sourceStats
}

type sourceStats struct {
	Total  int
	Recall int
}

type verifyFailure struct {
	Release         string
	Kind            string // "recall_miss" | "false_verify"
	ExpectedID      string
	FalseSceneID    string
	FalseSceneTitle string
	Conf            float64
	// For recall_miss rows: whether the expected scene was among the
	// candidates at all. Splits the misses into retrieval failures (the
	// queries never surfaced the scene — fix Track A/B query construction
	// or accept it's not on StashDB) and verification failures (the scene
	// was RIGHT THERE and Verify declined it — fix signals/thresholds).
	// The two need entirely different work, and the aggregate counter
	// alone couldn't attribute per-class.
	ExpectedWasCandidate bool
	ExpectedConf         float64 // candidate's confidence when present
}

// runExplain matches one release string and dumps every candidate — the
// microscope for failure-CSV rows. Shows per-candidate confidence, title
// overlap, reasons, cast, and what matcher.Verify would say for each, so
// guard trips (isTop, rival-title-margin) are directly visible.
func runExplain(ctx context.Context, m *matcher.Matcher, release, expectID string) {
	cands, err := m.Match(ctx, release)
	if err != nil {
		fmt.Fprintf(os.Stderr, "match: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("release: %s\ncandidates: %d\n\n", release, len(cands))
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "rank\tconf\toverlap\tverified\tid\tdate\ttitle\tcast\treasons")
	for i, c := range cands {
		v := matcher.Verify(cands, c.Scene.ID, c.Scene.Title, release)
		mark := ""
		if c.Scene.ID == expectID {
			mark = " <== EXPECTED"
		}
		var cast []string
		for _, p := range c.Scene.Performers {
			cast = append(cast, p.Name)
		}
		fmt.Fprintf(tw, "%d%s\t%.3f\t%.3f\t%v\t%s\t%s\t%s\t%s\t%s\n",
			i+1, mark, c.Confidence, c.TitleOverlap, v.Verified,
			c.Scene.ID, c.Scene.Date, c.Scene.Title,
			strings.Join(cast, ", "), strings.Join(c.Reasons, "; "))
	}
	tw.Flush()
}

// releaseMatcher is the slice of *matcher.Matcher the verify pass uses.
//
// It is an interface so the run loop can be driven offline. Nothing could
// before: runVerify needed a live StashDB and a populated SQLite, which is
// precisely why an accounting bug in it (a Match error left in the denominator
// and out of the dump) survived review and a full test suite.
type releaseMatcher interface {
	Match(ctx context.Context, releaseName string) ([]matcher.Candidate, error)
}

func (r *verifyResult) srcStat(src string) *sourceStats {
	if src == "" {
		src = "(unlabeled)"
	}
	if r.PerSource == nil {
		r.PerSource = map[string]*sourceStats{}
	}
	st := r.PerSource[src]
	if st == nil {
		st = &sourceStats{}
		r.PerSource[src] = st
	}
	return st
}

// recordMatchError books an entry the pipeline could not be asked about.
//
// Deliberately not a recall miss and deliberately not in Total: the matcher was
// never given the chance to answer, so scoring it as a failure to find the
// scene reports a StashDB outage as a retrieval regression, and the gate's
// message ("no longer finds scenes it used to find") would be confidently
// wrong about its own cause.
func (r *verifyResult) recordMatchError() { r.MatchErrors++ }

// recordEntry scores one successful Match and, when recording, appends it to
// the dump. Total, Replay and the per-source counters move together here so
// they cannot drift apart the way they did when Total was set up front.
func (r *verifyResult) recordEntry(e stash.LabeledScene, cands []matcher.Candidate, dump bool) {
	r.Total++
	if dump {
		r.Replay = append(r.Replay, matcher.RecordCandidates(e.Basename, e.StashDBID, cands))
	}
	var expTitle string
	var expConf float64
	expFound := false
	for _, c := range cands {
		if c.Scene.ID == e.StashDBID {
			expTitle = c.Scene.Title
			expConf = c.Confidence
			expFound = true
			break
		}
	}
	st := r.srcStat(e.Source)
	st.Total++
	if matcher.Verify(cands, e.StashDBID, expTitle, e.Basename).Verified {
		r.Recall++
		st.Recall++
	} else {
		if !expFound {
			r.NoExpectedCand++
		}
		r.Failures = append(r.Failures, verifyFailure{
			Release: e.Basename, Kind: "recall_miss", ExpectedID: e.StashDBID,
			ExpectedWasCandidate: expFound, ExpectedConf: expConf,
		})
	}
	falseHere := 0
	for _, c := range cands {
		if c.Scene.ID == e.StashDBID {
			continue
		}
		if vr := matcher.Verify(cands, c.Scene.ID, c.Scene.Title, e.Basename); vr.Verified {
			falseHere++
			r.Failures = append(r.Failures, verifyFailure{
				Release: e.Basename, Kind: "false_verify", ExpectedID: e.StashDBID,
				FalseSceneID: c.Scene.ID, FalseSceneTitle: c.Scene.Title, Conf: vr.Confidence,
			})
		}
	}
	if falseHere > 0 {
		r.FalseVerifies += falseHere
		r.EntriesWithFalse++
	}
}

func runVerify(ctx context.Context, log *slog.Logger, m releaseMatcher, entries []stash.LabeledScene, concurrency int, dump bool) *verifyResult {
	r := &verifyResult{PerSource: map[string]*sourceStats{}}
	if concurrency < 1 {
		concurrency = 1
	}
	type res struct {
		idx   int
		cands []matcher.Candidate
		err   error
	}
	jobs := make(chan int, concurrency*2)
	out := make(chan res, concurrency*2)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				c, e := m.Match(ctx, entries[idx].Basename)
				out <- res{idx: idx, cands: c, err: e}
			}
		}()
	}
	go func() {
		for i := range entries {
			jobs <- i
		}
		close(jobs)
	}()
	go func() { wg.Wait(); close(out) }()

	var mu sync.Mutex
	var processed atomic.Int64
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			log.Info("verify progress", "done", processed.Load(), "total", len(entries))
		case rr, ok := <-out:
			if !ok {
				return r
			}
			processed.Add(1)
			e := entries[rr.idx]
			mu.Lock()
			if rr.err != nil {
				r.recordMatchError()
				mu.Unlock()
				log.Warn("match error", "id", e.ID, "err", rr.err)
				continue
			}
			r.recordEntry(e, rr.cands, dump)
			mu.Unlock()
		}
	}
}

func writeVerifyFailures(path string, rows []verifyFailure, max int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(bufio.NewWriter(f))
	defer w.Flush()
	_ = w.Write([]string{"kind", "release", "expected_id", "false_scene_id", "false_scene_title", "conf", "expected_was_candidate", "expected_conf"})
	for i, row := range rows {
		if max > 0 && i >= max {
			break
		}
		_ = w.Write([]string{row.Kind, row.Release, row.ExpectedID, row.FalseSceneID, row.FalseSceneTitle,
			fmt.Sprintf("%.2f", row.Conf), strconv.FormatBool(row.ExpectedWasCandidate), fmt.Sprintf("%.2f", row.ExpectedConf)})
	}
	return nil
}

func printVerify(w *os.File, r *verifyResult, csvPath string) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "metric\tvalue")
	fmt.Fprintf(tw, "entries scored\t%d\n", r.Total)
	// Printed unconditionally, second, and before anything derived from the
	// scored set. When Match is failing, every rate under this line describes
	// the subset that got through, and a reader who has to go hunting in the
	// log for a Warn line to learn that will instead read a retrieval
	// regression that is not there.
	fmt.Fprintf(tw, "match errors (NOT scored, excluded from every rate below)\t%d  (%.4f)\n",
		r.MatchErrors, r.score().MatchErrorRate())
	fmt.Fprintf(tw, "recall (expected verified)\t%d  (%.3f)\n", r.Recall, ratio(r.Recall, r.Total))
	fmt.Fprintf(tw, "  of which expected not a candidate\t%d\n", r.NoExpectedCand)
	fmt.Fprintf(tw, "entries with >=1 false verify\t%d  (%.3f)\n", r.EntriesWithFalse, ratio(r.EntriesWithFalse, r.Total))
	fmt.Fprintf(tw, "total false verifies\t%d\n", r.FalseVerifies)
	// Clean is recall minus the entries that ALSO verified a wrong scene. It
	// is the honest single number, and it is what the scheduled gate floors
	// on, so it belongs in the normal report rather than only under --gate:
	// recall alone reads as an improvement when a change simply started
	// verifying everything.
	fmt.Fprintf(tw, "clean (right scene and nothing else)\t%.4f\n", r.score().CleanRate())
	fmt.Fprintf(tw, "elapsed\t%s\n", r.Elapsed.Round(time.Millisecond))
	fmt.Fprintf(tw, "failures csv\t%s\n", csvPath)
	tw.Flush()

	if len(r.PerSource) > 0 {
		srcs := make([]string, 0, len(r.PerSource))
		for s := range r.PerSource {
			srcs = append(srcs, s)
		}
		sort.Strings(srcs)
		fmt.Fprintln(w, "\nrecall by source (grabs-search = real matcher input):")
		st2 := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		fmt.Fprintln(st2, "source\trecall\tn")
		for _, s := range srcs {
			ss := r.PerSource[s]
			fmt.Fprintf(st2, "%s\t%.3f\t%d\n", s, ratio(ss.Recall, ss.Total), ss.Total)
		}
		st2.Flush()
	}
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

// score reshapes a live verify run into the same struct the offline replay
// scorer produces, so the scheduled full-pipeline gate and the per-push replay
// gate are comparing identical quantities. They were computed by two different
// bits of arithmetic before, which is how a "recall" in one place can quietly
// stop meaning what it means in the other.
func (r *verifyResult) score() matcher.ReplayScore {
	return matcher.ReplayScore{
		Entries:          r.Total,
		Recall:           r.Recall,
		FalseVerifies:    r.FalseVerifies,
		EntriesWithFalse: r.EntriesWithFalse,
		MatchErrors:      r.MatchErrors,
	}
}

// describeCorpus records what the run was measured on, so a sidecar built with
// different build-corpus flags is visible in a diff rather than inferred from
// an entry count that happens to look plausible.
func describeCorpus(corpusPath string, entries int) string {
	if corpusPath == "" {
		return fmt.Sprintf("%d labeled scenes from the Stash library (filenames, NOT production matcher input)", entries)
	}
	return fmt.Sprintf("%d entries from %s", entries, filepath.Base(corpusPath))
}

// writeDump writes the replay fixture and its sidecar.
//
// The entries are sorted first: runVerify appends in worker-completion order,
// so two runs over the same corpus produce byte-different files. That makes a
// refreshed fixture impossible to eyeball and turns every refresh into an
// unreviewable blob. SliceStable rather than Slice because a corpus can hold
// two rows with the same release string, and an unstable sort reorders those
// between runs, which is the byte-identity claim broken in the one case the
// sort exists to fix. ScoreReplay sums over entries, so order changes nothing
// it measures.
func writeDump(log *slog.Logger, path, commit, corpus string, vr *verifyResult) error {
	// The fixture and its sidecar must describe the same set of entries.
	// TestCorpusFixtureMatchesTheLiveRun compares the fixture's length against
	// the sidecar's entry count, so a skew here does not fail here: it fails on
	// somebody else's push, days later, and can only be cleared by another
	// 26-minute run. Refusing to write is recoverable; writing the broken pair
	// is not. The counts are taken from the recorded slice and the scored
	// counter, which recordEntry moves together, so this fires only if that
	// invariant is broken again.
	if len(vr.Replay) != vr.Total {
		return fmt.Errorf(
			"refusing to write a fixture that disagrees with its own sidecar: %d recorded entries but %d scored. Nothing was written",
			len(vr.Replay), vr.Total)
	}
	sort.SliceStable(vr.Replay, func(i, j int) bool { return vr.Replay[i].Release < vr.Replay[j].Release })
	if err := matcher.SaveReplay(path, vr.Replay); err != nil {
		return err
	}
	metaPath := matcher.ReplayMetaPath(path)
	meta := matcher.ReplayMeta{
		RecordedAt: time.Now().UTC().Truncate(time.Second),
		Commit:     commit,
		Corpus:     corpus,
		// From the recorded slice, not a counter: this number's whole job is
		// to describe the file next to it.
		Entries:          len(vr.Replay),
		Recall:           vr.Recall,
		FalseVerifies:    vr.FalseVerifies,
		EntriesWithFalse: vr.EntriesWithFalse,
		MatchErrors:      vr.MatchErrors,
		Config:           matcher.DefaultVerifyConfig,
	}
	if err := matcher.SaveReplayMeta(metaPath, meta); err != nil {
		return err
	}
	log.Info("replay dump written", "path", path, "meta", metaPath, "entries", len(vr.Replay))
	return nil
}

// checkFloors is the scheduled full-pipeline regression gate. Returns false
// when the run breached a floor.
//
// It prints the floors even on success. A gate whose numbers are only visible
// when it fails is a gate nobody knows the margin of, and the margin is what
// tells you a change is drifting before it trips.
func checkFloors(w io.Writer, log *slog.Logger, floorsPath string, vr *verifyResult) bool {
	var (
		floors matcher.PipelineFloors
		err    error
	)
	if floorsPath != "" {
		floors, err = matcher.LoadPipelineFloors(floorsPath)
	} else {
		floors, err = matcher.DefaultPipelineFloors()
	}
	if err != nil {
		log.Error("load pipeline floors", "err", err)
		return false
	}
	s := vr.score()
	fmt.Fprintf(w, "\nfull-pipeline gate (floors measured %s):\n", floors.MeasuredAt)
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "metric\tthis run\tfloor")
	fmt.Fprintf(tw, "entries scored\t%d\t>= %d\n", s.Entries, floors.MinEntries)
	// The gate's own output has to carry the error count, or a reader deciding
	// whether recall really moved has to go and find the run's stderr.
	fmt.Fprintf(tw, "match errors\t%d (%.4f)\t<= %.4f\n", s.MatchErrors, s.MatchErrorRate(), floors.MaxMatchErrorRate)
	fmt.Fprintf(tw, "recall\t%.4f\t>= %.4f\n", s.RecallRate(), floors.MinRecall)
	fmt.Fprintf(tw, "clean\t%.4f\t>= %.4f\n", s.CleanRate(), floors.MinClean)
	fmt.Fprintf(tw, "false verify rate\t%.4f\t<= %.4f\n", s.FalseVerifyRate(), floors.MaxFalseVerifyRate)
	tw.Flush()

	breaches := floors.Check(s)
	if len(breaches) == 0 {
		fmt.Fprintln(w, "gate: PASS")
		return true
	}
	fmt.Fprintln(w, "gate: FAIL")
	for _, b := range breaches {
		fmt.Fprintf(w, "  - %s\n", b)
	}
	return false
}
