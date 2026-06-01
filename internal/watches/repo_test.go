package watches

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// schema is the minimal watches DDL — kept in sync with db/schema.sql.
// Testing against it also catches DDL typos.
const schema = `
CREATE TABLE IF NOT EXISTS watches (
  stashdb_id TEXT PRIMARY KEY, title TEXT, date TEXT, studio_name TEXT,
  image_url TEXT, performer_name TEXT, performer_id TEXT,
  target TEXT NOT NULL DEFAULT 'any', status TEXT NOT NULL DEFAULT 'watching',
  found_title TEXT, found_url TEXT, found_indexer TEXT, found_protocol TEXT,
  found_size INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL, last_checked INTEGER NOT NULL DEFAULT 0,
  found_at INTEGER NOT NULL DEFAULT 0,
  ignored_urls TEXT NOT NULL DEFAULT '[]');`

func testRepo(t *testing.T) *Repo {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return NewRepo(db)
}

func TestAddListDelete(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	if err := r.Add(ctx, Watch{StashDBID: "s1", Title: "Scene 1", Target: Target1080}); err != nil {
		t.Fatal(err)
	}
	ws, err := r.List(ctx)
	if err != nil || len(ws) != 1 {
		t.Fatalf("list = %d (%v)", len(ws), err)
	}
	if ws[0].Target != Target1080 || ws[0].Status != StatusWatching {
		t.Errorf("got %+v", ws[0])
	}
	if err := r.Delete(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if ws, _ := r.List(ctx); len(ws) != 0 {
		t.Errorf("expected empty after delete, got %d", len(ws))
	}
}

func TestAddDefaultsTargetAny(t *testing.T) {
	r := testRepo(t)
	_ = r.Add(context.Background(), Watch{StashDBID: "s", Title: "x"})
	ws, _ := r.List(context.Background())
	if ws[0].Target != TargetAny {
		t.Errorf("empty target should default to any, got %q", ws[0].Target)
	}
}

func TestClaimBatchOldestFirst(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	// Three watches; bump one's last_checked so it's NOT the oldest.
	for _, id := range []string{"a", "b", "c"} {
		_ = r.Add(ctx, Watch{StashDBID: id, Title: id})
	}
	r.db.Exec(`UPDATE watches SET last_checked = 999 WHERE stashdb_id = 'b'`)
	got, err := r.ClaimBatch(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("claimed %d, want 2", len(got))
	}
	// b (last_checked=999) must NOT be in the first batch of 2.
	for _, w := range got {
		if w.StashDBID == "b" {
			t.Error("claimed b despite it being most-recently-checked")
		}
	}
	// Claiming stamps last_checked, so a re-claim returns the others.
	got2, _ := r.ClaimBatch(ctx, 2)
	if len(got2) == 0 {
		t.Error("second claim returned nothing")
	}
}

func TestMarkAvailable(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	_ = r.Add(ctx, Watch{StashDBID: "s", Title: "x", Target: Target1080})
	if err := r.MarkAvailable(ctx, "s", "Rel 1080p", "http://dl", "PornoLab", "torrent", 123); err != nil {
		t.Fatal(err)
	}
	ws, _ := r.List(ctx)
	w := ws[0]
	if w.Status != StatusAvailable || w.FoundTitle != "Rel 1080p" || w.FoundSize != 123 {
		t.Errorf("mark available wrong: %+v", w)
	}
	// Available rows are excluded from ClaimBatch (only 'watching').
	if got, _ := r.ClaimBatch(ctx, 10); len(got) != 0 {
		t.Error("available row should not be claimable")
	}
	// CountWatching excludes available too.
	if n, _ := r.CountWatching(ctx); n != 0 {
		t.Errorf("CountWatching = %d, want 0", n)
	}
}

func TestDismiss(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	_ = r.Add(ctx, Watch{StashDBID: "s", Title: "x", Target: Target1080})
	_ = r.MarkAvailable(ctx, "s", "Dead 1080p", "http://dead", "PornoLab", "torrent", 100)

	if err := r.Dismiss(ctx, "s", "http://dead"); err != nil {
		t.Fatal(err)
	}
	ws, _ := r.List(ctx)
	w := ws[0]
	// Flipped back to watching, found fields cleared, URL recorded as ignored.
	if w.Status != StatusWatching {
		t.Errorf("status = %q, want watching", w.Status)
	}
	if w.FoundURL != "" || w.FoundTitle != "" {
		t.Errorf("found fields should be cleared, got %+v", w)
	}
	if len(w.IgnoredURLs) != 1 || w.IgnoredURLs[0] != "http://dead" {
		t.Errorf("ignored urls = %v, want [http://dead]", w.IgnoredURLs)
	}
	// It's claimable again (back to watching).
	if got, _ := r.ClaimBatch(ctx, 10); len(got) != 1 {
		t.Error("dismissed watch should be claimable again")
	}
	// Dismissing a second different release accumulates, not replaces.
	_ = r.MarkAvailable(ctx, "s", "Tiny 1080p", "http://tiny", "1337x", "torrent", 50)
	_ = r.Dismiss(ctx, "s", "http://tiny")
	ws, _ = r.List(ctx)
	if len(ws[0].IgnoredURLs) != 2 {
		t.Errorf("ignored urls = %v, want 2 entries", ws[0].IgnoredURLs)
	}
}
