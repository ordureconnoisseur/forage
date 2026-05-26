// collision-bench compares candidate performer-detection strategies
// against the user's existing Stash library (which provides ground
// truth via StashDB-scraped scenes).
//
// Run from the forager repo root:
//
//	FORAGER_STASH_URL=... FORAGER_STASH_API_KEY=... \
//	  go run ./tools/collision-bench --limit=2000
//
// Reads `performer_cache` from `forager.db` for the matcher corpus, so
// the daemon needs to have run at least once first.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/db"
	"github.com/ordureconnoisseur/forager/internal/matcher"
	"github.com/ordureconnoisseur/forager/internal/stash"
)

func main() {
	var (
		limit       = flag.Int("limit", 2000, "max labeled scenes to fetch (0 = all)")
		strategies  = flag.String("strategies", "all", "comma list of strategy names, or 'all'")
		outputDir   = flag.String("output-dir", ".", "where to write per-strategy *.failures.csv files")
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

	corpus, err := loadPerformerCorpus(ctx, database)
	if err != nil {
		log.Error("load corpus", "err", err)
		os.Exit(1)
	}
	if len(corpus) == 0 {
		log.Error("performer_cache is empty — run the daemon and POST /refresh first")
		os.Exit(1)
	}
	log.Info("loaded corpus", "performers", len(corpus))

	stashClient := stash.New(cfg.StashURL, cfg.StashAPIKey)
	log.Info("fetching labeled scenes from Stash", "limit", *limit)
	t0 := time.Now()
	scenes, err := stashClient.FindLabeledScenes(ctx, *limit)
	if err != nil {
		log.Error("find scenes", "err", err)
		os.Exit(1)
	}
	log.Info("fetched scenes", "count", len(scenes), "elapsed", time.Since(t0))

	// Build the set of known performer IDs (the corpus) so we can filter
	// scene truth-sets to performers we actually know about. Performers
	// missing locally would otherwise look like false negatives.
	known := make(map[string]bool, len(corpus))
	for _, p := range corpus {
		known[p.StashID] = true
	}

	picked, err := pickStrategies(*strategies, corpus)
	if err != nil {
		log.Error("strategies", "err", err)
		os.Exit(1)
	}

	if err := os.MkdirAll(*outputDir, 0o755); err != nil {
		log.Error("mkdir output", "err", err)
		os.Exit(1)
	}

	// Run every picked strategy twice: once over basename only, once
	// over basename + parent folder. The user's library is largely
	// performer-folder-organised, so the second mode tests whether the
	// folder closes the recall gap.
	modes := []struct {
		Suffix   string
		Haystack func(stash.LabeledScene) string
	}{
		{"", func(s stash.LabeledScene) string { return s.Basename }},
		{"_folder", func(s stash.LabeledScene) string {
			parts := append([]string{s.Basename}, s.Folders...)
			return strings.Join(parts, " ")
		}},
	}

	results := make([]*strategyResult, 0, len(picked)*len(modes))
	for _, mode := range modes {
		for _, s := range picked {
			name := s.Name + mode.Suffix
			log.Info("running strategy", "name", name)
			t0 := time.Now()
			r := evaluate(s, scenes, known, mode.Haystack)
			r.Name = name
			r.Elapsed = time.Since(t0)
			results = append(results, r)

			csvPath := filepath.Join(*outputDir, name+".failures.csv")
			if err := writeFailuresCSV(csvPath, r.Failures, *maxFailures); err != nil {
				log.Error("write csv", "strategy", name, "err", err)
				os.Exit(1)
			}
			log.Info("strategy done",
				"name", name,
				"scored", r.ScoredScenes,
				"precision", fmt.Sprintf("%.3f", r.Precision),
				"recall", fmt.Sprintf("%.3f", r.Recall),
				"f1", fmt.Sprintf("%.3f", r.F1),
				"failures", len(r.Failures),
				"csv", csvPath,
				"elapsed", r.Elapsed,
			)
		}
	}

	printSummaryTable(os.Stdout, results)
}

// CachedPerformer is the slim shape strategies match against. Read
// once from performer_cache at startup; never mutated.
type CachedPerformer struct {
	StashID string
	Name    string
	Aliases []string
}

func loadPerformerCorpus(ctx context.Context, database *sql.DB) ([]CachedPerformer, error) {
	rows, err := database.QueryContext(ctx, `SELECT stash_id, name, aliases FROM performer_cache`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CachedPerformer
	for rows.Next() {
		var p CachedPerformer
		var aliasesJSON sql.NullString
		if err := rows.Scan(&p.StashID, &p.Name, &aliasesJSON); err != nil {
			return nil, err
		}
		if aliasesJSON.Valid && aliasesJSON.String != "" && aliasesJSON.String != "null" {
			_ = json.Unmarshal([]byte(aliasesJSON.String), &p.Aliases)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// pickStrategies turns the --strategies flag value into the ordered
// list of strategies to run. Order in `all` matches the plan: cheap
// baseline first, then progressively more selective filters.
func pickStrategies(spec string, corpus []CachedPerformer) ([]namedStrategy, error) {
	all := []namedStrategy{
		{Name: "substring_naive", Fn: substringNaiveFactory(corpus)},
		{Name: "token_aware", Fn: tokenAwareFactory(corpus)},
		{Name: "token_min2", Fn: tokenMin2Factory(corpus)},
		{Name: "token_last_required", Fn: tokenLastRequiredFactory(corpus)},
		{Name: "token_min2_min3", Fn: tokenMin2Min3Factory(corpus)},
		{Name: "token_min2_first_unique", Fn: tokenMin2FirstUniqueFactory(corpus)},
		{Name: "matcher_scanner", Fn: matcherScannerStrategy(corpus)},
	}
	if spec == "all" || spec == "" {
		return all, nil
	}
	want := map[string]bool{}
	for _, n := range strings.Split(spec, ",") {
		want[strings.TrimSpace(n)] = true
	}
	out := make([]namedStrategy, 0, len(want))
	for _, s := range all {
		if want[s.Name] {
			out = append(out, s)
			delete(want, s.Name)
		}
	}
	if len(want) > 0 {
		unknown := make([]string, 0, len(want))
		for n := range want {
			unknown = append(unknown, n)
		}
		return nil, fmt.Errorf("unknown strategies: %s", strings.Join(unknown, ","))
	}
	return out, nil
}

type namedStrategy struct {
	Name string
	Fn   StrategyFunc
}

// matcherScannerStrategy wraps internal/matcher.NewScanner so we can
// A/B it against the inline strategies and confirm the production
// scanner doesn't regress performer-bench performance after the
// CamelCase/digit-split tokenizer change.
func matcherScannerStrategy(corpus []CachedPerformer) StrategyFunc {
	entities := make([]matcher.Entity, 0, len(corpus))
	for _, p := range corpus {
		entities = append(entities, matcher.Entity{ID: p.StashID, Name: p.Name, Aliases: p.Aliases})
	}
	s := matcher.NewScanner(entities, matcher.DefaultScannerOptions())
	return func(haystack string) []string {
		return s.Match(haystack)
	}
}
