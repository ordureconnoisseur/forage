package poller

import (
	"context"
	"os"

	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/stash"
)

// Re-filing a grab once Stash says what it is.
//
// An adopted download arrives with nothing but a release name, so the folder
// is a guess made before anyone knows what the file contains. When the guess
// declines, it lands in Unsorted — and there it stayed, permanently, even
// after Stash's own identify attached a StashDB scene to it. forage learned
// the answer and did nothing with it.
//
// Measured on the live library: 561 grabs sitting under Unsorted, of which
// Stash had identified 203, and 201 of those still had no performer folder.
// The information to file them correctly had been sitting in the database.
//
// This is strictly better evidence than anything available earlier. Adoption
// guesses from a filename; the matcher answers from a release title. Here
// Stash has matched the actual file, by hash, to a scene, and attached the
// performers the LIBRARY holds — so the folder is guaranteed to be one that
// already exists rather than a new one named after a stranger.
//
// The pack path has done this since it was written (advancePackDistribute
// hardlinks each identified scene into its most-collected performer's
// folder). Singles simply never got it.

// refileIdentified moves a placed grab into its identified scene's performer
// folder. Reports whether it changed g.
//
// Only ever moves a grab that has no folder of its own. A grab filed under a
// name — by the adoption guess, by the matcher, or by hand through the
// set-performer endpoint — is left exactly where it is: those are decisions,
// and quietly relocating a file someone deliberately placed is worse than
// leaving a correct-but-unexciting one alone.
func (p *Poller) refileIdentified(ctx context.Context, sc *stash.Client, g *grabs.Grab, sceneID string) bool {
	if sc == nil || sceneID == "" || g.PerformerName != "" || g.PlacedPath == "" {
		return false
	}
	pl := p.pool.Placer()
	if !pl.Configured() || !p.libraryHealthy() {
		return false
	}
	perfs, err := sc.ScenePerformers(ctx, sceneID)
	if err != nil {
		p.log.Warn("refile: scene performers", "id", g.ID, "scene", sceneID, "err", err)
		return false
	}
	folder := mostCollected(perfs)
	if folder == "" {
		return false // identified, but nobody is on it: Unsorted is the answer
	}

	res, err := pl.Place(g.PlacedPath, folder)
	if err != nil {
		// Not fatal and not retried aggressively: the grab is already placed
		// and confirmed, so a failure here costs tidiness, not the file.
		p.log.Warn("refile: place failed", "id", g.ID, "performer", folder, "err", err)
		return false
	}
	if res.Path == g.PlacedPath {
		return false // already there
	}
	// Drop the old library link. Library side only — the download client's
	// copy is never touched, so anything still seeding keeps seeding.
	if err := os.RemoveAll(g.PlacedPath); err != nil {
		p.log.Warn("refile: remove old placement", "id", g.ID, "path", g.PlacedPath, "err", err)
	}
	p.log.Info("refiled identified grab", "id", g.ID, "scene", sceneID,
		"from", g.PlacedPath, "to", res.Path, "performer", folder)
	g.PlacedPath = res.Path
	g.PerformerName = folder
	g.Reason = "re-filed under " + folder + " after identify"
	return true
}

// mostCollected picks the performer whose folder the file belongs in: the one
// you already hold the most scenes of. A two-hander goes to whichever of them
// you actually collect, which is the same rule the pack distribute step uses,
// and the reason it is a rule at all is that any other choice scatters a
// performer's work across their co-stars' folders.
func mostCollected(perfs []stash.ScenePerformer) string {
	var top stash.ScenePerformer
	for _, pf := range perfs {
		if pf.Name == "" {
			continue
		}
		if top.Name == "" || pf.SceneCount > top.SceneCount {
			top = pf
		}
	}
	return top.Name
}
