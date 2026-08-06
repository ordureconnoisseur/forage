package api

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

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
}

// getDiscoverPerformers serves GET /discover/performers.
func (s *Server) getDiscoverPerformers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s.perfPicks.mu.Lock()
	defer s.perfPicks.mu.Unlock()
	// The lock is held across the fetch rather than released and retaken.
	// Two tabs opening the page at once would otherwise both pay for eight
	// live scene pages to compute the same answer.
	if s.perfPicks.resp != nil && time.Since(s.perfPicks.at) < performerPickTTL &&
		r.URL.Query().Get("refresh") != "true" {
		writeJSON(w, http.StatusOK, s.perfPicks.resp)
		return
	}

	sdb := s.pool.StashDB()
	if sdb == nil {
		writeErr(w, http.StatusServiceUnavailable, "StashDB is not configured")
		return
	}

	local := s.localPerformerStashDBIDs(ctx)
	hideMale := s.pool.Settings().HideMalePerformers

	out := &discoverPerformersResponse{
		Trending:    s.trendingPerformers(ctx, sdb, local, hideMale),
		Debut:       s.sortedPerformers(ctx, sdb, "DEBUT", local, hideMale),
		Active:      s.sortedPerformers(ctx, sdb, "LAST_SCENE", local, hideMale),
		RefreshedAt: nowUnix(),
	}
	// Only cache a result worth reusing. An empty strip usually means StashDB
	// was unreachable, and caching that would hold the failure for an hour.
	if len(out.Trending)+len(out.Debut)+len(out.Active) > 0 {
		s.perfPicks.at, s.perfPicks.resp = time.Now(), out
	}
	writeJSON(w, http.StatusOK, out)
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
