package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

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
	// Hydrate display metadata from StashDB when the caller didn't supply
	// it. The plugin's scene card already passes title/date/studio/image,
	// so this only fires for bare adds (curl, integrations) — keeping the
	// Watching tab able to render a thumbnail regardless of entry point.
	if req.ImageURL == "" {
		if sdb := s.pool.StashDB(); sdb != nil {
			if sc, ferr := sdb.FindScene(r.Context(), req.StashDBID); ferr == nil && sc != nil {
				if req.Title == "" {
					req.Title = sc.Title
				}
				if req.Date == "" {
					req.Date = sc.Date
				}
				if req.Studio == "" && sc.Studio != nil {
					req.Studio = sc.Studio.Name
				}
				if len(sc.Images) > 0 {
					req.ImageURL = sc.Images[0].URL
				}
			}
		}
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
	// Reconcile first: drop any watch whose scene forage has since grabbed
	// (by any path — Discover, missing-scenes, a release search — not just
	// the Watching tab's own grab button). Without this a watched scene you
	// obtained elsewhere lingers in Watching forever, since only
	// postWatchGrab removed watches before. Clears the existing backlog the
	// moment the tab loads.
	s.reconcileWatches(r.Context())

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

// reconcileWatches flips watches to 'grabbed' for scenes forage already has a
// grab for (queued → confirmed, the same set Discover hides), regardless of
// where the grab came from — a watch's job is "tell me when I can get this
// scene", and once a grab exists that job is done. It flips rather than
// deletes so the watch lingers in its batch and the batch's progress reads
// correctly; the user clears it (or the whole batch) when done. Preserves any
// found_* release fields already on the watch. Best-effort: logs and moves on.
func (s *Server) reconcileWatches(ctx context.Context) {
	grabbed := s.grabbedSceneSet(ctx)
	if len(grabbed) == 0 {
		return
	}
	list, err := s.watches.List(ctx)
	if err != nil {
		return
	}
	for _, wt := range list {
		if wt.Status == watches.StatusGrabbed {
			continue // already terminal
		}
		if grabbed[wt.StashDBID] {
			if err := s.watches.MarkGrabbed(ctx, wt.StashDBID,
				wt.FoundTitle, wt.FoundURL, wt.FoundIndexer, wt.FoundProtocol, wt.FoundSize); err != nil {
				s.log.Warn("watch reconcile mark grabbed", "scene", wt.StashDBID, "err", err)
				continue
			}
			s.log.Info("watch resolved — scene already grabbed", "scene", wt.StashDBID)
		}
	}
}

// findWatch loads a single watch by id, or nil. Small wrapper over List —
// the watch list is tiny, so a dedicated lookup isn't worth a repo method.
func (s *Server) findWatch(ctx context.Context, id string) *watches.Watch {
	list, err := s.watches.List(ctx)
	if err != nil {
		return nil
	}
	for i := range list {
		if list[i].StashDBID == id {
			return &list[i]
		}
	}
	return nil
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

// postWatchGrab grabs the auto-picked (best) release recorded on a watch and
// flips it to 'grabbed' — the watch LINGERS (it is not deleted) so its batch
// progress reads correctly. The user clicks this from the Watching tab when a
// watch goes available. To grab a DIFFERENT release than the best, see
// postWatchGrabCandidate.
func (s *Server) postWatchGrab(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	wt := s.findWatch(r.Context(), id)
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
		writeMappedErr(w, err, http.StatusBadGateway)
		return
	}
	if err := s.watches.MarkGrabbed(r.Context(), id,
		wt.FoundTitle, wt.FoundURL, wt.FoundIndexer, wt.FoundProtocol, wt.FoundSize); err != nil {
		s.log.Warn("watch mark grabbed", "scene", id, "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// postWatchGrabCandidate grabs a SPECIFIC release from a watch's stored
// candidate list (a re-pick when the auto-chosen best isn't what the user
// wants) and flips the watch to 'grabbed'. The candidate is matched by its
// download URL against the list captured when the watch went available.
func (s *Server) postWatchGrabCandidate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		DownloadURL string `json:"download_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if req.DownloadURL == "" {
		writeErr(w, http.StatusBadRequest, "download_url required")
		return
	}
	wt := s.findWatch(r.Context(), id)
	if wt == nil {
		writeErr(w, http.StatusNotFound, "watch not found")
		return
	}
	if wt.Status != watches.StatusAvailable {
		writeErr(w, http.StatusUnprocessableEntity, "watch has no candidates to pick from")
		return
	}
	var cands []sceneRelease
	if len(wt.Candidates) > 0 {
		_ = json.Unmarshal(wt.Candidates, &cands)
	}
	var cand *sceneRelease
	for i := range cands {
		if cands[i].DownloadURL == req.DownloadURL {
			cand = &cands[i]
			break
		}
	}
	if cand == nil {
		writeErr(w, http.StatusNotFound, "candidate not found on this watch")
		return
	}
	if _, err := s.doGrab(r.Context(), grabRequest{
		DownloadURL:    cand.DownloadURL,
		ReleaseTitle:   cand.Title,
		ReleaseSize:    cand.Size,
		ReleaseIndexer: cand.Indexer,
		Protocol:       cand.Protocol,
		SceneID:        wt.StashDBID,
		Confidence:     cand.Confidence,
		PerformerName:  wt.PerformerName,
	}); err != nil {
		writeMappedErr(w, err, http.StatusBadGateway)
		return
	}
	if err := s.watches.MarkGrabbed(r.Context(), id,
		cand.Title, cand.DownloadURL, cand.Indexer, cand.Protocol, cand.Size); err != nil {
		s.log.Warn("watch mark grabbed (candidate)", "scene", id, "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// addWatchBatchRequest is the bulk-create body: many watches sharing a batch.
// Used by "watch all missing for a performer" and Discover multi-select.
type addWatchBatchRequest struct {
	BatchLabel string            `json:"batch_label"`
	Watches    []addWatchRequest `json:"watches"`
}

// postWatchBatch creates many watches at once under a generated batch id, so
// the Watching tab can group + show their collective progress. Unlike
// postWatch it does NOT hydrate each scene's display metadata from StashDB
// (that would be N round-trips); callers pass card metadata they already have,
// and the watch loop's BackfillMeta fills any gaps later.
func (s *Server) postWatchBatch(w http.ResponseWriter, r *http.Request) {
	var req addWatchBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if len(req.Watches) == 0 {
		writeErr(w, http.StatusBadRequest, "no watches")
		return
	}
	batchID := fmt.Sprintf("b%d", time.Now().UnixNano())
	ws := make([]watches.Watch, 0, len(req.Watches))
	for _, it := range req.Watches {
		if it.StashDBID == "" {
			continue
		}
		ws = append(ws, watches.Watch{
			StashDBID:     it.StashDBID,
			Title:         it.Title,
			Date:          it.Date,
			StudioName:    it.Studio,
			ImageURL:      it.ImageURL,
			PerformerName: it.PerformerName,
			PerformerID:   it.PerformerID,
			Target:        normalizeTarget(it.Target),
			BatchID:       batchID,
			BatchLabel:    req.BatchLabel,
		})
	}
	if len(ws) == 0 {
		writeErr(w, http.StatusBadRequest, "no valid watches")
		return
	}
	if err := s.watches.AddBatch(r.Context(), ws); err != nil {
		s.log.Error("watch batch add", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	s.log.Info("watch batch added", "batch", batchID, "label", req.BatchLabel, "count", len(ws))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "batch_id": batchID, "count": len(ws)})
}

// clearBatch removes every watch in a batch (the Watching tab's per-batch
// "Clear", typically used once a collection batch is fully grabbed).
func (s *Server) clearBatch(w http.ResponseWriter, r *http.Request) {
	batchID := chi.URLParam(r, "batchId")
	if batchID == "" {
		writeErr(w, http.StatusBadRequest, "batch id required")
		return
	}
	if err := s.watches.DeleteBatch(r.Context(), batchID); err != nil {
		s.log.Error("watch batch clear", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// postWatchDismiss rejects the watch's current found release: ignore that
// exact release (by URL) going forward and flip the watch back to watching
// so the loop surfaces a different one. For the common case where the find
// is dead/over-compressed and you don't want it but still want the scene.
func (s *Server) postWatchDismiss(w http.ResponseWriter, r *http.Request) {
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
		writeErr(w, http.StatusUnprocessableEntity, "watch has no found release to dismiss")
		return
	}
	if err := s.watches.Dismiss(r.Context(), id, wt.FoundURL); err != nil {
		s.log.Error("watch dismiss", "scene", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	s.log.Info("watch find dismissed — back to watching", "scene", id, "ignored", wt.FoundURL)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// normalizeTarget validates the quality target, defaulting unknown values
// to "any".
func normalizeTarget(t string) string {
	switch t {
	case watches.Target480, watches.Target720, watches.Target1080, watches.Target4K:
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
