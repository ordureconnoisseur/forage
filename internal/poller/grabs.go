// Package poller is the Phase B background loop. It watches qBit for
// completion of forager-tracked grabs, then watches Stash for the
// corresponding scene to surface, and records actual-vs-predicted
// StashDB IDs on each grab.
//
// The state machine lives here. internal/grabs.Repo is just storage.
package poller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ordureconnoisseur/forager/internal/clientpool"
	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/pathmap"
	"github.com/ordureconnoisseur/forager/internal/qbit"
	"github.com/ordureconnoisseur/forager/internal/sabnzbd"
	"github.com/ordureconnoisseur/forager/internal/stash"
	"github.com/ordureconnoisseur/forager/internal/suggest"
	"github.com/ordureconnoisseur/forager/internal/torrentmeta"
)

// Poller advances grab state machines on a fixed interval. Holds a
// *clientpool.Pool rather than individual clients so /config saves
// reach the next tick automatically — every call into a client goes
// through the pool's atomic accessor.
type Poller struct {
	repo     *grabs.Repo
	db       *sql.DB // performer_cache lookups for adoption folder suggestions
	pool     *clientpool.Pool
	log      *slog.Logger
	interval time.Duration
	orphan   time.Duration

	// stashBoxEndpoint caches the user's StashDB endpoint as
	// configured in Stash. Identify needs it to match exactly;
	// fetched lazily on the first scanned-state transition and
	// reused for the rest of the daemon's lifetime.
	identifyMu       sync.Mutex
	stashBoxEndpoint string

	// adoptMu serialises orphan adoption so the periodic tick and a
	// manual force-adopt (AdoptNow) can't race the known-id check and
	// double-insert the same torrent.
	adoptMu sync.Mutex

	// lastScan throttles per-grab metadataScan retries. The initial
	// post-placement scan can be coalesced by Stash with a concurrent
	// one (e.g. several grabs placed into the same folder in one tick)
	// and miss the file, leaving the grab stuck at "placed". The
	// confirmation step re-triggers the scan on a throttle until Stash
	// indexes the file.
	scanMu   sync.Mutex
	lastScan map[int64]time.Time

	// packScan records, per pack grab, how many scenes Stash has indexed
	// under its directory and when that count last grew. A pack confirms
	// only once the count stops climbing (scan settled), so a
	// partially-scanned directory can't confirm + dedup prematurely.
	// In-memory: lost on restart, which merely re-arms the settle window.
	packMu   sync.Mutex
	packScan map[int64]packScanState
}

// packScanState is the high-water count of indexed pack scenes and the
// time it was last reached, used to detect when Stash's directory scan
// has settled.
type packScanState struct {
	count int
	since time.Time
}

// scanRetryInterval is the minimum gap between metadataScan retries
// for a single stuck grab. Short enough to recover quickly, long
// enough not to spam Stash every poll tick.
const scanRetryInterval = 90 * time.Second

// packScanStableWindow is how long a pack's indexed-scene count must hold
// steady before we treat Stash's directory scan as finished. Above
// scanRetryInterval so at least one scan/wait cycle passes with no new
// scenes — this is what stops a pack confirming mid-scan (when only some
// files are indexed) and the under-count dedup that would follow.
const packScanStableWindow = 3 * scanRetryInterval

// packIndexedFloorPct is the minimum percentage of a pack's expected
// video count (PackFiles) that must be indexed by Stash before a settled
// scan is allowed to confirm. Guards against a restart re-seeding the
// in-memory settle window at a partial count and confirming a half-
// scanned pack. Not 100%: torrents carry sample/extra video files and
// title counts can overstate, so demanding every file risks never
// settling; the orphan backstop covers the genuinely-missing remainder.
const packIndexedFloorPct = 80

// packIdentifyGrace bounds how long we keep retrying Identify on a pack
// whose scan has settled but whose scenes aren't all cross-id'd. Pack
// scenes legitimately may not be on StashDB (amateur content) or the
// title may overstate the count, so past this much time since download
// completion we confirm with whatever identified rather than waiting out
// the full (hours-long) orphan window.
const packIdentifyGrace = 20 * time.Minute

func New(repo *grabs.Repo, db *sql.DB, pool *clientpool.Pool, log *slog.Logger, interval, orphanAfter time.Duration) *Poller {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	if orphanAfter <= 0 {
		orphanAfter = 6 * time.Hour
	}
	return &Poller{
		repo:     repo,
		db:       db,
		pool:     pool,
		log:      log,
		interval: interval,
		orphan:   orphanAfter,
		lastScan: map[int64]time.Time{},
		packScan: map[int64]packScanState{},
	}
}

// Run ticks once at startup then on `interval` until ctx is cancelled.
// Errors are logged and the loop continues; a single bad tick doesn't
// kill the poller.
func (p *Poller) Run(ctx context.Context) {
	p.log.Info("poller starting", "interval", p.interval, "orphan_after", p.orphan)
	if err := p.tickOnce(ctx); err != nil {
		p.log.Error("initial tick", "err", err)
	}
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			p.log.Info("poller stopping")
			return
		case <-t.C:
			if err := p.tickOnce(ctx); err != nil {
				p.log.Error("tick", "err", err)
			}
		}
	}
}

// tickOnce advances every active grab by one step.
//
// Step 1 — Enrich qbit_hash for grabs that don't yet have one. We
// match against the tick's shared torrent list by category +
// add-time window + title-token signature (pickRecent).
//
// Step 2 — Refresh qBit state for any tracked grab, looked up by hash
// in the same shared list. Status updates: downloading | completed |
// failed (when qBit no longer knows about it).
//
// Step 3 — For completed grabs without an actual_stashdb_id yet,
// query Stash by filename. If Stash has indexed the file and has a
// StashDB cross-id for it, set actual_stashdb_id and transition to
// confirmed (matches prediction) or mismatched (doesn't). If still
// not in Stash after `orphan_after`, mark orphaned.
func (p *Poller) tickOnce(ctx context.Context) error {
	t0 := time.Now()
	// Adopt any forager-category qBit torrents we aren't tracking yet,
	// before loading active grabs so a freshly-adopted one is processed
	// this same tick.
	p.adoptOrphans(ctx, adoptionGrace)
	active, err := p.repo.Active(ctx)
	if err != nil {
		return err
	}
	// Drop per-grab throttle state for grabs that left Active() (confirmed,
	// failed, deleted): lastScan and packScan otherwise grow for the
	// daemon's lifetime. forgetPackScan covers the pack-confirm path, but
	// not failures and deletes.
	activeIDs := make(map[int64]bool, len(active))
	for i := range active {
		activeIDs[active[i].ID] = true
	}
	p.scanMu.Lock()
	for id := range p.lastScan {
		if !activeIDs[id] {
			delete(p.lastScan, id)
		}
	}
	p.scanMu.Unlock()
	p.packMu.Lock()
	for id := range p.packScan {
		if !activeIDs[id] {
			delete(p.packScan, id)
		}
	}
	p.packMu.Unlock()
	if len(active) == 0 {
		return nil
	}

	// One full qBit torrent list per tick serves BOTH the info-hash
	// enrichment (Step 1) and every tracked grab's state refresh (Step 2).
	// advanceQbit used to call TorrentInfo per grab, and TorrentInfo
	// fetches + scans the whole list anyway, so N tracked grabs (orphaned
	// ones included, indefinitely) cost N full-list downloads every tick.
	//
	// qbitListOK mirrors sabListsOK below: a missing entry in qbitByHash
	// is advanceQbit's "qBit no longer tracks this torrent" failure
	// signal, so a failed fetch must skip the qBit refresh for the tick
	// rather than present an empty list that fails every live grab.
	needsQbit := false
	for _, g := range active {
		if g.Client == "qbit" {
			needsQbit = true
			break
		}
	}
	var qbitTorrents []qbit.Torrent
	qbitByHash := map[string]*qbit.Torrent{}
	qbitListOK := false
	if qb := p.pool.Qbit(); needsQbit && qb != nil {
		qbitTorrents, err = qb.ListTorrents(ctx, qbit.ListOpts{Filter: "all"})
		if err != nil {
			p.log.Warn("qbit list torrents", "err", err)
		} else {
			qbitListOK = true
			for i := range qbitTorrents {
				qbitByHash[strings.ToLower(qbitTorrents[i].Hash)] = &qbitTorrents[i]
			}
		}
	}

	// Pre-fetch SAB queue + history once per tick if we have any SAB
	// grabs to advance. Both endpoints are cheap; one request each
	// covers an unbounded number of active SAB grabs.
	//
	// sabListsOK records whether BOTH fetches succeeded. advanceSab reads
	// absence from these lists as "SAB lost the download" and fails the
	// grab, so a transient fetch error (SAB restarting, network blip) must
	// skip the SAB refresh for the tick rather than hand advanceSab empty
	// lists that misread every live grab as gone.
	var sabQueue, sabHistory []sabnzbd.Item
	sabListsOK := false
	hasSabActive := false
	for _, g := range active {
		if g.Client == "sabnzbd" {
			hasSabActive = true
			break
		}
	}
	if sb := p.pool.Sab(); hasSabActive && sb != nil {
		sabListsOK = true
		sabQueue, err = sb.Queue(ctx)
		if err != nil {
			p.log.Warn("sab queue fetch", "err", err)
			sabListsOK = false
		}
		// Fetch a deep history slice: SAB history mixes forager
		// downloads with everything else the user grabs, so a
		// forager item can be buried fast. Too small a window and a
		// legitimately-completed grab scrolls out before the poller
		// sees it land, leaving it stuck mid-pipeline.
		sabHistory, err = sb.History(ctx, 200, p.pool.Settings().SabCategory)
		if err != nil {
			p.log.Warn("sab history fetch", "err", err)
			sabListsOK = false
		}
	}

	// Track hashes already linked so we don't double-assign one qBit
	// torrent to multiple forager grabs in this tick. (SAB grabs have
	// their client_id set at insert time, so no collision risk there.)
	claimed := make(map[string]bool, len(active))
	for _, g := range active {
		if g.Client == "qbit" && g.ClientID != "" {
			claimed[g.ClientID] = true
		}
	}

	for i := range active {
		if err := p.advance(ctx, &active[i], qbitTorrents, qbitByHash, qbitListOK, claimed, sabQueue, sabHistory, sabListsOK); err != nil {
			p.log.Warn("advance grab", "id", active[i].ID, "err", err)
		}
	}

	p.log.Debug("tick done", "active", len(active), "elapsed", time.Since(t0))
	return nil
}

func (p *Poller) advance(ctx context.Context, g *grabs.Grab, qbitTorrents []qbit.Torrent, qbitByHash map[string]*qbit.Torrent, qbitListOK bool, claimed map[string]bool, sabQueue, sabHistory []sabnzbd.Item, sabListsOK bool) error {
	dirty := false
	// srcPath is the live full filesystem path the client reports for
	// this grab — qBit's ContentPath, SAB's history Path. Used by the
	// place step below. Empty when unknown (still queued, or client
	// no longer tracks it).
	var srcPath string

	// ── Steps 1 + 2 — client-specific enrichment + state refresh.
	switch g.Client {
	case "qbit":
		// Skip the client refresh when this tick's torrent-list fetch
		// failed (or qBit is unconfigured): a missing hash is the failure
		// signal, and an empty-because-unfetched list would fail every
		// live grab. The Stash-side steps below still run.
		if !qbitListOK {
			break
		}
		d, sp := p.advanceQbit(g, qbitTorrents, qbitByHash, claimed)
		dirty = dirty || d
		srcPath = sp
	case "sabnzbd":
		// Skip the client refresh when this tick's queue/history fetch
		// failed (or SAB is unconfigured): absence from the lists is
		// advanceSab's failure signal, and empty-because-unfetched lists
		// would fail every live grab. The Stash-side steps below still run.
		if !sabListsOK {
			break
		}
		d, sp, err := p.advanceSab(g, sabQueue, sabHistory)
		if err != nil {
			return err
		}
		dirty = dirty || d
		srcPath = sp
	default:
		// Unknown client kind — leave the grab alone, log once.
		p.log.Warn("unknown grab client", "id", g.ID, "client", g.Client)
	}

	// Re-derive state from data. Earlier versions of the qBit/SAB
	// advance steps could downgrade a placed grab back to "completed"
	// when the source client still reported the torrent as active. If
	// we see a placed_path on a grab whose status got downgraded, lift
	// it back to "placed" so Step 4 can continue confirming.
	if g.PlacedPath != "" && (g.Status == "completed" || g.Status == "downloading") {
		prev := g.Status
		g.Status = "placed"
		g.Reason = "heal: placed_path set, status was " + prev
		dirty = true
	}

	// ── Step 3 — place the finished download into the library.
	// Skipped when the placer isn't configured (libraryRoot unset) —
	// the file stays in the download client's complete dir and Stash
	// confirmation works against that location instead.
	pl := p.pool.Placer()
	if g.Status == "completed" && g.PlacedPath == "" && pl.Configured() && srcPath != "" &&
		p.grabStillExists(ctx, g.ID) {
		// grabStillExists is re-checked immediately before the side-effecting
		// place: a concurrent delete during the tick's earlier I/O would
		// otherwise have us hardlink a file into the library for a grab being
		// torn down, leaving an orphaned untracked copy. (Shrinks the race to
		// the Get→Place gap; the deleted grab's later CAS write is a no-op.)
		res, err := pl.Place(srcPath, g.PerformerName)
		if err != nil {
			// Don't flip status — stay in "completed" so we retry next
			// tick. Surface the error so the UI can show it.
			if g.PlaceError != err.Error() {
				g.PlaceError = err.Error()
				g.Reason = "place failed: " + err.Error()
				dirty = true
				p.log.Warn("place failed", "id", g.ID, "src", srcPath, "err", err)
			}
		} else {
			g.PlacedPath = res.Path
			g.PlacedAt = time.Now().Unix()
			g.PlaceError = ""
			g.Status = "placed"
			if res.Mode != "" {
				g.Reason = "placed via " + res.Mode
			} else {
				g.Reason = "place idempotent (already present)"
			}
			dirty = true
			p.log.Info("placed", "id", g.ID, "path", res.Path, "mode", res.Mode)

			// Trigger Stash to scan + generate phashes for the new file
			// immediately, so the placed → confirmed transition takes
			// minutes instead of waiting on Stash's scheduled scan.
			// Best-effort: if it fails, the passive confirmation path
			// still works on Stash's next tick.
			//
			// Stash's library paths can differ from forager-container
			// paths (e.g. forager sees /data/media/Media but Stash on
			// Windows sees Z:\Media). FORAGER_STASH_PATH_MAPPING lets
			// the user opt into scoped scans; otherwise we trigger a
			// full-library scan (slow per scan but always works since
			// Stash skips unchanged files cheaply).
			if sc := p.pool.Stash(); sc != nil {
				p.triggerPlacementScan(ctx, sc, g.ID, res.Path)
			}

			// Usenet doesn't seed, so once the file is in the library
			// the SAB-side copy is dead weight. When enabled, remove
			// it (history entry + downloaded files). Safe: placement
			// already linked/copied the data into the library, so the
			// SAB files can go without touching the library copy.
			// qBit grabs are never cleaned here — torrents keep
			// seeding. Best-effort; a failure just leaves the download
			// in SAB. After deletion SAB no longer tracks the nzo_id,
			// but the grab is "placed" now so advanceSab's not-found
			// branch correctly leaves it alone.
			if g.Client == "sabnzbd" && g.ClientID != "" && p.pool.Settings().SabDeleteAfterPlace {
				if sb := p.pool.Sab(); sb != nil {
					if err := sb.DeleteHistory(ctx, g.ClientID, true); err != nil {
						p.log.Warn("sab cleanup after place failed", "id", g.ID, "nzo_id", g.ClientID, "err", err)
					} else {
						p.log.Info("sab download removed after place", "id", g.ID, "nzo_id", g.ClientID)
					}
				}
			}
		}
	}

	// ── Step 4 — try to confirm against Stash once the file is in
	// place (or, if placement is disabled, once the client reports
	// completion). Stash needs to have scanned the file's location;
	// FindSceneByPathContains matches on the basename which is the
	// same for hardlinked + copied files. Skipped when Stash isn't
	// configured — we'll re-try once credentials are saved.
	//
	// scanned + orphaned grabs join confirmable so we keep
	// re-evaluating: scanned waits for Stash's identify to attach a
	// StashDB cross-id; orphaned can recover if the user (or a
	// scheduled task) later scans the file in.
	confirmable := g.Status == "placed" || g.Status == "scanned" || g.Status == "orphaned" || (g.Status == "completed" && !pl.Configured())
	// Orphaned is a non-terminal parking state (it recovers if the file is
	// later scanned in), but it accumulates: without a throttle every
	// orphan costs a Stash query EVERY tick, forever. Re-check on a long
	// interval instead; recovery then lands within orphanRecheckInterval
	// of the user's scan rather than within one tick. Stamped before the
	// query so an error doesn't bypass the throttle.
	if confirmable && g.Status == "orphaned" {
		if !p.orphanRecheckElapsed(g.ID) {
			confirmable = false
		} else {
			p.markScanAttempt(g.ID)
		}
	}
	stashC := p.pool.Stash()
	if g.Kind == "pack" {
		// Pack grabs have their own confirm path — enumerate every placed
		// file's scene, drive identify across all of them, dedup, then
		// confirm. Distinct from the single-scene match below.
		if confirmable && stashC != nil {
			d, err := p.advancePackConfirm(ctx, g, stashC)
			if err != nil {
				return err
			}
			dirty = dirty || d
		}
	} else if confirmable && g.ActualStashDBID == "" && stashC != nil {
		needle := g.ClientName
		if g.PlacedPath != "" {
			// Prefer the placed-path basename — that's what Stash will
			// have indexed if the library_root is in Stash's paths.
			needle = pathmap.Base(g.PlacedPath)
		}
		if needle != "" {
			scene, err := stashC.FindSceneByPathContains(ctx, needle)
			if err != nil {
				return err
			}
			switch {
			case scene != nil && scene.StashDBID != "":
				g.ActualStashDBID = scene.StashDBID
				g.ConfirmedAt = time.Now().Unix()
				switch {
				case g.PredictedStashDBID == "":
					// No prediction to compare against (e.g. a grab
					// adopted from the download client rather than a
					// forage search) — can't be a mismatch.
					g.Status = "confirmed"
					g.Reason = "stash phash → scene (no prediction)"
				case scene.StashDBID == g.PredictedStashDBID:
					g.Status = "confirmed"
					g.Reason = "stash phash → predicted scene"
				default:
					g.Status = "mismatched"
					g.Reason = "stash phash → different scene than predicted"
				}
				dirty = true
			case scene != nil:
				// File is in Stash but has no StashDB cross-id.
				if g.Status != "scanned" {
					// First sighting: mark scanned and fire one Identify
					// (best-effort enrichment — picks up a cross-id when
					// StashDB actually has the scene).
					g.Status = "scanned"
					g.Reason = "in Stash, awaiting identify"
					dirty = true
					if jobID, err := p.triggerIdentify(ctx, stashC, scene.ID); err != nil {
						p.log.Warn("metadataIdentify trigger failed", "id", g.ID, "scene_id", scene.ID, "err", err)
					} else if jobID != "" {
						p.log.Info("metadataIdentify triggered", "id", g.ID, "scene_id", scene.ID, "job_id", jobID)
					}
					p.markScanAttempt(g.ID)
				} else if g.PredictedStashDBID == "" {
					// No prediction to verify against — for a manual/pack/
					// adopted grab, being scanned into the library IS the
					// goal. Don't strand it waiting on a StashDB match that
					// may never exist (amateur content often isn't on
					// StashDB, or is identified via another scraper).
					g.Status = "confirmed"
					g.Reason = "in library (scanned)"
					g.ConfirmedAt = time.Now().Unix()
					dirty = true
				} else if g.CompletedAt > 0 && time.Since(time.Unix(g.CompletedAt, 0)) > p.orphan {
					// Predicted grab: gave up on the StashDB match.
					g.Status = "confirmed"
					g.Reason = "in library; no StashDB match"
					g.ConfirmedAt = time.Now().Unix()
					dirty = true
				} else if p.scanThrottleElapsed(g.ID) {
					// Predicted grab: retry Identify until the cross-id lands.
					if jobID, err := p.triggerIdentify(ctx, stashC, scene.ID); err != nil {
						p.log.Warn("metadataIdentify trigger failed", "id", g.ID, "scene_id", scene.ID, "err", err)
					} else if jobID != "" {
						p.log.Info("metadataIdentify triggered", "id", g.ID, "scene_id", scene.ID, "job_id", jobID)
					}
					p.markScanAttempt(g.ID)
				}
			case g.CompletedAt > 0 && time.Since(time.Unix(g.CompletedAt, 0)) > p.orphan:
				if g.Status != "orphaned" {
					g.Status = "orphaned"
					g.Reason = "placed but Stash never picked up the file"
					dirty = true
				}
			default:
				// scene == nil and not past the orphan window. The file
				// is on disk (we placed it) but Stash hasn't indexed it
				// — the initial post-placement scan can be coalesced
				// with a concurrent one and miss it. Re-fire the scan,
				// throttled, until Stash picks it up. Without this the
				// grab sits at "placed" until it wrongly orphans, despite
				// the file being right there on disk.
				if g.PlacedPath != "" && p.scanThrottleElapsed(g.ID) {
					p.triggerPlacementScan(ctx, stashC, g.ID, g.PlacedPath)
				}
			}
		}
	}

	if dirty {
		// The tick loaded this grab via Active() at the top of tickOnce, then
		// did seconds of network I/O. If an API mutation (reassign, match,
		// retry, delete) committed in that window, our snapshot is stale —
		// the CAS in Update rejects it. Treat that as benign: drop this
		// tick's write so we don't revert the user's change; the next tick
		// re-loads the fresh row and re-derives state.
		if err := p.repo.Update(ctx, *g); err != nil {
			if errors.Is(err, grabs.ErrStaleUpdate) {
				p.log.Info("grab changed under tick; skipping stale write", "id", g.ID)
				return nil
			}
			return err
		}
	}
	return nil
}

// advancePackConfirm drives a pack grab from placed → confirmed. Unlike
// a single grab (one file → one scene), a pack is a directory of many
// of a performer's scenes, so confirmation means: enumerate every scene
// Stash has indexed under the placed pack dir, drive Identify across the
// ones still missing a StashDB cross-id, and confirm once they're all
// identified (or the orphan window elapses — some files may simply not
// be on StashDB). Dedup against the existing library happens here too
// (added in the dedup phase).
//
// Throttling reuses the per-grab scan throttle: each tick fires at most
// one Stash-side task (a directory rescan while nothing's indexed yet,
// then a batched identify once scenes appear).
func (p *Poller) advancePackConfirm(ctx context.Context, g *grabs.Grab, sc *stash.Client) (bool, error) {
	if g.PlacedPath == "" {
		return false, nil
	}
	// Identify pack scenes by their full placed-directory path, not just
	// its basename — the pack often lands at <performer>/<pack-name>
	// where <pack-name> can equal the performer (e.g.
	// /Media/comatozze/comatozze). The bare basename would then also
	// match the performer's pre-existing scenes under /Media/comatozze,
	// corrupting both the count and the dedup decision. Translate to the
	// Stash-side path so the substring is specific; fall back to the
	// basename only when no path mapping is configured.
	needle := pathmap.Translate(g.PlacedPath, p.pool.Settings().StashPathMapping)
	if needle == "" {
		needle = pathmap.Base(g.PlacedPath)
	}
	scenes, err := sc.FindScenesUnderPath(ctx, needle)
	if err != nil {
		return false, err
	}
	dirty := false
	found := len(scenes)
	identified := 0
	var toIdentify []string
	for _, s := range scenes {
		if s.StashDBID != "" {
			identified++
		} else {
			toIdentify = append(toIdentify, s.ID)
		}
	}
	if g.PackIdentified != identified {
		g.PackIdentified = identified
		dirty = true
	}
	// Nothing indexed yet — the post-placement scan can miss a large new
	// directory. Re-fire it, throttled, until scenes appear.
	if found == 0 {
		if (g.Status == "placed" || g.Status == "completed") && p.scanThrottleElapsed(g.ID) {
			// Surface a persistent miss: if this keeps logging after the pack
			// has landed + Stash scanned, `needle` doesn't match how Stash
			// indexed the files (a path-mapping issue) — and identify never
			// fires because we return here before it.
			p.log.Info("pack: no scenes indexed under path yet, re-scanning",
				"id", g.ID, "needle", needle, "placed", g.PlacedPath)
			p.triggerPlacementScan(ctx, sc, g.ID, g.PlacedPath)
		}
		return dirty, nil
	}

	// Stash has at least part of the pack — leave the pre-scan states
	// (placed, completed-with-placement-disabled, or a recovered orphan).
	if g.Status != "scanned" {
		g.Status = "scanned"
		dirty = true
	}

	// Drive identify across everything still missing a cross-id.
	if len(toIdentify) > 0 && p.scanThrottleElapsed(g.ID) {
		if jobID, err := p.triggerIdentifyBatch(ctx, sc, toIdentify); err != nil {
			p.log.Warn("pack identify trigger failed", "id", g.ID, "scenes", len(toIdentify), "err", err)
		} else if jobID != "" {
			p.log.Info("pack identify triggered", "id", g.ID, "scenes", len(toIdentify), "job_id", jobID)
		}
		p.markScanAttempt(g.ID)
	}

	// Completion is gated on the scan having SETTLED — the indexed-scene
	// count stopped climbing — NOT on hitting a guessed file total. A
	// half-scanned directory must never confirm: it would dedup against an
	// incomplete set and strand the rest. Once settled, confirm when either
	// every indexed scene is identified, or Identify has had a fair chance
	// (pack scenes legitimately may not be on StashDB, and title counts can
	// overstate the real file count — neither should hold the pack at
	// "scanned" for the full orphan window). A long orphan backstop covers
	// a missing download timestamp.
	settled := p.packScanSettled(g.ID, found)
	downloadDone := g.CompletedAt > 0
	var sinceDone time.Duration
	if downloadDone {
		sinceDone = time.Since(time.Unix(g.CompletedAt, 0))
	}
	identifyDone := len(toIdentify) == 0
	// Floor against confirming a half-scanned pack. The settle window
	// (packScanSettled) is the primary "scan stopped growing" signal, but
	// it lives in an in-memory map that's lost on restart — so a daemon
	// restart mid-scan re-seeds the high-water at whatever's indexed right
	// now and, if Stash's scan is genuinely stalled there (backlog, or
	// Stash itself restarted), the window elapses and the pack confirms +
	// dedups against a partial set. When we know the expected file count
	// (PackFiles, persisted at grab/adopt time), additionally require most
	// of it to be indexed before a settle can complete. The gaveUp orphan
	// backstop below still forces eventual confirmation if some files
	// legitimately never scan, so this can't hang a pack forever.
	ready := settled && packScanCoverageOK(found, g.PackFiles) && (identifyDone || !downloadDone || sinceDone > packIdentifyGrace)
	gaveUp := downloadDone && sinceDone > p.orphan
	if g.Status == "scanned" && (ready || gaveUp) {
		// Download-then-dedup: reconcile pack scenes whose StashDB id the
		// library already had outside this pack. Which copy survives is
		// configurable (PackDedupKeep): keep existing (default, removing
		// the pack's), keep pack (removing the existing), keep both (no
		// dedup), or review (record collisions for the user to resolve and
		// destroy nothing now). Best-effort — a failure still lets the pack
		// confirm.
		keep := p.pool.Settings().PackDedupKeep
		pendingReview := 0
		if keep != "both" {
			// coverageVerified gates the only irreversible AUTOMATIC dedup
			// branch (keep="pack", which deletes pre-existing library copies).
			// A magnet/manual pack has PackFiles==0, for which
			// packScanCoverageOK returns true vacuously — so we must treat
			// an unknown expected count as UNverified, not as "100% covered".
			// This is what stops the orphan-backstop / post-restart partial
			// confirm from destroying originals against an incomplete set.
			coverageVerified := g.PackFiles > 0 && packScanCoverageOK(found, g.PackFiles)
			endpoint, _ := p.identifyEndpoint(ctx, sc)
			if deduped, recorded, err := p.dedupPack(ctx, sc, g, scenes, endpoint, keep, coverageVerified); err != nil {
				p.log.Warn("pack dedup failed", "id", g.ID, "err", err)
			} else {
				if deduped > 0 {
					g.PackDeduped += deduped
				}
				pendingReview = recorded
			}
		}
		// Record the final scene total for the UI when we never had a real
		// count (manual/magnet packs). The scan has settled, so this is the
		// true total — not the mid-scan partial the old grab-time backfill
		// could pin.
		if g.PackFiles == 0 {
			g.PackFiles = found
		}
		g.Status = "confirmed"
		g.ConfirmedAt = time.Now().Unix()
		g.Reason = fmt.Sprintf("pack: %d/%d scenes identified, %d dup removed", identified, found, g.PackDeduped)
		if pendingReview > 0 {
			g.Reason += fmt.Sprintf(", %d to review", pendingReview)
		}
		dirty = true
		p.forgetPackScan(g.ID)
		p.log.Info("pack confirmed", "id", g.ID, "identified", identified, "found", found, "deduped", g.PackDeduped, "pendingReview", pendingReview)
	}
	return dirty, nil
}

// dedupPack reconciles pack scenes against copies the library already had
// outside the pack. For each identified pack scene it finds other scenes
// carrying the same StashDB cross-id; when any exist, `keep` decides which
// side survives:
//
//   - "existing" (default): destroy the pack's copy, keep the original.
//   - "pack": destroy the existing copies, keep the pack's.
//
// Pack membership is the SET of scene ids Stash indexed under the pack
// directory (packScenes) — NOT a path-substring test. The old
// strings.Contains(path, needle) test over-matched any sibling directory
// sharing the pack's basename (common when no path mapping is configured,
// e.g. a "comatozze" pack vs the performer's existing "comatozze" folder),
// silently mis-classifying copies and, with keep="pack", risking a wrong
// SceneDestroy(deleteFile=true). A scene is an "external" copy iff it
// carries a pack scene's cross-id but is NOT itself in the pack set.
//
// Copies are located per-cross-id via FindSceneRefsByStashID (cheap
// regardless of library size); when no stash-box endpoint is available to
// query by, a one-shot whole-library sweep backs it instead. Destroyed
// scenes are tracked so a cross-id shared by several pack files isn't
// destroyed twice. Destroy removes the Stash scene + its file; the torrent
// keeps seeding from the download client's own (separate) hardlink.
// Returns the count removed.
// Returns (destroyed, recorded): destroyed counts copies actually removed
// (keep="existing"/"pack"); recorded counts pending review items written
// (keep="review", which destroys nothing now and defers the decision to the
// user). Exactly one is non-zero for a given mode.
func (p *Poller) dedupPack(ctx context.Context, sc *stash.Client, g *grabs.Grab, packScenes []stash.SceneMatch, endpoint, keep string, coverageVerified bool) (int, int, error) {
	// keep="pack" is the only AUTOMATIC mode that deletes copies that were
	// already in the library (the "external" copies below). That deletion is
	// irreversible (SceneDestroy(deleteFile=true)), so we refuse it whenever
	// the pack scan isn't verifiably complete — a magnet/manual pack
	// (PackFiles==0), a partial scan that confirmed via the orphan backstop,
	// or a daemon restart that reset the in-memory settle high-water. In
	// those cases the pack set is not trustworthy as "everything that
	// landed," and acting on it could strand the user without an original.
	// Skipping leaves the pack's own copy in place and destroys nothing; the
	// duplicate simply goes unreconciled. keep="existing" is always safe — it
	// only ever drops the pack's freshly-downloaded copy — and keep="review"
	// destroys nothing here (it records pending items), so neither is gated.
	if keep == "pack" && !coverageVerified {
		p.log.Warn("pack dedup: keep=pack skipped — scan coverage unverified, originals preserved",
			"id", g.ID, "packFiles", g.PackFiles, "scanned", len(packScenes))
		return 0, 0, nil
	}

	packIDs := make(map[string]bool, len(packScenes))
	for _, ps := range packScenes {
		packIDs[ps.ID] = true
	}

	// Whole-library fallback only when we have no endpoint to query by.
	var sweep map[string][]stash.SceneRef
	if endpoint == "" {
		var err error
		if sweep, err = sc.FindAllSceneStashDBIDs(ctx); err != nil {
			return 0, 0, err
		}
	}
	cache := map[string][]stash.SceneRef{}
	copiesOf := func(stashID string) []stash.SceneRef {
		if sweep != nil {
			return sweep[stashID]
		}
		if refs, ok := cache[stashID]; ok {
			return refs
		}
		refs, err := sc.FindSceneRefsByStashID(ctx, endpoint, stashID)
		if err != nil {
			p.log.Warn("pack dedup lookup", "id", g.ID, "stashdb", stashID, "err", err)
			refs = nil
		}
		cache[stashID] = refs
		return refs
	}

	deduped := 0
	recorded := 0
	destroyed := map[string]bool{}
	destroy := func(sceneID string) {
		if sceneID == "" || destroyed[sceneID] {
			return
		}
		if err := sc.SceneDestroy(ctx, sceneID, true, true); err != nil {
			p.log.Warn("pack dedup destroy", "id", g.ID, "scene", sceneID, "err", err)
			return
		}
		destroyed[sceneID] = true
		deduped++
		p.log.Info("pack dedup removed duplicate", "id", g.ID, "scene", sceneID, "keep", keep)
	}
	for _, ps := range packScenes {
		if ps.StashDBID == "" {
			continue
		}
		refs := copiesOf(ps.StashDBID)
		var externalIDs []string
		for _, ref := range refs {
			if !packIDs[ref.SceneID] {
				externalIDs = append(externalIDs, ref.SceneID)
			}
		}
		if len(externalIDs) == 0 {
			continue // unique to this pack — keep it
		}
		switch keep {
		case "review":
			// Destroy nothing now — record the collision for the user to
			// resolve per scene. Idempotent: re-ticks refresh a still-pending
			// row in place.
			if p.recordReviewDuplicate(ctx, g, ps, refs, packIDs) {
				recorded++
			}
		case "pack":
			for _, eid := range externalIDs {
				destroy(eid) // keep the pack copy, drop the originals
			}
		default:
			destroy(ps.ID) // "existing": keep the original, drop the pack copy
		}
	}
	return deduped, recorded, nil
}

// recordReviewDuplicate writes (or refreshes) a pending review item for one
// pack scene that collides with copies already in the library. refs is every
// local copy of the scene's cross-id; packIDs marks which of those belong to
// this pack. Returns false (and logs) on a write error. Skips writing if the
// pack copy can't be located among refs AND we have no fallback identity —
// without a pack-side scene id there's nothing to act on later.
func (p *Poller) recordReviewDuplicate(ctx context.Context, g *grabs.Grab, ps stash.SceneMatch, refs []stash.SceneRef, packIDs map[string]bool) bool {
	var pack grabs.SceneCopy
	var existing []grabs.SceneCopy
	for _, ref := range refs {
		c := grabs.SceneCopy{SceneID: ref.SceneID, Title: ref.Title, Path: ref.Path, Size: ref.Size, Height: ref.Height}
		if packIDs[ref.SceneID] {
			// Representative pack copy: prefer the scene the pack confirm
			// identified (ps.ID); otherwise the largest, as a stable pick.
			if pack.SceneID == "" || ref.SceneID == ps.ID || ref.Size > pack.Size {
				pack = c
			}
		} else {
			existing = append(existing, c)
		}
	}
	if pack.SceneID == "" {
		pack = grabs.SceneCopy{SceneID: ps.ID, Title: ps.Title, Path: ps.FilePath}
	}
	if pack.SceneID == "" {
		return false
	}
	title := pack.Title
	if title == "" {
		if ps.Title != "" {
			title = ps.Title
		} else if len(existing) > 0 {
			title = existing[0].Title
		}
	}
	d := grabs.PackDuplicate{
		GrabID:     g.ID,
		StashDBID:  ps.StashDBID,
		SceneTitle: title,
		Pack:       pack,
		Existing:   existing,
	}
	if err := p.repo.UpsertDuplicate(ctx, d); err != nil {
		p.log.Warn("pack dedup: record review item", "id", g.ID, "scene", ps.StashDBID, "err", err)
		return false
	}
	return true
}

// packScanSettled reports whether Stash has stopped indexing new scenes
// under a pack's directory — i.e. the count `found` has held steady for
// packScanStableWindow. A still-growing count means the scan is in
// progress, so the pack isn't ready to confirm/dedup. Updates the
// high-water record as a side effect.
func (p *Poller) packScanSettled(grabID int64, found int) bool {
	p.packMu.Lock()
	defer p.packMu.Unlock()
	st, ok := p.packScan[grabID]
	if !ok || found > st.count {
		p.packScan[grabID] = packScanState{count: found, since: time.Now()}
		return false
	}
	return time.Since(st.since) >= packScanStableWindow
}

// packScanCoverageOK reports whether enough of a pack's expected video
// count (expected) has been indexed (found) to permit confirmation.
// Returns true when the expected count is unknown (0) — there's nothing
// to floor against, so the settle window alone governs. Otherwise
// requires found to reach packIndexedFloorPct of expected.
func packScanCoverageOK(found, expected int) bool {
	if expected <= 0 {
		return true
	}
	return found*100 >= expected*packIndexedFloorPct
}

// forgetPackScan drops the in-memory scan record once a pack confirms.
func (p *Poller) forgetPackScan(grabID int64) {
	p.packMu.Lock()
	delete(p.packScan, grabID)
	p.packMu.Unlock()
}

// triggerIdentifyBatch fires Stash's Identify task over a set of scene
// IDs at once, sourcing from the user's StashDB stash-box. Returns
// ("", nil) when no stash-box is configured (caller logs + moves on).
func (p *Poller) triggerIdentifyBatch(ctx context.Context, sc *stash.Client, sceneIDs []string) (string, error) {
	endpoint, err := p.identifyEndpoint(ctx, sc)
	if err != nil {
		return "", err
	}
	if endpoint == "" {
		return "", nil
	}
	return sc.MetadataIdentify(ctx, sceneIDs, endpoint)
}

// markScanAttempt records a throttle timestamp for the grab so the next
// scan/identify retry waits scanRetryInterval. triggerPlacementScan
// records its own; this is for the identify path which doesn't.
func (p *Poller) markScanAttempt(grabID int64) {
	p.scanMu.Lock()
	p.lastScan[grabID] = time.Now()
	p.scanMu.Unlock()
}

// advanceQbit handles the qBit-specific enrichment + state-refresh
// steps, working entirely off the tick's shared torrent list (ts, plus
// its byHash index) — no network calls of its own. Returns (dirty,
// contentPath). contentPath is qBit's full filesystem path for the
// torrent — passed to the placer when status flips to "completed".
func (p *Poller) advanceQbit(g *grabs.Grab, ts []qbit.Torrent, byHash map[string]*qbit.Torrent, claimed map[string]bool) (bool, string) {
	dirty := false
	// Link the info_hash if we don't have it yet (qBit doesn't return
	// it from /torrents/add).
	if g.ClientID == "" {
		if t := pickRecent(ts, g, claimed); t != nil {
			g.ClientID = t.Hash
			g.ClientName = t.Name
			g.Reason = "enriched from qBit recent-additions"
			claimed[t.Hash] = true
			dirty = true
		}
	}
	if g.ClientID == "" {
		// Never got linked to a qBit torrent. With .torrent grabs now
		// pinned to their info-hash at add time (and magnets to their
		// btih), an unlinked grab past this window means the add itself
		// never landed — don't leave it queued forever.
		if g.Status == "queued" && g.GrabbedAt > 0 &&
			time.Since(time.Unix(g.GrabbedAt, 0)) > qbitLinkTimeout {
			g.Status = "failed"
			g.Reason = "never linked to a qBit torrent (add likely failed)"
			return true, ""
		}
		return dirty, ""
	}
	t := byHash[strings.ToLower(g.ClientID)]
	if t == nil {
		// A still-queued grab pins its info-hash at insert/retry time, but
		// the actual qBit add runs asynchronously behind the fetch gate —
		// the hash legitimately isn't visible in qBit yet. Give it the same
		// window an unlinked grab gets before declaring the add dead;
		// failing here instantly races the in-flight add, and a torrent
		// that lands after the fail is stranded (failed grabs leave
		// Active() and KnownClientIDs blocks adoption). A grab past
		// "queued" was tracked by qBit before, so a missing hash there
		// really does mean the torrent was removed.
		if g.Status == "queued" && g.GrabbedAt > 0 &&
			time.Since(time.Unix(g.GrabbedAt, 0)) <= qbitLinkTimeout {
			return dirty, ""
		}
		if g.Status != "failed" {
			g.Status = "failed"
			g.Reason = "qbit no longer tracks this torrent"
			dirty = true
		}
		return dirty, ""
	}
	if t.Name != "" && t.Name != g.ClientName {
		g.ClientName = t.Name
		dirty = true
	}
	// Track download progress so the API can flag a stalled grab (no
	// progress for a while). Stamp the time only when progress actually
	// advances; a download stuck at the same fraction keeps its old
	// progress_at, which is what the stalled check measures against.
	if t.Progress > g.Progress {
		g.Progress = t.Progress
		g.ProgressAt = time.Now().Unix()
		dirty = true
	}

	// Self-heal a PREMATURE placement: a placed_path set while qBit still
	// reports the torrent incomplete (progress < 1). This is the bogus
	// state a mid-download reassign used to create — a partial file
	// hardlinked into the library, then wrongly promoted (placed → scanned
	// → …). qBit's progress is the authority here, NOT forage's status
	// (which may already have been promoted past "downloading"). Undo it:
	// remove the partial library copy, clear placement, reset to
	// downloading so the place step re-files cleanly on real completion.
	// The seeding source is untouched.
	//
	// Two guards keep the heal from deleting a LEGITIMATE library copy:
	//   - progress < 1 is only meaningful in genuine download states.
	//     During a force recheck (routine after an unclean qBit restart)
	//     progress is the verification fraction, and during a move it can
	//     dip — a tick landing mid-recheck would otherwise RemoveAll a
	//     fully-placed file or pack directory.
	//   - a placement stamped at/after a recorded completion was made from
	//     a finished download and is never premature; completed_at is
	//     COALESCE-persisted so a later recheck regressing progress can't
	//     re-arm the heal against it. Premature placements have
	//     CompletedAt == 0 (the download was never seen complete).
	if g.PlacedPath != "" && t.Progress < 1 &&
		!qbitProgressUnreliable(t.State) &&
		(g.CompletedAt == 0 || (g.PlacedAt > 0 && g.PlacedAt < g.CompletedAt)) {
		bad := g.PlacedPath
		if rerr := os.RemoveAll(bad); rerr != nil {
			p.log.Warn("heal: remove premature placement", "id", g.ID, "path", bad, "err", rerr)
		}
		g.PlacedPath = ""
		g.PlacedAt = 0
		g.PlaceError = ""
		g.Status = "downloading"
		g.Reason = "heal: cleared premature placement (download still in progress)"
		dirty = true
		p.log.Info("heal: cleared premature placement",
			"id", g.ID, "path", bad, "progress", t.Progress)
		return dirty, t.ContentPath
	}

	newStatus := classifyQbitState(t.State)
	// Progress is the authority for completion, not the state name. qBit
	// reports transient mid-download states — v5's "stoppedDL" (paused),
	// "checkingResumeData", "moving" — that classifyQbitState's default would
	// otherwise read as "completed", triggering a premature placement + Stash
	// scan of a half-downloaded pack. A torrent that isn't 100% is still
	// downloading no matter what its state string says. (The self-heal below
	// undoes a placement that slipped through; this stops it at the source so
	// no premature scan fires in the first place.)
	if newStatus == "completed" && t.Progress < 1 {
		newStatus = "downloading"
	}
	// Don't downgrade post-completed states (placed/scanned/etc.)
	// back to "completed" just because qBit still reports the torrent
	// as seeding (the most common case). qBit's view is limited to
	// download progress — anything we've learned downstream about
	// placement or Stash status is more authoritative.
	if !isPostCompleted(g.Status) && g.Status != newStatus && newStatus != "" {
		g.Status = newStatus
		if newStatus == "completed" && g.CompletedAt == 0 {
			g.CompletedAt = time.Now().Unix()
		}
		g.Reason = "qbit state=" + t.State
		dirty = true
	}
	return dirty, t.ContentPath
}

// sabRegisterGrace is how long after grabbing we tolerate a SAB
// nzo_id being absent from both queue and history before declaring
// the grab failed. SAB returns the nzo_id synchronously on add but
// takes a few seconds to surface the job in its queue, and the
// poller can tick within that window. Generous on purpose: a stuck
// "queued" grab is far less harmful than a false "failed" on a
// download that's actually in flight.
const sabRegisterGrace = 5 * time.Minute

// qbitLinkTimeout is how long a qBit grab may sit "queued" without ever
// linking to a torrent before it's declared failed. Well beyond the 2-min
// async-add budget, so a slow add still resolves first; a grab still
// unlinked past it means the add never landed.
const qbitLinkTimeout = 10 * time.Minute

// adoptionGrace delays adopting a freshly-added qBit torrent, so a
// torrent added through the forage UI gets linked to its existing grab
// (via pickRecent's ±120s window) before adoption could create a
// duplicate row for the same hash.
const adoptionGrace = 5 * time.Minute

// adoptMinVideos mirrors api.packMinVideos: at/above this video count an
// adopted torrent is treated as a pack. Keep in sync with that const.
const adoptMinVideos = 3

// manualAdoptGrace is the (short) minimum age the "scan for new downloads"
// button honours — far below the periodic adoptionGrace, but non-zero so it
// can't adopt one of forage's OWN just-added torrents (a fresh grab or a
// retry) before that grab has linked its info-hash, which would create a
// duplicate adopted row.
const manualAdoptGrace = 90 * time.Second

// AdoptNow force-adopts untracked forage-category torrents for the Grabs
// "scan for new downloads" button — much sooner than the 5-minute periodic
// grace, but still skipping torrents added in the last 90s so it can't race
// forage's own in-flight adds. Returns how many were adopted. Safe alongside
// the periodic tick: adoptOrphans serialises on adoptMu and the known-id
// check prevents double-adoption.
func (p *Poller) AdoptNow(ctx context.Context) int {
	return p.adoptOrphans(ctx, manualAdoptGrace)
}

// adoptOrphans creates grab rows for torrents the user added directly to
// qBit under the configured forage category that forage isn't tracking
// yet, so they flow through the normal place → scan → identify → dedup
// pipeline exactly like a UI add. Scoped strictly to that category;
// other categories (the *arr stack, ad-hoc qBit use) are never touched.
// qBit *tags* are ignored — only the category matches. minAge skips
// torrents added more recently than it (the periodic tick passes
// adoptionGrace; a manual force-adopt passes 0). Returns the count adopted.
func (p *Poller) adoptOrphans(ctx context.Context, minAge time.Duration) int {
	p.adoptMu.Lock()
	defer p.adoptMu.Unlock()
	qb := p.pool.Qbit()
	if qb == nil {
		return 0
	}
	cat := p.pool.Settings().QbitCategory
	if cat == "" {
		return 0 // never adopt uncategorised torrents
	}
	ts, err := qb.ListTorrents(ctx, qbit.ListOpts{Category: cat, Filter: "all"})
	if err != nil {
		p.log.Warn("adopt: list torrents", "err", err)
		return 0
	}
	if len(ts) == 0 {
		return 0
	}
	known, err := p.repo.KnownClientIDs(ctx)
	if err != nil {
		p.log.Warn("adopt: known client ids", "err", err)
		return 0
	}
	adopted := 0
	now := time.Now().Unix()
	graceSecs := int64(minAge / time.Second)
	for i := range ts {
		t := &ts[i]
		if t.Hash == "" || known[t.Hash] {
			continue
		}
		// Give a UI-added torrent time to claim its own grab first.
		if now-t.AddedOn < graceSecs {
			continue
		}
		kind, videos := p.classifyTorrent(ctx, qb, t.Hash)
		packFiles := 0
		if kind == "pack" {
			packFiles = videos
		}
		// Confidence-gated: only auto-file under a performer when the match
		// is a full, unambiguous multi-word name. A weak guess (lone first
		// name, or two performers both fitting) returns "" and the grab
		// lands in Unsorted — far easier to fix than a confidently wrong
		// folder, which is what mis-filed a batch of adopted torrents.
		folder := suggest.ConfidentTopFolder(ctx, p.db, t.Name)
		id, err := p.repo.Insert(ctx, grabs.Grab{
			ReleaseTitle:  t.Name,
			Client:        "qbit",
			ClientID:      t.Hash,
			ClientName:    t.Name,
			Category:      cat,
			Status:        "queued",
			PerformerName: folder,
			Kind:          kind,
			PackFiles:     packFiles,
			GrabbedAt:     t.AddedOn,
			Reason:        "adopted from qbit",
		})
		if err != nil {
			p.log.Warn("adopt: insert", "hash", t.Hash, "name", t.Name, "err", err)
			continue
		}
		adopted++
		p.log.Info("adopted qbit torrent", "id", id, "name", t.Name,
			"hash", t.Hash, "kind", kind, "videos", videos, "folder", folder)
	}
	return adopted
}

// classifyTorrent counts a torrent's video files via qBit's metainfo file
// list (available regardless of download progress) to decide pack vs
// single. Defaults to "single" when the file list isn't available yet.
func (p *Poller) classifyTorrent(ctx context.Context, qb *qbit.Client, hash string) (string, int) {
	files, err := qb.TorrentFiles(ctx, hash)
	if err != nil || len(files) == 0 {
		return "single", 0
	}
	videos := 0
	for _, f := range files {
		if torrentmeta.IsVideo(f.Name) {
			videos++
		}
	}
	if videos >= adoptMinVideos {
		return "pack", videos
	}
	return "single", videos
}

// advanceSab handles SAB tracking. SAB grabs already have client_id
// set at /grab time (mode=addurl returns the nzo_id synchronously),
// so there's no enrichment step.
//
// Lookup precedence: queue first (active downloads), then history
// (completed/failed). If found in neither it means SAB doesn't know
// about it — either user removed it or it never made it in, mark
// failed.
func (p *Poller) advanceSab(g *grabs.Grab, queue, history []sabnzbd.Item) (bool, string, error) {
	dirty := false
	if g.ClientID == "" {
		// No nzo_id to look up — shouldn't normally happen. Skip.
		return false, "", nil
	}
	if item := findByNzo(queue, g.ClientID); item != nil {
		if item.Name != "" && item.Name != g.ClientName {
			g.ClientName = item.Name
			dirty = true
		}
		if g.Status != "downloading" {
			g.Status = "downloading"
			g.Reason = "sab status=" + item.Status
			dirty = true
		}
		return dirty, "", nil
	}
	if item := findByNzo(history, g.ClientID); item != nil {
		// Prefer the final on-disk path's basename when available —
		// that's what Stash will see during a scan.
		name := item.Name
		if item.Path != "" {
			name = pathmap.Base(item.Path)
		}
		if name != "" && name != g.ClientName {
			g.ClientName = name
			dirty = true
		}
		switch item.Status {
		case "Completed":
			if g.Status != "placed" && g.Status != "completed" {
				g.Status = "completed"
				if g.CompletedAt == 0 {
					g.CompletedAt = time.Now().Unix()
				}
				g.Reason = "sab status=Completed"
				dirty = true
			} else if g.Status == "completed" && g.CompletedAt == 0 {
				g.CompletedAt = time.Now().Unix()
				dirty = true
			}
		case "Failed":
			if g.Status != "failed" {
				g.Status = "failed"
				g.Reason = "sab status=Failed"
				dirty = true
			}
		default:
			// Verifying / Repairing / Extracting / etc. — still
			// post-processing, treat as in-progress.
			if g.Status != "downloading" {
				g.Status = "downloading"
				g.Reason = "sab status=" + item.Status
				dirty = true
			}
		}
		return dirty, item.Path, nil
	}
	// Not in queue, not in history. Don't undo a later-stage status
	// (history rolls over on busy SABs; a grab we already advanced
	// shouldn't regress). For grabs still early in the pipeline there
	// are two sub-cases:
	//
	//   - Just grabbed: SAB accepted the nzo_id (it returned it on
	//     add) but hasn't surfaced the job in its queue yet — there's
	//     a few-second lag between add and the job appearing. Marking
	//     failed here is the bug that killed the Comatozze grab: the
	//     poller ran 1s after submit, didn't see the nzo_id, and gave
	//     up — then SAB completed the download 20s later. Within the
	//     registration grace we leave the grab alone.
	//
	//   - Long absent: past the grace window and still unknown to SAB
	//     means it genuinely failed to add or was removed. Mark failed.
	if g.Status == "completed" && g.PlacedPath == "" && p.pool.Placer().Configured() {
		// Completed but never placed, and the history entry is gone. That
		// entry's Path is the ONLY source of the download's on-disk
		// location (srcPath), so placement can never succeed now: the grab
		// would sit at "completed" forever, with any place_error frozen in
		// the UI. SAB purging history mid-pipeline (user cleanup, or
		// history rotation past the fetch window) is unrecoverable here,
		// so after the same registration grace (against a queue->history
		// flap during post-processing) fail it; a retry re-downloads
		// cleanly. With placement disabled this doesn't apply: the Stash
		// confirm path matches on ClientName and needs no path.
		ref := g.CompletedAt
		if ref == 0 {
			ref = g.GrabbedAt
		}
		if ref == 0 || time.Since(time.Unix(ref, 0)) > sabRegisterGrace {
			g.Status = "failed"
			g.Reason = "sab history entry removed before placement could finish; retry to re-download"
			dirty = true
		}
		return dirty, "", nil
	}
	if g.Status != "completed" && g.Status != "placed" && g.Status != "scanned" && g.Status != "confirmed" && g.Status != "mismatched" && g.Status != "orphaned" && g.Status != "failed" {
		if g.GrabbedAt > 0 && time.Since(time.Unix(g.GrabbedAt, 0)) < sabRegisterGrace {
			return dirty, "", nil
		}
		g.Status = "failed"
		g.Reason = "sab no longer tracks this nzo_id"
		dirty = true
	}
	return dirty, "", nil
}

// grabStillExists reports whether the grab row is still present — checked
// right before placement so a delete that purged it mid-tick stops us
// creating an orphaned library file. A read error is treated as "exists" (we
// don't want a transient DB hiccup to skip a legitimate placement).
func (p *Poller) grabStillExists(ctx context.Context, id int64) bool {
	g, err := p.repo.Get(ctx, id)
	if err != nil {
		return true
	}
	return g != nil
}

func findByNzo(items []sabnzbd.Item, nzoID string) *sabnzbd.Item {
	for i := range items {
		if items[i].NzoID == nzoID {
			return &items[i]
		}
	}
	return nil
}

// triggerPlacementScan asks Stash to scan the directory the placed
// file lives in, so the placed → confirmed transition takes minutes
// rather than waiting on Stash's scheduled scan. Records the attempt
// time so the confirmation step can throttle retries. Best-effort.
//
// The scan is scoped to the placed file's parent via
// FORAGER_STASH_PATH_MAPPING when set; otherwise paths is empty and
// Stash does a full-library scan (slow but always correct since it
// skips unchanged files).
func (p *Poller) triggerPlacementScan(ctx context.Context, sc *stash.Client, grabID int64, placedPath string) {
	stashSidePath := pathmap.Translate(filepath.Dir(placedPath), p.pool.Settings().StashPathMapping)
	var scanPaths []string
	if stashSidePath != "" {
		scanPaths = []string{stashSidePath}
	}
	p.scanMu.Lock()
	p.lastScan[grabID] = time.Now()
	p.scanMu.Unlock()
	if jobID, err := sc.MetadataScan(ctx, scanPaths); err != nil {
		p.log.Warn("metadataScan trigger failed", "id", grabID, "paths", scanPaths, "err", err)
	} else {
		p.log.Info("metadataScan triggered", "id", grabID, "paths", scanPaths, "job_id", jobID)
	}
}

// scanThrottleElapsed reports whether enough time has passed since the
// last scan attempt for this grab to retry. Read-only; triggering the
// scan (via triggerPlacementScan) is what records the new timestamp.
func (p *Poller) scanThrottleElapsed(grabID int64) bool {
	p.scanMu.Lock()
	defer p.scanMu.Unlock()
	last, ok := p.lastScan[grabID]
	return !ok || time.Since(last) >= scanRetryInterval
}

// orphanRecheckInterval spaces the Stash re-checks of an orphaned grab.
// Long on purpose: the grab already failed to surface across the whole
// orphan window of 90s-throttled checks, so the recheck only exists to
// catch a manual rescue (the user scanning the file in later).
const orphanRecheckInterval = 15 * time.Minute

// orphanRecheckElapsed reports whether an orphaned grab is due another
// Stash lookup. Shares the lastScan record with the scan throttle; the
// caller stamps it via markScanAttempt when it proceeds.
func (p *Poller) orphanRecheckElapsed(grabID int64) bool {
	p.scanMu.Lock()
	defer p.scanMu.Unlock()
	last, ok := p.lastScan[grabID]
	return !ok || time.Since(last) >= orphanRecheckInterval
}

// triggerIdentify fires Stash's Identify task on the given Stash
// scene ID, sourcing from the user's first StashDB-host stash-box.
// Cached endpoint lookup so we don't hit Stash's configuration query
// every transition.
//
// Returns (jobID, nil) on success; ("", nil) if the user has no
// stash-box configured (we log + skip rather than treating that as
// an error — the user can still identify manually).
func (p *Poller) triggerIdentify(ctx context.Context, sc *stash.Client, sceneID string) (string, error) {
	endpoint, err := p.identifyEndpoint(ctx, sc)
	if err != nil {
		return "", err
	}
	if endpoint == "" {
		return "", nil
	}
	return sc.MetadataIdentify(ctx, []string{sceneID}, endpoint)
}

// identifyEndpoint returns the user's StashDB stash-box endpoint,
// populated once on first need and reused. The endpoint string has
// to match exactly what Stash has configured (trailing slash + path
// included), so we use Stash's own value rather than synthesising
// one from FORAGER_STASHDB_URL.
func (p *Poller) identifyEndpoint(ctx context.Context, sc *stash.Client) (string, error) {
	p.identifyMu.Lock()
	defer p.identifyMu.Unlock()
	if p.stashBoxEndpoint != "" {
		return p.stashBoxEndpoint, nil
	}
	boxes, err := sc.StashBoxes(ctx)
	if err != nil {
		return "", err
	}
	// Prefer the StashDB-host box; fall back to the first configured
	// box. If none configured, return empty and the caller skips.
	for _, ep := range boxes {
		if strings.Contains(ep, stash.StashDBEndpointHost) {
			p.stashBoxEndpoint = ep
			return ep, nil
		}
	}
	if len(boxes) > 0 {
		p.stashBoxEndpoint = boxes[0]
		return boxes[0], nil
	}
	return "", nil
}

// isPostCompleted reports whether the grab has moved beyond the
// download-client's purview — placement, Stash scan, identify, or a
// terminal Stash-side result. The qBit/SAB advance steps must not
// downgrade these back to "completed" or earlier just because the
// client still reports the source torrent/nzo as active.
func isPostCompleted(status string) bool {
	switch status {
	case "placed", "scanned", "confirmed", "mismatched", "orphaned":
		return true
	}
	return false
}

// classifyQbitState was previously classifyState — renamed to make
// the qBit-specific meaning explicit now that SAB has its own state
// vocabulary handled in advanceSab.
func classifyQbitState(state string) string {
	switch state {
	// Download-side states. qBit v5 renamed "pausedDL" → "stoppedDL"; both
	// mean stopped mid-download, so they must read as in-progress, not done.
	case "downloading", "stalledDL", "metaDL", "queuedDL", "checkingDL", "forcedDL",
		"allocating", "pausedDL", "stoppedDL":
		return "downloading"
	// Seed-side / done states. v5 renamed "pausedUP" → "stoppedUP".
	case "uploading", "stalledUP", "queuedUP", "checkingUP", "forcedUP", "pausedUP", "stoppedUP":
		return "completed"
	case "missingFiles", "error":
		return "failed"
	}
	// "completed" and other terminal/transient states (moving,
	// checkingResumeData, …). The caller gates "completed" on progress >= 1,
	// so a transient state on a partial torrent can't slip through as done.
	return "completed"
}

// qbitProgressUnreliable reports states in which qBit's progress field
// does not measure download completion: while (re)checking it is the
// verification fraction climbing 0 → 1 over already-downloaded data, and
// while moving it can transiently dip. Callers that treat progress < 1 as
// "incomplete download" (the premature-placement heal) must skip these
// states or they misread a routine recheck as a half-finished torrent.
func qbitProgressUnreliable(state string) bool {
	switch state {
	case "checkingDL", "checkingUP", "checkingResumeData", "moving":
		return true
	}
	return false
}

// pickRecent links a yet-to-be-enriched grab to a qBit torrent.
//
// Empirically the qBit internal torrent name (e.g. "BLACKED_RAW_106289_
// 1080P.mp4") has near-zero token overlap with the curated Prowlarr
// release title — so token-similarity is unreliable. Instead we use:
//
//   - time window (±2 min around grab time, tolerates clock drift)
//   - category match (must equal grab's configured qBit category)
//   - not-already-claimed (avoid two grabs grabbing the same torrent)
//
// Among candidates we prefer the one added closest to the grab's time.
// In typical use the user clicks Grab one-at-a-time so the most-recent
// qBit add in the window is unambiguously theirs.
func pickRecent(ts []qbit.Torrent, g *grabs.Grab, claimed map[string]bool) *qbit.Torrent {
	if len(ts) == 0 {
		return nil
	}
	windowStart := g.GrabbedAt - 120
	// Anchored to the grab's add time, not the tick's wall clock. The old
	// now+120 end re-opened the window every tick, so a grab whose async
	// add silently died kept widening its claim and would link ANY torrent
	// the user later added under the category (wrong-content link, filed
	// under the wrong performer). GrabbedAt is re-stamped when a hashless
	// add actually lands (addTorrentAttempt), so an add delayed by the
	// fetch gate or 429 backoff still falls inside this window.
	windowEnd := g.GrabbedAt + 120
	// Gather every unclaimed torrent in the time window + category.
	var cands []*qbit.Torrent
	for i := range ts {
		t := &ts[i]
		if t.AddedOn < windowStart || t.AddedOn > windowEnd {
			continue
		}
		if g.Category != "" && t.Category != g.Category {
			continue
		}
		if claimed[t.Hash] {
			continue
		}
		cands = append(cands, t)
	}
	if len(cands) == 0 {
		return nil
	}
	if len(cands) == 1 {
		return cands[0]
	}
	// Multiple torrents landed in the same window — e.g. several grabs
	// fired together. Time proximity alone can't tell them apart (and with
	// equal grab times it effectively coin-flips, which swaps the grabs),
	// so disambiguate by how well each torrent's name overlaps the grab's
	// release title, using closest-added time only as the tiebreaker.
	gTokens := titleTokens(g.ReleaseTitle)
	var best *qbit.Torrent
	bestScore := -1
	bestDelta := int64(1<<62 - 1)
	for _, t := range cands {
		score := tokenOverlap(gTokens, titleTokens(t.Name))
		delta := t.AddedOn - g.GrabbedAt
		if delta < 0 {
			delta = -delta
		}
		if score > bestScore || (score == bestScore && delta < bestDelta) {
			best, bestScore, bestDelta = t, score, delta
		}
	}
	return best
}

// titleTokens lowercases a release/torrent name and splits it into the set
// of alphanumeric tokens ≥3 chars — enough to drop punctuation and noise
// words ("of", "my") while keeping discriminating tokens ("bbc", "shower").
func titleTokens(s string) map[string]bool {
	out := map[string]bool{}
	var b strings.Builder
	flush := func() {
		if b.Len() >= 3 {
			out[b.String()] = true
		}
		b.Reset()
	}
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// tokenOverlap counts how many of the grab's title tokens appear in the
// torrent name's token set — the disambiguation score in pickRecent.
func tokenOverlap(a, b map[string]bool) int {
	n := 0
	for tok := range a {
		if b[tok] {
			n++
		}
	}
	return n
}
