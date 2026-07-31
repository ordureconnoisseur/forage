package poller

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/ordureconnoisseur/forager/internal/clientpool"
	"github.com/ordureconnoisseur/forager/internal/db"
	"github.com/ordureconnoisseur/forager/internal/grabs"
)

// newTestPoller is the minimum Poller placementPerformer needs: a database.
// The full rig stands up four fake HTTP servers, none of which this touches.
func newTestPoller(t *testing.T) *Poller {
	t.Helper()
	dbh, err := db.Open(filepath.Join(t.TempDir(), "forager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbh.Close() })
	return New(grabs.NewRepo(dbh), dbh, clientpool.New(),
		slog.New(slog.NewTextHandler(io.Discard, nil)), time.Minute, 6*time.Hour, nil)
}

func seedWatch(t *testing.T, p *Poller, sceneID, performersJSON string) {
	t.Helper()
	_, err := p.db.Exec(
		`INSERT INTO watches (stashdb_id, performers, created_at) VALUES (?, ?, 0)`,
		sceneID, performersJSON)
	if err != nil {
		t.Fatal(err)
	}
}

func seedLocalPerformer(t *testing.T, p *Poller, name, id string) {
	t.Helper()
	if _, err := p.db.Exec(
		`INSERT INTO performer_cache (stash_id, name, refreshed_at) VALUES (?, ?, 0)`,
		id, name); err != nil {
		t.Fatal(err)
	}
}

// The name captured on the grab wins outright. It is what the user was looking
// at when they asked for the scene, and re-deriving over it would move files
// out from under a deliberate arrangement.
func TestPlacementPerformerKeepsTheCapturedName(t *testing.T) {
	p := newTestPoller(t)
	seedWatch(t, p, "scene-1", `["Someone Else","Another"]`)
	seedLocalPerformer(t, p, "Another", "42")

	g := &grabs.Grab{PerformerName: "Chosen By Hand", PredictedStashDBID: "scene-1"}
	if got := p.placementPerformer(context.Background(), g); got != "Chosen By Hand" {
		t.Errorf("got %q, want the captured name untouched", got)
	}
}

// The case that produced 5,325 Unsorted scenes: a confirmed grab whose watch
// recorded no performer to file under, but whose scene has a cast.
func TestPlacementPerformerFallsBackToTheCast(t *testing.T) {
	p := newTestPoller(t)
	seedWatch(t, p, "scene-1", `["Suraya Ndia","Some Guy"]`)

	g := &grabs.Grab{PredictedStashDBID: "scene-1"}
	if got := p.placementPerformer(context.Background(), g); got != "Suraya Ndia" {
		t.Errorf("got %q, want the billed lead rather than Unsorted", got)
	}
}

// A performer the library already has a folder for beats the billed order.
// Filing under a name Stash does not know creates a folder nothing else will
// ever use, which is a tidier-looking version of the same problem.
func TestPlacementPerformerPrefersOneInTheLibrary(t *testing.T) {
	p := newTestPoller(t)
	seedWatch(t, p, "scene-1", `["Unknown Newcomer","Kenzie Reeves"]`)
	seedLocalPerformer(t, p, "Kenzie Reeves", "7")

	if got := p.placementPerformer(context.Background(), &grabs.Grab{PredictedStashDBID: "scene-1"}); got != "Kenzie Reeves" {
		t.Errorf("got %q, want the performer the library already has", got)
	}
}

// When the prediction was wrong or absent, the phash-resolved scene is the
// next best source of a cast.
func TestPlacementPerformerUsesTheActualSceneWhenPredictionMisses(t *testing.T) {
	p := newTestPoller(t)
	seedWatch(t, p, "real-scene", `["Aspen Reign"]`)

	g := &grabs.Grab{PredictedStashDBID: "never-watched", ActualStashDBID: "real-scene"}
	if got := p.placementPerformer(context.Background(), g); got != "Aspen Reign" {
		t.Errorf("got %q, want the cast of the scene the phash resolved", got)
	}
}

// No watch, no cast, malformed JSON, empty names: every one of these means
// "no better answer than Unsorted", and none may fail a placement.
func TestPlacementPerformerFallsThroughToUnsorted(t *testing.T) {
	p := newTestPoller(t)
	seedWatch(t, p, "empty-cast", `[]`)
	seedWatch(t, p, "bad-json", `not json at all`)
	seedWatch(t, p, "blank-names", `["",""]`)

	for _, c := range []struct{ name, scene string }{
		{"no watch row", "never-heard-of-it"},
		{"empty cast", "empty-cast"},
		{"malformed json", "bad-json"},
		{"blank names", "blank-names"},
		{"no scene id at all", ""},
	} {
		g := &grabs.Grab{PredictedStashDBID: c.scene}
		if got := p.placementPerformer(context.Background(), g); got != "" {
			t.Errorf("%s: got %q, want \"\" so the placer's own fallback applies", c.name, got)
		}
	}
}
