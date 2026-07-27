package destroy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// trashRig builds a realistic layout: a "mount" holding a library dir and
// the sibling trash root TrashFromSettings would derive.
func trashRig(t *testing.T) (lib string, tc *TrashConfig) {
	t.Helper()
	mount := t.TempDir()
	lib = filepath.Join(mount, "Media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	tc = TrashFromSettings(lib, "", 7*24*time.Hour)
	if tc == nil {
		t.Fatal("trash disabled with a valid library root and TTL")
	}
	return lib, tc
}

// TestExecuteTrashesInsteadOfDeleting is the feature: with trash configured,
// the file MOVES (content intact, restorable) and Stash is asked to destroy
// METADATA ONLY — delete_file must be false, or the trash is a lie and the
// file dies anyway.
func TestExecuteTrashesInsteadOfDeleting(t *testing.T) {
	lib, tc := trashRig(t)
	file := filepath.Join(lib, "Performer", "scene.mp4")
	mustWrite(t, file, "the bytes")

	f := &fakeDestroyer{}
	rec := &fakeRecorder{}
	plan := Vet([]Target{{SceneID: "42", Files: []File{{Path: file}}}})
	out := Executor{Stash: f, Rec: rec, Log: discard(), Trash: tc}.
		Execute(context.Background(), plan, "test")

	if len(out.Destroyed) != 1 {
		t.Fatalf("outcome = %+v", out)
	}
	if len(f.deleteFileArgs) != 1 || f.deleteFileArgs[0] {
		t.Fatalf("delete_file args = %v — trash mode must destroy metadata only", f.deleteFileArgs)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("original path still exists; file was not moved")
	}
	// The journal's trashed entry carries the moves, and Restore puts the
	// file back byte-for-byte — the whole point of the feature.
	final, ok := rec.finals[1]
	if !ok || final[0] != "trashed" {
		t.Fatalf("journal finalisation = %v, want trashed", rec.finals)
	}
	if err := Restore(final[1]); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, err := os.ReadFile(file)
	if err != nil || string(got) != "the bytes" {
		t.Fatalf("restored content = %q, %v", got, err)
	}
}

// TestExecuteTrashRollsBackOnStashFailure: files move FIRST, metadata second
// — so when Stash refuses the destroy, the moves must be undone and the
// world left exactly as it was. This ordering is the safety argument for
// the whole design.
func TestExecuteTrashRollsBackOnStashFailure(t *testing.T) {
	lib, tc := trashRig(t)
	file := filepath.Join(lib, "P", "s.mp4")
	mustWrite(t, file, "x")

	f := &fakeDestroyer{fail: map[string]bool{"7": true}}
	plan := Vet([]Target{{SceneID: "7", Files: []File{{Path: file}}}})
	out := Executor{Stash: f, Log: discard(), Trash: tc}.
		Execute(context.Background(), plan, "test")

	if len(out.Failed) != 1 {
		t.Fatalf("outcome = %+v, want the failure surfaced", out)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("file not rolled back to its original path: %v", err)
	}
}

// TestExecuteFallsBackPermanentWhenUnreachable: a file the daemon can't
// reach (mapping gap, already gone) must not block the delete the user
// asked for — it falls back to the permanent path, and the journal
// discloses the downgrade.
func TestExecuteFallsBackPermanentWhenUnreachable(t *testing.T) {
	_, tc := trashRig(t)
	// A Stash-side path with no mapping configured that does not exist on
	// this machine.
	f := &fakeDestroyer{}
	rec := &fakeRecorder{}
	plan := Vet([]Target{{SceneID: "9", Files: []File{{Path: `Z:\Media\gone.mp4`}}}})
	out := Executor{Stash: f, Rec: rec, Log: discard(), Trash: tc}.
		Execute(context.Background(), plan, "test")

	if len(out.Destroyed) != 1 {
		t.Fatalf("outcome = %+v, want the permanent fallback to proceed", out)
	}
	if len(f.deleteFileArgs) != 1 || !f.deleteFileArgs[0] {
		t.Fatalf("delete_file args = %v — fallback must be the real delete", f.deleteFileArgs)
	}
	final := rec.finals[1]
	if final[0] != "destroyed" || final[1] == "" {
		t.Fatalf("journal = %v, want destroyed with the downgrade disclosed", final)
	}
}

// TestSweepTrash: dated directories past the TTL are unlinked (journalled
// per file); younger ones and foreign entries survive. And a whole-day
// grace: content trashed on day N dies only when all of day N is past the
// cutoff.
func TestSweepTrash(t *testing.T) {
	_, tc := trashRig(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	old := filepath.Join(tc.Root, "2026-07-10", "42", "old.mp4")
	fresh := filepath.Join(tc.Root, "2026-07-25", "43", "fresh.mp4")
	foreign := filepath.Join(tc.Root, "notes.txt")
	mustWrite(t, old, "o")
	mustWrite(t, fresh, "f")
	mustWrite(t, foreign, "keep me")

	rec := &fakeRecorder{}
	removed, err := SweepTrash(context.Background(), tc.Root, 7*24*time.Hour, rec, now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("expired file survived the sweep")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh file was swept early")
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Error("foreign file was swept — the sweep must only touch dated dirs")
	}
	if len(rec.entries) != 1 || rec.entries[0].outcome != "destroyed" {
		t.Fatalf("sweep journal = %+v, want one destroyed entry", rec.entries)
	}
}

// TestDefaultTrashRootIsSibling: the trash must sit BESIDE the library, not
// inside it — trash inside the library gets re-indexed by Stash's scan and
// every "deleted" scene reappears.
func TestDefaultTrashRootIsSibling(t *testing.T) {
	got := DefaultTrashRoot("/data/porn/Media")
	if got != "/data/porn/.forage-trash" {
		t.Fatalf("DefaultTrashRoot = %q", got)
	}
	if DefaultTrashRoot("") != "" {
		t.Fatal("empty library root must disable trash")
	}
}

// TestExecuteRefusesWhenLibraryUnavailable is the outage latch: when the
// library MOUNT is gone (not just one file), the permanent fallback must
// not fire — Stash is often a different machine whose own mount is fine,
// and "delete permanently because I can't see anything" would convert an
// outage into data loss. The destroy fails, retryable when the mount
// returns.
func TestExecuteRefusesWhenLibraryUnavailable(t *testing.T) {
	lib, tc := trashRig(t)
	file := filepath.Join(lib, "P", "s.mp4")
	mustWrite(t, file, "x")
	// Simulate the mount dropping: the library root vanishes wholesale.
	if err := os.RemoveAll(lib); err != nil {
		t.Fatal(err)
	}

	f := &fakeDestroyer{}
	rec := &fakeRecorder{}
	plan := Vet([]Target{{SceneID: "5", Files: []File{{Path: file}}}})
	out := Executor{Stash: f, Rec: rec, Log: discard(), Trash: tc}.
		Execute(context.Background(), plan, "test")

	if len(out.Failed) != 1 {
		t.Fatalf("outcome = %+v, want a refusal surfaced as failure", out)
	}
	if len(f.calls) != 0 {
		t.Fatalf("stash saw %v — NOTHING may be destroyed during a mount outage", f.calls)
	}
	if final := rec.finals[1]; final[0] != "failed" {
		t.Fatalf("journal = %v, want failed", final)
	}
}
