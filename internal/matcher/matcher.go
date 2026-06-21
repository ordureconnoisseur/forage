package matcher

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

// tiebreakerEpsilon is the confidence granularity below which two
// candidates count as tied and fall through to the next sort key.
// Component weights are coarse (0.05 steps), so 1e-6 separates any
// genuinely different score while absorbing float noise.
const tiebreakerEpsilon = 1e-6

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
	// TitleOverlap is the stopword-filtered Jaccard between the
	// release's tokens and the scene title's tokens. Stored separately
	// (in addition to being a component of Confidence) so the sort
	// comparator can use it as a tiebreaker when two candidates have
	// confidences within tiebreakerEpsilon — the most-frequent failure
	// mode the bench surfaced was true ties resolved arbitrarily.
	TitleOverlap float64
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
	// All plausible date readings, not one guessed best — each candidate's
	// own scene date picks the right one at scoring time.
	dates := AllDates(releaseName)

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
		// One date-narrowed precision pass per distinct date reading
		// (US/EU/year-first). The session cache dedups identical queries, and
		// the broad performer query below still guarantees candidacy when
		// every reading is wrong — so adding readings only ever helps.
		for _, d := range dates {
			queries = append([]stashdb.SceneQuery{
				{PerformerIDs: stashDBPerfIDs, StudioIDs: stashDBStudioIDs, Date: d, PerPage: 50},
			}, queries...)
		}
		// Performer-only broad query. The performer+studio query above is
		// an exact studio-id AND, which silently drops the right scene
		// when StashDB files it under a sub-studio (e.g. an "EvilAngel …"
		// release whose scene sits under a network sub-studio). Without a
		// broad fallback the correct scene never becomes a candidate at
		// all — the bug behind standard-format releases scoring 0.00.
		// The studio still contributes to the score, so ranking is intact.
		if len(stashDBPerfIDs) > 0 && len(stashDBStudioIDs) > 0 {
			queries = append(queries, stashdb.SceneQuery{PerformerIDs: stashDBPerfIDs, PerPage: 50})
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

	// Degrade gracefully on a PARTIAL source failure. Track A fans out several
	// StashDB queries and Track B a full-text search; a transient failure in
	// one (a 502, a rate-limit on the broad fallback, a timeout) must not
	// discard the candidates the others already produced. Every caller treats a
	// non-nil error as "throw the candidates away" (match.go 500s, the stream
	// callers skip the release), so returning the join here silently tanks
	// recall on a blip. Only surface an error when we have NOTHING usable:
	// zero candidates AND at least one failure. With candidates in hand we
	// score and return them (a partial match beats none); zero candidates with
	// no errors is a legitimate "no match" and returns empty, nil.
	//
	// Note: a partial failure is not logged here (the matcher has no logger and
	// runs per-release in hot batches). The dropped queries are transient
	// client errors; classifying and retrying them at the client boundary is
	// the durable fix (see docs/error-handling.md, phases 1 and 3).
	if err := matchOutcomeErr(len(candidates), errs); err != nil {
		return nil, err
	}

	// Score candidates.
	releaseTokenSet := tokenSet(filteredTokens)
	stashDBPerfSet := stringSet(stashDBPerfIDs)
	stashDBStudioSet := stringSet(stashDBStudioIDs)
	releaseJAVCodes := stringSet(ExtractJAVCodes(releaseName))
	for _, c := range candidates {
		// Ordered, UNfiltered tokens for cast-name phrase matching — a
		// performer name must match as adjacent tokens, and stopword
		// filtering would both drop name tokens and create false adjacency.
		c.Confidence, c.Reasons, c.TitleOverlap = score(c, releaseTokenSet, tokens, stashDBPerfSet, stashDBStudioSet, dates, releaseJAVCodes)
	}

	out := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, *c)
	}
	rankCandidates(out)
	if len(out) > maxCandidatesReturned {
		out = out[:maxCandidatesReturned]
	}
	return out, nil
}

// matchOutcomeErr decides whether per-source failures should fail the whole
// match. The match fans out across several StashDB queries and a full-text
// search; one failing must not discard the candidates the others produced
// (every caller throws candidates away on a non-nil error). So a failure is
// fatal ONLY when nothing usable survived: zero candidates AND at least one
// error. Candidates in hand → degrade to success (nil). Zero candidates with
// no errors is a legitimate empty match, also nil.
func matchOutcomeErr(candidateCount int, errs []error) error {
	if candidateCount == 0 && len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// rankCandidates orders candidates best-first, deterministically:
// Confidence (descending), then TitleOverlap (descending), then Scene.ID
// (ascending). Exact confidence ties are real and common — sibling scenes
// sharing performer/studio/date whose titles both floor at the minimum
// title score — and candidates are collected from a map, so with
// confidence as the only key the top slot (which Verify's isTop and
// release verification's bestOther hang off) flipped run to run.
// Quantising by tiebreakerEpsilon keeps the comparator a strict weak
// ordering (a raw |a-b|<eps comparison isn't transitive); TitleOverlap
// breaks ties toward the title the release actually names, and the scene
// id pins whatever remains.
func rankCandidates(cands []Candidate) {
	rank := func(v float64) int64 { return int64(math.Round(v / tiebreakerEpsilon)) }
	sort.SliceStable(cands, func(i, j int) bool {
		if ci, cj := rank(cands[i].Confidence), rank(cands[j].Confidence); ci != cj {
			return ci > cj
		}
		if ti, tj := rank(cands[i].TitleOverlap), rank(cands[j].TitleOverlap); ti != tj {
			return ti > tj
		}
		return cands[i].Scene.ID < cands[j].Scene.ID
	})
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
	// castBonusMax is the most the cast-in-title signal can add to a
	// candidate's confidence, scaled by the fraction of the scene's
	// StashDB cast named in the release title. Additive (not a reweight)
	// so it can only lift a candidate that names the actual cast — the
	// multi-performer case raw title Jaccard underscores — without
	// disturbing the 1.0-normalised four-component base. Tuned on the
	// corpus (matcher-bench --verify).
	castBonusMax  = 0.10
	minTitleScore = 0.05
	// javCodeFloor: when release and scene title share a JAV studio
	// code (e.g. both contain `SNOS-233`), force confidence to at
	// least this value. JAV codes are uniquely allocated per studio,
	// so a code match is a near-deterministic identity. Set below 1.0
	// so a candidate with code-match AND strong other signals still
	// outranks code-match alone.
	javCodeFloor = 0.85
)

func score(c *Candidate, releaseTokens map[string]bool, orderedTokens []string, perfSet, studioSet map[string]bool, releaseDates []string, releaseJAVCodes map[string]bool) (float64, []string, float64) {
	pScore, pReason := performerOverlap(c.Scene, perfSet)
	sScore, sReason := studioMatch(c.Scene, studioSet)
	dScore, dReason := bestDateProximity(c.Scene.Date, releaseDates)
	tScore, tReason := titleOverlap(c.Scene.Title, releaseTokens)
	castScore, castReason := castInTitle(c.Scene, orderedTokens)

	total := weightPerformer*pScore + weightStudio*sScore + weightDate*dScore + weightTitle*tScore
	if len(c.Tracks) >= 2 {
		total += bothTracksBonus
	}
	total += castBonusMax * castScore

	javReason := ""
	if len(releaseJAVCodes) > 0 {
		for _, code := range ExtractJAVCodes(c.Scene.Title) {
			if releaseJAVCodes[code] {
				if total < javCodeFloor {
					total = javCodeFloor
				}
				javReason = "jav-code: " + code
				break
			}
		}
	}

	if total > 1 {
		total = 1
	}
	if total < 0 {
		total = 0
	}

	reasons := []string{pReason, sReason, dReason, tReason, "tracks: " + strings.Join(c.Tracks, "+")}
	if castReason != "" {
		reasons = append(reasons, castReason)
	}
	if javReason != "" {
		reasons = append(reasons, javReason)
	}
	return total, reasons, tScore
}

// castInTitle counts how many of the candidate scene's StashDB cast
// members (their performer name or scene alias) appear as a contiguous
// token phrase in the release title — independent of the local performer
// cache. This is the signal performerOverlap can't see: performerOverlap's
// denominator is only performers the user already owns (so a release
// naming three performers, two of them not in the library, scores a
// misleading 1/1), whereas the candidate scene from StashDB knows its
// full cast. A release that names most of a scene's actual cast is very
// likely that scene, which is exactly the multi-performer case raw title
// Jaccard dilutes. Returns the matched fraction (0..1) and a reason like
// "cast: 3/4 named". Empty cast → 0 with no signal.
func castInTitle(scene stashdb.Scene, releaseTokens []string) (float64, string) {
	if len(scene.Performers) == 0 || len(releaseTokens) == 0 {
		return 0, ""
	}
	hit := 0
	for _, p := range scene.Performers {
		// Try the canonical name first, then the scene-credited alias.
		if nameInTokens(p.Name, releaseTokens) || nameInTokens(p.As, releaseTokens) {
			hit++
		}
	}
	if hit == 0 {
		return 0, fmt.Sprintf("cast: 0/%d named", len(scene.Performers))
	}
	return float64(hit) / float64(len(scene.Performers)),
		fmt.Sprintf("cast: %d/%d named", hit, len(scene.Performers))
}

// nameInTokens reports whether a performer name appears as a contiguous
// run of tokens within releaseTokens (so "Lucia Rossi" matches adjacent
// "lucia","rossi", not a stray "lucia" plus a distant "rossi"). A single-
// token name must be >= 4 chars to avoid matching short common words.
func nameInTokens(name string, releaseTokens []string) bool {
	nameTokens := Tokenize(name)
	if len(nameTokens) == 0 {
		return false
	}
	if len(nameTokens) == 1 && len(nameTokens[0]) < 4 {
		return false
	}
	if len(nameTokens) > len(releaseTokens) {
		return false
	}
	for i := 0; i+len(nameTokens) <= len(releaseTokens); i++ {
		ok := true
		for j := range nameTokens {
			if releaseTokens[i+j] != nameTokens[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
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

// bestDateProximity scores the candidate's scene date against every plausible
// reading of the release date and keeps the closest. The candidate's known
// date disambiguates the release's ambiguous one — a US "09.11.26" matches a
// 2026-09-11 scene under MM.DD.YY, an EU release matches under DD.MM.YY — so
// we never have to globally guess the day/month/year order. This can only
// raise the correct scene's score (it gets the best of its readings) and
// can't decide a match on its own: date is a soft 20% weight applied only to
// candidates already surfaced by a performer/studio match, so a coincidental
// hit on a wrong reading is both improbable and non-decisive.
func bestDateProximity(sceneDate string, releaseDates []string) (float64, string) {
	if len(releaseDates) == 0 {
		return dateProximity(sceneDate, "")
	}
	best, bestReason := 0.0, "date: no reading matched"
	for _, rd := range releaseDates {
		s, reason := dateProximity(sceneDate, rd)
		if s > best || bestReason == "date: no reading matched" {
			best, bestReason = s, reason
		}
		if best == 1.0 {
			break // exact — can't do better
		}
	}
	return best, bestReason
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

// TitleContainment returns the fraction of the scene title's
// significant (stopword-filtered) tokens that appear in the release
// name, plus how many significant tokens the scene title has. 1.0 means
// the release name contains the entire scene title.
//
// Unlike titleOverlap (Jaccard, which penalises the release for its
// extra studio/performer/codec tokens), containment asks only "is the
// scene title ⊆ the release name" — the right question when verifying
// that a release IS a given scene. The release-verifier uses it as a
// direct signal independent of the cross-scene ranking, so an obvious
// exact-title match isn't mislabelled just because some other
// same-performer scene happened to rank #1.
func TitleContainment(sceneTitle, releaseName string) (float64, int) {
	st := tokenSet(filterTitleStopwords(Tokenize(sceneTitle)))
	if len(st) == 0 {
		return 0, 0
	}
	rel := tokenSet(filterTitleStopwords(Tokenize(releaseName)))
	hit := 0
	for t := range st {
		if rel[t] {
			hit++
		}
	}
	return float64(hit) / float64(len(st)), len(st)
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
	// resolution-suffix orphans — the case+digit tokenizer always
	// splits `1080p` → `1080`+`p`, so the full-string entries above
	// never actually fire. Cover the post-split forms too.
	"480": true, "720": true, "1080": true, "2160": true,
	"p": true,
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
