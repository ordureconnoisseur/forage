// Package pathmap translates between forager-side filesystem paths and
// the paths Stash uses for the same files. forager often runs in a
// Docker container on Linux (seeing e.g. /data/media/Media) while Stash
// sees a different mount or OS path (e.g. Z:\Media on Windows). A
// FORAGER_STASH_PATH_MAPPING of "<forager-prefix>:<stash-prefix>" lets
// the daemon scope Stash scans and locate scenes by path.
//
// Extracted from the poller so the API layer (pack teardown) and the
// poller (scan scoping, pack confirm/dedup) share one implementation
// rather than each carrying its own copy.
package pathmap

import "strings"

// Translate rewrites a forager-side path into the path Stash uses for
// the same file. mapping is "<forager-prefix>:<stash-prefix>"; e.g.
// "/data/media/Media:Z:\\Media" turns
// "/data/media/Media/Hazel Moore/scene.mp4" into
// "Z:\\Media\\Hazel Moore\\scene.mp4".
//
// Returns empty when:
//   - mapping is unset (caller falls back to a full-library scan, or to
//     the basename via Base)
//   - the prefix doesn't match (path is outside the configured mount)
//
// Path separators are normalised — forager's container view uses '/',
// Stash on Windows uses '\'. We detect Windows-style targets and flip
// any '/' characters after the prefix to '\'.
func Translate(foragerPath, mapping string) string {
	if mapping == "" || foragerPath == "" {
		return ""
	}
	// Split on the first colon: accept any prefix on the left and treat
	// the rest as the stash-side target verbatim. Windows targets contain
	// embedded colons (Z:\Media), so we deliberately don't split on those.
	idx := strings.Index(mapping, ":")
	if idx <= 0 || idx == len(mapping)-1 {
		return ""
	}
	foragerPrefix := mapping[:idx]
	stashPrefix := mapping[idx+1:]
	if !strings.HasPrefix(foragerPath, foragerPrefix) {
		return ""
	}
	suffix := foragerPath[len(foragerPrefix):]
	if strings.ContainsRune(stashPrefix, '\\') {
		// Windows-style target — flip forward slashes in the suffix to
		// backslashes so the joined path is well-formed.
		suffix = strings.ReplaceAll(suffix, "/", `\`)
	}
	return stashPrefix + suffix
}

// Base returns the last path segment of p, handling both '/' and '\'
// separators (forager sees '/', Stash on Windows reports '\').
func Base(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[i+1:]
		}
	}
	return path
}
