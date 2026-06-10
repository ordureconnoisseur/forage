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
