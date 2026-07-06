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
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/ordureconnoisseur/forager/internal/clienterr"
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

	// pending is the api layer's in-flight async-add registry. The link
	// timeout must not fail a still-unlinked queued grab whose add is
	// legitimately queued behind the fetch gate (a bulk batch tail waits
	// far longer than any fixed window). Nil-safe; empty after a restart,
	// which is when the timeout SHOULD fire.
	pending *grabs.PendingAdds

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

	// grace is the one per-grab "how long has this bad condition held?" clock,
	// shared by both clients (a grab belongs to exactly one, so a single map
	// keyed by grab id never collides). It answers two structurally identical
	// questions:
	//
	//   - SAB: how long has the nzo been absent from SAB's queue+history?
	//     advanceSab fails only once absence exceeds sabInflightTimeout,
	//     measured from the last positive contact (graceClear on a sighting),
	//     NOT from grab time — usenet jobs sit invisibly in fetch/queue states
	//     far longer than the old grab-time grace, which false-failed
	//     slow-but-fine downloads SAB later completed.
	//   - qBit: how long has the torrent been in a failure state
	//     (error/missingFiles)? advanceQbit fails only once it exceeds
	//     qbitErrorGrace, measured from the first error sighting; a healthy
	//     reading (graceClear) resets it. Those states are usually transient (a
	//     tracker hiccup, a recheck, a volume not yet mounted after a restart),
	//     and failing on the first sighting strands the download: Active()
	//     excludes failed grabs, so a torrent that recovers is never re-checked.
	//
	// In-memory: lost on restart, which merely restarts the clock (the adopt
	// sweep's revive path is the correctness backstop). Pruned in tickOnce.
	graceMu sync.Mutex
	grace   map[int64]time.Time

	// identifyJob records the most recent Stash Identify job id fired per
	// grab. Before re-firing an Identify (a pack batch or a single scene),
	// we check whether that job is still queued or running; if so we skip,
	// so a backed-up Stash serial queue can't accumulate redundant identical
	// Identify jobs while an earlier one waits its turn. In-memory: lost on
	// restart, which merely permits one extra re-fire. Pruned in tickOnce.
	identifyJobMu sync.Mutex
	identifyJob   map[int64]string

	// resumeKick collects, during one tick's advance loop, the hashes of
	// torrents just seen ENTERING qBit's "error" state, so the tick can
	// fire one resume each after the loop. qBit never auto-resumes an
	// errored torrent, so a transient write failure (a stalled NAS mount)
	// otherwise strands mid-download torrents until someone hand-resumes
	// them. Owned by the single-goroutine tick: appended in advanceQbit,
	// drained in tickOnce, never touched concurrently.
	resumeKick []string

	// scanJob records the most recent Stash metadataScan job id fired per
	// grab. Before re-firing a placement scan (the throttled retry that waits
	// for Stash to index a placed file), we check whether that job is still
	// queued or running; if so we skip. Without this, a Stash serial queue
	// backed up behind other work (e.g. slow phash generation while the box is
	// also transcoding) leaves grabs at "placed" long enough that every 90s
	// tick re-queues the same scan, piling up thousands of redundant jobs.
	// In-memory: lost on restart, which merely permits one extra re-fire.
	// Pruned in tickOnce alongside identifyJob.
	scanJobMu sync.Mutex
	scanJob   map[int64]string
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

// singleIdentifyGrace bounds how long an adopted single with no prediction
// keeps retrying Identify before settling as "in library (scanned)". Generous
// enough to cover Stash's serial job queue draining a batch import (so a studio
// scene's identify lands even when it's queued behind many scans) and to
// survive a Stash restart that drops a queued identify (the retry re-fires it),
// but far shorter than the orphan window so genuinely-amateur content
// StashDB doesn't have settles in minutes, not hours.
const singleIdentifyGrace = 30 * time.Minute

func New(repo *grabs.Repo, db *sql.DB, pool *clientpool.Pool, log *slog.Logger, interval, orphanAfter time.Duration, pending *grabs.PendingAdds) *Poller {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	if orphanAfter <= 0 {
		orphanAfter = 6 * time.Hour
	}
	return &Poller{
		repo:        repo,
		db:          db,
		pool:        pool,
		log:         log,
		interval:    interval,
		orphan:      orphanAfter,
		pending:     pending,
		lastScan:    map[int64]time.Time{},
		packScan:    map[int64]packScanState{},
		grace:       map[int64]time.Time{},
		identifyJob: map[int64]string{},
		scanJob:     map[int64]string{},
	}
}

// Run ticks once at startup then on `interval` until ctx is cancelled.
// Errors are logged and the loop continues; a single bad tick doesn't
// kill the poller.
func (p *Poller) Run(ctx context.Context) {
	p.log.Info("poller starting", "interval", p.interval, "orphan_after", p.orphan)
	p.safeTick(ctx)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			p.log.Info("poller stopping")
			return
		case <-t.C:
			p.safeTick(ctx)
		}
	}
}

// safeTick runs one tick, converting a panic into a logged error. The tick
// chews qBit/SAB/Stash/StashDB responses whose shapes we don't control; a
// nil-field panic must cost one tick, not the daemon (main starts Run as a
// bare goroutine with nothing above it to recover).
func (p *Poller) safeTick(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			p.log.Error("panic in poller tick", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	if err := p.tickOnce(ctx); err != nil {
		p.log.Error("tick", "err", err)
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
	// Drop per-grab in-memory state for grabs that left Active() (confirmed,
	// failed, deleted): lastScan, packScan, grace and identifyJob otherwise
	// grow for the daemon's lifetime. This is the SOLE cleanup path — pack
	// confirm deliberately does not eagerly clear its settle high-water,
	// because its confirm write can be dropped by an optimistic-lock miss and
	// the retained high-water lets the retry re-confirm immediately. [C9]
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
	p.graceMu.Lock()
	for id := range p.grace {
		if !activeIDs[id] {
			delete(p.grace, id)
		}
	}
	p.graceMu.Unlock()
	p.identifyJobMu.Lock()
	for id := range p.identifyJob {
		if !activeIDs[id] {
			delete(p.identifyJob, id)
		}
	}
	p.identifyJobMu.Unlock()
	p.scanJobMu.Lock()
	for id := range p.scanJob {
		if !activeIDs[id] {
			delete(p.scanJob, id)
		}
	}
	p.scanJobMu.Unlock()
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

	// Fire the one-shot resume kicks collected by advanceQbit (torrents
	// just seen entering the error state). After the loop so the advance
	// path stays network-free per grab; best-effort — a failed kick just
	// leaves the torrent for the grace window to fail as before.
	if len(p.resumeKick) > 0 {
		if qb := p.pool.Qbit(); qb != nil {
			for _, h := range p.resumeKick {
				if err := qb.Resume(ctx, h); err != nil {
					p.log.Warn("resume kick", "hash", h, "err", err)
				}
			}
		}
		p.resumeKick = nil
	}

	p.log.Debug("tick done", "active", len(active), "elapsed", time.Since(t0))
	return nil
}

func (p *Poller) advance(ctx context.Context, g *grabs.Grab, qbitTorrents []qbit.Torrent, qbitByHash map[string]*qbit.Torrent, qbitListOK bool, claimed map[string]bool, sabQueue, sabHistory []sabnzbd.Item, sabListsOK bool) error {
	// A pack parked in "tagging" by the set-performer endpoint has already been
	// re-filed into the performer folder; there's no download work left. We only
	// wait for Stash to re-index the new path, then apply the pack's performer to
	// every scene. Handle it in isolation from the main place/confirm pipeline.
	if g.Kind == "pack" && g.Status == "tagging" {
		changed, err := p.advancePackTag(ctx, g)
		if err != nil {
			return err
		}
		if changed {
			if uerr := p.repo.Update(ctx, *g); uerr != nil {
				if errors.Is(uerr, grabs.ErrStaleUpdate) {
					p.log.Info("grab changed under tick; skipping stale write", "id", g.ID)
					return nil
				}
				return uerr
			}
		}
		return nil
	}

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
	// A confirm-step error must NOT return before the Update below. Steps
	// 1-3 may have done irreversible work this tick — the file hardlinked
	// into the library, SabDeleteAfterPlace removing the history entry —
	// whose record lives only in g until the write. Returning early dropped
	// that state: the next tick saw completed-with-no-placement, the client
	// entry already gone, and failed a grab whose file was in the library
	// (a retry then re-downloaded a duplicate). Stash the error and return
	// it AFTER the persist.
	var confirmErr error
	if g.Kind == "pack" {
		// Pack grabs have their own confirm path — enumerate every placed
		// file's scene, drive identify across all of them, dedup, then
		// confirm. Distinct from the single-scene match below.
		if confirmable && stashC != nil {
			d, err := p.advancePackConfirm(ctx, g, stashC)
			dirty = dirty || d
			confirmErr = err
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
			if err != nil && !errors.Is(err, clienterr.ErrNotFound) {
				// A real lookup failure (transient Stash error). ErrNotFound is
				// NOT one: it's the expected "scene not indexed yet" state, which
				// must fall through to the not-found handling below (re-scan /
				// orphan), exactly as the old (nil, nil) did.
				confirmErr = err
				scene = nil
			}
			prevStatus := g.Status
			switch {
			case confirmErr != nil:
				// Lookup failed — change nothing; retry next tick.
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
						p.rememberIdentifyJob(g.ID, jobID)
					}
					p.markScanAttempt(g.ID)
				} else if g.PredictedStashDBID == "" {
					// No prediction (an adopted/manual grab). Being scanned in
					// is the floor, BUT a manual add of studio content IS on
					// StashDB and should still be identified — and we can't tell
					// amateur from studio until identify actually runs. Stash's
					// identify is a queued, serial job that can land well after
					// the first attempt (behind other scans/generates), and a
					// queued one is lost on a Stash restart. So keep retrying
					// identify on the throttle until the cross-id lands, and
					// only settle as "in library (scanned)" once a bounded grace
					// has passed — that's the window for genuinely-amateur
					// content StashDB doesn't have. (Mirrors the predicted path
					// below, just with a shorter grace than the orphan window.)
					if g.CompletedAt > 0 && time.Since(time.Unix(g.CompletedAt, 0)) > singleIdentifyGrace {
						g.Status = "confirmed"
						g.Reason = "in library (scanned)"
						g.ConfirmedAt = time.Now().Unix()
						dirty = true
					} else if p.scanThrottleElapsed(g.ID) && !p.identifyInFlight(ctx, stashC, g.ID) {
						if jobID, err := p.triggerIdentify(ctx, stashC, scene.ID); err != nil {
							p.log.Warn("metadataIdentify trigger failed", "id", g.ID, "scene_id", scene.ID, "err", err)
						} else if jobID != "" {
							p.log.Info("metadataIdentify retried", "id", g.ID, "scene_id", scene.ID, "job_id", jobID)
							p.rememberIdentifyJob(g.ID, jobID)
						}
						p.markScanAttempt(g.ID)
					}
				} else if g.CompletedAt > 0 && time.Since(time.Unix(g.CompletedAt, 0)) > p.orphan {
					// Predicted grab: gave up on the StashDB match.
					g.Status = "confirmed"
					g.Reason = "in library; no StashDB match"
					g.ConfirmedAt = time.Now().Unix()
					dirty = true
				} else if p.scanThrottleElapsed(g.ID) && !p.identifyInFlight(ctx, stashC, g.ID) {
					// Predicted grab: retry Identify until the cross-id lands.
					if jobID, err := p.triggerIdentify(ctx, stashC, scene.ID); err != nil {
						p.log.Warn("metadataIdentify trigger failed", "id", g.ID, "scene_id", scene.ID, "err", err)
					} else if jobID != "" {
						p.log.Info("metadataIdentify triggered", "id", g.ID, "scene_id", scene.ID, "job_id", jobID)
						p.rememberIdentifyJob(g.ID, jobID)
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
				if g.PlacedPath != "" && p.scanThrottleElapsed(g.ID) &&
					!p.scanInFlight(ctx, stashC, g.ID) {
					p.triggerPlacementScan(ctx, stashC, g.ID, g.PlacedPath)
				}
			}
			if scene != nil && g.Status != prevStatus &&
				(g.Status == "confirmed" || g.Status == "mismatched") {
				// Identify settled this single (the scene is in Stash). Now
				// generate the previews/sprites the fast placement scan skipped
				// — after identify, so it can't block it in the serial queue.
				p.triggerGenerate(ctx, stashC, g.ID, []string{scene.ID})
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
	return confirmErr
}

// packTagTimeout bounds how long a re-filed pack waits for its rescan to
// surface the moved files before we give up auto-tagging and settle it
// confirmed. The folder is organised regardless; the user can still tag from
// the grab card. Generous, because the rescan sits behind Stash's serial job
// queue, which can be minutes deep.
const packTagTimeout = 30 * time.Minute

// packNeedle derives the Stash-side path substring that scopes a pack's scenes:
// the path-mapped placed dir, its basename, or the client save-name. Mirrors
// advancePackConfirm's derivation and the api layer's packNeedle.
func (p *Poller) packNeedle(g *grabs.Grab) string {
	if g.PlacedPath != "" {
		if n := pathmap.Translate(g.PlacedPath, p.pool.Settings().StashPathMapping); n != "" {
			return n
		}
		return pathmap.Base(g.PlacedPath)
	}
	return g.ClientName
}

// localPerformerID maps a performer display name to its LOCAL Stash id via
// performer_cache. Empty when the performer isn't in the library (so nothing to
// tag with). Mirrors the api layer's localPerformerIDByName.
func (p *Poller) localPerformerID(ctx context.Context, name string) string {
	if name == "" || p.db == nil {
		return ""
	}
	var id string
	_ = p.db.QueryRowContext(ctx,
		`SELECT stash_id FROM performer_cache WHERE name = ? COLLATE NOCASE AND stash_id != '' LIMIT 1`,
		name).Scan(&id)
	return id
}

// advancePackTag applies a re-filed pack's performer to every scene Stash has
// under the new placed dir, once the rescan has surfaced them. Returns whether
// it mutated g (a status change worth persisting). The set-performer endpoint
// parks a pack in "tagging" after re-filing + queuing a rescan; this waits for
// the scenes to appear at the new path (re-triggering the scan if it stalled),
// then ADDs the performer to all of them and confirms. ADD mode is additive, so
// identified scenes keep their performers. Bounded by packTagTimeout so a scan
// that never lands doesn't strand the grab in "tagging" forever.
func (p *Poller) advancePackTag(ctx context.Context, g *grabs.Grab) (bool, error) {
	sc := p.pool.Stash()
	if sc == nil {
		return false, nil // no Stash configured this tick — try again later
	}
	localID := p.localPerformerID(ctx, g.PerformerName)
	if localID == "" {
		// Performer left the library (renamed/removed) since it was set. Can't
		// tag, but the folder is organised — settle it.
		g.Status = "confirmed"
		g.Reason = "re-filed under " + g.PerformerName + " (not in library, so not auto-tagged)"
		p.graceClear(g.ID)
		return true, nil
	}
	needle := p.packNeedle(g)
	if needle == "" {
		g.Status = "confirmed"
		g.Reason = "re-filed, but no path to tag its scenes by"
		p.graceClear(g.ID)
		return true, nil
	}
	scenes, err := sc.FindScenesUnderPath(ctx, needle)
	if err != nil {
		return false, nil // transient Stash error — retry next tick without churning state
	}
	if len(scenes) == 0 {
		// The rescan hasn't surfaced the moved files yet. Re-trigger it
		// (throttled) and keep waiting until the timeout.
		if p.scanThrottleElapsed(g.ID) && !p.scanInFlight(ctx, sc, g.ID) {
			p.triggerPlacementScan(ctx, sc, g.ID, g.PlacedPath)
		}
		if p.graceElapsed(g.ID, packTagTimeout) {
			g.Status = "confirmed"
			g.Reason = "re-filed; re-index timed out, tag from the grab card"
			p.graceClear(g.ID)
			return true, nil
		}
		return false, nil // still "tagging"
	}
	ids := make([]string, 0, len(scenes))
	for _, m := range scenes {
		ids = append(ids, m.ID)
	}
	if _, err := sc.AddScenePerformer(ctx, ids, localID); err != nil {
		return false, nil // Stash write failed — retry next tick
	}
	g.Status = "confirmed"
	g.Reason = fmt.Sprintf("re-filed under %s + tagged %d scenes", g.PerformerName, len(ids))
	p.graceClear(g.ID)
	p.log.Info("pack auto-tagged after re-file", "id", g.ID, "performer", g.PerformerName, "scenes", len(ids))
	return true, nil
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
	// Identify pack scenes by their full placed-directory path, not just
	// its basename — the pack often lands at <performer>/<pack-name>
	// where <pack-name> can equal the performer (e.g.
	// /Media/comatozze/comatozze). The bare basename would then also
	// match the performer's pre-existing scenes under /Media/comatozze,
	// corrupting both the count and the dedup decision. Translate to the
	// Stash-side path so the substring is specific; fall back to the
	// basename only when no path mapping is configured.
	//
	// With placement disabled there is no placed path at all — but the
	// grab is still confirmable (completed && !placer.Configured()), so
	// match on the client-reported name (the download's directory in the
	// client's save path), the same fallback the single-scene path uses.
	// Returning early instead left such packs in Active() forever: never
	// confirmed, never orphaned, polled for the daemon's lifetime.
	var needle string
	if g.PlacedPath != "" {
		needle = pathmap.Translate(g.PlacedPath, p.pool.Settings().StashPathMapping)
		if needle == "" {
			needle = pathmap.Base(g.PlacedPath)
		}
	} else {
		needle = g.ClientName
	}
	if needle == "" {
		return false, nil
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
		// Give-up backstop: if the download finished long ago but Stash never
		// indexed a single scene under the path, orphan the pack instead of
		// re-scanning forever. The single-scene path orphans this exact
		// condition (g.CompletedAt>0 && since>p.orphan); packs lacked an
		// equivalent because this branch returns before the confirm/gaveUp
		// logic below, so a path-mapping mismatch (stale prefix after a mount
		// rename, no mapping configured) left the pack polled every tick for
		// the daemon's lifetime. Orphaned is a recoverable parking state:
		// if the files are later scanned in, found>0 and the pack resumes.
		if g.CompletedAt > 0 && time.Since(time.Unix(g.CompletedAt, 0)) > p.orphan {
			if g.Status != "orphaned" {
				g.Status = "orphaned"
				g.Reason = "placed but Stash never indexed any scene under " + needle
				dirty = true
				p.log.Info("pack orphaned: no scenes indexed under path", "id", g.ID, "needle", needle, "placed", g.PlacedPath)
			}
			return dirty, nil
		}
		if (g.Status == "placed" || g.Status == "completed") && p.scanThrottleElapsed(g.ID) &&
			!p.scanInFlight(ctx, sc, g.ID) {
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

	// Has Stash's directory scan settled (count stopped climbing) and indexed
	// enough of the expected file count? Computed before driving identify so a
	// scan that stalled mid-directory can re-trigger a scan rather than
	// identifying only the partial set. packScanSettled updates the in-memory
	// high-water as a side effect, so call it exactly once per tick.
	settled := p.packScanSettled(g.ID, found)
	downloadDone := g.CompletedAt > 0
	var sinceDone time.Duration
	if downloadDone {
		sinceDone = time.Since(time.Unix(g.CompletedAt, 0))
	}
	coverageOK := packScanCoverageOK(found, g.PackFiles)

	// A scan that SETTLED below the expected file count means the
	// post-placement scan was coalesced or interrupted and indexed only part
	// of the directory. Re-fire the directory scan (throttled) so Stash
	// indexes the rest, instead of stranding the pack at "scanned" until the
	// 6h backstop confirms + dedups against a partial set. Only fires when
	// the count has settled (still climbing? just wait) and is known
	// incomplete (PackFiles>0 and below floor); magnet/manual packs
	// (PackFiles==0) are coverageOK vacuously, so this never fires for them.
	// Shares the scan throttle with identify: getting the rest of the files
	// in takes priority while coverage is incomplete; identify resumes once
	// the scan grows again (settled flips false) or coverage is reached. [C7]
	if settled && !coverageOK && downloadDone && sinceDone <= p.orphan &&
		p.scanThrottleElapsed(g.ID) && !p.scanInFlight(ctx, sc, g.ID) {
		p.log.Info("pack: scan settled below expected count, re-scanning",
			"id", g.ID, "found", found, "packFiles", g.PackFiles, "placed", g.PlacedPath)
		p.triggerPlacementScan(ctx, sc, g.ID, g.PlacedPath)
	} else if len(toIdentify) > 0 && p.scanThrottleElapsed(g.ID) && !p.identifyInFlight(ctx, sc, g.ID) {
		// Drive identify across everything still missing a cross-id. Skip while
		// our previous batch is still queued/running in Stash's serial queue —
		// re-firing then would just stack a redundant identical job behind it.
		if jobID, err := p.triggerIdentifyBatch(ctx, sc, toIdentify); err != nil {
			p.log.Warn("pack identify trigger failed", "id", g.ID, "scenes", len(toIdentify), "err", err)
		} else if jobID != "" {
			p.log.Info("pack identify triggered", "id", g.ID, "scenes", len(toIdentify), "job_id", jobID)
			p.rememberIdentifyJob(g.ID, jobID)
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
	ready := settled && coverageOK && (identifyDone || !downloadDone || sinceDone > packIdentifyGrace)
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
			coverageVerified := g.PackFiles > 0 && coverageOK
			// Resolve the stash-box endpoint. A real lookup error means Stash
			// is unreachable right now — distinct from the genuine "no box
			// configured" case (err==nil, endpoint==""). Don't confirm against
			// a dedup we can't run: defer and retry next tick, otherwise a
			// transient outage at this exact tick would confirm the pack with
			// duplicates silently left unreconciled, never to be retried (the
			// grab leaves Active() once confirmed). [C10]
			endpoint, epErr := p.identifyEndpoint(ctx, sc)
			if epErr != nil {
				p.log.Warn("pack confirm deferred: stash-box endpoint lookup failed", "id", g.ID, "err", epErr)
				return dirty, nil
			}
			deduped, recorded, err := p.dedupPack(ctx, sc, g, scenes, endpoint, keep, coverageVerified)
			if err != nil {
				// Dedup didn't complete (a transient Stash error, or a failed
				// review-item write). Stay "scanned" and retry next tick rather
				// than confirming against an unfinished reconcile — keep="review"
				// would otherwise lose the pending-review record permanently,
				// and a destroy mode would leave duplicates unreconciled. The
				// retry re-runs dedup idempotently (destroyed copies drop from
				// the re-query; UpsertDuplicate refreshes in place). [C10][C11]
				p.log.Warn("pack confirm deferred: dedup failed", "id", g.ID, "err", err)
				return dirty, nil
			}
			if deduped > 0 {
				g.PackDeduped += deduped
			}
			pendingReview = recorded
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
		// NB: do NOT forgetPackScan here. The confirm write happens in the
		// caller and can be dropped by an optimistic-lock miss (a concurrent
		// performer reassignment bumps rev). If we cleared the settle
		// high-water now and the write were dropped, the next tick would
		// re-seed it and delay re-confirm by a full settle window. Leaving it
		// lets a dropped-write pack re-confirm immediately; tickOnce's prune
		// frees the entry one tick after the grab actually leaves Active(). [C9]
		p.log.Info("pack confirmed", "id", g.ID, "identified", identified, "found", found, "deduped", g.PackDeduped, "pendingReview", pendingReview)
		// Generate the previews/sprites the fast scan skipped for every scene
		// the pack landed, now that it's settled (after identify, so it can't
		// block it in the serial queue).
		packIDs := make([]string, 0, len(scenes))
		for _, s := range scenes {
			packIDs = append(packIDs, s.ID)
		}
		p.triggerGenerate(ctx, sc, g.ID, packIDs)
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
	copiesOf := func(stashID string) ([]stash.SceneRef, error) {
		if sweep != nil {
			return sweep[stashID], nil
		}
		if refs, ok := cache[stashID]; ok {
			return refs, nil
		}
		refs, err := sc.FindSceneRefsByStashID(ctx, endpoint, stashID)
		if err != nil {
			// Propagated, not swallowed: a transient Stash error here must
			// not read as "unique to this pack" — that would confirm the
			// pack with this scene's dedup silently skipped, permanently
			// (confirmed grabs leave Active()). The caller defers the
			// confirm and retries next tick, same as a review-write
			// failure. [C10][C11]
			return nil, fmt.Errorf("copies of %s: %w", stashID, err)
		}
		cache[stashID] = refs
		return refs, nil
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
		refs, err := copiesOf(ps.StashDBID)
		if err != nil {
			return deduped, recorded, err
		}
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
			// row in place. A write failure is propagated (not swallowed) so
			// the caller defers the confirm and retries, rather than confirming
			// the pack with this duplicate permanently unrecorded. [C11]
			ok, err := p.recordReviewDuplicate(ctx, g, ps, refs, packIDs)
			if err != nil {
				return deduped, recorded, err
			}
			if ok {
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
// this pack. Returns (true, nil) when a row was written, (false, err) on a
// write failure (the caller propagates it so the confirm defers + retries),
// and (false, nil) when there's nothing actionable to record — the pack copy
// can't be located among refs AND we have no fallback identity, so there's no
// pack-side scene id to act on later.
func (p *Poller) recordReviewDuplicate(ctx context.Context, g *grabs.Grab, ps stash.SceneMatch, refs []stash.SceneRef, packIDs map[string]bool) (bool, error) {
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
		return false, nil
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
		return false, err
	}
	return true, nil
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

// identifyInFlight reports whether the last Identify job fired for this grab
// is still pending or running in Stash's queue. A finished, failed,
// cancelled, or unknown (already drained from the queue) job returns false,
// so the caller is free to fire a fresh batch.
//
// On a JobStatus query error we report true (treat the prior job as still in
// flight) rather than false. A query error means Stash is unreachable for
// this call — and a fired identify would fail against the same unreachable
// Stash anyway, so firing gains nothing and would just stack a redundant job
// behind the still-pending one once Stash recovers (the pile-up this guard
// exists to prevent). This can't block identify indefinitely: a persistent
// JobStatus failure is a Stash outage, and identify resumes the moment the
// query succeeds again. [C12]
func (p *Poller) identifyInFlight(ctx context.Context, sc *stash.Client, grabID int64) bool {
	p.identifyJobMu.Lock()
	jobID := p.identifyJob[grabID]
	p.identifyJobMu.Unlock()
	if jobID == "" {
		return false
	}
	status, err := sc.JobStatus(ctx, jobID)
	if err != nil {
		p.log.Warn("identify job status check failed; assuming still in flight", "id", grabID, "job_id", jobID, "err", err)
		return true
	}
	switch status {
	case "READY", "RUNNING", "STOPPING":
		return true
	default:
		// FINISHED, CANCELLED, FAILED, or "" (no longer in the queue).
		return false
	}
}

// rememberIdentifyJob records the Identify job id last fired for a grab so a
// subsequent tick can see it's still in flight (see identifyInFlight).
func (p *Poller) rememberIdentifyJob(grabID int64, jobID string) {
	if jobID == "" {
		return
	}
	p.identifyJobMu.Lock()
	p.identifyJob[grabID] = jobID
	p.identifyJobMu.Unlock()
}

// scanInFlight reports whether the last placement scan fired for this grab is
// still queued or running in Stash — the metadataScan analogue of
// identifyInFlight. It exists to stop the throttled re-scan from stacking
// redundant identical scans behind an earlier one while Stash's serial queue
// is backed up (the pileup that buried the queue in thousands of duplicate
// scans when the box was busy transcoding). Same failure-mode handling: a
// JobStatus query error reports true (assume still in flight) rather than
// firing a doomed scan against an unreachable Stash; it can't block forever
// because the query recovers when Stash does.
func (p *Poller) scanInFlight(ctx context.Context, sc *stash.Client, grabID int64) bool {
	p.scanJobMu.Lock()
	jobID := p.scanJob[grabID]
	p.scanJobMu.Unlock()
	if jobID == "" {
		return false
	}
	status, err := sc.JobStatus(ctx, jobID)
	if err != nil {
		p.log.Warn("scan job status check failed; assuming still in flight", "id", grabID, "job_id", jobID, "err", err)
		return true
	}
	switch status {
	case "READY", "RUNNING", "STOPPING":
		return true
	default:
		// FINISHED, CANCELLED, FAILED, or "" (no longer in the queue).
		return false
	}
}

// rememberScanJob records the metadataScan job id last fired for a grab so a
// subsequent tick can see it's still in flight (see scanInFlight).
func (p *Poller) rememberScanJob(grabID int64, jobID string) {
	if jobID == "" {
		return
	}
	p.scanJobMu.Lock()
	p.scanJob[grabID] = jobID
	p.scanJobMu.Unlock()
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

// graceElapsed reports whether a per-grab bad condition (a SAB nzo absent from
// SAB, a qBit torrent in an error state) has held continuously for at least d.
// The first call for a grab seeds the clock to now and returns false, so the
// first sighting of the bad condition starts the timer rather than tripping it
// immediately; graceClear deletes the entry when the condition lifts, so a
// later recurrence starts a fresh window. One clock serves both clients — a
// grab belongs to exactly one, so the grab id never collides.
func (p *Poller) graceElapsed(grabID int64, d time.Duration) bool {
	p.graceMu.Lock()
	defer p.graceMu.Unlock()
	since, ok := p.grace[grabID]
	if !ok {
		p.grace[grabID] = time.Now()
		return false
	}
	return time.Since(since) >= d
}

// graceStart starts a grab's grace clock if it isn't already running and
// reports whether it just did — i.e. whether this is the FIRST sighting of
// the bad condition. Lets a caller attach a one-shot side effect (the
// error-state resume kick) to the transition without disturbing the
// elapsed measurement graceElapsed does on the same clock.
func (p *Poller) graceStart(grabID int64) bool {
	p.graceMu.Lock()
	defer p.graceMu.Unlock()
	if _, ok := p.grace[grabID]; ok {
		return false
	}
	p.grace[grabID] = time.Now()
	return true
}

// graceClear resets a grab's grace clock after the bad condition lifts (a SAB
// nzo seen again in queue/history, a qBit torrent healthy again), so the next
// recurrence is measured fresh instead of tripping off the stale first-seen
// time. Calling it when no clock is set is a harmless no-op.
func (p *Poller) graceClear(grabID int64) {
	p.graceMu.Lock()
	delete(p.grace, grabID)
	p.graceMu.Unlock()
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
		// never landed — don't leave it queued forever. EXCEPT while the
		// add is still pending in-process (fetch-gate queue, rate-limit
		// backoff): a bulk batch tail legitimately waits past any fixed
		// window, and failing it here races the in-flight add (the
		// post-gate status check then bails without adding, so a retried
		// batch re-fails its tail forever).
		if g.Status == "queued" && g.GrabbedAt > 0 &&
			time.Since(time.Unix(g.GrabbedAt, 0)) > qbitLinkTimeout &&
			!p.pending.Has(g.ID) {
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
			(time.Since(time.Unix(g.GrabbedAt, 0)) <= qbitLinkTimeout ||
				p.pending.Has(g.ID)) {
			return dirty, ""
		}
		// The torrent vanishing does NOT undo what already happened on disk.
		// Once the file is placed (or beyond), losing the qBit entry just
		// means seeding stopped — a seed-ratio auto-remove or a manual
		// delete of a finished torrent. Failing those grabs re-downloads
		// files the library already has; mirror advanceSab and leave
		// post-completed states alone so the Stash-side steps keep going.
		if isPostCompleted(g.Status) {
			return dirty, ""
		}
		if g.Status == "completed" {
			if g.PlacedPath == "" && p.pool.Placer().Configured() {
				// Completed but never placed, and the torrent entry (the only
				// source of the download's on-disk location) is gone —
				// placement can never succeed now. Fail for a clean retry,
				// like advanceSab's purged-history branch.
				g.Status = "failed"
				g.Reason = "qbit removed the torrent before placement could finish; retry to re-download"
				return true, ""
			}
			// Placement disabled (or already placed): confirmation matches on
			// ClientName / the placed file and needs nothing from qBit.
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
	} else if t.Progress < g.Progress && !qbitProgressUnreliable(t.State) &&
		classifyQbitState(t.State) == "downloading" {
		// Genuine regression: a recheck found lost or corrupt pieces and the
		// torrent is downloading again below its old high-water mark. A pure
		// ratchet would freeze progress/progress_at at the old peak — the
		// stalled badge then fires on an actively-downloading grab, and a
		// ratcheted Progress >= 1 masks the stall check entirely for the
		// re-download.
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
	//     a finished download and is never premature; nothing clears
	//     completed_at while a placement exists (retryGrab deliberately
	//     keeps the lifecycle stamps on placed grabs for exactly this
	//     reason), so a later recheck regressing progress can't re-arm the
	//     heal against it. Premature placements have CompletedAt == 0 (the
	//     download was never seen complete).
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
	// qBit reports "error"/"missingFiles" transiently (tracker hiccup, brief
	// disk/IO error, a recheck, a not-yet-mounted volume after an unclean
	// restart). Failing on the first sighting strands the download: Active()
	// excludes failed grabs, so a torrent qBit later recovers to
	// downloading/seeding is never re-checked and never placed. Hold off until
	// the bad state persists past qbitErrorGrace; a healthy reading clears the
	// clock. (adoptQbitOrphans' revive path is the backstop that recovers a
	// grab we DO end up failing, should it recover after the grace.)
	if newStatus == "failed" {
		// First sighting of the plain "error" state: kick ONE resume.
		// qBit never auto-resumes an errored torrent, so a transient
		// write failure (a stalled NAS mount flipped six mid-download
		// torrents to error on 2026-07-06) otherwise strands the
		// download until someone hand-resumes it. If the cause
		// persists the torrent re-errors and the grace window fails
		// the grab exactly as before. missingFiles is deliberately
		// NOT kicked: pack dedup deletes duplicate files out from
		// under seeding torrents, and a resume there would re-download
		// content the user chose to remove.
		if p.graceStart(g.ID) && t.State == "error" {
			p.resumeKick = append(p.resumeKick, t.Hash)
			p.log.Info("qbit torrent errored; kicking a one-shot resume",
				"id", g.ID, "hash", t.Hash)
		}
		if !p.graceElapsed(g.ID, qbitErrorGrace) {
			return dirty, t.ContentPath
		}
	} else if newStatus != "" {
		p.graceClear(g.ID)
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

// sabInflightTimeout is how long a SAB grab's nzo may be absent from BOTH
// SAB's queue and history — measured from the last positive contact (the
// shared grace clock) — before advanceSab declares it gone. Far longer than
// sabRegisterGrace because SAB legitimately holds a job out of view while it
// fetches the NZB from the indexer and while it waits in a backed-up queue;
// those gaps can run tens of minutes on a slow server. A real removal stays
// absent and still fails after this; a slow-but-live download re-surfaces and
// resets the clock long before it elapses.
const sabInflightTimeout = 45 * time.Minute

// qbitLinkTimeout is how long a qBit grab may sit "queued" without ever
// linking to a torrent before it's declared failed. Well beyond the 2-min
// async-add budget, so a slow add still resolves first; a grab still
// unlinked past it means the add never landed.
const qbitLinkTimeout = 10 * time.Minute

// qbitErrorGrace is how long a qBit torrent may report a failure state
// (error/missingFiles) before the grab is declared failed. qBit raises these
// transiently — a tracker re-announce after an outage, a brief disk/IO error,
// a force-recheck, or a volume not yet mounted right after an unclean restart
// — and usually clears within a re-announce cycle or two. Generous on purpose,
// matching the SAB philosophy: a download stuck "downloading" a few extra
// minutes is far cheaper than a false "failed" that strands a torrent qBit
// then completes (Active() never re-checks a failed grab).
const qbitErrorGrace = 10 * time.Minute

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
// forage's own in-flight adds. Returns how many were adopted and how many
// were skipped only for being too fresh (the button reports the latter so a
// bulk add the user just made reads as "auto-adopting soon", not "nothing
// new"). Safe alongside the periodic tick: adoptOrphans serialises on adoptMu
// and the known-id check prevents double-adoption.
func (p *Poller) AdoptNow(ctx context.Context) (adopted, skippedRecent int) {
	return p.adoptOrphans(ctx, manualAdoptGrace)
}

// adoptOrphans creates grab rows for downloads the user fed to a client
// directly under the configured forage category — torrents added straight
// to qBit, NZBs uploaded straight to SAB — so they flow through the
// normal place → scan → identify → dedup pipeline exactly like a UI add.
// Scoped strictly to the forage category in each client; other categories
// (the *arr stack, ad-hoc client use) are never touched. minAge applies
// to the qBit pass only (see adoptQbitOrphans); SAB adoption is gated on
// job completion instead. Returns the count adopted across both clients and
// the count skipped only for being too fresh (qBit pass only).
func (p *Poller) adoptOrphans(ctx context.Context, minAge time.Duration) (adopted, skippedRecent int) {
	p.adoptMu.Lock()
	defer p.adoptMu.Unlock()
	known, err := p.repo.KnownClientIDs(ctx)
	if err != nil {
		p.log.Warn("adopt: known client ids", "err", err)
		return 0, 0
	}
	qAdopted, skippedRecent := p.adoptQbitOrphans(ctx, known, minAge)
	return qAdopted + p.adoptSabOrphans(ctx, known), skippedRecent
}

// adoptQbitOrphans is the qBit half of adoptOrphans: untracked torrents
// under the forage category become grab rows. qBit *tags* are ignored —
// only the category matches. minAge skips torrents added more recently
// than it (the periodic tick passes adoptionGrace; the manual button
// passes manualAdoptGrace) so a torrent forage itself just added gets
// linked to its existing grab first.
func (p *Poller) adoptQbitOrphans(ctx context.Context, known map[string]bool, minAge time.Duration) (adopted, skippedRecent int) {
	qb := p.pool.Qbit()
	if qb == nil {
		return
	}
	cat := p.pool.Settings().QbitCategory
	if cat == "" {
		return // never adopt uncategorised torrents
	}
	ts, err := qb.ListTorrents(ctx, qbit.ListOpts{Category: cat, Filter: "all"})
	if err != nil {
		p.log.Warn("adopt: list torrents", "err", err)
		return
	}
	if len(ts) == 0 {
		return
	}
	// Failed-but-unplaced qBit grabs whose torrent is in fact still present and
	// healthy: a transient qBit error past qbitErrorGrace false-failed them,
	// and Active() then stopped polling them, so a torrent that recovered to
	// downloading/seeding strands. Looked up once and revived in the loop below
	// — keyed by info-hash, the same id qBit's list carries. Best-effort: a
	// query error just skips recovery this pass.
	recoverable, rerr := p.repo.Recoverable(ctx, "qbit")
	if rerr != nil {
		p.log.Warn("adopt: recoverable qbit", "err", rerr)
		recoverable = nil
	}
	now := time.Now().Unix()
	graceSecs := int64(minAge / time.Second)
	for i := range ts {
		t := &ts[i]
		if t.Hash == "" {
			continue
		}
		// Revive a false-failed grab before the known-hash skip below: a
		// recoverable grab's hash IS known (it has a client_id), so the skip
		// would otherwise strand its recovered download forever. Only revive
		// when the torrent is actually healthy again; a still-errored one is
		// left failed for a later pass.
		if g, ok := recoverable[t.Hash]; ok {
			switch classifyQbitState(t.State) {
			case "downloading", "completed":
				p.reviveQbitGrab(ctx, g, t)
			}
			continue // known hash — never adopt a second copy of it
		}
		if known[t.Hash] {
			continue
		}
		// Give a UI-added torrent time to claim its own grab first. Counted
		// so the manual button can say "N too new" rather than "nothing new".
		if now-t.AddedOn < graceSecs {
			skippedRecent++
			continue
		}
		// Duplicate guard (see adoptSabOrphans): forager already has this exact
		// release, and the StashDB dedup can't catch non-StashDB content. Skip
		// re-adopting it so it isn't placed a second time. Unlike SAB we DON'T
		// delete the torrent — it may be seeding for ratio; leaving it untracked
		// under the forage category is harmless (it just won't be re-adopted).
		if dup, derr := p.repo.HasLiveGrabForRelease(ctx, t.Name); derr != nil {
			p.log.Warn("adopt: dup check", "name", t.Name, "err", derr)
		} else if dup {
			p.log.Info("adopt: skipping duplicate, release already grabbed", "name", t.Name, "hash", t.Hash)
			continue
		}
		kind, videos, ok := p.classifyTorrent(ctx, qb, t.Hash)
		if !ok {
			// The file list isn't available yet — a magnet still resolving
			// its metadata (or a transient API error). Kind is classified
			// exactly once, at adoption, and nothing re-evaluates it, so
			// guessing "single" here permanently routed packs down the
			// single-scene confirm path (one arbitrary scene matched, no
			// settle window, no batch identify, no dedup). Defer; a later
			// pass adopts once the metadata exists.
			p.log.Info("adopt: deferred, file list not available yet", "hash", t.Hash, "name", t.Name)
			continue
		}
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
	return
}

// sabAdoptWindow bounds SAB history adoption to recent completions. SAB's
// history is long-lived: without a window, a fresh forage database pointed
// at an existing SAB instance would mass-import every old forage-category
// job on the first sweep.
const sabAdoptWindow = 48 * time.Hour

// adoptSabOrphans is the SAB half of adoptOrphans: untracked COMPLETED
// forage-category history jobs become grab rows. Queue items are left
// alone — SAB exposes no file inventory before completion, so pack vs
// single can't be classified (kind is decided exactly once, at adoption),
// and usenet queues drain in minutes; the periodic sweep adopts the job
// right after it lands in history. No minAge: SAB's addurl returns the
// nzo_id synchronously, so forage's own grabs are known the moment they
// exist and can't be re-adopted.
func (p *Poller) adoptSabOrphans(ctx context.Context, known map[string]bool) int {
	sb := p.pool.Sab()
	if sb == nil {
		return 0
	}
	cat := p.pool.Settings().SabCategory
	if cat == "" {
		return 0 // never adopt uncategorised jobs
	}
	hist, err := sb.History(ctx, 200, cat)
	if err != nil {
		p.log.Warn("adopt: sab history", "err", err)
		return 0
	}
	// Failed-but-unplaced SAB grabs whose download SAB may have completed
	// after the not-found timeout wrongly failed them. Looked up once and
	// revived in the loop below — keyed by nzo, the same id SAB history
	// carries. Best-effort: a query error just skips recovery this pass.
	recoverable, rerr := p.repo.Recoverable(ctx, "sabnzbd")
	if rerr != nil {
		p.log.Warn("adopt: recoverable sab", "err", rerr)
		recoverable = nil
	}
	adopted := 0
	now := time.Now().Unix()
	for _, it := range hist {
		if it.NzoID == "" {
			continue
		}
		// Revive a false-failed grab before the known-nzo skip below: a
		// recoverable grab's nzo IS known (it has a client_id), so the skip
		// would otherwise strand its completed download forever.
		if g, ok := recoverable[it.NzoID]; ok {
			if strings.EqualFold(it.Status, "Completed") {
				p.reviveSabGrab(ctx, g, it)
			}
			continue // known nzo — never adopt a second copy of it
		}
		if known[it.NzoID] {
			continue
		}
		if !strings.EqualFold(it.Status, "Completed") || it.Path == "" {
			continue
		}
		if it.Completed > 0 && now-it.Completed > int64(sabAdoptWindow/time.Second) {
			continue
		}
		// Duplicate guard: forager already has (or is downloading) this exact
		// release. The StashDB-cross-id dedup can't catch non-StashDB content
		// (OnlyFans siterips, etc.), so a re-download under the forage category
		// would otherwise be placed as a second copy. Drop the redundant SAB
		// download (files included) and skip. A failed/orphaned prior attempt
		// is NOT "live", so a genuine re-grab still proceeds.
		if dup, derr := p.repo.HasLiveGrabForRelease(ctx, it.Name); derr != nil {
			p.log.Warn("adopt: dup check", "name", it.Name, "err", derr)
		} else if dup {
			p.log.Info("adopt: skipping duplicate, release already grabbed", "name", it.Name, "nzo_id", it.NzoID)
			if err := sb.DeleteHistory(ctx, it.NzoID, true); err != nil {
				p.log.Warn("adopt: remove duplicate sab download", "nzo_id", it.NzoID, "err", err)
			}
			continue
		}
		kind, videos, ok := classifyDownloadPath(it.Path)
		if !ok {
			// Storage path not visible from this container (mount gap or
			// the files were already cleaned up). Don't guess — kind is
			// classified once and a wrong "single" permanently routes a
			// pack down the wrong confirm path.
			p.log.Info("adopt: deferred, sab storage not visible", "nzo_id", it.NzoID, "name", it.Name, "path", it.Path)
			continue
		}
		packFiles := 0
		if kind == "pack" {
			packFiles = videos
		}
		grabbedAt := it.Completed
		if grabbedAt == 0 {
			grabbedAt = now
		}
		folder := suggest.ConfidentTopFolder(ctx, p.db, it.Name)
		id, err := p.repo.Insert(ctx, grabs.Grab{
			ReleaseTitle:  it.Name,
			Client:        "sabnzbd",
			ClientID:      it.NzoID,
			ClientName:    it.Name,
			Category:      cat,
			Status:        "queued",
			PerformerName: folder,
			Kind:          kind,
			PackFiles:     packFiles,
			GrabbedAt:     grabbedAt,
			Reason:        "adopted from sab",
		})
		if err != nil {
			p.log.Warn("adopt: insert", "nzo_id", it.NzoID, "name", it.Name, "err", err)
			continue
		}
		adopted++
		p.log.Info("adopted sab job", "id", id, "name", it.Name,
			"nzo_id", it.NzoID, "kind", kind, "videos", videos, "folder", folder)
	}
	return adopted
}

// applyRevive persists a recovered grab: it clears the grace clock and the
// prior PlaceError, writes the row through the optimistic-lock CAS (a
// concurrent sweep/tick write is benign — skip it), and logs the outcome. The
// caller sets the client-specific fields (status, completion, name, reason) on
// g first; logKV carries client-specific log context. Best-effort: a stale or
// failed write just leaves the grab failed for a later pass to retry.
func (p *Poller) applyRevive(ctx context.Context, g grabs.Grab, logKV ...any) {
	g.PlaceError = ""
	p.graceClear(g.ID)
	err := p.repo.Update(ctx, g)
	switch {
	case errors.Is(err, grabs.ErrStaleUpdate):
		p.log.Info("revive grab: changed under sweep, skipping", "id", g.ID)
	case err != nil:
		p.log.Warn("revive grab", append([]any{"id", g.ID, "err", err}, logKV...)...)
	default:
		p.log.Info("revived false-failed grab", append([]any{"id", g.ID}, logKV...)...)
	}
}

// reviveSabGrab flips a false-failed SAB grab back to "completed" once SAB's
// history shows its nzo Completed, so the normal place → scan → confirm
// pipeline (re-)runs on the file SAB already downloaded. Restamps completion
// from SAB's timestamp and refreshes ClientName from the final on-disk path,
// since the failed grab may still carry SAB's "Trying to fetch NZB from…"
// placeholder name.
func (p *Poller) reviveSabGrab(ctx context.Context, g grabs.Grab, it sabnzbd.Item) {
	g.Status = "completed"
	g.Reason = "recovered: sab completed this nzo after a spurious not-found failure"
	if it.Completed > 0 {
		g.CompletedAt = it.Completed
	} else if g.CompletedAt == 0 {
		g.CompletedAt = time.Now().Unix()
	}
	if it.Path != "" {
		if base := pathmap.Base(it.Path); base != "" {
			g.ClientName = base
		}
	}
	p.applyRevive(ctx, g, "client", "sabnzbd", "nzo_id", it.NzoID, "name", g.ClientName)
}

// reviveQbitGrab flips a false-failed qBit grab back into the live pipeline
// once its torrent is healthy again, so the normal place → scan → confirm
// steps (re-)run on what qBit downloaded. Maps the current qBit state to
// downloading or completed (progress is the authority for completion, exactly
// as advanceQbit gates it) and refreshes ClientName from qBit's current
// torrent name. The caller only invokes this for a torrent already classified
// healthy.
func (p *Poller) reviveQbitGrab(ctx context.Context, g grabs.Grab, t *qbit.Torrent) {
	ns := classifyQbitState(t.State)
	if ns == "completed" && t.Progress < 1 {
		ns = "downloading"
	}
	g.Status = ns
	g.Reason = "recovered: qBit torrent healthy again after a transient error"
	if ns == "completed" && g.CompletedAt == 0 {
		g.CompletedAt = time.Now().Unix()
	}
	if t.Name != "" {
		g.ClientName = t.Name
	}
	p.applyRevive(ctx, g, "client", "qbit", "hash", g.ClientID, "state", t.State, "status", ns)
}

// classifyDownloadPath decides pack vs single for an on-disk completed
// download (SAB's history storage path): a lone file is a single; a
// directory is classified by its video count, mirroring classifyTorrent.
// ok is false when the path can't be inspected from this container.
func classifyDownloadPath(path string) (kind string, videos int, ok bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", 0, false
	}
	if !fi.IsDir() {
		if torrentmeta.IsVideo(fi.Name()) {
			videos = 1
		}
		return "single", videos, true
	}
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && torrentmeta.IsVideo(d.Name()) {
			videos++
		}
		return nil
	})
	if videos >= adoptMinVideos {
		return "pack", videos, true
	}
	return "single", videos, true
}

// classifyTorrent counts a torrent's video files via qBit's metainfo file
// list (available regardless of download progress) to decide pack vs
// single. ok is false when the file list isn't available yet (a magnet
// whose metadata hasn't resolved, or a transient API error) — callers
// must not classify then, since kind is decided once and never revisited.
func (p *Poller) classifyTorrent(ctx context.Context, qb *qbit.Client, hash string) (kind string, videos int, ok bool) {
	files, err := qb.TorrentFiles(ctx, hash)
	if err != nil || len(files) == 0 {
		return "", 0, false
	}
	for _, f := range files {
		if torrentmeta.IsVideo(f.Name) {
			videos++
		}
	}
	if videos >= adoptMinVideos {
		return "pack", videos, true
	}
	return "single", videos, true
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
		// No nzo_id to look up: the async addurl never linked — the daemon
		// died between the row insert and AddURL returning, or the nzo-link
		// write failed. Nothing in SAB matches this row and retryGrab only
		// accepts failed grabs, so without a timeout it polls forever as an
		// undeletable zombie. Mirror the qBit link timeout, skipping while
		// the add is still pending in-process (fetch-gate queue / backoff).
		if g.Status == "queued" && g.GrabbedAt > 0 &&
			time.Since(time.Unix(g.GrabbedAt, 0)) > qbitLinkTimeout &&
			!p.pending.Has(g.ID) {
			g.Status = "failed"
			g.Reason = "never linked to a SAB nzo (add likely failed)"
			return true, "", nil
		}
		return false, "", nil
	}
	if item := findByNzo(queue, g.ClientID); item != nil {
		p.graceClear(g.ID) // positive contact: nzo seen, reset the absence clock
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
		p.graceClear(g.ID) // positive contact: nzo seen, reset the absence clock
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
		// Fail only after contact has been LOST for sabInflightTimeout,
		// measured from the last time we saw the nzo in SAB (or first noticed
		// it missing) — not from grab time. SAB holds a job invisibly while it
		// fetches the NZB from the indexer and while it sits queued behind
		// others; those waits routinely exceed the old 5-minute grab-time
		// grace, so that grace false-failed slow-but-fine downloads that SAB
		// went on to complete 30+ minutes later. A genuinely removed job stays
		// absent and still fails here; a slow one re-appears (queue or, on
		// completion, history) and graceClear resets the clock. The adopt
		// sweep's revive path recovers anything this still gets wrong.
		if !p.graceElapsed(g.ID, sabInflightTimeout) {
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

// triggerPlacementScan asks Stash to scan the placed path itself, so the
// placed → confirmed transition takes minutes rather than waiting on
// Stash's scheduled scan. Records the attempt time so the confirmation
// step can throttle retries. Best-effort.
//
// The scan is scoped to exactly what was placed, at the grab's natural
// granularity: a single grab's placedPath is the file (or its own download
// folder), a pack's placedPath is the pack directory. Stash's metadataScan
// accepts a file path and scans just that file, so we no longer re-walk the
// whole containing performer/Unsorted folder (with its siblings + screenshot
// subfolders) on every retry — that per-grab over-scan is what let the job
// queue balloon under a Stash slowdown. A directory placedPath (packs) still
// scans recursively, so pack coverage counting is unchanged.
//
// The path is mapped Stash-side via FORAGER_STASH_PATH_MAPPING. If it can't
// be mapped (no mapping configured, or a stale prefix left by a mount
// rename), we deliberately skip the scan instead of falling back to a
// full-library scan — re-scanning the whole library per grab is far too
// expensive, and the grab still confirms via basename lookup once Stash's
// next scheduled scan indexes the file.
func (p *Poller) triggerPlacementScan(ctx context.Context, sc *stash.Client, grabID int64, placedPath string) {
	stashSidePath := pathmap.Translate(placedPath, p.pool.Settings().StashPathMapping)
	// Stamp the throttle even when we skip, so an unmappable grab doesn't
	// re-enter this path on every tick.
	p.scanMu.Lock()
	p.lastScan[grabID] = time.Now()
	p.scanMu.Unlock()
	if stashSidePath == "" {
		p.log.Info("metadataScan skipped: placed path not mappable to a Stash path; awaiting scheduled scan",
			"id", grabID, "placed", placedPath)
		return
	}
	if jobID, err := sc.MetadataScan(ctx, []string{stashSidePath}); err != nil {
		p.log.Warn("metadataScan trigger failed", "id", grabID, "path", stashSidePath, "err", err)
	} else {
		p.log.Info("metadataScan triggered", "id", grabID, "path", stashSidePath, "job_id", jobID)
		// Record it so the throttled re-scan (scanInFlight) won't stack a
		// duplicate while this one is still queued behind a busy Stash.
		p.rememberScanJob(grabID, jobID)
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

// triggerGenerate fires the deferred preview/sprite generation for a grab's
// scene(s) once it has settled. The placement scan only makes cover + phash
// (kept fast so identify isn't starved behind it); this produces the slow
// artifacts the user expects on the card afterwards, AFTER identify so it never
// re-blocks the queue. Best-effort: a failure just leaves the artifacts for
// Stash's scheduled Generate.
func (p *Poller) triggerGenerate(ctx context.Context, sc *stash.Client, grabID int64, sceneIDs []string) {
	if sc == nil || len(sceneIDs) == 0 {
		return
	}
	if jobID, err := sc.MetadataGenerate(ctx, sceneIDs); err != nil {
		p.log.Warn("metadataGenerate trigger failed", "id", grabID, "scenes", len(sceneIDs), "err", err)
	} else if jobID != "" {
		p.log.Info("metadataGenerate triggered", "id", grabID, "scenes", len(sceneIDs), "job_id", jobID)
	}
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
