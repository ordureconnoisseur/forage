// Package prowlarr is a thin REST client for Prowlarr's /api/v1.
// Mirrors the conventions of internal/stash and internal/stashdb: raw
// net/http, hand-crafted requests, no codegen, no third-party deps.
package prowlarr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
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
	Title     string
	Indexer   string
	IndexerID int
	Protocol  string // "torrent" | "usenet"
	Size      int64
	Seeders   int // torrent only; 0 for usenet
	// SeedersKnown distinguishes a real zero from an indexer that simply
	// omits the seeders field (cookie/HTML-scraped trackers do): a
	// seeder floor must not erase such an indexer's whole catalog.
	SeedersKnown bool
	Grabs        int // present for both; the dominant signal for usenet
	Popularity   int
	PublishDate  string // RFC3339
	InfoURL      string
	DownloadURL  string
	// Magnet is a magnet: URI when the indexer is magnet-only (e.g. The
	// Pirate Bay, whose downloadUrl is empty). GrabURL falls back to it.
	Magnet     string
	Categories []int
	// Files is the indexer-reported file count. Often absent (PornoLab
	// returns null), and when present counts every file (videos + thumbs
	// + NFOs), so it's only a weak pack hint — the authoritative video
	// count comes from parsing the .torrent (see internal/torrentmeta).
	Files int
}

// GrabURL is what to hand the download client: the .torrent download URL
// when present, else the magnet. Empty when the indexer gave neither.
func (r Release) GrabURL() string {
	if r.DownloadURL != "" {
		return r.DownloadURL
	}
	return r.Magnet
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
	MagnetURL   string `json:"magnetUrl"`
	GUID        string `json:"guid"`
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
		out.SeedersKnown = true
	}
	if r.Files != nil {
		out.Files = *r.Files
	}
	// Magnet-only indexers (TPB) leave downloadUrl empty and put the
	// magnet in guid (raw magnet:) or magnetUrl. Capture the raw magnet
	// so GrabURL has something the download client can use directly.
	switch {
	case strings.HasPrefix(r.GUID, "magnet:"):
		out.Magnet = r.GUID
	case strings.HasPrefix(r.MagnetURL, "magnet:"):
		out.Magnet = r.MagnetURL
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
	return c.SearchScoped(ctx, term, categories, nil)
}

// SearchScoped is Search restricted to specific indexer ids — used to
// fan a search out per-indexer (each with its own timeout) so one slow
// indexer can't stall the whole query. Empty indexerIDs = all indexers.
//
// An empty term is intentionally sent as `query=` (not rejected): Prowlarr
// treats a query-less search as the indexers' recent-uploads feed, which
// RecentReleases relies on. Keep emitting the empty query param.
func (c *Client) SearchScoped(ctx context.Context, term string, categories, indexerIDs []int) ([]Release, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("prowlarr base URL not configured")
	}
	q := url.Values{}
	q.Set("query", term)
	for _, id := range categories {
		q.Add("categories", strconv.Itoa(id))
	}
	for _, id := range indexerIDs {
		q.Add("indexerIds", strconv.Itoa(id))
	}
	u := c.baseURL + "/api/v1/search?" + q.Encode()

	var out []Release
	err := transientRetry(ctx, func() error {
		rels, aerr := c.searchOnce(ctx, u)
		if aerr != nil {
			return aerr
		}
		out = rels
		return nil
	})
	return out, err
}

// RecentReleases returns each indexer's most-recent uploads — the RSS-style
// "what's new" feed — by issuing a QUERY-LESS search (empty term). Prowlarr
// answers an empty query with the indexers' recent items rather than a keyword
// search, so this is the cheap forward-looking complement to per-scene Search:
// one aggregate request whose results the caller matches locally against the
// watchlist. categories filters by Newznab type as usual (empty = all). Each
// Release carries PublishDate (RFC3339) and IndexerID so the caller can
// watermark per indexer. Verified behaviour: GET /api/v1/search?query=&categories=…
// returns recent releases across all indexers.
func (c *Client) RecentReleases(ctx context.Context, categories []int) ([]Release, error) {
	return c.SearchScoped(ctx, "", categories, nil)
}

// searchOnce performs one /api/v1/search request and decodes it. Errors are
// classified (clienterr) so transientRetry can tell a retryable blip from a
// definitive failure.
func (c *Client) searchOnce(ctx context.Context, u string) ([]Release, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, clienterr.Transport("prowlarr search", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, clienterr.Transport("prowlarr search read", err)
	}
	if resp.StatusCode != 200 {
		return nil, clienterr.Status("prowlarr search", resp.StatusCode, body)
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

// Retry tuning (vars, not consts, so tests can shrink them). Prowlarr's
// /search intermittently fails transiently under load — a 5xx, or a refused
// connection while it restarts — and a couple of retries turn that into a short
// delay instead of a hard failure that drops the whole query.
var (
	prowlarrSearchAttempts = 3 // initial + 2 retries
	// prowlarrRetryFastFail bounds WHICH transients we retry: only ones that
	// failed quickly. A transient that took most of the request timeout is a
	// persistently slow indexer — retrying it just doubles the caller's wait
	// without helping (and would penalize interactive search), so we surface it
	// as-is. A refused connection / fast 5xx fails in well under this.
	prowlarrRetryFastFail  = 8 * time.Second
	prowlarrRetryBaseDelay = 300 * time.Millisecond
	prowlarrRetryMaxDelay  = 5 * time.Second
)

// shouldRetry decides whether a failed attempt is worth another try: it must be
// a transient error (clienterr.ErrTransient) AND have failed fast (a slow
// failure is a slow indexer, not a blip). Pure + deterministic so it can be
// unit-tested without timing.
func shouldRetry(err error, elapsed time.Duration) bool {
	return errors.Is(err, clienterr.ErrTransient) && elapsed < prowlarrRetryFastFail
}

// transientRetry runs fn up to prowlarrSearchAttempts times, retrying only when
// shouldRetry approves, with exponential backoff + jitter, bounded by ctx. A
// success, a non-transient error (4xx/auth/decode), or a slow transient returns
// immediately.
func transientRetry(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; attempt < prowlarrSearchAttempts; attempt++ {
		start := time.Now()
		err = fn()
		if !shouldRetry(err, time.Since(start)) {
			return err
		}
		if attempt == prowlarrSearchAttempts-1 {
			break
		}
		select {
		case <-time.After(retryBackoff(attempt)):
		case <-ctx.Done():
			return err
		}
	}
	return err
}

// retryBackoff is prowlarrRetryBaseDelay * 2^attempt, capped at
// prowlarrRetryMaxDelay, with up to ~20% jitter so concurrent per-indexer
// retries don't resynchronize into a thundering herd.
func retryBackoff(attempt int) time.Duration {
	d := prowlarrRetryBaseDelay << attempt
	if d > prowlarrRetryMaxDelay {
		d = prowlarrRetryMaxDelay
	}
	return d - d/10 + time.Duration(rand.Int63n(int64(d)/5+1))
}

// Indexer is the slim shape of a configured Prowlarr indexer.
type Indexer struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Protocol string `json:"protocol"` // "torrent" | "usenet"
	Enable   bool   `json:"enable"`
}

// Indexers lists the configured indexers (GET /api/v1/indexer), so
// callers can fan a search out per-indexer with independent timeouts.
func (c *Client) Indexers(ctx context.Context) ([]Indexer, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("prowlarr base URL not configured")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/api/v1/indexer", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, clienterr.Transport("prowlarr indexer list", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, clienterr.Status("prowlarr indexer list", resp.StatusCode, body)
	}
	var out []Indexer
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode indexer list: %w", err)
	}
	return out, nil
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
		return "", clienterr.Transport("prowlarr status", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", clienterr.Status("prowlarr status", resp.StatusCode, body)
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
