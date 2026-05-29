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
