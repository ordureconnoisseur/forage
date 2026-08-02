package torrentmeta

import (
	"fmt"
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

// bencodeMultiFile builds a multi-file .torrent whose files are named by the
// given paths, so a test can state a release's shape in one line.
func bencodeMultiFile(name string, files ...string) []byte {
	var b strings.Builder
	b.WriteString("d4:infod5:filesl")
	for _, f := range files {
		fmt.Fprintf(&b, "d6:lengthi100e4:pathl%d:%see", len(f), f)
	}
	fmt.Fprintf(&b, "e4:name%d:%see", len(name), name)
	return []byte(b.String())
}

// LacksVideo is the gate that decides whether a release is downloaded at all,
// so each case here is a grab that either wastes a full transfer or never
// happens. False negatives cost bandwidth; false POSITIVES cost the user a
// scene they asked for, which is why "unknown" must never read as "empty".
func TestLacksVideo(t *testing.T) {
	for _, c := range []struct {
		name string
		raw  []byte
		want bool
	}{
		{
			// The release the whole check exists for: a fake carrying an
			// installer and a readme. Downloading it in full places nothing.
			"only junk",
			bencodeMultiFile("Totally.Legit.XXX.1080p", "Setup.exe", "Read Me.txt", "cover.jpg"),
			true,
		},
		{
			// A normal release. The junk rides along and the placer's
			// allowlist drops it at placement — refusing here would throw
			// away the scene with the passengers.
			"video plus junk",
			bencodeMultiFile("Real.Release.XXX.1080p", "scene.mp4", "Setup.exe", "info.nfo"),
			false,
		},
		{
			// Packed releases hide their video behind an archive: the file
			// list genuinely says "no video" and is genuinely wrong.
			"rar set",
			bencodeMultiFile("Packed.Release", "release.rar", "release.r00", "release.r01", "release.sfv"),
			false,
		},
		{"multipart numeric archive", bencodeMultiFile("Packed", "x.001", "x.002"), false},
		{"single-file video", []byte("d4:infod6:lengthi1024e4:name9:movie.mp4ee"), false},
		{"single-file executable", []byte("d4:infod6:lengthi1024e4:name9:Setup.exeee"), true},
		{
			// No files and no length: the info dict told us nothing. This is
			// the shape a magnet's unresolved metadata arrives in, and it
			// must NOT be refused — unknown is not empty.
			"unknown file list",
			[]byte("d4:infod4:name4:packee"),
			false,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, err := Parse(c.raw)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := m.LacksVideo(); got != c.want {
				t.Errorf("LacksVideo() = %v, want %v (meta %+v)", got, c.want, m)
			}
		})
	}
	// A nil Meta is what a caller holds when Parse failed; it must answer
	// "don't refuse" rather than panic on the grab path.
	var nilMeta *Meta
	if nilMeta.LacksVideo() {
		t.Error("a nil Meta must not refuse a release")
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
