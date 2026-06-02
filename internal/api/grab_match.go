package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/stash"
)

type grabMatchRequest struct {
	// Optional. A bare StashDB scene UUID or a stashdb.org/scenes/<uuid>
	// URL. When omitted, the grab's own prediction is applied.
	StashDBID string `json:"stashdb_id"`
}

// postGrabMatch manually links a grab's placed scene to a StashDB scene
// and applies that scene's metadata — the escape hatch for when phash
// identify couldn't match the file (the scene has no fingerprint on
// StashDB) but it IS the scene you intended. Applies title, date, urls,
// the StashDB cross-id, and the performers/studio that already exist in
// your library (missing ones are skipped — no entities are created).
//
//	POST /grabs/{id}/match   body: { "stashdb_id"?: "<uuid|url>" }
func (s *Server) postGrabMatch(w http.ResponseWriter, r *http.Request) {
	sdb := s.pool.StashDB()
	sc := s.pool.Stash()
	if sdb == nil || sc == nil {
		writeErr(w, http.StatusServiceUnavailable, "stash and stashdb must be configured")
		return
	}
	gid, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad grab id")
		return
	}
	grab, err := s.grabs.Get(r.Context(), gid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	if grab == nil {
		writeErr(w, http.StatusNotFound, "grab not found")
		return
	}
	if grab.Kind == "pack" {
		// A pack is many scenes; applying one StashDB scene's metadata to
		// it would resolve a single arbitrary member and overwrite it.
		// Pack scenes are identified individually by the confirm path (or
		// in Stash directly), not via this single-scene match.
		writeErr(w, http.StatusUnprocessableEntity, "manual match isn't supported for pack grabs — identify the individual scenes in Stash")
		return
	}

	var req grabMatchRequest
	// Body is optional (an empty body falls back to the grab's own
	// prediction), so io.EOF is fine — but a present-yet-malformed body is
	// a client error, not a silent fallback.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	target := extractStashDBID(req.StashDBID)
	if target == "" {
		target = grab.PredictedStashDBID
	}
	if target == "" {
		writeErr(w, http.StatusBadRequest, "no StashDB id to apply — this grab has no prediction; pass stashdb_id")
		return
	}
	if grab.PlacedPath == "" {
		writeErr(w, http.StatusUnprocessableEntity, "grab has no placed file yet")
		return
	}

	scene, err := sdb.FindScene(r.Context(), target)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "stashdb: "+err.Error())
		return
	}
	if scene == nil {
		writeErr(w, http.StatusNotFound, "scene not found on stashdb")
		return
	}

	local, err := sc.FindSceneByPathContains(r.Context(), lastPathSegment(grab.PlacedPath))
	if err != nil {
		writeErr(w, http.StatusBadGateway, "stash: "+err.Error())
		return
	}
	if local == nil {
		writeErr(w, http.StatusUnprocessableEntity, "file not found in Stash yet — wait for it to scan, then retry")
		return
	}

	apply := stash.SceneApply{
		StashID:  target,
		Endpoint: s.stashDBEndpoint(r.Context(), sc),
		Title:    scene.Title,
		Date:     scene.Date,
	}
	if len(scene.Images) > 0 {
		apply.CoverURL = scene.Images[0].URL
	}
	for _, u := range scene.URLs {
		if u.URL != "" {
			apply.URLs = append(apply.URLs, u.URL)
		}
	}
	var sdbPerfIDs []string
	for _, p := range scene.Performers {
		if p.ID != "" {
			sdbPerfIDs = append(sdbPerfIDs, p.ID)
		}
	}
	apply.PerformerIDs = s.localPerformerIDs(r.Context(), sdbPerfIDs)
	if scene.Studio != nil && scene.Studio.Name != "" {
		if sid, _ := sc.FindStudioIDByName(r.Context(), scene.Studio.Name); sid != "" {
			apply.StudioID = sid
		}
	}

	if err := sc.ApplySceneMetadata(r.Context(), local.ID, apply); err != nil {
		writeErr(w, http.StatusBadGateway, "apply: "+err.Error())
		return
	}

	// CAS-retry so a poller tick mid-request can't revert the manual match
	// (or be reverted by it).
	if err := s.applyGrabUpdate(r.Context(), gid, func(fresh *grabs.Grab) {
		fresh.ActualStashDBID = target
		fresh.Status = "confirmed"
		fresh.Reason = "manually matched to StashDB"
		fresh.ConfirmedAt = nowUnix()
	}); err != nil {
		s.log.Error("grab match update", "id", gid, "err", err)
	}
	s.log.Info("grab manually matched", "id", gid, "stashdb", target,
		"scene", local.ID, "performers_applied", len(apply.PerformerIDs), "studio", apply.StudioID != "")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                 true,
		"stashdb_id":         target,
		"title":              scene.Title,
		"performers_applied": len(apply.PerformerIDs),
		"studio_applied":     apply.StudioID != "",
	})
}

// localPerformerIDs maps StashDB performer ids to the local Stash
// performer ids via performer_cache. Unmapped (not-in-library)
// performers are dropped — "apply only what you already have".
func (s *Server) localPerformerIDs(ctx context.Context, stashdbIDs []string) []string {
	if len(stashdbIDs) == 0 {
		return nil
	}
	ph := make([]string, len(stashdbIDs))
	args := make([]any, len(stashdbIDs))
	for i, id := range stashdbIDs {
		ph[i] = "?"
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT stash_id FROM performer_cache WHERE stashdb_id IN ("+strings.Join(ph, ",")+")", args...)
	if err != nil {
		s.log.Warn("map performers", "err", err)
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil && id != "" {
			out = append(out, id)
		}
	}
	return out
}

// stashDBEndpoint resolves the StashDB stash-box endpoint Stash expects
// on a stash_id, preferring the user's configured box (so it matches
// exactly) and falling back to the canonical host.
func (s *Server) stashDBEndpoint(ctx context.Context, sc *stash.Client) string {
	if boxes, err := sc.StashBoxes(ctx); err == nil {
		for _, ep := range boxes {
			if strings.Contains(ep, stash.StashDBEndpointHost) {
				return ep
			}
		}
		if len(boxes) > 0 {
			return boxes[0]
		}
	}
	return "https://" + stash.StashDBEndpointHost + "/graphql"
}

// extractStashDBID pulls a bare scene UUID out of either a raw id or a
// stashdb.org/scenes/<uuid> URL (with optional query/fragment).
func extractStashDBID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.LastIndex(s, "/scenes/"); i >= 0 {
		s = s[i+len("/scenes/"):]
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	return strings.Trim(s, "/")
}

func lastPathSegment(p string) string {
	p = strings.TrimRight(p, "/\\")
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[i+1:]
	}
	return p
}
