package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/ordureconnoisseur/forager/internal/stash"
	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

const (
	metaKeyScenesRefreshed = "scenes_refreshed_at"

	// sceneWindowDays bounds what we store in recent_scene_cache.
	// Scenes older than this drop out on each refresh pass. /discover
	// can narrow further via ?days=N but never exceeds this window.
	sceneWindowDays = 90

	// stashDBFetchWorkers caps parallel per-performer StashDB queries.
	// Empirically ~1s per typical performer × ~900 performers
	// sequential would be 15min; 8 workers brings that to ~2min while
	// staying polite to StashDB.
	stashDBFetchWorkers = 8
)

// ScenesRefreshedAt returns the most recent successful scene-cache
// refresh timestamp (unix seconds), or 0 if it has never run.
func ScenesRefreshedAt(ctx context.Context, db *sql.DB) (int64, error) {
	var s string
	err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, metaKeyScenesRefreshed).Scan(&s)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var v int64
	_, err = fmt.Sscanf(s, "%d", &v)
	return v, err
}

const (
	metaKeyTrendingRefreshed = "trending_refreshed_at"

	// trendingTopN is how many scenes we cache from StashDB's TRENDING
	// sort. The plugin's slider tops out at 50; this cap matches.
	trendingTopN = 50
)

// RefreshTrending pulls the top-N trending scenes from StashDB and
// stamps their trending_rank into recent_scene_cache. Runs on a 1h
// ticker separate from the 12h scene refresh — trending changes
// faster than the performer-filtered window.
//
// New scenes (not already cached from the 12h pass) are inserted with
// local_performer_ids derived from the current performer_cache, and
// owned=0 (best-effort; will get corrected at next 12h pass). Scenes
// already cached have their trending_rank updated without touching
// their owned flag — that's the 12h refresh's responsibility.
//
// Scenes that previously held a trending_rank > 0 but aren't in the
// new top-N get reset to 0 so the trending list stays tight.
func RefreshTrending(ctx context.Context, sc *stash.Client, sdb *stashdb.Client, db *sql.DB, log *slog.Logger) error {
	if sdb == nil {
		log.Info("trending refresh skipped (stashdb not configured)")
		return nil
	}
	start := time.Now().Unix()
	log.Info("trending refresh starting", "top_n", trendingTopN)

	// Owned sweep so trending rows carry an accurate owned flag and the
	// discover endpoint can hide already-owned scenes without waiting
	// on the 12h pass. Best-effort: on failure (or Stash unconfigured)
	// we preserve whatever owned flag the 12h pass last set rather than
	// clobbering it to 0.
	ownedSet := map[string]bool{}
	sweepOK := false
	if sc != nil {
		if ids, err := sc.FindAllOwnedStashDBSceneIDs(ctx); err != nil {
			log.Warn("trending owned sweep failed; preserving owned flags", "err", err)
		} else {
			sweepOK = true
			for _, id := range ids {
				ownedSet[id] = true
			}
		}
	}

	scenes, err := sdb.QueryAllScenes(ctx, stashdb.SceneQuery{
		Sort:    "TRENDING",
		PerPage: trendingTopN,
	}, trendingTopN)
	if err != nil {
		return fmt.Errorf("query trending: %w", err)
	}

	// Load performer_cache → stashdb_id → local stash_id map so we can
	// fill local_performer_ids on new trending scenes.
	rows, err := db.QueryContext(ctx,
		`SELECT stash_id, stashdb_id FROM performer_cache WHERE stashdb_id IS NOT NULL AND stashdb_id != ''`)
	if err != nil {
		return fmt.Errorf("load performer map: %w", err)
	}
	stashdbToLocal := map[string]string{}
	for rows.Next() {
		var local, sdbID string
		if err := rows.Scan(&local, &sdbID); err != nil {
			rows.Close()
			return err
		}
		stashdbToLocal[sdbID] = local
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// Persist the full scenes, not just their trending rank. Trending is the
	// one path that surfaces scenes for performers the user does NOT have, and
	// until now it wrote only recent_scene_cache — so a trending-only scene
	// had no stashdb_scene row and therefore no StashDB performer list
	// anywhere (31 of 50 trending rows had one on the reference instance, and
	// only because the 12h performer-filtered pass had already cached them).
	// Discover cannot offer a pill for a performer it has never heard of.
	//
	// Best-effort: trending is still usable without the enrichment, so a
	// failure here logs and carries on to the rank update below.
	if err := UpsertSceneBatch(ctx, db, scenes, start); err != nil {
		log.Warn("trending: cache scene details", "err", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Reset trending_rank everywhere first. Cheap (only ~50 rows ever
	// have rank > 0). Anything still in the new top-N gets re-set in
	// the upsert below.
	if _, err := tx.ExecContext(ctx,
		`UPDATE recent_scene_cache SET trending_rank = 0 WHERE trending_rank > 0`); err != nil {
		return fmt.Errorf("reset trending: %w", err)
	}

	// Update `owned` on conflict only when the sweep succeeded (then
	// it's authoritative); otherwise preserve whatever the 12h pass set.
	// local_performer_ids is always refreshed so a newly trending scene
	// picks up its chips immediately.
	ownedConflict := ""
	if sweepOK {
		ownedConflict = "owned = excluded.owned,\n\t\t  "
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO recent_scene_cache (
		  stashdb_id, title, release_date, release_unix,
		  studio_name, image_url, local_performer_ids, owned,
		  trending_rank, cached_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(stashdb_id) DO UPDATE SET
		  title               = excluded.title,
		  release_date        = excluded.release_date,
		  release_unix        = excluded.release_unix,
		  studio_name         = excluded.studio_name,
		  image_url           = excluded.image_url,
		  local_performer_ids = excluded.local_performer_ids,
		  `+ownedConflict+`trending_rank       = excluded.trending_rank,
		  cached_at           = excluded.cached_at
	`)
	if err != nil {
		return fmt.Errorf("prepare upsert: %w", err)
	}
	defer stmt.Close()

	for i, s := range scenes {
		rank := i + 1
		row := buildSceneRow(s, parseStashDBDate(s.Date), stashdbToLocal, ownedSet)
		idsJSON, _ := json.Marshal(row.localPerformerIDs)
		ownedFlag := 0
		if row.owned {
			ownedFlag = 1
		}
		if _, err := stmt.ExecContext(ctx,
			row.stashdbID, row.title, row.releaseDate, row.releaseUnix,
			row.studioName, row.imageURL, string(idsJSON), ownedFlag,
			rank, start,
		); err != nil {
			return fmt.Errorf("upsert trending %s: %w", row.stashdbID, err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, metaKeyTrendingRefreshed, fmt.Sprintf("%d", start)); err != nil {
		return fmt.Errorf("update meta: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	log.Info("trending refresh done",
		"scenes", len(scenes),
		"elapsed", time.Since(time.Unix(start, 0)))
	return nil
}

// TrendingRefreshedAt returns the most recent successful trending
// refresh timestamp (unix seconds), or 0 if it has never run.
func TrendingRefreshedAt(ctx context.Context, db *sql.DB) (int64, error) {
	var s string
	err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, metaKeyTrendingRefreshed).Scan(&s)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var v int64
	_, err = fmt.Sscanf(s, "%d", &v)
	return v, err
}

// buildSceneRow converts a StashDB Scene into the row shape we cache.
// Intersects the scene's StashDB performer list with our local
// performer map so the UI can render chips without an extra lookup.
func buildSceneRow(s stashdb.Scene, releaseUnix int64, stashdbToLocal map[string]string, ownedSet map[string]bool) *sceneRow {
	var localIDs []string
	for _, sp := range s.Performers {
		if local, ok := stashdbToLocal[sp.ID]; ok {
			localIDs = append(localIDs, local)
		}
	}
	studio := ""
	if s.Studio != nil {
		studio = s.Studio.Name
	}
	image := ""
	if len(s.Images) > 0 {
		image = s.Images[0].URL
	}
	return &sceneRow{
		stashdbID:         s.ID,
		title:             s.Title,
		releaseDate:       s.Date,
		releaseUnix:       releaseUnix,
		studioName:        studio,
		imageURL:          image,
		localPerformerIDs: localIDs,
		owned:             ownedSet[s.ID],
	}
}

type sceneRow struct {
	stashdbID         string
	title             string
	releaseDate       string
	releaseUnix       int64
	studioName        string
	imageURL          string
	localPerformerIDs []string
	owned             bool
}

// parseStashDBDate accepts the YYYY-MM-DD format StashDB returns.
// Empty / unparseable input returns 0 — those scenes lose their place
// in last-release ordering but stay cacheable, which is the right
// trade (better to surface a scene with unknown date than drop it).
func parseStashDBDate(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return 0
	}
	return t.Unix()
}
