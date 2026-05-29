package api

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/pathmap"
)

// grabDetailResponse enriches a single grab with the StashDB scene it
// resolves to (thumbnail/title/performers for the expanded card) and,
// when the file has landed, the local Stash scene id + a clickable
// "open in Stash" URL.
type grabDetailResponse struct {
	StashDBID     string             `json:"stashdb_id,omitempty"`
	Title         string             `json:"title,omitempty"`
	Date          string             `json:"date,omitempty"`
	Studio        string             `json:"studio,omitempty"`
	ImageURL      string             `json:"image_url,omitempty"`
	Performers    []missingPerformer `json:"performers"`
	LocalSceneID  string             `json:"local_scene_id,omitempty"`
	StashSceneURL string             `json:"stash_scene_url,omitempty"`
	// PerformerImageURL is the grab performer's portrait, served by the
	// user's own Stash (/performer/{id}/image). Forage is performer-driven
	// and a pack has no single scene, so the expanded card leads with the
	// performer's face rather than an empty scene poster. Best-effort:
	// resolved from performer_cache by the grab's folder name.
	PerformerImageURL string `json:"performer_image_url,omitempty"`
}

// getGrabDetail powers the expanded grab card.
//
//	GET /grabs/{id}/detail
//
// The grab carries a StashDB id (actual when confirmed, else the
// predicted one) — we resolve scene metadata from StashDB for the
// card. Separately we resolve the *local* Stash scene id from the
// placed file's basename so the UI can deep-link into the user's own
// Stash. Both lookups are best-effort: a grab that hasn't landed yet
// just renders with whatever's available.
func (s *Server) getGrabDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad grab id")
		return
	}
	g, err := s.grabs.Get(r.Context(), id)
	if err != nil {
		s.log.Error("grab get", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	if g == nil {
		writeErr(w, http.StatusNotFound, "grab not found")
		return
	}

	resp := grabDetailResponse{Performers: []missingPerformer{}}

	// Scene metadata from StashDB. Prefer the confirmed actual scene;
	// fall back to the matcher's prediction.
	sceneID := g.ActualStashDBID
	if sceneID == "" {
		sceneID = g.PredictedStashDBID
	}
	resp.StashDBID = sceneID
	if sceneID != "" {
		if sdb := s.pool.StashDB(); sdb != nil {
			if scene, err := sdb.FindScene(r.Context(), sceneID); err == nil && scene != nil {
				resp.Title = scene.Title
				resp.Date = scene.Date
				if scene.Studio != nil {
					resp.Studio = scene.Studio.Name
				}
				if len(scene.Images) > 0 {
					resp.ImageURL = scene.Images[0].URL
				}
				for _, p := range scene.Performers {
					resp.Performers = append(resp.Performers, missingPerformer{Name: p.Name, As: p.As})
				}
			}
		}
	}

	// Local Stash scene + deep link. Resolve from the placed file's
	// basename — same lookup the poller uses to confirm.
	if g.PlacedPath != "" {
		if sc := s.pool.Stash(); sc != nil {
			if scene, err := sc.FindSceneByPathContains(r.Context(), filepath.Base(g.PlacedPath)); err == nil && scene != nil {
				resp.LocalSceneID = scene.ID
				cfg, _ := config.Compose(s.bootstrap, s.store.Get())
				if cfg.StashURL != "" {
					resp.StashSceneURL = strings.TrimRight(cfg.StashURL, "/") + "/scenes/" + scene.ID
				}
			}
		}
	}

	// Performer portrait, from the user's own Stash. The grab's folder
	// name is the performer's display name (from the suggest step or the
	// matched release), so we map it back to a local stash_id and build
	// the image URL Stash already serves. Loads with the user's Stash
	// session since the plugin renders inside Stash.
	if g.PerformerName != "" {
		var stashID string
		err := s.db.QueryRowContext(r.Context(),
			`SELECT stash_id FROM performer_cache WHERE name = ? COLLATE NOCASE AND stash_id != '' LIMIT 1`,
			g.PerformerName).Scan(&stashID)
		if err == nil && stashID != "" {
			cfg, _ := config.Compose(s.bootstrap, s.store.Get())
			if cfg.StashURL != "" {
				resp.PerformerImageURL = strings.TrimRight(cfg.StashURL, "/") + "/performer/" + stashID + "/image"
			}
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// deleteGrabResponse reports which teardown steps ran, so the UI can
// surface partial failures (e.g. Stash scene gone but the torrent
// delete failed) rather than silently swallowing them.
type deleteGrabResponse struct {
	OK      bool     `json:"ok"`
	Removed []string `json:"removed"`
	Errors  []string `json:"errors,omitempty"`
}

// deleteGrab purges a grab and every trace of its download.
//
//	DELETE /grabs/{id}
//
// Teardown, best-effort per step (a failure in one doesn't block the
// rest; the grab row is always removed last so it can't get stuck):
//  1. Stash scene + media file + generated artifacts (sceneDestroy
//     delete_file=true). This unlinks the library-side file.
//  2. If there was no Stash scene but a placed file exists, delete the
//     file directly so nothing lingers on disk.
//  3. Download-client copy: qBit torrent+files / SAB history+files.
//  4. The grab row itself.
func (s *Server) deleteGrab(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad grab id")
		return
	}
	g, err := s.grabs.Get(r.Context(), id)
	if err != nil {
		s.log.Error("grab get", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	if g == nil {
		writeErr(w, http.StatusNotFound, "grab not found")
		return
	}

	out := deleteGrabResponse{}
	addErr := func(label string, err error) {
		s.log.Warn("grab purge step failed", "id", id, "step", label, "err", err)
		out.Errors = append(out.Errors, label+": "+err.Error())
	}

	// 1. Stash scene(s) + media file(s). A single grab placed one file →
	// one scene (matched by basename); a pack placed a directory of many
	// scenes, so enumerate the whole directory. SceneDestroy(delete_file)
	// unlinks the library-side file(s).
	stashHandled := false
	if g.PlacedPath != "" {
		sc := s.pool.Stash()
		switch {
		case g.Kind == "pack" && sc != nil:
			// Resolve the Stash-side directory and destroy every scene
			// under it. Without a path mapping we can't match Stash scenes
			// by path safely (a bare basename over-matches unrelated
			// scenes), so we skip the Stash destroys and let the disk sweep
			// below remove the files — Stash drops the now-missing scenes
			// on its next scan.
			if needle := pathmap.Translate(g.PlacedPath, s.pool.Settings().StashPathMapping); needle != "" {
				if scenes, ferr := sc.FindScenesUnderPath(r.Context(), needle); ferr != nil {
					addErr("find pack scenes", ferr)
				} else {
					n := 0
					for _, sm := range scenes {
						if derr := sc.SceneDestroy(r.Context(), sm.ID, true, true); derr != nil {
							addErr("stash scene "+sm.ID, derr)
							continue
						}
						n++
					}
					if n > 0 {
						out.Removed = append(out.Removed, fmt.Sprintf("%d pack scenes + files", n))
					}
					stashHandled = true
				}
			}
		case sc != nil:
			scene, ferr := sc.FindSceneByPathContains(r.Context(), filepath.Base(g.PlacedPath))
			if ferr != nil {
				addErr("find stash scene", ferr)
			} else if scene != nil {
				if derr := sc.SceneDestroy(r.Context(), scene.ID, true, true); derr != nil {
					addErr("stash scene", derr)
				} else {
					stashHandled = true
					out.Removed = append(out.Removed, "stash scene + file")
				}
			}
		}
	}

	// 2. Remove leftovers on disk. A single grab reaches here only when no
	// Stash scene deleted the file for us; a pack ALWAYS sweeps its placed
	// directory — it may hold files Stash never indexed, plus (with no path
	// mapping) the scenes we couldn't match above.
	if g.PlacedPath != "" && (g.Kind == "pack" || !stashHandled) {
		if rerr := os.RemoveAll(g.PlacedPath); rerr != nil {
			addErr("placed file", rerr)
		} else if g.Kind == "pack" {
			out.Removed = append(out.Removed, "pack directory")
		} else {
			out.Removed = append(out.Removed, "placed file")
		}
	}

	// 3. Download-client copy.
	if g.ClientID != "" {
		switch g.Client {
		case "qbit":
			if qb := s.pool.Qbit(); qb != nil {
				if derr := qb.DeleteTorrent(r.Context(), g.ClientID, true); derr != nil {
					addErr("qbit torrent", derr)
				} else {
					out.Removed = append(out.Removed, "qbit torrent + files")
				}
			}
		case "sabnzbd":
			if sb := s.pool.Sab(); sb != nil {
				// May already be gone if sabDeleteAfterPlace ran; that
				// surfaces as a refused delete, which we log but don't
				// treat as fatal.
				if derr := sb.DeleteHistory(r.Context(), g.ClientID, true); derr != nil {
					addErr("sab download", derr)
				} else {
					out.Removed = append(out.Removed, "sab download")
				}
			}
		}
	}

	// 4. The grab row — always, so a partial failure can't strand it.
	if derr := s.grabs.Delete(r.Context(), id); derr != nil {
		addErr("grab record", derr)
	} else {
		out.Removed = append(out.Removed, "grab record")
	}

	out.OK = len(out.Errors) == 0
	s.log.Info("grab purged", "id", id, "removed", out.Removed, "errors", out.Errors)
	writeJSON(w, http.StatusOK, out)
}
