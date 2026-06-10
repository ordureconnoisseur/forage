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

// FreeSpace returns the bytes available on the filesystem backing the
// library root — used for a disk-space preflight before a grab. Because
// placement hardlinks (staging and library share a filesystem in the
// recommended setup), free space here reflects what the download needs.
func (p *Placer) FreeSpace() (uint64, error) {
	if !p.Configured() {
		return 0, errors.New("placer not configured")
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(p.libraryRoot, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", p.libraryRoot, err)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// HardlinkResult reports whether placement from a given download dir into
// the library would hardlink (cheap, no extra space, seeds keep working) or
// fall back to a copy (different filesystem → double storage), or fail.
type HardlinkResult struct {
	OK       bool   // a probe ran to a conclusion (Hardlink known either way)
	Hardlink bool   // true = same filesystem, real hardlink; false = cross-device copy
	Reason   string // human explanation, esp. on !OK
}

// HardlinkProbe tests whether a file in downloadDir can be hardlinked into
// the library root — the real question behind "will placement work without
// doubling my storage?". It writes a tiny temp file in downloadDir, tries
// os.Link into the library, and reports the outcome. Both temp files are
// cleaned up. downloadDir is the client's complete/save path AS FORAGE SEES
// IT (same mount namespace), so this answers the question for forage's own
// placement, not the client's view.
func (p *Placer) HardlinkProbe(downloadDir string) HardlinkResult {
	if !p.Configured() {
		return HardlinkResult{Reason: "library root not set"}
	}
	if strings.TrimSpace(downloadDir) == "" {
		return HardlinkResult{Reason: "no download path to test against"}
	}
	if _, err := os.Stat(downloadDir); err != nil {
		return HardlinkResult{Reason: "download path not found from forage: " + downloadDir +
			" (is the client's save dir mounted into forage at this path?)"}
	}
	src := filepath.Join(downloadDir, ".forage-hardlink-probe")
	if err := os.WriteFile(src, []byte("probe"), 0o644); err != nil {
		return HardlinkResult{Reason: "download path not writable from forage: " + err.Error()}
	}
	defer os.Remove(src)
	dst := filepath.Join(p.libraryRoot, ".forage-hardlink-probe")
	_ = os.Remove(dst) // clear any stale probe
	err := os.Link(src, dst)
	if err == nil {
		os.Remove(dst)
		return HardlinkResult{OK: true, Hardlink: true,
			Reason: "same filesystem — placement will hardlink (no extra space)"}
	}
	// Link failed: distinguish cross-device (copy fallback, still works) from
	// a real error (e.g. library not writable).
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) && errors.Is(linkErr.Err, syscall.EXDEV) {
		return HardlinkResult{OK: true, Hardlink: false,
			Reason: "download dir and library are on different filesystems — placement will COPY (uses double the space). Put both on one mount for hardlinks."}
	}
	return HardlinkResult{Reason: "couldn't link into the library: " + err.Error()}
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

	// Single file release. Re-running after a placement whose grab update
	// was lost (a crash between place and the DB write, or a stale-CAS
	// dropped tick) must reclaim the existing copy, not mint "name (2).ext"
	// on every pass. A genuinely different file at the name still gets the
	// collision suffix so a re-grab from another indexer can't silently
	// overwrite the existing copy.
	destPath, already := singleFileTarget(destPath, info)
	if already {
		return Result{Path: destPath}, nil
	}
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

// linkOrCopy tries os.Link first (cheap, preserves seeding) and falls back to
// an atomic copy when the hardlink can't be made — cross-device (EXDEV), too
// many links (EMLINK), or a filesystem that doesn't support hardlinks at all
// (many CIFS/SMB and FUSE mounts). Rather than enumerate errnos, any link
// failure attempts the copy and only errors if the copy ALSO fails.
func linkOrCopy(src, dest string) (string, error) {
	if err := os.Link(src, dest); err == nil {
		return "hardlink", nil
	}
	if err := copyFile(src, dest); err != nil {
		return "", fmt.Errorf("hardlink and copy fallback both failed: %w", err)
	}
	return "copy", nil
}

// copyFile copies src to dest ATOMICALLY: write to a temp sibling in dest's
// directory, fsync, then rename into place. A crash mid-copy (SIGKILL, OOM,
// power loss) leaves only the temp — never a truncated file at the final
// path that mirrorTree's os.Stat skip or singleFileTarget would mistake for a
// complete copy. The temp shares dest's directory so the rename stays on one
// filesystem (atomic).
func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".forage-copy-*.partial")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpName) }
	if _, err := io.Copy(tmp, in); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
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

// singleFileTarget picks the destination for a single-file placement by
// walking the "name.ext", "name (2).ext", ... sequence. An existing entry
// that is already the source counts as a prior placement and is reclaimed
// as the idempotent result: os.SameFile catches an earlier hardlink, and an
// equal size catches an earlier copy (the basename already matches, so an
// equal-size different release at this exact name is not a realistic
// collision). Returns (path, true) for a prior placement to reuse, or
// (path, false) for a free name to place into. Stops at 1000 to avoid
// unbounded scans on pathological inputs; the caller surfaces any
// subsequent write failure as a place_error.
func singleFileTarget(path string, src os.FileInfo) (string, bool) {
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	try := path
	for i := 2; i <= 1000; i++ {
		di, err := os.Stat(try)
		if errors.Is(err, fs.ErrNotExist) {
			return try, false
		}
		if err == nil && (os.SameFile(src, di) || di.Size() == src.Size()) {
			return try, true
		}
		try = fmt.Sprintf("%s (%d)%s", stem, i, ext)
	}
	return try, false
}
