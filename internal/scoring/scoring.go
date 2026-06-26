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

// Resolution tiers — normalized labels matching the watch targets.
const (
	Res4K   = "4k"
	Res1080 = "1080p"
	Res720  = "720p"
	Res480  = "480p"
	ResNone = "" // no resolution token in the title
)

var (
	re4k   = regexp.MustCompile(`(?i)\b(2160p?|3840p?|4k|uhd)\b`)
	re1080 = regexp.MustCompile(`(?i)\b1080p?\b`)
	re720  = regexp.MustCompile(`(?i)\b720p?\b`)
	re480  = regexp.MustCompile(`(?i)\b(480p?|360p?)\b`)
	// re480exact distinguishes a literal 480 within the lowest tier:
	// 360p folds into the 480p TIER (rules and watch targets see one
	// bottom bucket), but its real pixel height matters to the upgrade
	// gate below.
	re480exact = regexp.MustCompile(`(?i)\b480p?\b`)
	// fhdToken matches the JAV/sukebei "Full HD" labels FHD and FHDC — both
	// are 1080p. JAV releases use these instead of "1080p", so without
	// canonicalizing them a 1080p rule (and the resolution classifier) misses
	// them entirely and the release scores as no-resolution.
	fhdToken = regexp.MustCompile(`(?i)\bfhdc?\b`)
)

// CanonicalizeResolution rewrites resolution synonyms in a release title to
// the standard token rules key off — currently FHD/FHDC → 1080p — and
// normalizes underscore separators to spaces. Applied before OnTitle rule
// matching and inside Resolution(), so a user's existing `\b1080p?\b` rule
// catches JAV Full-HD releases without re-saving anything. Like the matcher's
// diacritic folding: a normalization layer, not magic scoring — the rules stay
// clean and synonym-agnostic.
//
// Underscore folding matters because `\b` (every resolution rule's boundary)
// is a regex word boundary, and `_` is a word char: a release ending
// "…29.07.2022._1080p" has no boundary between the underscore and the digit,
// so `\b1080p\b` never matches and the release scores as no-resolution. Dots
// and dashes (the usual separators) are non-word chars and already fire `\b`;
// underscore was the lone separator that didn't.
func CanonicalizeResolution(title string) string {
	title = fhdToken.ReplaceAllString(title, "1080p")
	return strings.ReplaceAll(title, "_", " ")
}

// Resolution classifies a release title into its highest resolution tier
// (4K wins over 1080 if both appear, which happens in dual-version
// releases). Returns ResNone when no resolution token is present — the
// common bare-SiteRip case. Used by the watch loop's exact-match target.
func Resolution(title string) string {
	title = CanonicalizeResolution(title)
	switch {
	case re4k.MatchString(title):
		return Res4K
	case re1080.MatchString(title):
		return Res1080
	case re720.MatchString(title):
		return Res720
	case re480.MatchString(title):
		return Res480
	}
	return ResNone
}

// ResolutionHeight returns the approximate pixel height for a title's
// resolution tier (0 when none detected). Used to decide whether a release
// is a genuine upgrade over a copy you already own — comparing the release's
// tier against the owned file's reported height.
//
// Within the bottom tier, a 360p-labelled release reports its REAL height:
// returning 480 made a 360p release look taller than an owned 360-479px
// file, so the collection job's upgrade filter pre-selected a same-or-worse
// resolution as an "upgrade".
func ResolutionHeight(title string) int {
	switch Resolution(title) {
	case Res4K:
		return 2160
	case Res1080:
		return 1080
	case Res720:
		return 720
	case Res480:
		if !re480exact.MatchString(title) {
			return 360 // tier matched via its 360p token only
		}
		return 480
	}
	return 0
}

// On selects which release field a rule's Pattern matches against.
// Resolution etc. live in the title; the indexer/source is a separate
// structured field (Prowlarr's indexer name), not present in the title —
// so it gets its own target rather than a title regex that'd never hit.
type On string

const (
	OnTitle    On = "title"    // default; matches the release title (resolution lives here)
	OnIndexer  On = "indexer"  // matches the indexer/source name (PornoLab, 1337x, …)
	OnProtocol On = "protocol" // matches the source protocol ("torrent" / "usenet")
)

// Rule is one user preference. Pattern is matched (case-insensitive) as a
// regexp against the field named by On. Points are added to the release's
// score on a match (may be negative). Reject, when true and the rule
// matches, hard-disqualifies the release no matter its score.
type Rule struct {
	// Label is a human name for the rule, shown in the score breakdown.
	Label string `json:"label"`
	// On is the field to match: "title" (default), "indexer", or "protocol".
	On On `json:"on,omitempty"`
	// Pattern is a (case-insensitive) regexp matched against the On field.
	// Invalid patterns are skipped (never match) — a bad rule can't crash
	// scoring.
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

// Score evaluates one release against the rules. title is the release
// title (resolution etc.); indexer is the structured source name; protocol
// is "torrent" / "usenet". Score is the sum of matched points; Rejected is
// true if any matched rule has Reject.
func (s *Scorer) Score(title, indexer, protocol string) Result {
	var res Result
	for _, c := range s.rules {
		if c.re == nil {
			continue
		}
		// Title rules match against a resolution-canonicalized title so JAV
		// synonyms (FHD/FHDC → 1080p) hit the user's standard rules.
		subject := CanonicalizeResolution(title)
		switch c.rule.On {
		case OnIndexer:
			subject = indexer
		case OnProtocol:
			subject = protocol
		}
		if !c.re.MatchString(subject) {
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
// scoring is useful with zero configuration. Porn release titles reliably
// state RESOLUTION but rarely codec or release group, and the indexer is
// a structured field — so the defaults score on resolution (title) +
// leave indexer rules for the user to add their own source preferences.
// Users reorder/retune in Settings.
func DefaultRules() []Rule {
	return []Rule{
		{Label: "1080p", On: OnTitle, Pattern: `\b1080p?\b`, Points: 100},
		{Label: "4K / 2160p", On: OnTitle, Pattern: `\b(2160p?|3840p?|4k|uhd)\b`, Points: 70},
		{Label: "720p", On: OnTitle, Pattern: `\b720p?\b`, Points: 30},
		{Label: "480p / SD", On: OnTitle, Pattern: `\b(480p?|360p?|\bsd\b)\b`, Points: -50},
		// Prefer usenet at EQUAL quality: nzb downloads don't depend on
		// seeders, so they're more reliably grabbable (dead torrents are the
		// common stall). +25 breaks a same-resolution tie toward usenet but
		// is small enough never to cross a resolution tier (the gaps above
		// are ≥30), so quality always dominates. Harmless for torrent-only
		// setups — no usenet releases to prefer. Retune/remove in Settings.
		{Label: "prefer usenet", On: OnProtocol, Pattern: `usenet`, Points: 25},
	}
}
