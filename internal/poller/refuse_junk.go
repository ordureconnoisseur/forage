package poller

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/placer"
	"github.com/ordureconnoisseur/forager/internal/torrentmeta"
)

// Refusing a finished download that is a single unplaceable file.
//
// The placer's file-type allowlist (placer.Placeable) only bites while
// MIRRORING a directory: a multi-file release drops its passengers and places
// the rest. A single-file download has no such middle ground, and the placer
// cannot decide it alone. Its two options are to place the file (an .exe
// landing in a library browsed from Windows over SMB, which is the exact
// thing the allowlist exists to prevent) or to return an error, which leaves
// the grab in "completed" so the next tick retries the identical refusal,
// forever. Only a layer that can fail a grab PERMANENTLY can end it, so the
// question is asked here, immediately before placement.
//
// This is a BACKSTOP, not the main screen. A torrent forage fetched itself is
// judged from its file list before qBit is told anything (torrentmeta
// .LacksVideo), so what reaches here is what that screen cannot see: magnets
// whose metadata was not resolved yet, torrents adopted out of qBit, usenet
// downloads (forage never sees an NZB's file list), and hand-added releases.
// Both layers apply the SAME policy, including on archives: forage has no
// unpacker, so a .rar is refused here as well as there.

// refuseUnplaceableDownload returns a non-empty grab Reason when a finished
// download must not be placed at all, or "" to proceed.
//
// Deliberately narrow. It fires only on a source that is a single regular
// file whose extension the library policy rejects, which is the one case
// placement cannot handle. A directory is the placer's business (it filters
// per file). A stat failure returns "" as well: not seeing the file is a
// dropped mount or a client still moving it in, and reading "can't see it" as
// "junk" would fail grabs whose file is perfectly good. Place's own error
// path already retries those.
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
	// Say the true thing, per case. The first version of this told the user
	// "the release was not what it claimed to be" for every refusal, which is
	// simply false for a .rar: that release may be exactly what it claimed,
	// and forage just cannot open it.
	switch {
	case torrentmeta.IsArchive(name):
		return grabs.RefusedPrefix + "the whole download is one " + ext +
			" file and forage has no unpacker, so nothing in it can reach the library"
	case ext == "":
		return grabs.RefusedPrefix + "the whole download is a single file with no extension, " +
			"which forage never puts in the library (Stash reads scenes by extension)"
	}
	return grabs.RefusedPrefix + "the whole download is one " + ext +
		" file, which forage never puts in the library"
}
