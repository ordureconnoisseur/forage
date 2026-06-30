package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/pathmap"
	"github.com/ordureconnoisseur/forager/internal/stash"
)

// packScene is one taggable scene in a pack: a placed file Stash scanned but
// could NOT cross-id against StashDB (amateur content). The reviewer shows these
// with their cover so the user can deselect a stray before applying.
type packScene struct {
	SceneID  string `json:"scene_id"` // LOCAL Stash scene id
	Title    string `json:"title,omitempty"`
	ImageURL string `json:"image_url"` // daemon-relative /img/scene/{id}/screenshot
}

type packScenesResponse struct {
	PerformerName       string      `json:"performer_name"`
	PerformerLocalID    string      `json:"performer_local_id,omitempty"`
	PerformerResolvable bool        `json:"performer_resolvable"`
	Scenes              []packScene `json:"scenes"`
}

// packNeedle derives the Stash-side path substring that scopes a pack's scenes,
// exactly as the poller's advancePackConfirm does: prefer the path-mapped placed
// dir (specific), fall back to its basename, then the client save-name. Empty
// when there's nothing to match on.
func packNeedle(g *grabs.Grab, mapping string) string {
	if g.PlacedPath != "" {
		if n := pathmap.Translate(g.PlacedPath, mapping); n != "" {
			return n
		}
		return pathmap.Base(g.PlacedPath)
	}
	return g.ClientName
}

// packUnidentifiedScenes returns the pack's scenes that have no StashDB cross-id
// — the only ones the performer tag applies to. Identified scenes are excluded
// (Stash's determination stands).
func (s *Server) packUnidentifiedScenes(ctx context.Context, g *grabs.Grab) ([]stash.SceneMatch, error) {
	sc := s.pool.Stash()
	if sc == nil {
		return nil, nil
	}
	needle := packNeedle(g, s.pool.Settings().StashPathMapping)
	if needle == "" {
		return nil, nil
	}
	all, err := sc.FindScenesUnderPath(ctx, needle)
	if err != nil {
		return nil, err
	}
	out := make([]stash.SceneMatch, 0, len(all))
	for _, m := range all {
		if m.StashDBID == "" { // unidentified — no StashDB cross-id
			out = append(out, m)
		}
	}
	return out, nil
}

// localPerformerIDByName maps a performer's display name (the grab's folder) to
// its LOCAL Stash id via performer_cache. Empty when the performer isn't in the
// user's library. Mirrors the lookup in grab_detail.go.
func (s *Server) localPerformerIDByName(ctx context.Context, name string) string {
	if name == "" {
		return ""
	}
	var id string
	_ = s.db.QueryRowContext(ctx,
		`SELECT stash_id FROM performer_cache WHERE name = ? COLLATE NOCASE AND stash_id != '' LIMIT 1`,
		name).Scan(&id)
	return id
}

// getPackScenes lists a pack grab's unidentified scenes (with covers) plus the
// performer the pack is filed under, for the grab-card "tag scenes" reviewer.
//
//	GET /grabs/{id}/pack-scenes
func (s *Server) getPackScenes(w http.ResponseWriter, r *http.Request) {
	g, ok := s.grabByID(w, r)
	if !ok {
		return
	}
	if g.Kind != "pack" {
		writeErr(w, http.StatusUnprocessableEntity, "not a pack grab")
		return
	}
	scenes, err := s.packUnidentifiedScenes(r.Context(), g)
	if err != nil {
		s.log.Warn("pack scenes enumerate", "grab_id", g.ID, "err", err)
		writeErr(w, http.StatusBadGateway, "stash: "+err.Error())
		return
	}
	localID := s.localPerformerIDByName(r.Context(), g.PerformerName)
	resp := packScenesResponse{
		PerformerName:       g.PerformerName,
		PerformerLocalID:    localID,
		PerformerResolvable: localID != "",
		Scenes:              make([]packScene, 0, len(scenes)),
	}
	for _, m := range scenes {
		resp.Scenes = append(resp.Scenes, packScene{
			SceneID:  m.ID,
			Title:    m.Title,
			ImageURL: "/img/scene/" + m.ID + "/screenshot",
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// postApplyPerformer adds the pack's performer to the selected scenes, additively.
// It re-enumerates the pack's unidentified scenes and only tags ids in that set,
// so a stale or forged request can never touch identified scenes or anything
// outside this pack.
//
//	POST /grabs/{id}/apply-performer  {"scene_ids": [...]}
func (s *Server) postApplyPerformer(w http.ResponseWriter, r *http.Request) {
	g, ok := s.grabByID(w, r)
	if !ok {
		return
	}
	if g.Kind != "pack" {
		writeErr(w, http.StatusUnprocessableEntity, "not a pack grab")
		return
	}
	var req struct {
		SceneIDs []string `json:"scene_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if len(req.SceneIDs) == 0 {
		writeErr(w, http.StatusBadRequest, "no scene_ids")
		return
	}
	localID := s.localPerformerIDByName(r.Context(), g.PerformerName)
	if localID == "" {
		writeErr(w, http.StatusUnprocessableEntity,
			"performer \""+g.PerformerName+"\" isn't in your Stash library — can't resolve a local id")
		return
	}
	// Re-validate against the live pack: only tag ids that are genuinely this
	// pack's UNIDENTIFIED scenes. Anything else (identified now, or not part of
	// the pack) is dropped.
	scenes, err := s.packUnidentifiedScenes(r.Context(), g)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "stash: "+err.Error())
		return
	}
	allowed := make(map[string]bool, len(scenes))
	for _, m := range scenes {
		allowed[m.ID] = true
	}
	var valid []string
	for _, id := range req.SceneIDs {
		if allowed[id] {
			valid = append(valid, id)
		}
	}
	if len(valid) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": 0})
		return
	}
	applied, err := s.pool.Stash().AddScenePerformer(r.Context(), valid, localID)
	if err != nil {
		s.log.Error("apply pack performer", "grab_id", g.ID, "err", err)
		writeErr(w, http.StatusBadGateway, "stash: "+err.Error())
		return
	}
	s.log.Info("applied pack performer", "grab_id", g.ID, "performer", g.PerformerName, "scenes", applied)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": applied})
}
