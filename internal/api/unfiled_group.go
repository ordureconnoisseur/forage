package api

import (
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/ordureconnoisseur/forager/internal/stash"
)

// Grouping unfiled files into the units a person actually acts on.
//
// 4,139 of the 4,887 unfiled scenes on the reference library live inside 103
// pack folders: "Adorable Alice Video Pack" (489 files), "Toni Camille pack"
// (214), "cecerosee pack" (337). Listing those as individual files is wrong
// twice over. It makes a 4,887 row list out of about 851 real decisions, and
// it invites filing a pack one file at a time, which GUTS it: the poller's
// pack distribute step hardlinks identified scenes out and deliberately
// leaves the originals, so "the pack is never gutted" is existing policy that
// a per-file move would quietly reverse.
//
// So a pack is one row and moves as one directory. <performer>/<pack name>/
// keeps the provenance that made it a pack in the first place, and it is what
// the placer already does when it lands a pack directory.

// unfiledKind distinguishes the two things this view lists.
const (
	kindFile = "file"
	kindPack = "pack"
)

var unfiledSep = regexp.MustCompile(`[\\/]`)

// unfiledItem is one row: a loose file, or a whole pack folder.
type unfiledItem struct {
	Kind string `json:"kind"`
	// Key identifies the item to the filing action: a Stash scene id for a
	// file, the folder's path for a pack. Opaque to the UI.
	Key   string `json:"key"`
	Name  string `json:"name"`
	Path  string `json:"path"`
	Files int    `json:"files"`
	// Bucket for a pack is the best case among its members: a pack with one
	// identified scene in it is worth more attention than one with none.
	Bucket     string   `json:"bucket"`
	Suggested  string   `json:"suggested,omitempty"`
	Performers []string `json:"performers,omitempty"`
	Identified bool     `json:"identified"`
}

// packFolder returns the pack folder name for a path under root, or "" when
// the file sits directly in root or in root/Unsorted.
//
// Unsorted is skipped rather than treated as a pack: it is forage's own
// fallback bin, not a release, and grouping every loose file under one
// "Unsorted pack" would put 748 unrelated files behind a single row.
func packFolder(fullPath, root string) string {
	rel := fullPath
	if root != "" && strings.HasPrefix(strings.ToLower(rel), strings.ToLower(root)) {
		rel = rel[len(root):]
	}
	parts := unfiledSep.Split(strings.TrimLeft(rel, `\/`), -1)
	if len(parts) > 0 && strings.EqualFold(parts[0], "Unsorted") {
		parts = parts[1:]
	}
	if len(parts) > 1 {
		return parts[0]
	}
	return ""
}

// packPath rebuilds the folder's own path, which is what the filing action
// moves and what identifies the row.
func packPath(fullPath, root, folder string) string {
	i := strings.Index(strings.ToLower(fullPath), strings.ToLower(folder))
	if i < 0 {
		return ""
	}
	return fullPath[:i+len(folder)]
}

// bucketRank orders the three buckets so a pack can take the best case among
// its members. Filable beats identified beats unknown, because that is the
// order of how much can be done about a row.
func bucketRank(b string) int {
	switch b {
	case "filable":
		return 3
	case "identified":
		return 2
	default:
		return 1
	}
}

// groupUnfiled folds per-file scenes into per-decision rows.
//
// suggestFor names a performer from a string (the pack folder's name), which
// is where the answer actually lives: Stash names one performer across a whole
// pack in 6 of 103 folders and nobody at all in 93, while the folder is called
// "Toni Camille pack". That is a name string, which is exactly what the
// existing release-name suggester reads.
func groupUnfiled(found []stash.UnfiledScene, root string, want string,
	suggestFor func(string) string) ([]unfiledItem, map[string]int) {

	counts := map[string]int{"filable": 0, "identified": 0, "unknown": 0}
	type pack struct {
		item  *unfiledItem
		names map[string]int
	}
	packs := map[string]*pack{}
	var files []unfiledItem

	for _, f := range found {
		b := bucketFor(f)
		folder := packFolder(f.FilePath, root)
		if folder == "" {
			counts[b]++
			row := unfiledItem{
				Kind: kindFile, Key: f.ID, Name: path.Base(unfiledSep.ReplaceAllString(f.FilePath, "/")),
				Path: f.FilePath, Files: 1, Bucket: b,
				Suggested: topPerformerName(f.Performers), Identified: len(f.StashIDs) > 0,
			}
			for _, p := range f.Performers {
				if p.Name != "" {
					row.Performers = append(row.Performers, p.Name)
				}
			}
			files = append(files, row)
			continue
		}
		p, ok := packs[folder]
		if !ok {
			p = &pack{item: &unfiledItem{
				Kind: kindPack, Key: packPath(f.FilePath, root, folder),
				Name: folder, Path: packPath(f.FilePath, root, folder), Bucket: "unknown",
			}, names: map[string]int{}}
			packs[folder] = p
		}
		p.item.Files++
		if bucketRank(b) > bucketRank(p.item.Bucket) {
			p.item.Bucket = b
		}
		if len(f.StashIDs) > 0 {
			p.item.Identified = true
		}
		if n := topPerformerName(f.Performers); n != "" {
			p.names[n]++
		}
	}

	out := files
	for _, p := range packs {
		counts[p.item.Bucket]++
		// The folder name first: it is right far more often than the
		// metadata, which for 93 of 103 packs names nobody at all.
		if s := suggestFor(p.item.Name); s != "" {
			p.item.Suggested = s
		} else if best := mostNamed(p.names); best != "" {
			p.item.Suggested = best
		}
		for n := range p.names {
			p.item.Performers = append(p.item.Performers, n)
		}
		sort.Strings(p.item.Performers)
		out = append(out, *p.item)
	}

	if want != "" {
		kept := out[:0]
		for _, r := range out {
			if r.Bucket == want {
				kept = append(kept, r)
			}
		}
		out = kept
	}
	sort.SliceStable(out, func(i, j int) bool {
		// Packs first: they are the bigger decisions, and burying a 489 file
		// folder below 700 loose files is how it never gets made.
		if (out[i].Kind == kindPack) != (out[j].Kind == kindPack) {
			return out[i].Kind == kindPack
		}
		if out[i].Kind == kindPack && out[i].Files != out[j].Files {
			return out[i].Files > out[j].Files
		}
		return strings.ToLower(out[i].Path) < strings.ToLower(out[j].Path)
	})
	return out, counts
}

func mostNamed(m map[string]int) string {
	best, n := "", 0
	for k, v := range m {
		if v > n || (v == n && k < best) {
			best, n = k, v
		}
	}
	return best
}
