// Package grabs persists Phase B grab tracking: every user-initiated
// forager → qBit add is stored as a row that the background poller
// advances through queued → downloading → completed →
// confirmed/mismatched/orphaned.
//
// Repo is intentionally thin: a few CRUD methods, no behaviour. The
// state machine lives in internal/poller.
package grabs

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Grab is the in-memory shape; columns map 1:1 onto the SQLite schema.
// All optional fields are pointer-or-zero so they can be NULL on disk.
type Grab struct {
	ID                  int64
	PredictedStashDBID  string
	PredictedConfidence float64
	ReleaseTitle        string
	ReleaseSize         int64
	ReleaseIndexer      string
	DownloadURL         string
	Client              string // "qbit" | "sabnzbd"
	ClientID            string // qBit info_hash or SAB nzo_id
	ClientName          string // on-disk filename the client reports
	Category            string
	Status              string
	ActualStashDBID     string
	Reason              string
	// PerformerName is the directory under <library_root> the file is
	// placed into. Captured at /grab time so placement is predictable
	// regardless of how StashDB orders the scene's performer list.
	PerformerName string
	// PlacedPath is the final on-disk location after the placer
	// hardlinks/copies the file out of the download client's complete dir.
	PlacedPath string
	// PlaceError is the most recent placement failure reason. The
	// placer retries on subsequent ticks; the field is cleared on success.
	PlaceError  string
	GrabbedAt   int64
	UpdatedAt   int64
	CompletedAt int64
	PlacedAt    int64
	ConfirmedAt int64
	// Kind is "single" (the default — one release → one scene) or "pack"
	// (one release → many of a performer's scenes). Pack grabs follow a
	// distinct confirm path in the poller: enumerate every placed file's
	// scene, drive identify across all of them, then dedup against the
	// existing library.
	Kind string
	// Pack progress counters (0 for single grabs):
	//   PackFiles      expected video count (from the parsed .torrent at grab time)
	//   PackIdentified scenes Stash has cross-id'd to StashDB so far
	//   PackDeduped    scenes removed because the library already had them
	PackFiles      int
	PackIdentified int
	PackDeduped    int
}

// Repo is the persistence boundary. One per *sql.DB.
type Repo struct {
	db *sql.DB
}

func NewRepo(db *sql.DB) *Repo {
	return &Repo{db: db}
}

// Insert records a new grab at the moment the user clicks Grab and the
// torrent is successfully handed to qBit. status defaults to "queued"
// — the poller fills in the qBit hash + later transitions.
func (r *Repo) Insert(ctx context.Context, g Grab) (int64, error) {
	now := time.Now().Unix()
	if g.Status == "" {
		g.Status = "queued"
	}
	if g.Client == "" {
		g.Client = "qbit"
	}
	if g.GrabbedAt == 0 {
		g.GrabbedAt = now
	}
	if g.Kind == "" {
		g.Kind = "single"
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO grabs (
		  predicted_stashdb_id, predicted_confidence, release_title,
		  release_size, release_indexer, download_url,
		  client, client_id, client_name, category, status,
		  actual_stashdb_id, reason,
		  performer_name, placed_path, place_error,
		  grabbed_at, updated_at, completed_at, placed_at, confirmed_at,
		  kind, pack_files, pack_identified, pack_deduped
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nullString(g.PredictedStashDBID), nullFloat(g.PredictedConfidence), g.ReleaseTitle,
		nullInt(g.ReleaseSize), nullString(g.ReleaseIndexer), nullString(g.DownloadURL),
		g.Client, nullString(g.ClientID), nullString(g.ClientName),
		nullString(g.Category), g.Status,
		nullString(g.ActualStashDBID), nullString(g.Reason),
		nullString(g.PerformerName), nullString(g.PlacedPath), nullString(g.PlaceError),
		g.GrabbedAt, now,
		nullInt(g.CompletedAt), nullInt(g.PlacedAt), nullInt(g.ConfirmedAt),
		g.Kind, g.PackFiles, g.PackIdentified, g.PackDeduped,
	)
	if err != nil {
		return 0, fmt.Errorf("grabs insert: %w", err)
	}
	return res.LastInsertId()
}

// Update writes the mutable fields back to disk and bumps updated_at.
// The poller calls this after each tick's status transition. Caller is
// responsible for setting reason / timestamps before calling.
func (r *Repo) Update(ctx context.Context, g Grab) error {
	now := time.Now().Unix()
	_, err := r.db.ExecContext(ctx, `
		UPDATE grabs SET
		  client_id = ?, client_name = ?, status = ?,
		  actual_stashdb_id = ?, reason = ?,
		  performer_name = ?, placed_path = ?, place_error = ?,
		  updated_at = ?,
		  completed_at = COALESCE(?, completed_at),
		  placed_at = COALESCE(?, placed_at),
		  confirmed_at = COALESCE(?, confirmed_at),
		  pack_files = ?, pack_identified = ?, pack_deduped = ?
		WHERE id = ?`,
		nullString(g.ClientID), nullString(g.ClientName), g.Status,
		nullString(g.ActualStashDBID), nullString(g.Reason),
		nullString(g.PerformerName), nullString(g.PlacedPath), nullString(g.PlaceError),
		now,
		nullInt(g.CompletedAt), nullInt(g.PlacedAt), nullInt(g.ConfirmedAt),
		g.PackFiles, g.PackIdentified, g.PackDeduped,
		g.ID,
	)
	if err != nil {
		return fmt.Errorf("grabs update id=%d: %w", g.ID, err)
	}
	return nil
}

// Active returns grabs the poller still cares about — anything not in
// a terminal state.
//
//   - placed: file is on disk but not yet seen in Stash; the poller
//     re-checks each tick until it confirms or orphans.
//   - scanned: Stash has the file but hasn't attached a StashDB
//     cross-id yet (identify pending); we keep polling to detect when
//     the identify completes.
//   - orphaned: Stash didn't pick up the file within the orphan
//     window. Stays in Active so a later scan/identify (manual or
//     scheduled) can promote it back to scanned/confirmed instead of
//     leaving the user with a permanent false orphan label.
func (r *Repo) Active(ctx context.Context) ([]Grab, error) {
	return r.query(ctx, `
		SELECT * FROM grabs
		WHERE status IN ('queued', 'downloading', 'completed', 'placed', 'scanned', 'orphaned')
		ORDER BY grabbed_at ASC`)
}

// StatusByStashDBID returns a map of StashDB scene id → grab status for
// every grab that resolves to a StashDB id, so the missing-scenes view
// can mark scenes already grabbed/in-flight. Keyed by the actual cross-id
// when known (confirmed), else the predicted one. When several grabs map
// to the same scene id, the most advanced status wins (so a confirmed
// grab isn't masked by a later failed retry of the same scene).
func (r *Repo) StatusByStashDBID(ctx context.Context) (map[string]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT COALESCE(NULLIF(actual_stashdb_id, ''), predicted_stashdb_id) AS sid, status
		FROM grabs
		WHERE COALESCE(NULLIF(actual_stashdb_id, ''), predicted_stashdb_id) IS NOT NULL
		  AND COALESCE(NULLIF(actual_stashdb_id, ''), predicted_stashdb_id) != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var sid, status string
		if rows.Scan(&sid, &status) != nil {
			continue
		}
		if cur, ok := out[sid]; !ok || statusRank(status) > statusRank(cur) {
			out[sid] = status
		}
	}
	return out, rows.Err()
}

// statusRank orders grab statuses by pipeline progress so the most
// advanced grab for a scene wins in StatusByStashDBID. failed/orphaned
// rank below everything so a dead retry never masks a live/confirmed grab.
func statusRank(s string) int {
	switch s {
	case "confirmed":
		return 6
	case "scanned":
		return 5
	case "placed":
		return 4
	case "completed":
		return 3
	case "downloading":
		return 2
	case "queued":
		return 1
	default: // failed, orphaned, mismatched, unknown
		return 0
	}
}

// List returns the most recent grabs first, filtered by status if
// nonempty. Used by the GET /grabs endpoint.
func (r *Repo) List(ctx context.Context, status string, limit, offset int) ([]Grab, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if status == "" || status == "any" {
		return r.query(ctx, `
			SELECT * FROM grabs ORDER BY grabbed_at DESC LIMIT ? OFFSET ?`,
			limit, offset)
	}
	return r.query(ctx, `
		SELECT * FROM grabs WHERE status = ? ORDER BY grabbed_at DESC LIMIT ? OFFSET ?`,
		status, limit, offset)
}

// Totals returns a status → count map for the UI's top-of-page strip.
func (r *Repo) Totals(ctx context.Context) (map[string]int, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM grabs GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}

// ByDownloadURL is the lookup the search UI uses to mark a release as
// "already grabbed" — same Prowlarr download URL has been clicked before.
func (r *Repo) ByDownloadURL(ctx context.Context, url string) (*Grab, error) {
	if url == "" {
		return nil, nil
	}
	grabs, err := r.query(ctx, `
		SELECT * FROM grabs WHERE download_url = ? ORDER BY grabbed_at DESC LIMIT 1`, url)
	if err != nil || len(grabs) == 0 {
		return nil, err
	}
	g := grabs[0]
	return &g, nil
}

// Get returns a single grab by id, or (nil, nil) when not found.
func (r *Repo) Get(ctx context.Context, id int64) (*Grab, error) {
	grabs, err := r.query(ctx, `SELECT * FROM grabs WHERE id = ? LIMIT 1`, id)
	if err != nil || len(grabs) == 0 {
		return nil, err
	}
	g := grabs[0]
	return &g, nil
}

// Delete removes a grab row. Used by the purge flow after the file +
// Stash scene + download-client copy have been torn down.
func (r *Repo) Delete(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM grabs WHERE id = ?`, id)
	return err
}

// KnownClientIDs returns the set of download-client ids (qBit info_hashes
// / SAB nzo_ids) that already back a grab. The poller's adoption path
// uses it to skip torrents forage is already tracking.
func (r *Repo) KnownClientIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT client_id FROM grabs WHERE client_id IS NOT NULL AND client_id != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil && id != "" {
			out[id] = true
		}
	}
	return out, rows.Err()
}

// query is the shared SELECT path. The column order MUST match the
// scanRow helper.
func (r *Repo) query(ctx context.Context, sql string, args ...any) ([]Grab, error) {
	const cols = `
		id, predicted_stashdb_id, predicted_confidence, release_title,
		release_size, release_indexer, download_url,
		client, client_id, client_name, category, status, actual_stashdb_id,
		reason, performer_name, placed_path, place_error,
		grabbed_at, updated_at, completed_at, placed_at, confirmed_at,
		kind, pack_files, pack_identified, pack_deduped`
	// Inject column list into the SELECT *.
	sql = replaceFirstStar(sql, cols)
	rows, err := r.db.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Grab
	for rows.Next() {
		g, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func scanRow(rows *sql.Rows) (Grab, error) {
	var g Grab
	var (
		predictedID, releaseIndexer, downloadURL, clientID, clientName, category, actualID, reason sql.NullString
		performerName, placedPath, placeError                                                      sql.NullString
		predictedConfidence                                                                        sql.NullFloat64
		releaseSize, completedAt, placedAt, confirmedAt                                            sql.NullInt64
		kind                                                                                       sql.NullString
	)
	err := rows.Scan(&g.ID,
		&predictedID, &predictedConfidence, &g.ReleaseTitle,
		&releaseSize, &releaseIndexer, &downloadURL,
		&g.Client, &clientID, &clientName, &category, &g.Status, &actualID,
		&reason, &performerName, &placedPath, &placeError,
		&g.GrabbedAt, &g.UpdatedAt, &completedAt, &placedAt, &confirmedAt,
		&kind, &g.PackFiles, &g.PackIdentified, &g.PackDeduped)
	if err != nil {
		return g, err
	}
	g.Kind = kind.String
	if g.Kind == "" {
		g.Kind = "single"
	}
	g.PredictedStashDBID = predictedID.String
	g.PredictedConfidence = predictedConfidence.Float64
	g.ReleaseSize = releaseSize.Int64
	g.ReleaseIndexer = releaseIndexer.String
	g.DownloadURL = downloadURL.String
	g.ClientID = clientID.String
	g.ClientName = clientName.String
	g.Category = category.String
	g.ActualStashDBID = actualID.String
	g.Reason = reason.String
	g.PerformerName = performerName.String
	g.PlacedPath = placedPath.String
	g.PlaceError = placeError.String
	g.CompletedAt = completedAt.Int64
	g.PlacedAt = placedAt.Int64
	g.ConfirmedAt = confirmedAt.Int64
	return g, nil
}

func replaceFirstStar(sql, cols string) string {
	const tag = "*"
	for i := 0; i < len(sql); i++ {
		if sql[i] == tag[0] {
			return sql[:i] + cols + sql[i+1:]
		}
	}
	return sql
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(i int64) any {
	if i == 0 {
		return nil
	}
	return i
}

func nullFloat(f float64) any {
	if f == 0 {
		return nil
	}
	return f
}
