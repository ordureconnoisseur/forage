package main

import (
	"path/filepath"
	"strings"
)

// Buckets. A file lands in exactly one. "Can this be deleted" is a SEPARATE
// question, answered by Entry.Reclaimable, because redundancy and seeding are
// independent: a superseded re-encode original that a torrent is still serving
// must not be touched, and folding seeding into the bucket hides that.
const (
	BucketDuplicate  = "duplicate"   // identical copy already in the library
	BucketSuperseded = "superseded"  // library holds a SMALLER file of the same name
	BucketVariant    = "variant"     // same name in the library, but no smaller there
	BucketElsewhere  = "elsewhere"   // found in one of the extra roots (another drive)
	BucketOrphan     = "orphan"      // no copy anywhere: downloaded and never filed
	BucketInProgress = "in-progress" // still being written by the download client
	BucketNonMedia   = "non-media"   // .nfo, screenshots, sample images
)

// mediaExt is what counts as content. Everything else is noise that inflates
// file counts (3,019 of 7,132 entries on the reference library) while holding
// essentially no space.
var mediaExt = map[string]bool{
	".mp4": true, ".mkv": true, ".mov": true, ".wmv": true, ".avi": true,
	".m4v": true, ".ts": true, ".flv": true, ".mpg": true, ".mpeg": true,
	".webm": true, ".rmvb": true, ".asf": true, ".m2ts": true,
}

type nameSize struct {
	name string
	size int64
}

// Index is a searchable view of one library tree.
type Index struct {
	// FileIDs holds a per-file identity (the inode on unix) so a hardlinked
	// copy is recognised even after a rename. Empty where the platform can't
	// supply one, and the name indexes carry the load instead.
	FileIDs map[uint64]bool
	// NameSize is the strong match that works across filesystems. It caught
	// 253 files on the reference library that the inode check missed, i.e.
	// copies rather than hardlinks: exactly the files an inode-only sweep
	// would have deleted while calling them dead weight.
	NameSize map[nameSize]bool
	// NameMax is the largest size seen for a basename, which separates "the
	// library re-encoded this and kept something smaller" from "the library
	// already has a better copy than the download".
	NameMax map[string]int64
}

func NewIndex() *Index {
	return &Index{
		FileIDs:  map[uint64]bool{},
		NameSize: map[nameSize]bool{},
		NameMax:  map[string]int64{},
	}
}

// Add records one library file. fileID is ignored unless hasID.
func (ix *Index) Add(path string, size int64, fileID uint64, hasID bool) {
	n := IndexName(path)
	if hasID {
		ix.FileIDs[fileID] = true
	}
	ix.NameSize[nameSize{n, size}] = true
	if size > ix.NameMax[n] {
		ix.NameMax[n] = size
	}
}

// IndexName normalises a path to a comparable basename. Container paths and
// Windows paths reach this tool in the same run (the library over a mount, a
// second copy on a local drive), so separators are unified before splitting.
func IndexName(path string) string {
	p := strings.ReplaceAll(path, `\`, "/")
	return strings.ToLower(strings.TrimSpace(filepath.Base(p)))
}

// Entry is one download-side file and the conclusion drawn about it.
type Entry struct {
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	Bucket      string `json:"bucket"`
	Seeding     bool   `json:"seeding"`
	LibrarySize int64  `json:"library_size,omitempty"`
	// Hardlinked means this path and a library path are the SAME inode, so
	// the bytes are stored once and counted once. Deleting this directory
	// entry frees nothing at all.
	Hardlinked bool `json:"hardlinked,omitempty"`
}

// Redundant reports whether the content exists somewhere else. It says nothing
// about whether the file may be removed.
func (e Entry) Redundant() bool {
	switch e.Bucket {
	case BucketDuplicate, BucketSuperseded, BucketVariant, BucketElsewhere:
		return true
	}
	return false
}

// Reclaimable is the only question a cleanup may act on: the bytes exist
// elsewhere AND no torrent is serving this path. Deleting a seeding file is
// how ten torrents on this library broke once already.
func (e Entry) Reclaimable() bool { return e.Redundant() && !e.Seeding }

// Frees is how many bytes actually come back if this entry is removed, which
// is NOT the same as its size.
//
// A hardlinked duplicate shares one inode with the library file. Removing the
// download-side name drops the link count from 2 to 1 and frees zero bytes,
// because the library still references the same data. On the reference library
// this is the difference between a report promising 4.28 TB and one promising
// 1.28 TB: 3.00 TB of the "reclaimable" total was hardlinks that would have
// freed nothing, and a cleanup run against that figure would have deleted
// thousands of files, broken every torrent serving them, and recovered almost
// no space.
func (e Entry) Frees() int64 {
	if !e.Reclaimable() || e.Hardlinked {
		return 0
	}
	return e.Size
}

// Classify decides one file's bucket. library is the Stash library; extra
// holds any other place the content may legitimately live, such as a second
// drive that some category of content was deliberately moved to.
func Classify(path string, size int64, fileID uint64, hasID bool,
	library *Index, extra []*Index, seeding bool) Entry {

	e := Entry{Path: path, Size: size, Seeding: seeding}
	if InProgress(path) {
		e.Bucket = BucketInProgress
		return e
	}
	if !mediaExt[strings.ToLower(filepath.Ext(path))] {
		e.Bucket = BucketNonMedia
		return e
	}

	name := IndexName(path)
	if hasID && library.FileIDs[fileID] {
		// Same inode as a library file: one copy of the bytes, two names.
		e.Bucket = BucketDuplicate
		e.LibrarySize = size
		e.Hardlinked = true
		return e
	}
	if library.NameSize[nameSize{name, size}] {
		// Same name and size but a DIFFERENT inode: a real second copy, made
		// by the placer's cross-device fallback or by a manual copy. This one
		// does free space.
		e.Bucket = BucketDuplicate
		e.LibrarySize = size
		return e
	}
	if best, ok := library.NameMax[name]; ok {
		e.LibrarySize = best
		// A SMALLER file of the same name in the library is the signature of a
		// re-encode: the transcode wrote a new file at the library path, which
		// broke the hardlink and stranded this full-size original. 1,671 of
		// 1,736 such matches on the reference library were smaller, which is
		// 1.26 TB held by files their own replacements made redundant.
		if best < size {
			e.Bucket = BucketSuperseded
		} else {
			e.Bucket = BucketVariant
		}
		return e
	}
	for _, ix := range extra {
		if ix.NameSize[nameSize{name, size}] || ix.NameMax[name] > 0 {
			e.Bucket = BucketElsewhere
			return e
		}
	}
	e.Bucket = BucketOrphan
	return e
}

// InProgress reports whether the download client is still writing this file.
// Nothing in progress may be classified, let alone deleted.
func InProgress(path string) bool {
	p := strings.ReplaceAll(path, `\`, "/")
	return strings.Contains(p, "/incomplete/") ||
		strings.Contains(p, "/__ADMIN__/") ||
		strings.HasSuffix(p, ".part") ||
		strings.HasSuffix(p, ".!qB")
}

// SeedingPaths filters torrent content paths down to those that actually
// identify a file or folder.
//
// This exists because of a specific wrong answer. qBittorrent reports an EMPTY
// content_path for torrents still fetching metadata, and a naive prefix test,
// strings.HasPrefix(file, ""+"/"), is true for EVERY absolute path. Six such
// torrents claimed 8,051 files and 2.17 TB as "still seeding", which would have
// buried the entire question this tool exists to answer. A content path must be
// absolute and at least minDepth separators deep to count as evidence.
func SeedingPaths(contentPaths []string, minDepth int) []string {
	var out []string
	for _, p := range contentPaths {
		p = strings.TrimSpace(p)
		if p == "" || !strings.HasPrefix(p, "/") {
			continue
		}
		if strings.Count(strings.TrimRight(p, "/"), "/") < minDepth {
			continue
		}
		out = append(out, p)
	}
	return out
}

// IsSeeded reports whether path is, or lives inside, one of the given torrent
// content paths.
func IsSeeded(path string, seeds []string) bool {
	for _, cp := range seeds {
		if path == cp || strings.HasPrefix(path, strings.TrimRight(cp, "/")+"/") {
			return true
		}
	}
	return false
}
