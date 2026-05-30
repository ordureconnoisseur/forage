// Package scoring ranks release candidates by a user-defined, flat,
// additive preference list — the antidote to Radarr/Sonarr's opaque
// quality-tier-then-custom-format two-layer model.
//
// A release's score is simply the SUM of every matching rule's points.
// One transparent number, no tiers. A rule may also REJECT (hard
// never-grab) regardless of score. Selection within already
// scene-verified releases is: highest score wins, ties broken by the
// caller (seeders/grabs). Every contribution is reported so the UI can
// show exactly why a release scored what it did.
package scoring

import (
	"regexp"
	"strings"
)

// Rule is one user preference. Pattern is matched (case-insensitive) as a
// regular expression against the release title. Points are added to the
// release's score on a match (may be negative). Reject, when true and the
// rule matches, hard-disqualifies the release no matter its score.
type Rule struct {
	// Label is a human name for the rule, shown in the score breakdown.
	Label string `json:"label"`
	// Pattern is a (case-insensitive) regexp matched against the release
	// title. Invalid patterns are skipped (never match) — a bad rule
	// can't crash scoring.
	Pattern string `json:"pattern"`
	Points  int    `json:"points"`
	Reject  bool   `json:"reject,omitempty"`
}

// Hit records one rule that matched a release — for the breakdown.
type Hit struct {
	Label  string `json:"label"`
	Points int    `json:"points"`
	Reject bool   `json:"reject,omitempty"`
}

// Result is the outcome of scoring one release.
type Result struct {
	Score    int   `json:"score"`
	Rejected bool  `json:"rejected"`
	Hits     []Hit `json:"hits"`
}

// Scorer holds compiled rules. Build once (rules rarely change), reuse
// across many releases.
type Scorer struct {
	rules []compiledRule
}

type compiledRule struct {
	rule Rule
	re   *regexp.Regexp // nil if the pattern failed to compile
}

// New compiles a rule set into a Scorer. Rules with an invalid regexp are
// kept but will never match (so one typo doesn't silently drop the rest).
func New(rules []Rule) *Scorer {
	cs := make([]compiledRule, 0, len(rules))
	for _, r := range rules {
		var re *regexp.Regexp
		if p := strings.TrimSpace(r.Pattern); p != "" {
			// Case-insensitive; a compile error leaves re nil.
			re, _ = regexp.Compile("(?i)" + p)
		}
		cs = append(cs, compiledRule{rule: r, re: re})
	}
	return &Scorer{rules: cs}
}

// Score evaluates one release title against the rules. Score is the sum
// of matched points; Rejected is true if any matched rule has Reject.
func (s *Scorer) Score(title string) Result {
	var res Result
	for _, c := range s.rules {
		if c.re == nil || !c.re.MatchString(title) {
			continue
		}
		res.Score += c.rule.Points
		res.Hits = append(res.Hits, Hit{Label: c.rule.Label, Points: c.rule.Points, Reject: c.rule.Reject})
		if c.rule.Reject {
			res.Rejected = true
		}
	}
	return res
}

// DefaultRules is the opinionated out-of-the-box preference list, so
// scoring is useful with zero configuration (no TRaSH-guides treadmill).
// Prefer modern efficient encodes at 1080p, mildly prefer 4K, penalise
// low-res, and hard-reject cam/screener junk. Users reorder/retune these
// in Settings.
func DefaultRules() []Rule {
	return []Rule{
		{Label: "x265 / HEVC", Pattern: `\b(x265|hevc|h\.?265)\b`, Points: 100},
		{Label: "AV1", Pattern: `\bav1\b`, Points: 90},
		{Label: "1080p", Pattern: `\b1080p?\b`, Points: 80},
		{Label: "4K / 2160p", Pattern: `\b(2160p?|4k|uhd)\b`, Points: 60},
		{Label: "720p", Pattern: `\b720p?\b`, Points: 20},
		{Label: "x264 / H.264", Pattern: `\b(x264|h\.?264|avc)\b`, Points: 10},
		{Label: "480p / SD", Pattern: `\b(480p?|360p?|sd)\b`, Points: -50},
		{Label: "CAM / TS / screener", Pattern: `\b(cam|ts|telesync|telecine|screener|scr)\b`, Points: 0, Reject: true},
	}
}
