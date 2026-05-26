package matcher

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

// Matcher orchestrates Phase A: takes a release-name-shaped string and
// produces ranked StashDB scene candidates with confidence scores.
//
// Construct once at startup via New; call Match (single) or MatchStream
// (batch with intra-batch dedup + concurrency) many times.
type Matcher struct {
	stashDB           *stashdb.Client
	perfScanner       *Scanner
	studioScanner     *Scanner
	performerStashDB  map[string]string
	studioRealStashDB map[string]bool
}

type Candidate struct {
	Scene      stashdb.Scene
	Confidence float64
	Tracks     []string
	Reasons    []string
}

// BatchResult is one match result emitted by MatchStream. Index is the
// position in the input slice; Err is per-result, not fatal for the
// batch.
type BatchResult struct {
	Index      int
	Release    string
	Candidates []Candidate
	Err        error
}

// New constructs a Matcher. Loads performer + studio corpora from the
// SQLite caches and builds the lookup maps.
func New(ctx context.Context, db *sql.DB, stashDBClient *stashdb.Client) (*Matcher, error) {
	performers, err := LoadPerformers(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("load performers: %w", err)
	}
	studios, err := LoadStudios(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("load studios: %w", err)
	}
	perfMap, err := LoadPerformerStashDBMap(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("load performer cross-refs: %w", err)
	}

	studioReal := make(map[string]bool, len(studios))
	for _, s := range studios {
		if !strings.HasPrefix(s.ID, "stash:") {
			studioReal[s.ID] = true
		}
	}

	// Performer-name-studio suppression. See entity.go for the full
	// rationale; gist: stops "Angel Youngs" (performer) from being
	// detected as a studio just because she has a personal-brand
	// studio entry in the cache.
	performerNames := make(map[string]bool, len(performers)*2)
	for _, p := range performers {
		if p.Name != "" {
			performerNames[strings.ToLower(p.Name)] = true
		}
		for _, a := range p.Aliases {
			if a != "" {
				performerNames[strings.ToLower(a)] = true
			}
		}
	}
	studiosForScanner := make([]Entity, 0, len(studios))
	for _, s := range studios {
		if performerNames[strings.ToLower(s.Name)] {
			continue
		}
		studiosForScanner = append(studiosForScanner, s)
	}

	return &Matcher{
		stashDB:     stashDBClient,
		perfScanner: NewScanner(performers, DefaultScannerOptions()),
		// Studios use the looser CanonicalUnique rule so well-known
		// short studio names (Vixen, Blacked, Tushy) match even when
		// longer related studios share that token in their names.
		studioScanner:     NewScanner(studiosForScanner, StudioScannerOptions()),
		performerStashDB:  perfMap,
		studioRealStashDB: studioReal,
	}, nil
}

const maxCandidatesReturned = 10

// Match runs the full Phase A pipeline against a single release name.
// Internally fans Track A + Track B out concurrently, so call latency
// is bounded by the slower of the two StashDB roundtrips rather than
// their sum.
func (m *Matcher) Match(ctx context.Context, releaseName string) ([]Candidate, error) {
	return m.matchWithCache(ctx, releaseName, nil)
}

// MatchStream matches a batch of release names concurrently with
// intra-batch StashDB query deduplication. Results are emitted to the
// returned channel as each release completes (in arbitrary order); the
// channel is closed when the batch finishes or ctx is cancelled.
//
// concurrency caps simultaneous outstanding releases. Per-release work
// itself fans out two more StashDB calls in parallel; the effective
// peak in-flight HTTP request count is ~2*concurrency.
func (m *Matcher) MatchStream(ctx context.Context, releases []string, concurrency int) <-chan BatchResult {
	if concurrency < 1 {
		concurrency = 1
	}
	ch := make(chan BatchResult, concurrency)
	cache := newSessionCache()

	go func() {
		defer close(ch)
		sem := make(chan struct{}, concurrency)
		var wg sync.WaitGroup
		for i, rel := range releases {
			wg.Add(1)
			sem <- struct{}{}
			go func(idx int, r string) {
				defer wg.Done()
				defer func() { <-sem }()
				cands, err := m.matchWithCache(ctx, r, cache)
				select {
				case ch <- BatchResult{Index: idx, Release: r, Candidates: cands, Err: err}:
				case <-ctx.Done():
				}
			}(i, rel)
		}
		wg.Wait()
	}()

	return ch
}

func (m *Matcher) matchWithCache(ctx context.Context, releaseName string, cache *sessionCache) ([]Candidate, error) {
	tokens := Tokenize(releaseName)
	filteredTokens := filterTitleStopwords(tokens)

	localPerformerIDs := m.perfScanner.Match(releaseName)
	studioIDs := m.studioScanner.Match(releaseName)
	date := TopDate(releaseName)

	stashDBPerfIDs := make([]string, 0, len(localPerformerIDs))
	for _, id := range localPerformerIDs {
		if sid, ok := m.performerStashDB[id]; ok {
			stashDBPerfIDs = append(stashDBPerfIDs, sid)
		}
	}
	stashDBStudioIDs := make([]string, 0, len(studioIDs))
	for _, id := range studioIDs {
		if m.studioRealStashDB[id] {
			stashDBStudioIDs = append(stashDBStudioIDs, id)
		}
	}

	candidates := map[string]*Candidate{}
	var candMu sync.Mutex
	addScene := func(s stashdb.Scene, track string) {
		candMu.Lock()
		defer candMu.Unlock()
		c := candidates[s.ID]
		if c == nil {
			c = &Candidate{Scene: s}
			candidates[s.ID] = c
		}
		c.Tracks = appendOnce(c.Tracks, track)
	}

	var (
		errsMu sync.Mutex
		errs   []error
	)
	addErr := func(err error) {
		errsMu.Lock()
		errs = append(errs, err)
		errsMu.Unlock()
	}

	var wg sync.WaitGroup

	// Track A — possibly multiple queries (with-date narrow + no-date broad).
	if len(stashDBPerfIDs) > 0 || len(stashDBStudioIDs) > 0 {
		queries := []stashdb.SceneQuery{
			{PerformerIDs: stashDBPerfIDs, StudioIDs: stashDBStudioIDs, PerPage: 50},
		}
		if date != "" {
			queries = append([]stashdb.SceneQuery{
				{PerformerIDs: stashDBPerfIDs, StudioIDs: stashDBStudioIDs, Date: date, PerPage: 50},
			}, queries...)
		}
		for _, q := range queries {
			wg.Add(1)
			go func(q stashdb.SceneQuery) {
				defer wg.Done()
				scenes, err := m.queryScenesCached(ctx, q, cache)
				if err != nil {
					addErr(fmt.Errorf("track A: %w", err))
					return
				}
				for _, s := range scenes {
					addScene(s, "A")
				}
			}(q)
		}
	}

	// Track B — full-text searchScenes with tokenized release name.
	if len(tokens) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			term := strings.Join(tokens, " ")
			scenes, err := m.searchScenesCached(ctx, term, cache)
			if err != nil {
				addErr(fmt.Errorf("track B: %w", err))
				return
			}
			for _, s := range scenes {
				addScene(s, "B")
			}
		}()
	}

	wg.Wait()

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	// Score candidates.
	releaseTokenSet := tokenSet(filteredTokens)
	stashDBPerfSet := stringSet(stashDBPerfIDs)
	stashDBStudioSet := stringSet(stashDBStudioIDs)
	for _, c := range candidates {
		c.Confidence, c.Reasons = score(c, releaseTokenSet, stashDBPerfSet, stashDBStudioSet, date)
	}

	out := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, *c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Confidence > out[j].Confidence
	})
	if len(out) > maxCandidatesReturned {
		out = out[:maxCandidatesReturned]
	}
	return out, nil
}

// ── session cache ────────────────────────────────────────────────────
//
// sessionCache stores StashDB query results across the calls in one
// MatchStream batch. The same /search request often resolves the same
// performer-only Track A query across dozens of releases (Vixen scene,
// Brazzers scene, etc. all extract Angel Youngs as the performer but
// no studio or date) — caching collapses those into one HTTP call.
//
// Not thread-safe across batches; one per MatchStream invocation.

type sessionCache struct {
	mu sync.Mutex
	m  map[string][]stashdb.Scene
}

func newSessionCache() *sessionCache {
	return &sessionCache{m: map[string][]stashdb.Scene{}}
}

func (m *Matcher) queryScenesCached(ctx context.Context, q stashdb.SceneQuery, cache *sessionCache) ([]stashdb.Scene, error) {
	if cache == nil {
		res, err := m.stashDB.QueryScenes(ctx, q)
		if err != nil {
			return nil, err
		}
		return res.Scenes, nil
	}
	key := "qs|" + querySceneKey(q)
	cache.mu.Lock()
	if scenes, ok := cache.m[key]; ok {
		cache.mu.Unlock()
		return scenes, nil
	}
	cache.mu.Unlock()
	res, err := m.stashDB.QueryScenes(ctx, q)
	if err != nil {
		return nil, err
	}
	cache.mu.Lock()
	cache.m[key] = res.Scenes
	cache.mu.Unlock()
	return res.Scenes, nil
}

func (m *Matcher) searchScenesCached(ctx context.Context, term string, cache *sessionCache) ([]stashdb.Scene, error) {
	if cache == nil {
		return m.stashDB.SearchScenes(ctx, term, 50)
	}
	key := "ss|" + term
	cache.mu.Lock()
	if scenes, ok := cache.m[key]; ok {
		cache.mu.Unlock()
		return scenes, nil
	}
	cache.mu.Unlock()
	scenes, err := m.stashDB.SearchScenes(ctx, term, 50)
	if err != nil {
		return nil, err
	}
	cache.mu.Lock()
	cache.m[key] = scenes
	cache.mu.Unlock()
	return scenes, nil
}

func querySceneKey(q stashdb.SceneQuery) string {
	perfs := append([]string(nil), q.PerformerIDs...)
	sort.Strings(perfs)
	studios := append([]string(nil), q.StudioIDs...)
	sort.Strings(studios)
	return fmt.Sprintf("%s|%s|%s|%d|%d",
		strings.Join(perfs, ","),
		strings.Join(studios, ","),
		q.Date, q.Page, q.PerPage)
}

// ── scoring ──────────────────────────────────────────────────────────

const (
	weightPerformer = 0.40
	weightStudio    = 0.20
	weightDate      = 0.20
	weightTitle     = 0.20
	bothTracksBonus = 0.05
	minTitleScore   = 0.05
)

func score(c *Candidate, releaseTokens map[string]bool, perfSet, studioSet map[string]bool, releaseDate string) (float64, []string) {
	pScore, pReason := performerOverlap(c.Scene, perfSet)
	sScore, sReason := studioMatch(c.Scene, studioSet)
	dScore, dReason := dateProximity(c.Scene.Date, releaseDate)
	tScore, tReason := titleOverlap(c.Scene.Title, releaseTokens)

	total := weightPerformer*pScore + weightStudio*sScore + weightDate*dScore + weightTitle*tScore
	if len(c.Tracks) >= 2 {
		total += bothTracksBonus
	}
	if total > 1 {
		total = 1
	}
	if total < 0 {
		total = 0
	}

	reasons := []string{pReason, sReason, dReason, tReason, "tracks: " + strings.Join(c.Tracks, "+")}
	return total, reasons
}

func performerOverlap(scene stashdb.Scene, detected map[string]bool) (float64, string) {
	if len(detected) == 0 {
		return 0, "performers: none detected"
	}
	hit := 0
	for _, p := range scene.Performers {
		if detected[p.ID] {
			hit++
		}
	}
	frac := float64(hit) / float64(len(detected))
	return frac, fmt.Sprintf("performers: %d/%d", hit, len(detected))
}

func studioMatch(scene stashdb.Scene, detected map[string]bool) (float64, string) {
	if len(detected) == 0 {
		return 0, "studio: none detected"
	}
	if scene.Studio != nil && detected[scene.Studio.ID] {
		return 1, "studio: match"
	}
	return 0, "studio: no-match"
}

func dateProximity(sceneDate, releaseDate string) (float64, string) {
	if releaseDate == "" {
		return 0, "date: none detected"
	}
	if sceneDate == "" {
		return 0, "date: scene-missing"
	}
	a, errA := time.Parse("2006-01-02", releaseDate)
	b, errB := time.Parse("2006-01-02", sceneDate)
	if errA != nil || errB != nil {
		return 0, "date: parse-error"
	}
	delta := a.Sub(b).Hours() / 24
	if delta < 0 {
		delta = -delta
	}
	d := int(delta + 0.5)
	switch {
	case d == 0:
		return 1.0, "date: exact"
	case d <= 1:
		return 0.7, "date: ±1d"
	case d <= 7:
		return 0.4, "date: ±7d"
	}
	return 0, fmt.Sprintf("date: %dd off", d)
}

// titleOverlap is Jaccard on stopword-filtered tokens. Stopword
// filtering matters because release names carry a lot of technical
// noise (resolution / container / tracker tags) that dilutes the
// union when present on the release side but absent from the scene
// title.
func titleOverlap(sceneTitle string, releaseTokens map[string]bool) (float64, string) {
	if sceneTitle == "" || len(releaseTokens) == 0 {
		return minTitleScore, "title: n/a"
	}
	sceneTokens := tokenSet(filterTitleStopwords(Tokenize(sceneTitle)))
	if len(sceneTokens) == 0 {
		return minTitleScore, "title: n/a"
	}
	inter := 0
	for t := range sceneTokens {
		if releaseTokens[t] {
			inter++
		}
	}
	union := len(sceneTokens) + len(releaseTokens) - inter
	if union == 0 {
		return minTitleScore, "title: n/a"
	}
	j := float64(inter) / float64(union)
	if j < minTitleScore {
		j = minTitleScore
	}
	return j, fmt.Sprintf("title: %.2f", j)
}

// ── helpers ──────────────────────────────────────────────────────────

func tokenSet(tokens []string) map[string]bool {
	out := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		out[t] = true
	}
	return out
}

func stringSet(s []string) map[string]bool {
	out := make(map[string]bool, len(s))
	for _, v := range s {
		out[v] = true
	}
	return out
}

func appendOnce(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

// titleStopwords are tokens that contribute noise rather than signal
// in title-overlap comparison: container/codec/quality markers, common
// TLD fragments, and a handful of release-tag dialects. Scene-tag
// words like "anal" or "blowjob" are deliberately NOT in here — they
// genuinely appear in scene titles and removing them would lose recall.
var titleStopwords = map[string]bool{
	// containers + codecs + sources
	"mp4": true, "mkv": true, "avi": true, "wmv": true, "mov": true, "ts": true, "m4v": true,
	"x264": true, "x265": true, "h264": true, "h265": true, "hevc": true, "av1": true,
	"web": true, "webrip": true, "webdl": true, "bdrip": true, "bluray": true, "bd": true,
	"dvdrip": true, "hdrip": true, "siterip": true, "vertical": true,
	// resolution / quality
	"sd": true, "hd": true, "fhd": true, "uhd": true,
	"480p": true, "720p": true, "1080p": true, "2160p": true,
	"4k": true, "8k": true,
	// generic noise
	"xxx": true, "com": true, "net": true, "org": true,
	"г": true, // Cyrillic year marker found in PornoLab feeds
	// scene-release group separator
	"rarbg": true,
}

func filterTitleStopwords(tokens []string) []string {
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if titleStopwords[t] {
			continue
		}
		out = append(out, t)
	}
	return out
}
