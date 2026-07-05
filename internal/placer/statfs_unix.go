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
