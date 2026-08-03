package api

import (
	"testing"

	"github.com/ordureconnoisseur/forager/internal/placer"
	"github.com/ordureconnoisseur/forager/internal/stash"
)

func perf(name string, count int) stash.ScenePerformer {
	return stash.ScenePerformer{Name: name, SceneCount: count}
}

// The three buckets exist because the work each implies is different, so the
// classification is the load-bearing part of this view.
func TestBucketFor(t *testing.T) {
	for _, c := range []struct {
		name string
		in   stash.UnfiledScene
		want string
	}{
		{
			"a performer means it can be filed right now",
			stash.UnfiledScene{Performers: []stash.ScenePerformer{perf("Kenzie Reeves", 40)}},
			"filable",
		},
		{
			// A performer is all filing needs. Whether a metadata source has
			// also seen the scene is irrelevant to the folder it goes in.
			"a performer wins even with no cross-id",
			stash.UnfiledScene{Performers: []stash.ScenePerformer{perf("Someone", 1)}},
			"filable",
		},
		{
			"identified but nobody attached",
			stash.UnfiledScene{StashIDs: []stash.StashID{{Endpoint: "https://stashdb.org/graphql", StashID: "x"}}},
			"identified",
		},
		{
			// The permanent bucket. Amateur and OnlyFans content is largely
			// not on any metadata source and never will be.
			"nothing knows what it is",
			stash.UnfiledScene{},
			"unknown",
		},
		{
			"a blank performer name is not a performer",
			stash.UnfiledScene{Performers: []stash.ScenePerformer{perf("", 99)}},
			"unknown",
		},
	} {
		if got := bucketFor(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// Filing a two-hander under whichever performer you already collect is what
// stops their work scattering across co-stars' folders. Same rule as the
// poller's re-file and the pack distribute step.
func TestTopPerformerName(t *testing.T) {
	got := topPerformerName([]stash.ScenePerformer{
		perf("Guest Star", 3), perf("The One You Collect", 214), perf("Also Present", 11),
	})
	if got != "The One You Collect" {
		t.Errorf("got %q", got)
	}
	if topPerformerName(nil) != "" {
		t.Error("no cast means no suggestion")
	}
	if topPerformerName([]stash.ScenePerformer{perf("", 5)}) != "" {
		t.Error("a blank name must not be suggested as a folder")
	}
}

// A performer name reaches the filesystem as a directory. Anything a path
// cannot hold has to be neutralised, and an empty result must not put files
// in the library root, which is the state this whole view exists to clear.
func TestSanitiseFolder(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"Kenzie Reeves", "Kenzie Reeves"},
		{"  Padded  ", "Padded"},
		{"AC/DC", "AC_DC"},
		{`Back\Slash`, "Back_Slash"},
		{"Colon:Name", "Colon_Name"},
		{`Quote"Star*Q?`, "Quote_Star_Q_"},
		{"Angle<Br>", "Angle_Br_"},
		{"Pipe|Name", "Pipe_Name"},
		// A name that sanitises away entirely takes the fallback bin's
		// current spelling; the legacy one is still recognised everywhere it
		// is READ, it is just no longer what fresh writes choose.
		{"", placer.UnfiledFolder},
		{"   ", placer.UnfiledFolder},
	} {
		if got := sanitiseFolder(c.in); got != c.want {
			t.Errorf("sanitiseFolder(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
