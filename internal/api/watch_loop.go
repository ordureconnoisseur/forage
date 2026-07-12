package api

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/ordureconnoisseur/forager/internal/scoring"
	"github.com/ordureconnoisseur/forager/internal/stashdb"
	"github.com/ordureconnoisseur/forager/internal/watches"
)

// capPerformerNames dedupes (case-insensitive) and caps a stored performer-name
// list to the same ceiling the live resolver uses, keeping the query fan-out
// bounded.
func capPerformerNames(names []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		k := strings.ToLower(n)
		if n == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, n)
		if len(out) >= maxPerformerNames {
			break
		}
	}
	return out
}

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
	// before it's abandoned and the scene is judged on whatever returned so
	// far. A scene fans out several queries that now run SEQUENTIALLY, and
	// Prowlarr rate-limits requests to the same indexer (~2s apart) — so a
	// prolific performer's full fan-out (8+ queries, each hitting every
	// indexer) legitimately needs ~40s to complete. The old 25s budget
	// guillotined those mid-search, producing a flood of "context deadline
	// exceeded" and never flipping the watch. 60s lets the rate-limited
	// queries actually finish; a 30-min tick still fits a full batch
	// (8 scenes x ~40s = ~5 min).
	watchSearchTimeout = 60 * time.Second
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

// checkWatch re-searches one watch and, on a verified release, flips it to
// available (best release by preference). last_checked was already stamped by
// ClaimBatch.
func (s *Server) checkWatch(ctx context.Context, w watches.Watch) {
	// Hold: a scene whose grab is pending resolution (mismatched in the
	// review queue, orphaned in limbo) or already live must not be
	// re-searched — the machine's mismatch verdict is a question FOR THE
	// USER, and re-searching while it's unanswered re-offers releases for
	// a scene whose acquisition is in flight. Flip the watch to 'grabbed'
	// (the quiet state); resolving the mismatch (redo/delete) removes the
	// coverage and the reconcile reverse pass resumes the hunt.
	if _, covered, cerr := s.grabbedSceneSet(ctx); cerr == nil && covered[w.StashDBID] {
		if err := s.watches.MarkGrabbed(ctx, w.StashDBID,
			w.FoundTitle, w.FoundURL, w.FoundIndexer, w.FoundProtocol, w.FoundSize); err == nil {
			s.log.Info("watch held — a grab for this scene is live or pending mismatch review",
				"scene", w.StashDBID, "title", w.Title)
		}
		return
	}
	m, err := s.Matcher(ctx)
	if err != nil {
		return
	}
	stashDBC := s.pool.StashDB()
	pc := s.pool.Prowlarr()
	if stashDBC == nil || pc == nil {
		return
	}

	// Resolve the scene's title/studio/date + the FULL performer-name set the
	// search needs. Prefer the names stored on the watch (captured at add time)
	// so a re-check makes NO StashDB call — re-fetching an immutable scene every
	// check was the source of StashDB throttling. Only when none are stored
	// (a bare API add, or a pre-migration watch) do we fetch once and backfill.
	var (
		scene     *stashdb.Scene
		perfNames []string
	)
	if len(w.Performers) > 0 {
		scene = &stashdb.Scene{ID: w.StashDBID, Title: w.Title, Date: w.Date}
		if w.StudioName != "" {
			scene.Studio = &stashdb.SceneStudio{Name: w.StudioName}
		}
		perfNames = capPerformerNames(w.Performers)
	} else {
		sc, ferr := stashDBC.FindScene(ctx, w.StashDBID)
		if ferr != nil || sc == nil {
			return
		}
		scene = sc
		// Backfill display metadata for non-card (bare) adds.
		if w.ImageURL == "" || w.Title == "" || w.StudioName == "" {
			img := ""
			if len(sc.Images) > 0 {
				img = sc.Images[0].URL
			}
			studio := ""
			if sc.Studio != nil {
				studio = sc.Studio.Name
			}
			if berr := s.watches.BackfillMeta(ctx, w.StashDBID, sc.Title, sc.Date, studio, img); berr != nil {
				s.log.Warn("watch backfill meta", "scene", w.StashDBID, "err", berr)
			}
		}
		// ALL the scene's performers (ctxPerformer "" → not narrowed to the
		// tracked one) so the search covers releases named under any of them.
		perfNames = s.scenePerformerNames(ctx, sc, "", "")
		if len(perfNames) > 0 {
			if serr := s.watches.SetPerformers(ctx, w.StashDBID, perfNames); serr != nil {
				s.log.Warn("watch store performers", "scene", w.StashDBID, "err", serr)
			}
		}
	}
	if len(perfNames) == 0 {
		return // nothing to search by
	}

	// Cap each scene's search so one slow indexer can't stall it. The full
	// search fans out several queries and waits for all of them; a single slow
	// indexer (near the 60s Prowlarr client timeout) otherwise pins a scene at
	// ~1-2 min. With a tighter deadline the slow query is cancelled and the
	// scene is judged on whatever the fast indexers returned (still verifies).
	sctx, cancel := context.WithTimeout(ctx, watchSearchTimeout)
	defer cancel()
	// FULL (non-lean) search across all the performers above — lean's 2-query
	// set misses studio releases named by date + a non-primary performer.
	releases, err := s.searchSceneReleases(sctx, pc, scene, perfNames, s.pool.Settings().ProwlarrCategories, false /*full*/)
	if err != nil || len(releases) == 0 {
		return
	}
	cands := s.verifyReleases(ctx, m, w.StashDBID, scene.Title, releases)
	// Never offer a release that's already been grabbed: a completed grab
	// that identified as a DIFFERENT scene (mismatched) leaves this watch
	// hunting, the re-check re-finds the exact release the user already
	// has on disk, and the notification asks them to grab it again. Same
	// non-failed rule doGrab's dedup applies — a failed grab is a
	// legitimate fresh offer.
	cands = s.dropAlreadyGrabbed(ctx, cands)

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
		if c.Verified && !c.Rejected && c.DownloadURL != "" {
			picks = append(picks, c)
		}
	}
	// Order by the same comparator that chooses the auto-pick, so the stored
	// re-pick list leads with the release the watcher actually selected.
	sort.SliceStable(picks, func(i, j int) bool { return betterRelease(picks[i], picks[j]) })
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
// dropAlreadyGrabbed filters out candidates that already have a non-failed
// grab — the user is already getting (or has) that exact release, so
// surfacing it as a fresh find is noise at best and a repeat notification
// at worst. Matched by URL AND by (title, indexer): Prowlarr's download
// URLs carry a rotating encrypted link parameter, so the same release
// returns a different URL on every search and URL equality alone re-offers
// it forever. Point lookups per candidate against the local grabs table;
// candidate lists are small (≤ dozens per scene).
func (s *Server) dropAlreadyGrabbed(ctx context.Context, cands []sceneRelease) []sceneRelease {
	if s.grabs == nil {
		return cands
	}
	kept := cands[:0]
	for _, c := range cands {
		if g, err := s.grabs.LiveByRelease(ctx, c.DownloadURL, c.Title, c.Indexer); err == nil && g != nil {
			continue
		}
		kept = append(kept, c)
	}
	return kept
}

func (s *Server) bestWatchMatch(cands []sceneRelease, ignored []string) *sceneRelease {
	ignoredSet := make(map[string]bool, len(ignored))
	for _, u := range ignored {
		ignoredSet[u] = true
	}
	bestIdx := -1
	for i := range cands {
		c := &cands[i]
		// Skip unverified/rejected, releases with no grab link (a release we
		// can't actually download must never be the "found" one — it'd flip
		// the watch to available but fail silently on grab), and releases the
		// user dismissed for this watch (a dead/over-compressed find must not
		// re-surface). The ignored set holds both URLs and titles: Prowlarr
		// URLs rotate between searches, so the title is the durable half.
		if !c.Verified || c.Rejected || c.DownloadURL == "" ||
			ignoredSet[c.DownloadURL] || ignoredSet[c.Title] {
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

// confidenceTiebreakEpsilon is the smallest match-confidence gap that counts as
// "one is a meaningfully surer match". Below it the two are treated as equally
// confident, so scoring noise can't flip the pick on a near-coin-toss.
const confidenceTiebreakEpsilon = 0.05

// betterRelease reports whether a should rank above b for auto-selection — the
// single comparator both the watch pick and the per-scene release list use.
//
// Precedence:
//  1. grabbable — a dead torrent (0 seeders) can never win, whatever its score.
//  2. DIFFERENT resolution → the user's full preference SCORE decides. Resolution,
//     protocol and reject preferences all live in the score, including
//     idiosyncratic ones (1080p ranked above 4K, usenet weighted so a 1080p
//     usenet beats a VR torrent). Comparing whole scores across tiers preserves
//     all of that.
//  3. SAME resolution → the candidates are the same scene at the same quality, so
//     a tiny preference-score gap shouldn't decide it. Prefer the SURER match
//     first (a weakly-verified release is a mis-grab risk — e.g. one with the
//     exact scene date beats one matched only on performer+studio), then seed
//     health, then popularity (grabs), then the bigger encode, and only then the
//     score's fine indexer/protocol nudge as the last word.
func betterRelease(a, b sceneRelease) bool {
	if ga, gb := grabbable(a), grabbable(b); ga != gb {
		return ga
	}
	if scoring.Resolution(a.Title) != scoring.Resolution(b.Title) {
		return a.Score > b.Score
	}
	if d := a.Confidence - b.Confidence; d > confidenceTiebreakEpsilon || d < -confidenceTiebreakEpsilon {
		return d > 0
	}
	if sa, sb := seedTier(a), seedTier(b); sa != sb {
		return sa > sb
	}
	if a.Popularity != b.Popularity {
		return a.Popularity > b.Popularity
	}
	if a.Size != b.Size {
		return a.Size > b.Size
	}
	return a.Score > b.Score
}
