package poller

import (
	"context"
	"errors"
	"time"

	"github.com/ordureconnoisseur/forager/internal/clienterr"
	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/pathmap"
	"github.com/ordureconnoisseur/forager/internal/stash"
)

// ── Late-identify reconcile ─────────────────────────────────────────
//
// A single grab that lands in the library without a StashDB cross-id
// settles as confirmed anyway — "in library (scanned)" once
// singleIdentifyGrace passes for an adopted grab, "in library; no StashDB
// match" once the orphan window passes for a predicted one. That's the
// right call at the time: the alternative is leaving it in flight forever
// for content StashDB genuinely doesn't have.
//
// But settling is where the record stopped being maintained. Active()
// excludes 'confirmed', so nothing revisits the grab, while the link can
// still arrive afterwards two ways:
//
//   - Stash's identify is a serial queued job. It routinely runs after the
//     grace expires when the queue is deep (scans, phash generation), so
//     the cross-id lands on the scene minutes or hours past the settle.
//   - the user identifies the scene by hand in Stash, any time later.
//
// Either way Stash holds the link and the grab still says it was never
// matched — which the Grabs list then shows as NO MATCH on a scene that
// is, in fact, matched. (Matching through forage's own Find matches button
// writes the grab directly, so only these two paths drift.)
//
// reconcileConfirmed closes that gap: on a slow cadence it re-checks a
// bounded batch of settled-but-unlinked grabs and backfills the cross-id
// Stash has since gained. Fixing the record rather than papering over it in
// the UI keeps every consumer honest at once — the row badge, the status
// filters and the scene-group tallies all read the same field.

const (
	// reconcileInterval is the minimum gap between reconcile passes. This
	// is pure catch-up work — the main confirm path already handles every
	// link that arrives while the grab is still active — so it runs far
	// slower than the tick.
	reconcileInterval = 15 * time.Minute

	// reconcileWindow bounds how far back a pass looks. A late identify
	// lands within hours; a hand-identify might take days. Past that a
	// still-unlinked file is genuinely not on StashDB, and re-querying it
	// forever costs one Stash round-trip per pass to learn nothing.
	reconcileWindow = 14 * 24 * time.Hour

	// reconcileBatch caps Stash lookups per pass. Files that never get
	// identified stay in the result set permanently, so a pass must not
	// try to drain it — the cursor rotates through instead.
	reconcileBatch = 40
)

// reconcileConfirmed backfills the StashDB cross-id on settled grabs Stash
// has identified since they went terminal, and corrects the status when the
// late link turns out to be a different scene than forage predicted.
//
// Best-effort throughout: it never fails a tick. A Stash outage just means
// this pass finds nothing and the next one retries.
func (p *Poller) reconcileConfirmed(ctx context.Context) {
	stashC := p.pool.Stash()
	if stashC == nil {
		return
	}
	since := time.Now().Add(-reconcileWindow).Unix()
	// Each sub-pass owns its own early exits — the first version returned
	// from the whole reconcile when there were zero unlinked grabs, which
	// silently skipped the mismatch recovery below. (The tests caught it;
	// the nothing-active early return in tickOnce was the same lesson.)
	p.reconcileUnlinked(ctx, stashC, since)
	p.reconcileMismatched(ctx, stashC, since)
}

// reconcileUnlinked backfills cross-ids onto confirmed-but-unlinked grabs.
func (p *Poller) reconcileUnlinked(ctx context.Context, stashC *stash.Client, since int64) {
	total, err := p.repo.CountConfirmedUnlinked(ctx, since)
	if err != nil {
		p.log.Warn("reconcile: count unlinked", "err", err)
		return
	}
	if total == 0 {
		p.reconcileCursor = 0
		return
	}
	// Rotate: the never-identified rows are permanent residents of this set,
	// so a fixed offset-0 batch would re-check the same ones every pass and
	// never reach the rest.
	if p.reconcileCursor >= total {
		p.reconcileCursor = 0
	}
	rows, err := p.repo.ConfirmedUnlinked(ctx, since, reconcileBatch, p.reconcileCursor)
	if err != nil {
		p.log.Warn("reconcile: list unlinked", "err", err)
		return
	}
	p.reconcileCursor += reconcileBatch

	linked := 0
	for i := range rows {
		g := rows[i]
		needle := pathmap.Base(g.PlacedPath)
		if needle == "" {
			continue
		}
		scene, err := stashC.FindSceneByPathContains(ctx, needle)
		if err != nil {
			// ErrNotFound is the ordinary answer here (the file was moved or
			// removed outside forage) — not worth a log line per grab.
			if !errors.Is(err, clienterr.ErrNotFound) {
				p.log.Warn("reconcile: stash lookup", "id", g.ID, "err", err)
			}
			continue
		}
		if scene == nil || scene.StashDBID == "" {
			continue // still unidentified; a later pass will look again
		}
		p.applyLateLink(ctx, g, scene.StashDBID)
		linked++
	}
	if linked > 0 {
		p.log.Info("reconcile: backfilled late StashDB links",
			"linked", linked, "checked", len(rows), "unlinked_total", total)
	}
}

// reconcileMismatched gives a mismatched grab its recovery path. The
// mismatch verdict is the machine's ("stash phash → different scene than
// predicted"), but the user can overrule it in Stash by re-identifying the
// scene — and until this pass, nothing ever noticed: mismatched grabs are
// out of Active(), so a correction left the grab claiming a mismatch
// forever, its watch held, its row amber (the "no recovery path" residual
// in docs/error-handling.md).
//
// Only the CORRECTED-to-predicted case flips: the scene's current cross-id
// equalling the prediction is unambiguous evidence the user fixed the
// match. A scene re-identified to some third id stays mismatched — that is
// still a question for the user, and forage doesn't guess.
func (p *Poller) reconcileMismatched(ctx context.Context, stashC *stash.Client, since int64) {
	rows, err := p.repo.MismatchedRecent(ctx, since, reconcileBatch)
	if err != nil {
		p.log.Warn("reconcile: list mismatched", "err", err)
		return
	}
	fixed := 0
	for i := range rows {
		g := rows[i]
		if g.PredictedStashDBID == "" {
			continue
		}
		needle := pathmap.Base(g.PlacedPath)
		if needle == "" {
			continue
		}
		scene, err := stashC.FindSceneByPathContains(ctx, needle)
		if err != nil || scene == nil || scene.StashDBID != g.PredictedStashDBID {
			continue
		}
		g.ActualStashDBID = scene.StashDBID
		g.Status = "confirmed"
		g.Reason = "match corrected in stash to the predicted scene"
		if err := p.repo.Update(ctx, g); err != nil {
			if !errors.Is(err, grabs.ErrStaleUpdate) {
				p.log.Warn("reconcile: mismatch fix update", "id", g.ID, "err", err)
			}
			continue
		}
		fixed++
		p.log.Info("reconcile: mismatch corrected in stash; grab confirmed",
			"id", g.ID, "stashdb_id", scene.StashDBID)
	}
	if fixed > 0 {
		p.log.Info("reconcile: recovered corrected mismatches", "fixed", fixed, "checked", len(rows))
	}
}

// applyLateLink writes a cross-id that arrived after the grab settled,
// mirroring the confirm path's own predicted-vs-actual verdict so a grab
// reconciled here is indistinguishable from one confirmed on time.
func (p *Poller) applyLateLink(ctx context.Context, g grabs.Grab, stashDBID string) {
	g.ActualStashDBID = stashDBID
	switch {
	case g.PredictedStashDBID == "":
		// Adopted / manual grab: nothing was predicted, so there's nothing
		// this could contradict.
		g.Status = "confirmed"
		g.Reason = "stash identified it after settling"
	case stashDBID == g.PredictedStashDBID:
		g.Status = "confirmed"
		g.Reason = "stash identified the predicted scene after settling"
	default:
		// The late link points somewhere else. Same verdict the on-time path
		// reaches, so the row stops claiming a clean landing and the
		// pick-another-release escape hatch appears.
		g.Status = "mismatched"
		g.Reason = "stash identified a different scene than predicted"
	}
	if err := p.repo.Update(ctx, g); err != nil {
		if errors.Is(err, grabs.ErrStaleUpdate) {
			// The user changed this grab between our read and write (a manual
			// match, a re-file). Theirs wins; the next pass re-reads.
			p.log.Info("reconcile: grab changed under pass; skipping", "id", g.ID)
			return
		}
		p.log.Warn("reconcile: update", "id", g.ID, "err", err)
		return
	}
	p.log.Info("reconcile: late StashDB link",
		"id", g.ID, "stashdb_id", stashDBID, "status", g.Status)
}
