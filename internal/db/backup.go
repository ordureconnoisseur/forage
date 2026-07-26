package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"
)

// preciousTables is the state that cannot be rebuilt from Stash or StashDB.
// The rest of the database (performer/studio/scene caches) is exactly that —
// cache — and re-syncs on its own, so backing it up would multiply a
// ~740 MB file for nothing. This set is a few MB:
//
//	grabs           — the acquisition history and every lifecycle stamp
//	watches         — scene watches, candidates, ignore lists
//	subscriptions   — permanent performer/studio watches + watermarks
//	pack_duplicate  — pending review decisions
//	destruction_log — the audit journal (its whole point is surviving)
//	rss_sync_state  — RSS watermarks (cheap, avoids a replay burst)
//	meta            — the session signing key, so a restore doesn't log
//	                  every device out
var preciousTables = []string{
	"grabs", "watches", "subscriptions", "pack_duplicate",
	"destruction_log", "rss_sync_state", "meta",
}

// BackupPrecious writes the irreplaceable tables into a standalone SQLite
// file at path, atomically (tmp + rename), rotating path.1 and path.2
// behind it. Runs through the caller's own (serialised) connection pool, so
// it cannot race the WAL writer the way an external sqlite3 process would.
//
// The result is a complete, openable database restorable with:
//
//	sqlite3 forager.db ".param set @bak <path>" — or simply ATTACH it and
//	INSERT INTO main.<t> SELECT * FROM bak.<t> per table.
func BackupPrecious(ctx context.Context, dbh *sql.DB, path string) error {
	tmp := path + ".tmp"
	_ = os.Remove(tmp)

	// One connection for the whole ATTACH…DETACH dance: ATTACH is
	// per-connection state, and the pool is capped at one connection anyway
	// (see Open), but being explicit keeps this correct if that ever changes.
	conn, err := dbh.Conn(ctx)
	if err != nil {
		return fmt.Errorf("backup conn: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS bak`, tmp); err != nil {
		return fmt.Errorf("attach backup: %w", err)
	}
	detach := func() { _, _ = conn.ExecContext(ctx, `DETACH DATABASE bak`) }

	for _, t := range preciousTables {
		// Quote defensively even though the list is a compile-time constant.
		if strings.ContainsAny(t, `"'`+"`") {
			detach()
			return fmt.Errorf("suspicious table name %q", t)
		}
		if _, err := conn.ExecContext(ctx,
			fmt.Sprintf(`CREATE TABLE bak.%q AS SELECT * FROM main.%q`, t, t)); err != nil {
			detach()
			_ = os.Remove(tmp)
			return fmt.Errorf("backup %s: %w", t, err)
		}
	}
	detach()

	// Rotate previous generations, newest first: path -> .1 -> .2.
	_ = os.Remove(path + ".2")
	_ = os.Rename(path+".1", path+".2")
	_ = os.Rename(path, path+".1")
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("finalize backup: %w", err)
	}
	return nil
}

// BackupInterval is how often RunPeriodicBackups writes a fresh snapshot.
const BackupInterval = 24 * time.Hour

// RunPeriodicBackups writes one snapshot immediately (so a fresh install is
// covered from day one, and every daemon start refreshes generation .1),
// then every BackupInterval until ctx is cancelled. Failures are logged by
// the caller-provided report func and never stop the loop.
func RunPeriodicBackups(ctx context.Context, dbh *sql.DB, path string, report func(error)) {
	tick := func() {
		if err := BackupPrecious(ctx, dbh, path); err != nil && report != nil {
			report(err)
		}
	}
	tick()
	t := time.NewTicker(BackupInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}
