package api

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/ordureconnoisseur/forager/internal/grabs"
)

// Indexer failover: when a deferred grab's failure was INDEXER-side (the
// .torrent fetch through Prowlarr 429ed/500ed; fail_kind="indexer"), the
// same scene is often well-seeded on a different indexer. Before
// re-driving such a grab, the retry loop re-resolves the scene's ranked
// releases and switches the grab to the best verified alternative that
// is (a) not from the indexer that just failed and (b) not on an indexer
// Prowlarr currently has in failure backoff. The grab row is mutated in
// place, so the scene linkage (predicted_stashdb_id) and Phase-B
// confirmation flow are untouched. Client-side failures never fail over:
// the release is fine, the client wasn't, and the original pick was the
// ranked best.
const (
	indexerStatusTTL = 60 * time.Second
	// failoverResolveTimeout bounds one grab's scene re-resolution
	// (StashDB fetch + Prowlarr searches + matcher verify).
	failoverResolveTimeout = 45 * time.Second
)

// disabledIndexerCache is a short-TTL snapshot of Prowlarr's
// failure-backoff list, keyed by lowercase indexer name. Fetching it
// costs two Prowlarr calls (indexerstatus + indexer, for the id-to-name
// mapping), so one tick's worth of failovers shares a fetch.
type disabledIndexerCache struct {
	mu        sync.Mutex
	fetchedAt time.Time
	names     map[string]bool
}

// disabledIndexers returns the lowercase names of indexers Prowlarr
// currently has benched (an active disabledTill in /api/v1/indexerstatus).
// Best-effort and never blocking: the mutex guards only the cached
// snapshot, never the Prowlarr round-trips, and a fetch FAILURE keeps
// serving the previous (stale) set rather than an empty one. Stale beats
// blind: a Prowlarr blip is exactly when its bench list is most likely
// populated, and forgetting it would let failover burn attempts on
// benched indexers the check exists to avoid. Failed fetches re-try
// after a short hold-off instead of the full TTL.
func (s *Server) disabledIndexers(ctx context.Context) map[string]bool {
	s.indexerDisabled.mu.Lock()
	if time.Since(s.indexerDisabled.fetchedAt) < indexerStatusTTL && s.indexerDisabled.names != nil {
		cached := s.indexerDisabled.names
		s.indexerDisabled.mu.Unlock()
		return cached
	}
	// Stamp before fetching so concurrent callers (and a failing
	// Prowlarr) don't stack fetches; adjusted down on failure below.
	s.indexerDisabled.fetchedAt = time.Now()
	prev := s.indexerDisabled.names
	s.indexerDisabled.mu.Unlock()

	names, err := s.fetchDisabledIndexers(ctx)

	s.indexerDisabled.mu.Lock()
	defer s.indexerDisabled.mu.Unlock()
	if err != nil {
		s.log.Warn("failover: disabled-indexer fetch", "err", err)
		// Keep the stale set and allow a re-try well before the TTL.
		s.indexerDisabled.fetchedAt = time.Now().Add(15*time.Second - indexerStatusTTL)
		if prev == nil {
			prev = map[string]bool{}
			s.indexerDisabled.names = prev
		}
		return prev
	}
	s.indexerDisabled.names = names
	return names
}

// fetchDisabledIndexers does the two Prowlarr round-trips (benched ids,
// then the id-to-name mapping) with no cache lock held.
func (s *Server) fetchDisabledIndexers(ctx context.Context) (map[string]bool, error) {
	names := map[string]bool{}
	pc := s.pool.Prowlarr()
	if pc == nil {
		return names, nil
	}
	statuses, err := pc.IndexerStatuses(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	benched := map[int]bool{}
	for _, st := range statuses {
		if !st.DisabledTill.IsZero() && st.DisabledTill.After(now) {
			benched[st.IndexerID] = true
		}
	}
	if len(benched) == 0 {
		return names, nil
	}
	indexers, err := pc.Indexers(ctx)
	if err != nil {
		return nil, err
	}
	for _, ix := range indexers {
		if benched[ix.ID] {
			names[strings.ToLower(ix.Name)] = true
		}
	}
	return names, nil
}

// chooseFailover picks the first release from the ranked list that an
// automatic flow can safely switch to: matcher-verified (an automatic
// switch must not risk grabbing the wrong scene), not reject-ruled,
// actually grabbable, and from a different, non-benched indexer than
// the one that failed. protocol restricts the pick ("torrent" for the
// deferred failover, whose grab is already bound to qBit; "" for the
// watch auto-pick, where no client is committed yet). Returns nil when
// no alternative qualifies.
func chooseFailover(ranked []sceneRelease, failedIndexer, failedURL, protocol string, disabled map[string]bool) *sceneRelease {
	for i := range ranked {
		r := &ranked[i]
		if !r.Verified || r.Rejected || !grabbable(*r) {
			continue
		}
		if (protocol != "" && r.Protocol != protocol) || r.DownloadURL == "" {
			continue
		}
		if strings.EqualFold(r.Indexer, failedIndexer) || r.DownloadURL == failedURL {
			continue
		}
		if disabled[strings.ToLower(r.Indexer)] {
			continue
		}
		return r
	}
	return nil
}

// resolveFailoverRelease re-resolves the scene behind a deferred grab
// and returns the release to fail over to, or nil when the grab should
// simply retry its original release (no scene linkage, resolution
// failure, or no qualifying alternative). Only single qbit grabs with a
// predicted scene are eligible: packs aren't scene-resolved, and SAB
// fetch failures aren't distinguishable client-side.
func (s *Server) resolveFailoverRelease(ctx context.Context, g *grabs.Grab) *sceneRelease {
	if g.Client != "qbit" || g.Kind != "single" || g.PredictedStashDBID == "" {
		return nil
	}
	prowlarrC := s.pool.Prowlarr()
	stashDBC := s.pool.StashDB()
	if prowlarrC == nil || stashDBC == nil {
		return nil
	}
	// Bound the whole resolution: it runs synchronously inside the retry
	// tick, and a degraded Prowlarr (exactly when indexer failures
	// cluster) must not stretch one grab's failover into minutes that
	// starve the rest of the batch.
	ctx, cancel := context.WithTimeout(ctx, failoverResolveTimeout)
	defer cancel()
	scene, err := stashDBC.FindScene(ctx, g.PredictedStashDBID)
	if err != nil || scene == nil || scene.Title == "" {
		return nil
	}
	m, err := s.Matcher(ctx)
	if err != nil {
		return nil
	}
	// lean mode: this runs unattended and possibly for several grabs per
	// tick; the fewer-queries variant is plenty to find the well-known
	// alternates and keeps Prowlarr load down.
	perfNames := s.scenePerformerNames(ctx, scene, g.PerformerName, "")
	releases, err := s.searchSceneReleases(ctx, prowlarrC, scene, perfNames,
		s.pool.Settings().ProwlarrCategories, true)
	if err != nil {
		s.log.Warn("failover: release search", "grab_id", g.ID, "err", err)
		return nil
	}
	out := s.verifyReleases(ctx, m, g.PredictedStashDBID, scene.Title, releases)
	// Shared ranking (rankReleases): the failover pick is exactly what the
	// user would see at the top of the interactive list, minus the
	// failed/benched indexers.
	rankReleases(out)
	return chooseFailover(out, g.ReleaseIndexer, g.DownloadURL, "torrent", s.disabledIndexers(ctx))
}
