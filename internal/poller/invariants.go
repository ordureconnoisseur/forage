package poller

import (
	"context"
	"errors"
	"time"

	"github.com/ordureconnoisseur/forager/internal/invariants"
	"github.com/ordureconnoisseur/forager/internal/stash"
)

// The poller is where the invariant checker runs, for the same reason the
// reconcile and trash passes live here: this is the process's one scheduled
// loop, and it already holds everything the checker needs: the daemon's
// database handle, the library-mount latch, and a hot-swappable Stash client
// from the pool. Giving the checker its own goroutine would mean giving it
// its own copy of all three.
//
// The checker reads. It is spliced into the tick above the nothing-active
// early return, alongside the other housekeeping, because an idle library is
// exactly when a silent inconsistency is the only thing left to find.

// invariantInterval is how often the suite runs. Slow on purpose: nothing it
// finds is urgent (these are gaps that accumulate over weeks), and the two
// bounded checks rotate, so a shorter cadence would buy coverage at the cost
// of os.Stat and Stash traffic competing with real placement work.
const invariantInterval = 30 * time.Minute

// runInvariants executes one pass if the cadence is due. Owned by the
// single-goroutine tick, like the other pass gates.
func (p *Poller) runInvariants(ctx context.Context) {
	if time.Since(p.lastInvariants) < invariantInterval {
		return
	}
	p.lastInvariants = time.Now()
	p.invariantChecker().Run(ctx)
}

// invariantChecker lazily builds the checker over the daemon's own handles.
// Cached because it carries the rotating cursors for the two bounded checks
// and the previous run's counts (which is what lets a run log the transition
// to broken rather than only the level).
//
// Locked despite being built only by the tick goroutine: Invariants() reads
// the same field from request goroutines serving /healthz.
func (p *Poller) invariantChecker() *invariants.Checker {
	p.invariantMu.Lock()
	defer p.invariantMu.Unlock()
	if p.invariants == nil {
		p.invariants = invariants.
			New(p.db, p.log.With("component", "invariants")).
			WithLibraryHealth(p.libraryHealthy).
			WithStash(pollerSceneIndex{p})
	}
	return p.invariants
}

// Invariants returns the most recent invariant report, or nil before the
// first pass. Read by the API layer for /healthz, /diag and GET /invariants.
func (p *Poller) Invariants() *invariants.Report {
	p.invariantMu.Lock()
	c := p.invariants
	p.invariantMu.Unlock()
	if c == nil {
		return nil
	}
	return c.Last()
}

// pollerSceneIndex is the checker's bounded window onto Stash: one question,
// about one cross-id, at a time. Reads the client from the pool per call so a
// config hot-swap reaches the next pass, and reports "not configured" as an
// error rather than as zero scenes, because a missing Stash must never be
// mistaken for a missing scene: that would report every confirmed grab as
// broken.
type pollerSceneIndex struct{ p *Poller }

var errStashUnconfigured = errors.New("stash not configured")

func (i pollerSceneIndex) Endpoint(ctx context.Context) (string, error) {
	sc := i.p.pool.Stash()
	if sc == nil {
		return "", errStashUnconfigured
	}
	return i.p.identifyEndpoint(ctx, sc)
}

func (i pollerSceneIndex) CountScenes(ctx context.Context, endpoint, stashDBID string) (int, error) {
	sc := i.p.pool.Stash()
	if sc == nil {
		return 0, errStashUnconfigured
	}
	refs, err := sc.FindSceneRefsByStashID(ctx, endpoint, stashDBID)
	if err != nil {
		return 0, err
	}
	return countDistinctScenes(refs), nil
}

// countDistinctScenes collapses Stash's one-ref-per-FILE result into scenes.
// The pack-dedup path learned this the hard way: a matching-fingerprint
// re-download is filed as a SECOND file on the same scene, so counting refs
// made one scene look like two. Here only "zero or more than zero" matters,
// but counting the right thing keeps the number honest if it is ever read.
func countDistinctScenes(refs []stash.SceneRef) int {
	seen := map[string]bool{}
	for _, r := range refs {
		if r.SceneID != "" {
			seen[r.SceneID] = true
		}
	}
	return len(seen)
}
