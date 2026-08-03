package placer

import (
	"fmt"
	"os"
	"strconv"
)

// Ownership is the umask/PUID/PGID contract: files forage creates
// should be readable (and usually writable) by the
// Stash instance and by the human, whatever uid the daemon happens to run
// as. Configured once at startup from FORAGER_PUID / FORAGER_PGID /
// FORAGER_UMASK; unset means "inherit the process defaults", which is
// what a bare-binary install wants.
//
// Chown is best-effort on purpose: an unprivileged process cannot give
// files away, and CIFS/NFS mounts often fix ownership themselves. A
// failure must never fail a placement that otherwise succeeded.
var (
	ownUID  = -1
	ownGID  = -1
	dirMode os.FileMode = 0o775
	// fileMode is applied to files winnow creates (the copy path; a
	// hardlink shares the source's mode by definition).
	fileMode os.FileMode = 0o664
)

// ConfigureOwnership sets the desired uid/gid and umask-derived modes.
// uid/gid < 0 leave ownership alone; umask "" leaves the default modes.
func ConfigureOwnership(uid, gid int, umask string) {
	ownUID, ownGID = uid, gid
	if umask == "" {
		return
	}
	m, err := strconv.ParseUint(umask, 8, 32)
	if err != nil {
		return
	}
	dirMode = os.FileMode(0o777 &^ m)
	fileMode = os.FileMode(0o666 &^ m)
}

// DirMode / FileMode expose the configured modes to the other packages
// that create library files.
func DirMode() os.FileMode  { return dirMode }
func FileMode() os.FileMode { return fileMode }

// Adopt applies the configured ownership and mode to a path winnow just
// created. Best-effort by design (see above).
func Adopt(path string, isDir bool) {
	mode := fileMode
	if isDir {
		mode = dirMode
	}
	_ = os.Chmod(path, mode)
	if ownUID >= 0 || ownGID >= 0 {
		_ = os.Chown(path, ownUID, ownGID)
	}
}

// spaceHeadroom is kept free beyond the release itself: a filesystem
// run to literally zero breaks everything else writing to it.
const spaceHeadroom = 1 << 30 // 1 GiB

// checkSpaceFor refuses a placement that would need more room than the
// library has. Skipped when source and destination share a device (a
// hardlink writes no bytes). An unreadable filesystem reads as "can't
// tell": let the copy try and fail honestly rather than blocking a
// placement on a statfs quirk.
func (p *Placer) checkSpaceFor(srcPath, destDir string, size int64) error {
	if sameDevice(srcPath, destDir) {
		return nil
	}
	free, err := freeSpace(p.libraryRoot)
	if err != nil {
		return nil
	}
	if need := uint64(size) + spaceHeadroom; free < need {
		return fmt.Errorf("not enough room in the library: this release needs %.1f GB (plus 1 GB headroom) but only %.1f GB is free",
			float64(size)/1e9, float64(free)/1e9)
	}
	return nil
}
