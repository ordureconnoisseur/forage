package placer

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
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

// TestPlaceDirSkipsSamples guards the sample-clip filter: a scene-group
// release folder ships the full video plus a short preview (as a sibling
// "-sample" file and inside a Sample/ subdir), and mirroring those made
// Stash create junk, un-identifiable scenes. The full video must place; the
// samples must not. A large video that merely has "sample" in its name (no
// bigger sibling) must be kept — the precision guard.
func TestPlaceDirSkipsSamples(t *testing.T) {
	root := t.TempDir()
	lib := filepath.Join(root, "library")
	src := filepath.Join(root, "dl", "Studio.24.01.05.Perf.XXX.1080p-GRP")

	big := make([]byte, 4000)  // the real scene
	small := make([]byte, 100) // a preview sample (<50% of big)
	write := func(rel string, b []byte) {
		full := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("studio.24.01.05.perf.mp4", big)               // keep: the scene
	write("studio.24.01.05.perf-sample.mp4", small)      // skip: sibling sample
	write(filepath.Join("Sample", "preview.mp4"), small) // skip: in Sample/ dir
	write("studio.24.01.05.perf.nfo", small)             // keep: not a video
	write("proof-sample.jpg", small)                     // keep: not a video

	p := New(lib, discardLogger())
	dest := filepath.Join(lib, "Perf", "Studio.24.01.05.Perf.XXX.1080p-GRP")
	if _, err := p.Place(src, "Perf"); err != nil {
		t.Fatalf("Place: %v", err)
	}

	keep := []string{"studio.24.01.05.perf.mp4", "studio.24.01.05.perf.nfo", "proof-sample.jpg"}
	for _, n := range keep {
		if _, err := os.Stat(filepath.Join(dest, n)); err != nil {
			t.Errorf("expected %s to be placed: %v", n, err)
		}
	}
	skip := []string{"studio.24.01.05.perf-sample.mp4", filepath.Join("Sample", "preview.mp4")}
	for _, n := range skip {
		if _, err := os.Stat(filepath.Join(dest, n)); err == nil {
			t.Errorf("sample %s should NOT have been placed", n)
		}
	}
	// The Sample/ directory itself should not be recreated in the library.
	if _, err := os.Stat(filepath.Join(dest, "Sample")); err == nil {
		t.Errorf("Sample/ dir should not have been mirrored")
	}
}

// TestPlaceSampleNamedMainKept is the false-positive guard: when the only /
// largest video has "sample" in its name (some OnlyFans titles do), it's the
// scene, not a preview, and must be kept.
func TestPlaceSampleNamedMainKept(t *testing.T) {
	root := t.TempDir()
	lib := filepath.Join(root, "library")
	src := filepath.Join(root, "dl", "Selti-gym-workout-sample")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	main := "Selti-naked-gym-workout-sample-GovBRLDI.mp4"
	if err := os.WriteFile(filepath.Join(src, main), make([]byte, 3000), 0o644); err != nil {
		t.Fatal(err)
	}
	p := New(lib, discardLogger())
	dest := filepath.Join(lib, "Selti", "Selti-gym-workout-sample")
	if _, err := p.Place(src, "Selti"); err != nil {
		t.Fatalf("Place: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, main)); err != nil {
		t.Errorf("standalone 'sample'-named video should be kept: %v", err)
	}
}

// TestPlaceUnhidesHiddenFiles guards the dot-strip: dot-hidden source files
// (balbums.st names clips ".y0hw..._source.mp4") must land VISIBLE in the
// library, or Stash's scanner skips them and they never become scenes. Covers
// both the single-file and the pack-folder paths, and the idempotent re-run
// (the un-hidden dest must be recognised as ours, not re-placed as a hidden
// duplicate).
func TestPlaceUnhidesHiddenFiles(t *testing.T) {
	root := t.TempDir()
	lib := filepath.Join(root, "library")

	// Single hidden file.
	sf := filepath.Join(root, "dl", ".y0hw_source.mp4")
	if err := os.MkdirAll(filepath.Dir(sf), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sf, []byte("scene"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := New(lib, discardLogger())
	res, err := p.Place(sf, "Bubblexgun")
	if err != nil {
		t.Fatalf("Place single hidden: %v", err)
	}
	if base := filepath.Base(res.Path); base != "y0hw_source.mp4" {
		t.Errorf("single file placed as %q, want un-hidden y0hw_source.mp4", base)
	}

	// Pack with a mix of hidden and visible files.
	src := filepath.Join(root, "dl", "balbums - pack")
	for _, n := range []string{".y0hw1_source.mp4", ".y0hw2_source.mp4", "visible.mp4"} {
		if err := os.WriteFile(filepath.Join(src, n), []byte(n), 0o644); err != nil {
			if os.MkdirAll(src, 0o755) == nil {
				_ = os.WriteFile(filepath.Join(src, n), []byte(n), 0o644)
			}
		}
	}
	dest := filepath.Join(lib, "Bubblexgun", "balbums - pack")
	if _, err := p.Place(src, "Bubblexgun"); err != nil {
		t.Fatalf("Place pack: %v", err)
	}
	for _, want := range []string{"y0hw1_source.mp4", "y0hw2_source.mp4", "visible.mp4"} {
		if _, err := os.Stat(filepath.Join(dest, want)); err != nil {
			t.Errorf("expected un-hidden %s in library: %v", want, err)
		}
	}
	for _, notWant := range []string{".y0hw1_source.mp4", ".y0hw2_source.mp4"} {
		if _, err := os.Stat(filepath.Join(dest, notWant)); err == nil {
			t.Errorf("hidden %s should not exist in library", notWant)
		}
	}
	// Re-run must be a no-op: the un-hidden dest is recognised as ours (not
	// re-placed as hidden duplicates).
	res2, err := p.Place(src, "Bubblexgun")
	if err != nil {
		t.Fatalf("Place pack (idempotent): %v", err)
	}
	if res2.Mode != "" {
		t.Errorf("re-run placed files (Mode %q); un-hide broke idempotency", res2.Mode)
	}
	ents, _ := os.ReadDir(dest)
	if len(ents) != 3 {
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("dest has %d entries after re-run, want 3: %v", len(ents), names)
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

// TestPlaceDirPrematureEmptySourceErrors covers the placement race that
// stranded a pack for ~20h: the download client reported the release
// complete before it finished moving files into the complete dir, so Place
// mirrored an empty source — zero files into an empty destination. That
// must be an error (so the poller keeps the grab "completed" and retries),
// NOT a silent success that marks the grab placed against an empty folder
// forever. Once the source finishes moving in, the retry must place it.
func TestPlaceDirPrematureEmptySourceErrors(t *testing.T) {
	root := t.TempDir()
	lib := filepath.Join(root, "library")
	src := filepath.Join(root, "dl", "NotReadyPack")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	p := New(lib, discardLogger())

	// Source dir exists but files haven't moved in yet: must error.
	if _, err := p.Place(src, "Performer"); err == nil {
		t.Fatal("Place on an empty source dir returned nil; want a retry error")
	}

	// The download finishes moving in; the retry must now place the files.
	for _, n := range []string{"a.mkv", "b.mkv"} {
		if err := os.WriteFile(filepath.Join(src, n), []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := p.Place(src, "Performer")
	if err != nil {
		t.Fatalf("Place after source populated: %v", err)
	}
	if res.Mode == "" {
		t.Errorf("expected files placed, got idempotent")
	}
	for _, n := range []string{"a.mkv", "b.mkv"} {
		if _, err := os.Stat(filepath.Join(res.Path, n)); err != nil {
			t.Errorf("missing %s after retry: %v", n, err)
		}
	}
}

// TestPlaceDirCollisionSuffixes is the directory analogue of the
// single-file collision guarantee: a DIFFERENT release whose folder has
// the same name (pack folders are routinely just the performer's name)
// must not be merged into the existing placement — merging would let a
// later purge of either grab sweep the other's files. The newcomer gets
// "name (2)", and its own retries reclaim that suffixed dir.
func TestPlaceDirCollisionSuffixes(t *testing.T) {
	root := t.TempDir()
	lib := filepath.Join(root, "library")
	srcA := filepath.Join(root, "dl", "Amouranth")
	srcB := filepath.Join(root, "dl2", "Amouranth")
	for src, files := range map[string][]string{
		srcA: {"a.mkv", "shared.mkv"},
		srcB: {"b.mkv", "shared.mkv"},
	} {
		for _, n := range files {
			if err := os.MkdirAll(src, 0o755); err != nil {
				t.Fatal(err)
			}
			// "shared.mkv" exists in both packs with different content and
			// size — the same-name-different-file merge hazard.
			if err := os.WriteFile(filepath.Join(src, n), []byte(src+n), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	p := New(lib, discardLogger())

	resA, err := p.Place(srcA, "Amouranth")
	if err != nil {
		t.Fatalf("Place A: %v", err)
	}
	resB, err := p.Place(srcB, "Amouranth")
	if err != nil {
		t.Fatalf("Place B: %v", err)
	}
	if resB.Path == resA.Path {
		t.Fatalf("pack B merged into pack A's directory %q", resA.Path)
	}
	want := filepath.Join(lib, "Amouranth", "Amouranth (2)")
	if resB.Path != want {
		t.Errorf("pack B path = %q, want %q", resB.Path, want)
	}
	// Pack A's copy of shared.mkv is untouched.
	gotA, err := os.ReadFile(filepath.Join(resA.Path, "shared.mkv"))
	if err != nil || string(gotA) != srcA+"shared.mkv" {
		t.Errorf("pack A shared.mkv corrupted: %q err=%v", gotA, err)
	}

	// Retries of each pack reclaim their own directory.
	resA2, err := p.Place(srcA, "Amouranth")
	if err != nil || resA2.Path != resA.Path || resA2.Mode != "" {
		t.Errorf("re-place A = (%q, %q, %v), want (%q, idempotent, nil)", resA2.Path, resA2.Mode, err, resA.Path)
	}
	resB2, err := p.Place(srcB, "Amouranth")
	if err != nil || resB2.Path != resB.Path || resB2.Mode != "" {
		t.Errorf("re-place B = (%q, %q, %v), want (%q, idempotent, nil)", resB2.Path, resB2.Mode, err, resB.Path)
	}
}

// TestPlaceDirIgnoresStrandedPartial: a crashed copy-fallback strands a
// ".forage-copy-*.partial" temp in our own placement dir; that's ours,
// not foreign content, and must not push the retry to "name (2)".
func TestPlaceDirIgnoresStrandedPartial(t *testing.T) {
	root := t.TempDir()
	lib := filepath.Join(root, "library")
	src := filepath.Join(root, "dl", "MyPack")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.mkv"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(lib, "Performer", "MyPack")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, ".forage-copy-123.partial"), []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := New(lib, discardLogger()).Place(src, "Performer")
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	if res.Path != dest {
		t.Errorf("path = %q, want reclaimed %q", res.Path, dest)
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
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits are not honoured on windows")
	}
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
	// Against the configured contract, not a literal. The default moved from
	// 0644 to 0664 when ownership became configurable, and asserting the old
	// number here failed a change that satisfied this test's actual point.
	if perm := info.Mode().Perm(); perm != FileMode() {
		t.Errorf("copied file mode = %o, want %o (the configured FileMode)", perm, FileMode())
	}
	// And the point itself, independent of whatever the mode is configured
	// to: a library copy Stash cannot read is the bug this guards.
	if perm := info.Mode().Perm(); perm&0o044 == 0 {
		t.Errorf("copied file mode = %o: not readable by group or other, so a "+
			"Stash running as another uid cannot open it", perm)
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
