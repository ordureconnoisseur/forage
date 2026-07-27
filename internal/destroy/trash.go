package destroy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ordureconnoisseur/forager/internal/pathmap"
)

// TrashConfig turns permanent deletion into a recoverable move: instead of
// sceneDestroy(delete_file=true), the scene's METADATA is destroyed and its
// files are renamed into Root, where the TTL sweep unlinks them for real
// only after the retention window. Until then, every forage deletion is an
// os.Rename away from restoration.
//
// Root must live on the same filesystem as the library (rename, not copy)
// and OUTSIDE the tree Stash scans — trash inside the library would be
// re-indexed on the next scan and every "deleted" scene would reappear.
// DefaultTrashRoot places it beside the library root for exactly that
// reason.
type TrashConfig struct {
	// Root is the daemon-side trash directory.
	Root string
	// Mapping is the stash-path mapping ("<forager>:<stash>"), used to
	// reverse Stash-reported file paths into paths the daemon can act on.
	// Empty is fine when both sides share one mount: paths then pass
	// through verbatim.
	Mapping string
	// LibraryRoot is the daemon-side library directory — the health canary.
	// When it can't be stat'd the MOUNT is gone, and "file missing" stops
	// being evidence of anything: falling back to a permanent Stash-side
	// delete during an outage would convert every recoverable deletion into
	// a real one (Stash is often another machine whose own mount is fine).
	// Destroys are refused outright instead, until the mount returns.
	LibraryRoot string
}

// ErrLibraryUnavailable marks a destroy refused because the library mount
// itself is unreachable — retry when it returns; never degrade to a
// permanent delete.
var ErrLibraryUnavailable = errors.New("library mount unavailable")

// DefaultTrashRoot derives the trash location from the library root: a
// SIBLING (".forage-trash" next to the library directory), same filesystem
// so moves are atomic renames, outside the library so Stash never scans it.
// Empty when the library root is unset (no placement → nothing to trash).
func DefaultTrashRoot(libraryRoot string) string {
	lr := strings.TrimRight(libraryRoot, `/\`)
	if lr == "" {
		return ""
	}
	return filepath.ToSlash(filepath.Join(filepath.Dir(lr), ".forage-trash"))
}

// TrashFromSettings builds the trash configuration from composed settings,
// or nil when trash is off: a zero/negative TTL is the explicit escape
// hatch back to permanent deletion, and no library root means no placement
// and nowhere sane to put a trash.
func TrashFromSettings(libraryRoot, mapping string, ttl time.Duration) *TrashConfig {
	if ttl <= 0 {
		return nil
	}
	root := DefaultTrashRoot(libraryRoot)
	if root == "" {
		return nil
	}
	return &TrashConfig{Root: root, Mapping: mapping, LibraryRoot: strings.TrimRight(libraryRoot, `/\`)}
}

// daemonPath resolves a (possibly Stash-reported) file path into one the
// daemon can act on. Stash and the daemon are often different machines
// sharing storage — the daemon can never touch "Z:\Media\…", only its own
// mount. Reverse-map when the mapping applies; otherwise try the path
// verbatim (the shared-mount, no-mapping setup). Empty means "not
// actionable from here", which demotes the delete to permanent — never to
// a guess.
func (t *TrashConfig) daemonPath(p string) string {
	if rp := pathmap.Reverse(p, t.Mapping); rp != "" {
		return rp
	}
	if t.Mapping == "" {
		return p
	}
	return ""
}

// trashMove is one executed rename, journalled (as JSON in the entry's
// detail) so restore knows exactly what to put back where.
type trashMove struct {
	From string `json:"from"` // daemon-side original
	To   string `json:"to"`   // daemon-side trash location
}

// trashTargetFiles plans and performs the file moves for one target.
// Sequencing is the safety argument:
//
//  1. resolve every file to a daemon-side path and stat it — any miss and
//     we do nothing (caller falls back to the permanent path, disclosed);
//  2. rename each into Root/<date>/<sceneID>/ — and if any rename fails,
//     the ones already done are renamed BACK. Every step before the
//     caller's metadata destroy is undoable.
//
// Returns the executed moves, or an error with everything rolled back.
func (t *TrashConfig) trashTargetFiles(target Target, now time.Time) ([]trashMove, error) {
	// Library-health latch: with the mount itself gone, every per-file stat
	// below would fail and read as "file missing" — the legitimate
	// permanent-fallback signal — when the truth is "nothing is visible
	// right now". Refuse instead; the caller surfaces the failure and the
	// delete can be retried when the mount returns.
	if t.LibraryRoot != "" {
		if _, err := os.Stat(t.LibraryRoot); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrLibraryUnavailable, err)
		}
	}
	type planned struct{ from, to string }
	var plan []planned
	destDir := filepath.Join(t.Root, now.Format("2006-01-02"), sanitizeSegment(target.SceneID))
	for _, f := range target.Files {
		dp := t.daemonPath(f.Path)
		if dp == "" {
			return nil, fmt.Errorf("path %q is not reachable from the daemon (mapping %q)", f.Path, t.Mapping)
		}
		if _, err := os.Stat(dp); err != nil {
			return nil, fmt.Errorf("stat %q: %w", dp, err)
		}
		plan = append(plan, planned{from: dp, to: filepath.Join(destDir, pathmap.Base(dp))})
	}
	if len(plan) == 0 {
		return nil, fmt.Errorf("target has no files to trash")
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("create trash dir: %w", err)
	}
	var done []trashMove
	for _, m := range plan {
		if err := os.Rename(m.from, m.to); err != nil {
			// Roll back what already moved so the scene's files are intact
			// for the permanent-path fallback (or for nothing at all).
			for i := len(done) - 1; i >= 0; i-- {
				_ = os.Rename(done[i].To, done[i].From)
			}
			return nil, fmt.Errorf("move to trash: %w", err)
		}
		done = append(done, trashMove{From: m.from, To: m.to})
	}
	return done, nil
}

// TrashLocal moves paths the daemon already owns (the purge's placed file
// or pack directory — no reverse mapping involved) into the trash, with the
// same all-or-nothing rollback as trashTargetFiles. label names the trash
// subdirectory (e.g. "grab-2990").
func (t *TrashConfig) TrashLocal(paths []string, label string, now time.Time) ([]trashMove, error) {
	destDir := filepath.Join(t.Root, now.Format("2006-01-02"), sanitizeSegment(label))
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("stat %q: %w", p, err)
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("nothing to trash")
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("create trash dir: %w", err)
	}
	var done []trashMove
	for _, p := range paths {
		to := filepath.Join(destDir, pathmap.Base(strings.TrimRight(p, `/\`)))
		if err := os.Rename(p, to); err != nil {
			for i := len(done) - 1; i >= 0; i-- {
				_ = os.Rename(done[i].To, done[i].From)
			}
			return nil, fmt.Errorf("move to trash: %w", err)
		}
		done = append(done, trashMove{From: p, To: to})
	}
	return done, nil
}

// restoreMoves undoes a set of trash moves (newest journal knows them).
func restoreMoves(moves []trashMove) error {
	var firstErr error
	for _, m := range moves {
		if err := os.MkdirAll(filepath.Dir(m.From), 0o755); err != nil && firstErr == nil {
			firstErr = err
			continue
		}
		if err := os.Rename(m.To, m.From); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// MovesFromDetail decodes the moves JSON a "trashed" journal entry carries.
func MovesFromDetail(detail string) ([]trashMove, error) {
	var moves []trashMove
	if err := json.Unmarshal([]byte(detail), &moves); err != nil {
		return nil, err
	}
	return moves, nil
}

// Restore puts a trashed entry's files back where they came from. Exposed
// for the restore endpoint; the journal entry's detail carries the moves.
func Restore(detail string) error {
	moves, err := MovesFromDetail(detail)
	if err != nil {
		return fmt.Errorf("decode trash moves: %w", err)
	}
	if len(moves) == 0 {
		return fmt.Errorf("entry has no recorded moves")
	}
	return restoreMoves(moves)
}

// sanitizeSegment keeps a scene id usable as a directory name — ids are
// numeric in practice, but the trash layout must not trust that.
func sanitizeSegment(s string) string {
	if s == "" {
		return "scene"
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		return r
	}, s)
}

// SweepTrash unlinks trash older than ttl, journalling every file it
// removes — even the final deletion leaves a record. Dated directories
// (Root/<yyyy-mm-dd>/…) older than the ttl are removed whole. Best-effort;
// returns the number of files permanently deleted.
//
// The caller gates it on trash being enabled and on a sane Root; a Root
// stat failure aborts the sweep rather than concluding "nothing to do" —
// a dropped mount must never look like an empty trash.
func SweepTrash(ctx context.Context, root string, ttl time.Duration, rec Recorder, now time.Time) (int, error) {
	if root == "" || ttl <= 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // never trashed anything yet
		}
		return 0, fmt.Errorf("read trash root: %w", err)
	}
	cutoff := now.Add(-ttl)
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		day, derr := time.Parse("2006-01-02", e.Name())
		if derr != nil {
			continue // not ours; leave foreign files alone
		}
		// A day-directory expires when the whole DAY is past the cutoff —
		// end-of-day, so nothing trashed late on day N dies early.
		if day.Add(24 * time.Hour).After(cutoff) {
			continue
		}
		dir := filepath.Join(root, e.Name())
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, werr error) error {
			if werr != nil || d.IsDir() {
				return nil
			}
			if rec != nil {
				_, _ = rec.JournalDestruction(ctx, "trash sweep", Target{
					Files: []File{{Path: path}},
				}, "destroyed", fmt.Sprintf("retention (%s) expired", ttl))
			}
			removed++
			return nil
		})
		if rerr := os.RemoveAll(dir); rerr != nil {
			return removed, fmt.Errorf("sweep %s: %w", dir, rerr)
		}
	}
	return removed, nil
}
