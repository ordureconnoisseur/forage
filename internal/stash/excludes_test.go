package stash

import (
	"regexp"
	"strings"
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

// The 29,045-image case. These patterns are what the reference library had,
// and they match nothing at all against Windows paths.
func TestUnmatchableCatchesForwardSlashPatternsOnWindows(t *testing.T) {
	lib := []string{`Z:\Media`, `D:\Porn`}
	bad := Unmatchable([]string{
		`(?i).*/screen[^/]*/.*`,  // the real one: excludes nothing at all
		`.*/Media/incomplete/.*`, // likewise
		// A regex escape is not a separator. This one still cannot match a
		// backslash path, so a rule keyed on "contains a backslash" would
		// wrongly clear it.
		`.*/proof/.*\.jpg`,
		`(?i)[\\/]covers?[\\/]`,    // slash OR backslash: fine
		`(?i)Z:\\Media\\downloads`, // escaped backslashes: fine
		`somefolder`,               // no separator at all: fine
	}, lib)
	if len(bad) != 3 {
		t.Fatalf("flagged %d patterns %v, want the 3 forward-slash-only ones", len(bad), bad)
	}
}

// On a Linux Stash the same patterns are correct, so flagging them would be
// noise that trains people to ignore the warning.
func TestUnmatchableSaysNothingOnAPosixLibrary(t *testing.T) {
	if bad := Unmatchable([]string{`(?i).*/screen[^/]*/.*`},
		[]string{"/data/porn/Media"}); len(bad) != 0 {
		t.Errorf("flagged %v on a posix library", bad)
	}
}

func TestScreenshotExcludePatternsMatchBothSeparators(t *testing.T) {
	pats := ScreenshotExcludePatterns()
	if len(pats) == 0 {
		t.Fatal("no patterns")
	}
	joined := regexp.MustCompile("(" + strings.Join(pats, ")|(") + ")")
	for _, p := range []string{
		`D:\Porn\Hazel Moore\Screenlists\a.jpg`, // the real 29,045 case
		`D:\Porn\JAV\VRXS-068\Cover\x.jpg`,
		`D:\Porn\Leah\Rip\Proof\wop.jpg`,
		`/data/porn/Media/Perf/screens/a.jpg`, // posix
		`Z:\Media\Perf\SCR\a.jpg`,             // case-insensitive
	} {
		if !joined.MatchString(p) {
			t.Errorf("should exclude %q", p)
		}
	}
	for _, p := range []string{
		// A real scene must never be caught: these are IMAGE excludes, but a
		// folder merely containing the word is not a screenshot folder.
		`D:\Porn\Screencaps Girl\scene.jpg`,
		`D:\Porn\Perf\coverage\a.jpg`,
	} {
		if joined.MatchString(p) {
			t.Errorf("should NOT exclude %q", p)
		}
	}
}
