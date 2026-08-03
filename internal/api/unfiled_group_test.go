package api

import (
	"testing"

	"github.com/ordureconnoisseur/forager/internal/stash"
)

const root = `Z:\Media`

func scene(id, path string, perfs ...stash.ScenePerformer) stash.UnfiledScene {
	return stash.UnfiledScene{ID: id, FilePath: path, Performers: perfs}
}

func identified(s stash.UnfiledScene) stash.UnfiledScene {
	s.StashIDs = []stash.StashID{{Endpoint: "https://stashdb.org/graphql", StashID: "x"}}
	return s
}

func noSuggest(string) string { return "" }

// A pack folder is ONE decision, not 489 of them. Listing its files
// individually both drowns the list and invites filing them one at a time,
// which guts a folder the poller's distribute step deliberately preserves.
func TestGroupUnfiledFoldsAPackIntoOneRow(t *testing.T) {
	found := []stash.UnfiledScene{
		scene("1", root+`\Unsorted\Toni Camille pack\a.mp4`),
		scene("2", root+`\Unsorted\Toni Camille pack\b.mp4`),
		scene("3", root+`\Unsorted\Toni Camille pack\sub\c.mp4`),
		scene("4", root+`\Unsorted\loose.mp4`),
		scene("5", root+`\alsoloose.mp4`),
	}
	items, counts := groupUnfiled(found, root, "", noSuggest)
	if len(items) != 3 {
		t.Fatalf("got %d rows, want 3 (one pack plus two loose): %+v", len(items), items)
	}
	pack := items[0] // packs sort first
	if pack.Kind != kindPack || pack.Name != "Toni Camille pack" {
		t.Fatalf("first row should be the pack, got %+v", pack)
	}
	if pack.Files != 3 {
		t.Errorf("pack covers %d files, want 3 including the one in a subfolder", pack.Files)
	}
	if pack.Key != root+`\Unsorted\Toni Camille pack` {
		t.Errorf("pack key must be the folder path, got %q", pack.Key)
	}
	// Counts describe DECISIONS, so a 3-file pack counts once. Counting files
	// would put 4,139 against a bucket holding 103 real choices.
	if total := counts["filable"] + counts["identified"] + counts["unknown"]; total != 3 {
		t.Errorf("counts total %d, want 3 rows not 5 files: %+v", total, counts)
	}
}

// Unsorted is forage's own fallback bin, not a release. Treating it as a pack
// would hide 748 unrelated loose files behind a single row.
func TestGroupUnfiledDoesNotTreatUnsortedAsAPack(t *testing.T) {
	items, _ := groupUnfiled([]stash.UnfiledScene{
		scene("1", root+`\Unsorted\a.mp4`),
		scene("2", root+`\Unsorted\b.mp4`),
	}, root, "", noSuggest)
	if len(items) != 2 {
		t.Fatalf("got %d rows, want 2 separate files: %+v", len(items), items)
	}
	for _, it := range items {
		if it.Kind != kindFile {
			t.Errorf("Unsorted itself must not become a pack: %+v", it)
		}
	}
}

// A pack takes the best case among its members: one identified scene inside is
// worth more attention than none, so it must not be buried in "unknown".
func TestGroupUnfiledPackTakesItsBestBucket(t *testing.T) {
	items, _ := groupUnfiled([]stash.UnfiledScene{
		scene("1", root+`\Unsorted\Mixed pack\a.mp4`),
		identified(scene("2", root+`\Unsorted\Mixed pack\b.mp4`)),
	}, root, "", noSuggest)
	if items[0].Bucket != "identified" {
		t.Errorf("bucket = %q, want identified", items[0].Bucket)
	}
	if !items[0].Identified {
		t.Error("a pack containing an identified scene is identified")
	}

	withPerf := []stash.UnfiledScene{
		scene("1", root+`\Unsorted\Named pack\a.mp4`),
		scene("2", root+`\Unsorted\Named pack\b.mp4`, stash.ScenePerformer{Name: "Someone", SceneCount: 9}),
	}
	got, _ := groupUnfiled(withPerf, root, "", noSuggest)
	if got[0].Bucket != "filable" {
		t.Errorf("bucket = %q, want filable: a member with a performer can be filed", got[0].Bucket)
	}
}

// The folder name is where the answer lives. Stash names one performer across
// a whole pack in 6 of 103 folders on the reference library and nobody at all
// in 93, while the folder is called "Toni Camille pack".
func TestGroupUnfiledPrefersTheFolderNameForTheSuggestion(t *testing.T) {
	found := []stash.UnfiledScene{
		scene("1", root+`\Unsorted\Toni Camille pack\a.mp4`,
			stash.ScenePerformer{Name: "A Co-Star", SceneCount: 2}),
	}
	items, _ := groupUnfiled(found, root, "", func(name string) string {
		if name == "Toni Camille pack" {
			return "Toni Camille"
		}
		return ""
	})
	if items[0].Suggested != "Toni Camille" {
		t.Errorf("suggested %q, want the folder name's performer", items[0].Suggested)
	}
}

// When the folder name says nothing, whatever Stash managed to name inside it
// is still better than nothing.
func TestGroupUnfiledFallsBackToMemberPerformers(t *testing.T) {
	found := []stash.UnfiledScene{
		scene("1", root+`\Unsorted\[OnlyFans] @oh_honey69\a.mp4`,
			stash.ScenePerformer{Name: "Honey", SceneCount: 4}),
		scene("2", root+`\Unsorted\[OnlyFans] @oh_honey69\b.mp4`,
			stash.ScenePerformer{Name: "Honey", SceneCount: 4}),
	}
	items, _ := groupUnfiled(found, root, "", noSuggest)
	if items[0].Suggested != "Honey" {
		t.Errorf("suggested %q, want the most-named member performer", items[0].Suggested)
	}
}

// Filtering happens after grouping, or a pack whose members span buckets would
// appear and disappear depending on which file was looked at first.
func TestGroupUnfiledFiltersByBucket(t *testing.T) {
	found := []stash.UnfiledScene{
		identified(scene("1", root+`\Unsorted\Ident pack\a.mp4`)),
		scene("2", root+`\Unsorted\plain.mp4`),
	}
	items, counts := groupUnfiled(found, root, "unknown", noSuggest)
	if len(items) != 1 || items[0].Kind != kindFile {
		t.Fatalf("want just the loose unknown file, got %+v", items)
	}
	// Counts stay whole-population so the chips do not change as you filter.
	if counts["identified"] != 1 || counts["unknown"] != 1 {
		t.Errorf("counts should describe everything, got %+v", counts)
	}
}

func TestPackFolder(t *testing.T) {
	for _, c := range []struct{ path, want string }{
		{root + `\Unsorted\Toni Camille pack\a.mp4`, "Toni Camille pack"},
		{root + `\Unsorted\pack\sub\deep\a.mp4`, "pack"},
		{root + `\A Pack\a.mp4`, "A Pack"},
		{root + `\Unsorted\a.mp4`, ""},
		{root + `\a.mp4`, ""},
		{root + `/Unsorted/fwd pack/a.mp4`, "fwd pack"},
		{root + `\unsorted\lower\a.mp4`, "lower"},
	} {
		if got := packFolder(c.path, root); got != c.want {
			t.Errorf("packFolder(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}
