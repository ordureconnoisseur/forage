package api

import (
	"context"
	"database/sql"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ordureconnoisseur/forager/internal/prowlarr"
)

// Pack detection tunables. Hardcoded for now; Phase 4 promotes the
// thresholds to the config store + settings UI.
const (
	// packMinVideos: a title-stated video count at or above this marks a
	// release as a pack (when category/keyword don't already). 3 avoids
	// mislabelling a single scene that mentions e.g. "2 scenes".
	packMinVideos = 3
	// packIndexerTimeout caps each per-indexer search in the fan-out, so
	// one slow indexer (e.g. a FlareSolverr-backed tracker mid-Cloudflare
	// -challenge) gets dropped instead of stalling the whole pack search.
	packIndexerTimeout = 12 * time.Second
	// xxxPackCategory is Newznab's XXX/Pack — PornoLab tags collection
	// torrents with it, the strongest pack signal.
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
	VideoCount  int    `json:"video_count"` // parsed from the title; 0 when the title states no count
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
//  2. Prowlarr search on that name across enabled indexers (XXX parent +
//     the XXX/Pack category), fanned out per-indexer with a timeout.
//  3. Classify likely-pack candidates from signals already in each result
//     — XXX/Pack category, a pack keyword, or a title-stated video count.
//     No .torrent is downloaded: private trackers cap daily downloads, so
//     browsing must not burn that quota (the only download is the grab).
//  4. Return the candidates biggest-first.
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

	packs := classifyPacks(releases)
	// Biggest first — performer megapacks are what the user is after; a
	// huge pack with few seeders shouldn't sink below a small one.
	sort.SliceStable(packs, func(i, j int) bool { return packs[i].Size > packs[j].Size })

	resp := packsResponse{Packs: packs}
	resp.Performer.StashID = id
	resp.Performer.Name = name
	writeJSON(w, http.StatusOK, resp)
}

// classifyPacks turns search results into pack candidates using ONLY
// signals already present in the result — it never downloads .torrent
// files. Private trackers (PornoLab) cap daily downloads, so browsing
// for packs must not burn that quota; the only download happens when
// the user actually grabs one.
//
// A release is a pack when it's in the XXX/Pack category, has a pack
// keyword, or its title states a video count >= packMinVideos. The
// count is parsed from the title (e.g. "(87 роликов)", "186 videos")
// and is therefore an estimate — confirmed only once grabbed + scanned.
func classifyPacks(releases []prowlarr.Release) []packCandidate {
	out := []packCandidate{}
	for _, rel := range releases {
		kw := packKeywordRe.MatchString(rel.Title)
		pcat := slices.Contains(rel.Categories, xxxPackCategory)
		count := parsePackCount(rel.Title)
		if !pcat && !kw && count < packMinVideos {
			continue // looks like a single scene
		}
		out = append(out, packCandidate{
			Title:       rel.Title,
			Indexer:     rel.Indexer,
			Protocol:    rel.Protocol,
			Size:        rel.Size,
			VideoCount:  count, // 0 when the title states no count
			Seeders:     rel.Seeders,
			Grabs:       rel.Grabs,
			Popularity:  rel.Popularity,
			PublishDate: rel.PublishDate,
			InfoURL:     rel.InfoURL,
			DownloadURL: rel.GrabURL(),
		})
	}
	return out
}

// packCountRe pulls a stated video count out of a pack title across the
// forms PornoLab/ManyVids/PornHub use: "87 роликов", "133 ролика",
// "186 videos", "26 vid", "50 clips".
var packCountRe = regexp.MustCompile(`(?i)(\d{1,5})\s*(?:ролик|клип|videos?|vids?|clips?|scenes?)`)

func parsePackCount(title string) int {
	m := packCountRe.FindStringSubmatch(title)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
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
	// Cap concurrent indexer searches so many configured indexers (× two
	// category passes) can't stampede Prowlarr/FlareSolverr all at once —
	// the same bound the /search fan-out uses.
	sem := make(chan struct{}, searchMatcherConcurrency)
	for _, ix := range indexers {
		if !ix.Enable {
			continue
		}
		for _, cs := range catSets {
			wg.Add(1)
			go func(id int, name string, cats []int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
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
// pass, keyed by grab URL — download URL, else magnet for magnet-only
// indexers (falling back to info URL + title when neither is present).
func dedupReleases(rs []prowlarr.Release) []prowlarr.Release {
	seen := make(map[string]bool, len(rs))
	out := make([]prowlarr.Release, 0, len(rs))
	for _, r := range rs {
		key := r.GrabURL() // download URL, else magnet — stable for magnet-only indexers
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

// performerName resolves a local Stash performer id to its name via the
// performer cache.
func performerName(ctx context.Context, db *sql.DB, id string) (string, error) {
	var name string
	err := db.QueryRowContext(ctx, `SELECT name FROM performer_cache WHERE stash_id = ?`, id).Scan(&name)
	return name, err
}
