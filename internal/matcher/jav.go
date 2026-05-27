package matcher

import "regexp"

// javCodeRegex matches canonical JAV scene-identifier shapes:
//
//	SNOS-233          → snos-233      (the canonical form)
//	SNOS.233 / snos233→ snos-233      (separator + case variants)
//	OAE-302ch         → oae-302       (trailing chapter/version marker)
//	SSIS-984-C_GG5    → ssis-984      (trailing re-edition marker)
//	326FCT-221        → fct-221 + 326fct-221 (digit-prefixed
//	                    distributor codes — StashDB inconsistently
//	                    stores these with or without the prefix, so
//	                    ExtractJAVCodes emits both forms to match
//	                    either)
//
// Uppercase-only on the letter block: case-insensitive matching
// false-positives on long English titles (`School.300`,
// `Volume.4000`). Both StashDB titles and the overwhelming majority
// of JAV release names preserve the canonical caps.
//
// No trailing word-boundary: the `OAE-302ch` variant runs lowercase
// letters straight after the digits, and `\b` between `2` and `c`
// (both word characters) fails. Greedy `\d{3,5}` still bounds the
// digit count.
var javCodeRegex = regexp.MustCompile(`\b(\d{2,4})?([A-Z]{2,6})[-._]?(\d{3,5})`)

// ExtractJAVCodes returns normalized codes ("snos-233" form) found
// in s. De-duplicated, preserves first-occurrence order, returns nil
// when none present. When the source string carries a digit prefix
// (e.g. `326FCT-221`), both the full form (`326fct-221`) and the
// bare studio+number form (`fct-221`) are emitted — StashDB does
// not consistently include the prefix, so we match on either.
func ExtractJAVCodes(s string) []string {
	matches := javCodeRegex.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(matches)*2)
	emit := func(code string) {
		if code == "" || seen[code] {
			return
		}
		seen[code] = true
		out = append(out, code)
	}
	for _, m := range matches {
		// m[1] = digit prefix (may be empty)
		// m[2] = letter block
		// m[3] = digit suffix
		letters := lowercaseASCII(m[2])
		digits := m[3]
		if letters == "" || digits == "" {
			continue
		}
		bare := letters + "-" + digits
		emit(bare)
		if m[1] != "" {
			emit(m[1] + bare)
		}
	}
	return out
}

func lowercaseASCII(s string) string {
	buf := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		buf[i] = c
	}
	return string(buf)
}
