package matcher

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// fold strips combining marks: NFKD decomposes a precomposed letter
// (é → e + ◌́), then we drop the Mn (nonspacing-mark) runes. This makes the
// matcher diacritic-insensitive. StashDB stores "Renée"/"José García", but
// P2P release names overwhelmingly strip the accents ("Renee"/"Jose Garcia");
// without folding the two sides tokenise to unequal tokens (corpus [ren, e]
// vs release [renee]) and a large slice of accented performers silently never
// matched. Applied to BOTH sides because it lives in Tokenize, the single
// chokepoint.
//
// NFKD (not NFD) so COMPATIBILITY forms decompose too — JAV StashDB titles
// routinely use fullwidth letters/digits/hyphens (ＳＴＡＲＳ－６２９), which
// NFD leaves untouched: fullwidth digits match neither \d (ASCII-only in Go)
// nor any letter class, so they were silently dropped and such scenes could
// never token- or code-match an ASCII release name. Remaining limitation:
// stroke/ligature letters (ø, ł, ß) have no decomposition in either form and
// pass through unchanged, kept as whole tokens by the \p{Lo}/\p{Ll}
// alternatives in caseAndDigitSplit.
//
// norm.NFKD.String is safe for concurrent use (the matcher benches/runs
// Tokenize from many goroutines); a shared transform.Transformer is NOT, so
// we normalise + filter inline rather than reuse a Chain transformer.
func fold(s string) string {
	d := norm.NFKD.String(s)
	var b strings.Builder
	b.Grow(len(d))
	for _, r := range d {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

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

// caseAndDigitSplit further splits each piece into runs:
//  1. lowercase runs (most common)
//  2. all-uppercase runs ending in non-lowercase or end of string —
//     keeps JAV codes (`SNOS`, `OAE`) and other acronyms intact
//     (`HTTP`, `USB`)
//  3. CamelCase words (`Bang`, `Bros`)
//  4. caseless-letter runs (CJK, Thai, …) — kept as whole tokens
//  5. digit runs
//
// Uses Unicode letter classes (\p{Ll}/\p{Lu}/\p{Lo}) rather than [a-z]/[A-Z]
// so letters that survive folding (ø, ß, CJK) aren't dropped — the ASCII
// classes silently discarded them, shattering the surrounding token. For
// pure-ASCII input the classes are exactly equivalent to the old ranges, so
// existing behaviour is unchanged.
//
// Ordering matters: Go's RE2 uses leftmost-first alternation, so an
// earlier alternative wins even if a later one would match longer.
// The all-caps alternative MUST precede the single-uppercase one or
// it never fires — `\p{Lu}\p{Ll}*` would match the first letter of
// `SNOS` standalone and shatter the rest into [s n o s]. That was
// the tokenizer bug that caused JAV release names to score against
// the wrong StashDB scenes.
//
// The all-caps terminator excludes digits: with plain `[^\p{Ll}]` the
// alternative consumed one digit of a following run (`SNOS233` →
// [snos2 33], `S01E02` → [s0 1 e0 2]) while the lowercase forms split
// cleanly (`snos233` → [snos 233]) — a case-dependent asymmetry that
// made the same episode/code tag tokenise to disjoint tokens on the
// two sides of a match. With `\d` excluded, the run before a digit
// backtracks one letter and consumes it as the terminator (`SNOS233` →
// [snos 233]), or falls to the single-uppercase alternative (`S01E02`
// → [s 01 e 02]) — both now identical to their lowercase forms.
var caseAndDigitSplit = regexp.MustCompile(`\p{Ll}+|\p{Lu}+(?:[^\p{Ll}\d]|$)|\p{Lu}\p{Ll}*|\p{Lo}+|\d+`)

// Tokenize splits s on punctuation/whitespace, then further splits each
// piece on case and letter-digit boundaries. Output is lowercase, in
// original order, with empties dropped.
func Tokenize(s string) []string {
	s = fold(s) // diacritic-insensitive: Renée and Renee tokenise alike
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

// trimNonAlnumSuffix drops trailing runes that aren't letters or digits —
// chiefly the single trailing non-lowercase char the all-caps alternative
// (`\p{Lu}+(?:[^\p{Ll}]|$)`) may capture. Rune-aware (not byte-wise): a
// sub-token can now end in a multi-byte letter (ø, CJK), which a byte-level
// trim would corrupt; unicode.IsLetter/IsDigit keep those intact and only
// strip genuine punctuation/symbol tails.
func trimNonAlnumSuffix(s string) string {
	for len(s) > 0 {
		r, size := utf8.DecodeLastRuneInString(s)
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return s
		}
		s = s[:len(s)-size]
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
//
// ConcatNames additionally indexes every multi-token name/alias as its
// CONCATENATED single token ("Mom Drips" → "momdrips"). Member-rip
// release names routinely fuse the site name in lowercase (momdrips_,
// nfbusty_, sisswap.26.05.03), which tokenizes as one token and can
// never subsequence-match the multi-token corpus name — camel-case
// fusions split fine, lowercase ones don't. Concat forms go through
// exactly the same single-token safety rules as real names, so an
// ambiguous concatenation (one that collides with another entity) is
// dropped like any unsafe singleton.
type ScannerOptions struct {
	MinSingleTokenLen int
	SingleTokenRule   SingleTokenRule
	ConcatNames       bool
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
// ConcatNames is on: fused lowercase site names are a studio-side
// release convention (performer names in those same releases arrive
// underscore- or camel-separated and need no concat form).
func StudioScannerOptions() ScannerOptions {
	return ScannerOptions{MinSingleTokenLen: 3, SingleTokenRule: CanonicalUnique, ConcatNames: true}
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

	// names returns the entity's indexable name strings: canonical +
	// aliases, plus (when ConcatNames) the concatenated form of each
	// multi-token name. The concat variants flow through the owners
	// table and the safety rules below exactly like real names.
	names := func(e Entity) []string {
		ns := allNames(e)
		if !opts.ConcatNames {
			return ns
		}
		for _, n := range ns {
			if toks := Tokenize(n); len(toks) > 1 {
				ns = append(ns, strings.Join(toks, ""))
			}
		}
		return ns
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
			for _, n := range names(e) {
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
			for _, n := range names(e) {
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
		for _, n := range names(e) {
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
