package api

import (
	"context"
	"database/sql"
	"encoding/json"
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
	// performerPickCount is how many are SERVED per lens.
	performerPickCount = 24
	// performerPickStore is how many are kept per lens. Dismissals are
	// applied on the way out rather than baked into the computation, so
	// saying "not interested" costs nothing and the surplus is what refills
	// the strip behind it.
	performerPickStore = 40
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
	// Age is 0 when StashDB holds no birthdate, which is common; the card
	// shows nothing rather than a zero.
	Age int `json:"age,omitempty"`
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
		writeJSON(w, http.StatusOK, s.servable(r.Context(), cached))
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
		writeJSON(w, http.StatusOK, s.servable(r.Context(), again))
		return
	}

	// Detached, like the background refresh. This used to run on the
	// request's context, so whoever triggered the cold compute took it with
	// them when they navigated away: the walk aborted partway, and the
	// partial result was cached for an hour because it was not EMPTY. That
	// is exactly what a strip of twenty-four names with no portraits and no
	// other lenses is.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Minute)
	defer cancel()
	out, err := s.computePerformerPicks(cctx)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.servable(r.Context(), out))
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

	trending, terr := s.trendingPerformers(ctx, sdb, local, hideMale)
	debut, derr := s.sortedPerformers(ctx, sdb, "DEBUT", local, hideMale)
	active, aerr := s.sortedPerformers(ctx, sdb, "LAST_SCENE", local, hideMale)
	out := &discoverPerformersResponse{
		Trending:    trending,
		Debut:       debut,
		Active:      active,
		RefreshedAt: nowUnix(),
	}
	// Cache only a COMPLETE result. "Not empty" was too weak a test: a walk
	// that died partway still had two dozen names in it, so a degraded strip
	// with no portraits and two missing lenses was stored and served for an
	// hour. Partial answers are still returned to this caller, because a
	// half-strip beats none; they are just not remembered.
	if terr == nil && derr == nil && aerr == nil &&
		len(out.Trending)+len(out.Debut)+len(out.Active) > 0 {
		s.perfPicks.mu.Lock()
		s.perfPicks.at, s.perfPicks.resp = time.Now(), out
		s.perfPicks.mu.Unlock()
	} else if terr != nil || derr != nil || aerr != nil {
		s.log.Warn("discover performers: incomplete, not cached",
			"trending", terr, "debut", derr, "active", aerr)
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
	sortKey string, local map[string]bool, hideMale bool) ([]discoverPerformer2, error) {
	res, err := sdb.QueryPerformers(ctx, stashdb.PerformerQuery{
		Page: 1, PerPage: performerFetchCount, Sort: sortKey,
	})
	if err != nil {
		s.log.Warn("discover performers: query", "sort", sortKey, "err", err)
		return []discoverPerformer2{}, err
	}
	out := make([]discoverPerformer2, 0, performerPickStore)
	for _, p := range res.Performers {
		if len(out) >= performerPickStore {
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
			Age:        p.Age,
		})
	}
	return out, nil
}

// performerTally is one performer's standing in the derived ranking.
type performerTally struct {
	P     stashdb.ScenePerformer
	Count int
	// Best is the earliest position in the ranking they appear at, used only
	// to break ties: two performers on two trending scenes each are separated
	// by whose scenes rank higher.
	Best int
}

// rankTrendingPerformers is the derivation itself, kept apart from the
// fetching so it can be tested without a StashDB.
//
// scenes must arrive in ranking order; position in the slice IS the rank.
func rankTrendingPerformers(scenes []stashdb.Scene,
	local map[string]bool, hideMale bool) []performerTally {
	seen := map[string]*performerTally{}
	order := make([]string, 0, len(scenes))
	for rank, sc := range scenes {
		for _, p := range sc.Performers {
			if p.ID == "" || p.Name == "" {
				continue
			}
			t := seen[p.ID]
			if t == nil {
				t = &performerTally{P: p, Best: rank}
				seen[p.ID] = t
				order = append(order, p.ID)
			}
			t.Count++
		}
	}
	list := make([]performerTally, 0, len(order))
	// Seeded from `order`, not from the map: ranging a map is randomised, and
	// the tie-break below only orders pairs that differ, so equal entries
	// would shuffle between calls and the strip would reorder on every
	// refresh for no reason.
	for _, id := range order {
		t := seen[id]
		if keepPerformer(t.P.ID, t.P.Gender, local, hideMale) {
			list = append(list, *t)
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Count != list[j].Count {
			return list[i].Count > list[j].Count
		}
		return list[i].Best < list[j].Best
	})
	return list
}

// trendingPerformers walks the trending ranking and ranks who recurs in it.
func (s *Server) trendingPerformers(ctx context.Context, sdb *stashdb.Client,
	local map[string]bool, hideMale bool) ([]discoverPerformer2, error) {
	var failed error
	all := make([]stashdb.Scene, 0, trendingScanPages*trendingScanPer)
	for page := 1; page <= trendingScanPages; page++ {
		res, err := sdb.QueryScenes(ctx, stashdb.SceneQuery{
			Page: page, PerPage: trendingScanPer, Sort: "TRENDING",
		})
		if err != nil {
			// Partial depth still ranks; a shallower scan is a weaker signal,
			// not a wrong one.
			s.log.Warn("discover performers: trending scan", "page", page, "err", err)
			failed = err
			break
		}
		all = append(all, res.Scenes...)
		if len(res.Scenes) < trendingScanPer {
			break
		}
	}

	list := rankTrendingPerformers(all, local, hideMale)
	if len(list) > performerPickStore {
		list = list[:performerPickStore]
	}

	// The scene query carries a performer's name and gender but no portrait,
	// so the picks are hydrated in one alias-batched round trip rather than
	// asking for images on every scene of all eight pages.
	ids := make([]string, 0, len(list))
	for _, t := range list {
		ids = append(ids, t.P.ID)
	}
	profiles, perr := s.performerProfiles(ctx, sdb, ids)
	if perr != nil {
		// A strip of names with no faces is the degraded shape this whole
		// guard exists to stop being cached.
		failed = perr
	}

	out := make([]discoverPerformer2, 0, len(list))
	for _, t := range list {
		d := discoverPerformer2{
			StashDBID:      t.P.ID,
			Name:           t.P.Name,
			Gender:         strings.ToUpper(t.P.Gender),
			TrendingScenes: t.Count,
		}
		if pr, ok := profiles[t.P.ID]; ok {
			d.ImageURL = pr.ImageURL
			d.SceneCount = pr.SceneCount
			d.Age = pr.Age
			if pr.Name != "" {
				d.Name = performerLabel(pr)
			}
		}
		out = append(out, d)
	}
	return out, failed
}

// performerProfiles fetches portraits for a set of ids.
func (s *Server) performerProfiles(ctx context.Context, sdb *stashdb.Client,
	ids []string) (map[string]stashdb.PerformerProfile, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	got, err := sdb.FindPerformerProfilesByID(ctx, ids)
	if err != nil {
		// A card without a portrait still names someone worth adding, so the
		// partial map is returned; the error only stops it being cached.
		s.log.Warn("discover performers: profiles", "err", err)
	}
	return got, err
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

// ── dismissals ───────────────────────────────────────────────────────

// Saying "not interested".
//
// Applied when the strip is SERVED, not when it is computed. Dismissing
// therefore costs one row write and takes effect on the next paint, instead of
// invalidating an hour-old computation that took sixteen seconds to make. The
// lenses hold performerPickStore each and serve performerPickCount, so the
// surplus is what closes the gap a dismissal leaves.
const (
	dismissedPerformersKey = "dismissed_performers"
	// dismissedCap bounds the list. It is a "stop showing me this" set, not
	// an audit log, and the oldest entries are the ones whose absence would
	// be least noticed if the ranking ever surfaced them again.
	dismissedCap = 500
)

func loadDismissedPerformers(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	var raw string
	err := db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = ?`, dismissedPerformersKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set, nil
}

// servable applies the dismissals and trims each lens to what is shown.
func (s *Server) servable(ctx context.Context,
	in *discoverPerformersResponse) *discoverPerformersResponse {
	dismissed, err := loadDismissedPerformers(ctx, s.db)
	if err != nil {
		// Fail open: showing someone who asked not to be shown is a nuisance,
		// showing nobody is a broken page.
		s.log.Warn("discover performers: dismissals", "err", err)
	}
	return &discoverPerformersResponse{
		Trending:    trimPicks(in.Trending, dismissed),
		Debut:       trimPicks(in.Debut, dismissed),
		Active:      trimPicks(in.Active, dismissed),
		RefreshedAt: in.RefreshedAt,
	}
}

func trimPicks(in []discoverPerformer2, dismissed map[string]bool) []discoverPerformer2 {
	out := make([]discoverPerformer2, 0, performerPickCount)
	for _, p := range in {
		if len(out) >= performerPickCount {
			break
		}
		if dismissed[p.StashDBID] {
			continue
		}
		out = append(out, p)
	}
	return out
}

type dismissPerformerRequest struct {
	StashDBID string `json:"stashdb_id"`
	// Undo reverses it, so a mis-tap is not permanent.
	Undo bool `json:"undo"`
}

// postDismissPerformer serves POST /discover/performers/dismiss.
func (s *Server) postDismissPerformer(w http.ResponseWriter, r *http.Request) {
	var req dismissPerformerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request body")
		return
	}
	if !validImageID(req.StashDBID) {
		// StashDB ids are UUIDs; anything else cannot name a performer and
		// would just accumulate in the list forever.
		writeErr(w, http.StatusBadRequest, "not a stashdb id")
		return
	}
	ctx := r.Context()

	// Read-modify-write under the same lock the strip uses, so two rapid
	// dismissals cannot each write a list missing the other's entry.
	s.perfPicks.mu.Lock()
	defer s.perfPicks.mu.Unlock()

	var raw string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = ?`, dismissedPerformersKey).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusInternalServerError, "could not read dismissals")
		return
	}
	var ids []string
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &ids)
	}
	// Newest first, and de-duplicated: re-dismissing someone should refresh
	// their position rather than grow the list.
	next := make([]string, 0, len(ids)+1)
	if !req.Undo {
		next = append(next, req.StashDBID)
	}
	for _, id := range ids {
		if id == req.StashDBID || len(next) >= dismissedCap {
			continue
		}
		next = append(next, id)
	}
	enc, _ := json.Marshal(next)
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		dismissedPerformersKey, string(enc)); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not save dismissal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "dismissed": len(next)})
}
