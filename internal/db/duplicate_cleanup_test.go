package db

import (
	"path/filepath"
	"testing"
)

// TestSchemaDismissesNonActionableDuplicates covers the backlog cleanup in
// schema.sql. Review items with no existing side can't be acted on: the UI
// shows "?" where the other copy's details belong, "keep existing" can only
// 409 (the side it would keep isn't in Stash), and "keep new" destroys nothing
// while marking itself resolved. The detector and writer now refuse to create
// them, so this clears what was already written.
//
// Runs on every Open (schema.sql is re-executed), so it must also leave a
// legitimate pending row alone — the second half of this test.
func TestSchemaDismissesNonActionableDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forager.db")

	dbh, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Three pending rows: two non-actionable spellings of "no existing side",
	// and one real duplicate that must survive untouched.
	for _, row := range []struct {
		grabID   int64
		existing string
	}{
		{1, "[]"},
		{2, ""},
		{3, `[{"scene_id":"old-7","path":"/lib/old/a.mp4","size":10,"height":1080}]`},
	} {
		if _, err := dbh.Exec(`
INSERT INTO pack_duplicate
  (grab_id, stashdb_id, scene_title, pack_scene_id, pack_path, pack_size, pack_height,
   existing_copies, status, created_at)
VALUES (?, ?, 'Scene', 'p1', '/lib/new/a.mp4', 10, 1080, ?, 'pending', 1)`,
			row.grabID, "sdb-"+string(rune('a'+row.grabID)), row.existing); err != nil {
			t.Fatalf("insert grab %d: %v", row.grabID, err)
		}
	}
	dbh.Close()

	// Reopening re-runs schema.sql, which is where the cleanup lives.
	dbh, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer dbh.Close()

	for _, tc := range []struct {
		grabID     int64
		wantStatus string
		why        string
	}{
		{1, "resolved", `existing_copies "[]"`},
		{2, "resolved", `existing_copies ""`},
		{3, "pending", "a real duplicate with one existing copy"},
	} {
		var status, resolution string
		if err := dbh.QueryRow(
			`SELECT status, resolution FROM pack_duplicate WHERE grab_id = ?`, tc.grabID,
		).Scan(&status, &resolution); err != nil {
			t.Fatalf("select grab %d: %v", tc.grabID, err)
		}
		if status != tc.wantStatus {
			t.Errorf("grab %d (%s): status = %q, want %q", tc.grabID, tc.why, status, tc.wantStatus)
		}
		if tc.wantStatus == "resolved" && resolution != "both" {
			t.Errorf("grab %d: resolution = %q, want both (a dismissal destroys nothing)",
				tc.grabID, resolution)
		}
	}
}
