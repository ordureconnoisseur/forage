package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

// errNoStashDB is the one failure that is not partial: with no client there
// is nothing to ask.
var errNoStashDB = errors.New("StashDB is not configured")

// Three ways of asking "who should I be following?", for the strip at the top
// of the performers page.
//
// StashDB has no trending sort for performers. Its scene enum offers TRENDING;
// its performer enum does not, and POPULARITY is career volume, which answers
// with the same handful of men with four thousand scenes every time it is
// asked. So one of these three is derived here and two are sorts StashDB does
// have:
//
//	trending  who recurs across the trending SCENES. StashDB does not expose
//	          this, but it is implied by data it does: a performer on three of
//	          the current trending scenes is trending, whatever the enum says.
//	debut     DEBUT, whose first scene has just landed. New faces, and by
//	          definition nobody anyone follows yet.
//	active    LAST_SCENE, who released most recently. There is no date field
//	          to show for this, so the ORDER is the information.
//
// All three drop performers already in the library, because the question is
// who to add. That is also why the whole card is an add button.

const (
	// performerPickTTL is how long a computed strip is reused. The trending
	// lens costs several live scene pages to derive, and the ranking it reads
	// is refreshed hourly upstream, so recomputing faster would spend requests
	// to arrive at the same answer.
	performerPickTTL = time.Hour
	// performerPickCount is how many make it into each lens.
	performerPickCount = 24
	// trendingScanPages is how deep the trending ranking is walked to tally
	// performers. Measured on the live ranking: five pages of 48 yielded 98
	// un-owned performers of whom 8 appeared more than once, so the signal is
	// real but thin, and depth is what sharpens it.
	trendingScanPages = 8
	trendingScanPer   = 48
	// performerFetchCount over-fetches the two StashDB sorts, because the
	// library filter and the male filter both cut into the page and a strip
	// that renders four cards looks broken.
	performerFetchCount = 80
)

// discoverPerformer is one card in the strip.
type discoverPerformer2 struct {
	StashDBID string `json:"stashdb_id"`
	Name      string `json:"name"`
	Gender    string `json:"gender,omitempty"`
	ImageURL  string `json:"image_url,omitempty"`
	// SceneCount is how many scenes StashDB has for them in total.
	SceneCount int `json:"scene_count"`
	// TrendingScenes is how many of the CURRENT trending scenes they are on.
	// Only set for the trending lens, where it is the whole ranking signal.
	TrendingScenes int `json:"trending_scenes,omitempty"`
}

type discoverPerformersResponse struct {
	Trending    []discoverPerformer2 `json:"trending"`
	Debut       []discoverPerformer2 `json:"debut"`
	Active      []discoverPerformer2 `json:"active"`
	RefreshedAt int64                `json:"refreshed_at"`
}

// performerPickCache memoises the whole response. One value, not a map: the
// strip takes no parameters, so there is nothing to key on.
type performerPickCache struct {
	mu   sync.Mutex
	at   time.Time
	resp *discoverPerformersResponse
	// refreshing marks a background recompute in flight, so a burst of
	// requests against a stale entry kicks off one, not one each.
	refreshing bool
	// computeMu serialises the cold compute. Two tabs opening the page on a
	// freshly started daemon would otherwise both walk eight pages of the
	// trending ranking to arrive at the same answer.
	computeMu sync.Mutex
}

// getDiscoverPerformers serves GET /discover/performers.
//
// Stale-while-revalidate, because computing this costs about sixteen seconds:
// eight pages of the live trending ranking to tally, two performer sorts, and
// one batched hydration for the portraits. Nobody should wait that out to see
// a strip that was correct a minute ago. A stale answer is served immediately
// and refreshed behind it; only a daemon that has never computed one makes
// anybody wait, and then only once.
func (s *Server) getDiscoverPerformers(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("refresh") == "true"

	s.perfPicks.mu.Lock()
	cached, at, busy := s.perfPicks.resp, s.perfPicks.at, s.perfPicks.refreshing
	if cached != nil && !force {
		if time.Since(at) >= performerPickTTL && !busy {
			s.perfPicks.refreshing = true
			go s.refreshPerformerPicks()
		}
		s.perfPicks.mu.Unlock()
		writeJSON(w, http.StatusOK, cached)
		return
	}
	s.perfPicks.mu.Unlock()

	// Cold, or an explicit refresh. computeMu makes concurrent callers share
	// one computation rather than each paying for it.
	s.perfPicks.computeMu.Lock()
	defer s.perfPicks.computeMu.Unlock()
	s.perfPicks.mu.Lock()
	again := s.perfPicks.resp
	fresh := time.Since(s.perfPicks.at) < performerPickTTL
	s.perfPicks.mu.Unlock()
	if again != nil && fresh && !force {
		// Someone else computed it while this request waited for the lock.
		writeJSON(w, http.StatusOK, again)
		return
	}

	out, err := s.computePerformerPicks(r.Context())
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// refreshPerformerPicks recomputes in the background, off the request's
// context: that context is cancelled the moment the stale response is written,
// which would abort the very refresh it just triggered.
func (s *Server) refreshPerformerPicks() {
	defer func() {
		s.perfPicks.mu.Lock()
		s.perfPicks.refreshing = false
		s.perfPicks.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	s.perfPicks.computeMu.Lock()
	defer s.perfPicks.computeMu.Unlock()
	if _, err := s.computePerformerPicks(ctx); err != nil {
		s.log.Warn("discover performers: background refresh", "err", err)
	}
}

// computePerformerPicks builds all three lenses and stores them.
func (s *Server) computePerformerPicks(ctx context.Context) (*discoverPerformersResponse, error) {
	sdb := s.pool.StashDB()
	if sdb == nil {
		return nil, errNoStashDB
	}
	local := s.localPerformerStashDBIDs(ctx)
	hideMale := s.pool.Settings().HideMalePerformers

	out := &discoverPerformersResponse{
		Trending:    s.trendingPerformers(ctx, sdb, local, hideMale),
		Debut:       s.sortedPerformers(ctx, sdb, "DEBUT", local, hideMale),
		Active:      s.sortedPerformers(ctx, sdb, "LAST_SCENE", local, hideMale),
		RefreshedAt: nowUnix(),
	}
	// Only store a result worth reusing. An empty strip means StashDB was
	// unreachable, and caching that would hold the failure for an hour.
	if len(out.Trending)+len(out.Debut)+len(out.Active) > 0 {
		s.perfPicks.mu.Lock()
		s.perfPicks.at, s.perfPicks.resp = time.Now(), out
		s.perfPicks.mu.Unlock()
	}
	return out, nil
}

// localPerformerStashDBIDs is the set of StashDB ids the library already has,
// so the strip can offer only people who are not in it.
func (s *Server) localPerformerStashDBIDs(ctx context.Context) map[string]bool {
	byID, _, err := localPerformerIDsByStashDBID(ctx, s.db)
	if err != nil {
		// Fail open. Offering to add someone already present is recoverable
		// (the add path reports already_present); an empty strip is not.
		s.log.Warn("discover performers: local index", "err", err)
		return nil
	}
	set := make(map[string]bool, len(byID))
	for id := range byID {
		set[id] = true
	}
	return set
}

// sortedPerformers runs one of StashDB's own performer sorts and filters it.
func (s *Server) sortedPerformers(ctx context.Context, sdb *stashdb.Client,
	sortKey string, local map[string]bool, hideMale bool) []discoverPerformer2 {
	res, err := sdb.QueryPerformers(ctx, stashdb.PerformerQuery{
		Page: 1, PerPage: performerFetchCount, Sort: sortKey,
	})
	if err != nil {
		s.log.Warn("discover performers: query", "sort", sortKey, "err", err)
		return []discoverPerformer2{}
	}
	out := make([]discoverPerformer2, 0, performerPickCount)
	for _, p := range res.Performers {
		if len(out) >= performerPickCount {
			break
		}
		if !keepPerformer(p.ID, p.Gender, local, hideMale) {
			continue
		}
		out = append(out, discoverPerformer2{
			StashDBID:  p.ID,
			Name:       performerLabel(p),
			Gender:     strings.ToUpper(p.Gender),
			ImageURL:   p.ImageURL,
			SceneCount: p.SceneCount,
		})
	}
	return out
}

// trendingPerformers derives the ranking StashDB does not expose, by tallying
// who appears across the trending scenes.
func (s *Server) trendingPerformers(ctx context.Context, sdb *stashdb.Client,
	local map[string]bool, hideMale bool) []discoverPerformer2 {
	type tally struct {
		p     stashdb.ScenePerformer
		count int
		// best is the earliest position in the ranking they appear at, used
		// only to break ties: two performers on two trending scenes each are
		// separated by whose scenes rank higher.
		best int
	}
	seen := map[string]*tally{}
	rank := 0
	for page := 1; page <= trendingScanPages; page++ {
		res, err := sdb.QueryScenes(ctx, stashdb.SceneQuery{
			Page: page, PerPage: trendingScanPer, Sort: "TRENDING",
		})
		if err != nil {
			s.log.Warn("discover performers: trending scan", "page", page, "err", err)
			break
		}
		for _, sc := range res.Scenes {
			rank++
			for _, p := range sc.Performers {
				if p.ID == "" || p.Name == "" {
					continue
				}
				t := seen[p.ID]
				if t == nil {
					t = &tally{p: p, best: rank}
					seen[p.ID] = t
				}
				t.count++
			}
		}
		if len(res.Scenes) < trendingScanPer {
			break
		}
	}

	list := make([]*tally, 0, len(seen))
	for _, t := range seen {
		if keepPerformer(t.p.ID, t.p.Gender, local, hideMale) {
			list = append(list, t)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].count != list[j].count {
			return list[i].count > list[j].count
		}
		return list[i].best < list[j].best
	})
	if len(list) > performerPickCount {
		list = list[:performerPickCount]
	}

	// The scene query carries a performer's name and gender but no portrait,
	// so the picks are hydrated in one alias-batched round trip rather than
	// asking for images on every scene of all eight pages.
	ids := make([]string, 0, len(list))
	for _, t := range list {
		ids = append(ids, t.p.ID)
	}
	profiles := s.performerProfiles(ctx, sdb, ids)

	out := make([]discoverPerformer2, 0, len(list))
	for _, t := range list {
		d := discoverPerformer2{
			StashDBID:      t.p.ID,
			Name:           t.p.Name,
			Gender:         strings.ToUpper(t.p.Gender),
			TrendingScenes: t.count,
		}
		if pr, ok := profiles[t.p.ID]; ok {
			d.ImageURL = pr.ImageURL
			d.SceneCount = pr.SceneCount
			if pr.Name != "" {
				d.Name = performerLabel(pr)
			}
		}
		out = append(out, d)
	}
	return out
}

// performerProfiles fetches portraits for a set of ids.
func (s *Server) performerProfiles(ctx context.Context, sdb *stashdb.Client,
	ids []string) map[string]stashdb.PerformerProfile {
	if len(ids) == 0 {
		return nil
	}
	got, err := sdb.FindPerformerProfilesByID(ctx, ids)
	if err != nil {
		// A card without a portrait still names someone worth adding.
		s.log.Warn("discover performers: profiles", "err", err)
	}
	return got
}

// keepPerformer applies the two filters every lens shares.
func keepPerformer(id, gender string, local map[string]bool, hideMale bool) bool {
	if id == "" || local[id] {
		return false // already yours; the strip is for who to add
	}
	if hideMale && strings.EqualFold(gender, "MALE") {
		return false
	}
	return true
}

// performerLabel appends the disambiguation StashDB uses to tell two people
// of the same name apart, which is exactly when a card needs it most.
func performerLabel(p stashdb.PerformerProfile) string {
	if p.Disambiguation == "" {
		return p.Name
	}
	return p.Name + " (" + p.Disambiguation + ")"
}
