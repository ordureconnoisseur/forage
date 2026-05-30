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
	return nil
}
