package db

import (
	"database/sql"
	_ "embed"
	"fmt"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

func Open(path string) (*sql.DB, error) {
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	// Serialize to a single connection. WAL permits one writer at a time;
	// the default unbounded pool lets the poller's grab updates, two cache
	// refreshes, and HTTP handlers open concurrent write connections that
	// then race the WAL writer lock. A loser that exceeds busy_timeout(5s)
	// returns "database is locked", which the poller only logs at Warn —
	// silently dropping a grab's status transition. One connection makes
	// writes queue in-process instead, trading a little concurrency for
	// correctness. Reads are cheap and brief, so the serialization cost is
	// negligible at forage's request volume.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	// Migrations FIRST: rename qBit-specific columns to client-agnostic
	// names if we have an older schema. This must precede schema.sql
	// because schema.sql includes CREATE INDEX statements referencing
	// the new column names — they'd fail on a still-renamed table.
	if err := migrateGrabsColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate grabs columns: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

// migrateGrabsColumns brings older grabs tables up to the current
// schema. The 2026-05-26 SABnzbd integration generalised the qBit-
// specific `qbit_hash` / `qbit_name` columns to `client_id` /
// `client_name`, and added a `client` discriminator. CREATE TABLE IF
// NOT EXISTS in schema.sql doesn't update existing tables, so the
// migration has to happen here.
//
// All ALTER steps are guarded by `pragma_table_info` so the function
// is idempotent — running on an already-migrated DB is a no-op.
func migrateGrabsColumns(db *sql.DB) error {
	// Fresh database: no tables exist yet, so there's nothing to migrate —
	// schema.sql (run right after this) creates grabs, performer_cache and
	// recent_scene_cache with every current column already in place. Bail
	// out early; otherwise the column-presence guards below (which read 0
	// from pragma_table_info on a missing table) sail past the renames and
	// run `ALTER TABLE grabs ADD COLUMN ...` against a table that doesn't
	// exist — breaking first boot for every new install.
	var grabsExists int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='grabs'`,
	).Scan(&grabsExists); err != nil {
		return err
	}
	if grabsExists == 0 {
		return nil
	}

	has := func(col string) (bool, error) {
		var n int
		err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('grabs') WHERE name = ?`, col,
		).Scan(&n)
		if err != nil {
			return false, err
		}
		return n > 0, nil
	}

	hadQbitHash, err := has("qbit_hash")
	if err != nil {
		return err
	}
	if hadQbitHash {
		if _, err := db.Exec(`ALTER TABLE grabs RENAME COLUMN qbit_hash TO client_id`); err != nil {
			return fmt.Errorf("rename qbit_hash: %w", err)
		}
	}
	hadQbitName, err := has("qbit_name")
	if err != nil {
		return err
	}
	if hadQbitName {
		if _, err := db.Exec(`ALTER TABLE grabs RENAME COLUMN qbit_name TO client_name`); err != nil {
			return fmt.Errorf("rename qbit_name: %w", err)
		}
	}
	hasClient, err := has("client")
	if err != nil {
		return err
	}
	if !hasClient {
		if _, err := db.Exec(`ALTER TABLE grabs ADD COLUMN client TEXT NOT NULL DEFAULT 'qbit'`); err != nil {
			return fmt.Errorf("add client column: %w", err)
		}
	}
	// Old index referenced the renamed column; drop it. CREATE in
	// schema.sql will rebuild as idx_grabs_client_id.
	if _, err := db.Exec(`DROP INDEX IF EXISTS idx_grabs_qbit_hash`); err != nil {
		return fmt.Errorf("drop old qbit_hash index: %w", err)
	}

	// 2026-05-26 placement migration: forage now owns final file
	// placement (hardlink into <library_root>/<performer>/), so the
	// grab row tracks the destination + any placement failure.
	placementCols := []struct{ col, decl string }{
		{"performer_name", `ALTER TABLE grabs ADD COLUMN performer_name TEXT`},
		{"placed_path", `ALTER TABLE grabs ADD COLUMN placed_path TEXT`},
		{"place_error", `ALTER TABLE grabs ADD COLUMN place_error TEXT`},
		{"placed_at", `ALTER TABLE grabs ADD COLUMN placed_at INTEGER`},
	}
	for _, c := range placementCols {
		exists, err := has(c.col)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := db.Exec(c.decl); err != nil {
				return fmt.Errorf("add %s column: %w", c.col, err)
			}
		}
	}

	// 2026-05-29 pack migration: performer-pack grabs (one release →
	// many scenes) track a kind discriminator + progress counters.
	packCols := []struct{ col, decl string }{
		{"kind", `ALTER TABLE grabs ADD COLUMN kind TEXT NOT NULL DEFAULT 'single'`},
		{"pack_files", `ALTER TABLE grabs ADD COLUMN pack_files INTEGER NOT NULL DEFAULT 0`},
		{"pack_identified", `ALTER TABLE grabs ADD COLUMN pack_identified INTEGER NOT NULL DEFAULT 0`},
		{"pack_deduped", `ALTER TABLE grabs ADD COLUMN pack_deduped INTEGER NOT NULL DEFAULT 0`},
	}
	for _, c := range packCols {
		exists, err := has(c.col)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := db.Exec(c.decl); err != nil {
				return fmt.Errorf("add %s column: %w", c.col, err)
			}
		}
	}

	// 2026-05-31 stalled-detection migration: track download progress so
	// the poller can flag a grab that's made no progress for a while.
	progressCols := []struct{ col, decl string }{
		{"progress", `ALTER TABLE grabs ADD COLUMN progress REAL NOT NULL DEFAULT 0`},
		{"progress_at", `ALTER TABLE grabs ADD COLUMN progress_at INTEGER NOT NULL DEFAULT 0`},
	}
	for _, c := range progressCols {
		exists, err := has(c.col)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := db.Exec(c.decl); err != nil {
				return fmt.Errorf("add %s column: %w", c.col, err)
			}
		}
	}

	// 2026-06-02 optimistic-lock migration: a monotonic rev, bumped on every
	// Update, used as a compare-and-set guard so the poller tick can't
	// clobber a concurrent API write (performer reassign, manual match) it
	// held a stale snapshot across — and vice versa.
	revCols := []struct{ col, decl string }{
		{"rev", `ALTER TABLE grabs ADD COLUMN rev INTEGER NOT NULL DEFAULT 0`},
	}
	for _, c := range revCols {
		exists, err := has(c.col)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := db.Exec(c.decl); err != nil {
				return fmt.Errorf("add %s column: %w", c.col, err)
			}
		}
	}

	// 2026-07-12 deferred-retry migration: grabs whose add failed
	// transiently (client unreachable, indexer 5xx) park in
	// status='deferred' and are re-driven with exponential backoff;
	// attempts counts failed adds, next_retry_at gates the retry loop.
	retryCols := []struct{ col, decl string }{
		{"attempts", `ALTER TABLE grabs ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0`},
		{"next_retry_at", `ALTER TABLE grabs ADD COLUMN next_retry_at INTEGER`},
		// fail_kind ('indexer' | 'client') records which stage the deferred
		// failure happened in, so the retry loop knows whether an indexer
		// failover is worth attempting.
		{"fail_kind", `ALTER TABLE grabs ADD COLUMN fail_kind TEXT`},
	}
	for _, c := range retryCols {
		exists, err := has(c.col)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := db.Exec(c.decl); err != nil {
				return fmt.Errorf("add %s column: %w", c.col, err)
			}
		}
	}

	// 2026-05-26 discover migration: per-performer aggregates that
	// power /performers sort=last_release|missing_count. The new
	// recent_scene_cache table is created by schema.sql itself
	// (CREATE TABLE IF NOT EXISTS — no migration needed there).
	performerHas := func(col string) (bool, error) {
		var n int
		err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('performer_cache') WHERE name = ?`, col,
		).Scan(&n)
		if err != nil {
			return false, err
		}
		return n > 0, nil
	}
	aggCols := []struct{ col, decl string }{
		{"total_stashdb_scenes", `ALTER TABLE performer_cache ADD COLUMN total_stashdb_scenes INTEGER NOT NULL DEFAULT 0`},
		{"owned_scenes_count", `ALTER TABLE performer_cache ADD COLUMN owned_scenes_count INTEGER NOT NULL DEFAULT 0`},
		{"last_release_unix", `ALTER TABLE performer_cache ADD COLUMN last_release_unix INTEGER NOT NULL DEFAULT 0`},
	}
	for _, c := range aggCols {
		exists, err := performerHas(c.col)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := db.Exec(c.decl); err != nil {
				return fmt.Errorf("add performer_cache %s column: %w", c.col, err)
			}
		}
	}

	// 2026-05-26 trending column on recent_scene_cache. Idempotent so
	// re-applies on the next boot are a no-op. recent_scene_cache
	// itself is created by schema.sql (CREATE IF NOT EXISTS); we only
	// need this guard if the table existed before trending shipped.
	sceneHas := func(col string) (bool, error) {
		var n int
		err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('recent_scene_cache') WHERE name = ?`, col,
		).Scan(&n)
		if err != nil {
			return false, err
		}
		return n > 0, nil
	}
	if exists, err := sceneHas("trending_rank"); err != nil {
		return err
	} else if !exists {
		// Only ALTER when the table itself exists — otherwise schema.sql
		// will create it fresh with the column already in place.
		var tableExists int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='recent_scene_cache'`).Scan(&tableExists); err != nil {
			return err
		}
		if tableExists > 0 {
			if _, err := db.Exec(`ALTER TABLE recent_scene_cache ADD COLUMN trending_rank INTEGER NOT NULL DEFAULT 0`); err != nil {
				return fmt.Errorf("add trending_rank column: %w", err)
			}
		}
	}

	// 2026-06-01 watch-dismiss: ignored_urls holds the download URLs the
	// user has dismissed for a watch (JSON array). A dismissed release is
	// skipped by the watch loop so the same dead/over-compressed find can't
	// re-surface. Guard on the watches table existing (schema.sql creates it
	// fresh with the column already present on a new DB).
	var watchesExists int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='watches'`).Scan(&watchesExists); err != nil {
		return err
	}
	if watchesExists > 0 {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('watches') WHERE name = 'ignored_urls'`).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.Exec(`ALTER TABLE watches ADD COLUMN ignored_urls TEXT NOT NULL DEFAULT '[]'`); err != nil {
				return fmt.Errorf("add watches ignored_urls column: %w", err)
			}
		}

		// 2026-06-23 watch batches + candidate re-pick + persist-through-grab:
		// batch_id/batch_label group watches launched together (the Watching
		// tab groups by them); candidates stores the full verified release list
		// captured when a watch flips available (so the user can re-pick a
		// different release); grabbed_at records when a watch was grabbed
		// (status='grabbed' — a grabbed watch lingers instead of being deleted
		// so batch progress reads correctly). status gaining the 'grabbed'
		// value needs no DDL. Add each column only if missing; schema.sql
		// creates them on a fresh DB, and the idx_watches_batch index.
		watchCols := []struct{ name, ddl string }{
			{"batch_id", `ALTER TABLE watches ADD COLUMN batch_id TEXT NOT NULL DEFAULT ''`},
			{"batch_label", `ALTER TABLE watches ADD COLUMN batch_label TEXT NOT NULL DEFAULT ''`},
			{"candidates", `ALTER TABLE watches ADD COLUMN candidates TEXT NOT NULL DEFAULT '[]'`},
			{"grabbed_at", `ALTER TABLE watches ADD COLUMN grabbed_at INTEGER NOT NULL DEFAULT 0`},
			{"performers", `ALTER TABLE watches ADD COLUMN performers TEXT NOT NULL DEFAULT '[]'`},
			{"search_count", `ALTER TABLE watches ADD COLUMN search_count INTEGER NOT NULL DEFAULT 0`},
		}
		for _, c := range watchCols {
			var has int
			if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('watches') WHERE name = ?`, c.name).Scan(&has); err != nil {
				return err
			}
			if has == 0 {
				if _, err := db.Exec(c.ddl); err != nil {
					return fmt.Errorf("add watches %s column: %w", c.name, err)
				}
			}
		}
	}

	// 2026-06-23 studios page: studio_cache gains the local stash_id +
	// favorite/scene_count + the same total/owned/last_release aggregates
	// performer_cache has, so it can drive the /studios list. The matcher only
	// reads name/aliases, so these are additive. Guard on the table existing
	// (schema.sql creates them fresh on a new DB, plus the idx_studio_* indexes
	// which run after this migration).
	var studioCacheExists int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='studio_cache'`).Scan(&studioCacheExists); err != nil {
		return err
	}
	if studioCacheExists > 0 {
		studioCols := []struct{ name, ddl string }{
			{"stash_id", `ALTER TABLE studio_cache ADD COLUMN stash_id TEXT`},
			{"favorite", `ALTER TABLE studio_cache ADD COLUMN favorite INTEGER NOT NULL DEFAULT 0`},
			{"scene_count", `ALTER TABLE studio_cache ADD COLUMN scene_count INTEGER NOT NULL DEFAULT 0`},
			{"total_stashdb_scenes", `ALTER TABLE studio_cache ADD COLUMN total_stashdb_scenes INTEGER NOT NULL DEFAULT 0`},
			{"owned_scenes_count", `ALTER TABLE studio_cache ADD COLUMN owned_scenes_count INTEGER NOT NULL DEFAULT 0`},
			{"last_release_unix", `ALTER TABLE studio_cache ADD COLUMN last_release_unix INTEGER NOT NULL DEFAULT 0`},
		}
		for _, c := range studioCols {
			var has int
			if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('studio_cache') WHERE name = ?`, c.name).Scan(&has); err != nil {
				return err
			}
			if has == 0 {
				if _, err := db.Exec(c.ddl); err != nil {
					return fmt.Errorf("add studio_cache %s column: %w", c.name, err)
				}
			}
		}
	}

	// 2026-06-24 persistent StashDB scene cache: the per-subject delta-sync
	// watermark. The stashdb_scene / scene_performer tables are created fresh by
	// schema.sql (CREATE TABLE IF NOT EXISTS); only these columns need an ALTER
	// on existing performer_cache/studio_cache. Guarded on the table existing.
	for _, tbl := range []string{"performer_cache", "studio_cache"} {
		var exists int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			continue
		}
		var has int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(` + quoteIdent(tbl) + `) WHERE name = 'scenes_synced_at'`).Scan(&has); err != nil {
			return err
		}
		if has == 0 {
			if _, err := db.Exec(`ALTER TABLE ` + tbl + ` ADD COLUMN scenes_synced_at INTEGER NOT NULL DEFAULT 0`); err != nil {
				return fmt.Errorf("add %s scenes_synced_at column: %w", tbl, err)
			}
		}
	}
	return nil
}

// quoteIdent wraps a trusted (hardcoded) table name in single quotes for use
// inside pragma_table_info(...). Only ever called with literal table names from
// this file, never user input.
func quoteIdent(s string) string { return "'" + s + "'" }
