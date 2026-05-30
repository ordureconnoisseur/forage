package api

import (
	"context"
	"database/sql"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/ordureconnoisseur/forager/internal/matcher"
	"github.com/ordureconnoisseur/forager/internal/prowlarr"
	"github.com/ordureconnoisseur/forager/internal/scoring"
	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

type sceneReleasesResponse struct {
	Scene struct {
		StashDBID  string             `json:"stashdb_id"`
		Title      string             `json:"title"`
		Date       string             `json:"date,omitempty"`
		Studio     string             `json:"studio,omitempty"`
		ImageURL   string             `json:"image_url,omitempty"`
		Performers []missingPerformer `json:"performers"`
	} `json:"scene"`
	Releases []sceneRelease `json:"releases"`
}

type sceneRelease struct {
	Title       string  `json:"title"`
	Indexer     string  `json:"indexer"`
	Protocol    string  `json:"protocol"`
	Size        int64   `json:"size"`
	Popularity  int     `json:"popularity"`
	Seeders     int     `json:"seeders"`
	Grabs       int     `json:"grabs"`
	PublishDate string  `json:"publish_date"`
	InfoURL     string  `json:"info_url"`
	DownloadURL string  `json:"download_url"`
	Verified    bool    `json:"verified"` // matcher confirms this release is the target scene
	Confidence  float64 `json:"confidence"`
	// When a release is NOT verified because the matcher thinks it's a
	// different scene, these name that scene so the UI can warn the
	// user ("this looks like X, not the scene you're viewing").
	BestMatchID    string  `json:"best_match_id,omitempty"`
	BestMatchTitle string  `json:"best_match_title,omitempty"`
	BestMatchConf  float64 `json:"best_match_conf,omitempty"`
	// Reasons is the matcher's per-component breakdown for the viewed
	// scene against this release (performers/studio/date/title/tracks) —
	// the same strings the matcher scores on. Drives the "why did this
	// match?" expander. Empty when the viewed scene wasn't a candidate.
	Reasons []string `json:"reasons,omitempty"`
	// Score is the user's release-preference score for this release (sum
	// of matched rules); Rejected is true when a reject rule matched (the
	// release is hidden from auto-selection). ScoreHits is the per-rule
	// breakdown for the "why this score?" display.
	Score     int           `json:"score"`
	Rejected  bool          `json:"rejected,omitempty"`
	ScoreHits []scoring.Hit `json:"score_hits,omitempty"`
}

// getSceneReleases finds Prowlarr releases for a specific StashDB
// scene. This is the scene-targeted lookup the Stash plugin uses when
// the user clicks a missing-scene card.
//
//	GET /scenes/{stashdb_id}/releases?min_seeders=N
//
// Flow:
//  1. Look up the scene's metadata (title, performers, studio, date).
//  2. Query Prowlarr targeted on the scene title — this is what Phase
//     3 of /search already does well, just promoted to the headline
//     query rather than a refinement.
//  3. Run the matcher in verification mode: a release is `verified`
//     when its top candidate's scene_id matches the target.
//  4. Return releases ranked verified-first, then popularity-desc.
func (s *Server) getSceneReleases(w http.ResponseWriter, r *http.Request) {
	prowlarrC := s.pool.Prowlarr()
	stashDBC := s.pool.StashDB()
	if prowlarrC == nil || stashDBC == nil {
		writeErr(w, http.StatusServiceUnavailable, "prowlarr and stashdb must be configured (see Settings)")
		return
	}
	prowlarrCats := s.pool.Settings().ProwlarrCategories
	id := chi.URLParam(r, "id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "scene id required")
		return
	}
	// Optional context: the performer whose page the user came from, and
	// an explicit alias override to retry under a specific spelling.
	ctxPerformer := strings.TrimSpace(r.URL.Query().Get("performer"))
	aliasOverride := strings.TrimSpace(r.URL.Query().Get("alias"))
	// lean mode: the collection fan-out runs this endpoint for EVERY
	// missing scene concurrently, so the full multi-spelling × studio/year
	// query set (6+ per scene) overwhelms Prowlarr/the trackers — context
	// deadlines, mass "search failed". In lean mode we emit far fewer
	// queries per scene (the collection already covers the performer
	// broadly across all their scenes, so per-scene breadth is wasteful).
	lean := r.URL.Query().Get("lean") == "1"

	scene, err := stashDBC.FindScene(r.Context(), id)
	if err != nil {
		s.log.Error("findScene", "err", err)
		writeErr(w, http.StatusBadGateway, "stashdb: "+err.Error())
		return
	}
	if scene == nil {
		writeErr(w, http.StatusNotFound, "scene not found on stashdb")
		return
	}
	if scene.Title == "" {
		writeErr(w, http.StatusUnprocessableEntity, "scene has no title to search by")
		return
	}

	m, err := s.Matcher(r.Context())
	if err != nil {
		s.log.Error("matcher init", "err", err)
		writeErr(w, http.StatusServiceUnavailable, "matcher unavailable: "+err.Error())
		return
	}

	// Build the performer query names, scoped to the ONE performer the
	// user is browsing (not every performer on the scene — that dragged in
	// the male lead and missed the female-named release). We try that
	// performer's library name AND their StashDB spellings (canonical +
	// scene-credited "as"), because a tracker may index under any of them
	// — e.g. owned as "Summer Cline" but released as "Summer Kline". An
	// explicit alias override (manual retry) takes precedence. Falls back
	// to the scene's own performers when there's no context performer.
	perfNames := s.scenePerformerNames(r.Context(), scene, ctxPerformer, aliasOverride)

	// Fan out several scene-derived Prowlarr queries, not just the title.
	// Trackers (esp. PornoLab) name a release by studio + performer, NOT
	// the StashDB marketing title, so a title-only query returns nothing
	// for those. The matcher then verifies which hits are this scene, so
	// over-broad queries are harmless. Queries run concurrently; results
	// merge + dedup by grab URL.
	releases, err := s.searchSceneReleases(r.Context(), prowlarrC, scene, perfNames, prowlarrCats, lean)
	if err != nil {
		s.log.Error("prowlarr search", "err", err, "scene", scene.Title)
		writeErr(w, http.StatusBadGateway, "prowlarr: "+err.Error())
		return
	}

	// Verify which releases are this scene + shape them for the UI.
	out := s.verifyReleases(r.Context(), m, id, scene.Title, releases)

	// Rank: verified-first, then by the user's preference SCORE (the
	// quality ranking — x265/1080p/etc.), then popularity as the final
	// tiebreaker. Rejected releases sort to the bottom of their group so
	// they're visible but never lead.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Verified != out[j].Verified {
			return out[i].Verified
		}
		if out[i].Rejected != out[j].Rejected {
			return !out[i].Rejected // non-rejected first
		}
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Popularity > out[j].Popularity
	})

	resp := sceneReleasesResponse{Releases: out}
	resp.Scene.StashDBID = scene.ID
	resp.Scene.Title = scene.Title
	resp.Scene.Date = scene.Date
	if scene.Studio != nil {
		resp.Scene.Studio = scene.Studio.Name
	}
	if len(scene.Images) > 0 {
		resp.Scene.ImageURL = scene.Images[0].URL
	}
	for _, p := range scene.Performers {
		resp.Scene.Performers = append(resp.Scene.Performers, missingPerformer{
			Name: p.Name,
			As:   p.As,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// maxPerformerNames caps how many spellings of the browsed performer we
// query, so we don't explode the fan-out chasing every alias.
const maxPerformerNames = 3

// scenePerformerNames returns the spellings to query for, scoped to the
// performer the user is browsing. Priority:
//  1. an explicit alias override (manual retry) — used alone;
//  2. else the browsed performer's library name + their StashDB
//     canonical/credited spellings (a tracker may use any of them);
//  3. else (no context performer) fall back to the scene's own
//     performers, capped.
//
// Capped at maxPerformerNames.
func (s *Server) scenePerformerNames(ctx context.Context, scene *stashdb.Scene, ctxPerformer, aliasOverride string) []string {
	if aliasOverride != "" {
		return []string{aliasOverride}
	}

	seen := map[string]bool{}
	var out []string
	add := func(n string) {
		n = strings.TrimSpace(n)
		if n == "" || seen[strings.ToLower(n)] || len(out) >= maxPerformerNames {
			return
		}
		seen[strings.ToLower(n)] = true
		out = append(out, n)
	}

	if ctxPerformer != "" {
		// The library name first (what the user calls them).
		add(ctxPerformer)
		// Then this performer's StashDB spellings on THIS scene — match by
		// the owned performer's cross-id so we pick the right person, not
		// every performer on the scene.
		if sid, _ := s.performerStashDBIDByName(ctx, ctxPerformer); sid != "" {
			for _, p := range scene.Performers {
				if p.ID == sid {
					add(p.Name) // StashDB canonical
					add(p.As)   // scene-credited spelling
				}
			}
		}
		return out
	}

	// No context performer (e.g. opened from Discover without one): fall
	// back to the scene's listed performers.
	for _, p := range scene.Performers {
		add(p.Name)
	}
	return out
}

// performerStashDBIDByName resolves a local performer's StashDB cross-id
// from performer_cache by display name (case-insensitive). "" if unknown.
func (s *Server) performerStashDBIDByName(ctx context.Context, name string) (string, error) {
	var sid sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT stashdb_id FROM performer_cache WHERE name = ? COLLATE NOCASE AND stashdb_id != '' LIMIT 1`,
		name).Scan(&sid)
	if err != nil {
		return "", err
	}
	return sid.String, nil
}

// sceneSearchTerms derives the Prowlarr query strings from a scene plus
// the chosen performer name spellings. Per (performer × studio) and
// (performer × year), plus the title and each bare performer name.
// Deduped, blanks dropped.
//
// lean trims to the two most-productive terms (primary performer × studio,
// and the title) — for the collection fan-out, which runs this for every
// missing scene concurrently and can't afford 6 queries each.
func sceneSearchTerms(scene *stashdb.Scene, perfNames []string, lean bool) []string {
	studio := ""
	if scene.Studio != nil {
		studio = scene.Studio.Name
	}
	year := ""
	if len(scene.Date) >= 4 {
		year = scene.Date[:4]
	}

	var candidates []string
	if lean {
		// Just enough to find the scene without flooding Prowlarr: the
		// primary performer + studio (the most productive single query),
		// and the title.
		if len(perfNames) > 0 && studio != "" {
			candidates = append(candidates, perfNames[0]+" "+studio)
		} else if len(perfNames) > 0 {
			candidates = append(candidates, perfNames[0])
		}
		candidates = append(candidates, scene.Title)
	} else {
		for _, p := range perfNames {
			if studio != "" {
				candidates = append(candidates, p+" "+studio)
			}
			if year != "" {
				candidates = append(candidates, p+" "+year)
			}
		}
		candidates = append(candidates, scene.Title) // English-named trackers
		for _, p := range perfNames {
			candidates = append(candidates, p) // bare performer — broadest net
		}
	}

	seen := map[string]bool{}
	var out []string
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" || seen[strings.ToLower(c)] {
			continue
		}
		seen[strings.ToLower(c)] = true
		out = append(out, c)
	}
	return out
}

// verifyReleases runs the matcher over a set of Prowlarr releases and
// shapes each into a sceneRelease (verified flag, confidence, best-other
// scene, reason breakdown) for the target scene. Shared by the
// /scenes/{id}/releases endpoint and the collection-job worker so the
// verify/badge logic — and the stored candidate list a job exposes for
// re-pick — is identical to the interactive view.
func (s *Server) verifyReleases(ctx context.Context, m *matcher.Matcher, sceneID, sceneTitle string, releases []prowlarr.Release) []sceneRelease {
	scorer := s.releaseScorer()
	titles := make([]string, len(releases))
	for i, rel := range releases {
		titles[i] = rel.Title
	}
	out := make([]sceneRelease, len(releases))
	for res := range m.MatchStream(ctx, titles, searchMatcherConcurrency) {
		rel := releases[res.Index]
		var conf float64
		verified := false
		var bestOtherID, bestOtherTitle string
		var bestOtherConf float64
		var reasons []string
		if res.Err == nil && len(res.Candidates) > 0 {
			vr := matcher.Verify(res.Candidates, sceneID, sceneTitle, rel.Title)
			verified = vr.Verified
			conf = vr.Confidence
			for _, c := range res.Candidates {
				if c.Scene.ID == sceneID {
					reasons = c.Reasons
					break
				}
			}
			if top := res.Candidates[0]; !verified && top.Scene.ID != sceneID {
				bestOtherID = top.Scene.ID
				bestOtherTitle = top.Scene.Title
				bestOtherConf = top.Confidence
			}
		}
		sc := scorer.Score(rel.Title, rel.Indexer)
		out[res.Index] = sceneRelease{
			Title:          rel.Title,
			Indexer:        rel.Indexer,
			Protocol:       rel.Protocol,
			Size:           rel.Size,
			Popularity:     rel.Popularity,
			Seeders:        rel.Seeders,
			Grabs:          rel.Grabs,
			PublishDate:    rel.PublishDate,
			InfoURL:        rel.InfoURL,
			DownloadURL:    rel.GrabURL(),
			Verified:       verified,
			Confidence:     conf,
			BestMatchID:    bestOtherID,
			BestMatchTitle: bestOtherTitle,
			BestMatchConf:  bestOtherConf,
			Reasons:        reasons,
			Score:          sc.Score,
			Rejected:       sc.Rejected,
			ScoreHits:      sc.Hits,
		}
	}
	return out
}

// searchSceneReleases runs the scene's derived queries against Prowlarr
// concurrently and returns the merged, deduped release set. A single
// query failing doesn't fail the whole search — we return whatever the
// others found.
func (s *Server) searchSceneReleases(ctx context.Context, pc *prowlarr.Client, scene *stashdb.Scene, perfNames []string, cats []int, lean bool) ([]prowlarr.Release, error) {
	terms := sceneSearchTerms(scene, perfNames, lean)
	if len(terms) == 0 {
		return nil, nil
	}
	var (
		mu       sync.Mutex
		merged   []prowlarr.Release
		seen     = map[string]bool{}
		firstErr error
		wg       sync.WaitGroup
	)
	for _, term := range terms {
		wg.Add(1)
		go func(term string) {
			defer wg.Done()
			rels, err := pc.Search(ctx, term, cats)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				s.log.Warn("scene release query", "term", term, "err", err)
				return
			}
			for _, rel := range rels {
				key := rel.GrabURL()
				if key == "" {
					key = rel.Title
				}
				if seen[key] {
					continue
				}
				seen[key] = true
				merged = append(merged, rel)
			}
		}(term)
	}
	wg.Wait()
	// Only surface an error when every query failed (nothing to show).
	if len(merged) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return merged, nil
}
