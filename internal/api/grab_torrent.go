package api

import (
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/torrentmeta"
)

// maxUploadTorrent caps an uploaded .torrent. Real torrents are KBs to a
// few MB even for huge multi-file packs.
const maxUploadTorrent = 32 << 20

// postGrabTorrent accepts a .torrent file the user supplies directly
// (e.g. downloaded from a private tracker forage can't search), hands
// it to qBit, and tracks it through the normal pipeline — place into
// <library_root>/<folder>, scan, identify, confirm/dedup — exactly as
// if it had been queried. No StashDB prediction is attempted: phash
// identify after download does the matching (more reliable than a
// filename guess, and the only thing that works for non-English names).
//
//	POST /grab/torrent   multipart/form-data
//	  torrent  the .torrent file (required)
//	  name     library folder to place into (optional; "(manual)" default)
//
// Single vs pack is auto-detected from the parsed video count.
func (s *Server) postGrabTorrent(w http.ResponseWriter, r *http.Request) {
	qb := s.pool.Qbit()
	if qb == nil {
		writeErr(w, http.StatusServiceUnavailable, "qbit not configured (set qbitUrl in Settings)")
		return
	}
	if err := r.ParseMultipartForm(maxUploadTorrent); err != nil {
		writeErr(w, http.StatusBadRequest, "expected multipart form with a torrent file")
		return
	}
	file, _, err := r.FormFile("torrent")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "torrent file required (field 'torrent')")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxUploadTorrent))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read upload: "+err.Error())
		return
	}

	meta, err := torrentmeta.Parse(data)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "not a valid .torrent: "+err.Error())
		return
	}

	folder := strings.TrimSpace(r.FormValue("name"))
	if folder == "" {
		folder = "(manual)"
	}
	kind := "single"
	packFiles := 0
	if meta.VideoCount >= packMinVideos {
		kind = "pack"
		packFiles = meta.VideoCount
	}

	settings := s.pool.Settings()
	if err := qb.AddTorrentFile(r.Context(), data, settings.QbitCategory); err != nil {
		s.log.Error("manual torrent add", "name", meta.Name, "err", err)
		writeErr(w, http.StatusBadGateway, "qbit: "+err.Error())
		return
	}

	title := meta.Name
	if title == "" {
		title = folder
	}
	var grabID int64
	if s.grabs != nil {
		id, err := s.grabs.Insert(r.Context(), grabs.Grab{
			ReleaseTitle:  title,
			ReleaseSize:   meta.TotalSize,
			Client:        "qbit",
			Category:      settings.QbitCategory,
			Status:        "queued",
			PerformerName: folder,
			Kind:          kind,
			PackFiles:     packFiles,
			GrabbedAt:     time.Now().Unix(),
		})
		if err != nil {
			s.log.Error("grabs insert (manual)", "err", err)
		} else {
			grabID = id
		}
	}

	s.log.Info("manual torrent queued", "name", meta.Name, "folder", folder,
		"kind", kind, "videos", meta.VideoCount, "category", settings.QbitCategory, "grab_id", grabID)
	writeJSON(w, http.StatusOK, grabResponse{
		OK:       true,
		Client:   "qbit",
		Category: settings.QbitCategory,
		GrabID:   grabID,
	})
}
