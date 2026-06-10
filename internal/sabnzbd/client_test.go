package sabnzbd

import "testing"

func TestParseTimeLeft(t *testing.T) {
	cases := map[string]int64{
		"":         0,
		"0:00:00":  0,
		"05:30":    330,
		"1:02:03":  3723,
		"12:00:00": 43200,
		// SAB emits D:HH:MM:SS past 24h; the day component is days, not a
		// base-60 digit (folding it as one counted each day as 60 hours).
		"1:02:03:04": 93784,
		"2:00:00:00": 172800,
		"garbage":    0,
		"1:2:3:4:5":  0,
	}
	for in, want := range cases {
		if got := parseTimeLeft(in); got != want {
			t.Errorf("parseTimeLeft(%q) = %d, want %d", in, got, want)
		}
	}
}
