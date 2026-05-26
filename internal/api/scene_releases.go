package api

import (
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"
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

	// Targeted Prowlarr query — scene title is the signal.
	releases, err := prowlarrC.Search(r.Context(), scene.Title, prowlarrCats)
	if err != nil {
		s.log.Error("prowlarr search", "err", err, "scene", scene.Title)
		writeErr(w, http.StatusBadGateway, "prowlarr: "+err.Error())
		return
	}

	// Run matcher across all results to verify which are this scene.
	// Use the same concurrency/streaming infrastructure as /search
	// but consume the channel synchronously (small result set + we
	// only return after all done).
	titles := make([]string, len(releases))
	for i, rel := range releases {
		titles[i] = rel.Title
	}

	out := make([]sceneRelease, len(releases))
	for res := range m.MatchStream(r.Context(), titles, searchMatcherConcurrency) {
		rel := releases[res.Index]
		var conf float64
		verified := false
		// Verification: look for the target scene in the candidate
		// list. We accept anywhere in the top-N because a release
		// might match this scene strongly but the matcher's broader
		// pool put another scene first by a small margin.
		if res.Err == nil {
			for _, c := range res.Candidates {
				if c.Scene.ID == id {
					verified = true
					conf = c.Confidence
					break
				}
			}
		}
		out[res.Index] = sceneRelease{
			Title:       rel.Title,
			Indexer:     rel.Indexer,
			Protocol:    rel.Protocol,
			Size:        rel.Size,
			Popularity:  rel.Popularity,
			Seeders:     rel.Seeders,
			Grabs:       rel.Grabs,
			PublishDate: rel.PublishDate,
			InfoURL:     rel.InfoURL,
			DownloadURL: rel.DownloadURL,
			Verified:    verified,
			Confidence:  conf,
		}
	}

	// Verified-first, then popularity-desc within each group.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Verified != out[j].Verified {
			return out[i].Verified
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

