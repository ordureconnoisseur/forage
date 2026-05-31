// Package watches persists user-tracked StashDB scenes. A watch records
// "tell me when this scene has a release at quality target". A background
// loop (internal/api watchTicker) re-searches watching rows and flips
// them to available on a matching verified release; it never grabs.
//
// Repo is thin CRUD + the batch-claim the loop uses to pick which watches
// to re-check this tick (oldest-checked first).
package watches

import (
	"context"
	"database/sql"
	"time"
)

// Quality targets a watch waits for. Exact-match semantics: a 4k release
// does NOT satisfy a Target1080. TargetAny matches any verified release.
const (
	TargetAny  = "any"
	Target720  = "720p"
	Target1080 = "1080p"
	Target4K   = "4k"
)

// Status values.
const (
	StatusWatching  = "watching"
	StatusAvailable = "available"
)

// Watch is the in-memory row.
type Watch struct {
	StashDBID     string `json:"stashdb_id"`
	Title         string `json:"title"`
	Date          string `json:"date,omitempty"`
	StudioName    string `json:"studio_name,omitempty"`
	ImageURL      string `json:"image_url,omitempty"`
	PerformerName string `json:"performer_name,omitempty"`
	PerformerID   string `json:"performer_id,omitempty"`
	Target        string `json:"target"`
	Status        string `json:"status"`
	FoundTitle    string `json:"found_title,omitempty"`
	FoundURL      string `json:"found_url,omitempty"`
	FoundIndexer  string `json:"found_indexer,omitempty"`
	FoundProtocol string `json:"found_protocol,omitempty"`
	FoundSize     int64  `json:"found_size,omitempty"`
	CreatedAt     int64  `json:"created_at"`
	LastChecked   int64  `json:"last_checked"`
	FoundAt       int64  `json:"found_at,omitempty"`
}

type Repo struct{ db *sql.DB }

func NewRepo(db *sql.DB) *Repo { return &Repo{db: db} }

// Add inserts (or replaces) a watch. Re-watching an existing scene resets
// it to watching at the new target.
func (r *Repo) Add(ctx context.Context, w Watch) error {
	if w.Target == "" {
		w.Target = TargetAny
	}
	if w.CreatedAt == 0 {
		w.CreatedAt = time.Now().Unix()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO watches (
		  stashdb_id, title, date, studio_name, image_url,
		  performer_name, performer_id, target, status, created_at, last_checked
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'watching', ?, 0)
		ON CONFLICT(stashdb_id) DO UPDATE SET
		  target = excluded.target,
		  status = 'watching',
		  performer_name = excluded.performer_name,
		  performer_id = excluded.performer_id,
		  found_title = '', found_url = '', found_indexer = '',
		  found_protocol = '', found_size = 0, found_at = 0,
		  last_checked = 0`,
		w.StashDBID, w.Title, w.Date, w.StudioName, w.ImageURL,
		w.PerformerName, w.PerformerID, w.Target, w.CreatedAt)
	return err
}

// Delete removes a watch.
func (r *Repo) Delete(ctx context.Context, stashDBID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM watches WHERE stashdb_id = ?`, stashDBID)
	return err
}

// List returns all watches, available-first then newest.
func (r *Repo) List(ctx context.Context) ([]Watch, error) {
	return r.query(ctx, `
		SELECT `+cols+` FROM watches
		ORDER BY (status = 'available') DESC, created_at DESC`)
}

// IDs returns the set of watched scene ids — for annotating scene lists
// with a "watching/available" badge cheaply.
func (r *Repo) IDs(ctx context.Context) (map[string]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT stashdb_id, status FROM watches`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, st string
		if rows.Scan(&id, &st) == nil {
			out[id] = st
		}
	}
	return out, rows.Err()
}

// ClaimBatch returns up to n watching rows that haven't been checked
// recently (oldest last_checked first) and stamps their last_checked NOW
// so concurrent ticks don't re-claim them. The loop searches the returned
// scenes, then calls MarkAvailable on any hit.
func (r *Repo) ClaimBatch(ctx context.Context, n int) ([]Watch, error) {
	if n <= 0 {
		return nil, nil
	}
	ws, err := r.query(ctx, `
		SELECT `+cols+` FROM watches
		WHERE status = 'watching'
		ORDER BY last_checked ASC
		LIMIT ?`, n)
	if err != nil || len(ws) == 0 {
		return ws, err
	}
	now := time.Now().Unix()
	for _, w := range ws {
		_, _ = r.db.ExecContext(ctx, `UPDATE watches SET last_checked = ? WHERE stashdb_id = ?`, now, w.StashDBID)
	}
	return ws, nil
}

// CountWatching returns how many rows are still being watched — drives
// the loop's auto batch sizing (spread all over ~24h).
func (r *Repo) CountWatching(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM watches WHERE status = 'watching'`).Scan(&n)
	return n, err
}

// BackfillMeta fills display metadata (title/date/studio/image) for a
// watch ONLY where the stored value is empty — so a watch added with just
// an id (the API, a future "send to forage" integration) can still render
// itself once the loop has resolved its scene. Never clobbers a value the
// caller already set.
func (r *Repo) BackfillMeta(ctx context.Context, stashDBID, title, date, studio, image string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE watches SET
		  title       = CASE WHEN COALESCE(title,'')       = '' THEN ? ELSE title END,
		  date        = CASE WHEN COALESCE(date,'')        = '' THEN ? ELSE date END,
		  studio_name = CASE WHEN COALESCE(studio_name,'') = '' THEN ? ELSE studio_name END,
		  image_url   = CASE WHEN COALESCE(image_url,'')   = '' THEN ? ELSE image_url END
		WHERE stashdb_id = ?`,
		title, date, studio, image, stashDBID)
	return err
}

// MarkAvailable flips a watch to available and records the found release.
func (r *Repo) MarkAvailable(ctx context.Context, stashDBID, title, url, indexer, protocol string, size int64) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE watches SET
		  status = 'available',
		  found_title = ?, found_url = ?, found_indexer = ?,
		  found_protocol = ?, found_size = ?, found_at = ?
		WHERE stashdb_id = ?`,
		title, url, indexer, protocol, size, time.Now().Unix(), stashDBID)
	return err
}

// Nullable text columns are COALESCE'd to "" so a freshly-added watch
// (NULLs for the not-yet-found fields) scans cleanly into strings.
const cols = `stashdb_id, COALESCE(title,''), COALESCE(date,''),
	COALESCE(studio_name,''), COALESCE(image_url,''),
	COALESCE(performer_name,''), COALESCE(performer_id,''), target, status,
	COALESCE(found_title,''), COALESCE(found_url,''), COALESCE(found_indexer,''),
	COALESCE(found_protocol,''), found_size,
	created_at, last_checked, found_at`

func (r *Repo) query(ctx context.Context, q string, args ...any) ([]Watch, error) {
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Watch
	for rows.Next() {
		var w Watch
		if err := rows.Scan(
			&w.StashDBID, &w.Title, &w.Date, &w.StudioName, &w.ImageURL,
			&w.PerformerName, &w.PerformerID, &w.Target, &w.Status,
			&w.FoundTitle, &w.FoundURL, &w.FoundIndexer, &w.FoundProtocol, &w.FoundSize,
			&w.CreatedAt, &w.LastChecked, &w.FoundAt,
		); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
