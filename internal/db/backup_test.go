package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestBackupPrecious proves the backup is restorable, not just written: the
// snapshot must be an openable SQLite database containing every precious
// table's rows, and rotation must keep prior generations. A backup that has
// never been opened is a hope, not a backup.
func TestBackupPrecious(t *testing.T) {
	dir := t.TempDir()
	dbh, err := Open(filepath.Join(dir, "forager.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer dbh.Close()
	ctx := context.Background()

	if _, err := dbh.ExecContext(ctx, `
INSERT INTO grabs (release_title, status, grabbed_at, updated_at) VALUES ('precious', 'confirmed', 1, 1)`); err != nil {
		t.Fatalf("seed grab: %v", err)
	}
	if _, err := dbh.ExecContext(ctx, `
INSERT INTO destruction_log (reason, scene_id, created_at) VALUES ('test', 's1', 1)`); err != nil {
		t.Fatalf("seed journal: %v", err)
	}

	bak := filepath.Join(dir, "precious.bak")
	if err := BackupPrecious(ctx, dbh, bak); err != nil {
		t.Fatalf("backup: %v", err)
	}

	// Restorability: open the snapshot as a PLAIN SQLite database (the
	// snapshot holds only the precious tables, so forage's Open — which
	// migrates and expects the cache tables — is the wrong tool; the real
	// restore procedure is ATTACH + INSERT…SELECT, which needs exactly
	// this).
	bdb, err := sql.Open("sqlite", bak)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	var title string
	if err := bdb.QueryRowContext(ctx,
		`SELECT release_title FROM grabs WHERE release_title = 'precious'`).Scan(&title); err != nil {
		t.Fatalf("read back grab: %v", err)
	}
	var n int
	if err := bdb.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM destruction_log`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("journal rows in backup = %d (err %v), want 1", n, err)
	}
	// Close before the next backup rotates this file: Windows refuses to
	// rename a file another handle holds open, and the daemon never holds
	// its own backups open — only this test did.
	bdb.Close()

	// Rotation: a second backup moves the first to .1.
	if err := BackupPrecious(ctx, dbh, bak); err != nil {
		t.Fatalf("second backup: %v", err)
	}
	b1, err := sql.Open("sqlite", bak+".1")
	if err != nil {
		t.Fatalf("open rotated generation: %v", err)
	}
	defer b1.Close()
	if err := b1.QueryRowContext(ctx,
		`SELECT release_title FROM grabs WHERE release_title = 'precious'`).Scan(&title); err != nil {
		t.Fatalf("rotated generation unreadable: %v", err)
	}
}
