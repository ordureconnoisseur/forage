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
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/ordureconnoisseur/forager/internal/torrentmeta"
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
	return freeSpace(p.libraryRoot)
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
// back to the unfiled folder (see unfiled.go) so grabs missing the field
// don't pollute the root.
func (p *Placer) Place(srcPath, performer string) (Result, error) {
	if !p.Configured() {
		return Result{}, errors.New("placer not configured")
	}
	if srcPath == "" {
		return Result{}, errors.New("source path empty")
	}
	if performer == "" {
		performer = p.unfiledDir()
	}
	info, err := os.Stat(srcPath)
	if err != nil {
		return Result{}, fmt.Errorf("stat src: %w", err)
	}
	folder := sanitise(performer)
	if folder == "" {
		folder = p.unfiledDir()
	}
	destDir := filepath.Join(p.libraryRoot, folder)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("mkdir dest: %w", err)
	}
	p.sweepStalePartials(destDir)

	// Un-hide the leaf name so a dot-hidden single file becomes a scannable
	// scene (a pack folder name is un-hidden too, harmlessly — pack dirs aren't
	// dot-prefixed in practice).
	destPath := filepath.Join(destDir, unhideBase(filepath.Base(srcPath)))

	if info.IsDir() {
		// Multi-file release (a torrent that unpacked into a containing
		// folder). Mirror the tree as hardlinks; preserves any nested
		// structure without copying. Resume-safe: mirrorTree skips files
		// already present, so an interrupted run (daemon restart, or a
		// transient os.Link error on one of a pack's hundreds of files
		// aborting the walk) COMPLETES on the next call instead of being
		// mistaken for a finished placement. A re-run where everything is
		// already linked places nothing and reports idempotent (Mode "").
		//
		// Like singleFileTarget for files, dirTarget distinguishes "our own
		// earlier (possibly partial) mirror" from a DIFFERENT release whose
		// folder happens to share the name (pack folders are routinely just
		// the performer's name) — merging those would interleave two
		// releases under one path, and a later purge of either grab would
		// sweep the other's files.
		destPath, err = p.dirTarget(destPath, srcPath)
		if err != nil {
			return Result{}, err
		}
		mode, placed, skipped, err := p.mirrorTree(srcPath, destPath)
		if err != nil {
			return Result{}, err
		}
		if placed == 0 && skipped > 0 {
			// Everything in the release was refused by the file-type policy.
			// That is a decided outcome, not an incomplete source, so it must
			// NOT take the retry path below: a fake release consisting of an
			// installer and a .nfo would otherwise be re-placed every tick
			// forever. Report the (empty) destination and let the grab fail
			// confirmation, which the cull pass already handles.
			if p.log != nil {
				p.log.Warn("placer placed nothing: no file in the release is a media type",
					"src", srcPath, "skipped", skipped)
			}
			return Result{Path: destPath, Mode: mode}, nil
		}
		if placed == 0 {
			// placed==0 is a legitimate no-op ONLY when the destination
			// already holds the tree (a re-run of a finished placement). If
			// the destination is still empty, the source had nothing to
			// mirror — the download client reported the release complete
			// before it finished moving files into place (moving a large
			// pack across a CIFS mount isn't instant). Recording success
			// would strand the grab: marked "placed" against an empty
			// folder, it never re-places and Stash never sees the files.
			// Return an error so the poller keeps the grab in "completed"
			// and retries once the source is populated.
			if dirHasNoFiles(destPath) {
				return Result{}, fmt.Errorf("nothing placed and destination is empty (source %q not finished moving in)", srcPath)
			}
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

// unhideBase strips leading dots from a filename so a dot-hidden source file
// (e.g. balbums.st names its clips ".y0hw..._source.mp4") lands VISIBLE in the
// library. Stash's scanner skips hidden files, so a hidden video never becomes
// a scene — the whole point of placement. Only the leading dots go; an all-dots
// name is returned unchanged so it can't collapse to "".
func unhideBase(name string) string {
	t := strings.TrimLeft(name, ".")
	if t == "" {
		return name
	}
	return t
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
	// "" when the name sanitises away entirely. The CALLER substitutes the
	// fallback, so a pathological name lands in the same bin as a missing
	// one rather than inventing a second one on a library using the legacy
	// folder.
	return strings.Trim(strings.TrimSpace(b.String()), ".")
}

// linkOrCopy tries os.Link first (cheap, preserves seeding) and falls back to
// an atomic copy when the hardlink can't be made — cross-device (EXDEV), too
// many links (EMLINK), or a filesystem that doesn't support hardlinks at all
// (many CIFS/SMB and FUSE mounts). Rather than enumerate errnos, any link
// failure attempts the copy and only errors if the copy ALSO fails — except
// EEXIST, which is not a "can't hardlink here" condition: the destination
// appeared between the caller's exists-check and the link (a poller tick and
// the re-file API endpoint can place concurrently). Falling through to the
// copy would let its rename replace the file the other placement just made.
func linkOrCopy(src, dest string) (string, error) {
	err := os.Link(src, dest)
	if err == nil {
		return "hardlink", nil
	}
	if errors.Is(err, fs.ErrExist) {
		si, serr := os.Stat(src)
		di, derr := os.Stat(dest)
		if serr == nil && derr == nil && (os.SameFile(si, di) || si.Size() == di.Size()) {
			// The concurrent placement put the same content here — done.
			return "hardlink", nil
		}
		return "", fmt.Errorf("destination %s appeared during placement with different content, refusing to overwrite it", dest)
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
	// CreateTemp opens the file 0600 and that mode survives the rename into
	// place, which left copy-fallback placements unreadable to a Stash
	// running as a different uid (hardlinks were unaffected, since they
	// share the source's mode). Open up to the conventional 0644.
	if err := tmp.Chmod(0o644); err != nil {
		cleanup()
		return err
	}
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
	// os.Rename replaces an existing dest silently. The caller checked dest
	// was free before starting, but on copy-only filesystems (where the
	// EEXIST hardlink guard can't run) a concurrent placement may have landed
	// in the meantime; re-check just before the rename to shrink that window
	// from the whole copy's duration to an instant.
	if _, err := os.Lstat(dest); err == nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("destination %s appeared during copy, refusing to overwrite it", dest)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// What forage is willing to put in your library.
//
// mirrorTree used to hardlink every file in a release folder. Releases carry
// passengers: .nfo and artwork, which are harmless, but also .exe, .lnk, .scr
// and desktop.ini, which are not. forage never executes any of it, so the
// daemon is not the target — the library is. It lives on a NAS shared over
// SMB, and a .lnk or .url file makes Explorer fetch its icon from wherever the
// file says, which leaks NTLM credentials to a remote host merely by browsing
// the folder. No one has to click anything.
//
// An allowlist rather than a blocklist. A blocklist has to enumerate every
// dangerous extension and stays wrong until someone remembers to update it;
// this only has to enumerate what a media library is actually for. The cost of
// being wrong is inverted too: a missing entry here means a file did not get
// placed and is still sitting in the download client, which is visible and
// recoverable, where a missing blocklist entry means an executable is in a
// folder you browse from Windows.
//
// Generous on video containers on purpose. Stash will only make scenes from
// some of these, but placing an unusual container costs nothing and refusing
// one strands a legitimate grab.
//
// The video half is TAKEN from torrentmeta rather than written out again.
// The grab layer refuses a release whose file list holds no video before it
// downloads anything, and it asks torrentmeta that question; a second copy
// here drifted from that one and refused .3gp/.mpv/.m2v/.m4p releases the
// placer would have accepted. Two lists answering one question is the bug,
// so there is one list. Nothing dangerous can arrive this way without
// TestIsPlaceable failing: it asserts the executable/shortcut set stays out.
//
// The non-video half stays literal here, because "what else may sit next to
// a scene" is the library's business alone.
var placeableExts = func() map[string]bool {
	m := make(map[string]bool, 40)
	for _, ext := range torrentmeta.VideoExtensions() {
		m[ext] = true
	}
	for _, ext := range []string{
		// subtitles: Stash reads these next to the scene
		".srt", ".sub", ".idx", ".vtt", ".ass", ".ssa",
		// artwork and release notes: inert, and some tooling expects them
		".jpg", ".jpeg", ".png", ".webp", ".gif", ".bmp", ".nfo", ".txt",
	} {
		m[ext] = true
	}
	return m
}()

// Placeable reports whether a release file may enter the library. Extension
// only: content sniffing would be a stronger check and a much bigger promise,
// and the threat here is what Windows does with a NAME, not what the bytes
// turn out to be.
//
// Exported because the same question has to be asked one layer up. Inside
// mirrorTree an unplaceable file is simply skipped, but a SINGLE-file
// download has nothing to skip past: the placer's only choices are to place
// it or to error forever, so the grab layer asks this before handing the file
// over and refuses the grab instead. One list, two callers.
func Placeable(name string) bool {
	return placeableExts[strings.ToLower(filepath.Ext(name))]
}

// videoExts is the set of extensions Stash treats as scenes. Sample
// detection only applies to these — a "sample" in a non-video name (a
// proof image, a .nfo) never becomes a scene, so it's harmless clutter we
// don't bother filtering.
var videoExts = map[string]bool{
	".mp4": true, ".mkv": true, ".avi": true, ".wmv": true, ".m4v": true,
	".mov": true, ".mpg": true, ".mpeg": true, ".ts": true, ".flv": true,
	".webm": true, ".m2ts": true,
}

func isVideoFile(name string) bool {
	return videoExts[strings.ToLower(filepath.Ext(name))]
}

// sampleNameRe matches a "sample" token in a filename — bounded by
// non-letters so it fires on "release-sample.mp4" / "sample_01.mkv" but not
// on a performer/word that merely contains the letters (e.g. "…example…").
var sampleNameRe = regexp.MustCompile(`(?i)(^|[^a-z])sample([^a-z]|$)`)

func isSampleDirName(name string) bool {
	n := strings.ToLower(name)
	return n == "sample" || n == "samples"
}

// isSampleVideo reports whether rel (a path relative to the release root) is
// a preview sample clip that should not be placed as a library scene. Two
// signals, tuned for precision so a legitimate video is never dropped:
//   - it lives under a "sample"/"samples" directory (scene-group releases put
//     the preview there); or
//   - its name carries a "sample" token AND it's under half the size of the
//     release's main video — a real preview is always a small fraction, while
//     a legit standalone clip that merely has "sample" in its name (some
//     OnlyFans titles do) is the largest video and so is kept.
//
// The size guard needs largest > 0 (a main video exists); a sample-only
// release has no larger sibling to compare against and is left to the grab
// layer to reject, not silently dropped here.
func isSampleVideo(rel string, size, largest int64) bool {
	if !isVideoFile(rel) {
		return false
	}
	segs := strings.Split(filepath.ToSlash(rel), "/")
	for _, seg := range segs[:len(segs)-1] {
		if isSampleDirName(seg) {
			return true
		}
	}
	if sampleNameRe.MatchString(filepath.Base(rel)) && largest > 0 && size*2 < largest {
		return true
	}
	return false
}

// largestVideoSize returns the size of the biggest video under root, ignoring
// anything in a sample directory (so a sample can't set the bar it's compared
// against). Used to give isSampleVideo a main-video baseline.
func largestVideoSize(root string) int64 {
	var max int64
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if isSampleDirName(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !isVideoFile(d.Name()) {
			return nil
		}
		if info, e := d.Info(); e == nil && info.Size() > max {
			max = info.Size()
		}
		return nil
	})
	return max
}

// mirrorTree walks src and hardlinks every file into the matching dest
// path. On any cross-device fallback the overall mode flips to "copy".
// Returns the count of files it actually placed this pass — files that
// already exist at the destination are skipped, so the walk is
// resume-safe: a mirror interrupted partway (restart, transient CIFS
// link error) is finished by the next call rather than being treated as
// already-done with a half-populated directory. placed==0 means the tree
// was already fully present.
//
// Sample preview clips are skipped (see isSampleVideo): a scene-group
// release folder carries the full video plus a short sample, and mirroring
// both makes Stash create a junk, un-identifiable second scene from the
// sample. Skipping them here — not after the fact — keeps the library clean
// without a downstream sweep.
func (p *Placer) mirrorTree(src, dest string) (string, int, int, error) {
	mode := "hardlink"
	placed := 0
	skipped := 0
	largest := largestVideoSize(src)
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Don't recreate a sample directory or descend into it.
			if isSampleDirName(d.Name()) {
				return fs.SkipDir
			}
			return os.MkdirAll(filepath.Join(dest, rel), 0o755)
		}
		// Un-hide the leaf so dot-hidden clips become scannable scenes; keep the
		// directory structure as-is (dirMirrors reverses the leaf un-hide).
		destPath := filepath.Join(dest, filepath.Dir(rel), unhideBase(filepath.Base(rel)))
		// Skip preview sample clips so they never become a scene.
		if info, e := d.Info(); e == nil && isSampleVideo(rel, info.Size(), largest) {
			if p.log != nil {
				p.log.Info("placer skipped sample clip", "path", path, "size", info.Size())
			}
			return nil
		}
		// Refuse the passengers. Counted, not silent: a release that is
		// ENTIRELY unplaceable must be distinguishable from one whose source
		// has not finished moving in, or the caller retries it forever.
		if !Placeable(d.Name()) {
			skipped++
			if p.log != nil {
				p.log.Info("placer skipped file type", "path", path,
					"ext", strings.ToLower(filepath.Ext(d.Name())))
			}
			return nil
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
		return "", 0, 0, fmt.Errorf("mirror tree: %w", err)
	}
	return mode, placed, skipped, nil
}

// dirHasNoFiles reports whether dir contains no regular files — it's empty
// or holds only (empty) subdirectories. Used to tell a premature tree
// placement (download client reported complete before moving files in)
// from a genuine idempotent re-run, which leaves the whole tree present.
func dirHasNoFiles(dir string) bool {
	hasFile := false
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: keep looking rather than crash
		}
		if !d.IsDir() {
			hasFile = true
			return fs.SkipAll
		}
		return nil
	})
	return !hasFile
}

// dirTarget picks the destination directory for a tree placement by
// walking the "name", "name (2)", ... sequence, the directory analogue
// of singleFileTarget. A candidate is reused when everything in it
// mirrors the source (see dirMirrors) — that's a prior, possibly
// interrupted, placement of this same tree, and reusing it preserves
// mirrorTree's resume behaviour. A candidate holding anything else
// belongs to a different release and is skipped.
func (p *Placer) dirTarget(path, src string) (string, error) {
	try := path
	for i := 2; i <= 1000; i++ {
		di, err := os.Stat(try)
		if errors.Is(err, fs.ErrNotExist) {
			return try, nil
		}
		if err != nil {
			return "", fmt.Errorf("stat dest dir: %w", err)
		}
		if di.IsDir() {
			ours, err := dirMirrors(try, src)
			if err != nil {
				return "", err
			}
			if ours {
				return try, nil
			}
		}
		try = fmt.Sprintf("%s (%d)", path, i)
	}
	return "", fmt.Errorf("no free directory name for %s after 1000 tries", path)
}

// errDirConflict signals dirMirrors found content that isn't ours.
var errDirConflict = errors.New("dir holds foreign content")

// dirMirrors reports whether everything under dest mirrors the same
// relative path in src — by inode (an earlier hardlink) or size (an
// earlier copy; the relative path already matches, so an equal-size
// different file at the exact same nested name is not a realistic
// collision). An empty directory mirrors trivially (an interrupted run
// that only got as far as MkdirAll). Stranded ".forage-copy-*.partial"
// temps are ours (a crashed copy fallback), not foreign content.
//
// Known limitation, accepted deliberately: a SUPERSET release (same
// folder name, containing all of dest's files plus new ones — a v2
// re-rip) is content-indistinguishable from our own interrupted mirror,
// so it reuses the dir and the two grabs share a placement. Refusing
// that shape would also refuse every genuine resume, which matters far
// more often.
func dirMirrors(dest, src string) (bool, error) {
	err := filepath.WalkDir(dest, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if isPartialTemp(d.Name()) {
			return nil
		}
		rel, err := filepath.Rel(dest, path)
		if err != nil {
			return err
		}
		si, err := os.Stat(filepath.Join(src, rel))
		if err != nil {
			// The dest leaf may be an un-hidden name (leading dots stripped on
			// place) whose source is still dot-hidden; try the re-hidden basename
			// before concluding the file is foreign. Covers the single-leading-dot
			// case (the real one); deeper dot nesting falls through as a conflict.
			si, err = os.Stat(filepath.Join(src, filepath.Dir(rel), "."+filepath.Base(rel)))
		}
		if err != nil {
			// dest holds a file the source tree doesn't have.
			return errDirConflict
		}
		di, err := d.Info()
		if err != nil {
			return err
		}
		if os.SameFile(si, di) || si.Size() == di.Size() {
			return nil
		}
		return errDirConflict
	})
	if errors.Is(err, errDirConflict) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect dest dir %s: %w", dest, err)
	}
	return true, nil
}

func isPartialTemp(name string) bool {
	return strings.HasPrefix(name, ".forage-copy-") && strings.HasSuffix(name, ".partial")
}

// stalePartialAge is how old a ".forage-copy-*.partial" temp must be
// before the sweep collects it. An in-flight copy's temp keeps a fresh
// mtime (io.Copy is appending to it), so an hour-old one can only be the
// leftover of a crashed copy (SIGKILL, OOM, power loss). copyFile cleans
// its temp on every in-process failure, but nothing else ever would:
// retries mint fresh CreateTemp names, Stash ignores the extension, and
// multi-GB strands would otherwise sit in the library forever.
const stalePartialAge = time.Hour

// sweepStalePartials best-effort removes crashed copies' temp files
// under dir (the performer folder, so pack subdirectories are covered).
func (p *Placer) sweepStalePartials(dir string) {
	cutoff := time.Now().Add(-stalePartialAge)
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !isPartialTemp(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().After(cutoff) {
			return nil
		}
		if err := os.Remove(path); err == nil && p.log != nil {
			p.log.Info("swept stale copy temp", "path", path, "size", info.Size())
		}
		return nil
	})
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
