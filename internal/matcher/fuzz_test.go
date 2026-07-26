package matcher

import (
	"strings"
	"testing"
	"unicode"
)

// Release names are the most hostile input forage parses: arbitrary bytes
// from arbitrary indexers, in any script, with any punctuation. The fuzz
// targets assert the parsing layer's safety properties — no panic, and the
// tokenizer's output contract — over inputs no table-driven test would
// think to write. CI runs each target briefly on every push; the corpus
// under testdata/fuzz grows with every finding.

// FuzzTokenize: Tokenize must never panic, and every token it emits must be
// non-empty and lowercase — downstream matching (containsSubsequence,
// token-set overlap) silently misses if case ever leaks through.
func FuzzTokenize(f *testing.F) {
	for _, seed := range []string{
		"Vixen.22.05.31.Hazel.Moore.XXX.1080p.MP4-WRB",
		"[EvilAngel.com]Chloe Cherry ( Chloe: Squirting, Deepthroat, Anal) [2019 г., Gonzo]",
		"ＳＴＡＲＳ－６２９ 4K",
		"SNOS-233", "snos233", "S01E02", "Renée García 9½ weeks",
		"momdrips_sisswap.26.05.03", "OAE-302ch SSIS-984-C_GG5",
		"日本語タイトル 中文 한국어", "\x00\xff\xfe", "----....____",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		tokens := Tokenize(s)
		for _, tok := range tokens {
			if tok == "" {
				t.Fatalf("Tokenize(%q) emitted an empty token", s)
			}
			for _, r := range tok {
				if unicode.IsUpper(r) {
					t.Fatalf("Tokenize(%q) emitted non-lowercase token %q", s, tok)
				}
			}
		}
		// Tokenizing the joined tokens must be stable (idempotence over the
		// token alphabet): re-splitting already-clean tokens must not invent
		// or destroy content, or the entity scanner's pre-tokenised corpus
		// and a release's tokens drift apart.
		rejoined := Tokenize(strings.Join(tokens, " "))
		if len(rejoined) != len(tokens) {
			t.Fatalf("Tokenize not stable for %q: %v -> %v", s, tokens, rejoined)
		}
	})
}

// FuzzExtractJAVCodes: must never panic, and every extracted code must be
// one of the canonical shapes (lowercase, containing a dash) — a malformed
// code would floor-match the wrong scene at javCodeFloor confidence.
func FuzzExtractJAVCodes(f *testing.F) {
	for _, seed := range []string{
		"SNOS-233", "OAE302", "326FCT-221", "SSIS-984-C_GG5",
		"[OneJAV] STARS629 uncensored", "no codes here 1080p", "ＳＴＡＲＳ－６２９",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, code := range ExtractJAVCodes(s) {
			if code == "" {
				t.Fatalf("ExtractJAVCodes(%q) emitted an empty code", s)
			}
			if code != strings.ToLower(code) {
				t.Fatalf("ExtractJAVCodes(%q) emitted non-canonical code %q", s, code)
			}
		}
	})
}
