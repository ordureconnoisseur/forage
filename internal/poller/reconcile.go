package poller

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
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

	// There is deliberately NO recency window here any more. Both passes
	// used to carry one, and both were measured to be losing real work
	// because of it: 30% of aged-out unlinked grabs already had a stash_id
	// in Stash, and 91% of mismatched grabs had aged past recovery. People
	// identify and re-file their library on their own schedule, not within
	// a fortnight of downloading. The cursors below bound the cost instead.

	// reconcileBatch caps Stash lookups per pass. Files that never get
	// identified stay in the result set permanently, so a pass must not
	// try to drain it — the cursor rotates through instead.
	reconcileBatch = 40

	// The moved-file pass separates how WIDE it scans from how much work
	// it may do, because its two costs are wildly different: every row
	// costs one os.Stat, and only the rare row whose file is missing costs
	// a Stash lookup plus a write. Sharing reconcileBatch made coverage
	// hostage to the expensive case — 1637 rows on the reference instance
	// at 40 per 15-minute pass is ~10 hours to notice a moved file.
	// Scanning wider closes that to about an hour while the repair cap
	// keeps the expensive half bounded to the same 40 as before.
	movedScanBatch = 400
	// Caps the Stash round-trips a pass may make, which is the only
	// expensive thing here — the 400-row scan itself is just os.Stat.
	movedLookupCap = 40
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
	// Each sub-pass owns its own early exits — the first version returned
	// from the whole reconcile when there were zero unlinked grabs, which
	// silently skipped the mismatch recovery below. (The tests caught it;
	// the nothing-active early return in tickOnce was the same lesson.)
	p.reconcileUnlinked(ctx, stashC)
	p.reconcileMismatched(ctx, stashC)
	p.reconcileMovedFiles(ctx, stashC)
}

// reconcileUnlinked backfills cross-ids onto confirmed-but-unlinked grabs.
func (p *Poller) reconcileUnlinked(ctx context.Context, stashC *stash.Client) {
	total, err := p.repo.CountConfirmedUnlinked(ctx)
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
	// never reach the rest. Newest-first ordering means a fresh identify is
	// still seen at the start of each rotation rather than waiting its turn.
	if p.reconcileCursor == 0 {
		p.reconcileCursor = p.loadCursor(ctx, cursorKeyUnlinked)
	}
	if p.reconcileCursor >= total {
		p.reconcileCursor = 0
	}
	rows, err := p.repo.ConfirmedUnlinked(ctx, reconcileBatch, p.reconcileCursor)
	if err != nil {
		p.log.Warn("reconcile: list unlinked", "err", err)
		return
	}
	p.reconcileCursor += reconcileBatch
	p.saveCursor(ctx, cursorKeyUnlinked, p.reconcileCursor)

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
func (p *Poller) reconcileMismatched(ctx context.Context, stashC *stash.Client) {
	total, err := p.repo.CountMismatched(ctx)
	if err != nil {
		p.log.Warn("reconcile: count mismatched", "err", err)
		return
	}
	if total == 0 {
		p.mismatchCursor = 0
		return
	}
	// Rotate: a mismatch the user never resolves is a permanent resident of
	// this set, so a fixed offset-0 batch would re-check the newest few
	// forever and never reach the rest.
	if p.mismatchCursor == 0 {
		p.mismatchCursor = p.loadCursor(ctx, cursorKeyMismatched)
	}
	if p.mismatchCursor >= total {
		p.mismatchCursor = 0
	}
	rows, err := p.repo.Mismatched(ctx, reconcileBatch, p.mismatchCursor)
	if err != nil {
		p.log.Warn("reconcile: list mismatched", "err", err)
		return
	}
	p.mismatchCursor += reconcileBatch
	p.saveCursor(ctx, cursorKeyMismatched, p.mismatchCursor)
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
		g.PhashStashDBID = g.ActualStashDBID
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

// reconcileMovedFiles repairs placed_path on confirmed grabs whose file has
// been moved under forage's feet.
//
// forage records where IT put a file. Anything that reorganises the library
// afterwards — the user filing a scene out of an Unsorted holding folder,
// Stash's own organise — leaves that pointer aimed at nothing, and forage
// never notices. The observed case: 23 grabs, all placed 20 to 40 days
// earlier into /data/porn/Media/Unsorted, whose files had since moved to
// category folders.
//
// A stale pointer is not cosmetic. It costs two things:
//
//   - the seeding cull refuses to retire a torrent it cannot verify a
//     library copy for (correctly — it will not delete on a false
//     negative), so those torrents seed forever with no way out;
//   - a purge RemoveAll's the recorded path, finds nothing, and reports
//     success while the real file survives untouched.
//
// The cross-id makes the repair safe: Stash is asked where THAT scene's
// file is now, the answer is reverse-mapped into forage's namespace, and it
// is only adopted if a file actually exists there. Nothing is inferred from
// a name.
func (p *Poller) reconcileMovedFiles(ctx context.Context, stashC *stash.Client) {
	// With the library mount gone, EVERY placed_path stats missing. Repointing
	// the whole table off Stash during an outage is exactly the kind of
	// confident wrongness the library-health latch exists to prevent.
	if !p.libraryHealthy() {
		return
	}
	mapping := p.pool.Settings().StashPathMapping
	endpoint, eerr := p.identifyEndpoint(ctx, stashC)
	if eerr != nil || endpoint == "" {
		return // no StashDB endpoint configured in Stash: cross-ids can't be resolved
	}

	total, err := p.repo.CountConfirmedPlacedLinked(ctx)
	if err != nil {
		p.log.Warn("reconcile: count placed", "err", err)
		return
	}
	if total == 0 {
		p.movedCursor = 0
		return
	}
	if p.movedCursor == 0 {
		p.movedCursor = p.loadCursor(ctx, cursorKeyMoved)
	}
	if p.movedCursor >= total {
		p.movedCursor = 0
	}
	rows, err := p.repo.ConfirmedPlacedLinked(ctx, movedScanBatch, p.movedCursor)
	if err != nil {
		p.log.Warn("reconcile: list placed", "err", err)
		return
	}
	p.movedCursor += movedScanBatch
	p.saveCursor(ctx, cursorKeyMoved, p.movedCursor)

	repaired, looked := 0, 0
	for i := range rows {
		g := rows[i]
		// Only a MISSING file is a candidate. A present one is correct by
		// definition and must never be second-guessed against Stash.
		if _, serr := os.Stat(g.PlacedPath); serr == nil {
			continue
		}
		// Cap LOOKUPS, not repairs. Counting successes let the expensive half
		// run unbounded in exactly the bad case: a subtree moved somewhere
		// forage cannot see repairs nothing, never reaches the cap, and bills
		// a Stash round-trip per missing row every pass — inside the
		// single-goroutine tick, ahead of placement work.
		if looked >= movedLookupCap {
			break
		}
		looked++
		refs, rerr := stashC.FindSceneRefsByStashID(ctx, endpoint, g.ActualStashDBID)
		if rerr != nil || len(refs) == 0 {
			continue // Stash down, or it no longer knows this scene: leave it alone
		}
		// A cross-id can legitimately resolve to SEVERAL files: Stash returns
		// one ref per file per scene carrying it, and duplicate copies of a
		// scene are normal (95 stashdb ids have more than one grab on the
		// reference instance). Taking the first path that happens to exist
		// would point this grab at a file forage never placed — the user's
		// own separate copy — and a later purge would then delete THAT while
		// the real placed file survived untracked.
		//
		// A move preserves the filename, so the basename is the evidence that
		// two paths are the same file. Anything else is a different file, and
		// two equally-good matches mean we cannot tell which: both are
		// refused, because being wrong here destroys a file.
		want := pathmap.Base(g.PlacedPath)
		fresh := ""
		ambiguous := false
		for _, ref := range refs {
			// An empty mapping means Stash and forage share one mount, so
			// Stash's path IS forage's. Reverse() returns "" for that case,
			// which would silently disable this repair on every single-mount
			// deployment (the common Docker shape) if taken as a failure.
			cand := ref.Path
			if mapping != "" {
				cand = pathmap.Reverse(ref.Path, mapping)
			}
			if cand == "" {
				continue
			}
			// placed_path is not always a file. A third of them (522 of 1637
			// on the reference instance) are DIRECTORIES: a single whose
			// release was a folder with the video inside. Stash only ever
			// reports the file, so matching the leaf basename would refuse
			// every one of those and leave the repair inert exactly where the
			// unsafe version had been doing something.
			//
			// Same principle either way: the recorded NAME is the evidence.
			// Find that name among the candidate's components and adopt the
			// path up to and including it — the file itself when placed_path
			// was a file, its containing directory when it was a directory.
			cand = truncateAtComponent(cand, want)
			if cand == "" || cand == g.PlacedPath {
				continue
			}
			// It must genuinely be there. Without this the repair would just
			// swap one unverifiable path for another.
			if _, serr := os.Stat(cand); serr != nil {
				continue
			}
			if fresh != "" && cand != fresh {
				ambiguous = true
				break
			}
			fresh = cand
		}
		if ambiguous {
			p.log.Warn("reconcile: several candidate paths for a moved file; refusing to guess",
				"id", g.ID, "name", want)
			continue
		}
		if fresh == "" {
			continue
		}
		old := g.PlacedPath
		g.PlacedPath = fresh
		g.Reason = "reconcile: file moved; placed path repaired from Stash"
		if uerr := p.repo.Update(ctx, g); uerr != nil {
			if !errors.Is(uerr, grabs.ErrStaleUpdate) {
				p.log.Warn("reconcile: repair placed path", "id", g.ID, "err", uerr)
			}
			continue
		}
		repaired++
		p.log.Info("reconcile: placed path repaired", "id", g.ID, "from", old, "to", fresh)
	}
	if repaired > 0 {
		p.log.Info("reconcile: repaired moved files", "count", repaired)
	}
}

// truncateAtComponent returns path cut to end at the LAST component equal to
// name, or "" when no component matches. It is how the moved-file repair
// keeps one rule for both shapes of placed_path: a file's own basename
// matches the final component, and a directory's basename matches an
// ancestor, so both resolve to the thing that was actually recorded.
//
// Last rather than first, because a library legitimately repeats names down
// a path (a performer folder and a release folder can share one) and the
// deepest match is the specific one.
func truncateAtComponent(path, name string) string {
	if path == "" || name == "" {
		return ""
	}
	sep := "/"
	if strings.Contains(path, "\\") && !strings.Contains(path, "/") {
		sep = "\\"
	}
	parts := strings.Split(path, sep)
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] == name {
			return strings.Join(parts[:i+1], sep)
		}
	}
	return ""
}

// Cursor persistence ────────────────────────────────────────────────
//
// The three reconcile passes rotate through sets far larger than one
// batch: 808 unlinked grabs, 148 mismatched, 1637 placed paths on the
// reference instance, at 40 rows per pass. Held only in memory, a cursor
// restarts at row 0 on every daemon restart, so the tail of each set is
// reached only if the daemon runs uninterrupted for hours.
//
// That is not hypothetical here. TestPackTagTimeoutSurvivesRestart records
// the same shape biting already: two packs sat in "tagging" for 21 hours
// against a 30-minute budget because a day of deploys kept restarting an
// in-memory clock. A rotation that resets on deploy has exactly that
// failure mode, and a day of deploys is precisely what shipping these
// passes looked like.
//
// Best-effort on both sides: a read failure starts from 0 (costing a
// repeat of work already done, never skipping any), and a write failure is
// logged and dropped rather than failing a tick.

const (
	cursorKeyUnlinked   = "reconcile.cursor.unlinked"
	cursorKeyMismatched = "reconcile.cursor.mismatched"
	cursorKeyMoved      = "reconcile.cursor.moved"
)

func (p *Poller) loadCursor(ctx context.Context, key string) int {
	var stored string
	if err := p.db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = ?`, key).Scan(&stored); err != nil {
		return 0
	}
	n, err := strconv.Atoi(stored)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (p *Poller) saveCursor(ctx context.Context, key string, v int) {
	if _, err := p.db.ExecContext(ctx, `
		INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, strconv.Itoa(v)); err != nil {
		p.log.Warn("reconcile: cursor write", "key", key, "err", err)
	}
}
