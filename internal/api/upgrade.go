package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/ordureconnoisseur/forager/internal/watches"
)

// postUpgradeWatches creates UPGRADE watches for a subject's owned
// scenes whose best copy sits below a cutoff height: the arr-style
// "cutoff upgrade", forage-shaped. Each watch stores the owned copy's
// height as its floor; the watch loop only flips it available for a
// release that BEATS the floor, and it graduates when an owned copy
// exceeds the floor (the upgrade landed and confirmed). Replacement is
// review-queue based: when the upgrade confirms, the scene has two
// copies and the duplicate-review flow surfaces the pair.
func (s *Server) postUpgradeWatches(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind      string `json:"kind"` // "performer" | "studio"
		StashDBID string `json:"stashdb_id"`
		Name      string `json:"name"`
		Cutoff    int    `json:"cutoff"` // height: watch owned scenes strictly below this
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if req.StashDBID == "" || req.Cutoff <= 0 {
		writeErr(w, http.StatusBadRequest, "stashdb_id and a positive cutoff required")
		return
	}
	if req.Kind != "studio" {
		req.Kind = "performer"
	}
	scenes, err := s.subScenes(r.Context(), req.Kind, req.StashDBID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "scene list: "+err.Error())
		return
	}
	owned, err := s.ownedSceneCopies(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "owned sweep: "+err.Error())
		return
	}
	// Existing watches must not be clobbered (Add upserts): an acquire
	// watch in flight outranks an upgrade wish, and an existing upgrade
	// watch keeps its original floor.
	existing := map[string]bool{}
	if wl, werr := s.watches.List(r.Context()); werr == nil {
		for _, wt := range wl {
			existing[wt.StashDBID] = true
		}
	}

	var fresh []watches.Watch
	skippedWatched, skippedAtCutoff := 0, 0
	for _, sc := range scenes {
		copies := owned[sc.ID]
		if len(copies) == 0 {
			continue // not owned: that is acquire territory, not upgrade
		}
		best := 0
		for _, c := range copies {
			if c.Height > best {
				best = c.Height
			}
		}
		if best == 0 || best >= req.Cutoff {
			skippedAtCutoff++
			continue // unknown quality, or already at/above the cutoff
		}
		if existing[sc.ID] {
			skippedWatched++
			continue
		}
		wt := watches.Watch{
			StashDBID:    sc.ID,
			Title:        sc.Title,
			Date:         sc.Date,
			UpgradeFloor: best,
			BatchID:      "upg:" + req.StashDBID,
			BatchLabel:   req.Name + " upgrades",
			Target:       watches.TargetAny,
			CreatedAt:    time.Now().Unix(),
		}
		if sc.Studio != nil {
			wt.StudioName = sc.Studio.Name
		}
		if len(sc.Images) > 0 {
			wt.ImageURL = sc.Images[0].URL
		}
		if req.Kind == "performer" {
			wt.PerformerName = req.Name
			wt.PerformerID = req.StashDBID
		} else if len(sc.Performers) > 0 {
			wt.PerformerName = sc.Performers[0].Name
		}
		for _, p := range sc.Performers {
			wt.Performers = append(wt.Performers, p.Name)
		}
		fresh = append(fresh, wt)
		existing[sc.ID] = true
	}
	if len(fresh) > 0 {
		if err := s.watches.AddBatch(r.Context(), fresh); err != nil {
			writeErr(w, http.StatusInternalServerError, "db")
			return
		}
	}
	s.log.Info("upgrade watches created", "subject", req.Name, "cutoff", req.Cutoff,
		"created", len(fresh), "already_watched", skippedWatched, "at_cutoff", skippedAtCutoff)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "created": len(fresh),
		"already_watched": skippedWatched, "at_or_above_cutoff": skippedAtCutoff,
	})
}
