package api

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Creating a local performer from a StashDB id.
//
// Discover mixes trending StashDB scenes into the feed, including scenes
// whose performers the user does not have. Those pills carry a "+": this is
// what it calls.
//
// The name is only how candidates are FETCHED from the stash-box; the
// StashDB id is how one is CHOSEN. Stash has no create-from-stash-box-id
// call (scrapeSinglePerformer's performer_id wants a LOCAL integer id, and
// scrapePerformerURL on a stashdb.org performer URL errors internally), so a
// name query is the only route — and a name query alone would be a coin
// flip between same-named performers. Nothing is created unless the
// requested id comes back among the candidates.

type addPerformerRequest struct {
	StashDBID string `json:"stashdb_id"`
	// Name is the search term for the stash-box query. Discover already has
	// it from the scene's cached performer list, so the client sends it
	// rather than making the daemon look it up again.
	Name string `json:"name"`
}

func (s *Server) postPerformerFromStashDB(w http.ResponseWriter, r *http.Request) {
	var req addPerformerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	req.StashDBID = strings.TrimSpace(req.StashDBID)
	req.Name = strings.TrimSpace(req.Name)
	if req.StashDBID == "" || req.Name == "" {
		writeErr(w, http.StatusBadRequest, "stashdb_id and name are both required")
		return
	}

	sc := s.pool.Stash()
	if sc == nil {
		writeErr(w, http.StatusServiceUnavailable, "Stash isn't configured")
		return
	}
	boxes, eerr := sc.StashBoxes(r.Context())
	endpoint := ""
	if eerr == nil && len(boxes) > 0 {
		endpoint = boxes[0]
	}
	if endpoint == "" {
		writeErr(w, http.StatusServiceUnavailable,
			"Stash has no StashDB endpoint configured, so it can't look this performer up")
		return
	}

	// Already have them? Adding again would create a duplicate performer
	// carrying the same cross-id, which is worse than doing nothing.
	if existing, err := localPerformerIDsByStashDBID(r.Context(), s.db); err == nil {
		if localID, ok := existing[req.StashDBID]; ok {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "already_present": true, "local_id": localID,
			})
			return
		}
	}

	scraped, err := sc.ScrapePerformerByStashDBID(r.Context(), endpoint, req.Name, req.StashDBID)
	if err != nil {
		s.log.Warn("add performer: scrape", "stashdb_id", req.StashDBID, "err", err)
		writeErr(w, http.StatusBadGateway, "couldn't reach StashDB through Stash: "+err.Error())
		return
	}
	if scraped == nil {
		// The name search didn't surface the requested id. Refusing beats
		// creating a same-named different performer in the user's library.
		writeErr(w, http.StatusNotFound,
			"StashDB didn't return that performer for this name; not guessing which one to create")
		return
	}

	localID, err := sc.CreatePerformerFromScrape(r.Context(), endpoint, req.StashDBID, scraped)
	if err != nil {
		s.log.Error("add performer: create", "stashdb_id", req.StashDBID, "err", err)
		writeErr(w, http.StatusBadGateway, "Stash rejected the new performer: "+err.Error())
		return
	}
	s.log.Info("performer created from StashDB", "name", scraped.Name,
		"stashdb_id", req.StashDBID, "local_id", localID)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "local_id": localID, "name": scraped.Name,
	})
}
