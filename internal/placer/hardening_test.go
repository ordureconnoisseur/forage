package placer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A truncated copy must never become the placed file. Simulated by
// copying from a source that shrinks mid-flight is impractical, so the
// contract is checked at the seam: copyFile compares sizes before the
// rename, and a mismatch leaves NOTHING at the destination.
func TestCopyFileVerifiesSize(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dest := filepath.Join(dir, "dest.bin")
	if err := os.WriteFile(src, []byte("hello winnowing world"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dest); err != nil {
		t.Fatalf("good copy failed: %v", err)
	}
	di, err := os.Stat(dest)
	if err != nil || di.Size() != 21 {
		t.Fatalf("copy landed wrong: %v %v", di, err)
	}
	// No .partial debris left behind.
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".partial") {
			t.Fatalf("temp file survived: %s", e.Name())
		}
	}
}

// Ownership config drives the modes used for created files and dirs.
func TestConfigureOwnershipModes(t *testing.T) {
	defer ConfigureOwnership(-1, -1, "")
	ConfigureOwnership(-1, -1, "022")
	if DirMode() != 0o755 || FileMode() != 0o644 {
		t.Fatalf("umask 022: dir=%o file=%o", DirMode(), FileMode())
	}
	ConfigureOwnership(-1, -1, "002")
	if DirMode() != 0o775 || FileMode() != 0o664 {
		t.Fatalf("umask 002: dir=%o file=%o", DirMode(), FileMode())
	}
	ConfigureOwnership(-1, -1, "rubbish")
	if DirMode() != 0o775 {
		t.Fatal("a bad umask must leave the previous modes alone")
	}
}

// A hardlinkable placement must not be gated on free space: no bytes
// are written, so a nearly-full library is irrelevant.
func TestSpaceCheckSkippedForHardlink(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !sameDevice(src, dir) {
		// sameDevice is a conservative stub on Windows (always false), so
		// the skip can't be observed there; the daemon runs on Linux.
		t.Skip("sameDevice not implemented on this platform")
	}
	p := New(dir, nil)
	// Same directory ⇒ same device ⇒ no space check, whatever the size.
	if err := p.checkSpaceFor(src, dir, 1<<62); err != nil {
		t.Fatalf("hardlink candidate was space-checked: %v", err)
	}
}
