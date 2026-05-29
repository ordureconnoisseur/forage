package matcher

import "testing"

func TestTopDate(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// space-separated scene-release form (the case this fixes)
		{"EvilAngel 25 08 15 Scarlet Chase Anal Water Spouts XXX 1080p MP4 WRB [XC]", "2025-08-15"},
		{"Studio 2025 08 15 Performer Title", "2025-08-15"},
		// existing separated forms still work
		{"evilangel.26.05.25.scarlet.chase.oil.filled.anal.ride.1080p.mp4", "2026-05-25"},
		{"Performer - Scene Title (2024-03-17)", "2024-03-17"},
		// invalid month/day triples must NOT register as a date
		{"Best Of Top 25 99 15 Compilation", ""},
		{"no date here at all", ""},
	}
	for _, c := range cases {
		if got := TopDate(c.in); got != c.want {
			t.Errorf("TopDate(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
