package poller

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ordureconnoisseur/forager/internal/clienterr"
	"github.com/ordureconnoisseur/forager/internal/pathmap"
)

// Noticing when a download stops being worth keeping.
//
// The download folder accumulates two things nothing else reports. Re-encodes
// strand their originals: the transcode writes a new file at the library path,
// which breaks the hardlink, and the full-size original sits in the download
// tree forever with nothing pointing at it. And imports that never happened
// leave a completed download that reached no library at all, which is the only
// copy of something paid for and forgotten. On the reference library those
// were 1.26 TB and 1,045 files respectively, invisible for years.
//
// The obvious implementation walks both trees and compares. That is what the
// reconcile-downloads TOOL does, and at 323,539 library files it takes about
// ten minutes of solid I/O — fine to run by hand on a quiet box, completely
// wrong as a scheduled pass inside the daemon, where it would compete with
// placement and Stash's own scans and make forage the reason the NAS is slow.
//
// So this does not walk the library at all. Stash has already indexed it, and
// answers "is there a library file called this" per-candidate from a database.
// And it does not re-examine the whole download tree either: a cursor in the
// meta table records where the last pass got to, and only files written since
// then are considered. Steady state is a handful of new files per pass, each
// costing one indexed lookup.
//
// Report-only. It never deletes anything.

const (
	// reconcileCursorKey stores the unix time of the last completed pass.
	reconcileCursorKey = "reconcile_downloads_cursor"
	// reconcileInterval is deliberately slow. Nothing here is urgent: a
	// stranded original wastes space, it does not break anything, and the
	// point of the cursor is that a slow cadence costs nothing extra.
	downloadsReconcileInterval = 6 * time.Hour
	// reconcileMaxPerPass caps the work a single pass can do, so a first run
	// against a decade of downloads cannot monopolise a tick. What is left
	// is picked up next time, because the cursor only advances over files
	// actually examined.
	reconcileMaxPerPass = 400
)

// ReconcileVerdict is what a single download file turned out to be.
type ReconcileVerdict string

const (
	// VerdictSuperseded: the library holds a SMALLER file of the same name.
	// The signature of a re-encode that broke its hardlink.
	VerdictSuperseded ReconcileVerdict = "superseded"
	// VerdictDuplicate: the library holds this file at the same size.
	VerdictDuplicate ReconcileVerdict = "duplicate"
	// VerdictVariant: same name in the library, but not smaller. Could be a
	// better copy there, could be an unrelated file with a common name; not
	// something to act on either way.
	VerdictVariant ReconcileVerdict = "variant"
	// VerdictOrphan: no library file by that name. Downloaded and never
	// imported, so this is the ONLY copy and must never be treated as spare.
	VerdictOrphan ReconcileVerdict = "orphan"
	// VerdictSkip: not a candidate (in progress, not media).
	VerdictSkip ReconcileVerdict = "skip"
)

// classifyAgainstLibrary decides a download file's verdict from its own size
// and the sizes of the library files sharing its name.
//
// Pure, because this is the judgement worth pinning: "smaller in the library"
// and "the only copy" are one comparison apart, and they are the difference
// between reclaiming a stranded original and deleting something irreplaceable.
func classifyAgainstLibrary(size int64, librarySizes []int64) ReconcileVerdict {
	if len(librarySizes) == 0 {
		return VerdictOrphan
	}
	smaller := false
	for _, ls := range librarySizes {
		if ls == size {
			// An identical size is the same content: a hardlink, or a copy
			// the placer made cross-device. Either way the library has it.
			return VerdictDuplicate
		}
		if ls > 0 && ls < size {
			smaller = true
		}
	}
	if smaller {
		return VerdictSuperseded
	}
	return VerdictVariant
}

// reconcileMediaExt is what counts as content worth reconciling.
var reconcileMediaExt = map[string]bool{
	".mp4": true, ".mkv": true, ".mov": true, ".wmv": true, ".avi": true,
	".m4v": true, ".ts": true, ".flv": true, ".mpg": true, ".mpeg": true,
	".webm": true, ".rmvb": true, ".asf": true, ".m2ts": true,
}

// downloadInProgress reports whether the client is still writing this file.
// Nothing half-written can be judged against the library.
func downloadInProgress(path string) bool {
	q := strings.ReplaceAll(path, `\`, "/")
	return strings.Contains(q, "/incomplete/") ||
		strings.Contains(q, "/__ADMIN__/") ||
		strings.HasSuffix(q, ".part") ||
		strings.HasSuffix(q, ".!qB")
}

// reconcileCandidate reports whether a walked entry is worth a lookup at all.
func reconcileCandidate(path string, modTime, cursor time.Time) bool {
	if downloadInProgress(path) {
		return false
	}
	if !reconcileMediaExt[strings.ToLower(filepath.Ext(path))] {
		return false
	}
	// The cursor is the whole reason this is cheap. Everything examined by an
	// earlier pass is skipped unless it has been rewritten since.
	return modTime.After(cursor)
}

// reconcileDownloads runs one incremental pass.
func (p *Poller) reconcileDownloads(ctx context.Context) {
	cfg := p.pool.Settings()
	root := strings.TrimSpace(cfg.DownloadRoot)
	if root == "" || !p.libraryHealthy() {
		return
	}
	sc := p.pool.Stash()
	if sc == nil {
		return
	}
	cursor := p.downloadsCursor(ctx)
	started := time.Now()

	counts := map[ReconcileVerdict]int{}
	var supersededBytes, orphanBytes int64
	examined := 0
	newest := cursor

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if examined >= reconcileMaxPerPass {
			return filepath.SkipAll
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if !reconcileCandidate(path, info.ModTime(), cursor) {
			return nil
		}
		examined++
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		v := p.verdictFor(ctx, path, info.Size(), cfg.StashPathMapping)
		counts[v]++
		switch v {
		case VerdictSuperseded:
			supersededBytes += info.Size()
		case VerdictOrphan:
			orphanBytes += info.Size()
		}
		return nil
	})
	if err != nil {
		p.log.Warn("reconcile downloads: walk", "root", root, "err", err)
		return
	}
	if examined == 0 {
		p.setDownloadsCursor(ctx, started)
		return
	}
	// Advance only over what was actually examined. A capped pass leaves the
	// cursor at the newest file it reached, so the remainder is picked up
	// next time instead of being skipped forever.
	if examined >= reconcileMaxPerPass {
		p.setDownloadsCursor(ctx, newest)
	} else {
		p.setDownloadsCursor(ctx, started)
	}
	p.log.Info("reconcile downloads",
		"examined", examined,
		"superseded", counts[VerdictSuperseded],
		"superseded_gb", supersededBytes/(1<<30),
		"orphan", counts[VerdictOrphan],
		"orphan_gb", orphanBytes/(1<<30),
		"duplicate", counts[VerdictDuplicate],
		"variant", counts[VerdictVariant],
		"ms", time.Since(started).Milliseconds())
	if counts[VerdictOrphan] > 0 {
		p.log.Info("reconcile: downloads that never reached the library",
			"count", counts[VerdictOrphan], "gb", orphanBytes/(1<<30),
			"note", "these are the ONLY copy; they need filing, not deleting")
	}
}

// verdictFor asks Stash whether the library holds this basename, and stats
// whatever it names to get the sizes.
//
// Stash rather than a filesystem walk: it has the library indexed already, and
// this is the difference between a lookup and 323,539 stats.
func (p *Poller) verdictFor(ctx context.Context, path string, size int64, mapping string) ReconcileVerdict {
	sc := p.pool.Stash()
	if sc == nil {
		return VerdictSkip
	}
	match, err := sc.FindSceneByPathContains(ctx, filepath.Base(path))
	if err != nil {
		if errors.Is(err, clienterr.ErrNotFound) {
			return VerdictOrphan
		}
		return VerdictSkip // Stash unreachable: no verdict rather than a wrong one
	}
	if match == nil {
		return VerdictOrphan
	}
	var sizes []int64
	for _, lp := range match.FilePaths {
		local := pathmap.Reverse(lp, mapping)
		if local == "" {
			local = lp
		}
		// The download file itself can be what Stash indexed when the download
		// folder lives inside the library. Comparing a file to itself would
		// call every download a duplicate of itself.
		if local == path {
			continue
		}
		if st, serr := os.Stat(local); serr == nil {
			sizes = append(sizes, st.Size())
		}
	}
	return classifyAgainstLibrary(size, sizes)
}

func (p *Poller) downloadsCursor(ctx context.Context) time.Time {
	var v string
	err := p.db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = ?`, reconcileCursorKey).Scan(&v)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			p.log.Warn("reconcile cursor read", "err", err)
		}
		return time.Time{}
	}
	n, cerr := strconv.ParseInt(v, 10, 64)
	if cerr != nil {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

func (p *Poller) setDownloadsCursor(ctx context.Context, t time.Time) {
	if _, err := p.db.ExecContext(ctx, `
		INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		reconcileCursorKey, strconv.FormatInt(t.Unix(), 10)); err != nil {
		p.log.Warn("reconcile cursor write", "err", err)
	}
}
