// verify-sweep re-derives the verifier's thresholds against a recorded corpus.
//
// Usage:
//
//	go run ./tools/verify-sweep --replay=/path/to/replay.json
//
// The replay comes from `matcher-bench --verify --dump=...`. Because Verify is
// pure given its candidates, every arm here is a few milliseconds of CPU
// instead of a 26-minute run against StashDB, which is what makes an honest
// train/holdout split affordable.
//
// Method: split deterministically by a hash of the release name, fit on TRAIN
// only, then report the chosen config on HOLDOUT. A threshold picked on the
// same rows it is scored against is a threshold that has memorised them.
package main

import (
	"flag"
	"fmt"
	"hash/fnv"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/ordureconnoisseur/forager/internal/matcher"
)

// holdoutShare is the fraction of the corpus withheld from fitting.
const holdoutShare = 0.30

func split(entries []matcher.ReplayEntry) (train, holdout []matcher.ReplayEntry) {
	for _, e := range entries {
		h := fnv.New32a()
		_, _ = h.Write([]byte(e.Release))
		// Stable per release name: the same row lands in the same half on
		// every run and on every machine, so two sweeps are comparable.
		if float64(h.Sum32()%1000)/1000.0 < holdoutShare {
			holdout = append(holdout, e)
		} else {
			train = append(train, e)
		}
	}
	return
}

type arm struct {
	name  string
	cfg   matcher.VerifyConfig
	train matcher.ReplayScore
	hold  matcher.ReplayScore
}

func main() {
	replay := flag.String("replay", "", "path to a matcher-bench --dump file")
	flag.Parse()
	if *replay == "" {
		fmt.Fprintln(os.Stderr, "verify-sweep: --replay is required")
		os.Exit(2)
	}
	entries, err := matcher.LoadReplay(*replay)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify-sweep: %v\n", err)
		os.Exit(1)
	}
	train, hold := split(entries)
	fmt.Printf("corpus %d entries: train %d, holdout %d\n\n", len(entries), len(train), len(hold))

	base := matcher.DefaultVerifyConfig
	var arms []arm
	add := func(name string, cfg matcher.VerifyConfig) {
		arms = append(arms, arm{name: name, cfg: cfg,
			train: matcher.ScoreReplay(cfg, train), hold: matcher.ScoreReplay(cfg, hold)})
	}
	add("baseline (shipped)", base)

	// One knob at a time first: a combined sweep cannot tell you which change
	// paid for itself.
	for _, m := range []float64{0.01, 0.02, 0.03, 0.05, 0.08, 0.12} {
		c := base
		c.TopMargin = m
		add(fmt.Sprintf("TopMargin=%.2f", m), c)
	}
	c := base
	c.StrongMatchNeedsDate = true
	add("StrongMatchNeedsDate", c)
	// Re-measuring a claim the code makes: "conf-only added false verifies,
	// conf+containment recovers the real short-title scenes with no
	// precision cost". Taken on the filename corpus; this checks it against
	// the input the matcher actually sees.
	c = base
	c.ShortTitleNeedsContainment = false
	add("shortTitle: conf ONLY (no containment)", c)
	// The largest false-verify class found by hand-labelling: BTS cuts that
	// share every signal with their main scene.
	c = base
	c.RefuseBehindTheScenes = true
	add("RefuseBehindTheScenes", c)
	c = base
	c.RefuseBehindTheScenes = true
	c.TopMargin = 0.02
	add("RefuseBTS + TopMargin=0.02", c)
	for _, s := range []float64{0.75, 0.80, 0.85} {
		c := base
		c.StrongMatchConf = s
		add(fmt.Sprintf("StrongMatchConf=%.2f", s), c)
	}
	for _, r := range []float64{0.05, 0.10, 0.20} {
		c := base
		c.StrongMatchRivalTitleMargin = r
		add(fmt.Sprintf("RivalTitleMargin=%.2f", r), c)
	}
	for _, d := range []float64{0.70, 0.75} {
		c := base
		c.DateAnchorMinConf = d
		add(fmt.Sprintf("DateAnchorMinConf=%.2f", d), c)
	}
	// Then the promising combinations.
	for _, m := range []float64{0.02, 0.05} {
		c := base
		c.TopMargin = m
		c.StrongMatchNeedsDate = true
		add(fmt.Sprintf("TopMargin=%.2f + needsDate", m), c)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "arm\ttrain recall\ttrain clean\ttrain false\thold recall\thold clean\thold false")
	for _, a := range arms {
		fmt.Fprintf(w, "%s\t%.4f\t%.4f\t%d\t%.4f\t%.4f\t%d\n", a.name,
			a.train.RecallRate(), a.train.CleanRate(), a.train.FalseVerifies,
			a.hold.RecallRate(), a.hold.CleanRate(), a.hold.FalseVerifies)
	}
	w.Flush()

	// CleanRate is the ranking key: it counts an entry only when the right
	// scene verified AND nothing else did, which is what "identified" means.
	best := arms
	sort.SliceStable(best, func(i, j int) bool {
		return best[i].train.CleanRate() > best[j].train.CleanRate()
	})
	fmt.Printf("\nbest on TRAIN by clean rate: %s (train %.4f, holdout %.4f)\n",
		best[0].name, best[0].train.CleanRate(), best[0].hold.CleanRate())
	fmt.Printf("baseline for comparison:     train %.4f, holdout %.4f\n",
		arms[0].train.CleanRate(), arms[0].hold.CleanRate())
}
