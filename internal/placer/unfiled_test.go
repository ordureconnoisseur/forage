package placer

import (
	"os"
	"path/filepath"
	"testing"
)

// The rename must not split the bin. A library that already has Unsorted
// keeps writing there: starting to write Unfiled beside 4,884 existing files
// would leave two folders that both mean "not filed", which is worse than
// either name on its own.
func TestUnfiledDirKeepsAnExistingLegacyFolder(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, LegacyUnfiledFolder), 0o755); err != nil {
		t.Fatal(err)
	}
	p := New(root, nil)
	if got := p.unfiledDir(); got != LegacyUnfiledFolder {
		t.Errorf("unfiledDir() = %q, want %q on a library that already has it",
			got, LegacyUnfiledFolder)
	}
}

// A fresh library gets the name the rest of forage uses.
func TestUnfiledDirPrefersTheCurrentNameOnAFreshLibrary(t *testing.T) {
	p := New(t.TempDir(), nil)
	if got := p.unfiledDir(); got != UnfiledFolder {
		t.Errorf("unfiledDir() = %q, want %q", got, UnfiledFolder)
	}
}

// A FILE called Unsorted is not the bin.
func TestUnfiledDirIgnoresANonDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, LegacyUnfiledFolder), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := New(root, nil).unfiledDir(); got != UnfiledFolder {
		t.Errorf("unfiledDir() = %q, want %q: a regular file is not the bin", got, UnfiledFolder)
	}
}

// End to end: an empty performer lands in the library's own bin, not a second
// one, and the file is really there.
func TestPlaceWithNoPerformerUsesTheExistingBin(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, LegacyUnfiledFolder), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "release.mp4")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := New(root, nil).Place(src, "")
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	want := filepath.Join(root, LegacyUnfiledFolder, "release.mp4")
	if res.Path != want {
		t.Errorf("placed at %q, want %q", res.Path, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("nothing at the reported path: %v", err)
	}
}
