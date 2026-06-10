package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/ordureconnoisseur/forager/internal/prowlarr"
)

type searchPerformer struct {
	StashID    string   `json:"stash_id"`
	Name       string   `json:"name"`
	Aliases    []string `json:"aliases"`
	SceneCount int      `json:"scene_count"`
}

type searchRelease struct {
	Index       int              `json:"index"`
	Phase       string           `json:"phase"` // "1" | "2:<studio>" | "3:<scene-title>"
	Title       string           `json:"title"`
	Indexer     string           `json:"indexer"`
	Protocol    string           `json:"protocol"`
	Size        int64            `json:"size"`
	Popularity  int              `json:"popularity"`
	Seeders     int              `json:"seeders"`
	Grabs       int              `json:"grabs"`
	PublishDate string           `json:"publish_date"`
	InfoURL     string           `json:"info_url"`
	DownloadURL string           `json:"download_url"`
	Categories  []int            `json:"categories"`
	Candidates  []matchCandidate `json:"candidates"`
}

const (
	searchMatcherConcurrency = 8
	// Coverage-vs-cost knobs for phase 2/3 refinement. Each refinement
	// is one Prowlarr query + matcher fanout on its results; we cap to
	// keep latency bounded for searches that already have many high-
	// confidence hits.
	maxStudioRefinements = 5
	maxSceneRefinements  = 10
	// Only refine when phase-1 produced a top-candidate at or above this
	// confidence — sub-0.85 hits are too speculative to spend extra
	// queries chasing variants of.
	refinementConfidence = 0.85
)

// getSearch streams a three-phase progressive Prowlarr search:
//
//	Phase 1 — broad performer query (today's behaviour). Streams as it
//	          completes; collects studios + scene titles from high-
//	          confidence matches for refinement.
//	Phase 2 — for each unique studio in phase-1 high-confidence hits,
//	          query "Performer + Studio" (parallel, capped). Captures
//	          studio-specific older releases the date-sorted broad
//	          query missed.
//	Phase 3 — for each unique scene title in phase-1 high-confidence
//	          hits, query "<scene title>" (parallel, capped). Captures
//	          all resolution variants of the predicted scene — slots
//	          them into the same scene-group in the UI.
//
// Releases are deduped globally by Prowlarr's infoUrl. SSE event types:
//
//	meta, release, refining, error, done
//
// The UI groups releases by their top-candidate scene_id; phase 3
// results automatically join existing scene groups for visual dedup.
func (s *Server) getSearch(w http.ResponseWriter, r *http.Request) {
	prowlarrC := s.pool.Prowlarr()
	if prowlarrC == nil {
		writeErr(w, http.StatusServiceUnavailable, "prowlarr not configured (see Settings)")
		return
	}
	prowlarrCats := s.pool.Settings().ProwlarrCategories

	stashID := r.URL.Query().Get("performer")
	if stashID == "" {
		writeErr(w, http.StatusBadRequest, "performer query param required")
		return
	}
	minSeeders, _ := strconv.Atoi(r.URL.Query().Get("min_seeders"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 200
	}

	perf, err := loadPerformerByID(r.Context(), s.db, stashID)
	if err == sql.ErrNoRows {
		writeErr(w, http.StatusNotFound, "performer not found")
		return
	}
	if err != nil {
		s.log.Error("performer lookup", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}

	m, err := s.Matcher(r.Context())
	if err != nil {
		s.log.Error("matcher init", "err", err)
		writeErr(w, http.StatusServiceUnavailable, "matcher unavailable: "+err.Error())
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// SSE writes need serialisation: phase 2 and 3 fire in parallel
	// goroutines, all calling writeEvent on the same response writer.
	var emitMu sync.Mutex
	writeEvent := func(event string, payload any) {
		body, err := json.Marshal(payload)
		if err != nil {
			s.log.Error("sse marshal", "event", event, "err", err)
			return
		}
		emitMu.Lock()
		defer emitMu.Unlock()
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body)
		flusher.Flush()
	}

	// ── Phase 1 — broad performer query ───────────────────────────
	p1Releases, err := prowlarrC.Search(r.Context(), perf.Name, prowlarrCats)
	if err != nil {
		s.log.Error("prowlarr search (phase 1)", "err", err)
		writeErr(w, http.StatusBadGateway, "prowlarr: "+err.Error())
		return
	}
	p1Filtered := filterReleases(p1Releases, minSeeders, limit)

	// Global dedup set across all phases — Prowlarr's infoUrl is the
	// stable per-release key (different indexers may produce different
	// download_urls for the same release, so this is the right field).
	seenURLs := make(map[string]bool, len(p1Filtered)*3)
	var seenMu sync.Mutex
	for _, rel := range p1Filtered {
		seenURLs[rel.InfoURL] = true
	}

	writeEvent("meta", map[string]any{
		"performer": searchPerformer{
			StashID:    perf.StashID,
			Name:       perf.Name,
			Aliases:    perf.Aliases,
			SceneCount: perf.SceneCount,
		},
		"query":         perf.Name,
		"release_count": len(p1Filtered),
	})

	if len(p1Filtered) == 0 {
		writeEvent("done", map[string]any{"count": 0})
		return
	}

	// Collect refinement signals from phase-1 high-confidence matches.
	studios := map[string]struct{}{}
	sceneTitles := map[string]struct{}{}
	var collectMu sync.Mutex

	// runMatcherAndEmit runs the matcher over a slice of releases,
	// emits each result via SSE, and (in phase 1 only) harvests
	// refinement signals.
	runMatcherAndEmit := func(releases []prowlarr.Release, phase string, collect bool) {
		if len(releases) == 0 {
			return
		}
		titles := make([]string, len(releases))
		for i, rel := range releases {
			titles[i] = rel.Title
		}
		ch := m.MatchStream(r.Context(), titles, searchMatcherConcurrency)
		for res := range ch {
			if res.Err != nil {
				writeEvent("error", map[string]any{
					"phase":   phase,
					"release": res.Release,
					"error":   res.Err.Error(),
				})
				continue
			}
			cands := res.Candidates
			if len(cands) > 3 {
				cands = cands[:3]
			}
			rel := releases[res.Index]
			writeEvent("release", searchRelease{
				Index:       res.Index,
				Phase:       phase,
				Title:       rel.Title,
				Indexer:     rel.Indexer,
				Protocol:    rel.Protocol,
				Size:        rel.Size,
				Popularity:  rel.Popularity,
				Seeders:     rel.Seeders,
				Grabs:       rel.Grabs,
				PublishDate: rel.PublishDate,
				InfoURL:     rel.InfoURL,
				DownloadURL: rel.GrabURL(), // magnet fallback for magnet-only indexers (TPB)
				Categories:  rel.Categories,
				Candidates:  candidatesToWire(cands),
			})

			if collect && len(cands) > 0 && cands[0].Confidence >= refinementConfidence {
				top := cands[0]
				collectMu.Lock()
				if top.Scene.Studio != nil && top.Scene.Studio.Name != "" {
					studios[top.Scene.Studio.Name] = struct{}{}
				}
				if isRefinableTitle(top.Scene.Title) {
					sceneTitles[top.Scene.Title] = struct{}{}
				}
				collectMu.Unlock()
			}
		}
	}

	runMatcherAndEmit(p1Filtered, "1", true)

	// ── Phase 2 + 3 — parallel refinement based on phase-1 signals ──
	studioList := capKeys(studios, maxStudioRefinements)
	sceneList := capKeys(sceneTitles, maxSceneRefinements)

	if len(studioList) > 0 || len(sceneList) > 0 {
		writeEvent("refining", map[string]any{
			"studios": studioList,
			"scenes":  sceneList,
		})
	}

	// refine fans out a single Prowlarr query, dedupes by infoUrl,
	// runs the matcher on the new releases, and emits them. Safe to
	// call concurrently — emitMu + seenMu protect the shared state.
	refine := func(query, phase string) {
		results, err := prowlarrC.Search(r.Context(), query, prowlarrCats)
		if err != nil {
			s.log.Warn("refinement search", "query", query, "err", err)
			writeEvent("error", map[string]any{"phase": phase, "error": err.Error()})
			return
		}
		var fresh []prowlarr.Release
		seenMu.Lock()
		for _, rel := range results {
			if rel.InfoURL == "" || seenURLs[rel.InfoURL] {
				continue
			}
			if seedersBelow(rel, minSeeders) {
				continue
			}
			seenURLs[rel.InfoURL] = true
			fresh = append(fresh, rel)
		}
		seenMu.Unlock()
		runMatcherAndEmit(fresh, phase, false)
	}

	var wg sync.WaitGroup
	for _, st := range studioList {
		wg.Add(1)
		go func(studio string) {
			defer wg.Done()
			refine(perf.Name+" "+studio, "2:"+studio)
		}(st)
	}
	for _, title := range sceneList {
		wg.Add(1)
		go func(t string) {
			defer wg.Done()
			refine(t, "3:"+t)
		}(title)
	}
	wg.Wait()

	writeEvent("done", map[string]any{"count": len(seenURLs)})
}

// filterReleases applies minSeeders + limit caps. Keeps Prowlarr's
// popularity-desc ordering from prowlarr.Client.Search.
func filterReleases(releases []prowlarr.Release, minSeeders, limit int) []prowlarr.Release {
	out := make([]prowlarr.Release, 0, len(releases))
	for _, rel := range releases {
		if seedersBelow(rel, minSeeders) {
			continue
		}
		out = append(out, rel)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// seedersBelow reports whether a release fails the min-seeders floor.
// The floor means "can this actually download right now": for torrents
// that's the live seeder count. Popularity is max(seeders, grabs) — a
// ranking signal — so filtering on it waved through dead torrents whose
// historical grabs cleared the floor. Usenet has no swarm; the floor
// doesn't apply there.
func seedersBelow(rel prowlarr.Release, minSeeders int) bool {
	return minSeeders > 0 && rel.Protocol != "usenet" && rel.Seeders < minSeeders
}

// isRefinableTitle filters out titles too generic to make useful
// queries — single-word titles, very short titles, etc. These would
// pull back 50 unrelated results that Prowlarr's relevance ranking
// can't distinguish.
func isRefinableTitle(t string) bool {
	t = strings.TrimSpace(t)
	if t == "" {
		return false
	}
	toks := strings.Fields(t)
	if len(toks) < 2 {
		return false
	}
	// Sum length of "distinctive" tokens (≥3 chars). 12 chars across
	// multiple tokens is roughly two real words.
	distinct := 0
	for _, tk := range toks {
		if len(tk) >= 3 {
			distinct += len(tk)
		}
	}
	return distinct >= 12
}

// capKeys returns up to n keys from a set, in deterministic-ish order
// (we don't bother sorting — caller doesn't care).
func capKeys(set map[string]struct{}, n int) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
		if len(out) >= n {
			break
		}
	}
	return out
}

// ── performer lookup (unchanged) ──────────────────────────────────

type cachedPerformer struct {
	StashID    string
	Name       string
	Aliases    []string
	SceneCount int
}

func loadPerformerByID(ctx context.Context, db *sql.DB, stashID string) (*cachedPerformer, error) {
	var p cachedPerformer
	var aliasesJSON sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT stash_id, name, aliases, scene_count FROM performer_cache WHERE stash_id = ?`,
		stashID,
	).Scan(&p.StashID, &p.Name, &aliasesJSON, &p.SceneCount)
	if err != nil {
		return nil, err
	}
	if aliasesJSON.Valid && aliasesJSON.String != "" && aliasesJSON.String != "null" {
		_ = json.Unmarshal([]byte(aliasesJSON.String), &p.Aliases)
	}
	return &p, nil
}
