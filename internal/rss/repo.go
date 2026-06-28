// Package rss persists the RSS-sync watermark: the most-recent release
// publishDate (unix) seen per indexer, so each sync only matches uploads newer
// than what it has already processed. The matching/flip logic lives in the api
// package (RunRSSLoop); this is just the thin state store, mirroring the
// watches/grabs repo pattern.
package rss

import (
	"context"
	"database/sql"
)

// Repo reads and advances the per-indexer RSS watermark (rss_sync_state).
type Repo struct {
	db *sql.DB
}

// NewRepo wraps a DB handle. The rss_sync_state table is created by schema.sql.
func NewRepo(db *sql.DB) *Repo {
	return &Repo{db: db}
}

// Watermarks returns the per-indexer high-watermark (max release publish unix
// already processed), keyed by Prowlarr indexer id. A missing indexer means
// "never synced" (the caller treats it as a first-run bounded lookback).
func (r *Repo) Watermarks(ctx context.Context) (map[int]int64, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT indexer_id, last_publish_unix FROM rss_sync_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]int64{}
	for rows.Next() {
		var id int
		var ts int64
		if err := rows.Scan(&id, &ts); err != nil {
			return nil, err
		}
		out[id] = ts
	}
	return out, rows.Err()
}

// Advance raises an indexer's watermark to publishUnix. It never lowers it
// (MAX guard), so an out-of-order or duplicate tick can't rewind progress.
func (r *Repo) Advance(ctx context.Context, indexerID int, publishUnix int64) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO rss_sync_state (indexer_id, last_publish_unix)
		VALUES (?, ?)
		ON CONFLICT(indexer_id) DO UPDATE SET
		  last_publish_unix = MAX(last_publish_unix, excluded.last_publish_unix)`,
		indexerID, publishUnix)
	return err
}
