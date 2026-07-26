package poller

import (
	"context"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/stash"
)

// TestDedupPackRefusesMultiFileOriginal covers the automatic destroy — the
// only unattended deletion in the codebase, armed whenever packDedupKeep is
// "existing" or "pack". keep="pack" drops the ORIGINAL library copies; if
// one of those scenes holds two files (Stash attaches a matching-
// fingerprint re-download as a second FILE on the existing scene), a
// scene-level destroy takes both. The vetted plan must refuse it, and the
// pack's own single-file copy path must be unaffected.
func TestDedupPackRefusesMultiFileOriginal(t *testing.T) {
	r := newRig(t, "")
	sc := r.poller.pool.Stash()

	// One pack scene (pack-1) whose cross-id also exists on an EXTERNAL
	// original (old-1) that carries TWO files. Two fakeScenes sharing an id
	// produce exactly the one-ref-per-file shape FindSceneRefsByStashID
	// returns for a multi-file scene.
	r.stash.set([]fakeScene{
		{id: "pack-1", title: "Scene A", path: "/lib/pack/a.mp4", stashDBID: "sdb-multi"},
		{id: "old-1", title: "Scene A", path: "/lib/old/a.mp4", stashDBID: "sdb-multi"},
		{id: "old-1", title: "Scene A", path: "/lib/old/a-redl.mp4", stashDBID: "sdb-multi"},
	})
	g := &grabs.Grab{ID: 1, PackFiles: 1}
	packScenes := []stash.SceneMatch{{ID: "pack-1", StashDBID: "sdb-multi"}}

	deduped, _, err := r.poller.dedupPack(context.Background(), sc, g, packScenes,
		"https://stashdb.org/graphql", "pack", true)
	if err != nil {
		t.Fatalf("dedupPack: %v", err)
	}
	if got := r.stash.destroys(); len(got) != 0 {
		t.Fatalf("destroyed %v; the two-file original must be refused", got)
	}
	if deduped != 0 {
		t.Fatalf("deduped = %d, want 0", deduped)
	}
}

// TestDedupPackStillDropsSingleFileCopy is the control: with keep="existing"
// and everything single-file, the pack's own duplicate copy must still be
// destroyed — the guard must not have turned automatic dedup off.
func TestDedupPackStillDropsSingleFileCopy(t *testing.T) {
	r := newRig(t, "")
	sc := r.poller.pool.Stash()

	r.stash.set([]fakeScene{
		{id: "pack-1", title: "Scene A", path: "/lib/pack/a.mp4", stashDBID: "sdb-one"},
		{id: "old-1", title: "Scene A", path: "/lib/old/a.mp4", stashDBID: "sdb-one"},
	})
	g := &grabs.Grab{ID: 2, PackFiles: 1}
	packScenes := []stash.SceneMatch{{ID: "pack-1", StashDBID: "sdb-one"}}

	deduped, _, err := r.poller.dedupPack(context.Background(), sc, g, packScenes,
		"https://stashdb.org/graphql", "existing", true)
	if err != nil {
		t.Fatalf("dedupPack: %v", err)
	}
	got := r.stash.destroys()
	if len(got) != 1 || got[0] != "pack-1" {
		t.Fatalf("destroyed %v, want exactly the pack's own copy (pack-1)", got)
	}
	if deduped != 1 {
		t.Fatalf("deduped = %d, want 1", deduped)
	}
}

// TestDedupPackRefusesMultiFilePackCopy: the mirror case — keep="existing"
// drops the PACK's copy, and if the pack scene itself somehow holds two
// files, destroying it is refused too. The invariant is per-scene,
// whichever side is being dropped.
func TestDedupPackRefusesMultiFilePackCopy(t *testing.T) {
	r := newRig(t, "")
	sc := r.poller.pool.Stash()

	r.stash.set([]fakeScene{
		{id: "pack-1", title: "Scene A", path: "/lib/pack/a.mp4", stashDBID: "sdb-mfp"},
		{id: "pack-1", title: "Scene A", path: "/lib/pack/a-extra.mp4", stashDBID: "sdb-mfp"},
		{id: "old-1", title: "Scene A", path: "/lib/old/a.mp4", stashDBID: "sdb-mfp"},
	})
	g := &grabs.Grab{ID: 3, PackFiles: 1}
	packScenes := []stash.SceneMatch{{ID: "pack-1", StashDBID: "sdb-mfp"}}

	deduped, _, err := r.poller.dedupPack(context.Background(), sc, g, packScenes,
		"https://stashdb.org/graphql", "existing", true)
	if err != nil {
		t.Fatalf("dedupPack: %v", err)
	}
	if got := r.stash.destroys(); len(got) != 0 {
		t.Fatalf("destroyed %v; the two-file pack copy must be refused", got)
	}
	if deduped != 0 {
		t.Fatalf("deduped = %d, want 0", deduped)
	}
}
