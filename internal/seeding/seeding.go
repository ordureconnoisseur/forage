// Package seeding answers one question: is a torrent currently serving this
// path?
//
// forage needs it because nothing else in the system knows. The placer
// hardlinks and never moves, so forage's own placement is safe by
// construction, but every other surface that moves or removes a file — the
// unfiled filing action, the refile backfill, the destroy package — operates
// on paths without any idea whether a download client is reading them.
//
// That gap has already cost something. A qBittorrent category on the
// reference library had its save path set to the library root, so torrents
// downloaded straight into the library. A bulk move of loose files out of
// that root then broke ten of them at once: the files were fine, but the
// client still pointed at the old paths and every one went dark. Nineteen
// torrents on that library still seed from inside the library tree.
//
// A Set is a SNAPSHOT, not a live query. Callers take one before a batch of
// work and use it throughout, so a thousand-file plan costs one round trip
// rather than a thousand, and so the answer cannot change halfway through a
// plan the user has already been shown.
package seeding

import "strings"

// DefaultMinDepth is how many separators a content path must contain to be
// treated as evidence. Four keeps "/data/porn/downloads/complete/x.mp4" and
// rejects "/data/porn", which would claim an entire library.
const DefaultMinDepth = 4

// Set is a snapshot of the paths torrents are serving.
type Set struct {
	paths []string
}

// New builds a Set from raw torrent content paths, discarding any that are
// not specific enough to be evidence.
//
// The filtering is the whole point, and it comes from a wrong answer that
// looked completely convincing. qBittorrent reports an EMPTY content_path for
// torrents still fetching metadata. A naive prefix test then asks whether the
// file starts with "" + "/", which is true of every absolute path on the
// machine. Six such torrents claimed 8,051 files and 2.17 TB as "currently
// seeding" — a result that would have marked an entire download tree
// untouchable, or, with the test inverted, marked a live one deletable.
//
// So a content path counts only when it is absolute and at least minDepth
// separators deep. Pass DefaultMinDepth unless you know better.
func New(contentPaths []string, minDepth int) *Set {
	s := &Set{}
	for _, p := range contentPaths {
		p = strings.TrimSpace(p)
		if p == "" || !strings.HasPrefix(p, "/") {
			continue
		}
		if strings.Count(strings.TrimRight(p, "/"), "/") < minDepth {
			continue
		}
		s.paths = append(s.paths, strings.TrimRight(p, "/"))
	}
	return s
}

// Covers reports whether path is, or lives inside, a path being served.
//
// A nil Set covers nothing. That is deliberate: "we could not reach the
// download client" must never silently become "nothing is seeding", so
// callers check Known before treating a false as protection.
func (s *Set) Covers(path string) bool {
	if s == nil || path == "" {
		return false
	}
	for _, cp := range s.paths {
		if path == cp {
			return true
		}
		// The separator matters: "a pack extra/clip.mp4" must not match the
		// torrent "a pack", which is a different release entirely.
		if strings.HasPrefix(path, cp+"/") {
			return true
		}
	}
	return false
}

// Blocks reports a seeded path that MOVING path would break, or "" when the
// move is safe. It is the check every rename needs, and it is strictly wider
// than Covers.
//
// Three ways a move breaks a torrent, and a file-only check catches one:
//
//   - the path IS the torrent's content
//   - an ancestor is (the file sits inside a seeded pack folder)
//   - a descendant is (path is a FOLDER holding seeded content)
//
// The third is the one that matters here, because the unfiled filing action
// moves whole pack directories. Ten torrents on the reference library died to
// a bulk move of a directory nobody thought to check the inside of.
func (s *Set) Blocks(path string) string {
	if s == nil || path == "" {
		return ""
	}
	p := strings.TrimRight(path, "/")
	for _, cp := range s.paths {
		if p == cp ||
			strings.HasPrefix(p, cp+"/") || // a seeded ancestor
			strings.HasPrefix(cp, p+"/") { // seeded content inside it
			return cp
		}
	}
	return ""
}

// Entry pairs a torrent's identity with what it is serving.
type Entry struct {
	ID   string
	Path string
}

// NewFrom builds a Set from entries, skipping any whose ID matches skipID
// case-insensitively. An empty skipID keeps everything.
//
// The skip is what makes "is anyone ELSE serving this" expressible. Without
// it a torrent always blocks its own removal and nothing is ever retired,
// which is why the naive version of this check cannot simply reuse the
// whole-client snapshot.
func NewFrom(entries []Entry, skipID string, minDepth int) *Set {
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		if skipID != "" && strings.EqualFold(e.ID, skipID) {
			continue
		}
		paths = append(paths, e.Path)
	}
	return New(paths, minDepth)
}

// Len is how many usable content paths the snapshot holds.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.paths)
}

// Known reports whether this snapshot carries real information. A Set that is
// nil, or that filtered every path away, knows nothing, and a caller about to
// delete on the strength of "not seeded" should say so rather than proceed as
// if the client had confirmed it.
func (s *Set) Known() bool { return s.Len() > 0 }
