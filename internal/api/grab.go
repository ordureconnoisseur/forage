package api

import (
	"context"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
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
	res, err := s.doGrab(r.Context(), req)
	if err != nil {
		var ge grabError
		if errors.As(err, &ge) {
			writeErr(w, ge.status, ge.msg)
			return
		}
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// grabError carries an HTTP status for the API wrapper while letting the
// job worker treat the failure generically.
type grabError struct {
	status int
	msg    string
}

func (e grabError) Error() string { return e.msg }

// doGrab queues a single release: routes to qBit (async torrent add) or
// SAB (sync), persists the grab row, returns the response shape. Shared
// by the /grab endpoint and the collection-job worker so both go through
// exactly one grab path. The torrent add still happens in the background
// — doGrab returns as soon as the row is inserted.
func (s *Server) doGrab(ctx context.Context, req grabRequest) (grabResponse, error) {
	if req.DownloadURL == "" {
		return grabResponse{}, grabError{http.StatusBadRequest, "download_url required"}
	}
	protocol := req.Protocol
	if protocol == "" {
		protocol = inferProtocol(req.DownloadURL)
	}
	settings := s.pool.Settings()
	kind := req.Kind
	if kind == "" {
		kind = "single"
	}

	switch protocol {
	case "torrent":
		if s.pool.Qbit() == nil {
			return grabResponse{}, grabError{http.StatusServiceUnavailable, "qbit not configured (set qbitUrl in Settings)"}
		}
		// A magnet carries its info_hash in the URI (xt=urn:btih:…), so we
		// can set client_id synchronously and skip the poller's
		// recent-additions guess entirely. That guess is purely
		// time-based, so two grabs fired in the same instant could be
		// linked to each other's torrents (a swap). Pinning the hash up
		// front removes that risk for magnets; a .torrent fetch (hash not
		// known until the bytes are parsed) still falls back to the
		// poller, which now disambiguates by name. See pickRecent.
		clientID := magnetInfoHash(req.DownloadURL)
		// Async: insert the queued row, return, fetch + add in background
		// (Prowlarr-fronted trackers can be slow). Poller links the hash
		// when we couldn't derive it here.
		grabID := s.insertGrab(ctx, req, "qbit", clientID, settings.QbitCategory, kind)
		go s.addTorrentAsync(req.DownloadURL, settings.QbitCategory, req.ReleaseTitle, grabID)
		s.log.Info("grab queued (async torrent add)",
			"release", req.ReleaseTitle, "scene_id", req.SceneID,
			"category", settings.QbitCategory, "grab_id", grabID)
		return grabResponse{OK: true, Client: "qbit", Category: settings.QbitCategory, GrabID: grabID}, nil

	case "usenet":
		sb := s.pool.Sab()
		if sb == nil {
			return grabResponse{}, grabError{http.StatusServiceUnavailable, "sab not configured (set sabUrl + sabApiKey in Settings)"}
		}
		clientID, err := sb.AddURL(ctx, req.DownloadURL, settings.SabCategory)
		if err != nil {
			s.log.Error("grab failed", "protocol", "usenet",
				"release", req.ReleaseTitle, "scene_id", req.SceneID, "err", err)
			return grabResponse{}, grabError{http.StatusBadGateway, "sabnzbd: " + err.Error()}
		}
		grabID := s.insertGrab(ctx, req, "sabnzbd", clientID, settings.SabCategory, kind)
		s.log.Info("grab queued",
			"protocol", "usenet", "client", "sabnzbd", "release", req.ReleaseTitle,
			"scene_id", req.SceneID, "category", settings.SabCategory,
			"client_id", clientID, "grab_id", grabID)
		return grabResponse{OK: true, Client: "sabnzbd", Category: settings.SabCategory, GrabID: grabID, ClientID: clientID}, nil

	default:
		return grabResponse{}, grabError{http.StatusBadRequest, "unknown protocol; expected torrent or usenet"}
	}
}

// insertGrab persists a queued grab row, returning its id (0 on failure,
// logged). Shared by both protocol paths.
func (s *Server) insertGrab(ctx context.Context, req grabRequest, client, clientID, category, kind string) int64 {
	if s.grabs == nil {
		return 0
	}
	id, err := s.grabs.Insert(ctx, grabs.Grab{
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
		return 0
	}
	return id
}

// addTorrentAsync fetches the .torrent and hands it to qBit off the
// request path. On failure it marks the grab failed so the row doesn't
// sit "queued" forever. Uses a fresh background context with a generous
// timeout (the request's ctx is already gone — the response returned).
func (s *Server) addTorrentAsync(downloadURL, category, releaseTitle string, grabID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	qb := s.pool.Qbit()
	if qb == nil {
		s.failGrab(ctx, grabID, "qbit not configured")
		return
	}
	if err := qb.AddTorrent(ctx, downloadURL, category); err != nil {
		s.log.Error("async torrent add", "release", releaseTitle, "grab_id", grabID, "err", err)
		s.failGrab(ctx, grabID, "torrent add: "+err.Error())
		return
	}
	s.log.Info("async torrent added", "release", releaseTitle, "grab_id", grabID)
}

// magnetInfoHash extracts the v1 info_hash from a magnet URI, normalised
// to the lowercase 40-char hex that qBit keys torrents by. Handles both
// hex (btih:<40 hex>) and base32 (btih:<32 base32>) encodings. Returns ""
// for non-magnets, v2-only magnets (btmh), or anything malformed — callers
// then fall back to poller-side linking.
func magnetInfoHash(downloadURL string) string {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(downloadURL)), "magnet:") {
		return ""
	}
	u, err := url.Parse(downloadURL)
	if err != nil {
		return ""
	}
	const prefix = "urn:btih:"
	for _, xt := range u.Query()["xt"] {
		if !strings.HasPrefix(xt, prefix) {
			continue
		}
		h := strings.TrimSpace(xt[len(prefix):])
		switch len(h) {
		case 40:
			if _, err := hex.DecodeString(h); err == nil {
				return strings.ToLower(h)
			}
		case 32:
			b, err := base32.StdEncoding.DecodeString(strings.ToUpper(h))
			if err == nil && len(b) == 20 {
				return hex.EncodeToString(b)
			}
		}
	}
	return ""
}

// failGrab transitions a grab to failed with a reason. Best-effort.
func (s *Server) failGrab(ctx context.Context, grabID int64, reason string) {
	if s.grabs == nil || grabID == 0 {
		return
	}
	g, err := s.grabs.Get(ctx, grabID)
	if err != nil || g == nil {
		return
	}
	g.Status = "failed"
	g.Reason = reason
	if err := s.grabs.Update(ctx, *g); err != nil {
		s.log.Warn("mark grab failed", "grab_id", grabID, "err", err)
	}
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
