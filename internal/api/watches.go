package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/ordureconnoisseur/forager/internal/watches"
)

// Watch endpoints — track a StashDB scene to be told when a release at a
// target quality appears. The background loop (watch_loop.go) does the
// re-searching; these just manage the list and let the user grab an
// available watch.

type addWatchRequest struct {
	StashDBID     string `json:"stashdb_id"`
	Title         string `json:"title"`
	Date          string `json:"date,omitempty"`
	Studio        string `json:"studio,omitempty"`
	ImageURL      string `json:"image_url,omitempty"`
	PerformerName string `json:"performer_name,omitempty"`
	PerformerID   string `json:"performer_id,omitempty"`
	// Target resolution: "any" | "720p" | "1080p" | "4k".
	Target string `json:"target"`
}

func (s *Server) postWatch(w http.ResponseWriter, r *http.Request) {
	var req addWatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if req.StashDBID == "" {
		writeErr(w, http.StatusBadRequest, "stashdb_id required")
		return
	}
	target := normalizeTarget(req.Target)
	if err := s.watches.Add(r.Context(), watches.Watch{
		StashDBID:     req.StashDBID,
		Title:         req.Title,
		Date:          req.Date,
		StudioName:    req.Studio,
		ImageURL:      req.ImageURL,
		PerformerName: req.PerformerName,
		PerformerID:   req.PerformerID,
		Target:        target,
	}); err != nil {
		s.log.Error("watch add", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	s.log.Info("watch added", "scene", req.StashDBID, "target", target)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "target": target})
}

func (s *Server) getWatches(w http.ResponseWriter, r *http.Request) {
	ws, err := s.watches.List(r.Context())
	if err != nil {
		s.log.Error("watch list", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	if ws == nil {
		ws = []watches.Watch{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"watches": ws})
}

func (s *Server) deleteWatch(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.watches.Delete(r.Context(), id); err != nil {
		s.log.Error("watch delete", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// postWatchGrab grabs the available release recorded on a watch, then
// removes the watch (its job is done). The user clicks this from the
// Watching tab when a watch goes available.
func (s *Server) postWatchGrab(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	list, err := s.watches.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	var wt *watches.Watch
	for i := range list {
		if list[i].StashDBID == id {
			wt = &list[i]
			break
		}
	}
	if wt == nil {
		writeErr(w, http.StatusNotFound, "watch not found")
		return
	}
	if wt.Status != watches.StatusAvailable || wt.FoundURL == "" {
		writeErr(w, http.StatusUnprocessableEntity, "watch has no available release yet")
		return
	}
	if _, err := s.doGrab(r.Context(), grabRequest{
		DownloadURL:    wt.FoundURL,
		ReleaseTitle:   wt.FoundTitle,
		ReleaseSize:    wt.FoundSize,
		ReleaseIndexer: wt.FoundIndexer,
		Protocol:       wt.FoundProtocol,
		SceneID:        wt.StashDBID,
		PerformerName:  wt.PerformerName,
	}); err != nil {
		var ge grabError
		if errors.As(err, &ge) {
			writeErr(w, ge.status, ge.msg)
			return
		}
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	// Grabbed → the watch is done; remove it.
	_ = s.watches.Delete(r.Context(), id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// normalizeTarget validates the quality target, defaulting unknown values
// to "any".
func normalizeTarget(t string) string {
	switch t {
	case watches.Target720, watches.Target1080, watches.Target4K:
		return t
	default:
		return watches.TargetAny
	}
}

// watchStatusByScene returns scene-id → watch status for badging the
// missing-scenes / discover lists. Best-effort; nil on error.
func (s *Server) watchStatusByScene(ctx context.Context) map[string]string {
	m, err := s.watches.IDs(ctx)
	if err != nil {
		return nil
	}
	return m
}
