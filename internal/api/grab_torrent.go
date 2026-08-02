package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/suggest"
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
	qb := s.pool.Torrents()
	if qb == nil {
		writeErr(w, http.StatusServiceUnavailable, "qbit not configured (set qbitUrl in Settings)")
		return
	}
	data, errMsg := readTorrentUpload(r)
	if errMsg != "" {
		writeErr(w, http.StatusBadRequest, errMsg)
		return
	}

	meta, err := torrentmeta.Parse(data)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "not a valid .torrent: "+err.Error())
		return
	}
	// Same screen the searched-release path applies inside the qBit client
	// (see addByFetchedFile), just earlier: here the bytes are already in
	// hand. Refusing before the qBit add and before the insert means the
	// user gets told to their face rather than finding a failed grab later.
	if meta.LacksVideo() {
		writeErr(w, http.StatusUnprocessableEntity, fmt.Sprintf(
			"%s (%d files, none of them video or an archive that could hold one)",
			torrentmeta.ErrNoVideo, meta.FileCount))
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

// readTorrentUpload pulls the raw .torrent bytes from a multipart upload
// (field "torrent"), capped at maxUploadTorrent. Returns a non-empty
// message (for a 400) instead of an error so callers stay one-liners.
func readTorrentUpload(r *http.Request) ([]byte, string) {
	if err := r.ParseMultipartForm(maxUploadTorrent); err != nil {
		return nil, "expected multipart form with a torrent file"
	}
	file, _, err := r.FormFile("torrent")
	if err != nil {
		return nil, "torrent file required (field 'torrent')"
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxUploadTorrent))
	if err != nil {
		return nil, "read upload: " + err.Error()
	}
	return data, ""
}

type torrentInspectResponse struct {
	Name       string               `json:"name"`
	TotalSize  int64                `json:"total_size"`
	FileCount  int                  `json:"file_count"`
	VideoCount int                  `json:"video_count"`
	Kind       string               `json:"kind"` // "pack" | "single"
	Suggested  []suggestedPerformer `json:"suggested_performers"`
}

type suggestedPerformer struct {
	StashID    string `json:"stash_id"`
	Name       string `json:"name"`
	SceneCount int    `json:"scene_count"`
	Favorite   bool   `json:"favorite"`
}

// postGrabTorrentInspect parses an uploaded .torrent WITHOUT grabbing it,
// so the UI can show what's inside (the real info.name — not the opaque
// download filename — its size, video count, pack/single) and suggest a
// placement folder by matching that name against the local performer
// cache. The user confirms, then POSTs to /grab/torrent.
//
//	POST /grab/torrent/inspect   multipart/form-data { torrent }
func (s *Server) postGrabTorrentInspect(w http.ResponseWriter, r *http.Request) {
	data, errMsg := readTorrentUpload(r)
	if errMsg != "" {
		writeErr(w, http.StatusBadRequest, errMsg)
		return
	}
	meta, err := torrentmeta.Parse(data)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "not a valid .torrent: "+err.Error())
		return
	}
	kind := "single"
	if meta.VideoCount >= packMinVideos {
		kind = "pack"
	}
	writeJSON(w, http.StatusOK, torrentInspectResponse{
		Name:       meta.Name,
		TotalSize:  meta.TotalSize,
		FileCount:  meta.FileCount,
		VideoCount: meta.VideoCount,
		Kind:       kind,
		Suggested:  s.suggestPerformers(r.Context(), meta.Name),
	})
}

// suggestPerformers ranks local performers whose name (or alias) appears
// as a whole-word phrase in the torrent's name and adapts the shared
// suggest.Performers result into the API response shape (capped at 6).
func (s *Server) suggestPerformers(ctx context.Context, torrentName string) []suggestedPerformer {
	ps := suggest.Performers(ctx, s.db, torrentName)
	out := make([]suggestedPerformer, 0, 6)
	for _, p := range ps {
		out = append(out, suggestedPerformer{
			StashID:    p.StashID,
			Name:       p.Name,
			SceneCount: p.SceneCount,
			Favorite:   p.Favorite,
		})
		if len(out) >= 6 {
			break
		}
	}
	return out
}
