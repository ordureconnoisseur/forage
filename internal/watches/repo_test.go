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
  candidates TEXT NOT NULL DEFAULT '[]', grabbed_at INTEGER NOT NULL DEFAULT 0,
  performers TEXT NOT NULL DEFAULT '[]',
  search_count INTEGER NOT NULL DEFAULT 0);`

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

// TestClaimBatchRoundRobin pins the fairness fix: a huge batch must NOT starve
// the ungrouped singles. With everything unchecked (last_checked=0) and the big
// batch inserted FIRST (lower created_at), the old flat oldest-first claim
// returned only big-batch rows; round-robin must split the budget across groups.
func TestClaimBatchRoundRobin(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	// A 6-row batch, created first (lower created_at than the singles).
	for i, id := range []string{"big1", "big2", "big3", "big4", "big5", "big6"} {
		_ = r.Add(ctx, Watch{StashDBID: id, Title: id, BatchID: "big", BatchLabel: "Big", CreatedAt: int64(100 + i)})
	}
	// Two ungrouped singles added AFTER (higher created_at) — these are the
	// rows the old flat claim left at the back forever.
	_ = r.Add(ctx, Watch{StashDBID: "s1", Title: "s1", CreatedAt: 200})
	_ = r.Add(ctx, Watch{StashDBID: "s2", Title: "s2", CreatedAt: 201})

	got, err := r.ClaimBatch(ctx, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("claimed %d, want 4", len(got))
	}
	var singles, big int
	for _, w := range got {
		if w.BatchID == "" {
			singles++
		} else {
			big++
		}
	}
	// Round-robin between two groups → an even split, not 4 big + 0 singles.
	if singles != 2 || big != 2 {
		t.Errorf("round-robin split = %d singles / %d big, want 2 / 2 (singles starved?)", singles, big)
	}
}

// TestSearchCountIncrements pins the search-count badge data: each claim and
// each manual MarkChecked bumps search_count, so the Watching tab can show how
// many times a still-unfound scene has been searched.
func TestSearchCountIncrements(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	_ = r.Add(ctx, Watch{StashDBID: "s", Title: "x"})
	if ws, _ := r.List(ctx); ws[0].SearchCount != 0 {
		t.Fatalf("fresh watch search_count = %d, want 0", ws[0].SearchCount)
	}
	// A claim counts as a search.
	if _, err := r.ClaimBatch(ctx, 5); err != nil {
		t.Fatal(err)
	}
	// Reset last_checked so the next claim re-selects it.
	r.db.Exec(`UPDATE watches SET last_checked = 0 WHERE stashdb_id = 's'`)
	if _, err := r.ClaimBatch(ctx, 5); err != nil {
		t.Fatal(err)
	}
	// And a manual search-now bumps it too.
	if err := r.MarkChecked(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if ws, _ := r.List(ctx); ws[0].SearchCount != 3 {
		t.Errorf("search_count = %d, want 3 (2 claims + 1 MarkChecked)", ws[0].SearchCount)
	}
}

// TestResetUngrabbableAvailable pins the self-heal: an available row with an
// empty found_url is reset to watching (and re-queued via last_checked=0) while
// keeping its batch; a valid available row (real found_url) is left untouched.
func TestResetUngrabbableAvailable(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	_ = r.Add(ctx, Watch{StashDBID: "good", Title: "g", BatchID: "b1", BatchLabel: "B"})
	_ = r.MarkAvailable(ctx, "good", "rel", "http://x", "ix", "torrent", 1, nil)
	_ = r.Add(ctx, Watch{StashDBID: "bad", Title: "b", BatchID: "b1", BatchLabel: "B"})
	_ = r.MarkAvailable(ctx, "bad", "rel2", "", "Knaben", "torrent", 1, nil) // empty url = invalid

	n, err := r.ResetUngrabbableAvailable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reset %d rows, want 1 (only the empty-url one)", n)
	}
	ws, _ := r.List(ctx)
	for _, w := range ws {
		switch w.StashDBID {
		case "bad":
			if w.Status != StatusWatching {
				t.Errorf("bad should be back to watching, got %q", w.Status)
			}
			if w.LastChecked != 0 {
				t.Errorf("bad should re-queue (last_checked=0), got %d", w.LastChecked)
			}
			if w.BatchID != "b1" {
				t.Errorf("bad lost its batch: %q", w.BatchID)
			}
		case "good":
			if w.Status != StatusAvailable {
				t.Errorf("good (real url) must stay available, got %q", w.Status)
			}
		}
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

	if err := r.Dismiss(ctx, "s", "http://dead", "Dead 1080p"); err != nil {
		t.Fatal(err)
	}
	ws, _ := r.List(ctx)
	w := ws[0]
	// Flipped back to watching, found fields cleared, URL AND title recorded
	// as ignored (Prowlarr URLs rotate between searches; the title is the
	// durable half of the identity).
	if w.Status != StatusWatching {
		t.Errorf("status = %q, want watching", w.Status)
	}
	if w.FoundURL != "" || w.FoundTitle != "" {
		t.Errorf("found fields should be cleared, got %+v", w)
	}
	if len(w.IgnoredURLs) != 2 || w.IgnoredURLs[0] != "http://dead" || w.IgnoredURLs[1] != "Dead 1080p" {
		t.Errorf("ignored set = %v, want [http://dead, Dead 1080p]", w.IgnoredURLs)
	}
	// It's claimable again (back to watching).
	if got, _ := r.ClaimBatch(ctx, 10); len(got) != 1 {
		t.Error("dismissed watch should be claimable again")
	}
	// Dismissing a second different release accumulates, not replaces.
	_ = r.MarkAvailable(ctx, "s", "Tiny 1080p", "http://tiny", "1337x", "torrent", 50, nil)
	_ = r.Dismiss(ctx, "s", "http://tiny", "Tiny 1080p")
	ws, _ = r.List(ctx)
	if len(ws[0].IgnoredURLs) != 4 {
		t.Errorf("ignored set = %v, want 4 entries", ws[0].IgnoredURLs)
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

func TestPerformersRoundTripAndSet(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	// Stored at add time.
	if err := r.Add(ctx, Watch{StashDBID: "s", Title: "x", Performers: []string{"Slim Poke", "Cyber Doll"}}); err != nil {
		t.Fatal(err)
	}
	ws, _ := r.List(ctx)
	if len(ws[0].Performers) != 2 || ws[0].Performers[1] != "Cyber Doll" {
		t.Fatalf("performers round-trip wrong: %v", ws[0].Performers)
	}
	// Backfilled later (bare add path).
	_ = r.Add(ctx, Watch{StashDBID: "bare", Title: "y"})
	if err := r.SetPerformers(ctx, "bare", []string{"Eva Maxim"}); err != nil {
		t.Fatal(err)
	}
	ws, _ = r.List(ctx)
	for _, w := range ws {
		if w.StashDBID == "bare" && (len(w.Performers) != 1 || w.Performers[0] != "Eva Maxim") {
			t.Errorf("SetPerformers wrong: %v", w.Performers)
		}
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
