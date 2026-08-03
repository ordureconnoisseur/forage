package poller

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/ordureconnoisseur/forager/internal/grabs"
)

// Choosing the folder a grab lands in.
//
// The placer files under <library>/<performer>/ and falls back to the
// unfiled bin (placer.UnfiledFolder)
// when it is handed an empty name. That fallback was doing far more work than
// intended: on a sample of 300 recent grabs, 91 landed in Unsorted and every
// one of them was CONFIRMED, meaning forage's prediction and the phash agreed
// on exactly which scene the file was. It identified them correctly and filed
// them in the junk drawer anyway. Unsorted holds 5,325 scenes and was still
// growing daily.
//
// The cause is that the folder came only from grabs.performer_name, which is
// captured from wherever the watch was created. Watch a scene from a performer
// page and it is set; take a batch from Discover whose first billed performer
// is not in your library, or a subscription, or a bare scene watch, and it is
// empty. Nothing later filled it in, even though by then forage knew the scene
// and its full cast.
//
// watches.performers already holds every performer name on the scene, captured
// at add time and backfilled by the watch loop. So the answer was in the
// database the whole time, one join away.

// placementPerformer returns the folder name for a grab, or "" to leave the
// decision to the placer's own Unsorted fallback.
//
// Order matters. The name captured on the grab wins outright: it is what the
// user was looking at when they asked for this, and re-deriving over the top
// of it would move files out from under an existing, deliberate arrangement.
// Only when that is empty does this consult the scene's cast.
func (p *Poller) placementPerformer(ctx context.Context, g *grabs.Grab) string {
	if g.PerformerName != "" {
		return g.PerformerName
	}
	names := p.watchPerformers(ctx, g.PredictedStashDBID)
	if len(names) == 0 {
		// Fall back to whatever the phash later resolved the file to, for
		// grabs whose prediction was wrong or absent.
		names = p.watchPerformers(ctx, g.ActualStashDBID)
	}
	if len(names) == 0 {
		return ""
	}
	// Prefer a performer the library already has a folder for. Filing under a
	// name that is not in Stash creates a folder nothing else will ever use,
	// which is a tidier-looking version of the same problem.
	for _, n := range names {
		if n != "" && p.localPerformerID(ctx, n) != "" {
			return n
		}
	}
	// Nobody local: Unsorted.
	//
	// This used to fall back to the billed lead, on the reasoning that a named
	// folder beats Unsorted. It does not. A performer folder that is not a
	// Stash record is a folder nothing else in the system can reason about:
	// it never appears on the performer page, never gets counted as owned,
	// and the next pass has no way to tell it from a typo. The library has
	// 280 such folders already (Cosplay, PMV, TS Webcam Collection 7) and
	// they are exactly the ones nothing can act on.
	//
	// Unsorted is honest and reversible. Add the performer in Stash and the
	// next pass files it properly.
	return ""
}

// watchPerformers reads the cast a watch recorded for a scene. Returns nil for
// an unknown scene, an empty list, or malformed JSON: every one of those means
// "no better answer than Unsorted", and none is worth failing a placement over.
func (p *Poller) watchPerformers(ctx context.Context, stashDBID string) []string {
	if stashDBID == "" || p.db == nil {
		return nil
	}
	var raw string
	err := p.db.QueryRowContext(ctx,
		`SELECT performers FROM watches WHERE stashdb_id = ?`, stashDBID).Scan(&raw)
	if err != nil {
		if err != sql.ErrNoRows {
			p.log.Warn("placement performer lookup", "scene", stashDBID, "err", err)
		}
		return nil
	}
	var names []string
	if err := json.Unmarshal([]byte(raw), &names); err != nil {
		return nil
	}
	return names
}
