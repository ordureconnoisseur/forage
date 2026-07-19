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

const metaKeyPerformerRefreshed = "performer_refreshed_at"

// RefreshPerformers pulls every performer from local Stash and upserts
// them into performer_cache. Rows whose refreshed_at predates this
// pass's start timestamp are deleted — that's how performers removed
// from Stash drop out of our cache.
//
// For performers with a StashDB cross-id, the alias list is enriched
// with StashDB's canonical name + aliases. Local records routinely
// carry a variant spelling with an empty alias list ("Summer Cline"
// locally, "Summer Kline" on StashDB and in release names), and the
// release-token detector can only see what this cache holds.
//
// sdb may be nil — when so, the StashDB enrichment is skipped and the
// cache holds the Stash-side aliases only.
func RefreshPerformers(ctx context.Context, sc *stash.Client, sdb *stashdb.Client, db *sql.DB, log *slog.Logger) error {
	start := time.Now().Unix()
	log.Info("performer refresh starting")

	performers, err := sc.FindPerformers(ctx)
	if err != nil {
		return fmt.Errorf("fetch performers: %w", err)
	}

	stashdbPerformers := map[string]stashdb.Performer{}
	if sdb != nil {
		var ids []string
		for _, p := range performers {
			if id := stash.PickStashDBID(p.StashIDs); id != "" {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			t0 := time.Now()
			fetched, err := sdb.FindPerformersByID(ctx, ids)
			if err != nil {
				log.Warn("stashdb FindPerformersByID failed; alias enrichment skipped", "err", err)
			} else {
				stashdbPerformers = fetched
				log.Info("stashdb performers fetched", "count", len(fetched), "elapsed", time.Since(t0))
			}
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO performer_cache (stash_id, stashdb_id, name, aliases, favorite, gender, scene_count, refreshed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(stash_id) DO UPDATE SET
			stashdb_id   = excluded.stashdb_id,
			name         = excluded.name,
			aliases      = excluded.aliases,
			favorite     = excluded.favorite,
			gender       = excluded.gender,
			scene_count  = excluded.scene_count,
			refreshed_at = excluded.refreshed_at
	`)
	if err != nil {
		return fmt.Errorf("prepare upsert: %w", err)
	}
	defer stmt.Close()

	enriched := 0
	for _, p := range performers {
		crossID := stash.PickStashDBID(p.StashIDs)
		// Start with what local Stash returned; union in StashDB's
		// canonical name + aliases when we have a cross-id match. The
		// canonical name matters as much as the aliases — it's the
		// spelling release names use when the local record diverges.
		aliases := mergeAliases(p.AliasList)
		if sdbP, ok := stashdbPerformers[crossID]; ok {
			extra := make([]string, 0, 1+len(sdbP.Aliases))
			if sdbP.Name != "" && !strings.EqualFold(sdbP.Name, p.Name) {
				extra = append(extra, sdbP.Name)
			}
			for _, a := range sdbP.Aliases {
				if !strings.EqualFold(a, p.Name) {
					extra = append(extra, a)
				}
			}
			if len(extra) > 0 {
				aliases = mergeAliases(aliases, extra...)
				enriched++
			}
		}
		aliasesJSON, _ := json.Marshal(aliases)
		fav := 0
		if p.Favorite {
			fav = 1
		}
		if _, err := stmt.ExecContext(ctx,
			p.ID,
			crossID,
			p.Name,
			string(aliasesJSON),
			fav,
			p.Gender,
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
		"enriched_from_stashdb", enriched,
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
