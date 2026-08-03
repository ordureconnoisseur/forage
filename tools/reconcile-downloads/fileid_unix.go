//go:build !windows

package main

import (
	"io/fs"
	"syscall"
)

// fileID returns the inode, which identifies a file across every hardlink to
// it. That is how a library copy is recognised even when it was renamed after
// placement.
func fileID(info fs.FileInfo) (uint64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, false
	}
	return uint64(st.Ino), true
}
