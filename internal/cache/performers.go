package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/ordureconnoisseur/forager/internal/stash"
)

const metaKeyPerformerRefreshed = "performer_refreshed_at"

// RefreshPerformers pulls every performer from local Stash and upserts
// them into performer_cache. Rows whose refreshed_at predates this
// pass's start timestamp are deleted — that's how performers removed
// from Stash drop out of our cache.
func RefreshPerformers(ctx context.Context, sc *stash.Client, db *sql.DB, log *slog.Logger) error {
	start := time.Now().Unix()
	log.Info("performer refresh starting")

	performers, err := sc.FindPerformers(ctx)
	if err != nil {
		return fmt.Errorf("fetch performers: %w", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO performer_cache (stash_id, stashdb_id, name, aliases, favorite, scene_count, refreshed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(stash_id) DO UPDATE SET
			stashdb_id   = excluded.stashdb_id,
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

	for _, p := range performers {
		aliasesJSON, _ := json.Marshal(p.AliasList)
		fav := 0
		if p.Favorite {
			fav = 1
		}
		if _, err := stmt.ExecContext(ctx,
			p.ID,
			stash.PickStashDBID(p.StashIDs),
			p.Name,
			string(aliasesJSON),
			fav,
			p.SceneCount,
			start,
		); err != nil {
			return fmt.Errorf("upsert performer %s (%s): %w", p.ID, p.Name, err)
		}
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM performer_cache WHERE refreshed_at < ?`, start)
	if err != nil {
		return fmt.Errorf("delete stale: %w", err)
	}
	deleted, _ := res.RowsAffected()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, metaKeyPerformerRefreshed, fmt.Sprintf("%d", start)); err != nil {
		return fmt.Errorf("update meta: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	log.Info("performer refresh done",
		"upserted", len(performers),
		"deleted", deleted,
		"elapsed", time.Since(time.Unix(start, 0)))
	return nil
}

// PerformerRefreshedAt reads the last successful refresh timestamp (unix
// seconds). Returns 0 if the cache has never been populated.
func PerformerRefreshedAt(ctx context.Context, db *sql.DB) (int64, error) {
	var s string
	err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, metaKeyPerformerRefreshed).Scan(&s)
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
