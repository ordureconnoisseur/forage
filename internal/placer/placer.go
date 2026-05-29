// Package placer takes a finished download (file or folder reported by
// qBit / SAB) and lands it under <library_root>/<performer>/. Forage
// owns this step so users don't depend on download-client categories,
// arr-stack import orchestration, or filename templating to get scenes
// where Stash can see them.
//
// Hardlink-first: when source and destination are on the same
// filesystem, only an inode reference is added in the library —
// the original stays where the download client put it (still seedable
// for torrents). Falls back to a copy when cross-device. We never
// move; keeping the source intact is the whole point.
package placer

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type Placer struct {
	libraryRoot string
	log         *slog.Logger
}

// Result describes a single Place call.
type Result struct {
	Path string // final placed path, e.g. /library/Hazel Moore/release.mkv
	Mode string // "hardlink" | "copy" — empty if the destination already existed
}

func New(libraryRoot string, log *slog.Logger) *Placer {
	return &Placer{libraryRoot: libraryRoot, log: log}
}

// Configured returns true when the placer has somewhere to put files.
// Callers should skip the place stage when this is false.
func (p *Placer) Configured() bool {
	return p != nil && p.libraryRoot != ""
}

// LibraryRoot exposes the configured root for diagnostics.
func (p *Placer) LibraryRoot() string {
	if p == nil {
		return ""
	}
	return p.libraryRoot
}

// Place lands the source file (or folder) into
// <libraryRoot>/<performer>/<basename>. Returns the final path.
// Idempotent — re-running after a previous success returns the
// existing path without copying.
//
// performer is sanitised for filesystem safety. Empty performer falls
// back to "Unsorted" so grabs missing the field don't pollute the root.
func (p *Placer) Place(srcPath, performer string) (Result, error) {
	if !p.Configured() {
		return Result{}, errors.New("placer not configured")
	}
	if srcPath == "" {
		return Result{}, errors.New("source path empty")
	}
	if performer == "" {
		performer = "Unsorted"
	}
	info, err := os.Stat(srcPath)
	if err != nil {
		return Result{}, fmt.Errorf("stat src: %w", err)
	}
	destDir := filepath.Join(p.libraryRoot, sanitise(performer))
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("mkdir dest: %w", err)
	}

	destPath := filepath.Join(destDir, filepath.Base(srcPath))

	if info.IsDir() {
		// Multi-file release (a torrent that unpacked into a containing
		// folder). Mirror the tree as hardlinks; preserves any nested
		// structure without copying. Resume-safe: mirrorTree skips files
		// already present, so an interrupted run (daemon restart, or a
		// transient os.Link error on one of a pack's hundreds of files
		// aborting the walk) COMPLETES on the next call instead of being
		// mistaken for a finished placement. A re-run where everything is
		// already linked places nothing and reports idempotent (Mode "").
		mode, placed, err := p.mirrorTree(srcPath, destPath)
		if err != nil {
			return Result{}, err
		}
		if placed == 0 {
			return Result{Path: destPath}, nil
		}
		return Result{Path: destPath, Mode: mode}, nil
	}

	// Single file release. Suffix on collision so a re-grab with a
	// different indexer doesn't silently overwrite the existing copy.
	destPath = collisionSuffix(destPath)
	mode, err := linkOrCopy(srcPath, destPath)
	if err != nil {
		return Result{}, err
	}
	return Result{Path: destPath, Mode: mode}, nil
}

// sanitise strips characters that break filesystems on common
// platforms. Doesn't lowercase or transliterate — display names with
// apostrophes / accents stay readable.
func sanitise(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			b.WriteByte('_')
		default:
			if r < 0x20 {
				b.WriteByte('_')
				continue
			}
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	out = strings.Trim(out, ".")
	if out == "" {
		return "Unsorted"
	}
	return out
}

// linkOrCopy tries os.Link first (cheap, preserves seeding) and falls
// back to a copy on cross-device error. Any other failure propagates.
func linkOrCopy(src, dest string) (string, error) {
	err := os.Link(src, dest)
	if err == nil {
		return "hardlink", nil
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) && errors.Is(linkErr.Err, syscall.EXDEV) {
		if err := copyFile(src, dest); err != nil {
			return "", err
		}
		return "copy", nil
	}
	return "", fmt.Errorf("link: %w", err)
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(dest)
		return err
	}
	return out.Close()
}

// mirrorTree walks src and hardlinks every file into the matching dest
// path. On any cross-device fallback the overall mode flips to "copy".
// Returns the count of files it actually placed this pass — files that
// already exist at the destination are skipped, so the walk is
// resume-safe: a mirror interrupted partway (restart, transient CIFS
// link error) is finished by the next call rather than being treated as
// already-done with a half-populated directory. placed==0 means the tree
// was already fully present.
func (p *Placer) mirrorTree(src, dest string) (string, int, error) {
	mode := "hardlink"
	placed := 0
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		destPath := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(destPath, 0o755)
		}
		// Already linked/copied by a prior pass — skip so an interrupted
		// mirror resumes instead of erroring on the existing file.
		if _, err := os.Stat(destPath); err == nil {
			return nil
		}
		m, err := linkOrCopy(path, destPath)
		if err != nil {
			return err
		}
		if m == "copy" {
			mode = "copy"
		}
		placed++
		return nil
	})
	if err != nil {
		return "", 0, fmt.Errorf("mirror tree: %w", err)
	}
	return mode, placed, nil
}

// collisionSuffix returns an unused filename by appending " (N)" to
// the stem if the original exists. Stops at 1000 to avoid infinite
// loops on pathological inputs; the caller surfaces any subsequent
// write failure as a place_error.
func collisionSuffix(path string) string {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return path
	}
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	for i := 2; i < 1000; i++ {
		try := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if _, err := os.Stat(try); errors.Is(err, fs.ErrNotExist) {
			return try
		}
	}
	return path
}
