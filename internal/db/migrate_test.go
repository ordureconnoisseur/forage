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
