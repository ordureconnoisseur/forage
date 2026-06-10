package placer

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestPlaceDirResumesPartialMirror is the regression guard for the bug
// where an interrupted pack mirror (daemon restart, or a transient
// os.Link error aborting the walk) left the destination directory
// present but half-populated, and the next Place() treated the existing
// directory as a finished placement — silently confirming a pack with
// missing files. The mirror must now resume and complete.
func TestPlaceDirResumesPartialMirror(t *testing.T) {
	root := t.TempDir()
	lib := filepath.Join(root, "library")
	src := filepath.Join(root, "dl", "MyPack")

	files := []string{"a.mkv", filepath.Join("nested", "b.mkv"), "c.mkv"}
	for _, n := range files {
		full := filepath.Join(src, n)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("data-"+n), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	p := New(lib, discardLogger())
	dest := filepath.Join(lib, "Amouranth", "MyPack")

	// Simulate an interrupted prior run: the dest dir exists with only
	// one of the three files already linked.
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(src, "a.mkv"), filepath.Join(dest, "a.mkv")); err != nil {
		t.Fatal(err)
	}

	res, err := p.Place(src, "Amouranth")
	if err != nil {
		t.Fatalf("Place (resume): %v", err)
	}
	if res.Path != dest {
		t.Errorf("path = %q, want %q", res.Path, dest)
	}
	if res.Mode == "" {
		t.Errorf("expected a non-empty Mode (files were placed), got idempotent")
	}
	for _, n := range files {
		if _, err := os.Stat(filepath.Join(dest, n)); err != nil {
			t.Errorf("missing %s after resume: %v", n, err)
		}
	}

	// A second run with everything present must be a true no-op.
	res2, err := p.Place(src, "Amouranth")
	if err != nil {
		t.Fatalf("Place (idempotent): %v", err)
	}
	if res2.Mode != "" {
		t.Errorf("expected idempotent (Mode \"\"), got %q", res2.Mode)
	}
}

// TestPlaceDirFresh covers the clean path: an empty library mirrors the
// whole tree and reports the placement mode.
func TestPlaceDirFresh(t *testing.T) {
	root := t.TempDir()
	lib := filepath.Join(root, "library")
	src := filepath.Join(root, "dl", "Pack2")
	for _, n := range []string{"x.mkv", "y.mkv"} {
		if err := os.MkdirAll(src, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, n), []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := New(lib, discardLogger())
	res, err := p.Place(src, "Performer")
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	if res.Mode == "" {
		t.Errorf("expected files placed, got idempotent")
	}
	for _, n := range []string{"x.mkv", "y.mkv"} {
		if _, err := os.Stat(filepath.Join(res.Path, n)); err != nil {
			t.Errorf("missing %s: %v", n, err)
		}
	}
}

// TestPlaceSingleFileIdempotent guards the re-place path: a placement
// whose grab update was lost (crash between place and the DB write, or a
// stale-CAS dropped poller tick) re-runs Place with the file already in
// the library. It must reclaim the existing copy, not mint "name (2).ext"
// on every retry.
func TestPlaceSingleFileIdempotent(t *testing.T) {
	root := t.TempDir()
	lib := filepath.Join(root, "library")
	src := filepath.Join(root, "dl", "scene.mkv")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("video bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := New(lib, discardLogger())

	first, err := p.Place(src, "Hazel Moore")
	if err != nil {
		t.Fatalf("first Place: %v", err)
	}
	second, err := p.Place(src, "Hazel Moore")
	if err != nil {
		t.Fatalf("second Place: %v", err)
	}
	if second.Path != first.Path {
		t.Errorf("re-place minted a new path %q, want existing %q", second.Path, first.Path)
	}
	if second.Mode != "" {
		t.Errorf("re-place Mode = %q, want \"\" (idempotent)", second.Mode)
	}
	entries, err := os.ReadDir(filepath.Dir(first.Path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("performer folder has %d entries, want 1: %v", len(entries), entries)
	}
}

// TestPlaceSingleFileCollisionStillSuffixes keeps the original guarantee:
// a DIFFERENT file already at the destination name (a re-grab from another
// indexer) is never overwritten or reclaimed; the new file gets " (2)".
// And a third run of the same source reclaims that suffixed copy instead
// of minting " (3)".
func TestPlaceSingleFileCollisionStillSuffixes(t *testing.T) {
	root := t.TempDir()
	lib := filepath.Join(root, "library")
	src := filepath.Join(root, "dl", "scene.mkv")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("new release bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := New(lib, discardLogger())

	// A pre-existing, different file (different size) at the same name.
	other := filepath.Join(lib, "Hazel Moore", "scene.mkv")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("a much longer pre-existing copy of something else"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := p.Place(src, "Hazel Moore")
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	want := filepath.Join(lib, "Hazel Moore", "scene (2).mkv")
	if res.Path != want {
		t.Errorf("path = %q, want %q", res.Path, want)
	}

	// Re-place reclaims the suffixed copy rather than walking to " (3)".
	res2, err := p.Place(src, "Hazel Moore")
	if err != nil {
		t.Fatalf("re-Place: %v", err)
	}
	if res2.Path != want || res2.Mode != "" {
		t.Errorf("re-place = (%q, mode %q), want (%q, idempotent)", res2.Path, res2.Mode, want)
	}
}

// TestCopyFileWorldReadable: the copy fallback must not inherit
// CreateTemp's 0600, which made library copies unreadable to a Stash
// running as a different uid.
func TestCopyFileWorldReadable(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src.mkv")
	dest := filepath.Join(root, "dest.mkv")
	if err := os.WriteFile(src, []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dest); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("copied file mode = %o, want 644", perm)
	}
}

func TestFreeSpace(t *testing.T) {
	p := New(t.TempDir(), nil)
	free, err := p.FreeSpace()
	if err != nil {
		t.Fatalf("FreeSpace: %v", err)
	}
	if free == 0 {
		t.Fatal("expected non-zero free space on a real filesystem")
	}
	if _, err := New("", nil).FreeSpace(); err == nil {
		t.Fatal("expected an error from an unconfigured placer")
	}
}

// TestReassignRefilesAndRemovesOld mirrors the file dance the
// /grabs/{id}/performer endpoint performs: re-place the (still-present)
// source under a new performer and remove the old library-side copy. Guards
// against the two failure modes that matter — the file not landing in the
// new folder, or the old folder keeping a stale duplicate.
func TestReassignRefilesAndRemovesOld(t *testing.T) {
	root := t.TempDir()
	lib := filepath.Join(root, "library")
	src := filepath.Join(root, "dl", "scene.mkv")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := New(lib, discardLogger())

	// Initial (wrong) placement, as adoption would do under a bad guess.
	first, err := p.Place(src, "Unsorted")
	if err != nil {
		t.Fatalf("first Place: %v", err)
	}

	// Reassign: re-file under the correct performer, then drop the old link.
	second, err := p.Place(src, "Gracie Jane")
	if err != nil {
		t.Fatalf("reassign Place: %v", err)
	}
	if first.Path != second.Path {
		if err := os.RemoveAll(first.Path); err != nil {
			t.Fatalf("remove old placement: %v", err)
		}
	}

	// New folder has the file; the seeding source is untouched; the old
	// library copy is gone.
	if _, err := os.Stat(second.Path); err != nil {
		t.Errorf("file missing at new path %q: %v", second.Path, err)
	}
	if !filepath.HasPrefix(second.Path, filepath.Join(lib, "Gracie Jane")) {
		t.Errorf("new path %q not under the new performer folder", second.Path)
	}
	if _, err := os.Stat(first.Path); !os.IsNotExist(err) {
		t.Errorf("old placement %q should be gone, stat err = %v", first.Path, err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("source (seeding) must be untouched, got: %v", err)
	}
}
