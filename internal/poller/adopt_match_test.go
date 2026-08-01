package poller

import (
	"context"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/matcher"
	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

func sceneCand(id, title string, conf float64, cast ...string) matcher.Candidate {
	sc := stashdb.Scene{ID: id, Title: title}
	for _, n := range cast {
		sc.Performers = append(sc.Performers, stashdb.ScenePerformer{Name: n})
	}
	return matcher.Candidate{Scene: sc, Confidence: conf}
}

// The folder comes from the matched scene's cast, and a performer the library
// already has beats billing order — the same rule placementPerformer uses, so
// a scene lands in the same folder however forage came to know what it is.
func TestCastFolderPrefersALocalPerformer(t *testing.T) {
	p := newTestPoller(t)
	seedLocalPerformer(t, p, "Kenzie Reeves", "7")

	got := p.castFolder(context.Background(),
		sceneCand("s1", "Some Scene", 0.9, "Unknown Newcomer", "Kenzie Reeves"))
	if got != "Kenzie Reeves" {
		t.Errorf("got %q, want the performer the library already has", got)
	}
}

// Nobody local: the billed lead still beats Unsorted, and the performer page
// offers to add them.
func TestCastFolderFallsBackToTheBilledLead(t *testing.T) {
	p := newTestPoller(t)
	got := p.castFolder(context.Background(),
		sceneCand("s1", "Some Scene", 0.9, "First Billed", "Second Billed"))
	if got != "First Billed" {
		t.Errorf("got %q, want the billed lead", got)
	}
}

// A credited alias is how the scene actually bills them, so it wins over the
// canonical name for the folder.
func TestCastFolderUsesTheCreditedName(t *testing.T) {
	p := newTestPoller(t)
	c := sceneCand("s1", "Some Scene", 0.9)
	c.Scene.Performers = []stashdb.ScenePerformer{{Name: "Real Name", As: "Stage Name"}}
	if got := p.castFolder(context.Background(), c); got != "Stage Name" {
		t.Errorf("got %q, want the credited name", got)
	}
}

// A scene with no cast recorded gives nothing to file under, and must not
// invent one.
func TestCastFolderEmptyCast(t *testing.T) {
	p := newTestPoller(t)
	if got := p.castFolder(context.Background(), sceneCand("s1", "Some Scene", 0.9)); got != "" {
		t.Errorf("got %q, want empty so the caller falls through to Unsorted", got)
	}
}

// Without StashDB there is no matcher, and identification must degrade to
// "no answer" rather than failing an adoption.
func TestIdentifyAdoptedWithoutStashDB(t *testing.T) {
	p := newTestPoller(t) // clientpool.New() with no config: no StashDB
	got := p.identifyAdopted(context.Background(), "Some.Release.26.01.02.Someone.XXX.1080p")
	if got != (adoptedIdentity{}) {
		t.Errorf("got %+v, want the zero identity", got)
	}
}

// An empty release name is not a question worth asking StashDB.
func TestIdentifyAdoptedEmptyTitle(t *testing.T) {
	p := newTestPoller(t)
	if got := p.identifyAdopted(context.Background(), ""); got != (adoptedIdentity{}) {
		t.Errorf("got %+v, want the zero identity", got)
	}
}
