package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ordureconnoisseur/forager/internal/stash"
	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

const metaKeyStudioRefreshed = "studio_refreshed_at"

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
