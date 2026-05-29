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
		// calendar-valid but obviously-not-a-date space triples (volume/
		// part/track numbering) must NOT register: the space form requires
		// a recent year, so these old-year readings are rejected.
		{"Vol 01 02 03 Collection", ""},
		{"Part 05 06 07", ""},
		{"Chapter 10 11 12 Finale", ""},
		{"Mix 08 09 10 anniversary", ""},
		// a genuinely recent space date with small month/day still works
		{"Brazzers 15 06 20 Performer Title", "2015-06-20"},
	}
	for _, c := range cases {
		if got := TopDate(c.in); got != c.want {
			t.Errorf("TopDate(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
