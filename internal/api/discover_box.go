package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

// Discover against a SECONDARY stash-box. Live-queried, never cached, and
// deliberately thinner than the StashDB feed: this answers "what else is out
// there" rather than "what am I missing", which stays a StashDB question
// because that is where the library's cross-ids are.
//
// Nothing here writes to the database. A FansDB scene id is only meaningful on
// FansDB, and putting one into tables keyed on StashDB ids is the mistake this
// whole design exists to avoid.

// boxOwnedTTL caps how long a box's owned-id set is reused. It only changes
// when the user identifies scenes against that box, which is a deliberate act.
// (Distinct from ownedTTL, which memoises the StashDB owned-copies sweep.)
const boxOwnedTTL = 5 * time.Minute

type boxOwnedSet struct {
	at  time.Time
	ids map[string]bool
}

type boxOwnedCache struct {
	mu sync.Mutex
	by map[string]boxOwnedSet // endpoint → owned scene ids on that box
}

// ownedOnBox returns the StashDB-style ids of scenes the library ALREADY has
// identified against this endpoint, so the feed can mark them.
//
// One query for the whole set rather than a lookup per card: Stash's stash-id
// filter takes a single id, so marking a 30-scene page the obvious way would be
// 30 round trips per page view. The sets are small (93 FansDB, 253 JavStash on
// the reference library) because secondary boxes are lightly identified — which
// is exactly why this is a marker and not a filter.
func (s *Server) ownedOnBox(ctx context.Context, endpoint string) map[string]bool {
	s.owned.mu.Lock()
	if s.owned.by == nil {
		s.owned.by = map[string]boxOwnedSet{}
	}
	if got, ok := s.owned.by[endpoint]; ok && time.Since(got.at) < boxOwnedTTL {
		s.owned.mu.Unlock()
		return got.ids
	}
	s.owned.mu.Unlock()

	sc := s.pool.Stash()
	if sc == nil {
		return nil
	}
	ids, err := sc.SceneStashIDsForEndpoint(ctx, endpoint)
	if err != nil {
		// Not fatal: an unmarked scene you own is a cosmetic miss, and
		// failing the whole page over it would be worse.
		s.log.Warn("discover box: owned ids", "endpoint", endpoint, "err", err)
		return nil
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	s.owned.mu.Lock()
	s.owned.by[endpoint] = boxOwnedSet{at: time.Now(), ids: set}
	s.owned.mu.Unlock()
	return set
}

// getDiscoverForBox serves the Discover feed from a secondary stash-box.
//
// Both lists come from the same source with different sorts: TRENDING for the
// carousel, DATE for the feed. Unlike the StashDB path there is no "scenes by
// performers you follow" — that needs the performer identified on this box, and
// with 147 FansDB and 2 JavStash performers locally it would return an almost
// empty page and read as broken.
func (s *Server) getDiscoverForBox(w http.ResponseWriter, r *http.Request, e *boxEntry) {
	perPage, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if perPage <= 0 {
		perPage = 60
	}
	if perPage > 100 {
		perPage = 100 // one live page; this is a browse surface, not a sync
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page <= 0 {
		page = 1
	}
	trendingLimit, _ := strconv.Atoi(r.URL.Query().Get("trending_limit"))
	if trendingLimit <= 0 {
		trendingLimit = 20
	}
	if trendingLimit > 50 {
		trendingLimit = 50
	}

	ctx := r.Context()
	recent, err := e.client.QueryScenes(ctx, stashdb.SceneQuery{
		Page: page, PerPage: perPage, Sort: "DATE",
	})
	if err != nil {
		s.log.Warn("discover box: recent", "endpoint", e.box.Endpoint, "err", err)
		writeErr(w, http.StatusBadGateway, e.box.Name+" is not responding: "+shortErr(err))
		return
	}
	// Trending only on the first page. Paging the feed should not re-fetch a
	// carousel that has not changed.
	var trending *stashdb.QueryScenesResult
	if page == 1 {
		trending, err = e.client.QueryScenes(ctx, stashdb.SceneQuery{
			Page: 1, PerPage: trendingLimit, Sort: "TRENDING",
		})
		if err != nil {
			// A missing carousel is survivable; the feed is the point.
			s.log.Warn("discover box: trending", "endpoint", e.box.Endpoint, "err", err)
		}
	}

	owned := s.ownedOnBox(ctx, e.box.Endpoint)
	watchStatus := s.watchStatusByScene(ctx)
	hideMale := s.pool.Settings().HideMalePerformers

	out := discoverResponse{
		Scenes:   boxScenes(recent, owned, watchStatus, hideMale),
		Trending: boxScenes(trending, owned, watchStatus, hideMale),
		Days:     0, // no window: a live box query is not date-bounded
	}
	// Both lists are freshly fetched, so "refreshed" is now by definition.
	out.RefreshedAt = nowUnix()
	out.TrendingRefreshedAt = out.RefreshedAt
	writeJSON(w, http.StatusOK, out)
}

// boxScenes maps live stash-box scenes onto the wire shape the Discover UI
// already renders.
//
// Every performer is marked Local:false. On a secondary box that is honest
// rather than lazy: local performers are indexed by their StashDB cross-id, so
// a FansDB performer id cannot be matched against them, and claiming ownership
// forage cannot verify would be worse than claiming none. The consequence is
// that the "+" to add a performer is offered for everyone here.
func boxScenes(res *stashdb.QueryScenesResult, owned map[string]bool,
	watchStatus map[string]string, hideMale bool) []discoverScene {
	if res == nil {
		return []discoverScene{}
	}
	out := make([]discoverScene, 0, len(res.Scenes))
	for _, sc := range res.Scenes {
		if owned[sc.ID] {
			continue // already in the library; nothing to discover
		}
		d := discoverScene{
			StashDBID:   sc.ID,
			Title:       sc.Title,
			ReleaseDate: sc.Date,
			WatchStatus: watchStatus[sc.ID],
		}
		if sc.Studio != nil {
			d.StudioName = sc.Studio.Name
		}
		if len(sc.Images) > 0 {
			d.ImageURL = sc.Images[0].URL
		}
		for _, p := range sc.Performers {
			if p.ID == "" || p.Name == "" {
				continue
			}
			// The gender rides along on the scene query here, so unlike the
			// StashDB path this needs no backfill table to filter on.
			g := strings.ToUpper(strings.TrimSpace(p.Gender))
			if hideMale && g == "MALE" {
				continue
			}
			name := p.Name
			if p.As != "" {
				name = p.As
			}
			d.Performers = append(d.Performers, discoverPerformer{
				Name:      name,
				StashDBID: p.ID,
				Gender:    g,
				Local:     false,
			})
		}
		if d.Performers == nil {
			d.Performers = []discoverPerformer{}
		}
		out = append(out, d)
	}
	return out
}
