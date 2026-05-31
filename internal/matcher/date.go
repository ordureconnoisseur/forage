// Package matcher contains the building blocks of forager's Phase A
// matching pipeline: extractors that pull structured fields (date,
// studio, performers) out of release-name-shaped strings, plus the
// scoring + reconciliation logic that turns them into ranked scene
// candidates.
//
// Each extractor is a pure function over strings — no I/O, no global
// state — so they're independently benchable via the rigs under
// tools/. The matcher proper composes them.
package matcher

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ExtractedDate is a single date hit in a string. Date is normalised
// to YYYY-MM-DD. Match is the raw substring that produced it (useful
// for debugging and for the bench's failure CSV). Pos is the byte
// offset of the match in the input; lower-Pos hits are generally more
// trustworthy in release-name strings, since dates usually sit near
// the front.
type ExtractedDate struct {
	Date     string
	Match    string
	Pos      int
	Format   string // "yyyy.mm.dd", "yy.mm.dd", "yyyymmdd", ...
	InParens bool   // immediately preceded by ( or [ — a strong signal for DD.MM.YY format
}

// Compiled patterns, ordered by specificity. Go's RE2 dialect has no
// backreferences, so we use one pattern per separator instead of a
// single `(sep)...\1` form. The year prefix `(19|20)` keeps random
// 4-digit IDs from masquerading as dates.
//
// Boundary handling: we anchor matches with `(?:^|[^0-9])` on the left
// and `(?:[^0-9]|$)` on the right so partial numeric strings don't
// match.
type datePattern struct {
	rx       *regexp.Regexp
	label    string
	twoYY    bool // true → group 1 is 2-digit year that needs disambiguation; emits both YY.MM.DD and DD.MM.YY interpretations
	yearLast bool // true → group 3 is the (4-digit) year; group 1 is day. Used by DD.MM.YYYY family.
}

var datePatterns = []datePattern{
	// 4-digit-year, dotted: 2024.05.15
	{regexp.MustCompile(`(?:^|[^0-9])((?:19|20)\d{2})\.(\d{2})\.(\d{2})(?:[^0-9]|$)`), "yyyy.mm.dd", false, false},
	{regexp.MustCompile(`(?:^|[^0-9])((?:19|20)\d{2})-(\d{2})-(\d{2})(?:[^0-9]|$)`), "yyyy-mm-dd", false, false},
	{regexp.MustCompile(`(?:^|[^0-9])((?:19|20)\d{2})_(\d{2})_(\d{2})(?:[^0-9]|$)`), "yyyy_mm_dd", false, false},
	{regexp.MustCompile(`(?:^|[^0-9])((?:19|20)\d{2})(\d{2})(\d{2})(?:[^0-9]|$)`), "yyyymmdd", false, false},
	{regexp.MustCompile(`(?:^|[^0-9])((?:19|20)\d{2}) (\d{2}) (\d{2})(?:[^0-9]|$)`), "yyyy mm dd", false, false},

	// DD.MM.YYYY — European/scraper convention, often paren-wrapped.
	// Unambiguous because the year is 4 digits.
	{regexp.MustCompile(`(?:^|[^0-9])(\d{2})\.(\d{2})\.((?:19|20)\d{2})(?:[^0-9]|$)`), "dd.mm.yyyy", false, true},
	{regexp.MustCompile(`(?:^|[^0-9])(\d{2})-(\d{2})-((?:19|20)\d{2})(?:[^0-9]|$)`), "dd-mm-yyyy", false, true},
	{regexp.MustCompile(`(?:^|[^0-9])(\d{2})_(\d{2})_((?:19|20)\d{2})(?:[^0-9]|$)`), "dd_mm_yyyy", false, true},

	// 2-digit-year, dotted: 24.05.15  — common P2P scene-release form
	{regexp.MustCompile(`(?:^|[^0-9])(\d{2})\.(\d{2})\.(\d{2})(?:[^0-9]|$)`), "yy.mm.dd", true, false},
	{regexp.MustCompile(`(?:^|[^0-9])(\d{2})-(\d{2})-(\d{2})(?:[^0-9]|$)`), "yy-mm-dd", true, false},
	{regexp.MustCompile(`(?:^|[^0-9])(\d{2})_(\d{2})_(\d{2})(?:[^0-9]|$)`), "yy_mm_dd", true, false},
	// Space-separated — the dominant scene-release form ("Studio 25 08 15
	// Performer Title …"). calendar validation (month 1-12, day 1-31)
	// keeps stray number triples from matching.
	{regexp.MustCompile(`(?:^|[^0-9])(\d{2}) (\d{2}) (\d{2})(?:[^0-9]|$)`), "yy mm dd", true, false},
	// Intentionally no "yymmdd" pattern: 6 bare digits is too ambiguous.
}

// ExtractDates returns every date-shaped substring found in s,
// normalised to YYYY-MM-DD. Order is unspecified; the matcher calls
// TopDate (which sorts by format priority + position).
//
// For 2-digit-year patterns the same digit triple is genuinely
// ambiguous: `21.07.20` could be YY.MM.DD (2021-07-20) or DD.MM.YY
// (2020-07-21). We emit *both* interpretations and let TopDate pick by
// year-recency (20XX over 19XX) — that resolves the
// "29.01.20 → 1929-01-20" misread without losing the YY.MM.DD path for
// modern scene releases.
func ExtractDates(s string) []ExtractedDate {
	var out []ExtractedDate
	seen := map[string]bool{}

	emit := func(d ExtractedDate) {
		key := fmt.Sprintf("%s@%d@%s", d.Date, d.Pos, d.Format)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, d)
	}

	for _, dp := range datePatterns {
		for _, m := range dp.rx.FindAllStringSubmatchIndex(s, -1) {
			// m: [fullStart, fullEnd, g1Start, g1End, g2Start, g2End, g3Start, g3End]
			a, _ := strconv.Atoi(s[m[2]:m[3]])
			b, _ := strconv.Atoi(s[m[4]:m[5]])
			c, _ := strconv.Atoi(s[m[6]:m[7]])
			match := s[m[2]:m[7]]
			pos := m[2]

			if !dp.twoYY {
				// 4-digit-year forms: one interpretation. yearLast=true
				// means year is in group 3 (DD.MM.YYYY); otherwise
				// group 1 holds the year (YYYY.MM.DD).
				y, mo, d := a, b, c
				if dp.yearLast {
					d, mo, y = a, b, c
				}
				if !validYMD(y, mo, d) {
					continue
				}
				emit(ExtractedDate{
					Date:   fmt.Sprintf("%04d-%02d-%02d", y, mo, d),
					Match:  match,
					Pos:    pos,
					Format: dp.label,
				})
				continue
			}

			// 2-digit-year: emit both YY.MM.DD and DD.MM.YY when each
			// passes calendar validation. TopDate ranks by century +
			// paren context.
			inParens := pos > 0 && (s[pos-1] == '(' || s[pos-1] == '[')
			if y := disambiguateYY(a); validYMD(y, b, c) {
				emit(ExtractedDate{
					Date:     fmt.Sprintf("%04d-%02d-%02d", y, b, c),
					Match:    match,
					Pos:      pos,
					Format:   dp.label, // yy{sep}mm{sep}dd
					InParens: inParens,
				})
			}
			if y := disambiguateYY(c); validYMD(y, b, a) {
				emit(ExtractedDate{
					Date:     fmt.Sprintf("%04d-%02d-%02d", y, b, a),
					Match:    match,
					Pos:      pos,
					Format:   "dd" + dp.label[2:len(dp.label)-2] + "yy",
					InParens: inParens,
				})
			}
		}
	}

	return out
}

// TopDate returns the single most-trustworthy date hit, or "" if none.
//
// Priority (most → least):
//  1. 4-digit-year formats (`yyyy.mm.dd` and variants) — unambiguous.
//  2. 2-digit formats whose disambiguated year is 20XX, preferring the
//     MORE RECENT year. This resolves ambiguous digit triples like
//     `13.06.19` (could be 2013-06-19 or 2019-06-13) toward the recent
//     interpretation, which is empirically right far more often for
//     adult content.
//  3. 2-digit formats with 19XX years — almost always wrong for modern
//     content, included only as a last resort.
//  4. Tiebreak: lower byte position (release-name dates usually sit
//     near the front).
func TopDate(s string) string {
	hits := ExtractDates(s)
	if len(hits) == 0 {
		return ""
	}
	best := hits[0]
	for _, h := range hits[1:] {
		if dateBetter(h, best) {
			best = h
		}
	}
	return best.Date
}

// dateBetter reports whether candidate a should outrank b.
//
// Empirical finding (from the bench): when both YY.MM.DD and DD.MM.YY
// interpretations land in 20XX, the YY.MM.DD reading is correct far
// more often in this library (scene-release naming dominates), so we
// prefer YY.MM.DD on ties. We only switch to DD.MM.YY when the
// YY.MM.DD interpretation produces a 19XX year, which is almost
// always the `(29.01.20)` paren-date misread we're trying to fix.
//
// 4-digit-year formats (yyyy.mm.dd, dd.mm.yyyy, ...) always win over
// 2-digit-year formats — the year is unambiguous.
func dateBetter(a, b ExtractedDate) bool {
	aIs4 := strings.Contains(a.Format, "yyyy")
	bIs4 := strings.Contains(b.Format, "yyyy")
	if aIs4 != bIs4 {
		return aIs4
	}
	aYear, _ := strconv.Atoi(a.Date[:4])
	bYear, _ := strconv.Atoi(b.Date[:4])
	aIs20 := aYear >= 2000
	bIs20 := bYear >= 2000
	if aIs20 != bIs20 {
		return aIs20
	}
	// Both 20XX or both 19XX. Same digit triple emits both YY.MM.DD
	// and DD.MM.YY; choose based on the immediate left-context.
	// Paren-wrapped: DD.MM.YY (curated/scraper convention).
	// Otherwise: YY.MM.DD (scene-release convention).
	aIsYY := strings.HasPrefix(a.Format, "yy")
	bIsYY := strings.HasPrefix(b.Format, "yy")
	if aIsYY != bIsYY {
		paren := a.InParens || b.InParens
		if paren {
			return !aIsYY
		}
		return aIsYY
	}
	return a.Pos < b.Pos
}

// disambiguateYY maps a 2-digit year to a 4-digit year. Adult content
// release names are overwhelmingly post-2000; we treat anything ≤
// (currentYear%100 + small slack) as 20YY, otherwise 19YY. The "small
// slack" lets us not break the moment we cross into a new year — if
// today is 2026, "27" still parses as 2027 (a near-future date) rather
// than 1927.
func disambiguateYY(yy int) int {
	const currentYY = 26 // forager build year (2026); update if releases start dating into the future
	const slack = 1
	if yy <= currentYY+slack {
		return 2000 + yy
	}
	return 1900 + yy
}

// validYMD is a loose calendar check — month 1-12, day 1-31, no
// leap-year strictness. We're filtering obvious garbage (00/00/00,
// 99/99/99), not validating actual calendar dates.
func validYMD(y, m, d int) bool {
	if m < 1 || m > 12 {
		return false
	}
	if d < 1 || d > 31 {
		return false
	}
	if y < 1900 || y > 2100 {
		return false
	}
	return true
}
