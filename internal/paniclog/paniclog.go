// Package paniclog persists the most recent recovered background panic.
//
// Every loop in the daemon (poller ticks, watch/rss/notify loops, cache
// refreshes) recovers panics so one bad response can't take the process
// down — which also means a nightly crash-loop is invisible unless someone
// happens to read the logs. Persisting the last one (meta table, part of
// the precious backup set) lets /healthz badge it and the diagnostics
// bundle carry the stack, surviving restarts.
package paniclog

import (
	"database/sql"
	"encoding/json"
	"time"
)

const metaKey = "last_panic"

// Entry is one recovered panic. Value is fmt-rendered (the panic payload
// is rarely more structured than a string or error), Stack the full
// debug.Stack at recovery time.
type Entry struct {
	At    int64  `json:"at"` // unix seconds
	In    string `json:"in"` // which loop/goroutine, e.g. "poller tick"
	Value string `json:"value"`
	Stack string `json:"stack"`
}

// Record stores the panic as the most recent one. Best-effort by design:
// it runs inside a recover() path where a second failure must be
// swallowed, not propagated — the panic is already in the log either way.
func Record(db *sql.DB, in, value string, stack []byte) {
	if db == nil {
		return
	}
	b, err := json.Marshal(Entry{
		At:    time.Now().Unix(),
		In:    in,
		Value: value,
		Stack: string(stack),
	})
	if err != nil {
		return
	}
	_, _ = db.Exec(`
INSERT INTO meta (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`, metaKey, string(b))
}

// Last returns the most recent recorded panic, or nil when none has ever
// been recorded (or the row is unreadable — this is telemetry, not state).
func Last(db *sql.DB) *Entry {
	if db == nil {
		return nil
	}
	var raw string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = ?`, metaKey).Scan(&raw); err != nil {
		return nil
	}
	var e Entry
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		return nil
	}
	return &e
}
