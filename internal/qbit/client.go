// Package qbit is a thin client for qBittorrent's Web API.
//
// Same shape as internal/prowlarr — raw net/http, hand-crafted requests,
// no third-party deps. Supports both no-auth deployments (where qBit has
// `bypass_local_auth` enabled) and password login (cookie-based SID).
package qbit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Client struct {
	baseURL  string
	username string
	password string
	http     *http.Client
	// fetchHTTP downloads .torrent files from Prowlarr's /download proxy,
	// which in turn proxies the real tracker — slow/Cloudflare-gated
	// indexers (e.g. 1337x on a cold challenge) can take well past the
	// 30s the qBit *API* client uses. A separate, longer-timeout client
	// keeps qBit API calls snappy while giving the torrent fetch room.
	fetchHTTP *http.Client

	authMu     sync.Mutex
	authedOnce bool
}

// torrentFetchTimeout bounds a single .torrent download attempt from
// Prowlarr. Measured: a warm 1337x fetch is ~15s; a cold Cloudflare
// challenge runs longer, so allow generous headroom per attempt.
const torrentFetchTimeout = 90 * time.Second

// torrentFetchBudget bounds ALL attempts together, so a dead URL can't
// hang a grab (and, during a bulk grab, the queue) for retries × 90s.
// One slow-but-successful fetch fits; a hopeless one fails within this.
const torrentFetchBudget = 100 * time.Second

// fetchRetries is how many EXTRA attempts a timed-out/failed torrent
// fetch gets (so total attempts = fetchRetries + 1), subject to the
// overall budget above.
const fetchRetries = 2

func New(baseURL, username, password string) *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		username: username,
		password: password,
		http: &http.Client{
			Timeout: 30 * time.Second,
			Jar:     jar,
		},
		fetchHTTP: &http.Client{
			Timeout: torrentFetchTimeout,
			Jar:     jar,
		},
	}
}

// Login authenticates against /api/v2/auth/login and stows the SID
// cookie in the client's jar. No-op if username is empty — qBit's
// bypass_local_auth then handles requests without a session.
//
// Safe to call concurrently; only the first goroutine in a window
// actually hits the wire.
func (c *Client) Login(ctx context.Context) error {
	if c.username == "" {
		return nil
	}
	c.authMu.Lock()
	defer c.authMu.Unlock()
	if c.authedOnce {
		return nil
	}
	form := url.Values{}
	form.Set("username", c.username)
	form.Set("password", c.password)
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/v2/auth/login", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// qBit insists on a Referer header matching baseURL when CSRF is on;
	// harmless when off.
	req.Header.Set("Referer", c.baseURL)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("qbit login %d: %s", resp.StatusCode, body)
	}
	// qBit replies "Ok." on success, "Fails." otherwise — both with HTTP 200.
	if strings.TrimSpace(string(body)) != "Ok." {
		return fmt.Errorf("qbit login refused: %s", body)
	}
	c.authedOnce = true
	return nil
}

// relogin clears the cached session and forces a fresh login. Called
// when qBit answers 403, meaning the SID cookie is no longer valid
// (qBit's WebUI session expired, or qBit/the VPN container restarted out
// from under a long-running download).
func (c *Client) relogin(ctx context.Context) error {
	c.authMu.Lock()
	c.authedOnce = false
	c.authMu.Unlock()
	return c.Login(ctx)
}

// authedDo runs an authenticated request against qBit, transparently
// re-logging-in and replaying it ONCE if qBit rejects the session with
// 403. Without this, authedOnce latches true for the life of the process
// and a session that expires mid-pack (or a qBit restart) wedges every
// subsequent poll — the grab never advances. build is called fresh per
// attempt so request bodies can be replayed safely.
func (c *Client) authedDo(ctx context.Context, build func() (*http.Request, error)) (*http.Response, error) {
	if err := c.Login(ctx); err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	req, err := build()
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusForbidden {
		return resp, nil
	}
	resp.Body.Close()
	if err := c.relogin(ctx); err != nil {
		return nil, fmt.Errorf("re-login: %w", err)
	}
	req, err = build()
	if err != nil {
		return nil, err
	}
	return c.http.Do(req)
}

// Version returns qBittorrent's version string ("v5.1.4"). Used as a
// boot probe + by the manual probe tool. Hits /api/v2/app/version.
func (c *Client) Version(ctx context.Context) (string, error) {
	resp, err := c.authedDo(ctx, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, "GET", c.baseURL+"/api/v2/app/version", nil)
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("qbit version %d: %s", resp.StatusCode, body)
	}
	return strings.TrimSpace(string(body)), nil
}

// AddTorrent hands a download target to qBit. For magnet URIs we pass
// the URI straight through (qBit handles BEP-09 itself). For HTTP(S)
// .torrent URLs we fetch the bytes ourselves and upload them as a
// file, rather than letting qBit fetch — that decouples qBit from the
// indexer/Prowlarr network reachability (qBit in this setup runs
// inside a VPN container that can't resolve `host.docker.internal`
// back to the Prowlarr host).
//
// Returns nil on both fresh-add and duplicate-of-existing scenarios:
// qBit's response is "Ok." either way, and the user observes the
// torrent in qBit's UI either way.
func (c *Client) AddTorrent(ctx context.Context, downloadURL, category string) error {
	if downloadURL == "" {
		return fmt.Errorf("download URL is empty")
	}
	if err := c.Login(ctx); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if strings.HasPrefix(downloadURL, "magnet:") {
		return c.addByURLs(ctx, downloadURL, category)
	}
	return c.addByFetchedFile(ctx, downloadURL, category)
}

// addByURLs is the simple form qBit handles natively — used only for
// magnet URIs. HTTP .torrent URLs go through addByFetchedFile.
func (c *Client) addByURLs(ctx context.Context, urlOrMagnet, category string) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("urls", urlOrMagnet)
	if category != "" {
		_ = mw.WriteField("category", category)
	}
	_ = mw.WriteField("paused", "false")
	_ = mw.Close()
	return c.postAdd(ctx, &buf, mw.FormDataContentType())
}

// fetchTorrentBytes downloads a .torrent (or a magnet body) from a
// Prowlarr /download URL using the longer-timeout fetchHTTP client, with
// retries on a failed/timed-out attempt. Returns the raw body. The
// caller's ctx still bounds the whole operation — if it's cancelled, we
// stop retrying.
func (c *Client) fetchTorrentBytes(ctx context.Context, downloadURL string) ([]byte, error) {
	// Cap the whole retry loop so a hopeless URL fails fast instead of
	// blocking the grab (and the bulk queue) for retries × per-attempt.
	ctx, cancel := context.WithTimeout(ctx, torrentFetchBudget)
	defer cancel()
	var lastErr error
	for attempt := 0; attempt <= fetchRetries; attempt++ {
		if ctx.Err() != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, ctx.Err()
		}
		req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
		if err != nil {
			return nil, fmt.Errorf("build fetch: %w", err)
		}
		resp, err := c.fetchHTTP.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("fetch torrent: %w", err)
			continue // transient (timeout / reset) — retry
		}
		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			// 5xx is worth a retry (Prowlarr/tracker hiccup); 4xx isn't.
			lastErr = fmt.Errorf("fetch torrent %d: %s", resp.StatusCode, b)
			if resp.StatusCode < 500 {
				return nil, lastErr
			}
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("read torrent body: %w", err)
			continue
		}
		return body, nil
	}
	return nil, lastErr
}

func (c *Client) addByFetchedFile(ctx context.Context, downloadURL, category string) error {
	// 1. Fetch the .torrent file (or in some cases a redirect to a
	// magnet URI; we handle both). Retries on a transient stall — the
	// Prowlarr /download proxy fronts the real tracker, and a cold
	// Cloudflare challenge / slow indexer often succeeds on a second try.
	body, err := c.fetchTorrentBytes(ctx, downloadURL)
	if err != nil {
		return err
	}

	// Some indexers reply with a magnet URI in the body rather than a
	// .torrent file. qBit can take those via the `urls` field directly.
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte("magnet:")) {
		return c.addByURLs(ctx, strings.TrimSpace(string(body)), category)
	}

	// 2. Upload the .torrent bytes as a file.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if category != "" {
		_ = mw.WriteField("category", category)
	}
	_ = mw.WriteField("paused", "false")
	fileWriter, err := mw.CreateFormFile("torrents", "download.torrent")
	if err != nil {
		return fmt.Errorf("multipart file: %w", err)
	}
	if _, err := fileWriter.Write(body); err != nil {
		return fmt.Errorf("write torrent body: %w", err)
	}
	_ = mw.Close()
	return c.postAdd(ctx, &buf, mw.FormDataContentType())
}

// AddTorrentFile uploads raw .torrent bytes (e.g. a file the user
// supplied manually) to qBit under the given category. Same upload path
// as a fetched .torrent, minus the fetch.
func (c *Client) AddTorrentFile(ctx context.Context, data []byte, category string) error {
	if len(data) == 0 {
		return fmt.Errorf("torrent file is empty")
	}
	if err := c.Login(ctx); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if category != "" {
		_ = mw.WriteField("category", category)
	}
	_ = mw.WriteField("paused", "false")
	fileWriter, err := mw.CreateFormFile("torrents", "upload.torrent")
	if err != nil {
		return fmt.Errorf("multipart file: %w", err)
	}
	if _, err := fileWriter.Write(data); err != nil {
		return fmt.Errorf("write torrent body: %w", err)
	}
	_ = mw.Close()
	return c.postAdd(ctx, &buf, mw.FormDataContentType())
}

// ── torrent listing / inspection ─────────────────────────────────────

// Torrent is the slim shape forager's poller needs out of qBit's
// torrent listing. The full /api/v2/torrents/info response has dozens
// of fields; we project to what advances the grabs state machine.
type Torrent struct {
	Hash        string  `json:"hash"`
	Name        string  `json:"name"`
	Category    string  `json:"category"`
	State       string  `json:"state"`
	Progress    float64 `json:"progress"` // 0..1
	Dlspeed     int64   `json:"dlspeed"`  // bytes/s
	Eta         int64   `json:"eta"`      // seconds; 8640000 = unknown/infinity
	AddedOn     int64   `json:"added_on"`
	ContentPath string  `json:"content_path"`
}

// ListOpts maps to /api/v2/torrents/info query params. Zero-values
// omit the filter on the wire.
type ListOpts struct {
	Filter   string // "all" | "downloading" | "completed" | "active" | "inactive"
	Category string
	Sort     string // e.g. "added_on"
	Reverse  bool
	Limit    int
}

// ListTorrents returns qBit's torrent list filtered + sorted per opts.
// Used by the poller for two purposes: enriching a freshly-added grab
// with its info_hash (qBit's `/torrents/add` doesn't return the hash),
// and looking up state for already-tracked grabs.
func (c *Client) ListTorrents(ctx context.Context, opts ListOpts) ([]Torrent, error) {
	q := url.Values{}
	if opts.Filter != "" {
		q.Set("filter", opts.Filter)
	}
	if opts.Category != "" {
		q.Set("category", opts.Category)
	}
	if opts.Sort != "" {
		q.Set("sort", opts.Sort)
	}
	if opts.Reverse {
		q.Set("reverse", "true")
	}
	if opts.Limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", opts.Limit))
	}
	u := c.baseURL + "/api/v2/torrents/info"
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	resp, err := c.authedDo(ctx, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, "GET", u, nil)
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("qbit list %d: %s", resp.StatusCode, body)
	}
	var out []Torrent
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode list: %w", err)
	}
	return out, nil
}

// TorrentInfo returns a single torrent by hash, or nil if qBit doesn't
// know about it (deleted, never added, race). Wraps ListTorrents with
// a single-result filter rather than hitting /torrents/properties,
// which returns a different (more verbose) shape.
func (c *Client) TorrentInfo(ctx context.Context, hash string) (*Torrent, error) {
	if hash == "" {
		return nil, nil
	}
	ts, err := c.ListTorrents(ctx, ListOpts{Filter: "all"})
	if err != nil {
		return nil, err
	}
	for i := range ts {
		if strings.EqualFold(ts[i].Hash, hash) {
			return &ts[i], nil
		}
	}
	return nil, nil
}

// TorrentFile is one entry from /api/v2/torrents/files.
type TorrentFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// TorrentFiles returns a torrent's file list by hash. qBit knows this
// from the metainfo, so it's available regardless of download progress —
// the adoption path uses it to classify pack-vs-single before a download
// completes. Returns nil for an empty hash.
func (c *Client) TorrentFiles(ctx context.Context, hash string) ([]TorrentFile, error) {
	if hash == "" {
		return nil, nil
	}
	u := c.baseURL + "/api/v2/torrents/files?hash=" + url.QueryEscape(hash)
	resp, err := c.authedDo(ctx, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, "GET", u, nil)
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("qbit files %d: %s", resp.StatusCode, body)
	}
	var out []TorrentFile
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode files: %w", err)
	}
	return out, nil
}

// DeleteTorrent removes a torrent from qBit. When deleteFiles is true
// qBit also deletes the downloaded data from disk. Used by the grab
// purge ("delete, no traces"): for torrents we stop seeding and wipe
// the download-client copy. Safe alongside a hardlinked library file —
// deleting qBit's copy leaves the library hardlink intact.
func (c *Client) DeleteTorrent(ctx context.Context, hash string, deleteFiles bool) error {
	if hash == "" {
		return fmt.Errorf("hash is empty")
	}
	form := url.Values{}
	form.Set("hashes", hash)
	if deleteFiles {
		form.Set("deleteFiles", "true")
	} else {
		form.Set("deleteFiles", "false")
	}
	enc := form.Encode()
	resp, err := c.authedDo(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/v2/torrents/delete", strings.NewReader(enc))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Referer", c.baseURL)
		return req, nil
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("qbit delete %d: %s", resp.StatusCode, body)
	}
	return nil
}

func (c *Client) postAdd(ctx context.Context, body *bytes.Buffer, contentType string) error {
	raw := body.Bytes()
	resp, err := c.authedDo(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/v2/torrents/add", bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Referer", c.baseURL)
		return req, nil
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("qbit add %d: %s", resp.StatusCode, respBody)
	}
	if strings.TrimSpace(string(respBody)) == "Fails." {
		return fmt.Errorf("qbit refused the torrent (could not parse)")
	}
	return nil
}
