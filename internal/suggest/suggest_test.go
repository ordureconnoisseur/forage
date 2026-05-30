package suggest

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE performer_cache (
		stash_id TEXT, name TEXT, aliases TEXT, scene_count INTEGER, favorite INTEGER)`); err != nil {
		t.Fatal(err)
	}
	return db
}

func insert(t *testing.T, db *sql.DB, id, name, aliases string, sceneCount, fav int) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO performer_cache VALUES (?,?,?,?,?)`,
		id, name, aliases, sceneCount, fav); err != nil {
		t.Fatal(err)
	}
}

func TestTopFolderMatchesPerformerPhrase(t *testing.T) {
	db := newDB(t)
	defer db.Close()
	insert(t, db, "846", "Brie Belle", `[]`, 50, 0)

	got := TopFolder(context.Background(), db, "[OnlyFans.com] Bougie_BB - Brie Belle Siterip")
	if got != "Brie Belle" {
		t.Errorf("TopFolder = %q, want %q", got, "Brie Belle")
	}
}

func TestTopFolderShortNameNoSubstringMatch(t *testing.T) {
	db := newDB(t)
	defer db.Close()
	// A short single-word name (< 4 chars) must not match as a substring.
	insert(t, db, "1", "Mom", `[]`, 5, 0)

	got := TopFolder(context.Background(), db, "momdrips collection pack")
	if got != "" {
		t.Errorf("TopFolder = %q, want empty (short name must not substring-match)", got)
	}
}

func TestPerformersRanksMostSpecificFirst(t *testing.T) {
	db := newDB(t)
	defer db.Close()
	// Both could match, but the two-word phrase is more specific than the
	// single-word one, so it should rank first.
	insert(t, db, "1", "Hazel", `[]`, 999, 1)
	insert(t, db, "2", "Hazel Moore", `[]`, 10, 0)

	ps := Performers(context.Background(), db, "Studio Hazel Moore 1080p")
	if len(ps) == 0 || ps[0].Name != "Hazel Moore" {
		t.Fatalf("expected 'Hazel Moore' ranked first, got %+v", ps)
	}
}

func TestTopFolderNoMatch(t *testing.T) {
	db := newDB(t)
	defer db.Close()
	insert(t, db, "1", "Brie Belle", `[]`, 50, 0)

	if got := TopFolder(context.Background(), db, "Some Unrelated Release 2026"); got != "" {
		t.Errorf("TopFolder = %q, want empty", got)
	}
}
