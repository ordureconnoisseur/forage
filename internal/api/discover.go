package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
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
	StashID string `json:"stash_id"`
	Name    string `json:"name"`
	// StashDBID is the performer's StashDB cross-id. Present for performers
	// resolved from a scene's cached StashDB performer list, which is the
	// only way a performer the user does NOT have can be named at all.
	StashDBID string `json:"stashdb_id,omitempty"`
	// Local is false for a performer who exists on StashDB but not in the
	// user's Stash. Those used to be dropped silently: the discover query
	// hydrates from performer_cache, so a trending scene's unknown performer
	// simply had no pill, and therefore no way to add them. The UI renders
	// these differently and offers the "+".
	Local    bool `json:"local"`
	Favorite bool `json:"favorite"`
	// Gender: Stash performer gender enum, cached for the configurable
	// Discover content filters. Omitted from JSON when empty.
	Gender string `json:"gender,omitempty"`
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
	// Filters is the deployment's configured content filters as a full
	// name-to-genders mapping (same shape as /performers): the UI
	// renders selection chips only when non-empty, and derives each
	// chip's gender glyph from its set. Scene filtering itself stays
	// server-side (?flt=), since trending performers may not be local.
	Filters map[string][]string `json:"filters,omitempty"`
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
	// Trending scenes belong in the carousel, not sprinkled through the feed
	// as well. Both lists read one table (trending_rank > 0 marks the
	// trending set), and this query used to take everything in the window, so
	// a globally-trending scene with nothing to do with the user's performers
	// appeared below as if it were one of their new releases.
	//
	// The exclusion is "trending AND no local performer", not "trending":
	// a scene BY one of your performers that also happens to be trending is
	// in the feed because it is theirs, and dropping it would hide your own
	// performers' new releases. 12 rows in the current window are the leak;
	// 8 are legitimately both.
	//
	// The empty test spells out 'null' because that is the value actually
	// stored — a nil slice marshals to the four-character string "null", not
	// to '[]' or SQL NULL, so the obvious comparison silently matches nothing.
	recentRaw, err := queryDiscoverRows(r.Context(), s.db,
		`SELECT stashdb_id, title, release_date, release_unix,
		        studio_name, image_url, local_performer_ids
		   FROM recent_scene_cache
		  WHERE owned = 0 AND release_unix >= ?
		    AND NOT (trending_rank > 0
		             AND COALESCE(local_performer_ids,'') IN ('', '[]', 'null'))
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

	// Configurable content filters (deployment config, dormant when
	// unset): ?flt=<name> keeps scenes with at least one local performer
	// whose gender is in the named set. Trending scenes whose performers
	// aren't local can't be judged and are dropped under a filter.
	filters := parseDiscoverFilters(s.pool.Settings().DiscoverFilters)
	if name := r.URL.Query().Get("flt"); name != "" {
		if genders, ok := filters[name]; ok {
			scenes = filterScenesByGender(scenes, genders)
			trending = filterScenesByGender(trending, genders)
		}
	}

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

	// Fill in the performers the user does NOT have, on BOTH lists.
	//
	// This first shipped applied to `trending` only, which missed the case
	// that actually shows up: a scene in the main feed whose co-star isn't in
	// your Stash rendered with NO pill at all (a "Hot Blonde Charlie Forde"
	// card had zero, because she was its only performer and she is not
	// local). Hydration comes from performer_cache, so any performer you
	// don't have is dropped regardless of which list the scene is in.
	//
	// Deliberately AFTER the gender filter and the grabbed drop, so this only
	// decorates the final lists. Filtering matches on a local performer's
	// cached gender, which a non-local performer has none of, so running it
	// earlier would not change any verdict — but doing it last means it
	// cannot.
	sceneIDs := make([]string, 0, len(scenes)+len(trending))
	for i := range scenes {
		sceneIDs = append(sceneIDs, scenes[i].StashDBID)
	}
	for i := range trending {
		sceneIDs = append(sceneIDs, trending[i].StashDBID)
	}
	if sdbPerfs, perr := loadStashDBPerformers(r.Context(), s.db, sceneIDs); perr != nil {
		s.log.Warn("discover: stashdb performer hydrate", "err", perr)
	} else if len(sdbPerfs) > 0 {
		localByStashDBID, localByName, lerr := localPerformerIDsByStashDBID(r.Context(), s.db)
		if lerr != nil {
			s.log.Warn("discover: local performer index", "err", lerr)
		} else {
			scenes = addMissingPerformers(scenes, sdbPerfs, localByStashDBID, localByName)
			trending = addMissingPerformers(trending, sdbPerfs, localByStashDBID, localByName)
		}
	}

	refreshedAt, _ := cache.ScenesRefreshedAt(r.Context(), s.db)
	trendingRefreshedAt, _ := cache.TrendingRefreshedAt(r.Context(), s.db)
	outFilters := filters
	if len(outFilters) == 0 {
		outFilters = nil // omitempty: dormant deployments send no chips
	}
	writeJSON(w, http.StatusOK, discoverResponse{
		Scenes:              scenes,
		Trending:            trending,
		Days:                days,
		RefreshedAt:         refreshedAt,
		TrendingRefreshedAt: trendingRefreshedAt,
		Filters:             outFilters,
	})
}

// parseDiscoverFilters parses the deployment's named content filters
// ("Name=GENDER1,GENDER2;Name2=..."): filter name to the gender set
// (uppercased) it selects. Empty/malformed entries are skipped; an
// unset config yields an empty map, which keeps the whole feature
// dormant (no chips, no filtering).
func parseDiscoverFilters(raw string) map[string][]string {
	out := map[string][]string{}
	for _, part := range strings.Split(raw, ";") {
		name, list, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || strings.TrimSpace(name) == "" {
			continue
		}
		var genders []string
		for _, g := range strings.Split(list, ",") {
			if g = strings.ToUpper(strings.TrimSpace(g)); g != "" {
				genders = append(genders, g)
			}
		}
		if len(genders) > 0 {
			out[strings.TrimSpace(name)] = genders
		}
	}
	return out
}

// filterScenesByGender keeps scenes with at least one performer whose
// gender is in the set. Filters in place.
func filterScenesByGender(scenes []discoverScene, genders []string) []discoverScene {
	want := map[string]bool{}
	for _, g := range genders {
		want[g] = true
	}
	kept := scenes[:0]
	for _, sc := range scenes {
		for _, p := range sc.Performers {
			if want[p.Gender] {
				kept = append(kept, sc)
				break
			}
		}
	}
	return kept
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
		"SELECT stash_id, name, favorite, COALESCE(gender,''), scene_count, "+
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
		if err := rows.Scan(&p.StashID, &p.Name, &fav, &p.Gender, &p.SceneCount,
			&p.TotalStashDBScenes, &p.OwnedScenesCount, &p.LastReleaseUnix); err != nil {
			return nil, err
		}
		p.Favorite = fav != 0
		p.Local = true // it came from performer_cache, so the user has them
		out[p.StashID] = p
	}
	return out, rows.Err()
}

// stashDBPerformer is one entry of stashdb_scene.performers.
type stashDBPerformer struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// loadStashDBPerformers returns, per scene id, the scene's full StashDB
// performer list from the scene cache.
//
// This is what makes a performer the user does not have nameable at all.
// The discover queries hydrate from performer_cache, which by definition
// only holds performers already in Stash, so a trending scene's unknown
// performer had no pill and no route to being added. Every trending row on
// the reference instance had at least one LOCAL performer, which is why the
// cards looked populated while the interesting name was missing.
func loadStashDBPerformers(ctx context.Context, db *sql.DB, sceneIDs []string) (map[string][]stashDBPerformer, error) {
	out := map[string][]stashDBPerformer{}
	if len(sceneIDs) == 0 {
		return out, nil
	}
	const chunk = 400
	for start := 0; start < len(sceneIDs); start += chunk {
		end := min(start+chunk, len(sceneIDs))
		batch := sceneIDs[start:end]
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		q := `SELECT stashdb_id, COALESCE(performers,'[]') FROM stashdb_scene WHERE stashdb_id IN (?` +
			strings.Repeat(",?", len(batch)-1) + `)`
		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var sceneID, blob string
			if err := rows.Scan(&sceneID, &blob); err != nil {
				rows.Close()
				return out, err
			}
			var ps []stashDBPerformer
			if blob != "" && blob != "[]" {
				_ = json.Unmarshal([]byte(blob), &ps)
			}
			if len(ps) > 0 {
				out[sceneID] = ps
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return out, err
		}
	}
	return out, nil
}

// addMissingPerformers appends, to each scene, the StashDB performers that
// are not already represented by a local pill. Matching is by StashDB id
// where the local performer has one, else by name — a local performer
// without a cross-id would otherwise be duplicated by its own StashDB entry.
func addMissingPerformers(scenes []discoverScene, byScene map[string][]stashDBPerformer,
	localByStashDBID, localByName map[string]string) []discoverScene {
	for i := range scenes {
		sdbPerfs := byScene[scenes[i].StashDBID]
		if len(sdbPerfs) == 0 {
			continue
		}
		haveName := map[string]bool{}
		for _, p := range scenes[i].Performers {
			haveName[strings.ToLower(p.Name)] = true
		}
		for _, sp := range sdbPerfs {
			if sp.ID == "" || sp.Name == "" {
				continue
			}
			if haveName[strings.ToLower(sp.Name)] {
				continue // already rendered on this card
			}
			haveName[strings.ToLower(sp.Name)] = true

			// Do we actually own them? Two ways to know, because either
			// index can be stale or absent:
			//   - by cross-id, which misses the 308-of-1087 local performers
			//     that carry no cross-id at all;
			//   - by name, which catches those.
			localID, owned := localByStashDBID[sp.ID]
			if !owned {
				localID, owned = localByName[strings.ToLower(sp.Name)]
			}
			if owned {
				// Render them as the local performer they are, rather than
				// skipping. The scene's own local_performer_ids list is
				// rebuilt on a 12h cadence, so a performer added minutes ago
				// is missing from it — skipping here left them with NO pill,
				// and trusting that list alone offered to add someone the
				// user already had.
				scenes[i].Performers = append(scenes[i].Performers, discoverPerformer{
					StashID:   localID,
					Name:      sp.Name,
					StashDBID: sp.ID,
					Local:     true,
				})
				continue
			}
			scenes[i].Performers = append(scenes[i].Performers, discoverPerformer{
				Name:      sp.Name,
				StashDBID: sp.ID,
				Local:     false,
			})
		}
	}
	return scenes
}

// localPerformerIDsByStashDBID maps StashDB performer id → local Stash id
// for everyone the user already has. Used to tell "this StashDB performer is
// already on the card as a local pill" from "this one is genuinely missing",
// which decides whether a "+" is offered.
func localPerformerIDsByStashDBID(ctx context.Context, db *sql.DB) (byStashDBID, byName map[string]string, err error) {
	// Every local performer, not only the cross-id'd ones: 308 of 1087 on the
	// reference instance have no cross-id, and indexing by id alone treats
	// every one of them as un-owned.
	rows, err := db.QueryContext(ctx,
		`SELECT COALESCE(stashdb_id,''), stash_id, name FROM performer_cache`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	byStashDBID, byName = map[string]string{}, map[string]string{}
	for rows.Next() {
		var sdbID, local, name string
		if err := rows.Scan(&sdbID, &local, &name); err != nil {
			return nil, nil, err
		}
		if sdbID != "" {
			byStashDBID[sdbID] = local
		}
		if name != "" {
			byName[strings.ToLower(name)] = local
		}
	}
	return byStashDBID, byName, rows.Err()
}
