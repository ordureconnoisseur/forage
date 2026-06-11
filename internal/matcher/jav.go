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
// (both word characters) fails. The digit count is bounded instead by
// the overflow group: greedy `\d{3,7}` covers everything up to FC2's
// 7-digit IDs, and any digits left over land in the trailing `(\d*)`,
// which ExtractJAVCodes treats as "not a JAV code at all". Without the
// overflow check, a longer run would silently truncate — FC2-PPV-1234567
// and FC2-PPV-1234599 both used to extract as `ppv-12345` under
// `\d{3,5}`, and a shared code is treated as scene IDENTITY downstream
// (javCodeFloor, release verification), so truncation made distinct
// sequential FC2 scenes "verified" matches of each other.
var javCodeRegex = regexp.MustCompile(`\b(\d{2,4})?([A-Z]{2,6})[-._]?(\d{3,7})(\d*)([pPiI])?`)

// javMarkerWords are release-name quality/codec tokens that satisfy the
// letter-block shape and sit right next to resolution-sized numbers —
// "XXX.1080p" → xxx-1080, "WEB-DL.1080p" → dl-1080, "FHD-1080" →
// fhd-1080. A shared code is treated as scene IDENTITY downstream
// (javCodeFloor, release verification at 0.85 with no other signal), so
// these pseudo-codes let two unrelated releases "verify" each other
// whenever both carry the same marker. None of these collide with real
// JAV label prefixes.
// resolutionDigits are the pixel heights/widths release names attach a
// trailing p/i to. A real JAV serial followed by a glued word that
// happens to start with p ("...602part2") must not be mistaken for one.
var resolutionDigits = map[string]bool{
	"360": true, "480": true, "540": true, "576": true, "720": true,
	"1080": true, "1440": true, "2160": true, "4320": true,
}

var javMarkerWords = map[string]bool{
	"xxx": true, "web": true, "dl": true, "webdl": true, "rip": true,
	"fhd": true, "uhd": true, "qhd": true, "fullhd": true, "hd": true, "sd": true, "hq": true,
	"avc": true, "hevc": true, "xvid": true, "divx": true, "mp": true,
	"bdrip": true, "webrip": true, "dvdrip": true, "hdrip": true, "hdtv": true, "bluray": true,
}

// ExtractJAVCodes returns normalized codes ("snos-233" form) found
// in s. De-duplicated, preserves first-occurrence order, returns nil
// when none present. When the source string carries a digit prefix
// (e.g. `326FCT-221`), both the full form (`326fct-221`) and the
// bare studio+number form (`fct-221`) are emitted — StashDB does
// not consistently include the prefix, so we match on either.
func ExtractJAVCodes(s string) []string {
	// The regex is ASCII; fold first so fullwidth JAV titles
	// (ＳＴＡＲＳ－６２９) extract the same code as their ASCII release
	// names. No-op for already-ASCII input.
	s = fold(s)
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
		// m[4] = digit overflow past the 7-digit cap (non-empty → reject:
		//        the run is too long to be a JAV id, and emitting a
		//        truncated code would collide distinct scenes)
		// m[5] = a p/i straight after the digits — "1080p"/"1080i": a
		//        resolution token, not a JAV id. Only disqualifying when
		//        the digits ARE a resolution: "IPX-602part2" glues "part"
		//        after a real code and must keep extracting, while chapter
		//        markers ("302ch") use other letters and never hit this.
		if m[4] != "" {
			continue
		}
		letters := lowercaseASCII(m[2])
		digits := m[3]
		if m[5] != "" && resolutionDigits[digits] {
			continue
		}
		if letters == "" || digits == "" || javMarkerWords[letters] {
			continue
		}
		bare := letters + "-" + digits
		emit(bare)
		if m[1] != "" {
			emit(m[1] + bare)
		}
	}
	if len(out) == 0 {
		return nil // documented contract; also when every match was rejected
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
