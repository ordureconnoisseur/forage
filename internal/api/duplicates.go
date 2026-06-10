package api

import (
	"encoding/json"
	"net/http"
	"strconv"

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
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad duplicate id")
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
		if len(refs) == 0 {
			// The dedup that recorded this row may have matched copies via the
			// endpoint-agnostic whole-library sweep (poller dedup does exactly
			// that when no stash-box is configured), so an endpoint-filtered
			// miss isn't proof the copies are gone. Re-check the same way
			// before concluding anything was deleted.
			sweep, serr := sc.FindAllSceneStashDBIDs(r.Context())
			if serr != nil {
				s.log.Warn("duplicate resolve revalidate sweep", "dup", id, "err", serr)
				writeErr(w, http.StatusBadGateway, "stash: "+serr.Error())
				return
			}
			refs = sweep[dup.StashDBID]
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
	if err := sc.SceneDestroy(r.Context(), id, true, true); err != nil {
		s.log.Warn("destroy scene", "scene", id, "err", err)
		writeErr(w, http.StatusBadGateway, "stash: "+err.Error())
		return
	}
	s.invalidateOwned()
	s.log.Info("scene destroyed (duplicate cleanup)", "scene", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
