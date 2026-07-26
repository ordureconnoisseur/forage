package api

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ordureconnoisseur/forager/internal/clientpool"
	"github.com/ordureconnoisseur/forager/internal/stash"
)

// floorServer is the bare Server bestWatchMatch needs. contentDeadReleases
// tolerates a nil db (it reads dead-release memory best-effort), so no
// fixtures are required to exercise the candidate filter.
func floorServer() *Server {
	return &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func rel(title string, score int) sceneRelease {
	return sceneRelease{
		Title: title, DownloadURL: "http://x/" + title,
		Verified: true, Score: score, Seeders: 10, Protocol: "usenet",
	}
}

// TestOwnedFloorBlocksStrictDowngrade: with a 1080p copy on disk, a release
// that STATES a lower resolution must not be auto-selected.
//
// Note the limit this pins, and why the floor is only a secondary guard: the
// height comes from parsing the TITLE. ResolutionHeight knows the standard
// tiers only, so "400p" (the actual resolution of the release that motivated
// this) reads as unknown — and the real title, an EvilAngel PornoLab post,
// carried no resolution at all. Nothing title-based could have caught it. The
// primary fix is watchSatisfiedByLibrary, which stops the search regardless of
// what the title says.
func TestOwnedFloorBlocksStrictDowngrade(t *testing.T) {
	s := floorServer()
	cands := []sceneRelease{rel("Chloe.Squirting.Deepthroat.Anal.480p", 100)}

	if got := s.bestWatchMatch(cands, nil, 0, 1080); got != nil {
		t.Fatalf("selected %q despite a 1080p copy already on disk", got.Title)
	}
	// Same candidate with nothing owned: perfectly grabbable.
	if got := s.bestWatchMatch(cands, nil, 0, 0); got == nil {
		t.Fatal("with nothing owned, a 480p release is still a legitimate grab")
	}
}

// TestWatchSatisfiedByLibrary is the primary guard: an ordinary watch whose
// scene is already in the library must go quiet instead of searching. Seeds the
// owned-copies memo directly so no Stash call is needed.
func TestWatchSatisfiedByLibrary(t *testing.T) {
	ctx := context.Background()
	const scene = "b983cb11-ab4d-4124-a144-62c8c0e33729"

	s := floorServer()
	s.ownedCopies = map[string][]stash.SceneRef{
		scene: {{SceneID: "175309", Height: 1080, Path: `Z:\Media\Chloe Cherry\hevc.mp4`}},
	}
	s.ownedCopiesFetched = time.Now()

	if !s.watchSatisfiedByLibrary(ctx, scene) {
		t.Error("a scene with a copy in the library should satisfy its watch")
	}
	if s.watchSatisfiedByLibrary(ctx, "some-other-scene") {
		t.Error("an unowned scene must not be treated as satisfied")
	}
}

// TestWatchSatisfiedByLibraryFailsOpen: if the library can't be read, a watch
// must keep hunting. Silencing watches on a Stash outage would quietly stop
// forage acquiring anything.
func TestWatchSatisfiedByLibraryFailsOpen(t *testing.T) {
	s := floorServer()
	s.pool = clientpool.New() // no Stash configured → ownedSceneCopies errors

	if s.watchSatisfiedByLibrary(context.Background(), "any-scene") {
		t.Error("an unreadable library must not satisfy a watch")
	}
}

// TestOwnedFloorAllowsEqualAndBetter: only a STRICT downgrade is refused. An
// equal-height release can be a better encode, and a taller one is plainly
// wanted; blocking either would leave scenes unfilled.
func TestOwnedFloorAllowsEqualAndBetter(t *testing.T) {
	s := floorServer()

	for _, title := range []string{"Scene.1080p.WEB-DL", "Scene.2160p.WEB-DL"} {
		got := s.bestWatchMatch([]sceneRelease{rel(title, 100)}, nil, 0, 1080)
		if got == nil {
			t.Errorf("%s was refused against a 1080p owned copy; only strict downgrades should be", title)
		}
	}
}

// TestOwnedFloorAllowsUnparseableTitle is the guard's own safety property, and
// the reason it can't just compare heights blindly: ResolutionHeight returns 0
// for a title carrying no resolution, and reading that as "0p, therefore a
// downgrade" would silently block a large slice of real releases — PornoLab
// titles routinely don't state one.
func TestOwnedFloorAllowsUnparseableTitle(t *testing.T) {
	s := floorServer()
	cands := []sceneRelease{rel("[EvilAngel.com] Chloe Cherry (Chloe: Squirting, Deepthroat, Anal)", 100)}

	if got := s.bestWatchMatch(cands, nil, 0, 1080); got == nil {
		t.Fatal("a title with no parseable resolution was blocked; unknown must mean allow")
	}
}

// TestOwnedFloorPicksBestAboveFloor: the floor filters, it doesn't disturb
// ranking among what survives.
func TestOwnedFloorPicksBestAboveFloor(t *testing.T) {
	s := floorServer()
	cands := []sceneRelease{
		rel("Scene.480p", 900),  // highest score, but a downgrade
		rel("Scene.1080p", 100), // allowed (equal to the floor)
	}
	got := s.bestWatchMatch(cands, nil, 0, 1080)
	if got == nil || got.Title != "Scene.1080p" {
		t.Fatalf("got %v, want the 1080p release — a downgrade must not win on score alone", got)
	}
}

// TestUpgradeFloorUnaffected: upgrade watches keep their own, stricter rule
// (must BEAT the owned copy). The new floor is passed as 0 for them, so this
// pins that the two don't interfere.
func TestUpgradeFloorUnaffected(t *testing.T) {
	s := floorServer()
	cands := []sceneRelease{rel("Scene.1080p", 100)}

	if got := s.bestWatchMatch(cands, nil, 1080, 0); got != nil {
		t.Error("an equal-height release is not an upgrade and must be skipped")
	}
	if got := s.bestWatchMatch([]sceneRelease{rel("Scene.2160p", 100)}, nil, 1080, 0); got == nil {
		t.Error("a taller release must still satisfy an upgrade watch")
	}
}
