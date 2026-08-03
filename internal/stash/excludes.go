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
  configuration { general { excludes stashes { path } } }
}`

const configureGeneralMutation = `
mutation ForagerConfigureGeneral($input: ConfigGeneralInput!) {
  configureGeneral(input: $input) { excludes }
}`

// LibraryConfig is the part of Stash's general config forage cares about.
type LibraryConfig struct {
	// Excludes is the video exclusion regex list, verbatim.
	Excludes []string
	// Paths is every library root Stash scans.
	Paths []string
}

// LibraryConfig reads Stash's library roots and video exclusions.
func (c *Client) LibraryConfig(ctx context.Context) (LibraryConfig, error) {
	var resp struct {
		Configuration struct {
			General struct {
				Excludes []string `json:"excludes"`
				Stashes  []struct {
					Path string `json:"path"`
				} `json:"stashes"`
			} `json:"general"`
		} `json:"configuration"`
	}
	if err := c.do(ctx, generalConfigQuery, nil, &resp); err != nil {
		return LibraryConfig{}, err
	}
	out := LibraryConfig{Excludes: resp.Configuration.General.Excludes}
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
