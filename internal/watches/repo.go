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
	"encoding/json"
	"time"
)

// Quality targets a watch waits for. Exact-match semantics: a 4k release
// does NOT satisfy a Target1080. TargetAny matches any verified release.
const (
	TargetAny  = "any"
	Target480  = "480p" // SD (matches releases tagged 480p/360p)
	Target720  = "720p"
	Target1080 = "1080p"
	Target4K   = "4k"
)

// Status values.
const (
	StatusWatching  = "watching"
	StatusAvailable = "available"
	// StatusGrabbed is terminal: the user grabbed the watch's release. The
	// row LINGERS (it is not deleted) so a batch's progress reads correctly
	// ("9 of 30 grabbed"); the user clears it (or the whole batch) when done.
	StatusGrabbed = "grabbed"
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
	// BatchID/BatchLabel group watches launched together (the Watching tab
	// groups by BatchID). Empty BatchID = an ungrouped single track.
	BatchID    string `json:"batch_id,omitempty"`
	BatchLabel string `json:"batch_label,omitempty"`
	// Candidates is the full verified release list captured when the watch
	// flipped available, stored as raw JSON (an array of the api layer's
	// sceneRelease) so this package needn't import api. Lets the UI re-pick a
	// different release than the auto-chosen best. Empty ("[]") until available.
	Candidates json.RawMessage `json:"candidates,omitempty"`
	// GrabbedAt is when the watch was grabbed (status='grabbed'); 0 otherwise.
	GrabbedAt int64 `json:"grabbed_at,omitempty"`
	// SearchCount is how many times this scene has been re-searched for a
	// release (bumped on every loop claim and manual search-now). Lets the
	// Watching tab show how many attempts a still-unfound scene has had.
	SearchCount int `json:"search_count"`
	// Searching is a transient, NON-persisted view flag: true while a manual
	// "search now" is actively re-searching this watch. Set by the API layer
	// from an in-memory set (never read from / written to the DB), so the UI
	// can show a spinner. Always false off the DB.
	Searching bool `json:"searching,omitempty"`
	// IgnoredURLs are download URLs the user dismissed for this watch — the
	// loop skips them so a rejected dead/over-compressed find can't
	// re-surface. Not exposed in the API (internal to the loop's matching).
	IgnoredURLs []string `json:"-"`
	// Performers is ALL the scene's performer names, captured at add time. The
	// re-search uses these directly (no StashDB round-trip per check) and
	// searches every performer, not just the tracked one. Internal to matching.
	Performers []string `json:"-"`
}

type Repo struct{ db *sql.DB }

func NewRepo(db *sql.DB) *Repo { return &Repo{db: db} }

// Add inserts (or replaces) a watch. Re-watching an existing scene resets
// it to watching at the new target (and re-stamps its batch, clears any
// stored candidates / grabbed state).
func (r *Repo) Add(ctx context.Context, w Watch) error {
	return r.add(ctx, r.db, w)
}

// AddBatch inserts many watches sharing a batch in one transaction. Used by
// the bulk-create endpoint ("watch all missing", Discover multi-select); the
// caller stamps every watch's BatchID/BatchLabel before calling.
func (r *Repo) AddBatch(ctx context.Context, ws []Watch) error {
	if len(ws) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, w := range ws {
		if err := r.add(ctx, tx, w); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// execer is satisfied by both *sql.DB and *sql.Tx so add() can run inside or
// outside a transaction.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (r *Repo) add(ctx context.Context, ex execer, w Watch) error {
	if w.Target == "" {
		w.Target = TargetAny
	}
	if w.CreatedAt == 0 {
		w.CreatedAt = time.Now().Unix()
	}
	perfsJSON, _ := json.Marshal(w.Performers)
	if len(perfsJSON) == 0 {
		perfsJSON = []byte("[]")
	}
	_, err := ex.ExecContext(ctx, `
		INSERT INTO watches (
		  stashdb_id, title, date, studio_name, image_url,
		  performer_name, performer_id, target, status, created_at, last_checked,
		  batch_id, batch_label, performers
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'watching', ?, 0, ?, ?, ?)
		ON CONFLICT(stashdb_id) DO UPDATE SET
		  target = excluded.target,
		  status = 'watching',
		  performer_name = excluded.performer_name,
		  performer_id = excluded.performer_id,
		  batch_id = excluded.batch_id,
		  batch_label = excluded.batch_label,
		  performers = excluded.performers,
		  found_title = '', found_url = '', found_indexer = '',
		  found_protocol = '', found_size = 0, found_at = 0,
		  candidates = '[]', grabbed_at = 0,
		  last_checked = 0`,
		w.StashDBID, w.Title, w.Date, w.StudioName, w.ImageURL,
		w.PerformerName, w.PerformerID, w.Target, w.CreatedAt,
		w.BatchID, w.BatchLabel, string(perfsJSON))
	return err
}

// SetPerformers backfills a watch's performer-name list (used by the loop the
// first time it resolves a scene that was added without one — e.g. a bare API
// add — so later checks skip the StashDB fetch).
func (r *Repo) SetPerformers(ctx context.Context, stashDBID string, names []string) error {
	b, _ := json.Marshal(names)
	if len(b) == 0 {
		b = []byte("[]")
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE watches SET performers = ? WHERE stashdb_id = ?`, string(b), stashDBID)
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

// ClaimBatch returns up to n watching rows to re-check this tick and stamps
// their last_checked NOW so concurrent ticks don't re-claim them. The loop
// searches the returned scenes, then calls MarkAvailable on any hit.
//
// Selection is ROUND-ROBIN across batches, not a flat oldest-first: each
// batch (and the ungrouped singles, which all share an empty batch_id) is a
// group, and we take every group's oldest-checked, then the second-oldest,
// and so on (ROW_NUMBER per batch). A plain `ORDER BY last_checked ASC` let a
// single huge batch (e.g. a 600-scene performer backfill) monopolise the tick
// budget and starve everything added after it — the singles would sit at
// last_checked=0 forever behind the batch. Round-robin gives a 600-row batch
// and a handful of singles each a fair share of the budget, so both progress.
// Within a group's rank, oldest-checked wins (never-checked rows surface
// first); created_at/stashdb_id break remaining ties deterministically.
func (r *Repo) ClaimBatch(ctx context.Context, n int) ([]Watch, error) {
	if n <= 0 {
		return nil, nil
	}
	ws, err := r.query(ctx, `
		SELECT `+cols+` FROM watches
		WHERE status = 'watching'
		ORDER BY ROW_NUMBER() OVER (
		           PARTITION BY batch_id
		           ORDER BY last_checked ASC, created_at ASC, stashdb_id ASC) ASC,
		         last_checked ASC, created_at ASC, stashdb_id ASC
		LIMIT ?`, n)
	if err != nil || len(ws) == 0 {
		return ws, err
	}
	now := time.Now().Unix()
	for _, w := range ws {
		_, _ = r.db.ExecContext(ctx,
			`UPDATE watches SET last_checked = ?, search_count = search_count + 1 WHERE stashdb_id = ?`,
			now, w.StashDBID)
	}
	return ws, nil
}

// CountWatching returns how many rows are still being watched — drives
// the loop's auto batch sizing.
func (r *Repo) CountWatching(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM watches WHERE status = 'watching'`).Scan(&n)
	return n, err
}

// CountAvailable returns how many watches have found a release and are
// ready to grab — the headline notification signal.
func (r *Repo) CountAvailable(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM watches WHERE status = 'available'`).Scan(&n)
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

// MarkAvailable flips a watch to available, records the found (best) release,
// and stores the full verified candidate list (raw JSON) so the UI can offer
// a re-pick. candidates may be nil/empty, in which case the column is left as
// an empty array.
func (r *Repo) MarkAvailable(ctx context.Context, stashDBID, title, url, indexer, protocol string, size int64, candidates json.RawMessage) error {
	cands := string(candidates)
	if cands == "" {
		cands = "[]"
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE watches SET
		  status = 'available',
		  found_title = ?, found_url = ?, found_indexer = ?,
		  found_protocol = ?, found_size = ?, found_at = ?,
		  candidates = ?
		WHERE stashdb_id = ?`,
		title, url, indexer, protocol, size, time.Now().Unix(), cands, stashDBID)
	return err
}

// MarkChecked stamps a watch's last_checked to now — used after a manual
// "search now" so the background loop (which claims oldest-checked first)
// deprioritises rows that were just searched.
func (r *Repo) MarkChecked(ctx context.Context, stashDBID string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE watches SET last_checked = ?, search_count = search_count + 1 WHERE stashdb_id = ?`,
		time.Now().Unix(), stashDBID)
	return err
}

// MarkGrabbed flips a watch to the terminal 'grabbed' status and records the
// release that was actually grabbed (which may differ from the auto-picked
// best when the user re-picked a candidate). The row is NOT deleted — it
// lingers so a batch's progress reads correctly; the user clears it (or the
// batch) when done.
func (r *Repo) MarkGrabbed(ctx context.Context, stashDBID, title, url, indexer, protocol string, size int64) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE watches SET
		  status = 'grabbed',
		  found_title = ?, found_url = ?, found_indexer = ?,
		  found_protocol = ?, found_size = ?, grabbed_at = ?
		WHERE stashdb_id = ?`,
		title, url, indexer, protocol, size, time.Now().Unix(), stashDBID)
	return err
}

// DeleteBatch removes every watch in a batch (the Watching tab's per-batch
// "Clear"). Refuses an empty batchID — that would match every ungrouped
// single track and wipe them all.
func (r *Repo) DeleteBatch(ctx context.Context, batchID string) error {
	if batchID == "" {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `DELETE FROM watches WHERE batch_id = ?`, batchID)
	return err
}

// Dismiss rejects the watch's current found release: adds its URL to the
// ignored set (so the loop never re-surfaces it) and flips the watch back
// to watching, clearing the found_* fields so the next qualifying release
// can take their place. No-op-safe when the URL is empty or already
// ignored. Returns the watch's new ignored URL count for logging.
func (r *Repo) Dismiss(ctx context.Context, stashDBID, url string) error {
	// Read the current ignored set, append, re-marshal — small list, simple.
	var ignoredJSON string
	err := r.db.QueryRowContext(ctx,
		`SELECT COALESCE(ignored_urls,'[]') FROM watches WHERE stashdb_id = ?`,
		stashDBID).Scan(&ignoredJSON)
	if err != nil {
		return err
	}
	var ignored []string
	if ignoredJSON != "" {
		_ = json.Unmarshal([]byte(ignoredJSON), &ignored)
	}
	if url != "" {
		seen := false
		for _, u := range ignored {
			if u == url {
				seen = true
				break
			}
		}
		if !seen {
			ignored = append(ignored, url)
		}
	}
	b, _ := json.Marshal(ignored)
	_, err = r.db.ExecContext(ctx, `
		UPDATE watches SET
		  status = 'watching',
		  found_title = '', found_url = '', found_indexer = '',
		  found_protocol = '', found_size = 0, found_at = 0,
		  ignored_urls = ?
		WHERE stashdb_id = ?`,
		string(b), stashDBID)
	return err
}

// Nullable text columns are COALESCE'd to "" so a freshly-added watch
// (NULLs for the not-yet-found fields) scans cleanly into strings.
const cols = `stashdb_id, COALESCE(title,''), COALESCE(date,''),
	COALESCE(studio_name,''), COALESCE(image_url,''),
	COALESCE(performer_name,''), COALESCE(performer_id,''), target, status,
	COALESCE(found_title,''), COALESCE(found_url,''), COALESCE(found_indexer,''),
	COALESCE(found_protocol,''), found_size,
	created_at, last_checked, found_at, COALESCE(ignored_urls,'[]'),
	COALESCE(batch_id,''), COALESCE(batch_label,''), COALESCE(candidates,'[]'), grabbed_at,
	COALESCE(performers,'[]'), search_count`

func (r *Repo) query(ctx context.Context, q string, args ...any) ([]Watch, error) {
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Watch
	for rows.Next() {
		var w Watch
		var ignoredJSON, candJSON, perfsJSON string
		if err := rows.Scan(
			&w.StashDBID, &w.Title, &w.Date, &w.StudioName, &w.ImageURL,
			&w.PerformerName, &w.PerformerID, &w.Target, &w.Status,
			&w.FoundTitle, &w.FoundURL, &w.FoundIndexer, &w.FoundProtocol, &w.FoundSize,
			&w.CreatedAt, &w.LastChecked, &w.FoundAt, &ignoredJSON,
			&w.BatchID, &w.BatchLabel, &candJSON, &w.GrabbedAt, &perfsJSON, &w.SearchCount,
		); err != nil {
			return nil, err
		}
		if ignoredJSON != "" && ignoredJSON != "[]" {
			_ = json.Unmarshal([]byte(ignoredJSON), &w.IgnoredURLs)
		}
		if perfsJSON != "" && perfsJSON != "[]" {
			_ = json.Unmarshal([]byte(perfsJSON), &w.Performers)
		}
		if candJSON == "" {
			candJSON = "[]"
		}
		w.Candidates = json.RawMessage(candJSON)
		out = append(out, w)
	}
	return out, rows.Err()
}
