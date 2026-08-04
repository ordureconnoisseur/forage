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
	content := []byte("hello foraging world")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dest); err != nil {
		t.Fatalf("good copy failed: %v", err)
	}
	di, err := os.Stat(dest)
	// Against len(content), not a literal: the size was hardcoded and broke
	// the moment the fixture string changed by one character.
	if err != nil || di.Size() != int64(len(content)) {
		t.Fatalf("copy landed %d bytes, want %d (%v)", di.Size(), len(content), err)
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

// The default modes must not silently change what a library looks like.
//
// This commit moved created files from 0644/0755 to 0664/0775 so the daemon,
// Stash and the human can all write regardless of which uid the container
// runs as. That is deliberate, but it is a behaviour change on every existing
// deployment, so it is pinned rather than left to be rediscovered.
func TestDefaultModesAreGroupWritable(t *testing.T) {
	defer ConfigureOwnership(-1, -1, "")
	ConfigureOwnership(-1, -1, "")
	if got := DirMode(); got != 0o775 {
		t.Errorf("DirMode() = %o, want 775", got)
	}
	if got := FileMode(); got != 0o664 {
		t.Errorf("FileMode() = %o, want 664", got)
	}
}

// A malformed umask must leave the defaults alone rather than producing
// something absurd like mode 0.
func TestConfigureOwnershipIgnoresJunkUmask(t *testing.T) {
	defer ConfigureOwnership(-1, -1, "")
	for _, junk := range []string{"nonsense", "99999999999999999999", "-1"} {
		ConfigureOwnership(-1, -1, "")
		ConfigureOwnership(-1, -1, junk)
		if DirMode() != 0o775 || FileMode() != 0o664 {
			t.Errorf("umask %q changed the modes to %o/%o", junk, DirMode(), FileMode())
		}
	}
}
