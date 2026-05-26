package main

import (
	"regexp"
	"strings"
)

// StrategyFunc is the contract every candidate matching strategy
// fulfils: given a basename, return the set of corpus performer
// stash_ids it believes are present in that basename.
//
// Strategies precompute corpus-side data structures inside their
// factory; the returned closure must be safe for concurrent use, but
// the bench currently runs sequentially per strategy.
type StrategyFunc func(basename string) []string

// tokenSplit splits on characters that release names use to separate
// fields: dots, underscores, dashes, whitespace, common bracket forms,
// and a few punctuation marks that show up in basenames from various
// scrape sources.
var tokenSplit = regexp.MustCompile(`[._\-\s\[\]()!,@'"&+]+`)

func tokenize(s string) []string {
	parts := tokenSplit.Split(strings.ToLower(s), -1)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func tokenSet(tokens []string) map[string]bool {
	out := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		out[t] = true
	}
	return out
}

func dedupe(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// containsSubsequence returns true if needle appears as a contiguous
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

// allNames returns every name variant for a performer (canonical plus
// aliases). Empty entries are skipped.
func allNames(p CachedPerformer) []string {
	out := make([]string, 0, 1+len(p.Aliases))
	if p.Name != "" {
		out = append(out, p.Name)
	}
	for _, a := range p.Aliases {
		if a != "" {
			out = append(out, a)
		}
	}
	return out
}

// substring_naive: lowercased substring match against the full
// basename. No tokenisation, no boundaries. Maximum recall, terrible
// precision — useful as the floor.
func substringNaiveFactory(corpus []CachedPerformer) StrategyFunc {
	type cand struct {
		StashID string
		Needles []string // lowercased
	}
	candidates := make([]cand, 0, len(corpus))
	for _, p := range corpus {
		var ns []string
		for _, n := range allNames(p) {
			ns = append(ns, strings.ToLower(n))
		}
		if len(ns) == 0 {
			continue
		}
		candidates = append(candidates, cand{p.StashID, ns})
	}
	return func(basename string) []string {
		haystack := strings.ToLower(basename)
		hits := make([]string, 0)
		for _, c := range candidates {
			for _, n := range c.Needles {
				if strings.Contains(haystack, n) {
					hits = append(hits, c.StashID)
					break
				}
			}
		}
		return dedupe(hits)
	}
}

// token_aware: tokenise both sides; match each performer name (or
// alias) as a contiguous token subsequence. Catches `Adriana.Chechik`
// but not bare `Adriana` against the performer named "Adriana Chechik".
func tokenAwareFactory(corpus []CachedPerformer) StrategyFunc {
	type cand struct {
		StashID    string
		NameTokens [][]string
	}
	candidates := make([]cand, 0, len(corpus))
	for _, p := range corpus {
		var tt [][]string
		for _, n := range allNames(p) {
			toks := tokenize(n)
			if len(toks) > 0 {
				tt = append(tt, toks)
			}
		}
		if len(tt) == 0 {
			continue
		}
		candidates = append(candidates, cand{p.StashID, tt})
	}
	return func(basename string) []string {
		haystack := tokenize(basename)
		hits := make([]string, 0)
		for _, c := range candidates {
			for _, ntoks := range c.NameTokens {
				if containsSubsequence(haystack, ntoks) {
					hits = append(hits, c.StashID)
					break
				}
			}
		}
		return dedupe(hits)
	}
}

// computeSafeSingleton returns the set of tokens that (a) appear in
// exactly one performer's name/alias tokens across the corpus and (b)
// are at least minLen characters long. Caller passes minLen=1 to
// disable the length filter, or minLen=3 to reject generic 2-letter
// tokens like "An" or "A" that should never be valid match keys even
// if they happen to be unique in this particular library.
func computeSafeSingleton(corpus []CachedPerformer, minLen int) map[string]bool {
	owners := map[string]map[string]bool{}
	for _, p := range corpus {
		for _, n := range allNames(p) {
			for _, t := range tokenize(n) {
				if owners[t] == nil {
					owners[t] = map[string]bool{}
				}
				owners[t][p.StashID] = true
			}
		}
	}
	out := make(map[string]bool, len(owners))
	for t, ownerSet := range owners {
		if len(ownerSet) == 1 && len(t) >= minLen {
			out[t] = true
		}
	}
	return out
}

// makeContiguousMin2 is the shared body of token_min2 and its
// variants. minSingleTokenLen controls whether 1- and 2-character
// single-token aliases (like Ranran Fuji's "An") are eligible.
func makeContiguousMin2(corpus []CachedPerformer, minSingleTokenLen int) StrategyFunc {
	safeSingleton := computeSafeSingleton(corpus, minSingleTokenLen)
	type cand struct {
		StashID    string
		NameTokens [][]string
	}
	candidates := make([]cand, 0, len(corpus))
	for _, p := range corpus {
		var tt [][]string
		for _, n := range allNames(p) {
			toks := tokenize(n)
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
		candidates = append(candidates, cand{p.StashID, tt})
	}
	return func(basename string) []string {
		haystack := tokenize(basename)
		hits := make([]string, 0)
		for _, c := range candidates {
			for _, ntoks := range c.NameTokens {
				if containsSubsequence(haystack, ntoks) {
					hits = append(hits, c.StashID)
					break
				}
			}
		}
		return dedupe(hits)
	}
}

// token_min2: token_aware, but single-token name/alias entries are
// skipped UNLESS that token is unique to one performer across the
// corpus. Allows even 2-char unique tokens (baseline behaviour from
// the first iteration of the bench).
func tokenMin2Factory(corpus []CachedPerformer) StrategyFunc {
	return makeContiguousMin2(corpus, 1)
}

// token_min2_min3: same as token_min2, but single-token aliases must
// be at least 3 characters. This rejects generic 2-letter tokens
// ("An", "A", "I") that the bench identified as the dominant FP class.
func tokenMin2Min3Factory(corpus []CachedPerformer) StrategyFunc {
	return makeContiguousMin2(corpus, 3)
}

// token_min2_first_unique: builds on token_min2_min3 and adds a
// recall fallback — if a performer's canonical FIRST token is unique
// across all canonical first tokens in the corpus (e.g. "abella" →
// only Abella Danger), allow first-name-only matches. Recovers the
// "Abella Gets..." / "Riley & Abella" filename class without opening
// the door to common-first-name collisions.
func tokenMin2FirstUniqueFactory(corpus []CachedPerformer) StrategyFunc {
	base := makeContiguousMin2(corpus, 3)

	// Unique canonical first names → performer stash_id.
	firstOwners := map[string]map[string]bool{}
	for _, p := range corpus {
		toks := tokenize(p.Name)
		if len(toks) == 0 {
			continue
		}
		first := toks[0]
		if len(first) < 3 {
			continue
		}
		if firstOwners[first] == nil {
			firstOwners[first] = map[string]bool{}
		}
		firstOwners[first][p.StashID] = true
	}
	uniqueFirstName := map[string]string{}
	for t, ownerSet := range firstOwners {
		if len(ownerSet) == 1 {
			for id := range ownerSet {
				uniqueFirstName[t] = id
			}
		}
	}

	return func(basename string) []string {
		hits := base(basename)
		seen := make(map[string]bool, len(hits))
		for _, h := range hits {
			seen[h] = true
		}
		for _, tok := range tokenize(basename) {
			if id, ok := uniqueFirstName[tok]; ok && !seen[id] {
				hits = append(hits, id)
				seen[id] = true
			}
		}
		return hits
	}
}

// token_last_required: for multi-token names, the LAST token of the
// performer's name must appear in the basename AND at least one other
// token of the name must also appear (in any order, non-contiguous).
// Single-token names match like token_aware. Allows reversed-order
// matches ("Reid Riley") that contiguous strategies miss.
func tokenLastRequiredFactory(corpus []CachedPerformer) StrategyFunc {
	type cand struct {
		StashID    string
		NameTokens [][]string
	}
	candidates := make([]cand, 0, len(corpus))
	for _, p := range corpus {
		var tt [][]string
		for _, n := range allNames(p) {
			toks := tokenize(n)
			if len(toks) > 0 {
				tt = append(tt, toks)
			}
		}
		if len(tt) == 0 {
			continue
		}
		candidates = append(candidates, cand{p.StashID, tt})
	}
	return func(basename string) []string {
		hSet := tokenSet(tokenize(basename))
		hits := make([]string, 0)
		for _, c := range candidates {
			matched := false
			for _, toks := range c.NameTokens {
				if len(toks) == 1 {
					if hSet[toks[0]] {
						matched = true
						break
					}
					continue
				}
				last := toks[len(toks)-1]
				if !hSet[last] {
					continue
				}
				for i := 0; i < len(toks)-1; i++ {
					if hSet[toks[i]] {
						matched = true
						break
					}
				}
				if matched {
					break
				}
			}
			if matched {
				hits = append(hits, c.StashID)
			}
		}
		return dedupe(hits)
	}
}
