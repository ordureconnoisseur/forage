package stash

import (
	"regexp"
	"testing"
)

// The pattern decides what the whole Unfiled view can see, and a wrong one
// fails silently by matching nothing. Stash's plain `path` filter is WORD
// based rather than substring, which has produced wrong counts in this repo
// before; this is anchored and literal, so it has to be exercised as such.
func TestUnfiledPattern(t *testing.T) {
	for _, root := range []string{`Z:\Media`, `Z:\Media\`, "/data/porn/Media"} {
		re, err := regexp.Compile(UnfiledPattern(root))
		if err != nil {
			t.Fatalf("root %q produced an invalid pattern: %v", root, err)
		}
		sep := `\`
		if root[0] == '/' {
			sep = "/"
		}
		base := root
		for len(base) > 0 && (base[len(base)-1] == '/' || base[len(base)-1] == '\\') {
			base = base[:len(base)-1]
		}

		for _, want := range []string{
			base + sep + "loose.mp4",
			base + sep + "BLACKED_RAW_106289_1080P.mp4",
			base + sep + "Unsorted" + sep + "a.mp4",
			base + sep + "Unsorted" + sep + "pack" + sep + "b.mp4",
			base + sep + "unsorted" + sep + "c.mp4", // Stash reports what the fs has
		} {
			if !re.MatchString(want) {
				t.Errorf("root %q: should match %q", root, want)
			}
		}
		for _, no := range []string{
			// The whole point: a filed scene must not appear.
			base + sep + "Kenzie Reeves" + sep + "a.mp4",
			base + sep + "Kenzie Reeves" + sep + "sub" + sep + "a.mp4",
			// A sibling directory that merely starts with the root's name.
			base + " Old" + sep + "a.mp4",
			// Somewhere else entirely.
			`D:\Porn\a.mp4`,
			// The root directory itself is not a file in it.
			base,
		} {
			if re.MatchString(no) {
				t.Errorf("root %q: should NOT match %q", root, no)
			}
		}
	}
}

// A root with regex metacharacters must be quoted, not interpreted. Library
// roots on Windows routinely contain characters a regex cares about.
func TestUnfiledPatternQuotesTheRoot(t *testing.T) {
	re, err := regexp.Compile(UnfiledPattern(`Z:\My Media (new)`))
	if err != nil {
		t.Fatalf("invalid: %v", err)
	}
	if !re.MatchString(`Z:\My Media (new)\loose.mp4`) {
		t.Error("a root with parentheses must still match its own files")
	}
	if re.MatchString(`Z:\My Media new\loose.mp4`) {
		t.Error("parentheses were treated as a group rather than as literals")
	}
}

func TestUnfiledPatternEmptyRoot(t *testing.T) {
	if UnfiledPattern("") != "" {
		t.Error("no root means no query, not a pattern that matches everything")
	}
}
