//go:build windows

package main

import "io/fs"

// fileID has no cheap equivalent on Windows through io/fs, so the tool falls
// back to name+size matching there. That is the weaker signal for a renamed
// hardlink but the stronger one for a copy, and this tool is meant to run
// inside the container against the mounted library anyway. Reporting "no id"
// is safe: an unmatched file is only ever classified as MORE precious, never
// as more disposable.
func fileID(fs.FileInfo) (uint64, bool) { return 0, false }
