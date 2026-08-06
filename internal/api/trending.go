package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

// Browsing trending past the end of the carousel.
//
// Discover's carousel renders the daemon's cached top 50: RefreshTrending
// stamps a rank onto recent_scene_cache once an hour, and the whole cache
// exists to answer "what is new from the performers you follow". It is
// performer-scoped, date-bounded and periodically pruned, which makes it the
// wrong place to grow an open-ended browse surface. Caching 500 trending rows
// hourly to support scrolling would be ten round trips an hour for pages
// nobody opens, and still bounded at whatever number was picked.
//
// So this is live, one page per request, exactly the way a secondary stash-box
// is already served. Nothing here writes to the database. What it does share
// with the cached path is the enrichment (owned-copy marking, local-performer
// pills, watch state), so a card here behaves identically to a card on
// Discover rather than being a second, subtly different kind of scene card.
const (
	trendingPerPageDefault = 24
	trendingPerPageMax     = 60
	// StashDB's TRENDING sort is a ranking, not an archive. Several hundred
	// deep it has stopped meaning anything the word "trending" carries, and
	// the tail is just the scene table in an arbitrary order.
	trendingMaxPage = 40
)

// trendingResponse deliberately carries has_more rather than a total.
//
// queryScenes reports `count`, but for an unfiltered trending query that is
// the size of StashDB's entire scene table, around a million. Rendering
// "24 of 1,043,882" would be an honest number answering a question nobody
// asked, and it would imply the whole tail is reachable when trendingMaxPage
// says otherwise. What a scroller actually needs to know is whether to keep
// going.
type trendingResponse struct {
	Scenes  []discoverScene `json:"scenes"`
	Page    int             `json:"page"`
	PerPage int             `json:"per_page"`
	HasMore bool            `json:"has_more"`
	// Source names the box these came from, for the page header.
	Source string `json:"source"`
}

// primaryEndpoint is the endpoint Stash has configured for StashDB itself.
// ownedOnBox is keyed by endpoint, so the primary needs its own string rather
// than the empty one the query parameter uses to mean "the default source".
func (s *Server) primaryEndpoint(ctx context.Context) (endpoint, name string) {
	for _, e := range s.stashBoxes(ctx) {
		if e.box.Primary {
			return e.box.Endpoint, e.box.Name
		}
	}
	return "", "StashDB"
}

// getTrending serves GET /trending.
//
//	?page=1        1-based, capped at trendingMaxPage
//	?per_page=24   capped at trendingPerPageMax
//	?box=          a secondary stash-box endpoint; empty means StashDB
func (s *Server) getTrending(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if perPage <= 0 {
		perPage = trendingPerPageDefault
	}
	if perPage > trendingPerPageMax {
		perPage = trendingPerPageMax
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page <= 0 {
		page = 1
	}
	if page > trendingMaxPage {
		page = trendingMaxPage
	}

	// One code path for both sources. A secondary box brings its own client
	// and its own endpoint; the primary borrows the daemon's StashDB client,
	// which authenticates with the daemon's own key rather than Stash's.
	e := s.boxByEndpoint(ctx, r.URL.Query().Get("box"))
	var (
		client   *stashdb.Client
		endpoint string
		name     string
		primary  = true
	)
	if e != nil && e.client != nil {
		client, endpoint, name, primary = e.client, e.box.Endpoint, e.box.Name, false
	} else {
		client = s.pool.StashDB()
		endpoint, name = s.primaryEndpoint(ctx)
	}
	if client == nil {
		writeErr(w, http.StatusServiceUnavailable, "no metadata source is configured")
		return
	}

	res, err := client.QueryScenes(ctx, stashdb.SceneQuery{
		Page: page, PerPage: perPage, Sort: "TRENDING", WithFingerprints: true,
	})
	if err != nil {
		s.log.Warn("trending: query", "endpoint", endpoint, "page", page, "err", err)
		writeErr(w, http.StatusBadGateway, name+" is not responding: "+shortErr(err))
		return
	}

	owned := s.ownedOnBox(ctx, endpoint)
	// Fingerprints catch copies the library holds without having identified
	// them against this box, which is most of them on a lightly-tagged
	// library and the whole reason a "you have this" marker is worth showing.
	for id := range s.ownedByFingerprint(ctx, allScenes(res)) {
		if owned == nil {
			owned = map[string]bool{}
		}
		owned[id] = true
	}

	ownedPerformers := s.trendingOwnedPerformers(ctx, e, primary, res)

	writeJSON(w, http.StatusOK, trendingResponse{
		Scenes: boxScenes(res, owned, ownedPerformers,
			s.watchStatusByScene(ctx), s.pool.Settings().HideMalePerformers),
		Page:    page,
		PerPage: perPage,
		// A short page means the source ran out; the cap means we chose to.
		HasMore: res != nil && len(res.Scenes) == perPage && page < trendingMaxPage,
		Source:  name,
	})
}

// trendingOwnedPerformers maps a source's performer ids to LOCAL Stash
// performer ids, so people already in the library get an ordinary pill and
// everyone else keeps the "+" that adds them.
//
// On StashDB the source's performer id IS the id the library records against
// its performers, so the local index answers this outright. Asking the
// secondary-box path instead would work, but only after a round trip to
// StashDB asking StashDB which of its performers link to StashDB.
func (s *Server) trendingOwnedPerformers(ctx context.Context, e *boxEntry,
	primary bool, res *stashdb.QueryScenesResult) map[string]string {
	if !primary && e != nil {
		return s.performersOwnedOnBox(ctx, e, allScenes(res))
	}
	byID, _, err := localPerformerIDsByStashDBID(ctx, s.db)
	if err != nil {
		// A pill that offers to add someone you already have is a cosmetic
		// miss; failing the page over it is not.
		s.log.Warn("trending: local performer index", "err", err)
		return nil
	}
	return byID
}
