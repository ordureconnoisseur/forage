package matcher

import (
	"context"
	"database/sql"
	"encoding/json"
)

// LoadStudios reads every row of studio_cache and returns them as
// matcher.Entity values keyed by stashdb_id (the column used as PK).
// Studios with no aliases column are returned with an empty Aliases
// slice rather than a nil JSON.
func LoadStudios(ctx context.Context, db *sql.DB) ([]Entity, error) {
	rows, err := db.QueryContext(ctx, `SELECT stashdb_id, name, aliases FROM studio_cache`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEntityRows(rows)
}

// LoadPerformers reads every row of performer_cache. Each Entity.ID is
// the LOCAL Stash performer ID — the same key scenes' performers[].id
// refer to. (The performer's StashDB cross-id lives in a separate
// column we ignore here.)
func LoadPerformers(ctx context.Context, db *sql.DB) ([]Entity, error) {
	rows, err := db.QueryContext(ctx, `SELECT stash_id, name, aliases FROM performer_cache`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEntityRows(rows)
}

// LoadPerformerStashDBMap returns localStashID → stashdbID for every
// performer in performer_cache that has a StashDB cross-reference.
// Used by the matcher to translate the entity scanner's local IDs
// into the StashDB IDs that queryScenes expects.
func LoadPerformerStashDBMap(ctx context.Context, db *sql.DB) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT stash_id, stashdb_id FROM performer_cache WHERE stashdb_id IS NOT NULL AND stashdb_id != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var local, stashdb string
		if err := rows.Scan(&local, &stashdb); err != nil {
			return nil, err
		}
		out[local] = stashdb
	}
	return out, rows.Err()
}

func scanEntityRows(rows *sql.Rows) ([]Entity, error) {
	var out []Entity
	for rows.Next() {
		var e Entity
		var aliasesJSON sql.NullString
		if err := rows.Scan(&e.ID, &e.Name, &aliasesJSON); err != nil {
			return nil, err
		}
		if aliasesJSON.Valid && aliasesJSON.String != "" && aliasesJSON.String != "null" {
			_ = json.Unmarshal([]byte(aliasesJSON.String), &e.Aliases)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
