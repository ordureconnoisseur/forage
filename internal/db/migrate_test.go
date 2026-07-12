package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestWatchesMigrationAddsColumns proves the 2026-06-23 watch-batch migration
// brings an OLD watches table (pre batch_id/candidates/grabbed_at) up to the
// current schema on Open, without losing existing rows — the real upgrade
// path for a deployed forager.db.
func TestWatchesMigrationAddsColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forager.db")

	// 1. Fresh Open creates the current schema (incl. a grabs table, which
	//    migrateGrabsColumns requires to proceed past its early return).
	d1, err := Open(path)
	if err != nil {
		t.Fatalf("initial Open: %v", err)
	}
	d1.Close()

	// 2. Replace watches with an OLD-schema table (no batch/candidate/grabbed
	//    columns) and seed a row, simulating a pre-migration database.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TABLE watches`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE watches (
		  stashdb_id TEXT PRIMARY KEY, title TEXT,
		  target TEXT NOT NULL DEFAULT 'any',
		  status TEXT NOT NULL DEFAULT 'watching',
		  created_at INTEGER NOT NULL,
		  last_checked INTEGER NOT NULL DEFAULT 0,
		  ignored_urls TEXT NOT NULL DEFAULT '[]')`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(
		`INSERT INTO watches (stashdb_id, title, created_at) VALUES ('s1', 'Old Watch', 123)`,
	); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	// 3. Re-Open runs the migration.
	d2, err := Open(path)
	if err != nil {
		t.Fatalf("migrating Open: %v", err)
	}
	defer d2.Close()

	// 4. The new columns exist and the old row survived.
	for _, col := range []string{"batch_id", "batch_label", "candidates", "grabbed_at"} {
		var n int
		if err := d2.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('watches') WHERE name = ?`, col,
		).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("column %q missing after migration", col)
		}
	}
	var title, cands string
	if err := d2.QueryRow(
		`SELECT title, candidates FROM watches WHERE stashdb_id = 's1'`,
	).Scan(&title, &cands); err != nil {
		t.Fatalf("old row lost: %v", err)
	}
	if title != "Old Watch" {
		t.Errorf("row corrupted: title = %q", title)
	}
	if cands != "[]" {
		t.Errorf("candidates default = %q, want []", cands)
	}

	// Re-Open is idempotent.
	d2.Close()
	d3, err := Open(path)
	if err != nil {
		t.Fatalf("idempotent re-Open: %v", err)
	}
	d3.Close()
}

// TestStudioCacheMigrationAddsColumns proves the 2026-06-23 studios-page
// migration brings an OLD studio_cache table (matcher-only: stashdb_id, name,
// aliases) up to the current schema on Open, preserving rows — and that the
// matcher's name/aliases columns still read.
func TestStudioCacheMigrationAddsColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forager.db")

	d1, err := Open(path)
	if err != nil {
		t.Fatalf("initial Open: %v", err)
	}
	d1.Close()

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TABLE studio_cache`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE studio_cache (
		  stashdb_id TEXT PRIMARY KEY, name TEXT NOT NULL,
		  aliases TEXT, refreshed_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(
		`INSERT INTO studio_cache (stashdb_id, name, aliases, refreshed_at) VALUES ('sdb-1', 'Blacked', '["BLACKED"]', 1)`,
	); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	d2, err := Open(path)
	if err != nil {
		t.Fatalf("migrating Open: %v", err)
	}
	defer d2.Close()

	for _, col := range []string{"stash_id", "favorite", "scene_count", "total_stashdb_scenes", "owned_scenes_count", "last_release_unix"} {
		var n int
		if err := d2.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('studio_cache') WHERE name = ?`, col,
		).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("column %q missing after migration", col)
		}
	}
	// The matcher's columns still read, and the row survived with default
	// aggregates.
	var name, aliases string
	var total int
	if err := d2.QueryRow(
		`SELECT name, aliases, total_stashdb_scenes FROM studio_cache WHERE stashdb_id = 'sdb-1'`,
	).Scan(&name, &aliases, &total); err != nil {
		t.Fatalf("old row lost: %v", err)
	}
	if name != "Blacked" || aliases != `["BLACKED"]` {
		t.Errorf("row corrupted: name=%q aliases=%q", name, aliases)
	}
	if total != 0 {
		t.Errorf("total_stashdb_scenes default = %d, want 0", total)
	}
}

// TestSceneCacheMigration proves the 2026-06-24 persistent-scene-cache
// migration: an old performer_cache/studio_cache gains scenes_synced_at, and
// the stashdb_scene / scene_performer tables are created.
func TestSceneCacheMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forager.db")
	d1, err := Open(path)
	if err != nil {
		t.Fatalf("initial Open: %v", err)
	}
	d1.Close()

	// Strip scenes_synced_at + the new tables to simulate a pre-migration DB.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`ALTER TABLE performer_cache DROP COLUMN scenes_synced_at`,
		`ALTER TABLE studio_cache DROP COLUMN scenes_synced_at`,
		`DROP TABLE stashdb_scene`,
		`DROP TABLE scene_performer`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	raw.Close()

	d2, err := Open(path)
	if err != nil {
		t.Fatalf("migrating Open: %v", err)
	}
	defer d2.Close()

	for _, tc := range []struct{ table, col string }{
		{"performer_cache", "scenes_synced_at"},
		{"studio_cache", "scenes_synced_at"},
	} {
		var n int
		if err := d2.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('`+tc.table+`') WHERE name = ?`, tc.col).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("%s.%s missing after migration", tc.table, tc.col)
		}
	}
	for _, tbl := range []string{"stashdb_scene", "scene_performer"} {
		var n int
		if err := d2.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("table %s not created", tbl)
		}
	}
}

// TestGrabsRetryMigration verifies the 2026-07-12 deferred-retry columns
// (attempts, next_retry_at) are added to a pre-migration grabs table and
// existing rows survive with the zero-value defaults.
func TestGrabsRetryMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forager.db")

	d1, err := Open(path)
	if err != nil {
		t.Fatalf("initial Open: %v", err)
	}
	d1.Close()

	// Rebuild grabs WITHOUT the retry columns and seed a row, simulating a
	// database from before this migration.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TABLE grabs`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE grabs (
		  id INTEGER PRIMARY KEY,
		  release_title TEXT NOT NULL,
		  client_id TEXT,
		  client_name TEXT,
		  client TEXT NOT NULL DEFAULT 'qbit',
		  status TEXT NOT NULL DEFAULT 'queued',
		  grabbed_at INTEGER NOT NULL,
		  updated_at INTEGER NOT NULL,
		  rev INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(
		`INSERT INTO grabs (release_title, grabbed_at, updated_at) VALUES ('Old Grab', 1, 1)`,
	); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	d2, err := Open(path)
	if err != nil {
		t.Fatalf("migrating Open: %v", err)
	}
	defer d2.Close()

	for _, col := range []string{"attempts", "next_retry_at", "fail_kind"} {
		var n int
		if err := d2.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('grabs') WHERE name = ?`, col,
		).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("column %s missing after migration", col)
		}
	}
	var attempts int
	if err := d2.QueryRow(`SELECT attempts FROM grabs WHERE release_title = 'Old Grab'`).Scan(&attempts); err != nil {
		t.Fatalf("old row lost: %v", err)
	}
	if attempts != 0 {
		t.Errorf("attempts = %d, want 0 default", attempts)
	}
}
