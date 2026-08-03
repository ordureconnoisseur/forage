package stash

import (
	"context"
	"fmt"
	"regexp"
)

// Finding the scenes that are not filed anywhere.
//
// "Unfiled" is a property of the LIBRARY, not of forage's own records. Every
// count forage produced before this asked its grabs table, which knows only
// what forage fetched or adopted: on the reference library that is about 560
// rows against 5,325 scenes sitting under Unsorted, and it missed 194 files
// lying loose in the library root entirely. So this asks Stash.
//
// Two shapes count as unfiled:
//   - directly in the library root, which nothing should ever be, and which
//     FindScenesUnderPath cannot express (anchoring on a directory boundary
//     there matches the whole library)
//   - anywhere under a fallback folder: <root>/Unfiled, and <root>/Unsorted,
//     which is what older libraries call the same bin
//
// Hence a regex rather than the substring filters used elsewhere. Note Stash's
// plain `path` filter is WORD based, not substring, which has produced wrong
// counts here before; MATCHES_REGEX is literal and anchored.

// UnfiledScene is a library scene with at least one file that is not under a
// performer folder.
type UnfiledScene struct {
	ID    string
	Title string
	// FilePath is the file that actually matched, NOT Files[0].
	//
	// Stash matches a scene when ANY of its files matches, and one scene
	// routinely holds several: a re-download whose fingerprint matches is
	// filed as an extra file on the existing scene. So the first file is
	// frequently a properly filed copy while the match came from a stray one.
	// Measured while building this: a pattern for "under Unsorted" returned a
	// scene whose Files[0] was Z:\Media\cecerose\VIDEOS\..., which contains
	// no such path. Taking Files[0] here would have reported a filed path as
	// unfiled and, worse, moved the filed copy.
	FilePath string
	// OtherFiles is how many MORE of this scene's files are also unfiled.
	// Nonzero means filing one leaves siblings behind.
	OtherFiles int
	Performers []ScenePerformer
	// StashIDs is every cross-id Stash holds. Empty means no metadata source
	// has ever identified this file, which for amateur content is permanent
	// rather than pending.
	StashIDs []StashID
}

const unfiledQuery = `
query ForagerUnfiledScenes($value: String!, $page: Int!, $perPage: Int!) {
  findScenes(
    scene_filter: { path: { value: $value, modifier: MATCHES_REGEX } }
    filter: { page: $page, per_page: $perPage, sort: "path", direction: ASC }
  ) {
    count
    scenes {
      id
      title
      files { path }
      performers { id name scene_count }
      stash_ids { endpoint stash_id }
    }
  }
}`

// UnfiledPattern is the regex matching an unfiled path under root. Exported so
// a test can assert the shape without a live Stash, and so the handler can log
// exactly what it asked for when a count looks wrong.
//
// Both separators are accepted in every position: Stash on Windows reports
// backslashes, on Linux forward slashes, and forage is routinely pointed at a
// Windows Stash from a Linux container.
// folders are the fallback bin names to accept. Both the current and the
// legacy spelling are always passed in practice (placer.UnfiledFolders), so a
// library never stops being readable because the preferred name changed.
func UnfiledPattern(root string, folders []string) string {
	if root == "" {
		return ""
	}
	// Trim a trailing separator so the pattern does not end up with two.
	for len(root) > 0 && (root[len(root)-1] == '/' || root[len(root)-1] == '\\') {
		root = root[:len(root)-1]
	}
	q := regexp.QuoteMeta(root)
	// Either one segment after root (loose in the root), or anything at all
	// under one of the fallback folders. Those are matched case-insensitively:
	// Stash reports whatever the filesystem handed it, and a case-insensitive
	// filesystem will happily answer "unsorted".
	alts := ""
	for _, f := range folders {
		if f == "" {
			continue
		}
		alts += `|(?i:` + regexp.QuoteMeta(f) + `)[\\/].+`
	}
	return `^` + q + `[\\/](?:[^\\/]+` + alts + `)$`
}

// FindUnfiledScenes returns every scene whose file is loose in the library
// root or under one of the fallback folders.
func (c *Client) FindUnfiledScenes(ctx context.Context, root string, folders []string) ([]UnfiledScene, error) {
	pattern := UnfiledPattern(root, folders)
	if pattern == "" {
		return nil, nil
	}
	const perPage = 500
	var out []UnfiledScene
	type resp struct {
		FindScenes struct {
			Scenes []struct {
				ID    string `json:"id"`
				Title string `json:"title"`
				Files []struct {
					Path string `json:"path"`
				} `json:"files"`
				Performers []ScenePerformer `json:"performers"`
				StashIDs   []StashID        `json:"stash_ids"`
			} `json:"scenes"`
		} `json:"findScenes"`
	}
	// The same pattern, compiled here, to pick out WHICH file matched. Stash
	// answers at scene granularity; everything downstream acts on a file.
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("unfiled pattern: %w", err)
	}
	err = pagedQuery(ctx, c, "unfiledScenes", unfiledQuery,
		map[string]any{"value": pattern}, perPage, func(r resp) (int, bool) {
			for _, s := range r.FindScenes.Scenes {
				var matched []string
				for _, f := range s.Files {
					if re.MatchString(f.Path) {
						matched = append(matched, f.Path)
					}
				}
				if len(matched) == 0 {
					// Stash matched the scene on a file it did not return, or
					// its regex disagrees with Go's. Either way there is
					// nothing safe to move, so say nothing rather than guess.
					continue
				}
				out = append(out, UnfiledScene{
					ID: s.ID, Title: s.Title,
					FilePath: matched[0], OtherFiles: len(matched) - 1,
					Performers: s.Performers, StashIDs: s.StashIDs,
				})
			}
			return len(r.FindScenes.Scenes), false
		})
	if err != nil {
		return nil, err
	}
	return out, nil
}
