package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/matcher"
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

// suggestPerformers ranks local performers whose name (or an alias) appears
// as a whole-word phrase in the torrent's name — e.g. "Amouranth" or
// "<Studio> Hazel Moore SiteRip" → that performer's folder. Matching is
// word-level (not substring) so "Mom" can't match "momdrips", and a
// single-word name must be >= 4 chars to skip short common words. Best
// effort: a query failure just yields no suggestions (manual folder).
func (s *Server) suggestPerformers(ctx context.Context, torrentName string) []suggestedPerformer {
	nameWords := matcher.Tokenize(torrentName)
	if len(nameWords) == 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT stash_id, name, aliases, scene_count, favorite FROM performer_cache`)
	if err != nil {
		s.log.Warn("suggest performers query", "err", err)
		return nil
	}
	defer rows.Close()

	type cand struct {
		p        suggestedPerformer
		matchLen int // matched token count — higher is more specific
	}
	var cands []cand
	for rows.Next() {
		var stashID, name string
		var aliases sql.NullString
		var sceneCount, fav int
		if rows.Scan(&stashID, &name, &aliases, &sceneCount, &fav) != nil {
			continue
		}
		best := 0
		labels := append([]string{name}, parseAliasList(aliases.String)...)
		for _, label := range labels {
			lw := matcher.Tokenize(label)
			if performerLabelMatches(nameWords, lw) && len(lw) > best {
				best = len(lw)
			}
		}
		if best > 0 {
			cands = append(cands, cand{
				p:        suggestedPerformer{StashID: stashID, Name: name, SceneCount: sceneCount, Favorite: fav != 0},
				matchLen: best,
			})
		}
	}
	// Most-specific (longest phrase) first, then favourites, then library
	// prominence (scene_count), then name for stability.
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.matchLen != b.matchLen {
			return a.matchLen > b.matchLen
		}
		if a.p.Favorite != b.p.Favorite {
			return a.p.Favorite
		}
		if a.p.SceneCount != b.p.SceneCount {
			return a.p.SceneCount > b.p.SceneCount
		}
		return a.p.Name < b.p.Name
	})
	out := make([]suggestedPerformer, 0, 6)
	for _, c := range cands {
		out = append(out, c.p)
		if len(out) >= 6 {
			break
		}
	}
	return out
}

// performerLabelMatches reports whether labelWords appears as a contiguous
// run in nameWords. A single-word label must be >= 4 chars so short common
// words don't match everything.
func performerLabelMatches(nameWords, labelWords []string) bool {
	if len(labelWords) == 0 {
		return false
	}
	if len(labelWords) == 1 && len(labelWords[0]) < 4 {
		return false
	}
	if len(labelWords) > len(nameWords) {
		return false
	}
	for i := 0; i+len(labelWords) <= len(nameWords); i++ {
		match := true
		for j := range labelWords {
			if nameWords[i+j] != labelWords[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// parseAliasList decodes performer_cache.aliases (a JSON string array);
// returns nil on empty/invalid.
func parseAliasList(j string) []string {
	if j == "" {
		return nil
	}
	var out []string
	if json.Unmarshal([]byte(j), &out) != nil {
		return nil
	}
	return out
}
