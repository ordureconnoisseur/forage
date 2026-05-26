// Minimal bencode reader, just enough to extract `info.name` from a
// .torrent file. Bencode has four types — string, int, list, dict —
// and we only need the first three of them (and structurally
// recognise dicts) to navigate the metadata.
//
// Why not a library: bencode-go / anacrolix-torrent add a dep for one
// field. The format is small enough to handle in ~80 lines.
package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

// extractTorrentName parses a .torrent file at path and returns the
// info.name field. For single-file torrents this is the file name;
// for multi-file torrents this is the containing-directory name
// (which is also conventionally the release name).
//
// Returns "" with a non-nil error on any parse failure; never panics.
func extractTorrentName(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	d := &bdecoder{buf: b}
	v, err := d.read()
	if err != nil {
		return "", fmt.Errorf("bencode parse: %w", err)
	}
	top, ok := v.(map[string]any)
	if !ok {
		return "", errors.New("top-level value is not a dict")
	}
	info, ok := top["info"].(map[string]any)
	if !ok {
		return "", errors.New("no info dict")
	}
	name, ok := info["name"].(string)
	if !ok {
		return "", errors.New("info.name not a string")
	}
	return name, nil
}

type bdecoder struct {
	buf []byte
	pos int
}

func (d *bdecoder) read() (any, error) {
	if d.pos >= len(d.buf) {
		return nil, errors.New("unexpected EOF")
	}
	c := d.buf[d.pos]
	switch {
	case c == 'i':
		return d.readInt()
	case c == 'l':
		return d.readList()
	case c == 'd':
		return d.readDict()
	case c >= '0' && c <= '9':
		return d.readString()
	}
	return nil, fmt.Errorf("unexpected byte %q at offset %d", c, d.pos)
}

func (d *bdecoder) readInt() (int64, error) {
	// 'i' <int> 'e'
	d.pos++ // skip 'i'
	end := d.pos
	for end < len(d.buf) && d.buf[end] != 'e' {
		end++
	}
	if end >= len(d.buf) {
		return 0, errors.New("unterminated int")
	}
	n, err := strconv.ParseInt(string(d.buf[d.pos:end]), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad int: %w", err)
	}
	d.pos = end + 1
	return n, nil
}

func (d *bdecoder) readString() (string, error) {
	// <len> ':' <bytes>
	colon := d.pos
	for colon < len(d.buf) && d.buf[colon] != ':' {
		colon++
	}
	if colon >= len(d.buf) {
		return "", errors.New("unterminated string length")
	}
	n, err := strconv.Atoi(string(d.buf[d.pos:colon]))
	if err != nil {
		return "", fmt.Errorf("bad string length: %w", err)
	}
	start := colon + 1
	if start+n > len(d.buf) {
		return "", fmt.Errorf("string of length %d runs past buffer end", n)
	}
	s := string(d.buf[start : start+n])
	d.pos = start + n
	return s, nil
}

func (d *bdecoder) readList() ([]any, error) {
	d.pos++ // skip 'l'
	var out []any
	for d.pos < len(d.buf) && d.buf[d.pos] != 'e' {
		v, err := d.read()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if d.pos >= len(d.buf) {
		return nil, errors.New("unterminated list")
	}
	d.pos++ // skip 'e'
	return out, nil
}

func (d *bdecoder) readDict() (map[string]any, error) {
	d.pos++ // skip 'd'
	out := make(map[string]any)
	for d.pos < len(d.buf) && d.buf[d.pos] != 'e' {
		k, err := d.readString()
		if err != nil {
			return nil, fmt.Errorf("dict key: %w", err)
		}
		v, err := d.read()
		if err != nil {
			return nil, fmt.Errorf("dict value for key %q: %w", k, err)
		}
		out[k] = v
	}
	if d.pos >= len(d.buf) {
		return nil, errors.New("unterminated dict")
	}
	d.pos++ // skip 'e'
	return out, nil
}
