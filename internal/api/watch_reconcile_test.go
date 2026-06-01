package api

import (
	"context"
	"io"
	"log/slog"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/ordureconnoisseur/forager/internal/db"
	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/watches"
)

// TestReconcileWatches verifies a watch is dropped once forage has a grab
// for its scene (by any path — not just the Watching tab's grab button),
// while a watch for an un-grabbed scene survives. This is the fix for a
// scene you downloaded elsewhere lingering in Watching forever.
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
	got := map[string]bool{}
	for _, w := range list {
		got[w.StashDBID] = true
	}
	if got["grabbed-scene"] {
		t.Error("watch for an already-grabbed scene should have been removed")
	}
	if !got["still-waiting"] {
		t.Error("watch for an un-grabbed scene should survive reconcile")
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
