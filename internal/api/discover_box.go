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

// boxLinkTTL bounds how long the performer identity maps are reused. Shorter
// than it could be: adding a performer from a box card should make the next
// refresh show them as owned, not leave a stale "+" for an hour.
const boxLinkTTL = 5 * time.Minute

// boxIdentity is how one secondary box's performers map onto the library.
type boxIdentity struct {
	at time.Time
	// onBox: box performer id → local Stash performer id, for performers the
	// user has actually identified against this box. Exact.
	onBox map[string]string
	// onStashDB: StashDB performer id → local Stash performer id. The other
	// half of the URL route below.
	onStashDB map[string]string
	// links: box performer id → the StashDB performer id their profile links
	// to ("" when it links to none). Memoised across refreshes because it is
	// a fact about the box, not about the library.
	links map[string]string
}

type boxIdentityCache struct {
	mu sync.Mutex
	by map[string]*boxIdentity
}

// boxLinkMemoCap bounds the links memo. Browsing deep into a large box would
// otherwise accumulate an entry per performer ever seen, for the lifetime of
// the daemon. Dropping the whole map is fine: it costs one batched re-query.
const boxLinkMemoCap = 20000

// performersOwnedOnBox answers, for the performers on this page, which ones
// the user already has, as box performer id → local Stash performer id.
//
// Two routes, tried in that order:
//
//  1. The performer is identified against this box locally. Exact, and free
//     after one sweep, but rare: 147 FansDB and 2 JavStash performers on the
//     reference library, against 779 on StashDB.
//
//  2. The box's own profile for them links to their StashDB page, and the
//     library has that StashDB id. Secondary boxes carry no cross-id field,
//     but their editors list StashDB among a performer's urls often enough to
//     matter: 78% of JavStash performers and 47% of FansDB's on a 40-scene
//     sample. This is what makes browsing FansDB recognise the performers you
//     followed on StashDB.
//
// A failure at any step yields "not known to be owned", which shows the "+"
// on someone you have. That is the right way round: the opposite would hide
// the only control the pill offers.
func (s *Server) performersOwnedOnBox(ctx context.Context, e *boxEntry, scenes []stashdb.Scene) map[string]string {
	ids := make([]string, 0, 64)
	seen := map[string]bool{}
	for _, sc := range scenes {
		for _, p := range sc.Performers {
			if p.ID != "" && !seen[p.ID] {
				seen[p.ID] = true
				ids = append(ids, p.ID)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}

	id := s.boxIdentityFor(ctx, e.box.Endpoint)
	if id == nil {
		return nil
	}

	out := make(map[string]string, len(ids))
	var unlinked []string
	s.boxIdentity.mu.Lock()
	for _, pid := range ids {
		if local := id.onBox[pid]; local != "" {
			out[pid] = local
			continue
		}
		sdb, known := id.links[pid]
		if !known {
			unlinked = append(unlinked, pid)
			continue
		}
		if local := id.onStashDB[sdb]; sdb != "" && local != "" {
			out[pid] = local
		}
	}
	s.boxIdentity.mu.Unlock()

	if len(unlinked) > 0 {
		links, err := e.client.PerformerStashDBLinks(ctx, unlinked)
		if err != nil {
			// Partial results are still returned by the client, so keep
			// what came back rather than discarding the whole page's work.
			s.log.Warn("discover box: performer links",
				"endpoint", e.box.Endpoint, "err", err)
		}
		s.boxIdentity.mu.Lock()
		if len(id.links)+len(links) > boxLinkMemoCap {
			id.links = make(map[string]string, len(links))
		}
		for pid, sdb := range links {
			id.links[pid] = sdb
			if local := id.onStashDB[sdb]; sdb != "" && local != "" {
				out[pid] = local
			}
		}
		s.boxIdentity.mu.Unlock()
	}
	return out
}

// boxIdentityFor returns the (memoised) identity maps for one endpoint,
// refreshing the two Stash sweeps when they age out. The links memo survives
// a refresh; only the library-side halves are re-read.
func (s *Server) boxIdentityFor(ctx context.Context, endpoint string) *boxIdentity {
	s.boxIdentity.mu.Lock()
	if s.boxIdentity.by == nil {
		s.boxIdentity.by = map[string]*boxIdentity{}
	}
	got := s.boxIdentity.by[endpoint]
	if got != nil && time.Since(got.at) < boxLinkTTL {
		s.boxIdentity.mu.Unlock()
		return got
	}
	s.boxIdentity.mu.Unlock()

	sc := s.pool.Stash()
	if sc == nil {
		return got // stale beats nothing; nil if we never had one
	}
	onBox, err := sc.PerformerLocalIDsByStashID(ctx, endpoint)
	if err != nil {
		s.log.Warn("discover box: local performers on box", "endpoint", endpoint, "err", err)
		return got
	}
	// The endpoint STASH uses for StashDB, not the canonical spelling: the
	// cross-ids on local performers carry whatever it was configured with,
	// and a mismatch here would silently return an empty map.
	var onStashDB map[string]string
	if sdb := s.stashDBEndpoint(ctx, sc); sdb != "" && sdb != endpoint {
		onStashDB, err = sc.PerformerLocalIDsByStashID(ctx, sdb)
		if err != nil {
			s.log.Warn("discover box: local performers on stashdb", "err", err)
		}
	}

	s.boxIdentity.mu.Lock()
	defer s.boxIdentity.mu.Unlock()
	cur := s.boxIdentity.by[endpoint]
	if cur == nil {
		cur = &boxIdentity{links: map[string]string{}}
		s.boxIdentity.by[endpoint] = cur
	}
	cur.at = time.Now()
	cur.onBox = onBox
	if onStashDB != nil {
		cur.onStashDB = onStashDB
	}
	return cur
}

// allScenes flattens the feed and the carousel into one list, so the lookups
// that cost a round trip are done once for the page rather than per list.
func allScenes(lists ...*stashdb.QueryScenesResult) []stashdb.Scene {
	var out []stashdb.Scene
	for _, l := range lists {
		if l != nil {
			out = append(out, l.Scenes...)
		}
	}
	return out
}

// ownedByFingerprint returns the box scene ids whose file the library already
// holds under a StashDB id, so they can be dropped from the feed.
//
// ownedOnBox only catches scenes identified against THIS box, which is a small
// set (93 FansDB, 253 JavStash) because almost nobody identifies against a
// secondary box. Most of what the user owns is keyed on StashDB, and a FansDB
// scene id says nothing about that. Fingerprints do: they are computed from the
// file, so when both boxes have seen a release they agree on its hash.
//
// One request for the whole page. Scenes the box has no hashes for are simply
// not covered, which is why this supplements ownedOnBox rather than replacing
// it.
func (s *Server) ownedByFingerprint(ctx context.Context, scenes []stashdb.Scene) map[string]bool {
	sdb := s.pool.StashDB()
	if sdb == nil || len(scenes) == 0 {
		return nil
	}
	batches := make([][]stashdb.Fingerprint, len(scenes))
	any := false
	for i, sc := range scenes {
		batches[i] = sc.Fingerprints
		if len(sc.Fingerprints) > 0 {
			any = true
		}
	}
	if !any {
		// JavStash carries no fingerprints at all, so this is the normal
		// path there rather than a failure.
		return nil
	}
	matched, err := sdb.FindScenesByFingerprints(ctx, batches)
	if err != nil {
		// Partial results are usable; a scene we fail to recognise is one
		// the user sees again, which is the harmless direction.
		s.log.Warn("discover box: fingerprint join", "err", err)
	}
	// The narrow per-endpoint sweep, not ownedSceneCopies. The question here
	// is only "is this StashDB id in the library"; ownedSceneCopies answers a
	// richer one (every copy, with path and resolution) by paging all 124,094
	// scenes with their file lists, and its 60s memo meant a browse page paid
	// for that sweep about every other load. This asks for 20,300 ids and
	// nothing else, and shares the 5-minute memo the box path already has.
	ownedStashDB := s.ownedOnBox(ctx, s.stashDBEndpointCached(ctx))
	out := map[string]bool{}
	for i, stashDBID := range matched {
		if stashDBID == "" || i >= len(scenes) {
			continue
		}
		if ownedStashDB[stashDBID] {
			out[scenes[i].ID] = true
		}
	}
	return out
}

// stashDBEndpointCached is stashDBEndpoint without needing a Stash client in
// hand, for callers that only want the string.
func (s *Server) stashDBEndpointCached(ctx context.Context) string {
	sc := s.pool.Stash()
	if sc == nil {
		return ""
	}
	return s.stashDBEndpoint(ctx, sc)
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
		Page: page, PerPage: perPage, Sort: "DATE", WithFingerprints: true,
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
			Page: 1, PerPage: trendingLimit, Sort: "TRENDING", WithFingerprints: true,
		})
		if err != nil {
			// A missing carousel is survivable; the feed is the point.
			s.log.Warn("discover box: trending", "endpoint", e.box.Endpoint, "err", err)
		}
	}

	owned := s.ownedOnBox(ctx, e.box.Endpoint)
	// Both lists in one pass: the fingerprint join is a single request, and
	// splitting it in two would double it for no gain.
	for id := range s.ownedByFingerprint(ctx, allScenes(recent, trending)) {
		if owned == nil {
			owned = map[string]bool{}
		}
		owned[id] = true
	}
	ownedPerformers := s.performersOwnedOnBox(ctx, e, allScenes(recent, trending))
	watchStatus := s.watchStatusByScene(ctx)
	hideMale := s.pool.Settings().HideMalePerformers

	out := discoverResponse{
		Scenes:   boxScenes(recent, owned, ownedPerformers, watchStatus, hideMale),
		Trending: boxScenes(trending, owned, ownedPerformers, watchStatus, hideMale),
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
// ownedPerformers maps a box performer id to the LOCAL Stash performer id, for
// the performers the library already has. Anyone in it gets an ordinary pill
// that navigates to them; anyone absent keeps the "+" that adds them. See
// performersOwnedOnBox for how that mapping is established.
func boxScenes(res *stashdb.QueryScenesResult, owned map[string]bool,
	ownedPerformers map[string]string,
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
			local := ownedPerformers[p.ID]
			d.Performers = append(d.Performers, discoverPerformer{
				Name:      name,
				StashID:   local,
				StashDBID: p.ID,
				Gender:    g,
				Local:     local != "",
			})
		}
		if d.Performers == nil {
			d.Performers = []discoverPerformer{}
		}
		out = append(out, d)
	}
	return out
}
