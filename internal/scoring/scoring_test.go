package scoring

import "testing"

func TestScoreAdditive(t *testing.T) {
	s := New([]Rule{
		{Label: "1080p", On: OnTitle, Pattern: `1080p`, Points: 100},
		{Label: "720p", On: OnTitle, Pattern: `720p`, Points: 30},
		{Label: "480p", On: OnTitle, Pattern: `480p`, Points: -50},
	})
	// Only 1080p matches → 100.
	r := s.Score("Studio.Scene.1080p-GRP", "PornoLab")
	if r.Score != 100 {
		t.Errorf("score = %d, want 100", r.Score)
	}
	if r.Rejected {
		t.Error("should not be rejected")
	}
	if len(r.Hits) != 1 {
		t.Errorf("hits = %d, want 1", len(r.Hits))
	}
}

func TestIndexerRule(t *testing.T) {
	s := New([]Rule{
		{Label: "1080p", On: OnTitle, Pattern: `1080p`, Points: 100},
		{Label: "prefer PornoLab", On: OnIndexer, Pattern: `pornolab`, Points: 50},
		{Label: "avoid 1337x", On: OnIndexer, Pattern: `1337x`, Points: -30},
	})
	// Indexer rule matches the indexer field, not the title.
	r := s.Score("Scene 1080p", "PornoLab")
	if r.Score != 150 { // 1080p(100) + PornoLab(50)
		t.Errorf("score = %d, want 150", r.Score)
	}
	r2 := s.Score("Scene 1080p", "1337x")
	if r2.Score != 70 { // 1080p(100) - 1337x(30)
		t.Errorf("score = %d, want 70", r2.Score)
	}
	// An indexer pattern must NOT match against the title.
	s2 := New([]Rule{{Label: "lab", On: OnIndexer, Pattern: `pornolab`, Points: 99}})
	if s2.Score("PornoLab in the title", "1337x").Score != 0 {
		t.Error("indexer rule should match indexer field, not title")
	}
}

func TestOnDefaultsToTitle(t *testing.T) {
	// On omitted → title.
	s := New([]Rule{{Label: "1080p", Pattern: `1080p`, Points: 80}})
	if s.Score("x 1080p", "idx").Score != 80 {
		t.Error("empty On should default to title match")
	}
}

func TestReject(t *testing.T) {
	s := New([]Rule{
		{Label: "1080p", On: OnTitle, Pattern: `1080p`, Points: 80},
		{Label: "ban indexer", On: OnIndexer, Pattern: `badtracker`, Points: 0, Reject: true},
	})
	r := s.Score("Movie 1080p", "BadTracker")
	if !r.Rejected {
		t.Error("a matched reject rule must set Rejected")
	}
	if r.Score != 80 {
		t.Errorf("score = %d, want 80 (still computed)", r.Score)
	}
}

func TestNoMatchZero(t *testing.T) {
	s := New(DefaultRules())
	r := s.Score("Performer SiteRip", "SomeIndexer") // no resolution token
	if r.Score != 0 || r.Rejected || len(r.Hits) != 0 {
		t.Errorf("expected empty result, got %+v", r)
	}
}

func TestInvalidRegexSkipped(t *testing.T) {
	s := New([]Rule{
		{Label: "bad", Pattern: `(unclosed`, Points: 999},
		{Label: "1080p", Pattern: `1080p`, Points: 80},
	})
	if s.Score("Movie 1080p", "idx").Score != 80 {
		t.Error("bad rule must be skipped, not crash or match")
	}
}

func TestDefaultsResolution(t *testing.T) {
	s := New(DefaultRules())
	cases := []struct {
		title string
		want  int
	}{
		{"Studio.26.01.01.Performer.XXX.1080p", 100},
		{"Studio Performer 2160p", 70},
		{"Performer Scene 720p", 30},
		{"Performer Scene 480p", -50},
		{"Performer SiteRip", 0},
	}
	for _, c := range cases {
		if got := s.Score(c.title, "idx").Score; got != c.want {
			t.Errorf("%q: score=%d want %d", c.title, got, c.want)
		}
	}
}

func TestResolution(t *testing.T) {
	cases := map[string]string{
		"Scene 1080p x":            Res1080,
		"Scene 2160p":              Res4K,
		"Scene 3840p wide":         Res4K,
		"UHD release":              Res4K,
		"Old 720p":                 Res720,
		"SD 480p":                  Res480,
		"Performer SiteRip":        ResNone,
		"Both 1080p and 2160p cut": Res4K, // 4K wins when both present
	}
	for title, want := range cases {
		if got := Resolution(title); got != want {
			t.Errorf("Resolution(%q) = %q, want %q", title, got, want)
		}
	}
}
