package cache

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ordureconnoisseur/forager/internal/stash"
	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

// BodiesTTL is how long a subject's cached scene bodies are served before a
// detail-page visit re-fetches them. Bodies are now lazy (fetched on click),
// so this bounds staleness without an eager pass.
const BodiesTTL = 24 * time.Hour

// countCap bounds the ID-only count fetch. Set high enough to be exact for
// even the largest studios (the id payload is ~10x lighter than bodies, so the
// old 5000 body-cap no longer applies) — only a defensive backstop.
const countCap = 30000

// SyncStashDBCounts is the LAZY-model eager pass: it computes each owned
// performer/studio's total + owned + last-release from a light ID-ONLY StashDB
// fetch (no bodies — proven to match the old body-fetch counts, see
// docs/lazy-cache-redesign.md), writes them to the aggregates, and refreshes
// Discover from a date-scoped recent fetch. Scene bodies are NOT fetched here —
// they load lazily on a detail-page visit (ScenesForSubjectLazy). Replaces
// SyncStashDBScenes.
func SyncStashDBCounts(ctx context.Context, sc *stash.Client, sdb *stashdb.Client, db *sql.DB, log *slog.Logger) error {
	if sc == nil || sdb == nil {
		log.Info("count sync skipped (stash or stashdb not configured)")
		return nil
	}
	start := time.Now().Unix()
	log.Info("count sync starting")

	ownedIDs, err := sc.FindAllOwnedStashDBSceneIDs(ctx)
	if err != nil {
		return fmt.Errorf("sweep owned scenes: %w", err)
	}
	ownedSet := make(map[string]bool, len(ownedIDs))
	for _, id := range ownedIDs {
		ownedSet[id] = true
	}

	if err := countPass(ctx, sdb, db, log, ownedSet, subjectPerformer); err != nil {
		return err
	}
	if err := countPass(ctx, sdb, db, log, ownedSet, subjectStudio); err != nil {
		return err
	}
	if err := rebuildDiscoverScoped(ctx, sdb, db, log, ownedIDs, start); err != nil {
		return fmt.Errorf("discover: %w", err)
	}

	_ = writeMetaInt(ctx, db, metaKeyScenesRefreshed, start)
	_ = writeMetaInt(ctx, db, metaKeyStudioAggregatesRefreshed, start)
	log.Info("count sync done", "owned_total", len(ownedIDs), "elapsed", time.Since(time.Unix(start, 0)))
	return nil
}

type countResult struct {
	id              string
	total           int
	owned           int
	lastReleaseUnix int64
}

// countPass fetches every owned performer's (or studio's) scene IDS only,
// computes total/owned/last-release, and writes them to the aggregate columns.
// A majority of query failures aborts without touching the DB (keeps the
// previous pass's numbers through a StashDB outage).
func countPass(ctx context.Context, sdb *stashdb.Client, db *sql.DB, log *slog.Logger, ownedSet map[string]bool, kind subjectKind) error {
	var query string
	workers := stashDBFetchWorkers
	if kind == subjectStudio {
		query = `SELECT stashdb_id FROM studio_cache WHERE stashdb_id NOT LIKE 'stash:%' AND scene_count > 0`
		workers = studioStashDBFetchWorkers
	} else {
		query = `SELECT stashdb_id FROM performer_cache WHERE stashdb_id IS NOT NULL AND stashdb_id != ''`
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("load %ss: %w", kind, err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	var (
		mu          sync.Mutex
		results     []countResult
		queryErrors int
	)
	jobs := make(chan string, len(ids))
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				q := stashdb.SceneQuery{}
				if kind == subjectStudio {
					q.StudioIDs = []string{id}
				} else {
					q.PerformerIDs = []string{id}
				}
				cnt, err := sdb.QuerySceneIDs(ctx, q, countCap)
				if err != nil {
					log.Warn("count query failed", "kind", kind.String(), "id", id, "err", err)
					mu.Lock()
					queryErrors++
					mu.Unlock()
					continue
				}
				owned := 0
				for _, sid := range cnt.IDs {
					if ownedSet[sid] {
						owned++
					}
				}
				mu.Lock()
				results = append(results, countResult{id: id, total: cnt.Total, owned: owned, lastReleaseUnix: cnt.LastReleaseUnix})
				mu.Unlock()
			}
		}()
	}
	for _, id := range ids {
		jobs <- id
	}
	close(jobs)
	wg.Wait()

	if len(ids) > 0 && queryErrors*2 > len(ids) {
		return fmt.Errorf("count pass aborted (%s): %d/%d queries failed, keeping previous", kind, queryErrors, len(ids))
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Zero the subjects that can never receive a count (no cross-id / synthetic);
	// queried subjects are overwritten below, failed ones keep their values.
	zero := `UPDATE performer_cache SET total_stashdb_scenes=0, owned_scenes_count=0, last_release_unix=0 WHERE stashdb_id IS NULL OR stashdb_id=''`
	table := "performer_cache"
	if kind == subjectStudio {
		zero = `UPDATE studio_cache SET total_stashdb_scenes=0, owned_scenes_count=0, last_release_unix=0 WHERE stashdb_id LIKE 'stash:%'`
		table = "studio_cache"
	}
	if _, err := tx.ExecContext(ctx, zero); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx,
		`UPDATE `+table+` SET total_stashdb_scenes=?, owned_scenes_count=?, last_release_unix=? WHERE stashdb_id=?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range results {
		if _, err := stmt.ExecContext(ctx, r.total, r.owned, r.lastReleaseUnix, r.id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Info("count pass done", "kind", kind.String(), "subjects", len(ids), "errors", queryErrors)
	return nil
}

// rebuildDiscoverScoped refreshes recent_scene_cache by fetching only RECENT
// (≤ window) scenes per owned performer — bounded, since most performers have
// few recent releases — caches those bodies, then rebuilds the Discover view
// from the cache. Replaces the full-catalogue fetch that fed Discover before.
func rebuildDiscoverScoped(ctx context.Context, sdb *stashdb.Client, db *sql.DB, log *slog.Logger, ownedIDs []string, start int64) error {
	cutoff := start - sceneWindowDays*86400

	rows, err := db.QueryContext(ctx,
		`SELECT stashdb_id FROM performer_cache WHERE stashdb_id IS NOT NULL AND stashdb_id != ''`)
	if err != nil {
		return err
	}
	var perfIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		perfIDs = append(perfIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	var (
		mu       sync.Mutex
		sceneMap = map[string]stashdb.Scene{}
	)
	jobs := make(chan string, len(perfIDs))
	var wg sync.WaitGroup
	for w := 0; w < stashDBFetchWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				scenes := queryRecentScenes(ctx, sdb, id, cutoff)
				mu.Lock()
				for _, s := range scenes {
					sceneMap[s.ID] = s
				}
				mu.Unlock()
			}
		}()
	}
	for _, id := range perfIDs {
		jobs <- id
	}
	close(jobs)
	wg.Wait()

	// Warm the body cache with the recent scenes, then rebuild Discover from it.
	batch := make([]stashdb.Scene, 0, len(sceneMap))
	for _, s := range sceneMap {
		batch = append(batch, s)
	}
	if err := UpsertSceneBatch(ctx, db, batch, start); err != nil {
		log.Warn("discover body cache warm failed", "err", err)
	}
	if err := RebuildRecentSceneCache(ctx, db, cutoff, start, ownedIDs); err != nil {
		return err
	}
	log.Info("discover refresh done", "recent_scenes", len(sceneMap))
	return nil
}

// queryRecentScenes pages a performer's scenes newest-DATE-first and returns the
// ones at/after cutoff, stopping at the first older scene. Best-effort: a query
// error just yields what it has.
func queryRecentScenes(ctx context.Context, sdb *stashdb.Client, performerID string, cutoff int64) []stashdb.Scene {
	var out []stashdb.Scene
	page := 1
	for {
		res, err := sdb.QueryScenes(ctx, stashdb.SceneQuery{
			PerformerIDs: []string{performerID},
			Sort:         "DATE",
			Page:         page,
			PerPage:      100,
		})
		if err != nil || len(res.Scenes) == 0 {
			break
		}
		stop := false
		for _, s := range res.Scenes {
			if parseStashDBDate(s.Date) < cutoff {
				stop = true
				break
			}
			out = append(out, s)
		}
		if stop || len(res.Scenes) < 100 {
			break
		}
		page++
	}
	return out
}

// ScenesForPerformerLazy / ScenesForStudioLazy are the exported lazy loaders the
// API's detail pages call (the subjectKind enum is internal).
func ScenesForPerformerLazy(ctx context.Context, db *sql.DB, sdb *stashdb.Client, stashDBID string) ([]stashdb.Scene, error) {
	return ScenesForSubjectLazy(ctx, db, sdb, subjectPerformer, stashDBID)
}
func ScenesForStudioLazy(ctx context.Context, db *sql.DB, sdb *stashdb.Client, stashDBID string) ([]stashdb.Scene, error) {
	return ScenesForSubjectLazy(ctx, db, sdb, subjectStudio, stashDBID)
}

// ScenesForSubjectLazy serves a subject's filmography from the persistent cache
// when it's fresh (within BodiesTTL); otherwise it fetches the full filmography
// from StashDB, caches it, stamps the freshness time, and returns it. This is
// the lazy body load — a subject's bodies are pulled on first visit (and
// refreshed after the TTL), never eagerly. On a fetch error it falls back to
// whatever stale bodies are cached.
func ScenesForSubjectLazy(ctx context.Context, db *sql.DB, sdb *stashdb.Client, kind subjectKind, stashDBID string) ([]stashdb.Scene, error) {
	table := "performer_cache"
	read := ScenesForPerformer
	if kind == subjectStudio {
		table = "studio_cache"
		read = ScenesForStudio
	}
	var syncedAt int64
	_ = db.QueryRowContext(ctx,
		`SELECT COALESCE(scenes_synced_at,0) FROM `+table+` WHERE stashdb_id = ?`, stashDBID).Scan(&syncedAt)
	fresh := syncedAt > 0 && time.Now().Unix()-syncedAt < int64(BodiesTTL/time.Second)
	if fresh {
		if scenes, err := read(ctx, db, stashDBID); err == nil && len(scenes) > 0 {
			return scenes, nil
		}
	}

	q := stashdb.SceneQuery{PerPage: 100}
	if kind == subjectStudio {
		q.StudioIDs = []string{stashDBID}
	} else {
		q.PerformerIDs = []string{stashDBID}
	}
	scenes, err := sdb.QueryAllScenes(ctx, q, 5000)
	if err != nil {
		if cached, cerr := read(ctx, db, stashDBID); cerr == nil && len(cached) > 0 {
			return cached, nil // serve stale rather than fail
		}
		return nil, err
	}
	if err := UpsertSceneBatch(ctx, db, scenes, time.Now().Unix()); err != nil {
		return scenes, nil // serve what we fetched even if the cache write failed
	}
	_, _ = db.ExecContext(ctx,
		`UPDATE `+table+` SET scenes_synced_at = ? WHERE stashdb_id = ?`, time.Now().Unix(), stashDBID)
	return scenes, nil
}
