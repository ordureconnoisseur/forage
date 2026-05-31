package db

import (
	"path/filepath"
	"testing"
)

// TestOpenFreshDB guards the first-boot path: Open on a brand-new database
// file must succeed. migrateGrabsColumns runs before schema.sql, and its
// column guards read 0 from pragma_table_info on a missing table — so
// without the table-existence short-circuit it would run
// `ALTER TABLE grabs ADD COLUMN ...` against a table that doesn't exist yet
// and crash every new install. Regression test for that.
func TestOpenFreshDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a fresh DB should succeed, got: %v", err)
	}
	defer db.Close()

	// The schema must be fully materialised, including the columns the
	// migration would otherwise have added.
	for _, col := range []string{"client", "progress", "pack_files", "placed_path"} {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('grabs') WHERE name = ?`, col,
		).Scan(&n); err != nil {
			t.Fatalf("pragma_table_info(grabs) for %q: %v", col, err)
		}
		if n != 1 {
			t.Errorf("fresh grabs table missing column %q", col)
		}
	}
}

// TestOpenIdempotent verifies a second Open on an existing DB is a clean
// no-op — the migration's actual purpose. Mirrors a daemon restart.
func TestOpenIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")
	db1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	db1.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open (existing DB) should be a no-op, got: %v", err)
	}
	db2.Close()
}
