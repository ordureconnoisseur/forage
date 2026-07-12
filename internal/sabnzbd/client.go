// Package sabnzbd is a thin client for SABnzbd's HTTP API.
//
// Mirrors internal/qbit + internal/prowlarr — raw net/http, no
// third-party deps. Auth is a single `apikey` query param; everything
// else dispatches off `mode=` on /api.
package sabnzbd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ordureconnoisseur/forager/internal/clienterr"
)

type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
	// httpAdd is used only by AddURL. An addurl call makes SAB fetch the NZB
	// from the indexer (via Prowlarr) before it replies, which under indexer
	// slowness/rate-limiting routinely runs past the 30s status-call budget —
	// and the failure is misleading, because SAB usually completes the fetch
	// and queues the download anyway, it just answered too late. A longer
	// budget lets forage capture the nzo_id and track the grab instead of
	// false-failing it. Status polls stay on the tight 30s client.
	httpAdd *http.Client
}

func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 30 * time.Second},
		httpAdd: &http.Client{Timeout: 120 * time.Second},
	}
}

// Item is the slim shape forager's poller needs from SAB's queue or
// history. SAB has many more fields per slot; we project to what
// advances the grabs state machine.
type Item struct {
	NzoID      string
	Name       string
	Category   string
	Status     string  // "Queued" | "Downloading" | "Completed" | "Failed" | ...
	Percentage float64 // 0..100
	Path       string  // history: final on-disk path / storage location
	Completed  int64   // history: unix time the job finished (0 if absent)
	EtaSecs    int64   // queue: seconds remaining (parsed from timeleft)
	SpeedBps   int64   // queue: current download speed, bytes/s (queue-global)
}

// Version is the low-cost reachability + auth probe used at boot and
// by the probe tool.
func (c *Client) Version(ctx context.Context) (string, error) {
	body, err := c.get(ctx, url.Values{"mode": {"version"}})
	if err != nil {
		return "", err
	}
	var resp struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("decode version: %w (body=%s)", err, body)
	}
	return resp.Version, nil
}

// AddURL submits a single NZB URL via mode=addurl. SAB synchronously
// returns the nzo_id it assigns, which we store as the grab's
// client_id (analogous to qBit's info_hash but cleaner — no time-
// window matching needed).
//
// SAB's response shape:
//
//	{"status": true,  "nzo_ids": ["SABnzbd_nzo_xxx"]}    on success
//	{"status": false, "error":   "..."}                  on failure
func (c *Client) AddURL(ctx context.Context, nzbURL, category string) (string, error) {
	if nzbURL == "" {
		return "", fmt.Errorf("nzb URL is empty")
	}
	q := url.Values{
		"mode": {"addurl"},
		"name": {nzbURL},
	}
	if category != "" {
		q.Set("cat", category)
	}
	// Use the longer-timeout client — SAB fetches the NZB from the indexer
	// before replying, which routinely outlasts the 30s status budget.
	body, err := c.doGet(ctx, c.httpAdd, q)
	if err != nil {
		return "", err
	}
	var resp struct {
		Status bool     `json:"status"`
		NzoIDs []string `json:"nzo_ids"`
		ErrMsg string   `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("decode addurl: %w (body=%s)", err, body)
	}
	if !resp.Status {
		return "", fmt.Errorf("sab refused addurl: %s", resp.ErrMsg)
	}
	if len(resp.NzoIDs) == 0 {
		return "", fmt.Errorf("sab returned no nzo_id (body=%s)", body)
	}
	return resp.NzoIDs[0], nil
}

// DeleteHistory removes a completed/failed item from SAB's history.
// When delFiles is true SAB also deletes the downloaded files from
// its complete storage. forage uses this after placing a usenet grab
// into the library — usenet doesn't seed, so the SAB copy is dead
// weight once it's been hardlinked/copied across. Safe with
// delFiles: placement already linked the data into the library, so
// removing the SAB-side files leaves the library copy intact.
func (c *Client) DeleteHistory(ctx context.Context, nzoID string, delFiles bool) error {
	if nzoID == "" {
		return fmt.Errorf("nzo_id is empty")
	}
	q := url.Values{
		"mode":  {"history"},
		"name":  {"delete"},
		"value": {nzoID},
	}
	if delFiles {
		q.Set("del_files", "1")
	}
	body, err := c.get(ctx, q)
	if err != nil {
		return err
	}
	var resp struct {
		Status bool   `json:"status"`
		ErrMsg string `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("decode history delete: %w (body=%s)", err, body)
	}
	if !resp.Status {
		return fmt.Errorf("sab refused history delete: %s", resp.ErrMsg)
	}
	return nil
}

// Queue returns the currently-active downloads (queued, downloading,
// verifying). Used by the poller to detect "still in progress" state.
func (c *Client) Queue(ctx context.Context) ([]Item, error) {
	body, err := c.get(ctx, url.Values{"mode": {"queue"}})
	if err != nil {
		return nil, err
	}
	// SAB queue response: {"queue": {"kbpersec": "...", "slots": [...]}}.
	// kbpersec is queue-global; we attribute it to the active slot
	// (SAB downloads one job at a time), which is good enough for a
	// per-grab speed readout. timeleft is per-slot "HH:MM:SS".
	var resp struct {
		Queue struct {
			KBPerSec string `json:"kbpersec"`
			Slots    []struct {
				NzoID      string `json:"nzo_id"`
				Filename   string `json:"filename"`
				Category   string `json:"cat"`
				Status     string `json:"status"`
				Percentage string `json:"percentage"`
				TimeLeft   string `json:"timeleft"`
			} `json:"slots"`
		} `json:"queue"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode queue: %w (body=%s)", err, body)
	}
	kbps, _ := strconv.ParseFloat(resp.Queue.KBPerSec, 64)
	speedBps := int64(kbps * 1024)
	out := make([]Item, 0, len(resp.Queue.Slots))
	for _, s := range resp.Queue.Slots {
		pct, _ := strconv.ParseFloat(s.Percentage, 64)
		it := Item{
			NzoID:      s.NzoID,
			Name:       s.Filename,
			Category:   s.Category,
			Status:     s.Status,
			Percentage: pct,
			EtaSecs:    parseTimeLeft(s.TimeLeft),
		}
		if strings.EqualFold(s.Status, "Downloading") {
			it.SpeedBps = speedBps
		}
		out = append(out, it)
	}
	return out, nil
}

// parseTimeLeft converts SAB's remaining-time string to seconds: "MM:SS",
// "HH:MM:SS", or "D:HH:MM:SS" for jobs over 24h (SAB's calc_timeleft adds
// the day component). The day part is days, not a base-60 digit — folding
// every component as base-60 counted each day as 60 hours and inflated
// multi-day ETAs 2.5x per day. Returns 0 on anything unparseable.
func parseTimeLeft(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "0:00:00" {
		return 0
	}
	parts := strings.Split(s, ":")
	if len(parts) > 4 {
		return 0
	}
	var days int64
	if len(parts) == 4 {
		var err error
		if days, err = strconv.ParseInt(parts[0], 10, 64); err != nil {
			return 0
		}
		parts = parts[1:]
	}
	var rest int64
	for _, p := range parts {
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return 0
		}
		rest = rest*60 + n
	}
	return days*86400 + rest
}

// History returns the most-recently-completed items, newest first. Used by
// the poller to detect "completed" state and recover the final on-disk
// filename for Stash matching. When category is non-empty SAB scopes the
// result to it — important for the poller's fixed-size window: otherwise a
// burst of OTHER-category jobs on a busy SAB can push a forage job out of the
// limit before the poller sees it complete, and the not-found branch then
// falsely fails the (actually done) grab.
func (c *Client) History(ctx context.Context, limit int, category string) ([]Item, error) {
	if limit <= 0 {
		limit = 50
	}
	v := url.Values{
		"mode":  {"history"},
		"limit": {strconv.Itoa(limit)},
	}
	if category != "" {
		v.Set("category", category)
	}
	body, err := c.get(ctx, v)
	if err != nil {
		return nil, err
	}
	var resp struct {
		History struct {
			Slots []struct {
				NzoID     string `json:"nzo_id"`
				Name      string `json:"name"`
				Category  string `json:"category"`
				Status    string `json:"status"`
				Storage   string `json:"storage"`
				Completed int64  `json:"completed"`
			} `json:"slots"`
		} `json:"history"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode history: %w (body=%s)", err, body)
	}
	out := make([]Item, 0, len(resp.History.Slots))
	for _, s := range resp.History.Slots {
		out = append(out, Item{
			NzoID:     s.NzoID,
			Name:      s.Name,
			Category:  s.Category,
			Status:    s.Status,
			Path:      s.Storage,
			Completed: s.Completed,
		})
	}
	return out, nil
}

// get is the shared GET path. SAB demands `apikey` + `output=json`
// on every request; everything else is mode-specific.
// Category is a SAB category: its name and the dir downloads under it land
// in (SAB's "dir" — may be relative to the global complete dir).
type Category struct {
	Name string
	Dir  string
}

// Categories returns SAB's configured categories (name + dir). Read-only;
// used by the setup-check to verify the forage category exists and its dir
// is where forage expects. The "*" default category is skipped.
func (c *Client) Categories(ctx context.Context) ([]Category, error) {
	q := url.Values{}
	q.Set("mode", "get_config")
	q.Set("section", "categories")
	body, err := c.get(ctx, q)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Config struct {
			Categories []struct {
				Name string `json:"name"`
				Dir  string `json:"dir"`
			} `json:"categories"`
		} `json:"config"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode categories: %w", err)
	}
	out := make([]Category, 0, len(resp.Config.Categories))
	for _, c := range resp.Config.Categories {
		if c.Name == "" || c.Name == "*" {
			continue
		}
		out = append(out, Category{Name: c.Name, Dir: c.Dir})
	}
	return out, nil
}

func (c *Client) get(ctx context.Context, q url.Values) ([]byte, error) {
	return c.doGet(ctx, c.http, q)
}

func (c *Client) doGet(ctx context.Context, hc *http.Client, q url.Values) ([]byte, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("sab base URL not configured")
	}
	q.Set("apikey", c.apiKey)
	q.Set("output", "json")
	u := c.baseURL + "/api?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, c.redactKey(err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, clienterr.Transport("sab "+q.Get("mode"), c.redactKey(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, clienterr.Transport("sab "+q.Get("mode")+" read", c.redactKey(err))
	}
	if resp.StatusCode != 200 {
		return nil, clienterr.Status("sab "+q.Get("mode"), resp.StatusCode, body)
	}
	// SAB reports API-level failures (bad/limited API key, disabled API) as
	// HTTP 200 with an error envelope. Without this check those bodies decode
	// as an empty queue/history and the poller reads absence as "SAB lost the
	// download", falsely failing every active grab in one tick.
	var env struct {
		Status *bool  `json:"status"`
		ErrMsg string `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil && env.Status != nil && !*env.Status {
		msg := env.ErrMsg
		if msg == "" {
			msg = "unspecified error"
		}
		return nil, fmt.Errorf("sab %s refused: %s (%w)", q.Get("mode"), msg, clienterr.ErrRejected)
	}
	return body, nil
}

// redactKey scrubs the API key from an error message. Request/transport
// errors (url.Error) embed the full request URL including the apikey query
// param, and those strings get persisted as grab failure reasons and logged.
// The original error stays in the chain so errors.Is classification holds.
func (c *Client) redactKey(err error) error {
	if err == nil || c.apiKey == "" {
		return err
	}
	msg := strings.ReplaceAll(err.Error(), c.apiKey, "REDACTED")
	if escaped := url.QueryEscape(c.apiKey); escaped != c.apiKey {
		msg = strings.ReplaceAll(msg, escaped, "REDACTED")
	}
	if msg == err.Error() {
		return err
	}
	return &redactedError{msg: msg, err: err}
}

type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }
