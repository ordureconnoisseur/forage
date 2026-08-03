//go:build !windows

package placer

import (
	"fmt"
	"syscall"
)

// freeSpace returns the bytes available to the caller on the filesystem
// containing path.
func freeSpace(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// sameDevice reports whether two paths live on one filesystem, i.e.
// whether a hardlink between them is possible. Errors read as "not the
// same": callers only use this to SKIP a space check, so being wrong
// costs a redundant check, never a bad placement.
func sameDevice(a, b string) bool {
	var sa, sb syscall.Stat_t
	if err := syscall.Stat(a, &sa); err != nil {
		return false
	}
	if err := syscall.Stat(b, &sb); err != nil {
		return false
	}
	return sa.Dev == sb.Dev
}
