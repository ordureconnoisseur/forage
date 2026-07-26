package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/ordureconnoisseur/forager/internal/destroy"
	"github.com/ordureconnoisseur/forager/internal/grabs"
)

// dupView is the wire shape for one pending review-mode duplicate. Pack vs
// Existing reuse grabs.SceneCopy directly (it carries the json tags the UI
// reads: scene_id/title/path/size/height).
type dupView struct {
	ID         int64             `json:"id"`
	StashDBID  string            `json:"stashdb_id"`
	SceneTitle string            `json:"scene_title,omitempty"`
	Pack       grabs.SceneCopy   `json:"pack"`
	Existing   []grabs.SceneCopy `json:"existing"`
}

func toDupViews(dups []grabs.PackDuplicate) []dupView {
	out := make([]dupView, 0, len(dups))
	for _, d := range dups {
		existing := d.Existing
		if existing == nil {
			existing = []grabs.SceneCopy{}
		}
		out = append(out, dupView{
			ID:         d.ID,
			StashDBID:  d.StashDBID,
			SceneTitle: d.SceneTitle,
			Pack:       d.Pack,
			Existing:   existing,
		})
	}
	return out
}

type resolveDuplicateRequest struct {
	// Keep chooses which copy survives:
	//   "existing" — destroy the pack's copy, keep what the library had
	//   "pack"     — destroy the pre-existing copy/copies, keep the pack's
	//   "both"     — keep everything (dismiss the review item, delete nothing)
	Keep string `json:"keep"`
}

type resolveDuplicateResponse struct {
	OK         bool     `json:"ok"`
	Resolution string   `json:"resolution"`
	Removed    []string `json:"removed,omitempty"`
	Errors     []string `json:"errors,omitempty"`
}

// postResolveDuplicate applies a user's keep decision to one pending
// review-mode duplicate.
//
//	POST /duplicates/{id}/resolve   {"keep":"existing"|"pack"|"both"}
//
// This is the ONLY path that deletes a pre-existing library copy, and it
// runs here — foreground, user-initiated, on a set the user just saw —
// rather than unattended in the poller. The destroy uses
// SceneDestroy(delete_file=true), so it removes the Stash scene and its file;
// the torrent keeps seeding from the download client's own hardlink. The row
// is only marked resolved when every intended destroy succeeded, so a
// transient Stash failure leaves it pending for a retry.
func (s *Server) postResolveDuplicate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}

	var req resolveDuplicateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	switch req.Keep {
	case "existing", "pack", "both":
	default:
		writeErr(w, http.StatusBadRequest, `keep must be "existing", "pack", or "both"`)
		return
	}

	dup, err := s.grabs.GetDuplicate(r.Context(), id)
	if err != nil {
		s.log.Error("duplicate get", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	if dup == nil {
		writeErr(w, http.StatusNotFound, "duplicate not found")
		return
	}
	if dup.Status != "pending" {
		// Already decided — idempotent success rather than an error, so a
		// double-click or a stale UI doesn't surface a failure.
		writeJSON(w, http.StatusOK, resolveDuplicateResponse{OK: true, Resolution: dup.Resolution})
		return
	}

	out := resolveDuplicateResponse{Resolution: req.Keep}

	if req.Keep != "both" {
		sc := s.pool.Stash()
		if sc == nil {
			writeErr(w, http.StatusServiceUnavailable, "stash not configured (see Settings)")
			return
		}
		// The review row is a snapshot from pack-dedup time, and the library
		// may have changed since — the performer page's dupes view (or a grab
		// purge) can destroy either copy without touching this row. Re-resolve
		// the scene's CURRENT copies and only destroy when the kept side still
		// exists; otherwise this resolve would delete the last remaining copy.
		endpoint := s.stashDBEndpoint(r.Context(), sc)
		refs, ferr := sc.FindSceneRefsByStashID(r.Context(), endpoint, dup.StashDBID)
		if ferr != nil {
			s.log.Warn("duplicate resolve revalidate", "dup", id, "err", ferr)
			writeErr(w, http.StatusBadGateway, "stash: "+ferr.Error())
			return
		}
		alive := map[string]bool{}
		for _, ref := range refs {
			alive[ref.SceneID] = true
		}

		// Build the destroy set from the chosen side, and the kept set from
		// the other.
		var targets, kept []string
		switch req.Keep {
		case "existing":
			targets = append(targets, dup.Pack.SceneID)
			for _, e := range dup.Existing {
				kept = append(kept, e.SceneID)
			}
		case "pack":
			for _, e := range dup.Existing {
				if e.SceneID != "" {
					targets = append(targets, e.SceneID)
				}
			}
			kept = append(kept, dup.Pack.SceneID)
		}

		// The endpoint-filtered lookup can be PARTIAL, not just empty:
		// copies cross-tagged under a legacy/other endpoint string don't
		// come back, and treating an absent target as "already deleted"
		// would mark the review resolved while the duplicate file quietly
		// survives. Whenever any id from the snapshot is missing, re-check
		// via the endpoint-agnostic whole-library sweep (the same source
		// poller dedup uses when no stash-box is configured) before
		// concluding anything is gone.
		missing := false
		for _, sid := range append(append([]string{}, targets...), kept...) {
			if sid != "" && !alive[sid] {
				missing = true
				break
			}
		}
		if missing {
			sweep, serr := sc.FindAllSceneStashDBIDs(r.Context())
			if serr != nil {
				s.log.Warn("duplicate resolve revalidate sweep", "dup", id, "err", serr)
				writeErr(w, http.StatusBadGateway, "stash: "+serr.Error())
				return
			}
			// Merge the sweep's refs, not just its aliveness: refs are one
			// entry per FILE, and the destroy plan below counts files from
			// them. A scene known only via the sweep used to enter with an
			// implicit count of zero — which disabled the multi-file guard on
			// exactly the copies the endpoint-scoped lookup couldn't see.
			seen := map[string]bool{}
			for _, ref := range refs {
				seen[ref.SceneID] = true
			}
			for _, ref := range sweep[dup.StashDBID] {
				alive[ref.SceneID] = true
				if !seen[ref.SceneID] {
					refs = append(refs, ref)
				}
			}
		}
		keptAlive := false
		for _, sid := range kept {
			if sid != "" && alive[sid] {
				keptAlive = true
				break
			}
		}
		if !keptAlive {
			writeErr(w, http.StatusConflict,
				"the copy you chose to keep no longer exists in Stash — refusing to delete the other side; dismiss with keep=\"both\" if this review is stale")
			return
		}
		// Build the vetted destroy plan from the refs (one per FILE, so the
		// plan sees every scene's true blast radius). A target that isn't
		// alive is absent from refs and so simply isn't planned — already
		// destroyed via another surface, nothing to do.
		targetSet := map[string]bool{}
		for _, sid := range targets {
			if sid != "" {
				targetSet[sid] = true
			}
		}
		plan := destroy.Vet(destroy.FromRefs(refs, func(sceneID string) bool {
			return targetSet[sceneID]
		}))
		for _, ref := range plan.Refused {
			// Refuse and leave the item pending (the error below blocks the
			// resolve) rather than over-delete.
			s.log.Warn("duplicate resolve refused", "dup", id,
				"scene", ref.Target.SceneID, "why", ref.Reason, "keep", req.Keep)
			out.Errors = append(out.Errors, fmt.Sprintf(
				"scene %s: %s; sort that scene out in Stash first",
				ref.Target.SceneID, ref.Reason))
		}
		outcome := destroy.Execute(r.Context(), sc, plan, s.grabs, s.log, "duplicate resolve keep="+req.Keep)
		for _, f := range outcome.Failed {
			out.Errors = append(out.Errors, "scene "+f.Target.SceneID+": "+f.Err.Error())
		}
		for _, t := range outcome.Destroyed {
			out.Removed = append(out.Removed, "scene "+t.SceneID)
		}
		if len(out.Removed) > 0 {
			// Same as postDestroyScene: the performer page's owned/duplicates
			// memo must not keep listing the destroyed copy for ownedTTL and
			// invite a second destroy against the survivor.
			s.invalidateOwned()
		}
		// Don't mark resolved if anything failed — leave it pending so the
		// user can retry without losing the rest of the decision.
		if len(out.Errors) > 0 {
			out.OK = false
			writeJSON(w, http.StatusInternalServerError, out)
			return
		}
	}

	if derr := s.grabs.ResolveDuplicate(r.Context(), id, req.Keep); derr != nil {
		s.log.Error("duplicate mark resolved", "dup", id, "err", derr)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	out.OK = true
	s.log.Info("duplicate resolved", "dup", id, "grab", dup.GrabID, "keep", req.Keep, "removed", out.Removed)
	writeJSON(w, http.StatusOK, out)
}

// postDestroyScene deletes one local Stash scene (and its file) by id.
//
//	POST /scenes/{id}/destroy   ({id} = LOCAL Stash scene id)
//
// Backs the performer page's duplicates-cleanup view: when you hold 2+ copies
// of the same StashDB scene, this removes the one you don't want. Like the
// pack-review resolve, it's a deliberate, foreground, user-initiated destroy
// (SceneDestroy with delete_file=true) — note {id} here is a LOCAL scene id,
// distinct from the StashDB id that /scenes/{id}/releases takes. Invalidates
// the owned memos so the duplicates list refreshes immediately.
func (s *Server) postDestroyScene(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "scene id required")
		return
	}
	sc := s.pool.Stash()
	if sc == nil {
		writeErr(w, http.StatusServiceUnavailable, "stash not configured (see Settings)")
		return
	}
	// Resolve the scene's full file list and vet it through the destroy
	// plan. A lookup failure is NOT treated as safe: with an unknown blast
	// radius we don't destroy.
	info, ferr := sc.SceneFiles(r.Context(), id)
	if ferr != nil {
		s.log.Warn("destroy scene: files lookup", "scene", id, "err", ferr)
		writeErr(w, http.StatusBadGateway, "stash: couldn't check what files this scene has: "+ferr.Error())
		return
	}
	target := destroy.Target{SceneID: info.ID, Title: info.Title}
	for _, f := range info.Files {
		target.Files = append(target.Files, destroy.File{Path: f.Path, Size: f.Size})
	}
	plan := destroy.Vet([]destroy.Target{target})
	if plan.Empty() {
		// Refused: a scene holding several files — Stash attaches a
		// matching-fingerprint re-download as an extra FILE, and destroying
		// the scene deletes all of them plus the record (tags, o-counter,
		// markers). Return the file list so the user sees exactly what
		// refusing protected.
		ref := plan.Refused[0]
		s.log.Warn("destroy scene refused", "scene", id, "why", ref.Reason)
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": fmt.Sprintf(
				"scene %s has %d files attached, and deleting the scene deletes all of them. "+
					"Stash filed these as one scene (matching fingerprints), so there is no "+
					"single copy here to remove — sort the extra file out in Stash directly.",
				id, len(ref.Target.Files)),
			"kept": ref,
		})
		return
	}
	outcome := destroy.Execute(r.Context(), sc, plan, s.grabs, s.log, "performer-page duplicate cleanup")
	if len(outcome.Failed) > 0 {
		writeErr(w, http.StatusBadGateway, "stash: "+outcome.Failed[0].Err.Error())
		return
	}
	s.invalidateOwned()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": outcome.Destroyed})
}

// getDestructions returns the destruction journal, newest first — every
// scene destroy forage attempted (intent/destroyed/failed) or refused, with
// the complete file list snapshotted at decision time.
//
//	GET /destructions?limit=100
//
// The audit answer to "what did forage delete and why" without log
// archaeology.
func (s *Server) getDestructions(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	entries, err := s.grabs.ListDestructions(r.Context(), limit)
	if err != nil {
		s.log.Error("destruction journal list", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	if entries == nil {
		entries = []grabs.DestructionEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"destructions": entries})
}
