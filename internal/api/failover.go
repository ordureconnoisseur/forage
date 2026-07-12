package api

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
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
const indexerStatusTTL = 60 * time.Second

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
// Best-effort: any error returns an empty set (failover then only avoids
// the indexer that just failed), never blocks the retry.
func (s *Server) disabledIndexers(ctx context.Context) map[string]bool {
	s.indexerDisabled.mu.Lock()
	defer s.indexerDisabled.mu.Unlock()
	if time.Since(s.indexerDisabled.fetchedAt) < indexerStatusTTL && s.indexerDisabled.names != nil {
		return s.indexerDisabled.names
	}
	names := map[string]bool{}
	// Stamp before fetching so a failing Prowlarr isn't hammered once per
	// failover candidate; the empty set is cached for the TTL too.
	s.indexerDisabled.fetchedAt = time.Now()
	s.indexerDisabled.names = names

	pc := s.pool.Prowlarr()
	if pc == nil {
		return names
	}
	statuses, err := pc.IndexerStatuses(ctx)
	if err != nil {
		s.log.Warn("failover: indexerstatus fetch", "err", err)
		return names
	}
	now := time.Now()
	benched := map[int]bool{}
	for _, st := range statuses {
		if !st.DisabledTill.IsZero() && st.DisabledTill.After(now) {
			benched[st.IndexerID] = true
		}
	}
	if len(benched) == 0 {
		return names
	}
	indexers, err := pc.Indexers(ctx)
	if err != nil {
		s.log.Warn("failover: indexer list fetch", "err", err)
		return names
	}
	for _, ix := range indexers {
		if benched[ix.ID] {
			names[strings.ToLower(ix.Name)] = true
		}
	}
	return names
}

// chooseFailover picks the first release from the ranked list that a
// grab can safely switch to: matcher-verified (an automatic switch must
// not risk grabbing the wrong scene), not reject-ruled, actually
// grabbable, same protocol (the grab's download client doesn't change),
// and from a different, non-benched indexer than the one that failed.
// Returns nil when no alternative qualifies.
func chooseFailover(ranked []sceneRelease, failedIndexer, failedURL string, disabled map[string]bool) *sceneRelease {
	for i := range ranked {
		r := &ranked[i]
		if !r.Verified || r.Rejected || !grabbable(*r) {
			continue
		}
		if r.Protocol != "torrent" || r.DownloadURL == "" {
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
func (s *Server) resolveFailoverRelease(ctx context.Context, g failoverGrab) *sceneRelease {
	if g.Client != "qbit" || g.Kind != "single" || g.PredictedStashDBID == "" {
		return nil
	}
	prowlarrC := s.pool.Prowlarr()
	stashDBC := s.pool.StashDB()
	if prowlarrC == nil || stashDBC == nil {
		return nil
	}
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
	// Same ordering the interactive Grab view and the watcher use, so the
	// failover pick is exactly what the user would see at the top of the
	// list (minus the failed/benched indexers).
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Verified != out[j].Verified {
			return out[i].Verified
		}
		if out[i].Rejected != out[j].Rejected {
			return !out[i].Rejected
		}
		return betterRelease(out[i], out[j])
	})
	return chooseFailover(out, g.ReleaseIndexer, g.DownloadURL, s.disabledIndexers(ctx))
}

// failoverGrab is the slice of a grab the failover resolution needs;
// a plain value so tests can stub resolveFailover without a repo row.
type failoverGrab struct {
	ID                 int64
	Client             string
	Kind               string
	PredictedStashDBID string
	PerformerName      string
	ReleaseIndexer     string
	DownloadURL        string
}
