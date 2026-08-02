// Package torrentmeta parses .torrent files (bencode) into the slim
// shape forager's pack detection needs: name, total size, and a count
// of how many contained files are videos.
//
// Why this exists: Prowlarr's search results don't reliably carry a
// file count (PornoLab returns null), and even when present it counts
// thumbnails/NFOs alongside videos. To decide "is this release a
// multi-scene performer pack" we need the authoritative video count,
// which only the torrent metadata gives. Pure stdlib, zero deps —
// matches the hand-rolled style of internal/prowlarr and friends.
package torrentmeta

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Meta is the projection of a .torrent's info dict that pack detection
// cares about.
type Meta struct {
	Name       string
	TotalSize  int64
	FileCount  int // every file in the torrent (videos + thumbs + nfos)
	VideoCount int // files whose extension is a known video container
	// ArchiveCount is packed files (.rar, .zip, multipart .r00/.001, .iso).
	// forage has no unpacker, so these are not videos-in-waiting; the count
	// exists so a refusal can say "all archives" instead of "no video".
	ArchiveCount int
}

// videoExts is the set of extensions counted as a "video". Lower-case,
// leading dot.
//
// This is forage's ONLY video-container list. The placer builds the video
// half of its library allowlist from it (see placer.placeableExts), because
// the two questions are the same question asked at different times: "is
// there a video in this release" before downloading, and "may this file
// enter the library" after. When this list was the narrower of the two, a
// release whose only container was .3gp/.mpv/.m2v/.m4p was refused before
// download even though the placer would have accepted it happily. That is
// the expensive direction to be wrong in: a false refusal costs the user a
// scene, a false accept costs bandwidth we were already spending. So: be
// generous here, and let one list feed both callers.
var videoExts = map[string]bool{
	".mp4": true, ".mkv": true, ".avi": true, ".wmv": true, ".mov": true,
	".m4v": true, ".ts": true, ".m2ts": true, ".mpg": true, ".mpeg": true,
	".flv": true, ".webm": true, ".vob": true, ".rm": true, ".rmvb": true,
	".divx": true, ".asf": true, ".mts": true, ".f4v": true, ".ogv": true,
	".3gp": true, ".mpv": true, ".m2v": true, ".m4p": true,
}

func isVideo(name string) bool {
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return false
	}
	return videoExts[strings.ToLower(name[i:])]
}

// IsVideo reports whether a filename has a known video-container
// extension. Exported so the poller's qBit-adoption path classifies a
// torrent's files the same way the .torrent parser does.
func IsVideo(name string) bool {
	return isVideo(name)
}

// VideoExtensions returns every video-container extension, lower-case with a
// leading dot, in no particular order. Exported for the placer, which builds
// its allowlist from this rather than keeping a second copy: see videoExts.
func VideoExtensions() []string {
	out := make([]string, 0, len(videoExts))
	for ext := range videoExts {
		out = append(out, ext)
	}
	return out
}

// archiveExts is the set of container extensions whose CONTENTS are unknown
// from the file list alone. Being on this list does NOT save a release from
// refusal (see LacksVideo); it only changes what the user is told, because
// "12 files, all of them archives" explains the refusal where "no video"
// sounds like a mistake.
var archiveExts = map[string]bool{
	".rar": true, ".zip": true, ".7z": true, ".tar": true, ".gz": true,
	".bz2": true, ".xz": true, ".iso": true, ".arj": true, ".cab": true,
	".z": true, ".lzma": true, ".zst": true,
}

// isArchive reports whether a filename is a packed container. Beyond the
// named extensions it accepts the two multipart shapes scene releases use,
// ".r00".."r99" and ".001"..".999", because a rar set's later parts carry no
// recognisable extension at all.
func isArchive(name string) bool {
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return false
	}
	ext := strings.ToLower(name[i:])
	if archiveExts[ext] {
		return true
	}
	switch {
	case len(ext) == 4 && ext[1] == 'r' && isDigits(ext[2:]):
		return true // .r00 .. .r99
	case len(ext) == 4 && isDigits(ext[1:]):
		return true // .001 .. .999
	}
	return false
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}

// IsArchive reports whether a filename is a packed container forage cannot
// open. Exported for the poller's post-download refusal, which says so in
// the reason it shows the user: "a single .rar" needs a different sentence
// from "a single .exe".
func IsArchive(name string) bool {
	return isArchive(name)
}

// ErrNoVideo marks a release whose file list is known and provably carries
// nothing a media library can use. Exported so the grab layer can tell this
// refusal (forage decided against the release; no retry, no failover, and no
// backoff can change the answer) from a failure.
var ErrNoVideo = errors.New("this release has no video files in it, so forage did not download it")

// LacksVideo reports whether downloading this torrent is provably pointless:
// its file list is KNOWN and none of the files is a video container.
//
// An archive does NOT save a release, and the version of this that exempted
// them was wrong. The exemption assumed a .rar might unpack into a scene, but
// forage has no unpacker anywhere in it, and the library allowlist refuses
// every archive extension (placer.Placeable), so an archive-only release
// cannot reach the library no matter how it is downloaded. The exemption's
// only effect was to pay for the download and then refuse the same release
// afterwards, in poller/refuse_junk.go, with the bandwidth already spent.
// What is given up by refusing early is the chance to unpack the download by
// hand out of the client's complete dir; the refusal is shown on the grab, so
// that is a visible loss rather than a silent one.
//
// ArchiveCount is still counted, purely to explain the refusal (NoVideoError).
//
// The FileCount guard is the caution that remains. A magnet has no file list
// until its metadata resolves, and an NZB never exposes one here; both arrive
// as a zero FileCount, and "unknown" must never be read as "empty", because a
// false refusal costs the user a scene where a false accept only costs
// bandwidth we were already spending.
func (m *Meta) LacksVideo() bool {
	if m == nil || m.FileCount == 0 {
		return false
	}
	return m.VideoCount == 0
}

// NoVideoError is the refusal LacksVideo justifies, worded from the counts so
// the Grabs card says why. Wraps ErrNoVideo: callers key on that identity.
func (m *Meta) NoVideoError() error {
	if m == nil {
		return ErrNoVideo
	}
	if m.ArchiveCount > 0 {
		return fmt.Errorf("%w (%d files, %d of them archives, and forage has no unpacker)",
			ErrNoVideo, m.FileCount, m.ArchiveCount)
	}
	return fmt.Errorf("%w (%d files, none of them video)", ErrNoVideo, m.FileCount)
}

// maxDepth caps bencode container nesting. Real torrents nest only a
// few levels (top dict → info → files → file → path → string); a hostile
// or corrupt file with deeply nested containers would otherwise recurse
// until the goroutine stack overflows — a fatal runtime error that
// bypasses recover()/middleware.Recoverer and takes the whole daemon
// down. 256 is far beyond any legitimate torrent yet a trivial stack
// depth to allow.
const maxDepth = 256

// Parse decodes a .torrent byte stream and projects its info dict into
// a Meta. Handles both single-file and multi-file torrents.
func Parse(b []byte) (*Meta, error) {
	v, _, err := decode(b, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("bencode: %w", err)
	}
	top, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("torrent: top level is not a dict")
	}
	info, ok := top["info"].(map[string]any)
	if !ok {
		return nil, errors.New("torrent: missing info dict")
	}

	m := &Meta{}
	if name, ok := info["name"].([]byte); ok {
		m.Name = string(name)
	}

	// Multi-file: info.files = [ {length:int, path:[seg,...]}, ... ].
	if files, ok := info["files"].([]any); ok {
		for _, fe := range files {
			f, ok := fe.(map[string]any)
			if !ok {
				continue
			}
			m.FileCount++
			if ln, ok := f["length"].(int64); ok {
				if ln < 0 {
					return nil, errors.New("torrent: negative file length")
				}
				m.TotalSize += ln
			}
			// Last path segment is the filename.
			if path, ok := f["path"].([]any); ok && len(path) > 0 {
				if seg, ok := path[len(path)-1].([]byte); ok {
					switch name := string(seg); {
					case isVideo(name):
						m.VideoCount++
					case isArchive(name):
						m.ArchiveCount++
					}
				}
			}
		}
		return m, nil
	}

	// Single-file: info.length + info.name is the file.
	if ln, ok := info["length"].(int64); ok {
		if ln < 0 {
			return nil, errors.New("torrent: negative file length")
		}
		m.TotalSize = ln
		m.FileCount = 1
		switch {
		case isVideo(m.Name):
			m.VideoCount = 1
		case isArchive(m.Name):
			m.ArchiveCount = 1
		}
	}
	return m, nil
}

// InfoHash returns the v1 info-hash — the hex SHA-1 of the bencoded
// `info` dict's exact bytes — which is how qBit/libtorrent keys a
// torrent. Hashing the original bytes (not a re-encode) is essential:
// re-encoding could reorder keys and change the hash. SHA-1 here is the
// BitTorrent v1 spec, not a security choice.
func InfoHash(b []byte) (string, error) {
	if len(b) == 0 || b[0] != 'd' {
		return "", errors.New("torrent: not a bencoded dict")
	}
	i := 1
	for i < len(b) && b[i] != 'e' {
		k, ni, err := decode(b, i, 0)
		if err != nil {
			return "", err
		}
		kb, ok := k.([]byte)
		if !ok {
			return "", errors.New("torrent: dict key not a string")
		}
		_, vend, err := decode(b, ni, 0) // span of this key's value
		if err != nil {
			return "", err
		}
		if string(kb) == "info" {
			sum := sha1.Sum(b[ni:vend])
			return hex.EncodeToString(sum[:]), nil
		}
		i = vend
	}
	return "", errors.New("torrent: no info dict")
}

// decode is a minimal bencode reader. Returns the parsed value, the
// index just past it, and an error. Strings come back as []byte (they
// may be non-UTF8 binary, e.g. the pieces field); dicts as
// map[string]any; lists as []any; integers as int64.
func decode(b []byte, i, depth int) (any, int, error) {
	if i >= len(b) {
		return nil, i, errors.New("unexpected end")
	}
	if depth > maxDepth {
		return nil, i, errors.New("bencode nesting too deep")
	}
	switch c := b[i]; {
	case c == 'd':
		i++
		d := map[string]any{}
		for i < len(b) && b[i] != 'e' {
			k, ni, err := decode(b, i, depth+1)
			if err != nil {
				return nil, ni, err
			}
			kb, ok := k.([]byte)
			if !ok {
				return nil, ni, errors.New("dict key not a string")
			}
			v, nni, err := decode(b, ni, depth+1)
			if err != nil {
				return nil, nni, err
			}
			d[string(kb)] = v
			i = nni
		}
		if i >= len(b) {
			return nil, i, errors.New("unterminated dict")
		}
		return d, i + 1, nil
	case c == 'l':
		i++
		var l []any
		for i < len(b) && b[i] != 'e' {
			v, ni, err := decode(b, i, depth+1)
			if err != nil {
				return nil, ni, err
			}
			l = append(l, v)
			i = ni
		}
		if i >= len(b) {
			return nil, i, errors.New("unterminated list")
		}
		return l, i + 1, nil
	case c == 'i':
		j := indexByte(b, i+1, 'e')
		if j < 0 {
			return nil, i, errors.New("unterminated int")
		}
		n, err := atoi64(b[i+1 : j])
		if err != nil {
			return nil, j, err
		}
		return n, j + 1, nil
	case c >= '0' && c <= '9':
		colon := indexByte(b, i, ':')
		if colon < 0 {
			return nil, i, errors.New("string missing length delimiter")
		}
		n, err := atoi64(b[i:colon])
		if err != nil {
			return nil, colon, err
		}
		start := colon + 1
		// Bound the length to the bytes actually remaining BEFORE computing
		// end, so a hostile length (e.g. near maxInt64) can't overflow the
		// int addition and slip past an end>len(b) check into a panicking
		// b[start:end] (end < start) slice expression.
		if n < 0 || n > int64(len(b)-start) {
			return nil, start, errors.New("string length out of range")
		}
		end := start + int(n)
		return b[start:end], end, nil
	default:
		return nil, i, fmt.Errorf("unexpected byte %q at %d", c, i)
	}
}

func indexByte(b []byte, from int, c byte) int {
	for i := from; i < len(b); i++ {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// atoi64 parses a decimal integer (with optional leading '-') from a
// byte slice without allocating via strconv on a string.
func atoi64(b []byte) (int64, error) {
	if len(b) == 0 {
		return 0, errors.New("empty integer")
	}
	neg := false
	i := 0
	if b[0] == '-' {
		neg = true
		i = 1
	}
	const maxInt64 = 1<<63 - 1
	var n int64
	for ; i < len(b); i++ {
		if b[i] < '0' || b[i] > '9' {
			return 0, fmt.Errorf("invalid integer byte %q", b[i])
		}
		d := int64(b[i] - '0')
		// Reject before the multiply/add wraps — an overflowed length or
		// size would otherwise emit a negative/garbage value (bad
		// TotalSize) or feed the slice-bounds path above.
		if n > (maxInt64-d)/10 {
			return 0, errors.New("integer overflow")
		}
		n = n*10 + d
	}
	if neg {
		n = -n
	}
	return n, nil
}
