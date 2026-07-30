package api

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ordureconnoisseur/forager/internal/stash"
	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

// Secondary stash-boxes (FansDB, JavStash, ...) are a BROWSE surface only.
// StashDB remains the single source of identity: performer_cache, the scene
// cache, missing-scenes, owned/missing counts and the matcher are all keyed on
// StashDB cross-ids and are untouched by any of this.
//
// That is what keeps this cheap. A scene id is only unique within the box that
// issued it, so making the caches multi-box would mean an `endpoint` on the
// key of twelve tables and every query that reads them. Browsing needs none of
// it: a secondary box is queried live and nothing about it is persisted.
//
// The box list comes from Stash rather than forage's own config. Stash already
// holds each endpoint AND its key, the user maintains that list, and asking
// them to type the same four keys into a second app would only create a second
// list to keep in sync.

// boxCacheTTL bounds how stale the list may be. The user edits stash-boxes in
// Stash's settings roughly never, and a wrong entry costs a browse tab rather
// than anything durable.
const boxCacheTTL = 5 * time.Minute

// boxProbeTimeout caps the reachability check. ThePornDB answers 403 to every
// auth scheme on the reference instance, so "configured in Stash" is not the
// same as "usable" and the switcher must not offer a tab that cannot load.
const boxProbeTimeout = 12 * time.Second

// discoverBox is one selectable source on the Discover page.
type discoverBox struct {
	Endpoint string `json:"endpoint"`
	Name     string `json:"name"`
	// Primary marks StashDB: the cached, fully-featured feed. Everything
	// else is live-queried and browse-only.
	Primary bool `json:"primary"`
	// Unreachable carries WHY a configured box is not offered, so the user
	// can tell "I have not set that up" from "forage is ignoring it".
	Unreachable string `json:"unreachable,omitempty"`
}

type boxEntry struct {
	box    discoverBox
	client *stashdb.Client
}

type boxRegistry struct {
	mu      sync.Mutex
	at      time.Time
	entries []boxEntry
}

func isStashDBEndpoint(endpoint string) bool {
	return strings.Contains(strings.ToLower(endpoint), stash.StashDBEndpointHost)
}

// stashBoxes returns the user's stash-boxes with a client for each, refreshing
// from Stash at most every boxCacheTTL.
//
// Clients are cached rather than built per request because each carries its own
// request budget (see stashdb.pacer). A fresh client per page view would start
// with a full budget every time, which is the same as having no budget.
func (s *Server) stashBoxes(ctx context.Context) []boxEntry {
	s.boxes.mu.Lock()
	defer s.boxes.mu.Unlock()
	if time.Since(s.boxes.at) < boxCacheTTL && s.boxes.entries != nil {
		return s.boxes.entries
	}
	sc := s.pool.Stash()
	if sc == nil {
		return s.boxes.entries
	}
	configs, err := sc.StashBoxConfigs(ctx)
	if err != nil {
		// Keep whatever we had. A momentary Stash outage should not empty
		// the switcher out from under someone mid-browse.
		s.log.Warn("stash-boxes: read config", "err", err)
		return s.boxes.entries
	}

	prev := map[string]boxEntry{}
	for _, e := range s.boxes.entries {
		prev[e.box.Endpoint] = e
	}

	out := make([]boxEntry, 0, len(configs))
	for _, c := range configs {
		if c.Endpoint == "" {
			continue
		}
		name := c.Name
		if name == "" {
			name = hostOf(c.Endpoint)
		}
		e := boxEntry{box: discoverBox{
			Endpoint: c.Endpoint,
			Name:     name,
			Primary:  isStashDBEndpoint(c.Endpoint),
		}}
		if e.box.Primary {
			// The primary is served from the cache by the existing path and
			// uses the daemon's own StashDB credentials, not Stash's.
			out = append(out, e)
			continue
		}
		if c.APIKey == "" {
			e.box.Unreachable = "no API key set for this stash-box in Stash"
			out = append(out, e)
			continue
		}
		// Reuse a client that already probed clean, so browsing does not
		// re-probe every five minutes.
		if p, ok := prev[c.Endpoint]; ok && p.client != nil && p.box.Unreachable == "" {
			e.client = p.client
			out = append(out, e)
			continue
		}
		client := stashdb.New(strings.TrimSuffix(c.Endpoint, "/graphql"), c.APIKey)
		pctx, cancel := context.WithTimeout(ctx, boxProbeTimeout)
		// Probe with the query the feed actually runs, not an auth ping.
		// ThePornDB answers `me` with the right username and then returns
		// {"count": 2, "scenes": null} for queryScenes: its stash-box surface
		// reports totals but never returns scene bodies. An auth probe calls
		// that healthy and the switcher offers a tab that renders nothing.
		res, perr := client.QueryScenes(pctx, stashdb.SceneQuery{
			Page: 1, PerPage: 1, Sort: "DATE",
		})
		cancel()
		switch {
		case perr != nil:
			e.box.Unreachable = shortErr(perr)
		case res == nil || len(res.Scenes) == 0:
			e.box.Unreachable = "answers, but returns no scenes to browse"
		default:
			e.client = client
		}
		out = append(out, e)
	}
	// Primary first, then by name, so the switcher's order is stable.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].box.Primary != out[j].box.Primary {
			return out[i].box.Primary
		}
		return strings.ToLower(out[i].box.Name) < strings.ToLower(out[j].box.Name)
	})
	s.boxes.entries = out
	s.boxes.at = time.Now()
	return out
}

// boxByEndpoint finds a usable secondary box. Returns nil for the primary or
// for anything unreachable, so callers fall through to the cached StashDB feed
// rather than rendering an empty page.
func (s *Server) boxByEndpoint(ctx context.Context, endpoint string) *boxEntry {
	if endpoint == "" {
		return nil
	}
	for _, e := range s.stashBoxes(ctx) {
		if e.box.Endpoint == endpoint && !e.box.Primary && e.client != nil {
			return &e
		}
	}
	return nil
}

func hostOf(endpoint string) string {
	h := strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	if i := strings.IndexByte(h, '/'); i >= 0 {
		h = h[:i]
	}
	return h
}

func shortErr(err error) string {
	m := err.Error()
	if len(m) > 120 {
		m = m[:120] + "…"
	}
	return m
}

// getDiscoverBoxes lists the sources the Discover switcher can offer,
// unreachable ones included so the UI can say why.
func (s *Server) getDiscoverBoxes(w http.ResponseWriter, r *http.Request) {
	entries := s.stashBoxes(r.Context())
	out := make([]discoverBox, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.box)
	}
	writeJSON(w, http.StatusOK, map[string]any{"boxes": out})
}

// stashDBFor returns the client for the box that issued a stored scene id.
//
// source is the endpoint recorded on a watch or grab; "" means StashDB, which
// is every row created before secondary boxes existed and every row created
// from the primary feed. An unknown or now-unreachable source falls back to
// StashDB rather than returning nil: the callers are enrichment paths that
// treat a miss as "no extra detail", and a nil client would turn a cosmetic
// gap into a skipped code path.
//
// This exists because a scene id is only meaningful on the box that issued it.
// Asking StashDB about a FansDB uuid does not fail loudly; it returns nothing,
// which reads exactly like a scene StashDB has not indexed yet.
func (s *Server) stashDBFor(ctx context.Context, source string) *stashdb.Client {
	if source == "" || isStashDBEndpoint(source) {
		return s.pool.StashDB()
	}
	if e := s.boxByEndpoint(ctx, source); e != nil {
		return e.client
	}
	return s.pool.StashDB()
}
