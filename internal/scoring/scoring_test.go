package scoring

import "testing"

func TestScoreAdditive(t *testing.T) {
	s := New([]Rule{
		{Label: "1080p", On: OnTitle, Pattern: `1080p`, Points: 100},
		{Label: "720p", On: OnTitle, Pattern: `720p`, Points: 30},
		{Label: "480p", On: OnTitle, Pattern: `480p`, Points: -50},
	})
	// Only 1080p matches → 100.
	r := s.Score("Studio.Scene.1080p-GRP", "PornoLab", "")
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
	r := s.Score("Scene 1080p", "PornoLab", "")
	if r.Score != 150 { // 1080p(100) + PornoLab(50)
		t.Errorf("score = %d, want 150", r.Score)
	}
	r2 := s.Score("Scene 1080p", "1337x", "")
	if r2.Score != 70 { // 1080p(100) - 1337x(30)
		t.Errorf("score = %d, want 70", r2.Score)
	}
	// An indexer pattern must NOT match against the title.
	s2 := New([]Rule{{Label: "lab", On: OnIndexer, Pattern: `pornolab`, Points: 99}})
	if s2.Score("PornoLab in the title", "1337x", "").Score != 0 {
		t.Error("indexer rule should match indexer field, not title")
	}
}

func TestProtocolRule(t *testing.T) {
	s := New([]Rule{
		{Label: "1080p", On: OnTitle, Pattern: `1080p`, Points: 100},
		{Label: "prefer usenet", On: OnProtocol, Pattern: `usenet`, Points: 40},
	})
	if got := s.Score("Scene 1080p", "NZBFinder", "usenet").Score; got != 140 {
		t.Errorf("usenet score = %d, want 140", got)
	}
	if got := s.Score("Scene 1080p", "PornoLab", "torrent").Score; got != 100 {
		t.Errorf("torrent score = %d, want 100", got)
	}
	// A protocol pattern must NOT match the title or indexer field.
	s2 := New([]Rule{{Label: "p", On: OnProtocol, Pattern: `usenet`, Points: 9}})
	if s2.Score("usenet in the title", "usenet-indexer", "torrent").Score != 0 {
		t.Error("protocol rule should match the protocol field only")
	}
}

func TestOnDefaultsToTitle(t *testing.T) {
	// On omitted → title.
	s := New([]Rule{{Label: "1080p", Pattern: `1080p`, Points: 80}})
	if s.Score("x 1080p", "idx", "").Score != 80 {
		t.Error("empty On should default to title match")
	}
}

func TestReject(t *testing.T) {
	s := New([]Rule{
		{Label: "1080p", On: OnTitle, Pattern: `1080p`, Points: 80},
		{Label: "ban indexer", On: OnIndexer, Pattern: `badtracker`, Points: 0, Reject: true},
	})
	r := s.Score("Movie 1080p", "BadTracker", "")
	if !r.Rejected {
		t.Error("a matched reject rule must set Rejected")
	}
	if r.Score != 80 {
		t.Errorf("score = %d, want 80 (still computed)", r.Score)
	}
}

func TestNoMatchZero(t *testing.T) {
	s := New(DefaultRules())
	r := s.Score("Performer SiteRip", "SomeIndexer", "") // no resolution token
	if r.Score != 0 || r.Rejected || len(r.Hits) != 0 {
		t.Errorf("expected empty result, got %+v", r)
	}
}

func TestInvalidRegexSkipped(t *testing.T) {
	s := New([]Rule{
		{Label: "bad", Pattern: `(unclosed`, Points: 999},
		{Label: "1080p", Pattern: `1080p`, Points: 80},
	})
	if s.Score("Movie 1080p", "idx", "").Score != 80 {
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
		// Underscore before the token must still score (was 0 before the fix).
		{"Kenzie.Reeves.Is.The.Anal.Fuck.Buddy.29.07.2022._1080p", 100},
	}
	for _, c := range cases {
		if got := s.Score(c.title, "idx", "").Score; got != c.want {
			t.Errorf("%q: score=%d want %d", c.title, got, c.want)
		}
	}
}

// TestFHDScoresAs1080p: JAV/sukebei "FHD"/"FHDC" labels must score and
// classify as 1080p via canonicalization, so the user's existing
// `\b1080p?\b` rule catches them without a rule edit (the OAE-302 bug —
// FHD/FHDC releases scoring as no-resolution).
func TestFHDScoresAs1080p(t *testing.T) {
	s := New([]Rule{{Label: "1080p", On: OnTitle, Pattern: `\b1080p?\b`, Points: 100}})
	for _, title := range []string{
		"+++ [FHD] OAE-302",
		"+++ [FHDC] OAE-302",
		"OAE-302 fhd",
	} {
		if got := s.Score(title, "", "").Score; got != 100 {
			t.Errorf("Score(%q)=%d, want 100 (FHD→1080p)", title, got)
		}
		if r := Resolution(title); r != Res1080 {
			t.Errorf("Resolution(%q)=%q, want %q", title, r, Res1080)
		}
	}
	// A real "1080p" still works, and an unrelated word containing "fhd" as a
	// substring (not a token) must NOT trigger.
	if got := s.Score("Scene 1080p", "", "").Score; got != 100 {
		t.Errorf("plain 1080p should still score 100, got %d", got)
	}
}

func TestDefaultUsenetPreference(t *testing.T) {
	s := New(DefaultRules())
	// Same resolution: usenet wins the tie (+25).
	nzb1080 := s.Score("Scene 1080p", "NZBFinder", "usenet").Score
	tor1080 := s.Score("Scene 1080p", "PornoLab", "torrent").Score
	if nzb1080 <= tor1080 {
		t.Errorf("1080p usenet (%d) should outrank 1080p torrent (%d)", nzb1080, tor1080)
	}
	// But the usenet nudge must NOT cross a resolution tier: a higher-res
	// torrent still beats a lower-res nzb.
	tor1080b := s.Score("Scene 1080p", "PornoLab", "torrent").Score
	nzb720 := s.Score("Scene 720p", "NZBFinder", "usenet").Score
	if nzb720 >= tor1080b {
		t.Errorf("720p usenet (%d) must not beat 1080p torrent (%d)", nzb720, tor1080b)
	}
	// And 1080p torrent still beats 4K usenet (forage's 1080p>4K default
	// preference holds even with the usenet bump).
	nzb4k := s.Score("Scene 2160p", "NZBFinder", "usenet").Score
	if nzb4k >= tor1080b {
		t.Errorf("4K usenet (%d) must not beat 1080p torrent (%d)", nzb4k, tor1080b)
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
		// Underscore-adjacent token: \b never fires between _ and a digit, so
		// without underscore folding this scored as no-resolution.
		"Kenzie.Reeves.Anal.Fuck.Buddy.29.07.2022._1080p": Res1080,
		"Studio_2160p_release":                            Res4K,
	}
	for title, want := range cases {
		if got := Resolution(title); got != want {
			t.Errorf("Resolution(%q) = %q, want %q", title, got, want)
		}
	}
}

// TestResolutionHeight360 pins the upgrade-gate heights: a 360p release is
// in the bottom TIER (Res480) but must report its real 360px height, or the
// collection job's upgrade filter treats it as taller than an owned
// 360-479px file and pre-selects a non-upgrade.
func TestResolutionHeight360(t *testing.T) {
	cases := map[string]int{
		"Scene.Title.360p.mp4":  360,
		"Scene.Title.480p.mp4":  480,
		"Scene.Title.1080p.mp4": 1080,
		"Scene.Title.FHD.mp4":   1080, // canonicalized synonym
		"Scene.Title.mp4":       0,
	}
	for in, want := range cases {
		if got := ResolutionHeight(in); got != want {
			t.Errorf("ResolutionHeight(%q) = %d, want %d", in, got, want)
		}
	}
	if Resolution("Scene.Title.360p.mp4") != Res480 {
		t.Error("360p must stay in the 480p tier for rules/watch targets")
	}
}
