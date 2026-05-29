package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ordureconnoisseur/forager/internal/grabs"
)

type grabRequest struct {
	DownloadURL    string  `json:"download_url"`
	ReleaseTitle   string  `json:"release_title"`
	ReleaseSize    int64   `json:"release_size"`
	ReleaseIndexer string  `json:"release_indexer"`
	Protocol       string  `json:"protocol"` // "torrent" | "usenet"; falls back to URL inspection if missing
	SceneID        string  `json:"scene_id"`
	Confidence     float64 `json:"confidence"`
	// PerformerName is the folder forage will drop the finished file
	// into under <library_root>. Plugin sets this from whichever
	// performer page the user grabbed from. Optional — if missing the
	// placer falls back to "Unsorted" so files don't get stranded.
	PerformerName string `json:"performer_name"`
	// Kind is "pack" for a performer pack grab (one torrent → many
	// scenes), empty/"single" otherwise. VideoCount is the parsed video
	// count from the pack's .torrent, recorded as the expected total the
	// pack confirm path drives identify toward.
	Kind       string `json:"kind"`
	VideoCount int    `json:"video_count"`
}

type grabResponse struct {
	OK       bool   `json:"ok"`
	Client   string `json:"client,omitempty"`
	Category string `json:"category,omitempty"`
	GrabID   int64  `json:"grab_id,omitempty"`
	ClientID string `json:"client_id,omitempty"` // synchronously known for SAB; empty for qBit (poller links later)
}

// postGrab routes a Prowlarr-sourced release to the appropriate
// download client based on the release.protocol field. Torrents go to
// qBit (forager fetches the .torrent bytes itself and uploads — gluetun
// network can't resolve the Prowlarr host). NZBs go to SAB which
// happily fetches the URL directly and synchronously returns its
// nzo_id.
//
// In both cases a grabs row is persisted so the Phase B poller can
// track the download → confirmation lifecycle.
func (s *Server) postGrab(w http.ResponseWriter, r *http.Request) {
	var req grabRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if req.DownloadURL == "" {
		writeErr(w, http.StatusBadRequest, "download_url required")
		return
	}
	protocol := req.Protocol
	if protocol == "" {
		protocol = inferProtocol(req.DownloadURL)
	}

	var (
		client     string
		clientID   string
		category   string
		clientErr  error
	)
	settings := s.pool.Settings()
	switch protocol {
	case "torrent":
		qb := s.pool.Qbit()
		if qb == nil {
			writeErr(w, http.StatusServiceUnavailable, "qbit not configured (set qbitUrl in Settings)")
			return
		}
		clientErr = qb.AddTorrent(r.Context(), req.DownloadURL, settings.QbitCategory)
		client = "qbit"
		category = settings.QbitCategory
		// clientID populated asynchronously by the poller (qBit's add
		// endpoint doesn't return the info_hash).
	case "usenet":
		sb := s.pool.Sab()
		if sb == nil {
			writeErr(w, http.StatusServiceUnavailable, "sab not configured (set sabUrl + sabApiKey in Settings)")
			return
		}
		clientID, clientErr = sb.AddURL(r.Context(), req.DownloadURL, settings.SabCategory)
		client = "sabnzbd"
		category = settings.SabCategory
	default:
		writeErr(w, http.StatusBadRequest, "unknown protocol; expected torrent or usenet")
		return
	}

	if clientErr != nil {
		s.log.Error("grab failed",
			"protocol", protocol,
			"release", req.ReleaseTitle,
			"scene_id", req.SceneID,
			"err", clientErr)
		writeErr(w, http.StatusBadGateway, client+": "+clientErr.Error())
		return
	}

	var grabID int64
	if s.grabs != nil {
		kind := req.Kind
		if kind == "" {
			kind = "single"
		}
		id, err := s.grabs.Insert(r.Context(), grabs.Grab{
			PredictedStashDBID:  req.SceneID,
			PredictedConfidence: req.Confidence,
			ReleaseTitle:        req.ReleaseTitle,
			ReleaseSize:         req.ReleaseSize,
			ReleaseIndexer:      req.ReleaseIndexer,
			DownloadURL:         req.DownloadURL,
			Client:              client,
			ClientID:            clientID,
			Category:            category,
			Status:              "queued",
			PerformerName:       req.PerformerName,
			GrabbedAt:           time.Now().Unix(),
			Kind:                kind,
			PackFiles:           req.VideoCount,
		})
		if err != nil {
			s.log.Error("grabs insert", "err", err)
		} else {
			grabID = id
		}
	}

	s.log.Info("grab queued",
		"protocol", protocol,
		"client", client,
		"release", req.ReleaseTitle,
		"scene_id", req.SceneID,
		"category", category,
		"client_id", clientID,
		"grab_id", grabID)
	writeJSON(w, http.StatusOK, grabResponse{
		OK:       true,
		Client:   client,
		Category: category,
		GrabID:   grabID,
		ClientID: clientID,
	})
}

// inferProtocol falls back when the request lacks a Protocol field
// (older UI builds, manual curls). Magnet URIs + .torrent file URLs
// → torrent; everything else assumed to be NZB.
func inferProtocol(url string) string {
	u := strings.ToLower(url)
	switch {
	case strings.HasPrefix(u, "magnet:"):
		return "torrent"
	case strings.Contains(u, ".torrent"):
		return "torrent"
	}
	return "usenet"
}
