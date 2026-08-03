package stash

import (
	"regexp"
	"testing"
)

// The whole reason forage writes this pattern instead of asking a person to.
// The reference library's own hand-written exclusion was `(?i).*/screen[^/]*/.*`
// against Windows paths like `D:\Porn\...\Screenlists\...`, and a `/` does not
// match a `\`, so 29,045 images that were supposedly excluded got indexed.
// Silently: Stash reports no error and no count.
func TestExcludePatternMatchesEitherSeparator(t *testing.T) {
	re := regexp.MustCompile(ExcludePattern(`Z:\Media\.downloads`))
	for _, p := range []string{
		`Z:\Media\.downloads\file.mp4`,
		`Z:\Media\.downloads\sub\file.mp4`,
		`Z:/Media/.downloads/file.mp4`, // a Linux Stash on the same share
		`z:\media\.DOWNLOADS\file.mp4`, // case-insensitive filesystem
		`Z:\Media/.downloads\file.mp4`, // mixed, which really does happen
	} {
		if !re.MatchString(p) {
			t.Errorf("should exclude %q", p)
		}
	}
}

// An exclusion that swallows more than intended is worse than none: it hides
// real library files and the user cannot tell why they never appear.
func TestExcludePatternDoesNotOverreach(t *testing.T) {
	re := regexp.MustCompile(ExcludePattern(`Z:\Media\downloads`))
	for _, p := range []string{
		// A sibling whose name merely starts the same way.
		`Z:\Media\downloads-old\file.mp4`,
		`Z:\Media\downloadsomething\file.mp4`,
		// The library itself.
		`Z:\Media\Kenzie Reeves\file.mp4`,
		// Same folder name under a DIFFERENT root.
		`D:\Porn\Media\downloads\file.mp4`,
		// The directory entry itself is not a file inside it.
		`Z:\Media\downloads`,
	} {
		if re.MatchString(p) {
			t.Errorf("should NOT exclude %q", p)
		}
	}
}

// Regex metacharacters in a path must be literals. Library roots contain them
// routinely: "Z:\My Media (new)".
func TestExcludePatternQuotesMetacharacters(t *testing.T) {
	re := regexp.MustCompile(ExcludePattern(`Z:\My Media (new)\dl`))
	if !re.MatchString(`Z:\My Media (new)\dl\a.mp4`) {
		t.Error("a root with parentheses must match its own files")
	}
	if re.MatchString(`Z:\My Media new\dl\a.mp4`) {
		t.Error("parentheses were treated as a group rather than literals")
	}
}

func TestExcludePatternEmpty(t *testing.T) {
	for _, p := range []string{"", "   ", `\`, `/`} {
		if got := ExcludePattern(p); got != "" {
			t.Errorf("ExcludePattern(%q) = %q, want empty: a pattern for nothing "+
				"would anchor at the filesystem root and hide the library", p, got)
		}
	}
}

func TestPathInside(t *testing.T) {
	for _, c := range []struct {
		child, parent string
		want          bool
	}{
		{`Z:\Media\downloads`, `Z:\Media`, true},
		{`Z:/Media/downloads`, `Z:\Media`, true}, // separators disagree
		{`z:\media\downloads`, `Z:\Media`, true}, // case differs
		{`Z:\Media`, `Z:\Media`, true},           // the root itself
		{`Z:\Media\`, `Z:\Media`, true},          // trailing separator
		{`Z:\downloads`, `Z:\Media`, false},      // a sibling: Stash never scans it
		{`Z:\MediaOther\dl`, `Z:\Media`, false},  // shared prefix, different folder
		{`D:\Porn\downloads`, `Z:\Media`, false}, // another drive entirely
		{"", `Z:\Media`, false},
		{`Z:\Media\dl`, "", false},
	} {
		if got := PathInside(c.child, c.parent); got != c.want {
			t.Errorf("PathInside(%q, %q) = %v, want %v", c.child, c.parent, got, c.want)
		}
	}
}
