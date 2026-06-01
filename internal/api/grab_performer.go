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
	if g.Kind == "pack" {
		// A pack spans many performers; one folder can't represent it.
		writeErr(w, http.StatusUnprocessableEntity,
			"can't set a single performer on a pack grab — identify its scenes in Stash")
		return
	}

	pl := s.pool.Placer()
	if !pl.Configured() {
		writeErr(w, http.StatusUnprocessableEntity,
			"library root not configured — can't re-file (set it in Settings)")
		return
	}

	// Resolve the live source path from the download client (the seeding
	// content), the same path the poller places from.
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

	// Remove the OLD library-side placement if it moved. Library hardlink
	// only — never the seeding source. Best-effort: a stale leftover is
	// cosmetic (Stash drops it on the next scan), so we don't fail here.
	oldPath := g.PlacedPath
	if oldPath != "" && oldPath != res.Path {
		if rerr := os.RemoveAll(oldPath); rerr != nil {
			s.log.Warn("set performer: remove old placement", "id", gid, "path", oldPath, "err", rerr)
		}
	}

	g.PerformerName = performer
	g.PlacedPath = res.Path
	g.PlaceError = ""
	g.Reason = "performer set manually"
	if err := s.grabs.Update(r.Context(), *g); err != nil {
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	s.log.Info("grab performer reassigned", "id", gid, "performer", performer,
		"placed", res.Path, "mode", res.Mode)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"performer_name": performer,
		"placed_path":    res.Path,
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
