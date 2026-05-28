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
)

type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 30 * time.Second},
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
	body, err := c.get(ctx, q)
	if err != nil {
		return "", err
	}
	var resp struct {
		Status  bool     `json:"status"`
		NzoIDs  []string `json:"nzo_ids"`
		ErrMsg  string   `json:"error"`
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
	// SAB queue response: {"queue": {"slots": [...]}}.
	var resp struct {
		Queue struct {
			Slots []struct {
				NzoID      string `json:"nzo_id"`
				Filename   string `json:"filename"`
				Category   string `json:"cat"`
				Status     string `json:"status"`
				Percentage string `json:"percentage"`
			} `json:"slots"`
		} `json:"queue"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode queue: %w (body=%s)", err, body)
	}
	out := make([]Item, 0, len(resp.Queue.Slots))
	for _, s := range resp.Queue.Slots {
		pct, _ := strconv.ParseFloat(s.Percentage, 64)
		out = append(out, Item{
			NzoID:      s.NzoID,
			Name:       s.Filename,
			Category:   s.Category,
			Status:     s.Status,
			Percentage: pct,
		})
	}
	return out, nil
}

// History returns the most-recently-completed items, newest first.
// Used by the poller to detect "completed" state and recover the
// final on-disk filename for Stash matching.
func (c *Client) History(ctx context.Context, limit int) ([]Item, error) {
	if limit <= 0 {
		limit = 50
	}
	body, err := c.get(ctx, url.Values{
		"mode":  {"history"},
		"limit": {strconv.Itoa(limit)},
	})
	if err != nil {
		return nil, err
	}
	var resp struct {
		History struct {
			Slots []struct {
				NzoID    string `json:"nzo_id"`
				Name     string `json:"name"`
				Category string `json:"category"`
				Status   string `json:"status"`
				Storage  string `json:"storage"`
			} `json:"slots"`
		} `json:"history"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode history: %w (body=%s)", err, body)
	}
	out := make([]Item, 0, len(resp.History.Slots))
	for _, s := range resp.History.Slots {
		out = append(out, Item{
			NzoID:    s.NzoID,
			Name:     s.Name,
			Category: s.Category,
			Status:   s.Status,
			Path:     s.Storage,
		})
	}
	return out, nil
}

// get is the shared GET path. SAB demands `apikey` + `output=json`
// on every request; everything else is mode-specific.
func (c *Client) get(ctx context.Context, q url.Values) ([]byte, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("sab base URL not configured")
	}
	q.Set("apikey", c.apiKey)
	q.Set("output", "json")
	u := c.baseURL + "/api?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("sab %s %d: %s", q.Get("mode"), resp.StatusCode, body)
	}
	return body, nil
}
