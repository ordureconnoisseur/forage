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

func dateContains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// TestAllDatesEmitsAllReadings: an ambiguous 2-digit date yields every
// plausible ordering, including the US MM.DD.YY the old extractor never
// produced.
func TestAllDatesEmitsAllReadings(t *testing.T) {
	got := AllDates("Brazzers.09.11.26.Scene.XXX.1080p")
	for _, want := range []string{"2009-11-26" /*YY.MM.DD*/, "2026-11-09" /*DD.MM.YY*/, "2026-09-11" /*MM.DD.YY*/} {
		if !dateContains(got, want) {
			t.Errorf("AllDates missing %s; got %v", want, got)
		}
	}
}

// TestAllDatesUSOnly: day=15 invalidates both YY.MM.DD and DD.MM.YY (month
// 15), so MM.DD.YY is the ONLY valid reading — the old code emitted nothing
// for this, zeroing the date signal for US-studio releases.
func TestAllDatesUSOnly(t *testing.T) {
	got := AllDates("NaughtyAmerica.08.15.24.Scene.1080p")
	if !dateContains(got, "2024-08-15") {
		t.Fatalf("expected 2024-08-15 (MM.DD.YY only); got %v", got)
	}
}

// TestAllDatesUSFourDigitYear: the 4-digit-year family had no month-first
// reading at all — "10.28.2023" extracted NOTHING (28 fails as a month for
// dd.mm.yyyy) even though the 2-digit branch handles "10.28.23" fine.
func TestAllDatesUSFourDigitYear(t *testing.T) {
	got := AllDates("BrazzersExxtra.10.28.2023.Angela.White.Title")
	if !dateContains(got, "2023-10-28") {
		t.Fatalf("expected 2023-10-28 (MM.DD.YYYY only); got %v", got)
	}
}

// TestAllDatesAmbiguousFourDigitYear: when both readings are calendar-valid
// both must be emitted (the candidate's own date disambiguates), and TopDate
// keeps preferring the EU reading on ties.
func TestAllDatesAmbiguousFourDigitYear(t *testing.T) {
	got := AllDates("Studio.05.06.2024.Title")
	for _, want := range []string{"2024-06-05" /*DD.MM.YYYY*/, "2024-05-06" /*MM.DD.YYYY*/} {
		if !dateContains(got, want) {
			t.Errorf("AllDates missing %s; got %v", want, got)
		}
	}
	if top := TopDate("Studio.05.06.2024.Title"); top != "2024-06-05" {
		t.Errorf("TopDate = %s, want EU reading 2024-06-05 on ties", top)
	}
}

// TestBestDateProximityPicksMatchingReading: the candidate scene's own date
// selects the correct interpretation of an ambiguous release date.
func TestBestDateProximityPicksMatchingReading(t *testing.T) {
	dates := AllDates("Brazzers.09.11.26.Scene")
	if s, _ := bestDateProximity("2026-09-11", dates); s != 1.0 { // MM.DD.YY
		t.Errorf("scene 2026-09-11: proximity = %v, want 1.0", s)
	}
	if s, _ := bestDateProximity("2026-11-09", dates); s != 1.0 { // DD.MM.YY
		t.Errorf("scene 2026-11-09: proximity = %v, want 1.0", s)
	}
	if s, _ := bestDateProximity("2020-01-01", dates); s != 0 { // none near
		t.Errorf("unrelated scene: proximity = %v, want 0", s)
	}
}
