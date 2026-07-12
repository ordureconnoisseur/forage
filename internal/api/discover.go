package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/ordureconnoisseur/forager/internal/cache"
)

func nowUnix() int64 { return time.Now().Unix() }

// discoverScene is the wire shape of a Discover row. Performers are
// denormalised so the UI can render chips without a second lookup.
type discoverScene struct {
	StashDBID   string              `json:"stashdb_id"`
	Title       string              `json:"title,omitempty"`
	ReleaseDate string              `json:"release_date,omitempty"`
	ReleaseUnix int64               `json:"release_unix,omitempty"`
	StudioName  string              `json:"studio_name,omitempty"`
	ImageURL    string              `json:"image_url,omitempty"`
	Performers  []discoverPerformer `json:"performers"`
	// WatchStatus is "watching"/"available" when the user tracks this
	// scene, empty otherwise — so the card's watch control reflects it.
	WatchStatus string `json:"watch_status,omitempty"`
}

type discoverPerformer struct {
	StashID  string `json:"stash_id"`
	Name     string `json:"name"`
	Favorite bool   `json:"favorite"`
	// Stats used by the plugin's hovercard. Sourced from
	// performer_cache aggregates (set by the 12h scene-cache refresh).
	// Zero values are fine — UI elides them when 0.
	SceneCount         int   `json:"scene_count,omitempty"`
	TotalStashDBScenes int   `json:"total_stashdb_scenes,omitempty"`
	OwnedScenesCount   int   `json:"owned_scenes_count,omitempty"`
	LastReleaseUnix    int64 `json:"last_release_unix,omitempty"`
}

type discoverResponse struct {
	Scenes              []discoverScene `json:"scenes"`
	Trending            []discoverScene `json:"trending"`
	Days                int             `json:"days"`
	RefreshedAt         int64           `json:"refreshed_at"`
	TrendingRefreshedAt int64           `json:"trending_refreshed_at"`
}

// getDiscover returns recent unowned StashDB scenes featuring ≥1 of
// the user's local performers. Backed entirely by recent_scene_cache
// (rebuilt every 12h by cache.RefreshSceneCache); no live StashDB
// calls per request.
//
// Query params:
//
//	days          window in days from now (default 30, capped at 90 —
//	              the underlying cache window)
//	favorite_only ("true" / anything else); when true, post-filter to
//	              scenes featuring at least one favorited performer
//	limit            max rows returned in the performer-filtered list
//	                 (default 2000, cap 5000). The 90d cache typically
//	                 holds ~2k unowned recent scenes; UI renders lazy-
//	                 loaded image cards so the browser cost is dominated
//	                 by what's in viewport.
//	trending_limit   max trending rows (default 20, cap 50). StashDB's
//	                 TRENDING sort, refreshed hourly.
func (s *Server) getDiscover(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 {
		days = 30
	}
	if days > 90 {
		days = 90
	}
	favoriteOnly := r.URL.Query().Get("favorite_only") == "true"
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 2000
	}
	if limit > 5000 {
		limit = 5000
	}
	trendingLimit, _ := strconv.Atoi(r.URL.Query().Get("trending_limit"))
	if trendingLimit <= 0 {
		trendingLimit = 20
	}
	if trendingLimit > 50 {
		trendingLimit = 50
	}

	cutoff := nowUnix() - int64(days)*86400

	// Two queries against the same table: recent performer-filtered
	// (owned=0, within window) and trending (rank > 0).
	recentRaw, err := queryDiscoverRows(r.Context(), s.db,
		`SELECT stashdb_id, title, release_date, release_unix,
		        studio_name, image_url, local_performer_ids
		   FROM recent_scene_cache
		  WHERE owned = 0 AND release_unix >= ?
		  ORDER BY release_unix DESC
		  LIMIT ?`, cutoff, limit)
	if err != nil {
		s.log.Error("discover recent query", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	trendingRaw, err := queryDiscoverRows(r.Context(), s.db,
		`SELECT stashdb_id, title, release_date, release_unix,
		        studio_name, image_url, local_performer_ids
		   FROM recent_scene_cache
		  WHERE trending_rank > 0 AND owned = 0
		  ORDER BY trending_rank ASC
		  LIMIT ?`, trendingLimit)
	if err != nil {
		s.log.Error("discover trending query", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}

	// Hydrate performers via a single batched lookup covering both
	// result sets — most performer IDs overlap.
	perfIDset := map[string]struct{}{}
	for _, r := range recentRaw {
		collectPerformerIDs(r.idsJSON, perfIDset)
	}
	for _, r := range trendingRaw {
		collectPerformerIDs(r.idsJSON, perfIDset)
	}
	perfMap, err := loadPerformersByIDs(r.Context(), s.db, perfIDset)
	if err != nil {
		s.log.Error("discover hydrate performers", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}

	watchStatus := s.watchStatusByScene(r.Context())
	scenes := materializeScenes(recentRaw, perfMap, watchStatus, favoriteOnly)
	// Trending isn't favourite-filtered — the whole point is "what's
	// hot globally", regardless of which performers we have.
	trending := materializeScenes(trendingRaw, perfMap, watchStatus, false)

	// Hide scenes forage already has in flight or in the library. The
	// cached `owned` flag covers externally-owned scenes but lags a fresh
	// grab until the next scene-cache refresh; this drops a just-confirmed
	// (or actively-downloading) scene from Discover immediately.
	// Covered (not just live): a scene whose grab sits in the mismatch
	// review shouldn't be re-suggested while the user hasn't resolved it.
	if _, covered, err := s.grabbedSceneSet(r.Context()); err == nil && len(covered) > 0 {
		scenes = dropGrabbed(scenes, covered)
		trending = dropGrabbed(trending, covered)
	}

	refreshedAt, _ := cache.ScenesRefreshedAt(r.Context(), s.db)
	trendingRefreshedAt, _ := cache.TrendingRefreshedAt(r.Context(), s.db)
	writeJSON(w, http.StatusOK, discoverResponse{
		Scenes:              scenes,
		Trending:            trending,
		Days:                days,
		RefreshedAt:         refreshedAt,
		TrendingRefreshedAt: trendingRefreshedAt,
	})
}

// discoverRawRow is the intermediate shape between scan and hydrate.
// JSON column stays as a string so we only unmarshal once during
// materialize (avoid double-parsing per row).
type discoverRawRow struct {
	stashdbID, title, releaseDate, studio, image, idsJSON string
	releaseUnix                                           int64
}

func queryDiscoverRows(ctx context.Context, db *sql.DB, sqlText string, args ...any) ([]discoverRawRow, error) {
	rows, err := db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []discoverRawRow
	for rows.Next() {
		var r discoverRawRow
		var title, releaseDate, studio, image sql.NullString
		if err := rows.Scan(&r.stashdbID, &title, &releaseDate, &r.releaseUnix,
			&studio, &image, &r.idsJSON); err != nil {
			return nil, err
		}
		r.title = title.String
		r.releaseDate = releaseDate.String
		r.studio = studio.String
		r.image = image.String
		out = append(out, r)
	}
	return out, rows.Err()
}

func collectPerformerIDs(idsJSON string, into map[string]struct{}) {
	if idsJSON == "" || idsJSON == "null" {
		return
	}
	var ids []string
	if err := json.Unmarshal([]byte(idsJSON), &ids); err != nil {
		return
	}
	for _, id := range ids {
		into[id] = struct{}{}
	}
}

// grabbedSceneSet returns two scene-id sets built from the grabs table.
//
// live: scenes with a grab in flight or in the library (queued →
// confirmed) — Discover hides these and reconcileWatches flips their
// watches to 'grabbed'. Keyed by the grab's actual cross-id when known,
// else the predicted one.
//
// covered: live PLUS scenes whose acquisition is pending human resolution
// — a mismatched grab (the download made FOR this scene identified as a
// different one; it sits in the mismatch review) or an orphaned one (in
// limbo, revivable). A covered scene's watch must not re-search, revert,
// or re-notify: the machine's verdict isn't the final word, the user's
// is. Resolving the mismatch (redo/delete purges the grab) removes the
// coverage and the reconcile reverse pass resumes the hunt automatically.
//
// The error return distinguishes "no grabs" (a meaningful empty set — the
// reverse pass reverts on it) from a lookup failure (act on nothing).
func (s *Server) grabbedSceneSet(ctx context.Context) (live, covered map[string]bool, err error) {
	if s.grabs == nil {
		return nil, nil, errors.New("grabs unavailable")
	}
	byScene, err := s.grabs.StatusByStashDBID(ctx)
	if err != nil {
		return nil, nil, err
	}
	live = make(map[string]bool, len(byScene))
	covered = make(map[string]bool, len(byScene))
	for sid, st := range byScene {
		switch st {
		// 'deferred' is live: the add hasn't landed but the retry loop
		// will re-drive it. Omitting it here made the watch reconcile
		// treat the scene as uncovered, revert the watch to watching, and
		// auto-offer a DIFFERENT release of the same scene while the
		// deferred grab's own retry was minutes away: a duplicate
		// download of a scene the user was already getting.
		case "queued", "deferred", "downloading", "completed", "placed", "scanned", "confirmed":
			live[sid] = true
			covered[sid] = true
		case "mismatched", "orphaned":
			covered[sid] = true
		}
	}
	return live, covered, nil
}

// dropGrabbed removes scenes the user is already getting/owns. Filters in
// place — materializeScenes returns a freshly-allocated slice.
func dropGrabbed(scenes []discoverScene, grabbed map[string]bool) []discoverScene {
	kept := scenes[:0]
	for _, sc := range scenes {
		if !grabbed[sc.StashDBID] {
			kept = append(kept, sc)
		}
	}
	return kept
}

func materializeScenes(raw []discoverRawRow, perfMap map[string]discoverPerformer, watchStatus map[string]string, favoriteOnly bool) []discoverScene {
	out := make([]discoverScene, 0, len(raw))
	for _, r := range raw {
		var ids []string
		if r.idsJSON != "" && r.idsJSON != "null" {
			_ = json.Unmarshal([]byte(r.idsJSON), &ids)
		}
		performers := make([]discoverPerformer, 0, len(ids))
		anyFav := false
		for _, id := range ids {
			if p, ok := perfMap[id]; ok {
				performers = append(performers, p)
				if p.Favorite {
					anyFav = true
				}
			}
		}
		if favoriteOnly && !anyFav {
			continue
		}
		out = append(out, discoverScene{
			StashDBID:   r.stashdbID,
			Title:       r.title,
			ReleaseDate: r.releaseDate,
			ReleaseUnix: r.releaseUnix,
			StudioName:  r.studio,
			ImageURL:    r.image,
			Performers:  performers,
			WatchStatus: watchStatus[r.stashdbID],
		})
	}
	return out
}

// loadPerformersByIDs hydrates a set of local stash_ids into the
// denormalised name+favorite shape the Discover view shows on each
// scene card. Returns a map keyed by stash_id; entries missing from
// performer_cache are silently dropped (defensive — the scene cache
// only references IDs that existed at refresh time, but a performer
// could disappear between refreshes).
func loadPerformersByIDs(ctx context.Context, db *sql.DB, ids map[string]struct{}) (map[string]discoverPerformer, error) {
	out := map[string]discoverPerformer{}
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := make([]byte, 0, len(ids)*2)
	args := make([]any, 0, len(ids))
	first := true
	for id := range ids {
		if !first {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		args = append(args, id)
		first = false
	}
	rows, err := db.QueryContext(ctx,
		"SELECT stash_id, name, favorite, scene_count, "+
			"total_stashdb_scenes, owned_scenes_count, last_release_unix "+
			"FROM performer_cache WHERE stash_id IN ("+string(placeholders)+")",
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p discoverPerformer
		var fav int
		if err := rows.Scan(&p.StashID, &p.Name, &fav, &p.SceneCount,
			&p.TotalStashDBScenes, &p.OwnedScenesCount, &p.LastReleaseUnix); err != nil {
			return nil, err
		}
		p.Favorite = fav != 0
		out[p.StashID] = p
	}
	return out, rows.Err()
}
