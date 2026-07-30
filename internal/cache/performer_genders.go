package cache

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// GenderBackfillLimit bounds one pass. The pass runs on the poller's cadence,
// so an instance with a large backlog converges over a few passes instead of
// spending one long stretch inside StashDB's request budget.
const GenderBackfillLimit = 400

// PerformersMissingGender returns StashDB performer ids that appear on a scene
// in the DISCOVER window, are NOT in the local library, and have no gender
// recorded yet.
//
// Owned performers are excluded because performer_cache already carries their
// gender from Stash — asking StashDB again would be a second, slower answer to
// a question already answered.
//
// The join to recent_scene_cache is what makes this affordable, and it is not
// an optimisation so much as the correct scope. The persistent stashdb_scene
// cache holds 576k scenes and 50,420 distinct performers on the reference
// instance; the Discover feed those pills actually appear in draws from
// recent_scene_cache, which narrows the same question to 1,014. Asking about
// the other 49k would spend ten hours of StashDB budget resolving genders for
// performers who are never rendered.
func PerformersMissingGender(ctx context.Context, db *sql.DB, limit int) ([]string, error) {
	if limit <= 0 {
		limit = GenderBackfillLimit
	}
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT sp.performer_stashdb_id
		FROM scene_performer sp
		JOIN recent_scene_cache rc
		  ON rc.stashdb_id = sp.scene_id
		LEFT JOIN stashdb_performer_gender g
		  ON g.stashdb_id = sp.performer_stashdb_id
		LEFT JOIN performer_cache pc
		  ON pc.stashdb_id = sp.performer_stashdb_id
		WHERE sp.performer_stashdb_id <> ''
		  AND g.stashdb_id IS NULL
		  AND pc.stashdb_id IS NULL
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// StoreGenders records fetched genders. An empty gender is stored, not
// skipped: it means "asked, and the answer was nothing", and re-asking every
// pass for a performer StashDB has no gender for would never converge.
func StoreGenders(ctx context.Context, db *sql.DB, genders map[string]string, now int64) error {
	if len(genders) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for id, g := range genders {
		if id == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO stashdb_performer_gender (stashdb_id, gender, fetched_at)
			VALUES (?, ?, ?)
			ON CONFLICT(stashdb_id) DO UPDATE SET
			  gender = excluded.gender, fetched_at = excluded.fetched_at`,
			id, strings.ToUpper(strings.TrimSpace(g)), now); err != nil {
			return fmt.Errorf("store gender %s: %w", id, err)
		}
	}
	return tx.Commit()
}

// GendersByStashDBID loads the recorded genders for the given ids. Ids with no
// row are simply absent from the map, which callers must treat as unknown.
func GendersByStashDBID(ctx context.Context, db *sql.DB, ids []string) (map[string]string, error) {
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	// Chunked so a feed referencing thousands of performers cannot exceed
	// SQLite's variable limit (999 by default).
	const chunk = 400
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		part := ids[start:end]
		ph := strings.TrimSuffix(strings.Repeat("?,", len(part)), ",")
		args := make([]any, len(part))
		for i, id := range part {
			args[i] = id
		}
		rows, err := db.QueryContext(ctx,
			`SELECT stashdb_id, gender FROM stashdb_performer_gender WHERE stashdb_id IN (`+ph+`)`,
			args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, g string
			if err := rows.Scan(&id, &g); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = g
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
