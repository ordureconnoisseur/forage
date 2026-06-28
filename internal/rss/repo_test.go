package rss

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// schema mirrors the rss_sync_state DDL in db/schema.sql; testing against it
// also catches DDL typos.
const schema = `
CREATE TABLE IF NOT EXISTS rss_sync_state (
  indexer_id        INTEGER PRIMARY KEY,
  last_publish_unix INTEGER NOT NULL DEFAULT 0
);`

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

func TestWatermarksEmpty(t *testing.T) {
	r := testRepo(t)
	m, err := r.Watermarks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 0 {
		t.Errorf("fresh DB watermarks = %v, want empty", m)
	}
}

func TestAdvanceAndRead(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	if err := r.Advance(ctx, 1, 1000); err != nil {
		t.Fatal(err)
	}
	if err := r.Advance(ctx, 2, 2000); err != nil {
		t.Fatal(err)
	}
	m, _ := r.Watermarks(ctx)
	if m[1] != 1000 || m[2] != 2000 {
		t.Fatalf("watermarks = %v, want {1:1000, 2:2000}", m)
	}
}

// TestAdvanceNeverLowers pins the MAX guard: an out-of-order/duplicate tick
// must not rewind a watermark and re-open already-processed releases.
func TestAdvanceNeverLowers(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	if err := r.Advance(ctx, 1, 5000); err != nil {
		t.Fatal(err)
	}
	if err := r.Advance(ctx, 1, 3000); err != nil { // older — must be ignored
		t.Fatal(err)
	}
	m, _ := r.Watermarks(ctx)
	if m[1] != 5000 {
		t.Errorf("watermark = %d, want 5000 (advance must never lower)", m[1])
	}
	// A newer mark still raises it.
	if err := r.Advance(ctx, 1, 6000); err != nil {
		t.Fatal(err)
	}
	m, _ = r.Watermarks(ctx)
	if m[1] != 6000 {
		t.Errorf("watermark = %d, want 6000", m[1])
	}
}
