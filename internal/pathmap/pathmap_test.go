package pathmap

import "testing"

func TestTranslate(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		mapping string
		want    string
	}{
		{"unset mapping", "/data/media/Media/Hazel/x.mp4", "", ""},
		{"empty path", "", "/data/media:Z:\\Media", ""},
		{"unix to windows", "/data/media/Media/Hazel Moore/scene.mp4",
			`/data/media/Media:Z:\Media`, `Z:\Media\Hazel Moore\scene.mp4`},
		{"unix to unix", "/data/media/Media/Hazel/x.mp4",
			"/data/media/Media:/mnt/library", "/mnt/library/Hazel/x.mp4"},
		{"path outside prefix", "/downloads/Hazel/x.mp4",
			"/data/media:Z:\\Media", ""},

		// The prefix match must end on a path boundary — a sibling mount
		// sharing the string prefix is NOT inside the mapping.
		{"sibling dir sharing prefix", "/data/media2/Hazel/x.mp4",
			"/data/media:Z:\\Media", ""},
		{"partial last segment", "/data/library/Hazel/x.mp4",
			"/data/lib:Z:\\Lib", ""},
		{"path equals prefix", "/data/media",
			"/data/media:/mnt/library", "/mnt/library"},

		// Trailing separators in the mapping must not fuse the seam.
		{"trailing slash on forager side", "/data/media/Hazel/x.mp4",
			`/data/media/:Z:\Media`, `Z:\Media\Hazel\x.mp4`},
		{"trailing backslash on stash side", "/data/media/Hazel/x.mp4",
			`/data/media:Z:\Media\`, `Z:\Media\Hazel\x.mp4`},
		{"trailing slash both sides unix", "/data/media/Hazel/x.mp4",
			"/data/media/:/mnt/library/", "/mnt/library/Hazel/x.mp4"},

		// Windows targets keep embedded colons.
		{"windows drive colon kept", "/data/media/x.mp4",
			`/data/media:C:\Users\me\Media`, `C:\Users\me\Media\x.mp4`},

		{"malformed mapping no colon", "/data/media/x.mp4", "/data/media", ""},
		{"malformed mapping empty left", "/data/media/x.mp4", ":/mnt", ""},
		{"malformed mapping empty right", "/data/media/x.mp4", "/data/media:", ""},
		{"mapping collapses to empty after trim", "/data/media/x.mp4", "/:/", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Translate(tc.path, tc.mapping); got != tc.want {
				t.Errorf("Translate(%q, %q) = %q, want %q", tc.path, tc.mapping, got, tc.want)
			}
		})
	}
}

func TestReverse(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		mapping string
		want    string
	}{
		{"unset mapping", `Z:\Media\Hazel\x.mp4`, "", ""},
		{"empty path", "", `/data/media:Z:\Media`, ""},
		{"windows to unix", `Z:\Media\Hazel Moore\scene.mp4`,
			`/data/media/Media:Z:\Media`, "/data/media/Media/Hazel Moore/scene.mp4"},
		{"unix to unix", "/mnt/library/Hazel/x.mp4",
			"/data/media/Media:/mnt/library", "/data/media/Media/Hazel/x.mp4"},
		{"path outside stash prefix", `D:\Other\x.mp4`,
			`/data/media:Z:\Media`, ""},
		// Boundary: a sibling stash mount sharing the string prefix isn't inside.
		{"sibling stash dir sharing prefix", `Z:\Media2\Hazel\x.mp4`,
			`/data/media:Z:\Media`, ""},
		{"path equals stash prefix", `Z:\Media`,
			`/data/media:Z:\Media`, "/data/media"},
		{"trailing backslash on stash side", `Z:\Media\Hazel\x.mp4`,
			`/data/media:Z:\Media\`, "/data/media/Hazel/x.mp4"},
		{"round-trips Translate", "", "", ""}, // placeholder; real round-trip below
		{"malformed mapping no colon", `Z:\Media\x.mp4`, "/data/media", ""},
	}
	for _, tc := range cases {
		if tc.name == "round-trips Translate" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			if got := Reverse(tc.path, tc.mapping); got != tc.want {
				t.Errorf("Reverse(%q, %q) = %q, want %q", tc.path, tc.mapping, got, tc.want)
			}
		})
	}
	// Reverse must invert Translate for an in-mount path.
	mapping := `/data/porn/Media:Z:\Media`
	forager := "/data/porn/Media/Unsorted/pack/x.mp4"
	if got := Reverse(Translate(forager, mapping), mapping); got != forager {
		t.Errorf("Reverse∘Translate = %q, want %q", got, forager)
	}
}

func TestBase(t *testing.T) {
	cases := map[string]string{
		"/data/media/Hazel/x.mp4": "x.mp4",
		`Z:\Media\Hazel\x.mp4`:    "x.mp4",
		"x.mp4":                   "x.mp4",
	}
	for in, want := range cases {
		if got := Base(in); got != want {
			t.Errorf("Base(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParent(t *testing.T) {
	cases := map[string]string{
		"/data/media/Hazel/x.mp4": "Hazel",
		`Z:\Media\Hazel\x.mp4`:    "Hazel",
		"x.mp4":                   "",
	}
	for in, want := range cases {
		if got := Parent(in); got != want {
			t.Errorf("Parent(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestReverseCaseInsensitiveWindowsPrefix: NTFS/SMB paths are
// case-insensitive and Stash's reported casing can differ from the
// configured mapping. A sensitive compare silently failed the reverse
// mapping — which, for the trash flow, demotes a recoverable delete to a
// permanent one. POSIX prefixes stay case-sensitive.
func TestReverseCaseInsensitiveWindowsPrefix(t *testing.T) {
	const mapping = `/data/porn/Media:Z:\Media`
	for _, tc := range []struct{ in, want string }{
		{`z:\media\Chloe Cherry\x.mp4`, "/data/porn/Media/Chloe Cherry/x.mp4"},
		{`Z:\MEDIA\a\b.mp4`, "/data/porn/Media/a/b.mp4"},
		{`Z:\Media\a\b.mp4`, "/data/porn/Media/a/b.mp4"},
		// Boundary still enforced whatever the case: a sibling must not map.
		{`Z:\Media2\a.mp4`, ""},
		{`z:\mediakit\a.mp4`, ""},
	} {
		if got := Reverse(tc.in, mapping); got != tc.want {
			t.Errorf("Reverse(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// POSIX prefixes remain case-sensitive — the STASH side of this mapping
	// is /mnt/media, so a case-mangled /MNT must not reverse.
	if got := Reverse("/MNT/media/x.mp4", "/data/media:/mnt/media"); got != "" {
		t.Errorf("posix prefix matched case-insensitively: %q", got)
	}
}
