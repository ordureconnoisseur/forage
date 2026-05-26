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
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO grabs (
		  predicted_stashdb_id, predicted_confidence, release_title,
		  release_size, release_indexer, download_url,
		  client, client_id, client_name, category, status,
		  actual_stashdb_id, reason,
		  performer_name, placed_path, place_error,
		  grabbed_at, updated_at, completed_at, placed_at, confirmed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nullString(g.PredictedStashDBID), nullFloat(g.PredictedConfidence), g.ReleaseTitle,
		nullInt(g.ReleaseSize), nullString(g.ReleaseIndexer), nullString(g.DownloadURL),
		g.Client, nullString(g.ClientID), nullString(g.ClientName),
		nullString(g.Category), g.Status,
		nullString(g.ActualStashDBID), nullString(g.Reason),
		nullString(g.PerformerName), nullString(g.PlacedPath), nullString(g.PlaceError),
		g.GrabbedAt, now,
		nullInt(g.CompletedAt), nullInt(g.PlacedAt), nullInt(g.ConfirmedAt),
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
		  confirmed_at = COALESCE(?, confirmed_at)
		WHERE id = ?`,
		nullString(g.ClientID), nullString(g.ClientName), g.Status,
		nullString(g.ActualStashDBID), nullString(g.Reason),
		nullString(g.PerformerName), nullString(g.PlacedPath), nullString(g.PlaceError),
		now,
		nullInt(g.CompletedAt), nullInt(g.PlacedAt), nullInt(g.ConfirmedAt),
		g.ID,
	)
	if err != nil {
		return fmt.Errorf("grabs update id=%d: %w", g.ID, err)
	}
	return nil
}

// Active returns grabs the poller still cares about — anything not in
// a terminal state. `placed` joins the active set so the poller can
// re-check Stash for confirmation once the file is in the library.
func (r *Repo) Active(ctx context.Context) ([]Grab, error) {
	return r.query(ctx, `
		SELECT * FROM grabs
		WHERE status IN ('queued', 'downloading', 'completed', 'placed')
		ORDER BY grabbed_at ASC`)
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

// query is the shared SELECT path. The column order MUST match the
// scanRow helper.
func (r *Repo) query(ctx context.Context, sql string, args ...any) ([]Grab, error) {
	const cols = `
		id, predicted_stashdb_id, predicted_confidence, release_title,
		release_size, release_indexer, download_url,
		client, client_id, client_name, category, status, actual_stashdb_id,
		reason, performer_name, placed_path, place_error,
		grabbed_at, updated_at, completed_at, placed_at, confirmed_at`
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
		performerName, placedPath, placeError                                                       sql.NullString
		predictedConfidence                                                                         sql.NullFloat64
		releaseSize, completedAt, placedAt, confirmedAt                                             sql.NullInt64
	)
	err := rows.Scan(&g.ID,
		&predictedID, &predictedConfidence, &g.ReleaseTitle,
		&releaseSize, &releaseIndexer, &downloadURL,
		&g.Client, &clientID, &clientName, &category, &g.Status, &actualID,
		&reason, &performerName, &placedPath, &placeError,
		&g.GrabbedAt, &g.UpdatedAt, &completedAt, &placedAt, &confirmedAt)
	if err != nil {
		return g, err
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
