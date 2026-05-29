package api

import (
	"context"
	"database/sql"
	"net/http"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ordureconnoisseur/forager/internal/prowlarr"
	"github.com/ordureconnoisseur/forager/internal/torrentmeta"
)

// Pack detection tunables. Hardcoded for now; Phase 4 promotes the
// thresholds to the config store + settings UI.
const (
	// packMinVideos: a release is a "pack" at or above this many video
	// files. 3 avoids mislabelling a single scene that ships with a
	// trailer/sample as a pack.
	packMinVideos = 3
	// packParseSizeMin: fetch+parse a torrent's metadata when it's at
	// least this large (OR it has a pack keyword / the XXX/Pack
	// category). Keeps us from pulling .torrents for the hundreds of
	// sub-GB single-scene results a performer search returns.
	packParseSizeMin = int64(2) << 30
	// packParseMax caps how many candidate .torrents we fetch per
	// search; packParseWorkers bounds the concurrency; packFetchTimeout
	// caps each individual fetch so one slow tracker can't stall the
	// whole confirm phase.
	packParseMax     = 16
	packParseWorkers = 10
	packFetchTimeout = 15 * time.Second
	// packIndexerTimeout caps each per-indexer search in the fan-out, so
	// one slow indexer (e.g. a FlareSolverr-backed tracker mid-Cloudflare
	// -challenge) gets dropped instead of stalling the whole pack search.
	packIndexerTimeout = 12 * time.Second
	// xxxPackCategory is Newznab's XXX/Pack — PornoLab tags collection
	// torrents with it, a strong pre-fetch pack signal.
	xxxPackCategory = 6050
	// xxxParentCategory is the Newznab XXX parent; Prowlarr expands it
	// to every XXX subcategory on search.
	xxxParentCategory = 6000
)

// packKeywordRe matches pack-ish title cues across the languages
// PornoLab/1337x/sukebei use (incl. Russian ролик/клип = clip).
var packKeywordRe = regexp.MustCompile(`(?i)(mega\s*pack|\bpacks?\b|site\s*rip|siterip|collection|compilation|\d{2,}\s*(?:videos?|clips?|scenes?|ролик|клип))`)

type packCandidate struct {
	Title       string `json:"title"`
	Indexer     string `json:"indexer"`
	Protocol    string `json:"protocol"`
	Size        int64  `json:"size"`
	VideoCount  int    `json:"video_count"` // authoritative when parsed; 0 when unknown
	FileCount   int    `json:"file_count"`
	Estimated   bool   `json:"estimated"` // true when we couldn't parse (magnet/usenet) so video_count is a guess
	Seeders     int    `json:"seeders"`
	Grabs       int    `json:"grabs"`
	Popularity  int    `json:"popularity"`
	PublishDate string `json:"publish_date"`
	InfoURL     string `json:"info_url"`
	DownloadURL string `json:"download_url"`
}

type packsResponse struct {
	Performer struct {
		StashID string `json:"stash_id"`
		Name    string `json:"name"`
	} `json:"performer"`
	Packs []packCandidate `json:"packs"`
}

// getPerformerPacks finds multi-scene "pack" releases for a performer.
//
//	GET /performers/{id}/packs
//
// Flow:
//  1. Resolve the performer's name from performer_cache.
//  2. Prowlarr search on that name (configured XXX categories + the
//     XXX/Pack category).
//  3. Select likely-pack candidates by cheap signals (size, XXX/Pack
//     category, title keyword), then confirm by fetching + parsing each
//     candidate's .torrent for an authoritative video count.
//  4. Return releases with >= packMinVideos videos, popularity-desc.
func (s *Server) getPerformerPacks(w http.ResponseWriter, r *http.Request) {
	prowlarrC := s.pool.Prowlarr()
	if prowlarrC == nil {
		writeErr(w, http.StatusServiceUnavailable, "prowlarr must be configured (see Settings)")
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "performer id required")
		return
	}
	name, err := performerName(r.Context(), s.db, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "performer not found")
		return
	}

	releases := s.searchPackIndexers(r.Context(), prowlarrC, name)

	packs := s.classifyPacks(r.Context(), prowlarrC, releases)
	// Biggest first — performer megapacks are what the user is after; a
	// huge pack with few seeders shouldn't sink below a small one.
	sort.SliceStable(packs, func(i, j int) bool { return packs[i].Size > packs[j].Size })

	resp := packsResponse{Packs: packs}
	resp.Performer.StashID = id
	resp.Performer.Name = name
	writeJSON(w, http.StatusOK, resp)
}

// classifyPacks selects pack candidates from a release set and confirms
// torrents by parsing their .torrent metadata (bounded concurrency).
func (s *Server) classifyPacks(ctx context.Context, pc *prowlarr.Client, releases []prowlarr.Release) []packCandidate {
	type cand struct {
		rel     prowlarr.Release
		parse   bool // torrent we'll fetch+parse for an authoritative count
		keyword bool
		packCat bool
	}
	var cands []cand
	for _, rel := range releases {
		kw := packKeywordRe.MatchString(rel.Title)
		pcat := containsInt(rel.Categories, xxxPackCategory)
		if !kw && !pcat && rel.Size < packParseSizeMin {
			continue // clearly a single scene — skip
		}
		parse := rel.Protocol == "torrent" && rel.DownloadURL != ""
		cands = append(cands, cand{rel: rel, parse: parse, keyword: kw, packCat: pcat})
	}

	// Parse the most promising torrents first (largest), capped.
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].rel.Size > cands[j].rel.Size })
	metas := make([]*torrentmeta.Meta, len(cands))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < packParseWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				fctx, cancel := context.WithTimeout(ctx, packFetchTimeout)
				raw, err := pc.FetchTorrent(fctx, cands[i].rel.DownloadURL)
				cancel()
				if err != nil {
					s.log.Debug("pack torrent fetch", "title", cands[i].rel.Title, "err", err)
					continue
				}
				m, err := torrentmeta.Parse(raw)
				if err != nil {
					s.log.Debug("pack torrent parse", "title", cands[i].rel.Title, "err", err)
					continue
				}
				metas[i] = m
			}
		}()
	}
	parsed := 0
	for i := range cands {
		if cands[i].parse && parsed < packParseMax {
			jobs <- i
			parsed++
		}
	}
	close(jobs)
	wg.Wait()

	out := []packCandidate{}
	for i, c := range cands {
		item := packCandidate{
			Title:       c.rel.Title,
			Indexer:     c.rel.Indexer,
			Protocol:    c.rel.Protocol,
			Size:        c.rel.Size,
			Seeders:     c.rel.Seeders,
			Grabs:       c.rel.Grabs,
			Popularity:  c.rel.Popularity,
			PublishDate: c.rel.PublishDate,
			InfoURL:     c.rel.InfoURL,
			DownloadURL: c.rel.DownloadURL,
		}
		if m := metas[i]; m != nil {
			// Authoritative: we parsed the torrent.
			if m.VideoCount < packMinVideos {
				continue
			}
			item.VideoCount = m.VideoCount
			item.FileCount = m.FileCount
			if m.TotalSize > 0 {
				item.Size = m.TotalSize
			}
		} else {
			// Couldn't parse (magnet / usenet / fetch failed). Keep it
			// only on a strong signal, with an estimated count.
			if !c.packCat && !c.keyword {
				continue
			}
			item.Estimated = true
			item.VideoCount = c.rel.Files // indexer hint; may be 0
		}
		out = append(out, item)
	}
	return out
}

// searchPackIndexers runs the performer-name search across every
// enabled indexer in parallel, each with its own timeout, and merges
// the results. This is the fix for one slow indexer (e.g. 1337x via
// FlareSolverr at ~57s) dragging the whole search past forage's client
// timeout and failing it: a laggard simply gets dropped while the fast
// ones (PornoLab et al., sub-second) return. Searches the XXX parent
// category, which Prowlarr expands to every XXX subcategory (covering
// XXX/Pack 6050 and XXX/Other 6070 alike). Best-effort — returns
// whatever came back.
func (s *Server) searchPackIndexers(ctx context.Context, pc *prowlarr.Client, term string) []prowlarr.Release {
	// Two category passes per indexer:
	//   6050 (XXX/Pack) — precise: an indexer caps results per query
	//     (PornoLab returns 50), and for a prolific performer the broad
	//     pass fills entirely with recent individual scenes, burying old
	//     packs. Filtering to XXX/Pack leaves only packs in that window,
	//     so they surface.
	//   6000 (XXX parent) — broad: catches packs an indexer mis-tagged
	//     outside XXX/Pack (e.g. XXX/Other). The biggest-first parse
	//     selection means the huge packs win the budget over the scene
	//     noise this pass also returns.
	catSets := [][]int{{xxxPackCategory}, {xxxParentCategory}}
	indexers, err := pc.Indexers(ctx)
	if err != nil || len(indexers) == 0 {
		var all []prowlarr.Release
		for _, cs := range catSets {
			if rel, err := pc.Search(ctx, term, cs); err == nil {
				all = append(all, rel...)
			} else {
				s.log.Warn("pack aggregate search", "err", err)
			}
		}
		return dedupReleases(all)
	}
	var (
		mu  sync.Mutex
		all []prowlarr.Release
		wg  sync.WaitGroup
	)
	for _, ix := range indexers {
		if !ix.Enable {
			continue
		}
		for _, cs := range catSets {
			wg.Add(1)
			go func(id int, name string, cats []int) {
				defer wg.Done()
				ictx, cancel := context.WithTimeout(ctx, packIndexerTimeout)
				defer cancel()
				rel, err := pc.SearchScoped(ictx, term, cats, []int{id})
				if err != nil {
					s.log.Debug("pack indexer search dropped", "indexer", name, "err", err)
					return
				}
				mu.Lock()
				all = append(all, rel...)
				mu.Unlock()
			}(ix.ID, ix.Name, cs)
		}
	}
	wg.Wait()
	return dedupReleases(all)
}

// dedupReleases collapses results that appear in more than one category
// pass, keyed by download URL (falling back to info URL + title).
func dedupReleases(rs []prowlarr.Release) []prowlarr.Release {
	seen := make(map[string]bool, len(rs))
	out := make([]prowlarr.Release, 0, len(rs))
	for _, r := range rs {
		key := r.DownloadURL
		if key == "" {
			key = r.InfoURL + "|" + r.Title
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out
}

func containsInt(xs []int, want int) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// performerName resolves a local Stash performer id to its name via the
// performer cache.
func performerName(ctx context.Context, db *sql.DB, id string) (string, error) {
	var name string
	err := db.QueryRowContext(ctx, `SELECT name FROM performer_cache WHERE stash_id = ?`, id).Scan(&name)
	return name, err
}
