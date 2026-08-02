package poller

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/placer"
)

// Refusing a finished download that is a single unplaceable file.
//
// The placer's file-type allowlist (placer.Placeable) only bites while
// MIRRORING a directory: a multi-file release drops its passengers and places
// the rest. A single-file download has no such middle ground, and the placer
// cannot decide it alone — its two options are to place the file (an .exe
// landing in a library browsed from Windows over SMB, which is the exact
// thing the allowlist exists to prevent) or to return an error, which leaves
// the grab in "completed" so the next tick retries the identical refusal,
// forever. Only a layer that can fail a grab PERMANENTLY can end it, so the
// question is asked here, immediately before placement.

// refuseUnplaceableDownload returns a non-empty grab Reason when a finished
// download must not be placed at all, or "" to proceed.
//
// Deliberately narrow. It fires only on a source that is a single regular
// file whose extension the library policy rejects — the one case placement
// cannot handle. A directory is the placer's business (it filters per file).
// A stat failure returns "" as well: not seeing the file is a dropped mount
// or a client still moving it in, and reading "can't see it" as "junk" would
// fail grabs whose file is perfectly good. Place's own error path already
// retries those.
func refuseUnplaceableDownload(srcPath string) string {
	info, err := os.Stat(srcPath)
	if err != nil || info.IsDir() {
		return ""
	}
	name := filepath.Base(srcPath)
	if placer.Placeable(name) {
		return ""
	}
	ext := strings.ToLower(filepath.Ext(name))
	if ext == "" {
		ext = "(no extension)"
	}
	return grabs.RefusedPrefix + "the download is a single " + ext +
		" file, which forage never puts in the library — the release was not what it claimed to be"
}
