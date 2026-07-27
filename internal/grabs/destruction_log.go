package grabs

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ordureconnoisseur/forager/internal/destroy"
)

// The destruction journal (destruction_log): forage's answer to "what did
// you delete, when, and why" without log archaeology. Rows are written by
// destroy.Execute through the destroy.Recorder interface, which *Repo
// satisfies — intent BEFORE the destroy runs (so a crash mid-destroy still
// leaves evidence of what was in flight), finalised with the outcome after,
// and refusals recorded outright.

// DestructionEntry is one journal row, files decoded.
type DestructionEntry struct {
	ID        int64          `json:"id"`
	Reason    string         `json:"reason"`
	SceneID   string         `json:"scene_id"`
	Title     string         `json:"title,omitempty"`
	Files     []destroy.File `json:"files"`
	Outcome   string         `json:"outcome"`
	Detail    string         `json:"detail,omitempty"`
	CreatedAt int64          `json:"created_at"`
}

// JournalDestruction records one destruction event (or intent, or refusal)
// and returns the row id for FinalizeDestruction.
func (r *Repo) JournalDestruction(ctx context.Context, reason string, t destroy.Target, outcome, detail string) (int64, error) {
	files := t.Files
	if files == nil {
		files = []destroy.File{}
	}
	fj, err := json.Marshal(files)
	if err != nil {
		return 0, err
	}
	res, err := r.db.ExecContext(ctx, `
INSERT INTO destruction_log (reason, scene_id, title, files, outcome, detail, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		reason, t.SceneID, t.Title, string(fj), outcome, detail, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinalizeDestruction sets the outcome on a previously-journalled intent.
func (r *Repo) FinalizeDestruction(ctx context.Context, id int64, outcome, detail string) error {
	_, err := r.db.ExecContext(ctx, `
UPDATE destruction_log SET outcome = ?, detail = ? WHERE id = ?`, outcome, detail, id)
	return err
}

// GetDestruction loads one journal row by id, or (nil, nil) when absent.
func (r *Repo) GetDestruction(ctx context.Context, id int64) (*DestructionEntry, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT id, reason, scene_id, title, files, outcome, detail, created_at
  FROM destruction_log WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	var e DestructionEntry
	var filesJSON string
	if err := rows.Scan(&e.ID, &e.Reason, &e.SceneID, &e.Title,
		&filesJSON, &e.Outcome, &e.Detail, &e.CreatedAt); err != nil {
		return nil, err
	}
	if filesJSON != "" {
		_ = json.Unmarshal([]byte(filesJSON), &e.Files)
	}
	return &e, nil
}

// CountDestructionsByOutcome tallies journal rows per outcome, optionally
// restricted to rows created at/after since (0 = all time). Feeds the
// /healthz destructions block: a bug report should show at a glance whether
// the daemon has been deleting, refusing, or neither — without shipping the
// journal itself.
func (r *Repo) CountDestructionsByOutcome(ctx context.Context, since int64) (map[string]int64, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT outcome, COUNT(*) FROM destruction_log
 WHERE created_at >= ? GROUP BY outcome`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var outcome string
		var n int64
		if err := rows.Scan(&outcome, &n); err != nil {
			return nil, err
		}
		out[outcome] = n
	}
	return out, rows.Err()
}

// ListDestructions returns the newest journal rows, for the audit view.
func (r *Repo) ListDestructions(ctx context.Context, limit int) ([]DestructionEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT id, reason, scene_id, title, files, outcome, detail, created_at
  FROM destruction_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DestructionEntry
	for rows.Next() {
		var e DestructionEntry
		var filesJSON string
		if err := rows.Scan(&e.ID, &e.Reason, &e.SceneID, &e.Title,
			&filesJSON, &e.Outcome, &e.Detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		if filesJSON != "" {
			// Best-effort decode: a malformed row must not hide the rest of
			// the journal — the raw outcome/reason columns still tell the
			// story.
			_ = json.Unmarshal([]byte(filesJSON), &e.Files)
		}
		if e.Files == nil {
			e.Files = []destroy.File{}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
