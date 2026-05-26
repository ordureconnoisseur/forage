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

const metaKeyStudioRefreshed = "studio_refreshed_at"

// RefreshStudios pulls every studio from local Stash. Studios in Stash
// already include alias_list (sourced from StashDB/FansDB on scrape),
// which is what the matcher will scan against — no separate StashDB
// pull is needed in this phase.
func RefreshStudios(ctx context.Context, sc *stash.Client, db *sql.DB, log *slog.Logger) error {
	start := time.Now().Unix()
	log.Info("studio refresh starting")

	studios, err := sc.FindStudios(ctx)
	if err != nil {
		return fmt.Errorf("fetch studios: %w", err)
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
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO studio_cache (stashdb_id, name, aliases, refreshed_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(stashdb_id) DO UPDATE SET
			name         = excluded.name,
			aliases      = excluded.aliases,
			refreshed_at = excluded.refreshed_at
	`)
	if err != nil {
		return fmt.Errorf("prepare upsert: %w", err)
	}
	defer stmt.Close()

	upserted := 0
	for _, s := range studios {
		key := stash.PickStashDBID(s.StashIDs)
		if key == "" {
			key = "stash:" + s.ID
		}
		aliasesJSON, _ := json.Marshal(s.Aliases)
		if _, err := stmt.ExecContext(ctx, key, s.Name, string(aliasesJSON), start); err != nil {
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
		"deleted", deleted,
		"elapsed", time.Since(time.Unix(start, 0)))
	return nil
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
