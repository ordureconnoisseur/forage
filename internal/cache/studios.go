package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/ordureconnoisseur/forager/internal/stash"
	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

const (
	metaKeyStudioRefreshed           = "studio_refreshed_at"
	metaKeyStudioAggregatesRefreshed = "studio_aggregates_refreshed_at"

	// studioStashDBFetchWorkers caps parallel per-studio StashDB queries.
	// Lower than the performer pass's 8: a studio's catalogue is far larger
	// (up to the 5000 hard cap, many pages each), so fewer concurrent studios
	// keeps total in-flight request volume polite to StashDB.
	studioStashDBFetchWorkers = 6

	// studioSceneHardCap bounds how many of a studio's StashDB scenes we
	// enumerate for the aggregate counts — a major studio can have many
	// thousands; the count past this is academic for a completion bar.
	studioSceneHardCap = 5000
)

// RefreshStudios pulls every studio from local Stash, then enriches
// each studio's alias list with StashDB's current view (which can be
// fresher than Stash's locally-scraped copy) plus its parent studio's
// name. The parent inclusion closes the LegalPorno → American Anal
// matcher failure mode: release names that mention the parent studio
// will match scenes catalogued under the child.
//
// sdb may be nil — when so, the StashDB enrichment is skipped and the
// cache holds the Stash-side aliases only.
func RefreshStudios(ctx context.Context, sc *stash.Client, sdb *stashdb.Client, db *sql.DB, log *slog.Logger) error {
	start := time.Now().Unix()
	log.Info("studio refresh starting")

	studios, err := sc.FindStudios(ctx)
	if err != nil {
		return fmt.Errorf("fetch studios: %w", err)
	}

	// Bulk-pull StashDB studios so we can union their aliases (+ parent
	// name) into what local Stash gave us. Keyed by StashDB id.
	stashdbStudios := map[string]stashdb.Studio{}
	if sdb != nil {
		t0 := time.Now()
		all, err := sdb.QueryAllStudios(ctx)
		if err != nil {
			log.Warn("stashdb QueryAllStudios failed; alias enrichment skipped", "err", err)
		} else {
			for _, s := range all {
				stashdbStudios[s.ID] = s
			}
			log.Info("stashdb studios fetched", "count", len(all), "elapsed", time.Since(t0))
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Studios are keyed in our cache by stashdb_id (the cross-id). For
	// studios that have no StashDB mapping, we fall back to a synthetic
	// "stash:{local_id}" key so we can still cache them — matcher logic
	// can decide later whether to use them.
	// Upsert the local-studio fields (name, aliases, local id, favorite,
	// scene_count). The StashDB-filmography aggregates (total/owned/
	// last_release) are owned by RefreshStudioCache and deliberately left out
	// of the SET list so this pass never clobbers them; a fresh INSERT gets
	// their column defaults (0) until the aggregate pass fills them in.
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO studio_cache (stashdb_id, stash_id, name, aliases, favorite, scene_count, refreshed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(stashdb_id) DO UPDATE SET
			stash_id     = excluded.stash_id,
			name         = excluded.name,
			aliases      = excluded.aliases,
			favorite     = excluded.favorite,
			scene_count  = excluded.scene_count,
			refreshed_at = excluded.refreshed_at
	`)
	if err != nil {
		return fmt.Errorf("prepare upsert: %w", err)
	}
	defer stmt.Close()

	upserted, enriched := 0, 0
	for _, s := range studios {
		key := stash.PickStashDBID(s.StashIDs)
		if key == "" {
			key = "stash:" + s.ID
		}
		// Start with what local Stash returned; union in StashDB
		// aliases + parent name when we have a cross-id match.
		aliases := mergeAliases(s.Aliases)
		if sdbStudio, ok := stashdbStudios[key]; ok {
			aliases = mergeAliases(aliases, sdbStudio.Aliases...)
			if sdbStudio.ParentName != "" {
				aliases = mergeAliases(aliases, sdbStudio.ParentName)
			}
			enriched++
		}
		aliasesJSON, _ := json.Marshal(aliases)
		fav := 0
		if s.Favorite {
			fav = 1
		}
		if _, err := stmt.ExecContext(ctx, key, s.ID, s.Name, string(aliasesJSON), fav, s.SceneCount, start); err != nil {
			return fmt.Errorf("upsert studio %s (%s): %w", s.ID, s.Name, err)
		}
		upserted++
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM studio_cache WHERE refreshed_at < ?`, start)
	if err != nil {
		return fmt.Errorf("delete stale: %w", err)
	}
	deleted, _ := res.RowsAffected()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, metaKeyStudioRefreshed, fmt.Sprintf("%d", start)); err != nil {
		return fmt.Errorf("update meta: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	log.Info("studio refresh done",
		"upserted", upserted,
		"enriched_from_stashdb", enriched,
		"deleted", deleted,
		"elapsed", time.Since(time.Unix(start, 0)))
	return nil
}

// mergeAliases unions multiple alias sources into a single
// deduplicated, order-preserving list. Comparison is case-insensitive
// + whitespace-trimmed so "BLACKED" and "Blacked" don't show up
// twice. Original casing is preserved from the first occurrence so
// the matcher's tokenization keeps producing the same tokens.
func mergeAliases(base []string, extra ...string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(base)+len(extra))
	add := func(a string) {
		a = strings.TrimSpace(a)
		if a == "" {
			return
		}
		k := strings.ToLower(a)
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, a)
	}
	for _, a := range base {
		add(a)
	}
	for _, a := range extra {
		add(a)
	}
	return out
}

func StudioRefreshedAt(ctx context.Context, db *sql.DB) (int64, error) {
	var s string
	err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, metaKeyStudioRefreshed).Scan(&s)
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

// StudioAggregatesRefreshedAt returns the last successful studio aggregate
// refresh timestamp (unix seconds), or 0 if it has never run.
func StudioAggregatesRefreshedAt(ctx context.Context, db *sql.DB) (int64, error) {
	var s string
	err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, metaKeyStudioAggregatesRefreshed).Scan(&s)
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

// RefreshStudioCache fills studio_cache's per-studio aggregates (total StashDB
// scenes / owned / last release) by querying each cross-id'd studio's StashDB
// filmography — the studio analogue of RefreshSceneCache's performer pass.
// Studios with no StashDB cross-id (synthetic "stash:<id>" key) can't be
// queried, so their aggregates are zeroed. A majority of query failures aborts
// without touching the DB, keeping the previous pass's counts (so a StashDB
// outage doesn't wipe every studio's completion bar for up to 12h).
//
// Runs AFTER RefreshStudios on the same ticker — it reads the studio rows that
// pass enumerated. StashDB queries run OUTSIDE the write transaction.
func RefreshStudioCache(ctx context.Context, sc *stash.Client, sdb *stashdb.Client, db *sql.DB, log *slog.Logger) error {
	if sc == nil || sdb == nil {
		log.Info("studio aggregate refresh skipped (stash or stashdb not configured)")
		return nil
	}
	start := time.Now().Unix()
	log.Info("studio aggregate refresh starting")

	// ── Owned scenes sweep ───────────────────────────────────────────
	ownedIDs, err := sc.FindAllOwnedStashDBSceneIDs(ctx)
	if err != nil {
		return fmt.Errorf("sweep owned scenes: %w", err)
	}
	ownedSet := make(map[string]bool, len(ownedIDs))
	for _, id := range ownedIDs {
		ownedSet[id] = true
	}

	// ── Owned studios with a real StashDB cross-id (skip synthetic keys
	// and studios the user has no scenes from — querying their full StashDB
	// catalogue would be pure waste). ─────────────────────────────────
	rows, err := db.QueryContext(ctx,
		`SELECT stashdb_id FROM studio_cache WHERE stashdb_id NOT LIKE 'stash:%' AND scene_count > 0`)
	if err != nil {
		return fmt.Errorf("load studios: %w", err)
	}
	var studioIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan studio: %w", err)
		}
		studioIDs = append(studioIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iter studios: %w", err)
	}

	// ── Parallel StashDB fetches ──────────────────────────────────────
	type aggUpdate struct {
		stashdbID       string
		totalScenes     int
		ownedCount      int
		lastReleaseUnix int64
	}
	var (
		mu          sync.Mutex
		aggUpdates  []aggUpdate
		queryErrors int
	)
	jobs := make(chan string, len(studioIDs))
	var wg sync.WaitGroup
	for w := 0; w < studioStashDBFetchWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				scenes, err := sdb.QueryAllScenes(ctx, stashdb.SceneQuery{
					StudioIDs: []string{id},
					PerPage:   100,
				}, studioSceneHardCap)
				if err != nil {
					log.Warn("stashdb studio scenes query failed", "studio", id, "err", err)
					mu.Lock()
					queryErrors++
					mu.Unlock()
					continue
				}
				// Populate the persistent scene cache (additive — feeds the
				// studio page; the scene's studio_id lands on the row). The
				// studio pass catches scenes featuring no OWNED performer, which
				// the performer pass never sees. Non-fatal.
				if err := UpsertSceneBatch(ctx, db, scenes, start); err != nil {
					log.Warn("studio scene cache upsert failed", "studio", id, "err", err)
				}
				agg := aggUpdate{stashdbID: id, totalScenes: len(scenes)}
				for _, s := range scenes {
					ts := parseStashDBDate(s.Date)
					if ts > agg.lastReleaseUnix {
						agg.lastReleaseUnix = ts
					}
					if ownedSet[s.ID] {
						agg.ownedCount++
					}
				}
				mu.Lock()
				aggUpdates = append(aggUpdates, agg)
				mu.Unlock()
			}
		}()
	}
	for _, id := range studioIDs {
		jobs <- id
	}
	close(jobs)
	wg.Wait()

	if len(studioIDs) > 0 && queryErrors*2 > len(studioIDs) {
		return fmt.Errorf("studio aggregate refresh aborted: %d/%d studio queries failed, keeping previous cache",
			queryErrors, len(studioIDs))
	}

	// ── DB writes (single tx) ─────────────────────────────────────────
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Synthetic (cross-id-less) studios can never receive an update — zero
	// them. Real studios that queried successfully get overwritten below; a
	// real studio whose query FAILED keeps its previous values (mirrors the
	// performer pass's failed-minority handling).
	if _, err := tx.ExecContext(ctx, `
		UPDATE studio_cache
		   SET total_stashdb_scenes = 0, owned_scenes_count = 0, last_release_unix = 0
		 WHERE stashdb_id LIKE 'stash:%'
	`); err != nil {
		return fmt.Errorf("reset synthetic aggregates: %w", err)
	}

	stmt, err := tx.PrepareContext(ctx, `
		UPDATE studio_cache
		   SET total_stashdb_scenes = ?, owned_scenes_count = ?, last_release_unix = ?
		 WHERE stashdb_id = ?
	`)
	if err != nil {
		return fmt.Errorf("prepare agg update: %w", err)
	}
	defer stmt.Close()
	for _, a := range aggUpdates {
		if _, err := stmt.ExecContext(ctx, a.totalScenes, a.ownedCount, a.lastReleaseUnix, a.stashdbID); err != nil {
			return fmt.Errorf("update studio agg %s: %w", a.stashdbID, err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, metaKeyStudioAggregatesRefreshed, fmt.Sprintf("%d", start)); err != nil {
		return fmt.Errorf("update meta: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	log.Info("studio aggregate refresh done",
		"studios", len(studioIDs),
		"studios_with_errors", queryErrors,
		"owned_total", len(ownedSet),
		"elapsed", time.Since(time.Unix(start, 0)))
	return nil
}
