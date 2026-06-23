package api

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/ordureconnoisseur/forager/internal/watches"
)

// Watch loop — the background re-search that powers the watchlist. Each
// tick it claims a batch of the least-recently-checked watches and
// re-searches them; on a VERIFIED release matching the watch's target
// resolution it flips the watch to available (recording the release for
// one-click grab). It NEVER grabs — that's the whole point, the user
// decides. Notify-only.

const (
	// watchTickInterval is how often the loop wakes.
	watchTickInterval = 30 * time.Minute
	// watchMaxBatch caps how many watches are re-searched per tick. A lean
	// search is only ~2 Prowlarr queries, so checking up to this many every
	// 30 min is trivial load — small watchlists get re-checked every tick
	// (responsive), and only a large list spreads across ticks (each scene
	// every ceil(total/8) ticks) to keep Prowlarr load bounded.
	watchMaxBatch = 8
	// watchSearchTimeout caps how long one watch's release search may run
	// before a slow indexer is abandoned and the scene is judged on the fast
	// indexers' results. Bounds per-scene latency so "search all" stays usable.
	watchSearchTimeout = 25 * time.Second
)

// RunWatchLoop drives the watchlist re-search until ctx is cancelled.
// Started as a goroutine from main; short-circuits when Prowlarr/StashDB
// aren't configured.
func (s *Server) RunWatchLoop(ctx context.Context) {
	t := time.NewTicker(watchTickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// One panicking tick (release-data-driven matching inside
			// checkWatch) must not kill the daemon or the loop.
			func() {
				defer s.recoverPanic("watch loop tick")
				s.watchTick(ctx)
			}()
		}
	}
}

// watchTick claims and re-checks one auto-sized batch.
func (s *Server) watchTick(ctx context.Context) {
	// First drop any watch whose scene has since been grabbed (by any path),
	// so the Watching tab self-cleans even between visits. Independent of
	// Prowlarr/StashDB — runs even when re-search can't.
	s.reconcileWatches(ctx)
	if s.pool.Prowlarr() == nil || s.pool.StashDB() == nil {
		return
	}
	total, err := s.watches.CountWatching(ctx)
	if err != nil || total == 0 {
		return
	}
	batch := s.watchBatchSize(total)
	claimed, err := s.watches.ClaimBatch(ctx, batch)
	if err != nil {
		s.log.Warn("watch claim", "err", err)
		return
	}
	for _, w := range claimed {
		if ctx.Err() != nil {
			return
		}
		s.checkWatch(ctx, w)
	}
}

// watchBatchSize returns how many watches to re-check this tick: the whole
// list while it's small (≤ watchMaxBatch — checked every tick), capped at
// watchMaxBatch once it grows (then each scene is covered every
// ceil(total/watchMaxBatch) ticks).
func (s *Server) watchBatchSize(total int) int {
	if total > watchMaxBatch {
		return watchMaxBatch
	}
	return total
}

// checkWatch re-searches one watch and, on a verified release matching the
// target resolution, flips it to available. last_checked was already
// stamped by ClaimBatch.
func (s *Server) checkWatch(ctx context.Context, w watches.Watch) {
	m, err := s.Matcher(ctx)
	if err != nil {
		return
	}
	stashDBC := s.pool.StashDB()
	pc := s.pool.Prowlarr()
	if stashDBC == nil || pc == nil {
		return
	}
	scene, err := stashDBC.FindScene(ctx, w.StashDBID)
	if err != nil || scene == nil {
		return
	}
	// Backfill any display metadata the watch is missing (e.g. it was
	// added bare via the API) from the scene we just resolved — so the
	// Watching tab can show a thumbnail/title even for non-card adds.
	if w.ImageURL == "" || w.Title == "" || w.StudioName == "" {
		img := ""
		if len(scene.Images) > 0 {
			img = scene.Images[0].URL
		}
		studio := ""
		if scene.Studio != nil {
			studio = scene.Studio.Name
		}
		if berr := s.watches.BackfillMeta(ctx, w.StashDBID, scene.Title, scene.Date, studio, img); berr != nil {
			s.log.Warn("watch backfill meta", "scene", w.StashDBID, "err", berr)
		}
	}
	perfNames := s.scenePerformerNames(ctx, scene, w.PerformerName, "")
	// Cap each scene's search so one slow indexer can't stall it. The full
	// search fans out several queries and waits for all of them; a single slow
	// indexer (near the 60s Prowlarr client timeout) otherwise pins a scene at
	// ~1-2 min, making a "search all" take 20-30 min. With a tighter deadline
	// the slow query is cancelled and the scene is judged on whatever the fast
	// indexers returned (partial results still verify fine).
	sctx, cancel := context.WithTimeout(ctx, watchSearchTimeout)
	defer cancel()
	// Use the FULL (non-lean) search. The lean 2-query set (primary performer
	// + studio, and the title) systematically misses studio releases named by
	// date + a NON-primary performer (e.g. a "Slim Poke + Cyber Doll" scene
	// titled "Wild Open House" whose release is "BlacksOnBlondes.26.06.19.
	// Cyber.Doll.XXX.1080p" — caught only by a bare "Cyber Doll" query). lean
	// existed for the collection fan-out's many-scenes-at-once load; the watch
	// loop processes scenes sequentially (and search-now is bounded), so it can
	// afford the complete query set — and needs it to actually find releases.
	releases, err := s.searchSceneReleases(sctx, pc, scene, perfNames, s.pool.Settings().ProwlarrCategories, false /*full*/)
	if err != nil || len(releases) == 0 {
		return
	}
	cands := s.verifyReleases(ctx, m, w.StashDBID, scene.Title, releases)

	// The best release to surface — top of the user's preference ranking,
	// whatever its resolution (no quality target; the stored candidate list
	// lets the user pick a different one, and quality floors live in the
	// release reject rules the scorer already applies).
	best := s.bestWatchMatch(cands, w.IgnoredURLs)
	if best == nil {
		return
	}
	// Store the verified candidate list alongside the best pick so the
	// Watching tab can offer a re-pick when the auto-chosen release isn't what
	// the user wants (the #1 watch pain). Capped to keep the row small.
	if err := s.watches.MarkAvailable(ctx, w.StashDBID,
		best.Title, best.DownloadURL, best.Indexer, best.Protocol, best.Size,
		watchCandidatesJSON(cands)); err != nil {
		s.log.Warn("watch mark available", "scene", w.StashDBID, "err", err)
		return
	}
	s.log.Info("watch available", "scene", w.StashDBID, "title", scene.Title,
		"release", best.Title)
}

// watchCandidateCap bounds how many candidates a watch stores (verified,
// non-rejected, best-scoring first) so an available watch's row stays small.
const watchCandidateCap = 25

// watchCandidatesJSON marshals the verified, non-rejected releases (all
// resolutions, best score first) for storage on the watch — the re-pick list.
// All resolutions are kept (not just the target tier) so the user can pick a
// higher- or lower-res release than the one the target auto-selected.
func watchCandidatesJSON(cands []sceneRelease) json.RawMessage {
	picks := make([]sceneRelease, 0, len(cands))
	for _, c := range cands {
		if c.Verified && !c.Rejected {
			picks = append(picks, c)
		}
	}
	sort.SliceStable(picks, func(i, j int) bool { return picks[i].Score > picks[j].Score })
	if len(picks) > watchCandidateCap {
		picks = picks[:watchCandidateCap]
	}
	if len(picks) == 0 {
		return json.RawMessage("[]")
	}
	b, err := json.Marshal(picks)
	if err != nil {
		return json.RawMessage("[]")
	}
	return json.RawMessage(b)
}

// bestWatchMatch returns the single release to surface for a watch: the best
// VERIFIED, non-rejected, non-ignored release by the user's preference ranking
// — the same precedence the release list uses (grabbable first so a dead
// torrent can't win, then preference score, then seed health, then
// popularity). No resolution target: a watch surfaces the best available
// release whatever its quality, and the stored candidate list lets the user
// pick a different one. Quality FLOORS are release reject rules (Settings),
// which the scorer already enforces via Rejected.
func (s *Server) bestWatchMatch(cands []sceneRelease, ignored []string) *sceneRelease {
	ignoredSet := make(map[string]bool, len(ignored))
	for _, u := range ignored {
		ignoredSet[u] = true
	}
	bestIdx := -1
	for i := range cands {
		c := &cands[i]
		// Skip unverified/rejected, and releases the user dismissed for this
		// watch (a dead/over-compressed find must not re-surface).
		if !c.Verified || c.Rejected || ignoredSet[c.DownloadURL] {
			continue
		}
		if bestIdx == -1 || betterRelease(cands[i], cands[bestIdx]) {
			bestIdx = i
		}
	}
	if bestIdx == -1 {
		return nil
	}
	return &cands[bestIdx]
}

// betterRelease reports whether a should rank above b for auto-selection:
// grabbable first, then preference score, then seed health, then popularity —
// the precedence the release list sorts on.
func betterRelease(a, b sceneRelease) bool {
	if ga, gb := grabbable(a), grabbable(b); ga != gb {
		return ga
	}
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	if sa, sb := seedTier(a), seedTier(b); sa != sb {
		return sa > sb
	}
	return a.Popularity > b.Popularity
}
