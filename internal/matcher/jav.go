package matcher

import "regexp"

// javCodeRegex matches the canonical JAV scene-identifier shape:
// a 2-6 letter studio code, an optional separator, and a 3-5 digit
// release number. StashDB stores JAV scene titles in this form
// (`SNOS-233`, `IPX-1234`), and the matching uploader-supplied code
// in release filenames is a much stronger identity signal than the
// fuzzy title overlap that drives most western matches.
//
// Uppercase-only intentional: case-insensitive matching false-
// positives on long English titles (`School.300`, `Volume.4000`),
// and both StashDB titles and the overwhelming majority of JAV
// release names preserve the canonical caps. Edge-case lowercase
// uploaders are a follow-up if/when the corpus shows we miss them.
var javCodeRegex = regexp.MustCompile(`\b[A-Z]{2,6}[-._]?\d{3,5}\b`)

// ExtractJAVCodes returns normalized codes ("snos-233" form) found
// in s. De-duplicated, preserves first-occurrence order, returns nil
// when none present.
func ExtractJAVCodes(s string) []string {
	matches := javCodeRegex.FindAllString(s, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		n := normalizeJAVCode(m)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// normalizeJAVCode lowercases letters, drops any separator, and
// reinserts a single dash between the letter prefix and digit
// suffix. "SNOS-233", "SNOS.233", "snos233" all map to "snos-233".
func normalizeJAVCode(s string) string {
	var letters, digits []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			letters = append(letters, c+('a'-'A'))
		case c >= 'a' && c <= 'z':
			letters = append(letters, c)
		case c >= '0' && c <= '9':
			digits = append(digits, c)
		}
	}
	if len(letters) == 0 || len(digits) == 0 {
		return ""
	}
	return string(letters) + "-" + string(digits)
}
