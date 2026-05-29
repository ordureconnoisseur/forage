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

	authMu     sync.Mutex
	authedOnce bool
}

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

// Version returns qBittorrent's version string ("v5.1.4"). Used as a
// boot probe + by the manual probe tool. Hits /api/v2/app/version.
func (c *Client) Version(ctx context.Context) (string, error) {
	if err := c.Login(ctx); err != nil {
		return "", fmt.Errorf("login: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/api/v2/app/version", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
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

func (c *Client) addByFetchedFile(ctx context.Context, downloadURL, category string) error {
	// 1. Fetch the .torrent file (or in some cases a redirect to a
	// magnet URI; we handle both).
	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return fmt.Errorf("build fetch: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("fetch torrent: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("fetch torrent %d: %s", resp.StatusCode, body)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read torrent body: %w", err)
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
	if err := c.Login(ctx); err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
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
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
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

// DeleteTorrent removes a torrent from qBit. When deleteFiles is true
// qBit also deletes the downloaded data from disk. Used by the grab
// purge ("delete, no traces"): for torrents we stop seeding and wipe
// the download-client copy. Safe alongside a hardlinked library file —
// deleting qBit's copy leaves the library hardlink intact.
func (c *Client) DeleteTorrent(ctx context.Context, hash string, deleteFiles bool) error {
	if hash == "" {
		return fmt.Errorf("hash is empty")
	}
	if err := c.Login(ctx); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	form := url.Values{}
	form.Set("hashes", hash)
	if deleteFiles {
		form.Set("deleteFiles", "true")
	} else {
		form.Set("deleteFiles", "false")
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/v2/torrents/delete", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", c.baseURL)
	resp, err := c.http.Do(req)
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
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/v2/torrents/add", body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Referer", c.baseURL)

	resp, err := c.http.Do(req)
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
