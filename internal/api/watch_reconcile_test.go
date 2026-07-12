package api

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ordureconnoisseur/forager/internal/db"
	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/stash"
	"github.com/ordureconnoisseur/forager/internal/watches"
)

// TestReconcileWatches verifies a watch flips to 'grabbed' once forage has a
// grab for its scene (by any path — not just the Watching tab's grab button),
// while a watch for an un-grabbed scene stays 'watching'. The grabbed watch
// LINGERS (not deleted) so its batch progress reads correctly; the user
// clears it (or the batch) later.
func TestReconcileWatches(t *testing.T) {
	ctx := context.Background()
	// A real, fully-migrated DB so both repos share one schema.
	dbh, err := db.Open(t.TempDir() + "/forager.db")
	if err != nil {
		t.Fatal(err)
	}
	defer dbh.Close()

	grabsRepo := grabs.NewRepo(dbh)
	watchesRepo := watches.NewRepo(dbh)
	s := &Server{
		db:      dbh,
		grabs:   grabsRepo,
		watches: watchesRepo,
		log:     slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{})),
	}

	// Two watches: one whose scene we'll grab, one we won't.
	if err := watchesRepo.Add(ctx, watches.Watch{StashDBID: "grabbed-scene", Title: "Got it", Target: watches.TargetAny}); err != nil {
		t.Fatal(err)
	}
	if err := watchesRepo.Add(ctx, watches.Watch{StashDBID: "still-waiting", Title: "Pending", Target: watches.TargetAny}); err != nil {
		t.Fatal(err)
	}

	// A confirmed grab for the first scene, obtained outside the watch flow
	// (actual_stashdb_id set, as the poller would on phash confirm).
	if _, err := grabsRepo.Insert(ctx, grabs.Grab{
		ReleaseTitle:    "Some.Release.1080p",
		ActualStashDBID: "grabbed-scene",
		Status:          "confirmed",
	}); err != nil {
		t.Fatal(err)
	}

	s.reconcileWatches(ctx)

	list, err := watchesRepo.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, w := range list {
		status[w.StashDBID] = w.Status
	}
	// Both watches survive (nothing is deleted by reconcile now), but the
	// grabbed scene's watch flips to terminal 'grabbed' while the other stays.
	if status["grabbed-scene"] != watches.StatusGrabbed {
		t.Errorf("watch for an already-grabbed scene = %q, want grabbed", status["grabbed-scene"])
	}
	if status["still-waiting"] != watches.StatusWatching {
		t.Errorf("watch for an un-grabbed scene = %q, want watching", status["still-waiting"])
	}
}

// TestReconcileWatchesRevertsStranded verifies the reverse reconcile: a
// 'grabbed' watch whose grab died (failed async add, deleted row) without
// the scene landing flips back to 'watching', while grabbed watches with a
// live grab or an owned copy stay terminal. MarkGrabbed fires as soon as the
// queued row exists, so this is the only recovery when the add fails later.
func TestReconcileWatchesRevertsStranded(t *testing.T) {
	ctx := context.Background()
	dbh, err := db.Open(t.TempDir() + "/f3.db")
	if err != nil {
		t.Fatal(err)
	}
	defer dbh.Close()

	grabsRepo := grabs.NewRepo(dbh)
	watchesRepo := watches.NewRepo(dbh)
	s := &Server{
		db:      dbh,
		grabs:   grabsRepo,
		watches: watchesRepo,
		log:     slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{})),
		// Pre-warm the owned memo so the revert path needs no Stash client:
		// "owned-scene" landed in the library, the others didn't.
		ownedCopies:        map[string][]stash.SceneRef{"owned-scene": {{SceneID: "1"}}},
		ownedCopiesFetched: time.Now(),
	}

	for _, id := range []string{"stranded-scene", "owned-scene", "live-scene", "mismatch-scene"} {
		if err := watchesRepo.Add(ctx, watches.Watch{StashDBID: id, Title: id, Target: watches.TargetAny}); err != nil {
			t.Fatal(err)
		}
		if err := watchesRepo.MarkGrabbed(ctx, id, "Rel", "http://dl/"+id, "idx", "torrent", 1); err != nil {
			t.Fatal(err)
		}
	}
	// live-scene still has an in-flight grab; stranded-scene's grab FAILED
	// (excluded from the live set); owned-scene has no grab row at all but
	// its copy is in the library; mismatch-scene's grab identified as a
	// DIFFERENT scene and sits pending in the mismatch review.
	if _, err := grabsRepo.Insert(ctx, grabs.Grab{
		ReleaseTitle: "Live.Release.1080p", PredictedStashDBID: "live-scene", Status: "queued",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := grabsRepo.Insert(ctx, grabs.Grab{
		ReleaseTitle: "Dead.Release.1080p", PredictedStashDBID: "stranded-scene", Status: "failed",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := grabsRepo.Insert(ctx, grabs.Grab{
		ReleaseTitle: "Dup.Listing.1080p", PredictedStashDBID: "mismatch-scene",
		ActualStashDBID: "some-other-scene", Status: "mismatched",
	}); err != nil {
		t.Fatal(err)
	}

	s.reconcileWatches(ctx)

	list, err := watchesRepo.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, w := range list {
		status[w.StashDBID] = w.Status
	}
	if status["stranded-scene"] != watches.StatusWatching {
		t.Errorf("stranded watch = %q, want watching (grab died, scene never landed)", status["stranded-scene"])
	}
	if status["owned-scene"] != watches.StatusGrabbed {
		t.Errorf("owned watch = %q, want grabbed (scene landed anyway)", status["owned-scene"])
	}
	if status["live-scene"] != watches.StatusGrabbed {
		t.Errorf("live watch = %q, want grabbed (grab still in flight)", status["live-scene"])
	}
	// The core of the mismatch-hold contract: a pending, unresolved
	// mismatch keeps its watch quiet — no revert, no re-hunt, until the
	// user's verdict (redo/delete) removes the coverage.
	if status["mismatch-scene"] != watches.StatusGrabbed {
		t.Errorf("mismatch watch = %q, want grabbed (held pending review)", status["mismatch-scene"])
	}

	// The user resolves the mismatch by deleting/redoing the grab: coverage
	// disappears and the next reconcile resumes the hunt automatically.
	if _, err := dbh.ExecContext(ctx, `DELETE FROM grabs WHERE predicted_stashdb_id = 'mismatch-scene'`); err != nil {
		t.Fatal(err)
	}
	s.reconcileWatches(ctx)
	list, _ = watchesRepo.List(ctx)
	for _, w := range list {
		if w.StashDBID == "mismatch-scene" && w.Status != watches.StatusWatching {
			t.Errorf("after resolving the mismatch: watch = %q, want watching (hunt resumes)", w.Status)
		}
	}
}

// sanity: an empty grab set is a no-op (no panic, nothing removed).
func TestReconcileWatchesNoGrabs(t *testing.T) {
	ctx := context.Background()
	mdb, err := db.Open(t.TempDir() + "/f2.db")
	if err != nil {
		t.Fatal(err)
	}
	defer mdb.Close()
	s := &Server{
		db:      mdb,
		grabs:   grabs.NewRepo(mdb),
		watches: watches.NewRepo(mdb),
		log:     slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{})),
	}
	if err := s.watches.Add(ctx, watches.Watch{StashDBID: "x", Title: "X", Target: watches.TargetAny}); err != nil {
		t.Fatal(err)
	}
	s.reconcileWatches(ctx) // no grabs → nothing removed
	list, _ := s.watches.List(ctx)
	if len(list) != 1 {
		t.Errorf("expected 1 surviving watch, got %d", len(list))
	}
}
