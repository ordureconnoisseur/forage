package grabs

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/db"
)

func newTestRepo(t *testing.T) *Repo {
	t.Helper()
	dbh, err := db.Open(filepath.Join(t.TempDir(), "grabs.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { dbh.Close() })
	return NewRepo(dbh)
}

// TestUpdateOptimisticLock pins the rev compare-and-set: two readers load the
// same row; the first write wins and bumps rev; the second (stale) write is
// rejected with ErrStaleUpdate rather than silently clobbering — the
// poller-vs-API lost-update race. A fresh re-load can write again.
func TestUpdateOptimisticLock(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()
	id, err := r.Insert(ctx, Grab{ReleaseTitle: "x", Client: "qbit", Status: "queued", GrabbedAt: 1})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	g1, _ := r.Get(ctx, id)
	g2, _ := r.Get(ctx, id) // a second actor holding the same rev
	if g1 == nil || g2 == nil {
		t.Fatal("get returned nil")
	}

	g1.Status = "downloading"
	if err := r.Update(ctx, *g1); err != nil {
		t.Fatalf("first update should win: %v", err)
	}

	// Stale write (rev advanced under it) must be rejected — not clobber.
	g2.Status = "failed"
	if err := r.Update(ctx, *g2); !errors.Is(err, ErrStaleUpdate) {
		t.Fatalf("stale update: got %v, want ErrStaleUpdate", err)
	}

	got, _ := r.Get(ctx, id)
	if got.Status != "downloading" {
		t.Fatalf("status=%q, want downloading (stale write must not clobber)", got.Status)
	}

	// A fresh load carries the new rev and can write again.
	got.Status = "completed"
	if err := r.Update(ctx, *got); err != nil {
		t.Fatalf("fresh update should succeed: %v", err)
	}
}
