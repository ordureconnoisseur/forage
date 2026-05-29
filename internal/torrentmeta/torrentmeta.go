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
}

// videoExts is the set of extensions counted as a "video" for pack
// classification. Lower-case, leading dot.
var videoExts = map[string]bool{
	".mp4": true, ".mkv": true, ".avi": true, ".wmv": true, ".mov": true,
	".m4v": true, ".ts": true, ".m2ts": true, ".mpg": true, ".mpeg": true,
	".flv": true, ".webm": true, ".vob": true, ".rm": true, ".rmvb": true,
	".divx": true, ".asf": true, ".mts": true, ".f4v": true, ".ogv": true,
}

func isVideo(name string) bool {
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return false
	}
	return videoExts[strings.ToLower(name[i:])]
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
				m.TotalSize += ln
			}
			// Last path segment is the filename.
			if path, ok := f["path"].([]any); ok && len(path) > 0 {
				if seg, ok := path[len(path)-1].([]byte); ok && isVideo(string(seg)) {
					m.VideoCount++
				}
			}
		}
		return m, nil
	}

	// Single-file: info.length + info.name is the file.
	if ln, ok := info["length"].(int64); ok {
		m.TotalSize = ln
		m.FileCount = 1
		if isVideo(m.Name) {
			m.VideoCount = 1
		}
	}
	return m, nil
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
