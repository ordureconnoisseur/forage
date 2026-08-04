package seeding

import "testing"

// The bug this package exists to make impossible. qBittorrent reports an empty
// content_path while a torrent fetches metadata, and testing HasPrefix(file,
// ""+"/") is true for every absolute path. Six such torrents claimed 8,051
// files and 2.17 TB as seeded on the reference library.
func TestNewRejectsPathsThatWouldClaimEverything(t *testing.T) {
	s := New([]string{
		"",              // metaDL torrent: the one that caused this
		"   ",           // whitespace is the same thing
		"/",             // the root
		"/data",         // a mount
		"/data/porn",    // an entire library
		"relative/path", // not absolute: cannot be compared meaningfully
		"/data/porn/downloads/complete/real.mp4",
	}, DefaultMinDepth)

	if s.Len() != 1 {
		t.Fatalf("kept %d paths, want only the specific one", s.Len())
	}
	if s.Covers("/some/unrelated/file.mp4") {
		t.Error("a rejected path must not claim unrelated files")
	}
	if !s.Covers("/data/porn/downloads/complete/real.mp4") {
		t.Error("the one real path should still be covered")
	}
}

func TestCoversFilesAndFolders(t *testing.T) {
	s := New([]string{
		"/data/porn/downloads/complete/single.mp4",
		"/data/porn/downloads/complete/a pack",
		"/data/porn/downloads/complete/trailing/", // trailing separator normalised
	}, DefaultMinDepth)

	for _, c := range []struct {
		path string
		want bool
	}{
		{"/data/porn/downloads/complete/single.mp4", true},
		{"/data/porn/downloads/complete/a pack", true},
		{"/data/porn/downloads/complete/a pack/inner/clip.mp4", true},
		{"/data/porn/downloads/complete/trailing/x.mp4", true},
		// A sibling that merely shares a prefix is a DIFFERENT release. Without
		// the separator check this would be protected by someone else's torrent.
		{"/data/porn/downloads/complete/a pack extra/clip.mp4", false},
		{"/data/porn/downloads/complete/single.mp4.bak", false},
		{"/data/porn/Media/Performer/other.mp4", false},
		{"", false},
	} {
		if got := s.Covers(c.path); got != c.want {
			t.Errorf("Covers(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// "We could not reach the download client" must never read as "nothing is
// seeding". A nil Set answers false to everything, so Known is what callers
// check before trusting that false.
func TestNilSetKnowsNothing(t *testing.T) {
	var s *Set
	if s.Covers("/anything") {
		t.Error("a nil Set covers nothing")
	}
	if s.Len() != 0 {
		t.Error("a nil Set is empty")
	}
	if s.Known() {
		t.Fatal("a nil Set must report that it knows nothing")
	}
}

func TestEmptyAfterFilteringIsNotKnown(t *testing.T) {
	// qBit answered, but every path it gave was useless. Indistinguishable
	// from an outage as far as safety goes.
	s := New([]string{"", "/data"}, DefaultMinDepth)
	if s.Known() {
		t.Error("filtering everything away leaves no information")
	}
}

func TestKnownWithRealPaths(t *testing.T) {
	if !New([]string{"/data/porn/downloads/complete/x.mp4"}, DefaultMinDepth).Known() {
		t.Error("a usable path is information")
	}
}

// Moving a FOLDER breaks every torrent seeding from inside it, which is
// exactly how ten torrents died to one bulk move. Covers only looks upward, so
// a directory containing seeded files reads as safe; Blocks looks both ways.
func TestBlocksCatchesSeededContentInsideAFolder(t *testing.T) {
	s := New([]string{
		"/data/porn/Media/Unfiled/Big Pack/inner/scene.mp4",
		"/data/porn/Media/Unfiled/single.mp4",
	}, DefaultMinDepth)

	// The folder itself is not a content path, and Covers says nothing.
	dir := "/data/porn/Media/Unfiled/Big Pack"
	if s.Covers(dir) {
		t.Fatal("precondition: the folder is not itself a content path")
	}
	if got := s.Blocks(dir); got == "" {
		t.Error("moving the folder would break the torrent seeding from inside it")
	}

	// The three shapes, and the safe case.
	for _, c := range []struct {
		path  string
		block bool
	}{
		{"/data/porn/Media/Unfiled/single.mp4", true},               // is the content
		{"/data/porn/Media/Unfiled/Big Pack/inner/scene.mp4", true}, // is the content
		{"/data/porn/Media/Unfiled/Big Pack/inner", true},           // holds it
		{"/data/porn/Media/Unfiled/Big Pack", true},                 // holds it, deeper
		{"/data/porn/Media/Unfiled/Other Pack", false},              // unrelated
		{"/data/porn/Media/Unfiled/Big Pack extra", false},          // shared prefix only
		{"/data/porn/Media/Unfiled/single.mp4.bak", false},          // shared prefix only
	} {
		got := s.Blocks(c.path) != ""
		if got != c.block {
			t.Errorf("Blocks(%q) = %v, want %v", c.path, got, c.block)
		}
	}
}

func TestBlocksOnNilAndEmpty(t *testing.T) {
	var s *Set
	if s.Blocks("/anything") != "" {
		t.Error("a nil Set blocks nothing")
	}
	if New([]string{"/data/porn/downloads/complete/x.mp4"}, DefaultMinDepth).Blocks("") != "" {
		t.Error("an empty path blocks nothing")
	}
}
