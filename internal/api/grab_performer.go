package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

type setPerformerRequest struct {
	PerformerName string `json:"performer_name"`
}

// postGrabPerformer reassigns a grab's performer folder and re-files the
// download there. The fix for adopted/Unsorted grabs the name-guess
// couldn't confidently place: pick the right performer and forage re-links
// the still-seeding download into <library>/<performer>/, removing the old
// library-side link. The qBit/SAB source is never touched (it keeps
// seeding); only the library hardlink moves.
//
//	POST /grabs/{id}/performer   body: { "performer_name": "Brie Belle" }
func (s *Server) postGrabPerformer(w http.ResponseWriter, r *http.Request) {
	gid, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad grab id")
		return
	}
	var req setPerformerRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	performer := strings.TrimSpace(req.PerformerName)
	if performer == "" {
		writeErr(w, http.StatusBadRequest, "performer_name required")
		return
	}

	g, err := s.grabs.Get(r.Context(), gid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	if g == nil {
		writeErr(w, http.StatusNotFound, "grab not found")
		return
	}
	// Packs are allowed: forage files a whole pack into a single performer
	// folder anyway, so reassigning just moves that directory. (This differs
	// from /match, which applies one scene's metadata and genuinely can't
	// represent a many-scene pack.)

	// State-aware. Re-placing a file is only valid once the grab has
	// actually been placed (has a placed_path). For a grab that hasn't
	// landed yet (queued/downloading), there's no finished file to move —
	// we ONLY update the performer name, and the poller files it into the
	// right folder when the download completes. (Placing mid-download would
	// hardlink a partial file into the library and trip the poller's
	// placed→scanned heal, which is the bug this guards against.)
	alreadyPlaced := g.PlacedPath != ""

	if alreadyPlaced {
		pl := s.pool.Placer()
		if !pl.Configured() {
			writeErr(w, http.StatusUnprocessableEntity,
				"library root not configured — can't re-file (set it in Settings)")
			return
		}
		src := s.grabSourcePath(r.Context(), g.Client, g.ClientID)
		if src == "" {
			writeErr(w, http.StatusUnprocessableEntity,
				"the download is no longer in the client, so there's nothing to re-file")
			return
		}
		res, err := pl.Place(src, performer)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "re-file failed: "+err.Error())
			return
		}
		// Remove the OLD library-side placement if it moved. Library
		// hardlink only — never the seeding source. Best-effort: a stale
		// leftover is cosmetic (Stash drops it on the next scan).
		oldPath := g.PlacedPath
		if oldPath != "" && oldPath != res.Path {
			if rerr := os.RemoveAll(oldPath); rerr != nil {
				s.log.Warn("set performer: remove old placement", "id", gid, "path", oldPath, "err", rerr)
			}
		}
		g.PlacedPath = res.Path
		s.log.Info("grab performer reassigned (re-filed)", "id", gid, "performer", performer,
			"placed", res.Path, "mode", res.Mode)
	} else {
		// Not placed yet: just retarget the folder for when it lands.
		s.log.Info("grab performer set (not yet placed)", "id", gid, "performer", performer)
	}

	g.PerformerName = performer
	g.PlaceError = ""
	g.Reason = "performer set manually"
	if err := s.grabs.Update(r.Context(), *g); err != nil {
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"performer_name": performer,
		"placed_path":    g.PlacedPath,
	})
}

// grabSourcePath returns the live on-disk path of a grab's download from
// its client (qBit ContentPath / SAB history Path), or "" when the client
// no longer tracks it. Mirrors what the poller's advance step derives, but
// standalone — keyed only by the grab's client + client_id.
func (s *Server) grabSourcePath(ctx context.Context, client, clientID string) string {
	if clientID == "" {
		return ""
	}
	switch client {
	case "qbit":
		if qb := s.pool.Qbit(); qb != nil {
			if t, err := qb.TorrentInfo(ctx, clientID); err == nil && t != nil {
				return t.ContentPath
			}
		}
	case "sabnzbd":
		if sb := s.pool.Sab(); sb != nil {
			if items, err := sb.History(ctx, 0); err == nil {
				for _, it := range items {
					if it.NzoID == clientID {
						return it.Path
					}
				}
			}
		}
	}
	return ""
}
