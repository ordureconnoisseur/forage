package grabs

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
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

// seedGrab inserts one grab, failing the test on error.
func seedGrab(t *testing.T, r *Repo, g Grab) {
	t.Helper()
	if _, err := r.Insert(context.Background(), g); err != nil {
		t.Fatalf("insert %q: %v", g.ReleaseTitle, err)
	}
}

// TestListFilter pins the status + free-text search the Grabs history view
// relies on: q matches release_title / performer_name / release_indexer /
// client_name case-insensitively, status narrows independently, and both
// combine — so an old grab is findable server-side without paging the whole
// table into the client.
func TestListFilter(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()

	// grabbed_at ascending so newest-first ordering is deterministic. The
	// confirmed rows carry a cross-id because 'confirmed' now means "landed
	// AND linked to a StashDB scene" — without one they'd fall under the
	// 'unmatched' half of the split, which is a different test's subject.
	seedGrab(t, r, Grab{ReleaseTitle: "Ninounini DOG SHIT", PerformerName: "Ninounini", ReleaseIndexer: "empornium", Status: "confirmed", ActualStashDBID: "sdb-nin", GrabbedAt: 100})
	seedGrab(t, r, Grab{ReleaseTitle: "Emma Vai Pack", PerformerName: "Emma Vai", ReleaseIndexer: "pornbay", Status: "confirmed", ActualStashDBID: "sdb-emma", GrabbedAt: 200})
	seedGrab(t, r, Grab{ReleaseTitle: "Random Studio Scene", PerformerName: "Someone", ReleaseIndexer: "empornium", Status: "failed", GrabbedAt: 300, ClientName: "ninou_bonus.mp4"})

	all, err := r.List(ctx, "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("unfiltered list = %d, want 3", len(all))
	}
	// Newest (highest grabbed_at) first.
	if all[0].ReleaseTitle != "Random Studio Scene" {
		t.Errorf("order: got %q first, want newest", all[0].ReleaseTitle)
	}

	// Status narrows.
	if got, _ := r.List(ctx, "confirmed", "", 0, 0); len(got) != 2 {
		t.Errorf("status=confirmed = %d, want 2", len(got))
	}

	// q matches across the whole table, case-insensitive. "ninou" hits the
	// Ninounini title/performer AND the client_name of the failed studio grab
	// (ninou_bonus.mp4) — proving client_name is searched.
	if got, _ := r.List(ctx, "", "ninou", 0, 0); len(got) != 2 {
		t.Fatalf("q=ninou = %d, want 2 (title/performer and client_name)", len(got))
	}

	// q matches performer only.
	if got, _ := r.List(ctx, "", "emma", 0, 0); len(got) != 1 || got[0].PerformerName != "Emma Vai" {
		t.Errorf("q=emma = %+v, want the Emma Vai grab", got)
	}

	// q matches indexer.
	if got, _ := r.List(ctx, "", "empornium", 0, 0); len(got) != 2 {
		t.Errorf("q=empornium = %d, want 2", len(got))
	}

	// status + q combine (AND): only the confirmed Ninounini grab, not the
	// failed studio grab whose client_name also contains "ninou".
	if got, _ := r.List(ctx, "confirmed", "ninou", 0, 0); len(got) != 1 || got[0].ReleaseTitle != "Ninounini DOG SHIT" {
		t.Errorf("status=confirmed q=ninou = %+v, want just the confirmed Ninounini grab", got)
	}
}

// TestCountFilteredMatchesFilter pins that CountFiltered counts the SAME set
// List returns (minus paging), so the UI's result count and end-of-list
// detection stay correct when matches exceed one page.
func TestCountFilteredMatchesFilter(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()

	for i := 0; i < 250; i++ {
		status := "confirmed"
		if i%5 == 0 {
			status = "failed"
		}
		// Cross-id on the confirmed rows: see TestListFilter — a confirmed
		// grab without one counts as 'unmatched' under the split.
		seedGrab(t, r, Grab{ReleaseTitle: "Scene", PerformerName: "Perf", Status: status,
			ActualStashDBID: "sdb-" + strconv.Itoa(i), GrabbedAt: int64(i + 1)})
	}

	if n, _ := r.CountFiltered(ctx, "", ""); n != 250 {
		t.Errorf("count all = %d, want 250", n)
	}
	// 50 of the 250 are 'failed' (every 5th).
	if n, _ := r.CountFiltered(ctx, "failed", ""); n != 50 {
		t.Errorf("count failed = %d, want 50", n)
	}
	// Count is independent of the list's per-page cap: the page is capped at
	// 100 but the count reflects the full 200-row match set behind it.
	page, _ := r.List(ctx, "confirmed", "", 100, 0)
	total, _ := r.CountFiltered(ctx, "confirmed", "")
	if len(page) != 100 {
		t.Fatalf("page = %d, want 100", len(page))
	}
	if total != 200 {
		t.Errorf("count confirmed = %d, want 200 (more than one page)", total)
	}
}
