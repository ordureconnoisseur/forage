package api

import (
	"context"
	"time"

	"github.com/ordureconnoisseur/forager/internal/scoring"
	"github.com/ordureconnoisseur/forager/internal/watches"
)

// Watch loop — the background re-search that powers the watchlist. Each
// tick it claims a batch of the least-recently-checked watches and
// re-searches them; on a VERIFIED release matching the watch's target
// resolution it flips the watch to available (recording the release for
// one-click grab). It NEVER grabs — that's the whole point, the user
// decides. Notify-only.

const (
	// watchTickInterval is how often the loop wakes. The batch size is
	// auto-sized so ALL watching rows are re-checked ~once per 24h, spread
	// across these ticks (so the watchlist growing doesn't hammer
	// Prowlarr — same rate-limit discipline as the collection search).
	watchTickInterval = 30 * time.Minute
	// watchFullCycle is the target time to re-check every watch once.
	watchFullCycle = 24 * time.Hour
	// watchMinBatch / watchMaxBatch bound the auto-sized batch so a tiny
	// list still progresses and a huge one can't spike Prowlarr.
	watchMinBatch = 1
	watchMaxBatch = 8
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
			s.watchTick(ctx)
		}
	}
}

// watchTick claims and re-checks one auto-sized batch.
func (s *Server) watchTick(ctx context.Context) {
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

// watchBatchSize spreads the whole watchlist over watchFullCycle: with
// ticksPerCycle ticks in 24h, each tick should cover total/ticks rows
// (rounded up), clamped to [min,max].
func (s *Server) watchBatchSize(total int) int {
	ticksPerCycle := int(watchFullCycle / watchTickInterval) // 48
	if ticksPerCycle < 1 {
		ticksPerCycle = 1
	}
	n := (total + ticksPerCycle - 1) / ticksPerCycle
	if n < watchMinBatch {
		n = watchMinBatch
	}
	if n > watchMaxBatch {
		n = watchMaxBatch
	}
	return n
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
	releases, err := s.searchSceneReleases(ctx, pc, scene, perfNames, s.pool.Settings().ProwlarrCategories, true /*lean*/)
	if err != nil || len(releases) == 0 {
		return
	}
	cands := s.verifyReleases(ctx, m, w.StashDBID, scene.Title, releases)

	// First verified, non-rejected release whose resolution matches the
	// target (exact; "any" accepts anything). Best-scoring first so the
	// recorded release is the nicest qualifying one.
	best := s.bestWatchMatch(cands, w.Target)
	if best == nil {
		return
	}
	if err := s.watches.MarkAvailable(ctx, w.StashDBID,
		best.Title, best.DownloadURL, best.Indexer, best.Protocol, best.Size); err != nil {
		s.log.Warn("watch mark available", "scene", w.StashDBID, "err", err)
		return
	}
	s.log.Info("watch available", "scene", w.StashDBID, "title", scene.Title,
		"target", w.Target, "release", best.Title)
}

// bestWatchMatch returns the best verified, non-rejected release matching
// the target resolution, or nil. Candidates are score-ranked first so the
// chosen release is the highest-scoring qualifier.
func (s *Server) bestWatchMatch(cands []sceneRelease, target string) *sceneRelease {
	// Rank like the releases endpoint: score desc (verified/non-rejected
	// filtered below).
	bestIdx := -1
	bestScore := 0
	for i := range cands {
		c := &cands[i]
		if !c.Verified || c.Rejected {
			continue
		}
		if !resolutionMatches(target, scoring.Resolution(c.Title)) {
			continue
		}
		if bestIdx == -1 || c.Score > bestScore {
			bestIdx = i
			bestScore = c.Score
		}
	}
	if bestIdx == -1 {
		return nil
	}
	return &cands[bestIdx]
}

// resolutionMatches reports whether a release's resolution satisfies the
// watch target. "any" accepts anything; otherwise it's an EXACT tier
// match (a 4k release does NOT satisfy a 1080p watch, per the design).
func resolutionMatches(target, releaseRes string) bool {
	if target == "" || target == watches.TargetAny {
		return true
	}
	return target == releaseRes
}
