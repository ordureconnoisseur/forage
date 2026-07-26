package poller

import (
	"context"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/stash"
)

// TestDedupPackLogOnlyDestroysNothing pins the dry run: keep="log-only"
// walks the exact same planning path as a real dedup — so the log shows
// precisely what an armed mode would have removed — but zero destroys reach
// Stash. This is the recommended first-week setting for a new install, so
// "destroys nothing" has to be a tested property, not an intention.
func TestDedupPackLogOnlyDestroysNothing(t *testing.T) {
	r := newRig(t, "")
	sc := r.poller.pool.Stash()

	// A clean single-file duplicate — the case an armed "existing" mode
	// WOULD destroy (proven by TestDedupPackStillDropsSingleFileCopy).
	r.stash.set([]fakeScene{
		{id: "pack-1", title: "Scene A", path: "/lib/pack/a.mp4", stashDBID: "sdb-dry"},
		{id: "old-1", title: "Scene A", path: "/lib/old/a.mp4", stashDBID: "sdb-dry"},
	})
	g := &grabs.Grab{ID: 9, PackFiles: 1}
	packScenes := []stash.SceneMatch{{ID: "pack-1", StashDBID: "sdb-dry"}}

	deduped, recorded, err := r.poller.dedupPack(context.Background(), sc, g, packScenes,
		"https://stashdb.org/graphql", "log-only", true)
	if err != nil {
		t.Fatalf("dedupPack: %v", err)
	}
	if got := r.stash.destroys(); len(got) != 0 {
		t.Fatalf("log-only destroyed %v; a dry run must touch nothing", got)
	}
	if deduped != 0 || recorded != 0 {
		t.Fatalf("deduped=%d recorded=%d, want 0/0 — a dry run neither destroys nor records reviews", deduped, recorded)
	}
}
