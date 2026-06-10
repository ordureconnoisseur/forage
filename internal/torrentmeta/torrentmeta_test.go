package torrentmeta

import (
	"strings"
	"testing"
)

func TestParseSingleFile(t *testing.T) {
	// d4:infod6:lengthi1024e4:name9:movie.mp4ee
	raw := []byte("d4:infod6:lengthi1024e4:name9:movie.mp4ee")
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Name != "movie.mp4" || m.TotalSize != 1024 || m.FileCount != 1 || m.VideoCount != 1 {
		t.Fatalf("got %+v", m)
	}
}

func TestParseMultiFile(t *testing.T) {
	// info{ files:[ {2000,a.mp4}, {30,a.jpg} ], name:pack }
	raw := []byte("d4:infod5:filesld6:lengthi2000e4:pathl5:a.mp4eed6:lengthi30e4:pathl5:a.jpgeee4:name4:packee")
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Name != "pack" {
		t.Errorf("name = %q", m.Name)
	}
	if m.TotalSize != 2030 {
		t.Errorf("size = %d", m.TotalSize)
	}
	if m.FileCount != 2 {
		t.Errorf("filecount = %d", m.FileCount)
	}
	if m.VideoCount != 1 {
		t.Errorf("videocount = %d (want 1, .jpg must not count)", m.VideoCount)
	}
}

func TestParseRejectsDeepNesting(t *testing.T) {
	// Nesting deeper than maxDepth must error, not recurse until the
	// goroutine stack overflows (a fatal, unrecoverable crash).
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Parse panicked on deep nesting: %v", r)
		}
	}()
	raw := []byte(strings.Repeat("l", maxDepth+50))
	if _, err := Parse(raw); err == nil {
		t.Fatal("expected error on deeply nested input, got nil")
	}
}

func TestParseRejectsBadLengths(t *testing.T) {
	// Hostile/corrupt length and integer fields must error cleanly, never
	// panic (overflowed int64, valid-but-oversized string length, etc.).
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Parse panicked: %v", r)
		}
	}()
	for _, raw := range []string{
		"99999999999999999999:x", // string length overflows int64
		"9223372036854775807:x",  // valid int64 but far exceeds the buffer
		"5:ab",                   // length exceeds the bytes that follow
		"i99999999999999999999e", // integer value overflows int64
		// Negative file lengths: bencode integers may be negative, but a
		// file length never is. Summing them silently understated
		// TotalSize (or made it negative) for corrupt/hostile torrents.
		"d4:infod6:lengthi-1e4:name9:movie.mp4ee",
		"d4:infod5:filesld6:lengthi-9999999999e4:pathl5:a.mp4eeee4:name4:packee",
	} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("expected error for %q, got nil", raw)
		}
	}
}

func TestParseLargeFileSize(t *testing.T) {
	// A legitimately huge size (50 GB) must still parse — the overflow
	// guard must not reject normal large packs.
	raw := []byte("d4:infod6:lengthi53687091200e4:name9:movie.mp4ee")
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.TotalSize != 53687091200 {
		t.Fatalf("size = %d, want 53687091200", m.TotalSize)
	}
}

func TestInfoHash(t *testing.T) {
	// Top dict wraps a known info dict; the hash is SHA-1 of the info
	// dict's exact bytes (precomputed). Keys around `info` (announce)
	// must not affect it.
	tor := []byte("d8:announce3:foo4:infod6:lengthi12e4:name3:bar12:piece lengthi16e6:pieces0:ee")
	got, err := InfoHash(tor)
	if err != nil {
		t.Fatalf("InfoHash: %v", err)
	}
	const want = "0dda68641cb282a91fdfa3a1208ea330044ed441"
	if got != want {
		t.Fatalf("InfoHash = %s, want %s", got, want)
	}
	if _, err := InfoHash([]byte("<html>nope</html>")); err == nil {
		t.Fatal("expected error on non-bencode input")
	}
}
