package main

import "testing"

func libWith(files ...[2]interface{}) *Index {
	ix := NewIndex()
	for _, f := range files {
		ix.Add(f[0].(string), int64(f[1].(int)), 0, false)
	}
	return ix
}

// The find that started this: a download whose library counterpart is SMALLER
// under the same name. The transcode wrote a new file at the library path,
// which broke the hardlink and left the original sitting at full size.
func TestClassifySupersededByASmallerLibraryCopy(t *testing.T) {
	lib := libWith([2]interface{}{`Z:\Media\Perf\scene.mp4`, 900})
	e := Classify("/dl/complete/scene.mp4", 3000, 0, false, lib, nil, false)
	if e.Bucket != BucketSuperseded {
		t.Fatalf("bucket = %q, want %q", e.Bucket, BucketSuperseded)
	}
	if e.LibrarySize != 900 {
		t.Errorf("LibrarySize = %d, want the library's 900 for the report", e.LibrarySize)
	}
	if !e.Reclaimable() {
		t.Error("a superseded original that nothing is seeding is reclaimable")
	}
}

// The inverse must NOT be called superseded: if the library copy is bigger,
// the download is the lesser file and deleting it is still fine, but calling
// it a transcode leftover would misreport why.
func TestClassifyVariantWhenTheLibraryCopyIsBigger(t *testing.T) {
	lib := libWith([2]interface{}{"scene.mp4", 5000})
	if e := Classify("/dl/scene.mp4", 3000, 0, false, lib, nil, false); e.Bucket != BucketVariant {
		t.Errorf("bucket = %q, want %q", e.Bucket, BucketVariant)
	}
}

func TestClassifyDuplicateByNameAndSize(t *testing.T) {
	lib := libWith([2]interface{}{`Z:\Media\A\scene.mp4`, 3000})
	if e := Classify("/dl/scene.mp4", 3000, 0, false, lib, nil, false); e.Bucket != BucketDuplicate {
		t.Errorf("bucket = %q, want %q", e.Bucket, BucketDuplicate)
	}
}

// A hardlink that was renamed in the library has neither the same basename nor
// a name+size hit, so only the file identity finds it. On the reference
// library the reverse also held: name+size caught 253 files the inode check
// missed. Both are needed; neither alone is safe.
func TestClassifyDuplicateByFileID(t *testing.T) {
	lib := NewIndex()
	lib.Add("/library/Perf/renamed to something else.mp4", 4242, 99, true)
	e := Classify("/dl/original.release.name.mp4", 4242, 99, true, lib, nil, false)
	if e.Bucket != BucketDuplicate {
		t.Errorf("bucket = %q, want the hardlink to be recognised through the rename", e.Bucket)
	}
}

// Content deliberately moved to another drive is not orphaned.
func TestClassifyFindsContentInAnExtraRoot(t *testing.T) {
	other := libWith([2]interface{}{`D:\Porn\Scat\clip.mp4`, 700})
	e := Classify("/dl/complete/clip.mp4", 700, 0, false, NewIndex(), []*Index{other}, false)
	if e.Bucket != BucketElsewhere {
		t.Errorf("bucket = %q, want %q", e.Bucket, BucketElsewhere)
	}
}

// The pile this tool exists to surface: complete, not seeding, nowhere else.
func TestClassifyOrphan(t *testing.T) {
	e := Classify("/dl/complete/GenderX.Luna.Love.mp4", 1800, 0, false, NewIndex(), nil, false)
	if e.Bucket != BucketOrphan {
		t.Fatalf("bucket = %q, want %q", e.Bucket, BucketOrphan)
	}
	if e.Reclaimable() {
		t.Error("an orphan is the ONLY copy: it must never be reclaimable")
	}
}

// Redundancy and seeding are independent, and conflating them is how ten
// torrents broke on this library.
func TestSeedingIsNeverReclaimableHoweverRedundant(t *testing.T) {
	lib := libWith([2]interface{}{"scene.mp4", 900})
	e := Classify("/dl/scene.mp4", 3000, 0, false, lib, nil, true)
	if e.Bucket != BucketSuperseded {
		t.Errorf("bucket = %q, want the redundancy still reported", e.Bucket)
	}
	if !e.Redundant() {
		t.Error("it IS redundant")
	}
	if e.Reclaimable() {
		t.Fatal("but a torrent is serving it, so it must not be reclaimable")
	}
}

func TestClassifyInProgressBeatsEverything(t *testing.T) {
	// Identical to a library file, but SAB is still writing it.
	lib := libWith([2]interface{}{"pack.mp4", 10})
	for _, p := range []string{
		"/data/porn/downloads/incomplete/Pack.1/pack.mp4",
		"/dl/x/__ADMIN__/SABnzbd_nzf_sd82k3u8",
		"/dl/half.mp4.part",
		"/dl/half.mp4.!qB",
	} {
		if e := Classify(p, 10, 0, false, lib, nil, false); e.Bucket != BucketInProgress {
			t.Errorf("%s: bucket = %q, want %q", p, e.Bucket, BucketInProgress)
		}
	}
}

func TestClassifyNonMedia(t *testing.T) {
	for _, p := range []string{"/dl/x.nfo", "/dl/scene_screens.jpg", "/dl/readme.txt"} {
		if e := Classify(p, 10, 0, false, NewIndex(), nil, false); e.Bucket != BucketNonMedia {
			t.Errorf("%s: bucket = %q, want %q", p, e.Bucket, BucketNonMedia)
		}
	}
}

// The bug that made a first pass of this analysis worthless: qBittorrent
// reports an empty content_path while fetching metadata, and prefixing "" with
// "/" matches every absolute path on the system. Six such torrents claimed
// 8,051 files and 2.17 TB as seeded.
func TestSeedingPathsRejectsPathsThatWouldClaimTheWholeTree(t *testing.T) {
	got := SeedingPaths([]string{
		"",
		"   ",
		"/",
		"/data",
		"/data/porn",
		"relative/path",
		"/data/porn/downloads/complete/real.mp4",
		"/data/porn/downloads/complete/a pack/",
	}, 4)
	want := []string{
		"/data/porn/downloads/complete/real.mp4",
		"/data/porn/downloads/complete/a pack/",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d paths %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestIsSeeded(t *testing.T) {
	seeds := []string{
		"/data/porn/downloads/complete/single.mp4",
		"/data/porn/downloads/complete/a pack",
	}
	for _, c := range []struct {
		path string
		want bool
	}{
		{"/data/porn/downloads/complete/single.mp4", true},
		{"/data/porn/downloads/complete/a pack/inner/clip.mp4", true},
		{"/data/porn/downloads/complete/a pack", true},
		// A sibling whose name merely starts with the same characters is a
		// different release and is NOT covered by that torrent.
		{"/data/porn/downloads/complete/a pack extra/clip.mp4", false},
		{"/data/porn/downloads/complete/other.mp4", false},
	} {
		if got := IsSeeded(c.path, seeds); got != c.want {
			t.Errorf("IsSeeded(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// An empty seed list must never mark anything seeded, or a qBit outage would
// silently turn "do not touch" into "safe to delete".
func TestIsSeededWithNoTorrents(t *testing.T) {
	if IsSeeded("/dl/anything.mp4", nil) {
		t.Error("no torrents means nothing is seeded")
	}
}
