// Package prowlarr is a thin REST client for Prowlarr's /api/v1.
// Mirrors the conventions of internal/stash and internal/stashdb: raw
// net/http, hand-crafted requests, no codegen, no third-party deps.
package prowlarr

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
		http:    &http.Client{Timeout: 60 * time.Second},
	}
}

// Release is the slim shape forager cares about. Prowlarr returns
// many more fields per release; we project to what the matcher and UI
// will actually use.
//
// Popularity is "seeders" for torrents and "grabs" for usenet so callers
// can sort across protocols with one comparable signal.
type Release struct {
	Title       string
	Indexer     string
	IndexerID   int
	Protocol    string // "torrent" | "usenet"
	Size        int64
	Seeders     int // torrent only; 0 for usenet
	Grabs       int // present for both; the dominant signal for usenet
	Popularity  int
	PublishDate string // RFC3339
	InfoURL     string
	DownloadURL string
	Categories  []int
	// Files is the indexer-reported file count. Often absent (PornoLab
	// returns null), and when present counts every file (videos + thumbs
	// + NFOs), so it's only a weak pack hint — the authoritative video
	// count comes from parsing the .torrent (see internal/torrentmeta).
	Files int
}

// rawRelease is the wire shape — Prowlarr's response objects include
// many fields we don't need. seeders is omitted on usenet results, so
// it lives in a *int.
type rawRelease struct {
	Title       string `json:"title"`
	Indexer     string `json:"indexer"`
	IndexerID   int    `json:"indexerId"`
	Protocol    string `json:"protocol"`
	Size        int64  `json:"size"`
	Seeders     *int   `json:"seeders"`
	Grabs       int    `json:"grabs"`
	PublishDate string `json:"publishDate"`
	InfoURL     string `json:"infoUrl"`
	DownloadURL string `json:"downloadUrl"`
	Files       *int   `json:"files"`
	Categories  []struct {
		ID int `json:"id"`
	} `json:"categories"`
}

func (r rawRelease) toRelease() Release {
	out := Release{
		Title:       r.Title,
		Indexer:     r.Indexer,
		IndexerID:   r.IndexerID,
		Protocol:    r.Protocol,
		Size:        r.Size,
		Grabs:       r.Grabs,
		PublishDate: r.PublishDate,
		InfoURL:     r.InfoURL,
		DownloadURL: r.DownloadURL,
	}
	if r.Seeders != nil {
		out.Seeders = *r.Seeders
	}
	if r.Files != nil {
		out.Files = *r.Files
	}
	for _, c := range r.Categories {
		out.Categories = append(out.Categories, c.ID)
	}
	// Unified popularity: seeders for torrents, grabs for usenet. Take
	// the larger if both are present (defensive).
	out.Popularity = out.Seeders
	if r.Protocol == "usenet" || r.Grabs > out.Popularity {
		out.Popularity = r.Grabs
	}
	return out
}

// Search calls GET /api/v1/search?query=<term>&categories=<csv>.
// Returns releases ordered by Popularity descending. categories is the
// Newznab-style numeric list (e.g. 6000 = XXX, 6010 = XXX/DVD, ...);
// when empty no category filter is applied.
func (c *Client) Search(ctx context.Context, term string, categories []int) ([]Release, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("prowlarr base URL not configured")
	}
	q := url.Values{}
	q.Set("query", term)
	for _, id := range categories {
		q.Add("categories", strconv.Itoa(id))
	}
	u := c.baseURL + "/api/v1/search?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", c.apiKey)

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
		return nil, fmt.Errorf("prowlarr search %d: %s", resp.StatusCode, body)
	}

	var raw []rawRelease
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode search response: %w", err)
	}

	out := make([]Release, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.toRelease())
	}
	sortByPopularity(out)
	return out, nil
}

// maxTorrentBytes caps a .torrent download. Real .torrents are KBs to
// a few MB even for huge multi-file packs; anything larger is almost
// certainly not a torrent file (a misrouted HTML error page, etc.).
const maxTorrentBytes = 32 << 20

// FetchTorrent downloads the raw .torrent bytes for a release's
// DownloadURL so the caller can parse its file list (pack detection).
//
// Prowlarr hands back proxied download URLs of the form
// "<prowlarr-base>/<n>/download?apikey=...&link=...". The base in that
// URL reflects whatever host Prowlarr saw on the request, which may not
// be reachable from forager's container. So for proxied URLs we keep
// the path+query but swap in the base URL forager is already configured
// to talk to. Non-proxied / direct URLs are fetched as-is. Magnet links
// (no .torrent to fetch) return an error — the caller falls back to
// size/keyword heuristics.
func (c *Client) FetchTorrent(ctx context.Context, downloadURL string) ([]byte, error) {
	if downloadURL == "" {
		return nil, fmt.Errorf("no download url")
	}
	if strings.HasPrefix(downloadURL, "magnet:") {
		return nil, fmt.Errorf("magnet link has no .torrent to fetch")
	}
	u, err := url.Parse(downloadURL)
	if err != nil {
		return nil, fmt.Errorf("parse download url: %w", err)
	}
	if strings.Contains(u.Path, "/download") && c.baseURL != "" {
		if base, err := url.Parse(c.baseURL); err == nil {
			u.Scheme = base.Scheme
			u.Host = base.Host
		}
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTorrentBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fetch torrent %d", resp.StatusCode)
	}
	return body, nil
}

// Status hits /api/v1/system/status as a lightweight reachability +
// auth probe. Returns Prowlarr's version string on success.
func (c *Client) Status(ctx context.Context) (string, error) {
	if c.baseURL == "" {
		return "", fmt.Errorf("prowlarr base URL not configured")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/api/v1/system/status", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("prowlarr status %d: %s", resp.StatusCode, body)
	}
	var s struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return "", err
	}
	return s.Version, nil
}

func sortByPopularity(rs []Release) {
	// stdlib sort.Slice avoided to keep zero deps; in-place insertion
	// sort is plenty fast for the ~50–200 results Prowlarr returns.
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && rs[j].Popularity > rs[j-1].Popularity; j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
}
