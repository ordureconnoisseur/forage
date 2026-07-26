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
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ordureconnoisseur/forager/internal/clienterr"
	"github.com/ordureconnoisseur/forager/internal/torrentmeta"
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
			// Follow http(s) redirects normally, but STOP before a magnet:
			// (or any non-http) target — the Go client can't GET those and
			// would fail the whole fetch. Surfacing the 3xx lets
			// fetchTorrentBytes hand the magnet straight to qBit. Aggregators
			// like Knaben 301 their /download proxy to a magnet.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if req.URL != nil && req.URL.Scheme != "http" && req.URL.Scheme != "https" {
					return http.ErrUseLastResponse
				}
				if len(via) >= 10 {
					return http.ErrUseLastResponse
				}
				return nil
			},
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
		return clienterr.Transport("qbit login", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return clienterr.Status("qbit login", resp.StatusCode, body)
	}
	// qBit replies "Ok." on success, "Fails." otherwise — both with HTTP 200.
	if strings.TrimSpace(string(body)) != "Ok." {
		return fmt.Errorf("qbit login refused: %s (%w)", body, clienterr.ErrRejected)
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
		return nil, clienterr.Transport("qbit request", err)
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
	resp, err = c.http.Do(req)
	return resp, clienterr.Transport("qbit request", err)
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
		return "", clienterr.Status("qbit version", resp.StatusCode, body)
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
// AddTorrent adds a magnet or .torrent URL to qBit. It returns the v1
// info-hash when known (always for a fetched .torrent; empty for a magnet,
// whose hash the caller derives from the URI) so the grab can be linked
// without the poller's recent-additions guess.
func (c *Client) AddTorrent(ctx context.Context, downloadURL, category string) (string, error) {
	if downloadURL == "" {
		return "", fmt.Errorf("download URL is empty")
	}
	if err := c.Login(ctx); err != nil {
		return "", fmt.Errorf("login: %w", err)
	}
	if strings.HasPrefix(downloadURL, "magnet:") {
		err := c.addByURLs(ctx, downloadURL, category)
		if err == nil {
			return "", nil
		}
		// qBit refuses a duplicate magnet with the same bare "Fails." it
		// uses for a genuinely unparseable one. When the URI carries its
		// btih and that torrent is already present, this is the duplicate
		// case — recover it exactly like the .torrent path instead of
		// failing a grab whose download is already sitting in qBit.
		if hash := torrentmeta.MagnetInfoHash(downloadURL); hash != "" {
			if recovered, rerr := c.recoverDuplicateAdd(ctx, hash, category, func() error {
				return c.addByURLs(ctx, downloadURL, category)
			}); recovered {
				return hash, rerr
			}
		}
		return "", err
	}
	return c.addByFetchedFile(ctx, downloadURL, category)
}

// recoverDuplicateAdd handles qBit's "Fails." refusal when the refused
// info-hash turns out to already be present: the add is a recoverable
// success (the grab links to the existing download), not a failure.
// Returns recovered=false when the hash isn't in qBit after all — the
// refusal really was a parse failure and the caller should surface its
// own error. readd re-attempts the original add after a dead
// missingFiles husk is cleared.
func (c *Client) recoverDuplicateAdd(ctx context.Context, hash, category string, readd func() error) (recovered bool, err error) {
	t, terr := c.TorrentInfo(ctx, hash)
	if terr != nil || t == nil {
		return false, nil
	}
	// A missingFiles husk — the data was deleted out from under the old
	// entry (a manual download later removed from the library, say) — can
	// never download again, and its mere presence blocks every future add
	// of this hash. Whatever category it nominally belongs to, it's dead
	// for everyone: drop the entry (its files are already gone) and add
	// fresh.
	if t.State == "missingFiles" {
		if derr := c.DeleteTorrent(ctx, hash, false); derr == nil {
			if rerr := readd(); rerr == nil {
				return true, nil
			}
		}
		return true, fmt.Errorf("a stale qbit entry for this torrent had lost its files; clearing it didn't unblock the add — remove the torrent in qBit and retry")
	}
	// The grab now rides a torrent added outside forage. If it's
	// uncategorised (a manual add), adopt it properly: set the forage
	// category and start it, so a paused 0% torrent doesn't wedge the grab
	// with no hint why. But a torrent that BELONGS to another category (a
	// cross-seed under the *arr stack, say) is another app's: re-
	// categorising it can physically relocate the payload under Auto
	// Torrent Management and break that app's import/seeding, so leave it
	// entirely alone — the hash link alone is the success.
	if t.Category == "" || t.Category == category {
		if category != "" && t.Category != category {
			_ = c.SetCategory(ctx, hash, category)
		}
		_ = c.Resume(ctx, hash)
	}
	return true, nil
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
			return nil, clienterr.Transport("fetch torrent", ctx.Err())
		}
		req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
		if err != nil {
			return nil, fmt.Errorf("build fetch: %w", err)
		}
		resp, err := c.fetchHTTP.Do(req)
		if err != nil {
			lastErr = clienterr.Transport("fetch torrent", err)
			continue // transient (timeout / reset) — retry
		}
		// A redirect we deliberately didn't follow (CheckRedirect stops at a
		// magnet: target). Hand the magnet back as the "body" so the caller's
		// magnet-body path adds it via qBit's urls field. http(s) redirects
		// were already followed, so a 3xx here is a non-http target.
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc := resp.Header.Get("Location")
			resp.Body.Close()
			if strings.HasPrefix(loc, "magnet:") {
				return []byte(loc), nil
			}
			return nil, fmt.Errorf("fetch torrent: redirect to unusable target %q", loc)
		}
		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			// 5xx is worth a retry (Prowlarr/tracker hiccup); 4xx isn't.
			// clienterr.Status classifies the body accordingly (5xx ->
			// ErrTransient, 4xx -> ErrRejected/ErrNotFound).
			lastErr = clienterr.Status("fetch torrent", resp.StatusCode, b)
			if resp.StatusCode < 500 {
				return nil, lastErr
			}
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = clienterr.Transport("read torrent body", err)
			continue
		}
		return body, nil
	}
	return nil, lastErr
}

// looksLikeTorrent reports whether the body is a bencoded .torrent (a
// top-level dict containing an `info` key) rather than the HTML/error page
// some indexers return when a download is rate-limited or the session has
// lapsed. Cheap structural check — not a full bencode parse.
func looksLikeTorrent(b []byte) bool {
	t := bytes.TrimLeft(b, " \t\r\n")
	return len(t) > 0 && t[0] == 'd' && bytes.Contains(b, []byte("4:info"))
}

// ErrIndexerFetch marks a failure in the INDEXER-side stage of a torrent
// add: fetching the .torrent bytes through the Prowlarr /download proxy.
// Callers use errors.Is to distinguish "the indexer couldn't serve the
// release" (worth failing over to the same scene's release on a
// different indexer) from "qBit couldn't take it" (worth retrying the
// same release once the client recovers). Composes with the clienterr
// sentinels: a fetch 429/5xx is both ErrIndexerFetch and ErrTransient.
var ErrIndexerFetch = errors.New("indexer fetch failed")

func (c *Client) addByFetchedFile(ctx context.Context, downloadURL, category string) (string, error) {
	// 1. Fetch the .torrent file (or in some cases a redirect to a
	// magnet URI; we handle both). Retries on a transient stall — the
	// Prowlarr /download proxy fronts the real tracker, and a cold
	// Cloudflare challenge / slow indexer often succeeds on a second try.
	body, err := c.fetchTorrentBytes(ctx, downloadURL)
	if err != nil {
		return "", fmt.Errorf("%w (%w)", err, ErrIndexerFetch)
	}

	// Some indexers reply with a magnet URI in the body rather than a
	// .torrent file. qBit can take those via the `urls` field directly.
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte("magnet:")) {
		return "", c.addByURLs(ctx, strings.TrimSpace(string(body)), category)
	}

	// Guard: trackers behind a download cap / lapsed session reply 200
	// with an HTML error or login page instead of a .torrent. Uploading
	// that to qBit just yields an opaque "could not parse" — detect it
	// here and fail with an actionable reason instead.
	if !looksLikeTorrent(body) {
		// Classified TRANSIENT deliberately: a download cap or lapsed
		// session is per-indexer and time-bound (caps reset daily), and
		// with the deferred-retry flow "transient" means "a retry can
		// help": the deferral's indexer failover switches the grab to
		// another indexer's copy of the scene at the single-shot slot,
		// which is exactly the remedy for a capped tracker. The in-process
		// seconds-scale retry does NOT re-fire on this (its trigger is
		// 429/503 strings), so the cap isn't hammered within the attempt.
		return "", fmt.Errorf("indexer returned a non-torrent response (HTML/error page): likely the tracker's download cap or an expired session, not a bad torrent (%w) (%w)", ErrIndexerFetch, clienterr.ErrTransient)
	}

	// The info-hash (a) lets the grab link to its torrent without the
	// poller's recent-additions guess, and (b) turns a "duplicate"
	// refusal into a recoverable success when the torrent's already in
	// qBit. Best-effort — a parse miss just means we fall back to the
	// poller's linking.
	hash, _ := torrentmeta.InfoHash(body)

	// 2. Upload the .torrent bytes as a file.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if category != "" {
		_ = mw.WriteField("category", category)
	}
	_ = mw.WriteField("paused", "false")
	fileWriter, err := mw.CreateFormFile("torrents", "download.torrent")
	if err != nil {
		return "", fmt.Errorf("multipart file: %w", err)
	}
	if _, err := fileWriter.Write(body); err != nil {
		return "", fmt.Errorf("write torrent body: %w", err)
	}
	_ = mw.Close()
	if err := c.postAdd(ctx, &buf, mw.FormDataContentType()); err != nil {
		// qBit refuses a *duplicate* info-hash with "Fails." — but the
		// torrent is already present, so that's a recoverable success
		// (the grab links to the existing download), not a failure.
		if hash != "" {
			if recovered, rerr := c.recoverDuplicateAdd(ctx, hash, category, func() error {
				return c.postAdd(ctx, &buf, mw.FormDataContentType())
			}); recovered {
				return hash, rerr
			}
		}
		// We confirmed the bytes are a bencoded .torrent above, so this
		// isn't "couldn't parse" — qBit/libtorrent declined an otherwise-
		// valid file (often a private-tracker encoding quirk, e.g.
		// PornoLab's cp1251 names). Say so, and point at the fix.
		return hash, fmt.Errorf("qbit declined this torrent — it's a valid .torrent but qbit/libtorrent wouldn't add it (often a tracker encoding quirk); try another release")
	}
	return hash, nil
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
	// Hashes restricts the response to these info-hashes ("|"-separated
	// on the wire; supported since qBit API v2.0.1). Single-torrent
	// lookups pass one hash instead of downloading the whole list.
	Hashes []string
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
	if len(opts.Hashes) > 0 {
		q.Set("hashes", strings.Join(opts.Hashes, "|"))
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
		return nil, clienterr.Status("qbit list", resp.StatusCode, body)
	}
	var out []Torrent
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode list: %w", err)
	}
	return out, nil
}

// Category is a qBit category: its name and the save path downloads under
// it land in.
type Category struct {
	Name     string `json:"name"`
	SavePath string `json:"savePath"`
}

// Categories returns the client's configured categories keyed by name.
// Read-only; used by the setup-check to verify the forage category exists
// and points where forage expects.
func (c *Client) Categories(ctx context.Context) (map[string]Category, error) {
	resp, err := c.authedDo(ctx, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, "GET", c.baseURL+"/api/v2/torrents/categories", nil)
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, clienterr.Status("qbit categories", resp.StatusCode, body)
	}
	var out map[string]Category
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode categories: %w", err)
	}
	return out, nil
}

// TorrentInfo returns a single torrent by hash, or clienterr.ErrNotFound if
// qBit doesn't know about it (deleted, never added, race) so callers can tell
// "definitely gone" from a transient list-fetch failure with errors.Is. Uses
// the hashes= filter so qBit returns just that torrent — this used to download
// and decode the entire torrent list (hundreds of entries on a long-seeding
// box) to find one row. The EqualFold scan stays as cheap insurance against
// hash-casing mismatches (qBit reports lowercase hashes).
func (c *Client) TorrentInfo(ctx context.Context, hash string) (*Torrent, error) {
	if hash == "" {
		return nil, clienterr.ErrNotFound
	}
	ts, err := c.ListTorrents(ctx, ListOpts{Filter: "all", Hashes: []string{strings.ToLower(hash)}})
	if err != nil {
		return nil, err
	}
	for i := range ts {
		if strings.EqualFold(ts[i].Hash, hash) {
			return &ts[i], nil
		}
	}
	return nil, clienterr.ErrNotFound
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
		return nil, clienterr.Status("qbit files", resp.StatusCode, body)
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
		return clienterr.Status("qbit delete", resp.StatusCode, body)
	}
	return nil
}

// postForm sends a one-shot form POST to a torrents endpoint and checks
// for 200. Shared by the small mutation calls (setCategory, start).
func (c *Client) postForm(ctx context.Context, path string, form url.Values) error {
	enc := form.Encode()
	resp, err := c.authedDo(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+path, strings.NewReader(enc))
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
		return clienterr.Status("qbit "+path, resp.StatusCode, body)
	}
	return nil
}

// EnsureCategory creates the category with the given save path, or points an
// existing one at that path. Idempotent: safe to call on every save.
//
// This is the one cross-app step setup used to push onto the user ("go into
// qBittorrent, make a category, set its save path, come back and type the
// name"). forage already knows both values, so it does it instead.
//
// qBit answers 409 when the category already exists, which is not an error
// here — the point is that it exists and points at savePath, so fall through
// to editCategory rather than failing.
func (c *Client) EnsureCategory(ctx context.Context, name, savePath string) error {
	if name == "" {
		return fmt.Errorf("category name is empty")
	}
	form := url.Values{}
	form.Set("category", name)
	form.Set("savePath", savePath)
	createErr := c.postForm(ctx, "/api/v2/torrents/createCategory", form)
	if createErr == nil {
		return nil
	}
	// createCategory fails when the category already exists (409). Rather than
	// matching on the status — which would mean parsing it back out of the
	// error string — just try editCategory, which is the right call in that
	// case anyway: an existing category pointing somewhere ELSE is the subtler
	// version of the same misconfiguration, and leaving it is how downloads
	// end up outside the hardlink filesystem.
	//
	// If both fail the cause is something else entirely (unreachable, bad
	// credentials), so report the original create error, which is the more
	// descriptive of the two.
	if editErr := c.postForm(ctx, "/api/v2/torrents/editCategory", form); editErr == nil {
		return nil
	}
	return createErr
}

// SetCategory moves a torrent into the given category.
func (c *Client) SetCategory(ctx context.Context, hash, category string) error {
	if hash == "" {
		return fmt.Errorf("hash is empty")
	}
	form := url.Values{}
	form.Set("hashes", hash)
	form.Set("category", category)
	return c.postForm(ctx, "/api/v2/torrents/setCategory", form)
}

// Resume starts a stopped/paused torrent. qBit v5 renamed the endpoint
// to /torrents/start; older versions use /torrents/resume — try the new
// name first and fall back.
func (c *Client) Resume(ctx context.Context, hash string) error {
	if hash == "" {
		return fmt.Errorf("hash is empty")
	}
	form := url.Values{}
	form.Set("hashes", hash)
	if err := c.postForm(ctx, "/api/v2/torrents/start", form); err == nil {
		return nil
	}
	return c.postForm(ctx, "/api/v2/torrents/resume", form)
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
		return clienterr.Status("qbit add", resp.StatusCode, respBody)
	}
	if strings.TrimSpace(string(respBody)) == "Fails." {
		return fmt.Errorf("qbit refused the torrent (could not parse) (%w)", clienterr.ErrRejected)
	}
	return nil
}
