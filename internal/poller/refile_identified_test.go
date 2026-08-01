package poller

import (
	"testing"

	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/stash"
)

// The folder is the performer you already collect most of. Any other choice
// scatters someone's work across their co-stars' folders.
func TestMostCollected(t *testing.T) {
	got := mostCollected([]stash.ScenePerformer{
		{Name: "Guest Star", SceneCount: 3},
		{Name: "The One You Collect", SceneCount: 214},
		{Name: "Also Present", SceneCount: 11},
	})
	if got != "The One You Collect" {
		t.Errorf("got %q", got)
	}
}

// A single-name cast is that name, counts notwithstanding.
func TestMostCollectedSingle(t *testing.T) {
	if got := mostCollected([]stash.ScenePerformer{{Name: "Only Her"}}); got != "Only Her" {
		t.Errorf("got %q, want the only performer even with a zero count", got)
	}
}

// Nobody on the scene, or nobody with a name: there is nothing to file under
// and Unsorted remains the honest answer.
func TestMostCollectedNobody(t *testing.T) {
	for _, c := range [][]stash.ScenePerformer{
		nil,
		{},
		{{Name: "", SceneCount: 99}},
	} {
		if got := mostCollected(c); got != "" {
			t.Errorf("mostCollected(%v) = %q, want empty", c, got)
		}
	}
}

// A grab that already has a folder is never moved. That name is a decision —
// the adoption guess, the matcher, or a person using the set-performer
// endpoint — and relocating a deliberately placed file is worse than leaving
// a correct but unexciting one alone.
func TestRefileLeavesAnExistingFolderAlone(t *testing.T) {
	p := newTestPoller(t)
	g := &grabs.Grab{
		ID: 1, PerformerName: "Chosen By Hand",
		PlacedPath: "/lib/Chosen By Hand/scene.mp4",
	}
	// nil Stash client: if this returned true it would have acted without
	// even being able to look the scene up.
	if p.refileIdentified(t.Context(), nil, g, "scene-1") {
		t.Error("must not re-file a grab that already has a folder")
	}
	if g.PerformerName != "Chosen By Hand" {
		t.Errorf("performer changed to %q", g.PerformerName)
	}
}

// Nothing placed yet, or no scene identified: nothing to move.
func TestRefileNoOpWithoutPlacementOrScene(t *testing.T) {
	p := newTestPoller(t)
	for _, c := range []struct {
		name  string
		g     *grabs.Grab
		scene string
	}{
		{"not placed", &grabs.Grab{ID: 1}, "scene-1"},
		{"no scene id", &grabs.Grab{ID: 2, PlacedPath: "/lib/Unsorted/x.mp4"}, ""},
	} {
		if p.refileIdentified(t.Context(), nil, c.g, c.scene) {
			t.Errorf("%s: expected a no-op", c.name)
		}
	}
}
