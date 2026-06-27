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

// TestHasLiveGrabForRelease pins the adoption duplicate guard: a release with
// a live grab (any non-failed status) reports as already-held so adoption
// skips re-placing it; a failed prior attempt does not, so a genuine re-grab
// still proceeds; an unrelated title is unaffected.
func TestHasLiveGrabForRelease(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()
	title := "Onlyfans.Siterip.Ari.Kytsya._video.001"

	if has, err := r.HasLiveGrabForRelease(ctx, title); err != nil || has {
		t.Fatalf("no grab yet: got has=%v err=%v, want false/nil", has, err)
	}

	id, err := r.Insert(ctx, Grab{ReleaseTitle: title, Client: "sabnzbd", Status: "placed", GrabbedAt: 1})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if has, err := r.HasLiveGrabForRelease(ctx, title); err != nil || !has {
		t.Fatalf("placed grab: got has=%v err=%v, want true/nil", has, err)
	}
	// An unrelated title must not match.
	if has, _ := r.HasLiveGrabForRelease(ctx, title+".002"); has {
		t.Fatal("different title must not report as held")
	}
	// Empty title is a no-op (never blocks).
	if has, _ := r.HasLiveGrabForRelease(ctx, ""); has {
		t.Fatal("empty title must not report as held")
	}

	// A failed prior attempt is NOT live — a re-grab should be allowed.
	g, _ := r.Get(ctx, id)
	g.Status = "failed"
	if err := r.Update(ctx, *g); err != nil {
		t.Fatalf("update to failed: %v", err)
	}
	if has, err := r.HasLiveGrabForRelease(ctx, title); err != nil || has {
		t.Fatalf("failed grab: got has=%v err=%v, want false/nil (re-grab allowed)", has, err)
	}
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
