package api

import (
	"context"
	"strings"
	"time"

	"github.com/ordureconnoisseur/forager/internal/matcher"
	"github.com/ordureconnoisseur/forager/internal/prowlarr"
	"github.com/ordureconnoisseur/forager/internal/watches"
)

// RSS-sync loop — the cheap, forward-looking complement to the per-scene watch
// search. Each tick pulls every indexer's recent-uploads feed in ONE aggregate
// query-less Prowlarr call, drops releases that name no watched performer with
// a free LOCAL scan, confirms the survivors against the watchlist with the
// existing matcher, and — on a hit — triggers the normal per-scene checkWatch.
//
// RSS only DETECTS that a release now exists for a watched scene; checkWatch
// then does the full search and picks the BEST release (not just the one RSS
// happened to see). It never grabs. The expensive search runs only when a
// release actually exists, instead of speculatively on every watched scene.

const (
	// rssTickInterval is how often the loop pulls + matches the recent feed.
	// RSS is cheap (one aggregate call + a local pre-filter), so it runs more
	// often than the 30-min search loop — that's how a freshly dropped release
	// for a watched scene gets noticed quickly.
	rssTickInterval = 20 * time.Minute
	// rssFetchTimeout bounds the single query-less RecentReleases call.
	rssFetchTimeout = 90 * time.Second
	// rssFirstRunLookback bounds an indexer's FIRST sync (no watermark yet):
	// only releases newer than this are considered, so the initial pass isn't
	// the indexer's entire recent window.
	rssFirstRunLookback = 14 * 24 * time.Hour
	// rssWatermarkOverlap re-scans a little behind the watermark each tick so an
	// upload that surfaces with a slightly older publishDate (indexer backfill /
	// clock skew) isn't skipped. Re-matching a seen release is harmless — a
	// scene already flipped to available isn't re-triggered.
	rssWatermarkOverlap = 6 * time.Hour
	// rssMaxTriggersPerTick caps how many watched scenes one tick kicks a full
	// search for, so a burst of matches can't monopolise Prowlarr. Overflow is
	// left for the next tick / the search loop and is logged (no silent cap).
	rssMaxTriggersPerTick = 25
)

// RunRSSLoop drives the RSS-sync watcher until ctx is cancelled. Started as a
// goroutine from main alongside RunWatchLoop; short-circuits when
// Prowlarr/StashDB aren't configured.
func (s *Server) RunRSSLoop(ctx context.Context) {
	t := time.NewTicker(rssTickInterval)
	defer t.Stop()
	// Boot-fire once: the watermark makes a restart cheap (only new-since-last
	// is processed) and it gives immediate coverage after a deploy.
	func() {
		defer s.recoverPanic("rss loop boot tick")
		s.rssTick(ctx)
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			func() {
				defer s.recoverPanic("rss loop tick")
				s.rssTick(ctx)
			}()
		}
	}
}

// rssTick runs one RSS sync: pull recent -> watermark filter -> local
// performer pre-filter -> match/verify -> trigger checkWatch -> advance marks.
func (s *Server) rssTick(ctx context.Context) {
	pc := s.pool.Prowlarr()
	if pc == nil || s.pool.StashDB() == nil {
		return
	}

	// Snapshot the watchlist: the watched-performer name set (for the cheap
	// local pre-filter) and the watching scenes by id (for match lookup).
	all, err := s.watches.List(ctx)
	if err != nil {
		s.log.Warn("rss watch list", "err", err)
		return
	}
	watchedPerf := map[string]bool{}
	watching := map[string]watches.Watch{}
	for _, w := range all {
		if w.Status != watches.StatusWatching {
			continue
		}
		watching[w.StashDBID] = w
		for _, p := range w.Performers {
			if p = strings.TrimSpace(p); p != "" {
				watchedPerf[strings.ToLower(p)] = true
			}
		}
	}
	if len(watching) == 0 {
		return // nothing to catch
	}

	// One aggregate query-less pull = each indexer's recent uploads.
	fctx, cancel := context.WithTimeout(ctx, rssFetchTimeout)
	defer cancel()
	rels, err := pc.RecentReleases(fctx, s.pool.Settings().ProwlarrCategories)
	if err != nil {
		s.log.Warn("rss fetch", "err", err)
		return
	}

	marks, err := s.rss.Watermarks(ctx)
	if err != nil {
		s.log.Warn("rss watermarks", "err", err)
		return
	}
	m, err := s.Matcher(ctx)
	if err != nil {
		s.log.Warn("rss matcher", "err", err)
		return
	}
	firstRunCutoff := time.Now().Add(-rssFirstRunLookback).Unix()

	// Pass 1: fresh + watched-performer filter. newMarks tracks the max publish
	// seen per indexer (over ALL fresh releases, matched or not — we've now
	// "seen" them, so they won't be reconsidered next tick).
	newMarks := map[int]int64{}
	var candidates []prowlarr.Release
	var freshN int
	for _, r := range rels {
		pub := parsePublishUnix(r.PublishDate)
		if pub == 0 {
			continue // undatable — can't watermark it
		}
		cutoff := firstRunCutoff
		if wm := marks[r.IndexerID]; wm > 0 {
			cutoff = wm - int64(rssWatermarkOverlap.Seconds())
		}
		if pub <= cutoff {
			continue
		}
		freshN++
		if pub > newMarks[r.IndexerID] {
			newMarks[r.IndexerID] = pub
		}
		// Cheap LOCAL pre-filter (no StashDB): drop releases that name no
		// watched performer. This is what keeps a frequent RSS tick affordable.
		if touchesWatchedPerformer(m, r.Title, watchedPerf) {
			candidates = append(candidates, r)
		}
	}

	// Pass 2: confirm each candidate against the watchlist with the matcher.
	triggers := map[string]watches.Watch{}
	for _, r := range candidates {
		if isPackRelease(r.Title) || isImageSetRelease(r.Title) || isLinkSpamRelease(r.Title) {
			continue
		}
		cands, err := m.Match(ctx, r.Title)
		if err != nil || len(cands) == 0 {
			continue
		}
		for i := range cands {
			w, ok := watching[cands[i].Scene.ID]
			if !ok {
				continue
			}
			if matcher.Verify(cands, w.StashDBID, w.Title, r.Title).Verified {
				triggers[w.StashDBID] = w
				break // one scene per release
			}
		}
	}

	// Pass 3: trigger the full per-scene search for each matched scene, so the
	// BEST release is chosen rather than the one RSS happened to surface.
	triggered := 0
	for _, w := range triggers {
		if ctx.Err() != nil {
			return
		}
		if triggered >= rssMaxTriggersPerTick {
			s.log.Warn("rss trigger cap hit",
				"capped_at", rssMaxTriggersPerTick, "pending", len(triggers)-triggered)
			break
		}
		s.checkWatch(ctx, w)
		triggered++
	}

	// Advance watermarks only after processing (a mid-tick failure re-scans).
	for ix, pub := range newMarks {
		if err := s.rss.Advance(ctx, ix, pub); err != nil {
			s.log.Warn("rss advance watermark", "indexer", ix, "err", err)
		}
	}

	if freshN > 0 || len(candidates) > 0 {
		s.log.Info("rss tick", "pulled", len(rels), "fresh", freshN,
			"candidates", len(candidates), "triggered", triggered)
	}
}

// parsePublishUnix parses a Prowlarr RFC3339 publishDate to unix seconds, or 0
// on failure (such releases are skipped — we can't watermark what we can't date).
func parsePublishUnix(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0
	}
	return t.Unix()
}

// touchesWatchedPerformer reports whether a release title names any watched
// performer, using the matcher's LOCAL scanner only (no StashDB roundtrip).
func touchesWatchedPerformer(m *matcher.Matcher, title string, watched map[string]bool) bool {
	for _, name := range m.PerformersInTitle(title) {
		if watched[strings.ToLower(name)] {
			return true
		}
	}
	return false
}
