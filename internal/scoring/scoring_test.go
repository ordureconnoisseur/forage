package scoring

import "testing"

func TestScoreAdditive(t *testing.T) {
	s := New([]Rule{
		{Label: "x265", Pattern: `x265`, Points: 100},
		{Label: "1080p", Pattern: `1080p`, Points: 80},
		{Label: "480p", Pattern: `480p`, Points: -50},
	})
	// x265 + 1080p, no 480p → 180.
	r := s.Score("Studio.Scene.1080p.x265-GRP")
	if r.Score != 180 {
		t.Errorf("score = %d, want 180", r.Score)
	}
	if r.Rejected {
		t.Error("should not be rejected")
	}
	if len(r.Hits) != 2 {
		t.Errorf("hits = %d, want 2", len(r.Hits))
	}
}

func TestScoreCaseInsensitive(t *testing.T) {
	s := New([]Rule{{Label: "hevc", Pattern: `hevc`, Points: 100}})
	if s.Score("Movie HEVC 1080p").Score != 100 {
		t.Error("HEVC should match case-insensitively")
	}
}

func TestReject(t *testing.T) {
	s := New([]Rule{
		{Label: "1080p", Pattern: `1080p`, Points: 80},
		{Label: "cam", Pattern: `\bcam\b`, Points: 0, Reject: true},
	})
	r := s.Score("Some.Movie.CAM.1080p")
	if !r.Rejected {
		t.Error("a matched reject rule must set Rejected")
	}
	// Score still computed (1080p hit) even when rejected — the caller
	// filters on Rejected, not Score.
	if r.Score != 80 {
		t.Errorf("score = %d, want 80", r.Score)
	}
}

func TestNoMatchZero(t *testing.T) {
	s := New(DefaultRules())
	// A bare SiteRip with no resolution/codec token: nothing matches.
	r := s.Score("Performer SiteRip")
	if r.Score != 0 || r.Rejected || len(r.Hits) != 0 {
		t.Errorf("expected empty result, got %+v", r)
	}
}

func TestInvalidRegexSkipped(t *testing.T) {
	// A bad pattern must not match and must not break the other rules.
	s := New([]Rule{
		{Label: "bad", Pattern: `(unclosed`, Points: 999},
		{Label: "1080p", Pattern: `1080p`, Points: 80},
	})
	r := s.Score("Movie 1080p")
	if r.Score != 80 {
		t.Errorf("score = %d, want 80 (bad rule skipped)", r.Score)
	}
}

func TestEmptyPatternNeverMatches(t *testing.T) {
	s := New([]Rule{{Label: "blank", Pattern: "   ", Points: 50}})
	if s.Score("anything").Score != 0 {
		t.Error("blank pattern must not match")
	}
}

func TestDefaultsRealTitles(t *testing.T) {
	s := New(DefaultRules())
	cases := []struct {
		title    string
		wantRej  bool
		minScore int // lower bound; exact value is tuning-dependent
	}{
		{"Studio.26.01.01.Performer.XXX.1080p.x265-GRP", false, 180},
		{"Studio Performer 2160p HEVC", false, 160}, // 4k(60)+hevc(100)
		{"Performer Scene 480p", false, -50},
		{"Leaked.CAM.copy.1080p", true, 0},
	}
	for _, c := range cases {
		r := s.Score(c.title)
		if r.Rejected != c.wantRej {
			t.Errorf("%q: rejected=%v want %v", c.title, r.Rejected, c.wantRej)
		}
		if !c.wantRej && r.Score < c.minScore {
			t.Errorf("%q: score=%d want >= %d", c.title, r.Score, c.minScore)
		}
	}
}
