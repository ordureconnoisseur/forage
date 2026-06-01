package api

import "testing"

func TestMagnetInfoHash(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{
			// The real Shower grab's magnet — 40-char hex, must lowercase.
			"hex btih lowercased",
			"magnet:?xt=urn:btih:14E9A3CD6BD02D5BA10BAA1F9145C44E75EBDB99&dn=Whatever&tr=udp%3a%2f%2ftracker",
			"14e9a3cd6bd02d5ba10baa1f9145c44e75ebdb99",
		},
		{
			// base32 btih (32 chars) decodes to the same 40-hex.
			"base32 btih decoded",
			"magnet:?xt=urn:btih:CTU2HTLL2AWVXIILVIPZCROEJZ26XW4Z",
			"14e9a3cd6bd02d5ba10baa1f9145c44e75ebdb99",
		},
		{"not a magnet (.torrent url)", "http://prowlarr/123/download?apikey=x", ""},
		{"magnet without btih (v2 only)", "magnet:?xt=urn:btmh:1220abc&dn=x", ""},
		{"malformed hex", "magnet:?xt=urn:btih:zzzz", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := magnetInfoHash(c.in); got != c.want {
				t.Errorf("magnetInfoHash(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestValidGrabURL(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"https://prowlarr.example/dl/abc.torrent", true},
		{"http://tracker.example/x", true},
		{"magnet:?xt=urn:btih:14E9A3CD6BD02D5BA10BAA1F9145C44E75EBDB99", true},
		{"  magnet:?xt=urn:btih:abc  ", true}, // trimmed
		{"file:///etc/passwd", false},
		{"ftp://host/x", false},
		{"gopher://host", false},
		{"", false},
		{"not a url", false},
		{"https://", false}, // scheme but no host
	}
	for _, c := range cases {
		if got := validGrabURL(c.in); got != c.want {
			t.Errorf("validGrabURL(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
