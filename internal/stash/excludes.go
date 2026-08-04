package stash

import (
	"context"
	"regexp"
	"strings"
)

// Keeping the download folder out of Stash's library.
//
// This exists because Stash's exclusion is a raw regex matched against a raw
// filesystem path, and it fails SILENTLY. On the reference library the user
// had written `(?i).*/screen[^/]*/.*` to skip screenshot folders. Stash is on
// Windows, the paths are `D:\Porn\...\Screenlists\...`, and a `/` does not
// match a `\`, so 29,045 images they had excluded were indexed anyway. No
// error, no warning, no count of what was skipped.
//
// That matters more the moment the download folder lives INSIDE the library
// root, which is the layout worth recommending: one folder from the user, one
// filesystem, hardlinks guaranteed rather than hoped for. The cost of that
// layout is that Stash would scan half-written downloads into the library, and
// the only thing standing in the way is a regex someone typed. So forage
// writes it, the same way it already creates the qBit and SAB categories so
// nobody has to configure a download client by hand.

var pathSepSplit = regexp.MustCompile(`[\\/]+`)

// ExcludePattern builds a Stash exclusion regex for everything under path.
//
// Separator-agnostic in every position, because forage routinely talks to a
// Windows Stash from a Linux container and the two disagree about which slash
// a path uses. Case-insensitive, because Windows and macOS filesystems are.
func ExcludePattern(path string) string {
	parts := pathSepSplit.Split(strings.Trim(strings.TrimSpace(path), `\/`), -1)
	var quoted []string
	for _, p := range parts {
		if p != "" {
			quoted = append(quoted, regexp.QuoteMeta(p))
		}
	}
	if len(quoted) == 0 {
		return ""
	}
	// Anchored at the start and closed with a separator, so it covers the
	// folder's CONTENTS and cannot match a sibling whose name merely begins
	// the same way ("downloads-old").
	return `(?i)^` + strings.Join(quoted, `[\\/]`) + `[\\/]`
}

// PathInside reports whether child lies within parent, comparing
// case-insensitively and treating both separators as equivalent.
func PathInside(child, parent string) bool {
	norm := func(s string) string {
		return strings.ToLower(strings.Trim(pathSepSplit.ReplaceAllString(strings.TrimSpace(s), "/"), "/"))
	}
	c, p := norm(child), norm(parent)
	if c == "" || p == "" {
		return false
	}
	return c == p || strings.HasPrefix(c, p+"/")
}

const generalConfigQuery = `
query ForagerStashGeneralConfig {
  configuration { general { excludes imageExcludes stashes { path } } }
}`

const configureGeneralMutation = `
mutation ForagerConfigureGeneral($input: ConfigGeneralInput!) {
  configureGeneral(input: $input) { excludes }
}`

// LibraryConfig is the part of Stash's general config forage cares about.
type LibraryConfig struct {
	// Excludes is the video exclusion regex list, verbatim.
	Excludes []string
	// ImageExcludes is the same for images.
	ImageExcludes []string
	// Paths is every library root Stash scans.
	Paths []string
}

// LibraryConfig reads Stash's library roots and video exclusions.
func (c *Client) LibraryConfig(ctx context.Context) (LibraryConfig, error) {
	var resp struct {
		Configuration struct {
			General struct {
				Excludes      []string `json:"excludes"`
				ImageExcludes []string `json:"imageExcludes"`
				Stashes       []struct {
					Path string `json:"path"`
				} `json:"stashes"`
			} `json:"general"`
		} `json:"configuration"`
	}
	if err := c.do(ctx, generalConfigQuery, nil, &resp); err != nil {
		return LibraryConfig{}, err
	}
	out := LibraryConfig{
		Excludes:      resp.Configuration.General.Excludes,
		ImageExcludes: resp.Configuration.General.ImageExcludes,
	}
	for _, s := range resp.Configuration.General.Stashes {
		out.Paths = append(out.Paths, s.Path)
	}
	return out, nil
}

// AddVideoExclude appends pattern to Stash's exclusion list, and reports
// whether it changed anything.
//
// The existing list is read and re-sent WITH the new entry appended, never
// replaced: these are the user's own patterns and forage has no business
// dropping them. Already present is success with no write.
func (c *Client) AddVideoExclude(ctx context.Context, pattern string) (bool, error) {
	if strings.TrimSpace(pattern) == "" {
		return false, nil
	}
	cfg, err := c.LibraryConfig(ctx)
	if err != nil {
		return false, err
	}
	for _, e := range cfg.Excludes {
		if e == pattern {
			return false, nil
		}
	}
	var resp struct {
		ConfigureGeneral struct {
			Excludes []string `json:"excludes"`
		} `json:"configureGeneral"`
	}
	input := map[string]any{"excludes": append(append([]string{}, cfg.Excludes...), pattern)}
	if err := c.do(ctx, configureGeneralMutation, map[string]any{"input": input}, &resp); err != nil {
		return false, err
	}
	return true, nil
}

// Screenshot folders: the images nobody wants in a library.
//
// Scene packs routinely ship a folder of contact sheets and preview grids
// beside the video: Screens/, Screenlists/, Covers/, Proof/, scr/. Stash
// indexes them as images, and they swamp a gallery view without ever being
// content anyone browses. On the reference library there are 29,045 of them,
// and the user had ALREADY written rules to exclude them; the rules just used
// forward slashes against Windows paths and matched nothing at all.
//
// So forage generates these rather than documenting them: same reasoning as
// the download folder, and the same separator-agnostic construction.
var screenshotFolders = []string{
	"screens", "screenshots", "screenlist", "screenlists",
	"cover", "covers", "proof", "proofs", "scr", "thumbs", "thumbnails",
}

// ScreenshotExcludePatterns returns an image-exclusion regex per screenshot
// folder name, matching that folder ANYWHERE in a path rather than anchored at
// a root, because these folders sit beside each release wherever it landed.
func ScreenshotExcludePatterns() []string {
	out := make([]string, 0, len(screenshotFolders))
	for _, f := range screenshotFolders {
		// [\\/] is slash OR backslash. [\/] would be an ESCAPED slash, which
		// matches only "/" and is precisely the mistake that left 29,045
		// screenshots in this library.
		out = append(out, `(?i)[\\/]`+regexp.QuoteMeta(f)+`[\\/]`)
	}
	return out
}

// AddImageExcludes appends any of the given patterns Stash does not already
// have, and returns how many it added. Existing entries are preserved.
func (c *Client) AddImageExcludes(ctx context.Context, patterns []string) (int, error) {
	var resp struct {
		Configuration struct {
			General struct {
				ImageExcludes []string `json:"imageExcludes"`
			} `json:"general"`
		} `json:"configuration"`
	}
	const q = `query ForagerImageExcludes { configuration { general { imageExcludes } } }`
	if err := c.do(ctx, q, nil, &resp); err != nil {
		return 0, err
	}
	have := map[string]bool{}
	for _, e := range resp.Configuration.General.ImageExcludes {
		have[e] = true
	}
	merged := append([]string{}, resp.Configuration.General.ImageExcludes...)
	added := 0
	for _, p := range patterns {
		if p == "" || have[p] {
			continue
		}
		merged = append(merged, p)
		have[p] = true
		added++
	}
	if added == 0 {
		return 0, nil
	}
	const m = `mutation ForagerConfigureImageExcludes($input: ConfigGeneralInput!) {
  configureGeneral(input: $input) { imageExcludes }
}`
	var out struct {
		ConfigureGeneral struct {
			ImageExcludes []string `json:"imageExcludes"`
		} `json:"configureGeneral"`
	}
	if err := c.do(ctx, m, map[string]any{"input": map[string]any{"imageExcludes": merged}}, &out); err != nil {
		return 0, err
	}
	return added, nil
}

// Unmatchable reports patterns that CANNOT match any of the given library
// paths because of separator style, which is the silent failure this whole
// area suffers from.
//
// Stash matches a raw regex against a raw OS path and says nothing when a
// pattern never fires: no error, no warning, no count of what was skipped. A
// rule written with `/` against `D:\Porn\...` excludes exactly nothing, and
// the only symptom is content you thought you had ruled out showing up in the
// library months later.
func Unmatchable(patterns, libraryPaths []string) []string {
	backslashLib := false
	for _, p := range libraryPaths {
		if strings.Contains(p, `\`) {
			backslashLib = true
			break
		}
	}
	if !backslashLib {
		return nil
	}
	var bad []string
	for _, p := range patterns {
		// A pattern is unmatchable when it uses "/" as a separator and offers
		// no way to match a backslash. `\\` (an escaped backslash) is the only
		// construct that reliably does, whether alone or inside a class like
		// [\\/]; a bare `\` in the pattern is usually a regex escape such as
		// `\.` and says nothing about separators.
		//
		// Deliberately conservative: a false alarm here trains people to
		// ignore the warning, which is worse than the silence it replaces.
		if strings.Contains(p, "/") && !strings.Contains(p, `\\`) {
			bad = append(bad, p)
		}
	}
	return bad
}
