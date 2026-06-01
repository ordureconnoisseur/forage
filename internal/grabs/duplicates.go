package grabs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// SceneCopy is one physical copy of a scene in the library. A duplicate
// compares the pack's copy against the pre-existing one(s) so the user can
// keep the better file (resolution + size are the deciding signals).
type SceneCopy struct {
	SceneID string `json:"scene_id"`
	Title   string `json:"title,omitempty"`
	Path    string `json:"path,omitempty"`
	Size    int64  `json:"size,omitempty"`
	Height  int    `json:"height,omitempty"` // bucketed into 1080p/2160p/… by the UI
}

// PackDuplicate is a review-mode dedup item: a StashDB scene the pack
// delivered that the library ALREADY had elsewhere. In review mode the
// poller records it as `pending` instead of auto-destroying either copy;
// the user later picks per scene which side survives, and only then does
// the (irreversible) destroy run. See schema.sql for the column contract.
type PackDuplicate struct {
	ID         int64
	GrabID     int64
	StashDBID  string
	SceneTitle string
	Pack       SceneCopy   // the pack's freshly-downloaded copy
	Existing   []SceneCopy // copies already in the library before this pack
	Status     string      // pending | resolved
	Resolution string      // existing | pack | both (once resolved)
	CreatedAt  int64
	ResolvedAt int64
}

// UpsertDuplicate records (or refreshes) a pending duplicate. The
// UNIQUE(grab_id, stashdb_id) constraint makes detection idempotent: a
// re-tick refreshes the snapshot of a still-pending row, but never
// resurrects or clobbers one the user has already resolved.
func (r *Repo) UpsertDuplicate(ctx context.Context, d PackDuplicate) error {
	existing, err := json.Marshal(d.Existing)
	if err != nil {
		return fmt.Errorf("marshal existing copies: %w", err)
	}
	now := time.Now().Unix()
	_, err = r.db.ExecContext(ctx, `
INSERT INTO pack_duplicate
  (grab_id, stashdb_id, scene_title, pack_scene_id, pack_path, pack_size, pack_height, existing_copies, status, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?)
ON CONFLICT(grab_id, stashdb_id) DO UPDATE SET
  scene_title     = excluded.scene_title,
  pack_scene_id   = excluded.pack_scene_id,
  pack_path       = excluded.pack_path,
  pack_size       = excluded.pack_size,
  pack_height     = excluded.pack_height,
  existing_copies = excluded.existing_copies
WHERE pack_duplicate.status = 'pending'`,
		d.GrabID, d.StashDBID, d.SceneTitle, d.Pack.SceneID, d.Pack.Path, d.Pack.Size, d.Pack.Height, string(existing), now)
	return err
}

// PendingDuplicatesByGrab returns the unresolved review items for one pack,
// newest first, for the inline Grabs UI.
func (r *Repo) PendingDuplicatesByGrab(ctx context.Context, grabID int64) ([]PackDuplicate, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT id, grab_id, stashdb_id, scene_title, pack_scene_id, pack_path, pack_size, pack_height,
       existing_copies, status, resolution, created_at, resolved_at
  FROM pack_duplicate
 WHERE grab_id = ? AND status = 'pending'
 ORDER BY id DESC`, grabID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDuplicates(rows)
}

// PendingDuplicateCounts returns grab_id → pending-count, so the Grabs list
// can badge packs that need review without loading every row.
func (r *Repo) PendingDuplicateCounts(ctx context.Context) (map[int64]int, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT grab_id, COUNT(*) FROM pack_duplicate WHERE status = 'pending' GROUP BY grab_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// GetDuplicate loads a single review item by id (nil if absent).
func (r *Repo) GetDuplicate(ctx context.Context, id int64) (*PackDuplicate, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT id, grab_id, stashdb_id, scene_title, pack_scene_id, pack_path, pack_size, pack_height,
       existing_copies, status, resolution, created_at, resolved_at
  FROM pack_duplicate WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	dups, err := scanDuplicates(rows)
	if err != nil {
		return nil, err
	}
	if len(dups) == 0 {
		return nil, nil
	}
	return &dups[0], nil
}

// ResolveDuplicate marks a review item resolved with the chosen keep side.
// Called AFTER the destroy succeeds, so a row only leaves `pending` once the
// library actually reflects the decision.
func (r *Repo) ResolveDuplicate(ctx context.Context, id int64, resolution string) error {
	_, err := r.db.ExecContext(ctx, `
UPDATE pack_duplicate SET status = 'resolved', resolution = ?, resolved_at = ? WHERE id = ?`,
		resolution, time.Now().Unix(), id)
	return err
}

// DeleteDuplicatesByGrab clears a pack's review items — called when the grab
// itself is deleted so orphaned rows don't linger.
func (r *Repo) DeleteDuplicatesByGrab(ctx context.Context, grabID int64) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM pack_duplicate WHERE grab_id = ?`, grabID)
	return err
}

func scanDuplicates(rows *sql.Rows) ([]PackDuplicate, error) {
	var out []PackDuplicate
	for rows.Next() {
		var d PackDuplicate
		var existingJSON string
		if err := rows.Scan(&d.ID, &d.GrabID, &d.StashDBID, &d.SceneTitle,
			&d.Pack.SceneID, &d.Pack.Path, &d.Pack.Size, &d.Pack.Height,
			&existingJSON, &d.Status, &d.Resolution, &d.CreatedAt, &d.ResolvedAt); err != nil {
			return nil, err
		}
		if existingJSON != "" {
			if err := json.Unmarshal([]byte(existingJSON), &d.Existing); err != nil {
				return nil, fmt.Errorf("unmarshal existing copies (dup %d): %w", d.ID, err)
			}
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
