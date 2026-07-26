package poller

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ordureconnoisseur/forager/internal/grabs"
)

// settledUnlinked inserts a grab in the state the settle paths leave behind:
// confirmed, a real file in the library, no StashDB cross-id. Returns the id
// and the placed path.
func settledUnlinked(t *testing.T, r *rig, performer, fileName, predicted string) (int64, string) {
	t.Helper()
	placed := filepath.Join(r.libRoot, performer, fileName)
	if err := os.MkdirAll(filepath.Dir(placed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(placed, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	id, err := r.repo.Insert(context.Background(), grabs.Grab{
		ReleaseTitle: fileName, Client: "qbit", ClientID: fileName + "hash",
		Category: "forager", Status: "confirmed", PlacedPath: placed,
		PerformerName: performer, Kind: "single",
		Reason: "in library (scanned)",
		// no ActualStashDBID — that is the whole point
		PredictedStashDBID: predicted,
		CompletedAt:        now - 3600,
		ConfirmedAt:        now - 1800,
		GrabbedAt:          now - 7200,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	return id, placed
}

// forceReconcile clears the slow-cadence gate so the next tick runs a
// reconcile pass (the pass is deliberately 15-minutely in production).
func forceReconcile(r *rig) {
	r.poller.lastReconcile = time.Time{}
	r.poller.reconcileCursor = 0
}

// TestReconcileBackfillsLateIdentify is the bug this pass exists for: an
// adopted single settles as confirmed "in library (scanned)" because Stash's
// queued identify hadn't run yet. When it later lands, the scene carries a
// cross-id while the grab still says it was never matched — so the Grabs list
// badged an identified scene NO MATCH. The reconcile pass must adopt the link.
func TestReconcileBackfillsLateIdentify(t *testing.T) {
	r := newRig(t, "forager")

	id, placed := settledUnlinked(t, r, "Late Identify", "sis-loves-me_full_1080.mp4", "")

	// Still unidentified: a pass must change nothing and must not invent a link.
	r.stash.set([]fakeScene{{id: "300", title: "", path: placed, stashDBID: ""}})
	forceReconcile(r)
	r.tick(t)
	if g := r.get(t, id); g.ActualStashDBID != "" {
		t.Fatalf("unidentified scene: actual_stashdb_id=%q, want empty", g.ActualStashDBID)
	}

	// Stash's identify finally runs and attaches the cross-id.
	r.stash.set([]fakeScene{{id: "300", title: "Sis Loves Me", path: placed, stashDBID: "sdb-late-1"}})
	forceReconcile(r)
	r.tick(t)

	g := r.get(t, id)
	if g.ActualStashDBID != "sdb-late-1" {
		t.Fatalf("actual_stashdb_id=%q, want the late cross-id", g.ActualStashDBID)
	}
	if g.Status != "confirmed" {
		t.Fatalf("status=%q, want confirmed", g.Status)
	}
	if g.Reason == "in library (scanned)" {
		t.Errorf("reason still %q — it no longer describes the grab", g.Reason)
	}
}

// TestReconcileRespectsCadence guards the cost: the pass is catch-up work, so
// a tick inside reconcileInterval must not re-query Stash for every settled
// grab. Without the gate every 90s tick would cost one lookup per unlinked
// grab, forever.
func TestReconcileRespectsCadence(t *testing.T) {
	r := newRig(t, "forager")
	_, placed := settledUnlinked(t, r, "Cadence", "cadence_clip.mp4", "")
	r.stash.set([]fakeScene{{id: "301", title: "", path: placed, stashDBID: ""}})

	// The grab is confirmed, so it's out of Active() — nothing else in a tick
	// talks to Stash here, making reqCount a clean proxy for reconcile work.
	forceReconcile(r)
	r.tick(t)
	after := r.stash.reqCount()
	if after == 0 {
		t.Fatal("first tick made no Stash request at all; reconcile never ran")
	}
	// Second tick, gate NOT cleared: no further requests.
	r.tick(t)
	if got := r.stash.reqCount(); got != after {
		t.Fatalf("Stash requests grew %d -> %d inside reconcileInterval; cadence gate not holding", after, got)
	}
}

// TestReconcileLateLinkToDifferentSceneMismatches covers the predicted case: a
// grab that gave up as "in library; no StashDB match" and is later identified
// as a DIFFERENT scene is a mismatch, not a clean confirm. Same verdict the
// on-time confirm path reaches, so the row offers pick-another-release.
func TestReconcileLateLinkToDifferentSceneMismatches(t *testing.T) {
	r := newRig(t, "forager")

	id, placed := settledUnlinked(t, r, "Wrong Scene", "wrong_scene_1080.mp4", "sdb-predicted")
	r.stash.set([]fakeScene{{id: "302", title: "Something Else", path: placed, stashDBID: "sdb-other"}})

	forceReconcile(r)
	r.tick(t)

	g := r.get(t, id)
	if g.ActualStashDBID != "sdb-other" {
		t.Fatalf("actual_stashdb_id=%q, want sdb-other", g.ActualStashDBID)
	}
	if g.Status != "mismatched" {
		t.Fatalf("status=%q, want mismatched (late link went to a different scene)", g.Status)
	}
}

// TestReconcileSkipsPacks: a pack never carries a single cross-id, and
// advancePackConfirm owns its per-scene identify state. The reconcile pass
// must leave packs alone rather than stamping one scene's id onto the whole
// pack grab.
func TestReconcileSkipsPacks(t *testing.T) {
	r := newRig(t, "forager")
	ctx := context.Background()

	packDir := filepath.Join(r.libRoot, "Pack Performer")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	id, err := r.repo.Insert(ctx, grabs.Grab{
		ReleaseTitle: "Pack Performer Megapack", Client: "qbit", ClientID: "packhash",
		Category: "forager", Status: "confirmed", PlacedPath: packDir,
		PerformerName: "Pack Performer", Kind: "pack", Reason: "pack confirmed",
		CompletedAt: now - 3600, ConfirmedAt: now - 1800, GrabbedAt: now - 7200,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	r.stash.set([]fakeScene{{
		id: "303", title: "One Pack Scene",
		path: filepath.Join(packDir, "scene1.mp4"), stashDBID: "sdb-pack-scene",
	}})

	forceReconcile(r)
	r.tick(t)

	if g := r.get(t, id); g.ActualStashDBID != "" {
		t.Fatalf("pack got actual_stashdb_id=%q; reconcile must skip packs", g.ActualStashDBID)
	}
}

// TestReconcileWindowExcludesStale bounds the query: a file still unlinked
// after reconcileWindow genuinely isn't on StashDB, and re-checking it forever
// costs a Stash round-trip per pass to learn nothing.
func TestReconcileWindowExcludesStale(t *testing.T) {
	r := newRig(t, "forager")
	ctx := context.Background()

	placed := filepath.Join(r.libRoot, "Ancient", "ancient_clip.mp4")
	if err := os.MkdirAll(filepath.Dir(placed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(placed, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-reconcileWindow - 24*time.Hour).Unix()
	id, err := r.repo.Insert(ctx, grabs.Grab{
		ReleaseTitle: "ancient clip", Client: "qbit", ClientID: "ancienthash",
		Category: "forager", Status: "confirmed", PlacedPath: placed,
		PerformerName: "Ancient", Kind: "single", Reason: "in library (scanned)",
		CompletedAt: old, ConfirmedAt: old, GrabbedAt: old,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Even though Stash now has a cross-id, this row is out of window.
	r.stash.set([]fakeScene{{id: "304", title: "Ancient", path: placed, stashDBID: "sdb-ancient"}})

	forceReconcile(r)
	r.tick(t)

	if g := r.get(t, id); g.ActualStashDBID != "" {
		t.Fatalf("out-of-window grab was reconciled (actual=%q); window not applied", g.ActualStashDBID)
	}
}
