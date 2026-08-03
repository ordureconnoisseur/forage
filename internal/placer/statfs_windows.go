//go:build windows

package placer

import (
	"fmt"
	"syscall"
	"unsafe"
)

// freeSpace returns the bytes available to the caller on the volume
// containing path. The daemon deploys to macOS; this port exists so the
// module builds and tests on the Windows dev box.
func freeSpace(path string) (uint64, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	var avail, total, free uint64
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")
	ret, _, callErr := proc.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&avail)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&free)),
	)
	if ret == 0 {
		return 0, fmt.Errorf("statfs %s: %w", path, callErr)
	}
	return avail, nil
}

// sameDevice reports whether two paths share a volume. Windows is a dev
// platform for the daemon, so this is the conservative answer: assume
// they don't, which only means the space check always runs.
func sameDevice(a, b string) bool { return false }
