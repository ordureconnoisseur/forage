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

const (
	// reconcileInterval is how often the sync does a FULL re-fetch (vs delta):
	// re-stamps every live scene's cached_at and prunes ones that have vanished
	// from StashDB. Between reconciles it runs cheap deltas.
	reconcileInterval = 7 * 24 * time.Hour

	metaKeySceneReconciled = "scene_cache_reconciled_at"
)

// SyncStashDBScenes is the unified scene-cache sync — it replaces the old
// full-download performer + studio aggregate passes. Normally it runs a DELTA
// fetch (only scenes changed since each subject's watermark); every
// reconcileInterval, or when the cache is empty, it runs a FULL fetch that
// re-stamps everything and prunes vanished scenes.
//
// Flow: owned sweep → performer pass → studio pass → recompute aggregates from
// the cache → rebuild recent_scene_cache (Discover) → (full only) prune.
// StashDB queries run outside any write transaction.
func SyncStashDBScenes(ctx context.Context, sc *stash.Client, sdb *stashdb.Client, db *sql.DB, log *slog.Logger) error {
	if sc == nil || sdb == nil {
		log.Info("scene sync skipped (stash or stashdb not configured)")
		return nil
	}
	start := time.Now().Unix()
	reconciledAt, _ := readMetaInt(ctx, db, metaKeySceneReconciled)
	full := reconciledAt == 0 ||
		start-reconciledAt > int64(reconcileInterval/time.Second) ||
		SceneCacheEmpty(ctx, db)
	mode := "delta"
	if full {
		mode = "full"
	}
	log.Info("scene sync starting", "mode", mode)

	ownedIDs, err := sc.FindAllOwnedStashDBSceneIDs(ctx)
	if err != nil {
		return fmt.Errorf("sweep owned scenes: %w", err)
	}

	if err := syncSubjects(ctx, sdb, db, log, full, start, subjectPerformer); err != nil {
		return err
	}
	if err := syncSubjects(ctx, sdb, db, log, full, start, subjectStudio); err != nil {
		return err
	}

	if err := RecomputeAggregates(ctx, db, ownedIDs); err != nil {
		return fmt.Errorf("recompute aggregates: %w", err)
	}
	cutoff := start - sceneWindowDays*86400
	if err := RebuildRecentSceneCache(ctx, db, cutoff, start, ownedIDs); err != nil {
		return fmt.Errorf("rebuild recent: %w", err)
	}

	pruned := int64(0)
	if full {
		if pruned, err = PruneStaleScenes(ctx, db, start); err != nil {
			return fmt.Errorf("prune: %w", err)
		}
		_ = writeMetaInt(ctx, db, metaKeySceneReconciled, start)
	}
	// Stamp the legacy timestamps too so the boot-freshness checks + API status
	// readers (ScenesRefreshedAt / StudioAggregatesRefreshedAt) keep working.
	_ = writeMetaInt(ctx, db, metaKeyScenesRefreshed, start)
	_ = writeMetaInt(ctx, db, metaKeyStudioAggregatesRefreshed, start)

	log.Info("scene sync done", "mode", mode, "owned_total", len(ownedIDs),
		"pruned", pruned, "elapsed", time.Since(time.Unix(start, 0)))
	return nil
}

type subjectKind int

const (
	subjectPerformer subjectKind = iota
	subjectStudio
)

func (k subjectKind) String() string {
	if k == subjectStudio {
		return "studio"
	}
	return "performer"
}

// syncSubjects runs one delta/full fetch pass over every owned performer (or
// studio), upserting fetched scenes into the persistent cache and advancing
// each subject's watermark. A majority of query failures aborts WITHOUT
// advancing watermarks, so a StashDB outage doesn't poison the deltas.
func syncSubjects(ctx context.Context, sdb *stashdb.Client, db *sql.DB, log *slog.Logger, full bool, start int64, kind subjectKind) error {
	var query string
	workers, hardCap := stashDBFetchWorkers, 5000
	if kind == subjectStudio {
		query = `SELECT stashdb_id, scenes_synced_at FROM studio_cache WHERE stashdb_id NOT LIKE 'stash:%' AND scene_count > 0`
		workers, hardCap = studioStashDBFetchWorkers, studioSceneHardCap
	} else {
		query = `SELECT stashdb_id, scenes_synced_at FROM performer_cache WHERE stashdb_id IS NOT NULL AND stashdb_id != ''`
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("load %ss: %w", kind, err)
	}
	type subj struct {
		id        string
		watermark int64
	}
	var subjects []subj
	for rows.Next() {
		var s subj
		if err := rows.Scan(&s.id, &s.watermark); err != nil {
			rows.Close()
			return fmt.Errorf("scan %s: %w", kind, err)
		}
		subjects = append(subjects, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iter %ss: %w", kind, err)
	}

	var (
		mu            sync.Mutex
		queryErrors   int
		newWatermarks = map[string]int64{}
	)
	jobs := make(chan subj, len(subjects))
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range jobs {
				since := s.watermark
				if full {
					since = 0
				}
				q := stashdb.SceneQuery{PerPage: 100}
				if kind == subjectStudio {
					q.StudioIDs = []string{s.id}
				} else {
					q.PerformerIDs = []string{s.id}
				}
				scenes, err := sdb.QueryScenesSince(ctx, q, since, hardCap)
				if err != nil {
					log.Warn("scene sync query failed", "kind", kind.String(), "id", s.id, "err", err)
					mu.Lock()
					queryErrors++
					mu.Unlock()
					continue
				}
				if err := UpsertSceneBatch(ctx, db, scenes, start); err != nil {
					log.Warn("scene sync upsert failed", "kind", kind.String(), "id", s.id, "err", err)
				}
				wm := s.watermark
				for i := range scenes {
					if scenes[i].Updated > wm {
						wm = scenes[i].Updated
					}
				}
				mu.Lock()
				newWatermarks[s.id] = wm
				mu.Unlock()
			}
		}()
	}
	for _, s := range subjects {
		jobs <- s
	}
	close(jobs)
	wg.Wait()

	if len(subjects) > 0 && queryErrors*2 > len(subjects) {
		return fmt.Errorf("scene sync aborted (%s): %d/%d queries failed, keeping previous watermarks",
			kind, queryErrors, len(subjects))
	}

	// Advance watermarks for the subjects that fetched cleanly.
	table := "performer_cache"
	if kind == subjectStudio {
		table = "studio_cache"
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx,
		`UPDATE `+table+` SET scenes_synced_at = ? WHERE stashdb_id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for id, wm := range newWatermarks {
		if _, err := stmt.ExecContext(ctx, wm, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SceneReconciledAt returns the last full-reconcile timestamp (unix seconds),
// or 0 if the delta sync has never run a full reconcile (e.g. right after the
// feature ships). Boot uses 0 to force the first full reconcile.
func SceneReconciledAt(ctx context.Context, db *sql.DB) (int64, error) {
	return readMetaInt(ctx, db, metaKeySceneReconciled)
}

// readMetaInt / writeMetaInt are small helpers over the meta key/value table.
func readMetaInt(ctx context.Context, db *sql.DB, key string) (int64, error) {
	var s string
	err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&s)
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

func writeMetaInt(ctx context.Context, db *sql.DB, key string, v int64) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, fmt.Sprintf("%d", v))
	return err
}
