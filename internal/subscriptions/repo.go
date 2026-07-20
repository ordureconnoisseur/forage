// Package subscriptions persists permanent performer/studio watches.
// A subscription is standing intent ("get everything new from X"); the
// api layer's subscription loop turns that intent into ordinary scene
// watches, so this package stays a thin repo like grabs/watches.
package subscriptions

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// BatchPrefix namespaces the watch batches a subscription creates
// ("sub:<stashdb_id>"), letting the UI and the auto-grab sweep find a
// subscription's watches without a join table.
const BatchPrefix = "sub:"

type Subscription struct {
	StashDBID string `json:"stashdb_id"`
	Kind      string `json:"kind"` // "performer" | "studio"
	Name      string `json:"name"`
	// LocalID is the subject's LOCAL Stash id (performer/studio). The
	// loop only needs the StashDB cross-id, but the plugin navigates its
	// subject pages and proxies portrait images by local id.
	LocalID     string `json:"local_id,omitempty"`
	ImageURL    string `json:"image_url,omitempty"`
	AutoGrab    bool   `json:"auto_grab"`
	CreatedAt   int64  `json:"created_at"`
	LastChecked int64  `json:"last_checked"`
	// Watermark is the newest StashDB release date (unix, day-granular)
	// the loop has already processed; only scenes dated >= watermark are
	// candidates (dedup against existing watches/grabs absorbs the same-
	// day overlap the >= comparison allows).
	Watermark int64 `json:"watermark"`
	// SeenTotal mirrors the subject's total-scene aggregate at the last
	// processed pass; combined with Watermark it forms the change signal
	// (same-day additions move the count but not the newest date).
	SeenTotal int `json:"seen_total"`
	// LastSeenAt is when the user last looked at this subscription's rail
	// card; batch activity after it renders as the "new" badge.
	LastSeenAt int64 `json:"last_seen_at"`
}

// BatchID returns the watch-batch id this subscription's watches carry.
func (s Subscription) BatchID() string { return BatchPrefix + s.StashDBID }

type Repo struct {
	db *sql.DB
}

func NewRepo(db *sql.DB) *Repo { return &Repo{db: db} }

// Add creates (or re-activates) a subscription. The watermark starts at
// the beginning of today UTC: today's releases count, the backlog does
// not (the UI offers watch-all-missing for that).
func (r *Repo) Add(ctx context.Context, s Subscription) error {
	if s.StashDBID == "" || s.Name == "" {
		return fmt.Errorf("subscriptions: stashdb_id and name required")
	}
	if s.Kind == "" {
		s.Kind = "performer"
	}
	now := time.Now()
	watermark := now.UTC().Truncate(24 * time.Hour).Unix()
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO subscriptions (stashdb_id, kind, name, local_id, image_url, auto_grab, created_at, watermark)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(stashdb_id) DO UPDATE SET
		  name = excluded.name, local_id = excluded.local_id, image_url = excluded.image_url`,
		s.StashDBID, s.Kind, s.Name, s.LocalID, nullString(s.ImageURL),
		boolInt(s.AutoGrab), now.Unix(), watermark)
	return err
}

// Rekey moves a subscription to a new primary key, preserving its
// watermark/auto-grab state: the self-heal for rows created with a
// LOCAL Stash id where the StashDB cross-id belongs (the loop reads
// aggregates and scene lists by cross-id, so those rows never fired).
// If a subscription already exists under the new id, the stale row is
// simply dropped instead.
func (r *Repo) Rekey(ctx context.Context, oldID, newID, localID string) error {
	if oldID == newID {
		// Same key: just backfill the local id.
		_, err := r.db.ExecContext(ctx,
			`UPDATE subscriptions SET local_id = ? WHERE stashdb_id = ?`, localID, oldID)
		return err
	}
	var exists int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM subscriptions WHERE stashdb_id = ?`, newID).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		_, err := r.db.ExecContext(ctx, `DELETE FROM subscriptions WHERE stashdb_id = ?`, oldID)
		return err
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE subscriptions SET stashdb_id = ?, local_id = ? WHERE stashdb_id = ?`,
		newID, localID, oldID)
	return err
}

// Remove deletes a subscription. The watches it created live on (they
// are ordinary watches the user may still want); only the standing
// intent is retired.
func (r *Repo) Remove(ctx context.Context, stashdbID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM subscriptions WHERE stashdb_id = ?`, stashdbID)
	return err
}

// List returns every subscription, newest first.
func (r *Repo) List(ctx context.Context) ([]Subscription, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT stashdb_id, kind, name, COALESCE(local_id, ''), COALESCE(image_url, ''), auto_grab,
		       created_at, last_checked, watermark, seen_total, last_seen_at
		FROM subscriptions ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subscription
	for rows.Next() {
		var s Subscription
		var auto int
		if err := rows.Scan(&s.StashDBID, &s.Kind, &s.Name, &s.LocalID, &s.ImageURL, &auto,
			&s.CreatedAt, &s.LastChecked, &s.Watermark, &s.SeenTotal, &s.LastSeenAt); err != nil {
			return nil, err
		}
		s.AutoGrab = auto != 0
		out = append(out, s)
	}
	return out, rows.Err()
}

// SetAutoGrab flips one subscription's hands-free grabbing.
func (r *Repo) SetAutoGrab(ctx context.Context, stashdbID string, enabled bool) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE subscriptions SET auto_grab = ? WHERE stashdb_id = ?`, boolInt(enabled), stashdbID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// MarkSeen stamps the badge watermark: activity before now is no longer
// "new" on the rail card.
func (r *Repo) MarkSeen(ctx context.Context, stashdbID string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE subscriptions SET last_seen_at = ? WHERE stashdb_id = ?`, time.Now().Unix(), stashdbID)
	return err
}

// AdvanceCheck records a completed loop pass: last_checked always, the
// watermark only forward (never backward, so a partial scene list from a
// flaky fetch can't reopen already-processed days), and the seen total.
func (r *Repo) AdvanceCheck(ctx context.Context, stashdbID string, watermark int64, seenTotal int) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE subscriptions
		SET last_checked = ?, watermark = MAX(watermark, ?), seen_total = ?
		WHERE stashdb_id = ?`, time.Now().Unix(), watermark, seenTotal, stashdbID)
	return err
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
