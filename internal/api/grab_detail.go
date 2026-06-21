package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/ordureconnoisseur/forager/internal/clienterr"
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
	// LocalStashDBID is the StashDB cross-id actually on the placed scene in
	// the user's Stash, the source of truth for "is this identified?". It can
	// differ from the grab's own actual_stashdb_id: an adopted single often
	// settles "in library (scanned)" before Stash's identify lands (or the
	// user identifies it by hand later), so the grab record stays blank while
	// Stash holds the real link. The card trusts this over the grab field.
	LocalStashDBID string `json:"local_stashdb_id,omitempty"`
	// PerformerImageURL is the grab performer's portrait, served by the
	// user's own Stash (/performer/{id}/image). Forage is performer-driven
	// and a pack has no single scene, so the expanded card leads with the
	// performer's face rather than an empty scene poster. Best-effort:
	// resolved from performer_cache by the grab's folder name.
	PerformerImageURL string `json:"performer_image_url,omitempty"`
	// LocalSceneImageURL is the placed scene's own screenshot from the
	// user's Stash (/scene/{id}/screenshot). For a single grab this is the
	// real thumbnail of what was downloaded — preferred over both the
	// StashDB image and the performer portrait — and crucially it exists
	// even when the scene has no StashDB cross-id (adopted/manual grabs).
	LocalSceneImageURL string `json:"local_scene_image_url,omitempty"`
	// PerformerSuggestions ranks local performers whose name appears in the
	// release title — the one-click options the card offers for reassigning
	// a mis-filed / Unsorted grab to the right folder.
	PerformerSuggestions []suggestedPerformer `json:"performer_suggestions,omitempty"`
	// Duplicates are pending review-mode dedup items for a pack: scenes the
	// pack delivered that the library already had. The card renders a
	// per-scene compare (pack vs existing) with Keep buttons. Empty unless
	// PackDedupKeep="review" found collisions.
	Duplicates []dupView `json:"duplicates,omitempty"`
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
	g, ok := s.grabByID(w, r)
	if !ok {
		return
	}
	id := g.ID

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
				// The cross-id Stash actually holds on this file — the truth
				// for the card's identified/not-identified hero, regardless of
				// what the grab record captured.
				resp.LocalStashDBID = scene.StashDBID
				cfg, _ := config.Compose(s.bootstrap, s.store.Get())
				if cfg.StashURL != "" {
					// "View in Stash" opens the user's Stash UI directly, so
					// it stays an absolute Stash URL.
					resp.StashSceneURL = strings.TrimRight(cfg.StashURL, "/") + "/scenes/" + scene.ID
					// The screenshot is rendered inside forage, so it goes
					// through the daemon's image proxy (daemon-relative path;
					// the client prepends its base). A real thumbnail for the
					// single even when StashDB has no match.
					resp.LocalSceneImageURL = "/img/scene/" + scene.ID + "/screenshot"
				}
			}
		}
	}

	// Performer portrait, from the user's own Stash. The grab's folder
	// name is the performer's display name (from the suggest step or the
	// matched release), so we map it back to a local stash_id and serve the
	// portrait through the daemon's image proxy (daemon-relative path; the
	// client prepends its base). Works whether forage runs standalone or as
	// a Stash launcher — the browser never needs a Stash session.
	if g.PerformerName != "" {
		var stashID string
		err := s.db.QueryRowContext(r.Context(),
			`SELECT stash_id FROM performer_cache WHERE name = ? COLLATE NOCASE AND stash_id != '' LIMIT 1`,
			g.PerformerName).Scan(&stashID)
		if err == nil && stashID != "" {
			cfg, _ := config.Compose(s.bootstrap, s.store.Get())
			if cfg.StashURL != "" {
				resp.PerformerImageURL = "/img/performer/" + stashID
			}
		}
	}

	// Performer reassignment options (one-click chips on the card). Ranked
	// guesses from the release title — for singles and packs alike (a pack
	// is filed into one performer folder, so it's reassignable too).
	resp.PerformerSuggestions = s.suggestPerformers(r.Context(), g.ReleaseTitle)

	// Pending duplicate-review items (PackDedupKeep="review"). Best-effort —
	// a lookup failure just renders the card without the review section.
	if dups, err := s.grabs.PendingDuplicatesByGrab(r.Context(), id); err != nil {
		s.log.Warn("grab duplicates lookup", "id", id, "err", err)
	} else {
		resp.Duplicates = toDupViews(dups)
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
	g, ok := s.grabByID(w, r)
	if !ok {
		return
	}
	id := g.ID

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
			if ferr != nil && !errors.Is(ferr, clienterr.ErrNotFound) {
				addErr("find stash scene", ferr)
			} else if scene != nil && !sameParentDir(scene.FilePath, g.PlacedPath) {
				// The lookup is keyed on basename, and even segment-anchored
				// a generic name ("video.mp4") can resolve to a same-named
				// file under ANOTHER performer's folder — destroying that
				// would delete an unrelated scene's media. Skip the Stash
				// destroy; the disk sweep below still removes the grab's own
				// file and Stash drops the scene on its next scan.
				addErr("find stash scene", fmt.Errorf(
					"basename matched scene %s in a different directory (%s), refusing to destroy it",
					scene.ID, scene.FilePath))
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

	// 4. Pending review-duplicate rows for this pack — they reference scenes
	// we just destroyed, so drop them so they can't linger as stale reviews.
	if derr := s.grabs.DeleteDuplicatesByGrab(r.Context(), id); derr != nil {
		addErr("duplicate records", derr)
	}

	// 5. The grab row — always, so a partial failure can't strand it.
	if derr := s.grabs.Delete(r.Context(), id); derr != nil {
		addErr("grab record", derr)
	} else {
		out.Removed = append(out.Removed, "grab record")
	}

	out.OK = len(out.Errors) == 0
	s.log.Info("grab purged", "id", id, "removed", out.Removed, "errors", out.Errors)
	writeJSON(w, http.StatusOK, out)
}

// sameParentDir reports whether the Stash-side file belongs to the
// forage-side placement, compared by last directory segment only (the
// two sides see the same file through different mounts/roots). Used to
// verify a basename-keyed scene lookup before destructive or
// metadata-overwriting actions.
//
// Two placement shapes:
//   - file placement: the scene file sits NEXT TO the placed path, so
//     their parent segments match (the performer folder).
//   - directory placement (a torrent with a root folder, or any SAB
//     job): the scene file sits INSIDE the placed path, so the file's
//     parent segment equals the placed path's own basename. Comparing
//     parents only — performer vs release dir — rejected every such
//     placement, 409ing all manual matches for folder-shaped grabs.
//
// False when segments are missing — an unverifiable match is not safe
// to act on. (A scene parent equal to a placed FILE's basename is not
// realistic, so accepting Base(placedPath) costs nothing for files.)
func sameParentDir(stashPath, placedPath string) bool {
	sp := pathmap.Parent(stashPath)
	if sp == "" {
		return false
	}
	if pp := pathmap.Parent(placedPath); pp != "" && sp == pp {
		return true
	}
	return sp == pathmap.Base(placedPath)
}
