package matcher

import (
	"regexp"
	"strings"
)

// Entity is the shape the entity scanner matches against: an opaque ID
// plus a canonical name and a list of aliases. Both performers and
// studios fit this shape, so a single scanner serves both.
type Entity struct {
	ID      string
	Name    string
	Aliases []string
}

// tokenSplit is the punctuation/whitespace class that primarily
// separates tokens in release names and filenames. Empirically chosen
// via the collision bench.
var tokenSplit = regexp.MustCompile(`[._\-\s\[\]()!,@'"&+]+`)

// caseAndDigitSplit further splits each piece into [a-z]+ / [A-Z][a-z]*
// / \d+ runs. This handles two real-world cases the studio bench
// surfaced:
//  1. CamelCase run-together names (`BangBros18` → bang/bros/18)
//  2. letter-digit boundaries even in lowercase (`cum4k` → cum/4k)
// Apply this BEFORE lower-casing so the case information is still
// present to split on.
var caseAndDigitSplit = regexp.MustCompile(`[a-z]+|[A-Z][a-z]*|[A-Z]+(?:[^a-z]|$)|\d+`)

// Tokenize splits s on punctuation/whitespace, then further splits each
// piece on case and letter-digit boundaries. Output is lowercase, in
// original order, with empties dropped.
func Tokenize(s string) []string {
	pieces := tokenSplit.Split(s, -1)
	out := make([]string, 0, len(pieces))
	for _, p := range pieces {
		if p == "" {
			continue
		}
		// Apply case/digit-split. For all-lowercase pieces without
		// digits this collapses back to a single sub-token, so we
		// stay backward-compatible with the entity-side encoding.
		subs := caseAndDigitSplit.FindAllString(p, -1)
		if len(subs) == 0 {
			out = append(out, strings.ToLower(p))
			continue
		}
		for _, sub := range subs {
			// `[A-Z]+(?:[^a-z]|$)` can capture a trailing non-letter;
			// trim it so we only retain alphanumeric content.
			sub = trimNonAlnumSuffix(sub)
			if sub != "" {
				out = append(out, strings.ToLower(sub))
			}
		}
	}
	return out
}

func trimNonAlnumSuffix(s string) string {
	for len(s) > 0 {
		last := s[len(s)-1]
		if (last >= 'a' && last <= 'z') || (last >= 'A' && last <= 'Z') || (last >= '0' && last <= '9') {
			return s
		}
		s = s[:len(s)-1]
	}
	return s
}

// containsSubsequence reports whether needle appears as a contiguous
// run of equal tokens inside haystack.
func containsSubsequence(haystack, needle []string) bool {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return false
	}
	for i := 0; i <= len(haystack)-len(needle); i++ {
		ok := true
		for j, n := range needle {
			if haystack[i+j] != n {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// ScannerOptions tunes the entity scanner.
//
// MinSingleTokenLen is the minimum length a single-token name or alias
// must have to be eligible at all. 3 kills the "An"/"Ai"/2-char
// alias FP class.
//
// SingleTokenRule controls what counts as "safe" for a single-token
// name. See the SingleTokenRule constants for the two behaviours.
type ScannerOptions struct {
	MinSingleTokenLen int
	SingleTokenRule   SingleTokenRule
}

// SingleTokenRule picks the disqualifier for a single-token name.
type SingleTokenRule int

const (
	// AnyTokenUnique: a single-token name `T` is allowed only if no
	// other entity in the corpus uses the token `T` anywhere in its
	// canonical or alias names. Strict. Used for performers, where
	// reusing a first name as someone else's alias is rare but
	// inflates FP rate when allowed.
	AnyTokenUnique SingleTokenRule = iota
	// CanonicalUnique: a single-token name `T` is allowed if no
	// other entity in the corpus has the exact string `T` as a full
	// canonical name or full alias. Other entities can still contain
	// `T` as a token in their multi-word names without blocking.
	// Looser. Used for studios — unblocks Vixen / Blacked / Tushy
	// whose full canonicals are single-token even though related
	// studios (Vixen X, Blacked Raw) share that token in their names.
	CanonicalUnique
)

// DefaultScannerOptions returns the strict rule set (used for
// performers): `token_min2_min3` per the collision-bench winner.
func DefaultScannerOptions() ScannerOptions {
	return ScannerOptions{MinSingleTokenLen: 3, SingleTokenRule: AnyTokenUnique}
}

// StudioScannerOptions returns the looser rule set used for studios.
func StudioScannerOptions() ScannerOptions {
	return ScannerOptions{MinSingleTokenLen: 3, SingleTokenRule: CanonicalUnique}
}

// Scanner is a pre-compiled entity scanner. Construct once per corpus
// (the corpus-wide unique-token analysis is not free), call Match many
// times.
type Scanner struct {
	candidates []scanCandidate
}

type scanCandidate struct {
	id         string
	nameTokens [][]string // pre-tokenised name + each eligible alias
}

// NewScanner pre-computes per-corpus structures (single-token uniqueness
// + per-entity tokenised names) and returns a ready-to-call Scanner.
func NewScanner(corpus []Entity, opts ScannerOptions) *Scanner {
	if opts.MinSingleTokenLen < 1 {
		opts.MinSingleTokenLen = 1
	}

	// Build the safe-singleton table. The exact disqualifier depends
	// on opts.SingleTokenRule.
	owners := map[string]map[string]bool{}
	switch opts.SingleTokenRule {
	case CanonicalUnique:
		// Only single-token canonical-or-alias strings contribute.
		// "Vixen" → entity Vixen owns "vixen". "Vixen X" doesn't
		// contribute "vixen" because its full name isn't single-token.
		for _, e := range corpus {
			for _, n := range allNames(e) {
				toks := Tokenize(n)
				if len(toks) != 1 {
					continue
				}
				t := toks[0]
				if owners[t] == nil {
					owners[t] = map[string]bool{}
				}
				owners[t][e.ID] = true
			}
		}
	default:
		// AnyTokenUnique: every token in every name contributes.
		for _, e := range corpus {
			for _, n := range allNames(e) {
				for _, t := range Tokenize(n) {
					if owners[t] == nil {
						owners[t] = map[string]bool{}
					}
					owners[t][e.ID] = true
				}
			}
		}
	}
	safeSingleton := make(map[string]bool, len(owners))
	for t, ownerSet := range owners {
		if len(ownerSet) == 1 && len(t) >= opts.MinSingleTokenLen {
			safeSingleton[t] = true
		}
	}

	out := make([]scanCandidate, 0, len(corpus))
	for _, e := range corpus {
		var tt [][]string
		for _, n := range allNames(e) {
			toks := Tokenize(n)
			if len(toks) == 0 {
				continue
			}
			if len(toks) == 1 && !safeSingleton[toks[0]] {
				continue
			}
			tt = append(tt, toks)
		}
		if len(tt) == 0 {
			continue
		}
		out = append(out, scanCandidate{id: e.ID, nameTokens: tt})
	}
	return &Scanner{candidates: out}
}

// Match returns deduplicated entity IDs whose name (canonical or any
// alias) appears as a contiguous token subsequence inside haystack.
func (s *Scanner) Match(haystack string) []string {
	tokens := Tokenize(haystack)
	hits := make([]string, 0)
	for _, c := range s.candidates {
		for _, ntoks := range c.nameTokens {
			if containsSubsequence(tokens, ntoks) {
				hits = append(hits, c.id)
				break
			}
		}
	}
	return hits
}

func allNames(e Entity) []string {
	out := make([]string, 0, 1+len(e.Aliases))
	if e.Name != "" {
		out = append(out, e.Name)
	}
	for _, a := range e.Aliases {
		if a != "" {
			out = append(out, a)
		}
	}
	return out
}
