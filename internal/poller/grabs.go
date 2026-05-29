// Package poller is the Phase B background loop. It watches qBit for
// completion of forager-tracked grabs, then watches Stash for the
// corresponding scene to surface, and records actual-vs-predicted
// StashDB IDs on each grab.
//
// The state machine lives here. internal/grabs.Repo is just storage.
package poller

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ordureconnoisseur/forager/internal/clientpool"
	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/qbit"
	"github.com/ordureconnoisseur/forager/internal/sabnzbd"
	"github.com/ordureconnoisseur/forager/internal/stash"
)

// Poller advances grab state machines on a fixed interval. Holds a
// *clientpool.Pool rather than individual clients so /config saves
// reach the next tick automatically — every call into a client goes
// through the pool's atomic accessor.
type Poller struct {
	repo     *grabs.Repo
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

	// lastScan throttles per-grab metadataScan retries. The initial
	// post-placement scan can be coalesced by Stash with a concurrent
	// one (e.g. several grabs placed into the same folder in one tick)
	// and miss the file, leaving the grab stuck at "placed". The
	// confirmation step re-triggers the scan on a throttle until Stash
	// indexes the file.
	scanMu   sync.Mutex
	lastScan map[int64]time.Time
}

// scanRetryInterval is the minimum gap between metadataScan retries
// for a single stuck grab. Short enough to recover quickly, long
// enough not to spam Stash every poll tick.
const scanRetryInterval = 90 * time.Second

func New(repo *grabs.Repo, pool *clientpool.Pool, log *slog.Logger, interval, orphanAfter time.Duration) *Poller {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	if orphanAfter <= 0 {
		orphanAfter = 6 * time.Hour
	}
	return &Poller{
		repo:     repo,
		pool:     pool,
		log:      log,
		interval: interval,
		orphan:   orphanAfter,
		lastScan: map[int64]time.Time{},
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
// look at qBit's recent additions (filtered by our category) and
// match against the grab's title-token signature + add-time window.
//
// Step 2 — Refresh qBit state for any tracked grab. Status updates:
// downloading | completed | failed (when qBit no longer knows about it).
//
// Step 3 — For completed grabs without an actual_stashdb_id yet,
// query Stash by filename. If Stash has indexed the file and has a
// StashDB cross-id for it, set actual_stashdb_id and transition to
// confirmed (matches prediction) or mismatched (doesn't). If still
// not in Stash after `orphan_after`, mark orphaned.
func (p *Poller) tickOnce(ctx context.Context) error {
	t0 := time.Now()
	active, err := p.repo.Active(ctx)
	if err != nil {
		return err
	}
	if len(active) == 0 {
		return nil
	}

	// Step 1: enrich qBit grabs without client_ids (qBit's add API
	// doesn't return the info_hash; we match by recent-additions).
	// SAB grabs already have client_id set synchronously at /grab time.
	needsQbitEnrichment := false
	for _, g := range active {
		if g.Client == "qbit" && g.ClientID == "" {
			needsQbitEnrichment = true
			break
		}
	}
	var recentQbit []qbit.Torrent
	if qb := p.pool.Qbit(); needsQbitEnrichment && qb != nil {
		recentQbit, err = qb.ListTorrents(ctx, qbit.ListOpts{
			Filter: "all", Sort: "added_on", Reverse: true, Limit: 50,
		})
		if err != nil {
			p.log.Warn("list torrents for enrichment", "err", err)
		}
	}

	// Pre-fetch SAB queue + history once per tick if we have any SAB
	// grabs to advance. Both endpoints are cheap; one request each
	// covers an unbounded number of active SAB grabs.
	var sabQueue, sabHistory []sabnzbd.Item
	hasSabActive := false
	for _, g := range active {
		if g.Client == "sabnzbd" {
			hasSabActive = true
			break
		}
	}
	if sb := p.pool.Sab(); hasSabActive && sb != nil {
		sabQueue, err = sb.Queue(ctx)
		if err != nil {
			p.log.Warn("sab queue fetch", "err", err)
		}
		// Fetch a deep history slice: SAB history mixes forager
		// downloads with everything else the user grabs, so a
		// forager item can be buried fast. Too small a window and a
		// legitimately-completed grab scrolls out before the poller
		// sees it land, leaving it stuck mid-pipeline.
		sabHistory, err = sb.History(ctx, 200)
		if err != nil {
			p.log.Warn("sab history fetch", "err", err)
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
		if err := p.advance(ctx, &active[i], recentQbit, claimed, sabQueue, sabHistory); err != nil {
			p.log.Warn("advance grab", "id", active[i].ID, "err", err)
		}
	}

	p.log.Debug("tick done", "active", len(active), "elapsed", time.Since(t0))
	return nil
}

func (p *Poller) advance(ctx context.Context, g *grabs.Grab, recentForEnrichment []qbit.Torrent, claimed map[string]bool, sabQueue, sabHistory []sabnzbd.Item) error {
	dirty := false
	// srcPath is the live full filesystem path the client reports for
	// this grab — qBit's ContentPath, SAB's history Path. Used by the
	// place step below. Empty when unknown (still queued, or client
	// no longer tracks it).
	var srcPath string

	// ── Steps 1 + 2 — client-specific enrichment + state refresh.
	switch g.Client {
	case "qbit":
		d, sp, err := p.advanceQbit(ctx, g, recentForEnrichment, claimed)
		if err != nil {
			return err
		}
		dirty = dirty || d
		srcPath = sp
	case "sabnzbd":
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
	if g.Status == "completed" && g.PlacedPath == "" && pl.Configured() && srcPath != "" {
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
			needle = baseName(g.PlacedPath)
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
				// Stash has the file but no StashDB cross-id yet —
				// scan completed, identify hasn't run. Distinct from
				// orphaned (Stash never saw it at all).
				if g.Status != "scanned" {
					g.Status = "scanned"
					g.Reason = "in Stash, awaiting identify"
					dirty = true
					// Kick Stash's Identify task once on the transition.
					// Best-effort: if it fails (no stash-box configured,
					// network blip), the user can still trigger
					// identify manually from Stash's UI and the next
					// poll will detect the resulting stash_id.
					if jobID, err := p.triggerIdentify(ctx, stashC, scene.ID); err != nil {
						p.log.Warn("metadataIdentify trigger failed", "id", g.ID, "scene_id", scene.ID, "err", err)
					} else if jobID != "" {
						p.log.Info("metadataIdentify triggered", "id", g.ID, "scene_id", scene.ID, "job_id", jobID)
					}
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
		return p.repo.Update(ctx, *g)
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
	needle := translateStashPath(g.PlacedPath, p.pool.Settings().StashPathMapping)
	if needle == "" {
		needle = baseName(g.PlacedPath)
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
	// Backfill the expected count from what Stash sees when we never got
	// one at grab time (manual grab, magnet pack). Never lower a real
	// expected — Stash may report fewer mid-scan.
	if g.PackFiles == 0 && found > 0 {
		g.PackFiles = found
		dirty = true
	}

	// Nothing indexed yet — the post-placement scan can miss a large new
	// directory. Re-fire it, throttled, until scenes appear.
	if found == 0 {
		if g.Status == "placed" && p.scanThrottleElapsed(g.ID) {
			p.triggerPlacementScan(ctx, sc, g.ID, g.PlacedPath)
		}
		return dirty, nil
	}

	// Stash has at least part of the pack — leave the queued/placed state.
	if g.Status == "placed" {
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

	// Completion: everything found is identified and we've reached the
	// expected count (when known); or the orphan window elapsed and we
	// confirm with whatever's identified.
	expected := g.PackFiles
	allIdentified := len(toIdentify) == 0 && (expected == 0 || found >= expected)
	gaveUp := g.CompletedAt > 0 && time.Since(time.Unix(g.CompletedAt, 0)) > p.orphan
	if g.Status == "scanned" && (allIdentified || gaveUp) {
		// Download-then-dedup: now that the pack's scenes are identified,
		// drop any whose StashDB id the library already had outside this
		// pack (keeps the existing copy; the pack copy is the redundant
		// one). Best-effort — a dedup failure still lets the pack confirm.
		if deduped, err := p.dedupPack(ctx, sc, g, scenes, needle); err != nil {
			p.log.Warn("pack dedup failed", "id", g.ID, "err", err)
		} else if deduped > 0 {
			g.PackDeduped += deduped
		}
		g.Status = "confirmed"
		g.ConfirmedAt = time.Now().Unix()
		g.Reason = fmt.Sprintf("pack: %d/%d scenes identified, %d dup removed", identified, found, g.PackDeduped)
		dirty = true
		p.log.Info("pack confirmed", "id", g.ID, "identified", identified, "found", found, "expected", expected, "deduped", g.PackDeduped)
	}
	return dirty, nil
}

// dedupPack removes pack scenes the library already has elsewhere. For
// each identified pack scene it checks whether the same StashDB id sits
// on a file outside the pack directory; if so, that pack file is a
// duplicate and is destroyed (file + generated assets). One library
// sweep backs the whole pass. Returns how many scenes were removed.
//
// Keeps the pre-existing copy by design — swapping in a higher-quality
// pack copy is a later refinement. The torrent keeps seeding (the
// download client's copy is a separate hardlink); only the library
// duplicate is removed.
func (p *Poller) dedupPack(ctx context.Context, sc *stash.Client, g *grabs.Grab, packScenes []stash.SceneMatch, packNeedle string) (int, error) {
	libByID, err := sc.FindAllSceneStashDBIDs(ctx)
	if err != nil {
		return 0, err
	}
	deduped := 0
	for _, ps := range packScenes {
		if ps.StashDBID == "" {
			continue
		}
		external := false
		for _, pth := range libByID[ps.StashDBID] {
			if !strings.Contains(pth, packNeedle) {
				external = true
				break
			}
		}
		if !external {
			continue // unique to this pack — keep it
		}
		if err := sc.SceneDestroy(ctx, ps.ID, true, true); err != nil {
			p.log.Warn("pack dedup destroy", "id", g.ID, "scene", ps.ID, "err", err)
			continue
		}
		deduped++
		p.log.Info("pack dedup removed duplicate", "id", g.ID, "scene", ps.ID, "stashdb", ps.StashDBID)
	}
	return deduped, nil
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
// steps. Returns (dirty, contentPath). contentPath is qBit's full
// filesystem path for the torrent — passed to the placer when status
// flips to "completed".
func (p *Poller) advanceQbit(ctx context.Context, g *grabs.Grab, recent []qbit.Torrent, claimed map[string]bool) (bool, string, error) {
	dirty := false
	qb := p.pool.Qbit()
	if qb == nil {
		return false, "", nil
	}
	// Link the info_hash if we don't have it yet (qBit doesn't return
	// it from /torrents/add).
	if g.ClientID == "" {
		if t := pickRecent(recent, g, claimed); t != nil {
			g.ClientID = t.Hash
			g.ClientName = t.Name
			g.Reason = "enriched from qBit recent-additions"
			claimed[t.Hash] = true
			dirty = true
		}
	}
	if g.ClientID == "" {
		return dirty, "", nil
	}
	t, err := qb.TorrentInfo(ctx, g.ClientID)
	if err != nil {
		return dirty, "", err
	}
	if t == nil {
		if g.Status != "failed" {
			g.Status = "failed"
			g.Reason = "qbit no longer tracks this torrent"
			dirty = true
		}
		return dirty, "", nil
	}
	if t.Name != "" && t.Name != g.ClientName {
		g.ClientName = t.Name
		dirty = true
	}
	newStatus := classifyQbitState(t.State)
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
	return dirty, t.ContentPath, nil
}

// sabRegisterGrace is how long after grabbing we tolerate a SAB
// nzo_id being absent from both queue and history before declaring
// the grab failed. SAB returns the nzo_id synchronously on add but
// takes a few seconds to surface the job in its queue, and the
// poller can tick within that window. Generous on purpose: a stuck
// "queued" grab is far less harmful than a false "failed" on a
// download that's actually in flight.
const sabRegisterGrace = 5 * time.Minute

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
			name = baseName(item.Path)
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

func findByNzo(items []sabnzbd.Item, nzoID string) *sabnzbd.Item {
	for i := range items {
		if items[i].NzoID == nzoID {
			return &items[i]
		}
	}
	return nil
}

func baseName(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[i+1:]
		}
	}
	return path
}

// translateStashPath rewrites a forager-side filesystem path into the
// path Stash uses for the same file, so a scoped metadataScan call
// hits actual files. Mapping is "<forager-prefix>:<stash-prefix>";
// e.g. "/data/media/Media:Z:\\Media" turns
// "/data/media/Media/Hazel Moore/scene.mp4" into
// "Z:\\Media\\Hazel Moore\\scene.mp4".
//
// Returns empty when:
//   - mapping is unset (caller falls back to full-library scan)
//   - the prefix doesn't match (path is outside the configured mount)
//
// Path separators are normalised — forager's container view uses '/',
// Stash on Windows uses '\'. We detect Windows-style targets and flip
// any '/' characters after the prefix to '\'.
func translateStashPath(foragerPath, mapping string) string {
	if mapping == "" || foragerPath == "" {
		return ""
	}
	idx := strings.Index(mapping, ":")
	// Find the LAST colon-pair boundary — Windows targets contain
	// embedded colons (Z:\Media). We look for the first ":" only when
	// it's followed by something that doesn't look like a drive letter.
	// Practically: split on the first colon, accept any prefix on the
	// left, and treat the rest as the stash-side target verbatim.
	if idx <= 0 || idx == len(mapping)-1 {
		return ""
	}
	foragerPrefix := mapping[:idx]
	stashPrefix := mapping[idx+1:]
	if !strings.HasPrefix(foragerPath, foragerPrefix) {
		return ""
	}
	suffix := foragerPath[len(foragerPrefix):]
	if strings.ContainsRune(stashPrefix, '\\') {
		// Windows-style target — flip forward slashes in the suffix
		// to backslashes so the joined path is well-formed.
		suffix = strings.ReplaceAll(suffix, "/", `\`)
	}
	return stashPrefix + suffix
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
	stashSidePath := translateStashPath(filepath.Dir(placedPath), p.pool.Settings().StashPathMapping)
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
	case "downloading", "stalledDL", "metaDL", "queuedDL", "checkingDL", "forcedDL", "allocating":
		return "downloading"
	case "uploading", "stalledUP", "queuedUP", "checkingUP", "forcedUP", "pausedUP":
		return "completed"
	case "missingFiles", "error":
		return "failed"
	}
	// "completed" and other terminal-success states.
	return "completed"
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
	windowEnd := time.Now().Unix() + 120
	var best *qbit.Torrent
	bestDelta := int64(1<<62 - 1)
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
		delta := t.AddedOn - g.GrabbedAt
		if delta < 0 {
			delta = -delta
		}
		if delta < bestDelta {
			bestDelta = delta
			best = t
		}
	}
	return best
}
