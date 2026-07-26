package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
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
		// refs is one entry per FILE, so counting them per scene gives the
		// file count for free — no extra Stash call. Needed because
		// sceneDestroy(delete_file) takes every file on a scene, so a target
		// holding two would destroy more than the copy being resolved.
		files := map[string]int{}
		for _, ref := range refs {
			alive[ref.SceneID] = true
			files[ref.SceneID]++
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
			for _, ref := range sweep[dup.StashDBID] {
				alive[ref.SceneID] = true
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
		for _, sid := range targets {
			if sid == "" {
				continue
			}
			if !alive[sid] {
				// Already gone (destroyed via another surface) — nothing to do.
				continue
			}
			if files[sid] > 1 {
				// Destroying this side would delete files beyond the copy under
				// review. Refuse and leave the item pending (the error below
				// blocks the resolve) rather than over-delete.
				s.log.Warn("duplicate resolve refused: multi-file scene",
					"dup", id, "scene", sid, "files", files[sid], "keep", req.Keep)
				out.Errors = append(out.Errors, fmt.Sprintf(
					"scene %s has %d files attached and deleting it removes all of them; "+
						"sort that scene out in Stash first", sid, files[sid]))
				continue
			}
			if derr := sc.SceneDestroy(r.Context(), sid, true, true); derr != nil {
				s.log.Warn("duplicate resolve destroy", "dup", id, "scene", sid, "keep", req.Keep, "err", derr)
				out.Errors = append(out.Errors, "scene "+sid+": "+derr.Error())
				continue
			}
			out.Removed = append(out.Removed, "scene "+sid)
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
	// A scene can hold several FILES — Stash attaches a re-download whose
	// fingerprint matches as an extra file on the existing scene rather than
	// as a new scene. sceneDestroy(delete_file) deletes every one of them,
	// so "remove the copy I don't want" would silently take the copy the user
	// is keeping, along with the scene's tags, o-counter and markers.
	//
	// Refuse instead. Deleting a single file out of a scene is a different
	// operation that forage doesn't have, and guessing is how a dedup sweep
	// turns into mass deletion. A lookup failure is NOT treated as safe: with
	// an unknown file count we don't destroy.
	n, ferr := sc.SceneFileCount(r.Context(), id)
	if ferr != nil {
		s.log.Warn("destroy scene: file count", "scene", id, "err", ferr)
		writeErr(w, http.StatusBadGateway, "stash: couldn't check how many files this scene has: "+ferr.Error())
		return
	}
	if n > 1 {
		s.log.Warn("destroy scene refused: multi-file scene", "scene", id, "files", n)
		writeErr(w, http.StatusConflict, fmt.Sprintf(
			"scene %s has %d files attached, and deleting the scene deletes all of them. "+
				"Stash filed these as one scene (matching fingerprints), so there is no "+
				"single copy here to remove — sort the extra file out in Stash directly.", id, n))
		return
	}
	if err := sc.SceneDestroy(r.Context(), id, true, true); err != nil {
		s.log.Warn("destroy scene", "scene", id, "err", err)
		writeErr(w, http.StatusBadGateway, "stash: "+err.Error())
		return
	}
	s.invalidateOwned()
	s.log.Info("scene destroyed (duplicate cleanup)", "scene", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
