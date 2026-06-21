package api

import (
	"context"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
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
	// Force bypasses the disk-space preflight (the user chose to grab
	// anyway despite the library volume looking too full).
	Force bool `json:"force,omitempty"`
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
		writeMappedErr(w, err, http.StatusBadGateway)
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
	// Scheme allowlist: forage only ever fetches a .torrent over http(s) or
	// hands a magnet straight to qBit. Reject anything else (file://,
	// gopher://, ftp://, …) so a crafted download_url can't point the daemon
	// at an unintended scheme. In practice the URL comes from Prowlarr, but
	// the endpoint is callable, so validate at the boundary.
	if !validGrabURL(req.DownloadURL) {
		return grabResponse{}, grabError{http.StatusBadRequest,
			"download_url must be an http(s) or magnet URL"}
	}
	// Idempotency: if a non-failed grab for this exact URL already exists,
	// return it instead of creating a second. Stops a double-click — or two
	// job-review "grab" requests racing on the same suggestion — from
	// duplicating the download. A FAILED prior grab doesn't block: re-grabbing
	// the same URL is a legitimate fresh attempt. (MaxOpenConns(1) serialises
	// the check+insert, so sequential clicks are caught; a truly simultaneous
	// pair has a tiny residual window, acceptable vs a download_url unique
	// index that would forbid post-failure re-grabs.)
	if s.grabs != nil {
		if existing, err := s.grabs.ByDownloadURL(ctx, req.DownloadURL); err == nil &&
			existing != nil && existing.Status != "failed" {
			s.log.Info("grab deduped: non-failed grab for this URL exists",
				"grab_id", existing.ID, "status", existing.Status, "release", req.ReleaseTitle)
			return grabResponse{
				OK: true, Client: existing.Client, Category: existing.Category,
				GrabID: existing.ID, ClientID: existing.ClientID,
			}, nil
		}
	}

	protocol := req.Protocol
	if protocol == "" {
		protocol = inferProtocol(req.DownloadURL)
	}

	// Disk-space preflight: refuse a grab that won't fit on the library
	// volume (hardlink placement means staging shares that filesystem).
	// Skipped when placement is off or the size is unknown; the user can
	// override with force.
	if !req.Force && req.ReleaseSize > 0 {
		if pl := s.pool.Placer(); pl.Configured() {
			if free, err := pl.FreeSpace(); err == nil && free > 0 && uint64(req.ReleaseSize) > free {
				return grabResponse{}, grabError{http.StatusInsufficientStorage,
					fmt.Sprintf("not enough free space: needs %s, only %s free on the library volume — grab anyway to override",
						humanBytes(req.ReleaseSize), humanBytes(int64(free)))}
			}
		}
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

// Bounds for auto-retrying a rate-limited .torrent fetch (indexer HTTP
// 429/503). Backoff is exponential off the base, capped — so a bulk batch
// that trips the indexer's limit fills in on its own instead of a wall of
// fails. minFetchInterval spaces fetches so the burst doesn't 429 to begin
// with.
const (
	addMaxAttempts   = 5
	addBackoffBase   = 20 * time.Second
	addBackoffCap    = 3 * time.Minute
	minFetchInterval = 2 * time.Second
)

// fetchGate spaces out .torrent fetches. Each caller reserves the next slot
// (>= minFetchInterval after the previous) and waits for it, so concurrent
// bulk adds/retries trickle to the indexer instead of bursting.
type fetchGate struct {
	mu   sync.Mutex
	next time.Time
}

func (g *fetchGate) wait(ctx context.Context) {
	g.mu.Lock()
	at := g.next
	if now := time.Now(); at.Before(now) {
		at = now
	}
	g.next = at.Add(minFetchInterval)
	g.mu.Unlock()
	d := time.Until(at)
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// isTransientFetchErr reports whether a torrent-add failure is a transient
// indexer rate-limit / unavailability (HTTP 429 / 503) worth auto-retrying,
// vs a permanent problem (bad torrent, qbit declined) that should fail now.
func isTransientFetchErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "429") ||
		strings.Contains(s, "503") ||
		strings.Contains(s, "too many requests") ||
		strings.Contains(s, "rate limit")
}

func addBackoff(attempt int) time.Duration {
	d := addBackoffBase << (attempt - 1)
	if d <= 0 || d > addBackoffCap {
		d = addBackoffCap
	}
	return d
}

// addTorrentAsync fetches the .torrent and hands it to qBit off the request
// path. A transient indexer error (429/503) is auto-retried with backoff
// (the grab stays queued); a permanent error fails the grab so the row
// doesn't sit "queued" forever.
func (s *Server) addTorrentAsync(downloadURL, category, releaseTitle string, grabID int64) {
	s.addTorrentAttempt(downloadURL, category, releaseTitle, grabID, 1)
}

func (s *Server) addTorrentAttempt(downloadURL, category, releaseTitle string, grabID int64, attempt int) {
	qb := s.pool.Qbit()
	if qb == nil {
		s.failGrab(context.Background(), grabID, "qbit not configured")
		return
	}
	// Wait out the fetch-gate slot BEFORE starting the add's two-minute
	// budget. The gate spaces fetches minFetchInterval apart, so the tail
	// of a bulk batch queues behind it for minutes; with the deadline
	// started first (the old order) that queueing burned the whole budget
	// and every grab past ~60 in a batch hard-failed on a context deadline
	// without a single fetch attempt, unretried (a deadline isn't a
	// transient indexer error).
	s.torrentGate.wait(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// After the (possibly long) gate wait, bail if the grab is gone or no
	// longer queued: deleted, retried by the user, or already linked. Also
	// what stops a backoff re-attempt from double-adding.
	if grabID != 0 && s.grabs != nil {
		if g, _ := s.grabs.Get(ctx, grabID); g == nil || g.Status != "queued" {
			return
		}
	}
	hash, err := qb.AddTorrent(ctx, downloadURL, category)
	if err != nil {
		if isTransientFetchErr(err) && attempt < addMaxAttempts {
			d := addBackoff(attempt)
			s.log.Warn("torrent add rate-limited; auto-retrying",
				"grab_id", grabID, "attempt", attempt, "retry_in", d.String(), "err", err)
			if s.grabs != nil {
				_ = s.applyGrabUpdate(ctx, grabID, func(g *grabs.Grab) {
					if g.Status == "queued" {
						g.Reason = fmt.Sprintf("indexer rate-limited — auto-retrying (%d/%d)", attempt, addMaxAttempts)
					}
				})
			}
			time.AfterFunc(d, func() {
				s.addTorrentAttempt(downloadURL, category, releaseTitle, grabID, attempt+1)
			})
			return
		}
		s.log.Error("async torrent add", "release", releaseTitle, "grab_id", grabID, "attempt", attempt, "err", err)
		s.failGrab(context.Background(), grabID, "torrent add: "+err.Error())
		return
	}
	// Link the grab to its qBit torrent by info-hash when we have it, only if
	// the poller hasn't already linked it (pickRecent) — CAS-retry so a
	// concurrent tick write doesn't make us lose the hash.
	if hash != "" && s.grabs != nil {
		if uerr := s.applyGrabUpdate(ctx, grabID, func(g *grabs.Grab) {
			if g.ClientID == "" {
				g.ClientID = hash
			}
		}); uerr != nil {
			s.log.Warn("link grab hash", "grab_id", grabID, "err", uerr)
		}
	} else if hash == "" && grabID != 0 && s.grabs != nil {
		// No hash to link (a magnet whose URI hash we couldn't parse): the
		// poller links it via pickRecent, whose claim window is anchored on
		// GrabbedAt. This add may have landed minutes after the row was
		// inserted (fetch-gate queueing, 429 backoff), so re-stamp GrabbedAt
		// to the actual add time or the window can't contain the torrent's
		// AddedOn. Only while still queued and unlinked, so an already
		// linked or user-mutated grab keeps its clocks.
		if uerr := s.applyGrabUpdate(ctx, grabID, func(g *grabs.Grab) {
			if g.ClientID == "" && g.Status == "queued" {
				g.GrabbedAt = time.Now().Unix()
			}
		}); uerr != nil {
			s.log.Warn("re-stamp grab add time", "grab_id", grabID, "err", uerr)
		}
	}
	s.log.Info("async torrent added", "release", releaseTitle, "grab_id", grabID, "hash", hash, "attempt", attempt)
}

// postGrabRetry re-attempts a failed grab using its stored download URL —
// resets the row to queued and re-runs the add. Good for transient
// failures (indexer hiccup, network blip); a tracker download cap will
// just fail again, where "Pick another release" is the real fix.
func (s *Server) postGrabRetry(w http.ResponseWriter, r *http.Request) {
	if s.grabs == nil {
		writeErr(w, http.StatusServiceUnavailable, "grabs unavailable")
		return
	}
	g, ok := s.grabByID(w, r)
	if !ok {
		return
	}
	if err := s.retryGrab(r.Context(), g); err != nil {
		writeMappedErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// retryGrab re-queues one failed grab: clears the prior failure + client
// link and re-adds to the download client (async for qBit, sync for SAB).
// Returns a grabError carrying an HTTP status for the typed failures.
// Shared by the single-retry endpoint and the bulk retry-all-failed.
func (s *Server) retryGrab(ctx context.Context, g *grabs.Grab) error {
	if g.Status != "failed" {
		return grabError{http.StatusUnprocessableEntity, "only failed grabs can be retried"}
	}
	if g.DownloadURL == "" {
		return grabError{http.StatusUnprocessableEntity, "grab has no download URL to retry"}
	}
	settings := s.pool.Settings()
	// Reset the failure, but KEEP the client link: a torrent's info-hash
	// doesn't change on retry, and clearing ClientID would drop the torrent
	// out of KnownClientIDs — so a still-present qBit torrent (with its
	// original, often past-grace AddedOn) gets adopted as a DUPLICATE grab.
	// addTorrentAsync re-adds idempotently and links the hash only if we don't
	// already have one.
	//
	// The write goes through applyGrabUpdate (Get → apply → CAS-Update, retried
	// on a stale rev) rather than from this handler's snapshot: a poller tick
	// that advances the row between the handler's Get and our write would make a
	// bare Update lose the optimistic lock — surfacing as a spurious 500 (single
	// retry) or a silently-skipped grab (bulk retry-all). applyReset is
	// deterministic, so re-running it on a freshly-loaded row is idempotent,
	// which applyGrabUpdate requires.
	//
	// A retry is a fresh add to the client, so re-arm every clock keyed on
	// GrabbedAt — the SAB register grace (else the first tick can't find the
	// new nzo and re-fails it), the qBit link timeout, and pickRecent's
	// window — and reset the stall baseline so the restarted download isn't
	// instantly badged stalled off the old progress timestamp. The lifecycle
	// timestamps reset too: a stale completed_at from the previous attempt
	// would otherwise feed the orphan/identify-grace clocks, instantly
	// orphaning the new attempt (or prematurely confirming a pack) when the
	// retry happens later than the orphan window after the original completion.
	//
	// EXCEPT when the previous attempt already placed a file: the poller's
	// premature-placement heal treats "placed_path set + download progress
	// < 1 + completed_at unset" as a partial file to RemoveAll, so zeroing
	// completed_at while keeping placed_path would re-arm that heal against
	// a legitimate library copy the moment the retried download starts. A
	// placed grab keeps its lifecycle stamps; its file confirms on the
	// placed path regardless of the new attempt's clock.
	applyReset := func(fresh *grabs.Grab) {
		fresh.Status = "queued"
		fresh.Reason = "retry requested"
		fresh.PlaceError = ""
		now := time.Now().Unix()
		fresh.GrabbedAt = now
		fresh.Progress = 0
		fresh.ProgressAt = now
		if fresh.PlacedPath == "" {
			fresh.CompletedAt = 0
			fresh.PlacedAt = 0
			fresh.ConfirmedAt = 0
		}
	}

	switch g.Client {
	case "qbit":
		if s.pool.Qbit() == nil {
			return grabError{http.StatusServiceUnavailable, "qbit not configured"}
		}
		cat := g.Category
		if cat == "" {
			cat = settings.QbitCategory
		}
		if err := s.applyGrabUpdate(ctx, g.ID, func(fresh *grabs.Grab) {
			applyReset(fresh)
			fresh.Category = cat
			if fresh.ClientID == "" {
				fresh.ClientID = magnetInfoHash(fresh.DownloadURL) // pin magnet hash when we have none
			}
		}); err != nil {
			return err
		}
		go s.addTorrentAsync(g.DownloadURL, cat, g.ReleaseTitle, g.ID)
	case "sabnzbd":
		sb := s.pool.Sab()
		if sb == nil {
			return grabError{http.StatusServiceUnavailable, "sab not configured"}
		}
		cat := g.Category
		if cat == "" {
			cat = settings.SabCategory
		}
		// AddURL is a real side effect (it enqueues the NZB) and returns the
		// nzo_id we must persist, so it runs ONCE here, before the CAS write —
		// not inside the applyGrabUpdate closure, which can re-run on a stale
		// row and would enqueue the download twice.
		clientID, aerr := sb.AddURL(ctx, g.DownloadURL, cat)
		if aerr != nil {
			s.failGrab(ctx, g.ID, "retry add: "+aerr.Error())
			return grabError{http.StatusBadGateway, "sabnzbd: " + aerr.Error()}
		}
		if err := s.applyGrabUpdate(ctx, g.ID, func(fresh *grabs.Grab) {
			applyReset(fresh)
			fresh.Category = cat
			fresh.ClientID = clientID
		}); err != nil {
			return err
		}
	default:
		return grabError{http.StatusUnprocessableEntity, "unknown client; can't retry"}
	}
	s.log.Info("grab retry", "id", g.ID, "client", g.Client, "release", g.ReleaseTitle)
	return nil
}

// postRetryAllFailed re-queues every failed grab that has a download URL —
// the bulk recovery after a collection-job batch where many grabs failed.
// Grabs with no URL (or another client issue) are skipped, not fatal.
func (s *Server) postRetryAllFailed(w http.ResponseWriter, r *http.Request) {
	if s.grabs == nil {
		writeErr(w, http.StatusServiceUnavailable, "grabs unavailable")
		return
	}
	failed, err := s.grabs.List(r.Context(), "failed", 500, 0)
	if err != nil {
		s.log.Error("retry-all list", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	retried, skipped := 0, 0
	for i := range failed {
		g := failed[i]
		if rerr := s.retryGrab(r.Context(), &g); rerr != nil {
			skipped++
			continue
		}
		retried++
	}
	s.log.Info("retry all failed", "retried", retried, "skipped", skipped)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "retried": retried, "skipped": skipped})
}

// applyGrabUpdate loads the grab fresh, runs apply (which sets the handler's
// final fields), and persists it with the optimistic-lock retry. If a
// concurrent poller tick bumped the row between our Get and Update, the CAS
// rejects the write (ErrStaleUpdate) and we re-Get + re-apply once more, so
// an API mutation never spuriously fails — or clobbers — because the tick
// happened to write in the same instant. apply MUST be idempotent (it re-runs
// on a fresh row).
func (s *Server) applyGrabUpdate(ctx context.Context, id int64, apply func(*grabs.Grab)) error {
	for attempt := 0; attempt < 3; attempt++ {
		g, err := s.grabs.Get(ctx, id)
		if err != nil {
			return err
		}
		if g == nil {
			return fmt.Errorf("grab %d not found", id)
		}
		apply(g)
		err = s.grabs.Update(ctx, *g)
		if errors.Is(err, grabs.ErrStaleUpdate) {
			continue // someone wrote between Get and Update — reload + retry
		}
		return err
	}
	return grabs.ErrStaleUpdate
}

// humanBytes formats a byte count as a compact GB/MB string for grab
// error messages.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0fMB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%dB", n)
	}
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
	// Only fail a grab still sitting in 'queued'. If the poller has already
	// advanced it (downloading/placed/…), the add "failure" was a false alarm
	// — the client accepted the torrent but our add-response read tripped —
	// and regressing a live download to failed would be wrong. (The CAS in
	// Update covers a poller write that lands AFTER our Get; this covers the
	// case where we loaded the already-advanced row.)
	if g.Status != "queued" {
		s.log.Info("skip fail: grab already advanced past queued", "grab_id", grabID, "status", g.Status)
		return
	}
	g.Status = "failed"
	g.Reason = reason
	if err := s.grabs.Update(ctx, *g); err != nil && !errors.Is(err, grabs.ErrStaleUpdate) {
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

// validGrabURL accepts only the schemes forage actually acts on: a magnet
// URI (handed to qBit) or an http(s) URL (a .torrent / NZB the daemon
// fetches). Everything else is rejected so a crafted download_url can't
// steer the daemon at file://, ftp://, gopher://, etc.
func validGrabURL(raw string) bool {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(strings.ToLower(s), "magnet:") {
		return true
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
