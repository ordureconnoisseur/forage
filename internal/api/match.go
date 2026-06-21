package api

import (
	"encoding/json"
	"net/http"

	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/matcher"
	"github.com/ordureconnoisseur/forager/internal/pathmap"
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
	Image      string   `json:"image,omitempty"` // StashDB cover, for the pick-list thumbnail
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

// getGrabMatchCandidates runs the matcher against a grab's own name and returns
// ranked StashDB scene candidates — the pick-list a no-prediction / mismatched
// grab needs when phash couldn't link it, so the user chooses from suggestions
// (with confidence + matching tokens) instead of hunting a StashDB URL by hand.
// Read-only: it suggests, the existing POST /grabs/{id}/match applies the pick.
func (s *Server) getGrabMatchCandidates(w http.ResponseWriter, r *http.Request) {
	g, ok := s.grabByID(w, r)
	if !ok {
		return
	}
	name := grabMatchName(g)
	if name == "" {
		writeErr(w, http.StatusUnprocessableEntity, "grab has no name to match on")
		return
	}
	m, err := s.Matcher(r.Context())
	if err != nil {
		// notConfiguredErr -> 503 ("stashdb not configured"); else 503 generic.
		writeMappedErr(w, err, http.StatusServiceUnavailable)
		return
	}
	candidates, err := m.Match(r.Context(), name)
	if err != nil {
		s.log.Error("grab match candidates", "id", g.ID, "err", err)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, matchResponse{Candidates: candidatesToWire(candidates)})
}

// grabMatchName picks the most match-worthy name for a grab: the curated
// release title when present, else the placed file's basename, else the
// download client's name. The matcher extracts performer/studio/date/tokens
// from whichever it gets.
func grabMatchName(g *grabs.Grab) string {
	if g.ReleaseTitle != "" {
		return g.ReleaseTitle
	}
	if g.PlacedPath != "" {
		return pathmap.Base(g.PlacedPath)
	}
	return g.ClientName
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
	if len(c.Scene.Images) > 0 {
		mc.Image = c.Scene.Images[0].URL
	}
	return mc
}

func displayPerformer(p stashdb.ScenePerformer) string {
	if p.As != "" && p.As != p.Name {
		return p.Name + " (as " + p.As + ")"
	}
	return p.Name
}
