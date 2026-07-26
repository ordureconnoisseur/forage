package poller

import (
	"context"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/stash"
)

const testEndpoint = "https://stashdb.org/graphql"

// TestSingleDuplicateIgnoresSameSceneExtraFile is the bug: re-downloading a
// scene you already have gives Stash a file with a matching fingerprint, and
// Stash attaches it as a SECOND FILE on the existing scene rather than
// creating a second scene.
//
// FindSceneRefsByStashID emits one ref per file, so counting refs saw "2
// copies" and queued a review. But there's only one scene, so every ref
// belonged to the grab's own side and the row was written with an empty
// existing list — the UI showed "?" where the old file's resolution/size/path
// belong, and neither keep button could do anything (the scene-level destroy
// would take both files, which the resolve endpoint's keptAlive guard
// refuses).
//
// Two fakeScenes sharing an id reproduce the ref shape exactly: the client
// flattens scenes to one ref per file, so both refs carry the same scene id.
func TestSingleDuplicateIgnoresSameSceneExtraFile(t *testing.T) {
	r := newRig(t, "")
	ctx := context.Background()
	sc := r.poller.pool.Stash()

	r.stash.set([]fakeScene{
		{id: "s1", title: "Liora Vane Is Unboxed", path: "/lib/Liora/orig.mp4", stashDBID: "sdb-same"},
		{id: "s1", title: "Liora Vane Is Unboxed", path: "/lib/Liora/redownload.mp4", stashDBID: "sdb-same"},
	})
	g := &grabs.Grab{ID: 1, Kind: "single"}
	scene := &stash.SceneMatch{
		ID: "s1", Title: "Liora Vane Is Unboxed",
		FilePath: "/lib/Liora/redownload.mp4", StashDBID: "sdb-same",
	}

	r.poller.maybeRecordSingleDuplicate(ctx, sc, g, scene)

	dups, err := r.repo.PendingDuplicatesByGrab(ctx, 1)
	if err != nil {
		t.Fatalf("PendingDuplicatesByGrab: %v", err)
	}
	if len(dups) != 0 {
		t.Fatalf("recorded %d review item(s) for a single scene with two files; "+
			"nothing here is actionable, so none should be queued (existing=%+v)",
			len(dups), dups[0].Existing)
	}
}

// TestSingleDuplicateRecordsDistinctScene is the positive control, so the fix
// above can't have simply disabled single-grab duplicate detection: two
// genuinely separate local scenes for one cross-id must still queue a review,
// with the other scene on the existing side.
func TestSingleDuplicateRecordsDistinctScene(t *testing.T) {
	r := newRig(t, "")
	ctx := context.Background()
	sc := r.poller.pool.Stash()

	r.stash.set([]fakeScene{
		{id: "new-1", title: "Scene A", path: "/lib/new/a.mp4", stashDBID: "sdb-two"},
		{id: "old-7", title: "Scene A", path: "/lib/old/a.mp4", stashDBID: "sdb-two"},
	})
	g := &grabs.Grab{ID: 2, Kind: "single"}
	scene := &stash.SceneMatch{
		ID: "new-1", Title: "Scene A",
		FilePath: "/lib/new/a.mp4", StashDBID: "sdb-two",
	}

	r.poller.maybeRecordSingleDuplicate(ctx, sc, g, scene)

	dups, err := r.repo.PendingDuplicatesByGrab(ctx, 2)
	if err != nil {
		t.Fatalf("PendingDuplicatesByGrab: %v", err)
	}
	if len(dups) != 1 {
		t.Fatalf("expected 1 review item for two distinct scenes, got %d", len(dups))
	}
	d := dups[0]
	if d.Pack.SceneID != "new-1" {
		t.Fatalf("this grab's copy = %q, want new-1", d.Pack.SceneID)
	}
	if len(d.Existing) != 1 || d.Existing[0].SceneID != "old-7" {
		t.Fatalf("existing copies = %+v, want [old-7]", d.Existing)
	}
}

// TestRecordReviewDuplicateRefusesEmptyExisting covers the shared writer's own
// guard, which backs up the detector for the pack path too: with nothing on
// the existing side there is no comparison to show and no file to destroy, so
// no row should be written whatever the caller passes.
func TestRecordReviewDuplicateRefusesEmptyExisting(t *testing.T) {
	r := newRig(t, "")
	ctx := context.Background()

	// Every ref belongs to the caller's own side.
	refs := []stash.SceneRef{
		{SceneID: "p1", Title: "Pack Scene", Path: "/lib/pack/a.mp4", Size: 10, Height: 1080},
		{SceneID: "p1", Title: "Pack Scene", Path: "/lib/pack/a.dup.mp4", Size: 10, Height: 1080},
	}
	g := &grabs.Grab{ID: 3, Kind: "pack"}
	ps := stash.SceneMatch{ID: "p1", Title: "Pack Scene", StashDBID: "sdb-empty"}

	wrote, err := r.poller.recordReviewDuplicate(ctx, g, ps, refs, map[string]bool{"p1": true})
	if err != nil {
		t.Fatalf("recordReviewDuplicate: %v", err)
	}
	if wrote {
		t.Error("reported a written row with no existing copies")
	}
	dups, err := r.repo.PendingDuplicatesByGrab(ctx, 3)
	if err != nil {
		t.Fatalf("PendingDuplicatesByGrab: %v", err)
	}
	if len(dups) != 0 {
		t.Fatalf("persisted %d non-actionable review item(s), want 0", len(dups))
	}
}
