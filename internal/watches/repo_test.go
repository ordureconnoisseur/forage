package watches

import (
	"context"
	"database/sql"
	"encoding/json"
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
  ignored_urls TEXT NOT NULL DEFAULT '[]',
  batch_id TEXT NOT NULL DEFAULT '', batch_label TEXT NOT NULL DEFAULT '',
  candidates TEXT NOT NULL DEFAULT '[]', grabbed_at INTEGER NOT NULL DEFAULT 0);`

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
	if err := r.MarkAvailable(ctx, "s", "Rel 1080p", "http://dl", "PornoLab", "torrent", 123, nil); err != nil {
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
	_ = r.MarkAvailable(ctx, "s", "Dead 1080p", "http://dead", "PornoLab", "torrent", 100, nil)

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
	_ = r.MarkAvailable(ctx, "s", "Tiny 1080p", "http://tiny", "1337x", "torrent", 50, nil)
	_ = r.Dismiss(ctx, "s", "http://tiny")
	ws, _ = r.List(ctx)
	if len(ws[0].IgnoredURLs) != 2 {
		t.Errorf("ignored urls = %v, want 2 entries", ws[0].IgnoredURLs)
	}
}

func TestAddBatchAndDeleteBatch(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	batch := []Watch{
		{StashDBID: "a", Title: "A", BatchID: "b1", BatchLabel: "Avery Black"},
		{StashDBID: "b", Title: "B", BatchID: "b1", BatchLabel: "Avery Black"},
		{StashDBID: "c", Title: "C", BatchID: "b1", BatchLabel: "Avery Black"},
	}
	if err := r.AddBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	// A separate ungrouped single track that DeleteBatch must never touch.
	_ = r.Add(ctx, Watch{StashDBID: "loose", Title: "Loose"})

	ws, _ := r.List(ctx)
	if len(ws) != 4 {
		t.Fatalf("expected 4 watches, got %d", len(ws))
	}
	for _, w := range ws {
		if w.StashDBID != "loose" && (w.BatchID != "b1" || w.BatchLabel != "Avery Black") {
			t.Errorf("batch fields not persisted: %+v", w)
		}
	}

	// Empty batch id must be a no-op, NOT a wipe of all ungrouped rows.
	if err := r.DeleteBatch(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if ws, _ := r.List(ctx); len(ws) != 4 {
		t.Fatalf("DeleteBatch(\"\") wiped rows: %d left", len(ws))
	}

	if err := r.DeleteBatch(ctx, "b1"); err != nil {
		t.Fatal(err)
	}
	ws, _ = r.List(ctx)
	if len(ws) != 1 || ws[0].StashDBID != "loose" {
		t.Fatalf("after DeleteBatch only the loose watch should remain, got %+v", ws)
	}
}

func TestMarkGrabbedLingers(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	_ = r.Add(ctx, Watch{StashDBID: "s", Title: "x", Target: Target1080, BatchID: "b1"})
	_ = r.MarkAvailable(ctx, "s", "Rel 1080p", "http://dl", "PornoLab", "torrent", 123, nil)

	if err := r.MarkGrabbed(ctx, "s", "Rel 1080p", "http://dl", "PornoLab", "torrent", 123); err != nil {
		t.Fatal(err)
	}
	ws, _ := r.List(ctx)
	if len(ws) != 1 {
		t.Fatalf("grabbed watch must linger, got %d rows", len(ws))
	}
	w := ws[0]
	if w.Status != StatusGrabbed {
		t.Errorf("status = %q, want grabbed", w.Status)
	}
	if w.GrabbedAt == 0 {
		t.Errorf("grabbed_at should be set")
	}
	// Grabbed rows are not claimable and not counted as watching/available.
	if got, _ := r.ClaimBatch(ctx, 10); len(got) != 0 {
		t.Error("grabbed row should not be claimable")
	}
	if n, _ := r.CountAvailable(ctx); n != 0 {
		t.Errorf("CountAvailable = %d, want 0 (grabbed is not available)", n)
	}
}

func TestCandidatesRoundTrip(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	_ = r.Add(ctx, Watch{StashDBID: "s", Title: "x"})
	cands := json.RawMessage(`[{"title":"Rel 4k","download_url":"http://a"},{"title":"Rel 1080p","download_url":"http://b"}]`)
	if err := r.MarkAvailable(ctx, "s", "Rel 4k", "http://a", "ix", "torrent", 1, cands); err != nil {
		t.Fatal(err)
	}
	ws, _ := r.List(ctx)
	var got []map[string]any
	if err := json.Unmarshal(ws[0].Candidates, &got); err != nil {
		t.Fatalf("candidates not valid JSON: %v (%s)", err, ws[0].Candidates)
	}
	if len(got) != 2 || got[0]["title"] != "Rel 4k" {
		t.Errorf("candidates round-trip wrong: %s", ws[0].Candidates)
	}
}
