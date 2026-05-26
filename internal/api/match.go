package api

import (
	"encoding/json"
	"net/http"

	"github.com/ordureconnoisseur/forager/internal/matcher"
	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

type matchRequest struct {
	ReleaseName string `json:"release_name"`
}

type matchResponse struct {
	Candidates []matchCandidate `json:"candidates"`
}

type matchCandidate struct {
	SceneID    string   `json:"scene_id"`
	Title      string   `json:"title"`
	Studio     string   `json:"studio,omitempty"`
	Date       string   `json:"date,omitempty"`
	Performers []string `json:"performers,omitempty"`
	Confidence float64  `json:"confidence"`
	Tracks     []string `json:"tracks"`
	Reasons    []string `json:"reasons"`
	URL        string   `json:"url,omitempty"`
}

func (s *Server) postMatch(w http.ResponseWriter, r *http.Request) {
	var req matchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if req.ReleaseName == "" {
		writeErr(w, http.StatusBadRequest, "release_name required")
		return
	}

	m, err := s.Matcher(r.Context())
	if err != nil {
		s.log.Error("matcher init", "err", err)
		writeErr(w, http.StatusServiceUnavailable, "matcher unavailable: "+err.Error())
		return
	}

	candidates, err := m.Match(r.Context(), req.ReleaseName)
	if err != nil {
		s.log.Error("match", "err", err, "release", req.ReleaseName)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, matchResponse{
		Candidates: candidatesToWire(candidates),
	})
}

func candidatesToWire(cs []matcher.Candidate) []matchCandidate {
	out := make([]matchCandidate, 0, len(cs))
	for _, c := range cs {
		out = append(out, candidateToWire(c))
	}
	return out
}

func candidateToWire(c matcher.Candidate) matchCandidate {
	mc := matchCandidate{
		SceneID:    c.Scene.ID,
		Title:      c.Scene.Title,
		Date:       c.Scene.Date,
		Confidence: c.Confidence,
		Tracks:     c.Tracks,
		Reasons:    c.Reasons,
	}
	if c.Scene.Studio != nil {
		mc.Studio = c.Scene.Studio.Name
	}
	for _, p := range c.Scene.Performers {
		mc.Performers = append(mc.Performers, displayPerformer(p))
	}
	// Prefer the first URL as a clickable link out — StashDB scenes
	// usually carry a single canonical URL.
	if len(c.Scene.URLs) > 0 {
		mc.URL = c.Scene.URLs[0].URL
	}
	return mc
}

func displayPerformer(p stashdb.ScenePerformer) string {
	if p.As != "" && p.As != p.Name {
		return p.Name + " (as " + p.As + ")"
	}
	return p.Name
}
