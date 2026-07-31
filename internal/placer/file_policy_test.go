package placer

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newPolicyPlacer(t *testing.T) (*Placer, string, string) {
	t.Helper()
	base := t.TempDir()
	lib := filepath.Join(base, "library")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	p := New(lib, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return p, base, lib
}

// The passengers a release carries must not enter the library. forage never
// runs them, but the library is browsed from Windows over SMB, where a .lnk or
// .url fetches its icon from wherever the file says.
func TestPlaceRefusesNonMediaFiles(t *testing.T) {
	p, base, lib := newPolicyPlacer(t)
	rel := filepath.Join(base, "Release.Name.XXX.1080p")
	writeFile(t, filepath.Join(rel, "scene.mp4"), 5000)
	writeFile(t, filepath.Join(rel, "scene.nfo"), 20)
	writeFile(t, filepath.Join(rel, "cover.jpg"), 40)
	writeFile(t, filepath.Join(rel, "scene.srt"), 30)
	for _, bad := range []string{
		"Setup.exe", "Watch Now.lnk", "readme.url", "desktop.ini",
		"install.scr", "run.bat", "payload.js", "thing.vbs", "lib.dll",
		"archive.rar", "disc.iso",
	} {
		writeFile(t, filepath.Join(rel, bad), 100)
	}

	res, err := p.Place(rel, "Someone")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	for _, want := range []string{"scene.mp4", "scene.nfo", "cover.jpg", "scene.srt"} {
		if !got[want] {
			t.Errorf("%s should have been placed; got %v", want, entries)
		}
	}
	for _, bad := range []string{"Setup.exe", "Watch Now.lnk", "readme.url",
		"desktop.ini", "install.scr", "run.bat", "payload.js", "thing.vbs",
		"lib.dll", "archive.rar", "disc.iso"} {
		if got[bad] {
			t.Errorf("%s must not enter the library", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(lib, "Someone")); err != nil {
		t.Errorf("performer folder missing: %v", err)
	}
}

// A release with nothing placeable in it is a DECIDED outcome, not a source
// that has not finished copying. Confusing the two makes the poller re-place
// a fake release on every tick, forever.
func TestPlaceEntirelyUnplaceableReleaseDoesNotRetryForever(t *testing.T) {
	p, base, _ := newPolicyPlacer(t)
	rel := filepath.Join(base, "Totally.Legit.Release")
	writeFile(t, filepath.Join(rel, "Setup.exe"), 900)
	writeFile(t, filepath.Join(rel, "Read Me.lnk"), 90)

	res, err := p.Place(rel, "Someone")
	if err != nil {
		t.Fatalf("an all-junk release must not error (that is the retry path): %v", err)
	}
	if res.Path == "" {
		t.Fatal("expected a destination path so the grab can move on")
	}
	entries, _ := os.ReadDir(res.Path)
	if len(entries) != 0 {
		t.Errorf("nothing should have been placed, found %v", entries)
	}
}

// The existing guard must survive: an EMPTY source is the download client
// still moving files in, and that does have to retry.
func TestPlaceStillRetriesAnEmptySource(t *testing.T) {
	p, base, _ := newPolicyPlacer(t)
	rel := filepath.Join(base, "Still.Copying")
	if err := os.MkdirAll(rel, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Place(rel, "Someone"); err == nil {
		t.Error("an empty source must error so the poller retries once it populates")
	}
}

func TestIsPlaceable(t *testing.T) {
	for _, c := range []struct {
		name string
		want bool
	}{
		{"scene.mp4", true}, {"scene.MKV", true}, {"clip.m2ts", true},
		{"old.vob", true}, {"weird.rmvb", true},
		{"subs.srt", true}, {"subs.IDX", true}, {"art.jpg", true},
		{"notes.nfo", true}, {"info.txt", true},

		{"Setup.exe", false}, {"shortcut.lnk", false}, {"link.url", false},
		{"desktop.ini", false}, {"go.bat", false}, {"go.cmd", false},
		{"x.ps1", false}, {"x.vbs", false}, {"x.js", false}, {"x.scr", false},
		{"x.msi", false}, {"x.dll", false}, {"x.com", false},
		{"pack.rar", false}, {"pack.zip", false}, {"pack.7z", false},
		{"disc.iso", false}, {"half.mp4.part", false}, {"noextension", false},
		{"", false},
	} {
		if got := isPlaceable(c.name); got != c.want {
			t.Errorf("isPlaceable(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}
