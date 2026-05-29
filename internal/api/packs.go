package api

import (
	"context"
	"database/sql"
	"net/http"
	"regexp"
	"sort"
	"sync"

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
	// search; packParseWorkers bounds the concurrency of those fetches.
	packParseMax     = 24
	packParseWorkers = 6
	// xxxPackCategory is Newznab's XXX/Pack — PornoLab tags collection
	// torrents with it, a strong pre-fetch pack signal.
	xxxPackCategory = 6050
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

	cats := packSearchCategories(s.pool.Settings().ProwlarrCategories)
	releases, err := prowlarrC.Search(r.Context(), name, cats)
	if err != nil {
		s.log.Error("pack search", "err", err, "performer", name)
		writeErr(w, http.StatusBadGateway, "prowlarr: "+err.Error())
		return
	}

	packs := s.classifyPacks(r.Context(), prowlarrC, releases)
	sort.SliceStable(packs, func(i, j int) bool { return packs[i].Popularity > packs[j].Popularity })

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
				raw, err := pc.FetchTorrent(ctx, cands[i].rel.DownloadURL)
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

// packSearchCategories unions the configured XXX categories with the
// XXX/Pack category so collection torrents tagged 6050 aren't missed
// when a user's config omits the parent 6000.
func packSearchCategories(configured []int) []int {
	cats := append([]int(nil), configured...)
	if !containsInt(cats, xxxPackCategory) {
		cats = append(cats, xxxPackCategory)
	}
	return cats
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
