package stash

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/ordureconnoisseur/forager/internal/clienterr"
	"github.com/ordureconnoisseur/forager/internal/gqlclient"
)

type Client struct {
	gql *gqlclient.Client
}

func New(baseURL, apiKey string) *Client {
	return &Client{gql: gqlclient.New(baseURL, apiKey, "stash")}
}

func (c *Client) do(ctx context.Context, query string, vars map[string]any, out any) error {
	return c.gql.Do(ctx, query, vars, out)
}

// StashID is Stash's cross-reference to an external metadata source
// (StashDB, FansDB, etc.). The endpoint distinguishes them; we filter
// for the StashDB endpoint when populating performer_cache.stashdb_id.
type StashID struct {
	Endpoint string `json:"endpoint"`
	StashID  string `json:"stash_id"`
}

type Performer struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	AliasList  []string  `json:"alias_list"`
	StashIDs   []StashID `json:"stash_ids"`
	SceneCount int       `json:"scene_count"`
	Favorite   bool      `json:"favorite"`
	// Gender is Stash's performer gender enum (MALE, FEMALE,
	// TRANSGENDER_MALE, TRANSGENDER_FEMALE, INTERSEX, NON_BINARY) or
	// empty. Cached so Discover's configurable content filters can
	// select scenes by the genders of their local performers.
	Gender string `json:"gender"`
}

type Studio struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Aliases    []string  `json:"aliases"`
	StashIDs   []StashID `json:"stash_ids"`
	SceneCount int       `json:"scene_count"`
	Favorite   bool      `json:"favorite"`
}

const performersQuery = `
query ForagerAllPerformers($page: Int!, $perPage: Int!) {
  findPerformers(filter: { page: $page, per_page: $perPage, sort: "id", direction: ASC }) {
    count
    performers { id name alias_list stash_ids { endpoint stash_id } scene_count favorite gender }
  }
}`

const studiosQuery = `
query ForagerAllStudios($page: Int!, $perPage: Int!) {
  findStudios(filter: { page: $page, per_page: $perPage, sort: "id", direction: ASC }) {
    count
    studios { id name aliases stash_ids { endpoint stash_id } scene_count favorite }
  }
}`

// pagedQuery drives the offset-pagination skeleton every library sweep
// shares: fetch page 1, 2, ... until a page comes back empty or short.
// Terminating on page shape (not the reported count) is deliberate —
// counts drift on libraries that grow mid-fetch, and every query pins
// an explicit sort so the offset cursor stays stable against inserts.
//
// query must accept $page and $perPage; vars carries the query's other
// variables (nil is fine). onPage processes one decoded response and
// returns how many rows it held, plus stop=true to end early (caller
// limits). label names the query in error wrapping.
func pagedQuery[T any](ctx context.Context, c *Client, label, query string, vars map[string]any, perPage int, onPage func(T) (rows int, stop bool)) error {
	if vars == nil {
		vars = map[string]any{}
	}
	for page := 1; ; page++ {
		vars["page"], vars["perPage"] = page, perPage
		var resp T
		if err := c.do(ctx, query, vars, &resp); err != nil {
			return fmt.Errorf("%s (page %d): %w", label, page, err)
		}
		rows, stop := onPage(resp)
		if stop || rows == 0 || rows < perPage {
			return nil
		}
	}
}

// FindPerformers pages through every performer in the user's Stash
// library.
func (c *Client) FindPerformers(ctx context.Context) ([]Performer, error) {
	type resp struct {
		FindPerformers struct {
			Performers []Performer `json:"performers"`
		} `json:"findPerformers"`
	}
	var all []Performer
	err := pagedQuery(ctx, c, "findPerformers", performersQuery, nil, 1000, func(r resp) (int, bool) {
		all = append(all, r.FindPerformers.Performers...)
		return len(r.FindPerformers.Performers), false
	})
	if err != nil {
		return nil, err
	}
	return all, nil
}

func (c *Client) FindStudios(ctx context.Context) ([]Studio, error) {
	type resp struct {
		FindStudios struct {
			Studios []Studio `json:"studios"`
		} `json:"findStudios"`
	}
	var all []Studio
	err := pagedQuery(ctx, c, "findStudios", studiosQuery, nil, 1000, func(r resp) (int, bool) {
		all = append(all, r.FindStudios.Studios...)
		return len(r.FindStudios.Studios), false
	})
	if err != nil {
		return nil, err
	}
	return all, nil
}

// LabeledScene is the minimal shape the bench rigs need: a scene
// filename, ancestor folder names (nearest-first), ground-truth scene
// date, and the local performer IDs to match against. Only scenes
// with a StashDB cross-id are returned.
//
// Folders contains up to 2 ancestor folder basenames (parent +
// grandparent). Users frequently organise libraries with the
// performer name 1-2 levels up from the file, e.g.
//
//	D:\Porn\Abella Danger\Abella Danger 04\vid\file.mp4
//	                                       ^^^ parent (uninformative)
//	                       ^^^^^^^^^^^^^^^^^^ grandparent (still has performer)
//
// Date is the StashDB-scraped scene date as YYYY-MM-DD, or "" if
// missing. Bench rigs that need ground-truth dates filter on this.
//
// StashDBID is the scene's StashDB cross-id — the ground-truth answer
// the matcher-bench compares against.
//
// StudioStashDBID is the StashDB cross-id of the scene's studio (the
// same key we cache studios under in studio_cache.stashdb_id). May be
// "" if the scene has no studio or no StashDB cross-id for it.
type LabeledScene struct {
	ID              string
	Basename        string
	Folders         []string
	Date            string
	StashDBID       string
	StudioStashDBID string
	PerformerIDs    []string
	// Source labels where a corpus entry came from ("grabs-search",
	// "qbit", "sab", "torrent"); empty for live-library benchmarking. Used
	// only by matcher-bench to report recall per input distribution.
	Source string
}

const labeledScenesQuery = `
query ForagerLabeledScenes($page: Int!, $perPage: Int!, $endpoint: String!) {
  findScenes(
    scene_filter: { stash_id_endpoint: { endpoint: $endpoint, modifier: NOT_NULL } }
    filter: { page: $page, per_page: $perPage, sort: "id", direction: ASC }
  ) {
    count
    scenes {
      id
      date
      stash_ids { endpoint stash_id }
      files { path }
      studio { stash_ids { endpoint stash_id } }
      performers { id }
    }
  }
}`

// splitPathParts returns (ancestor folder basenames nearest-first, file
// basename) for paths that may use either Unix or Windows separators.
// Stash on Windows surfaces backslash-separated paths; Stash on
// Mac/Linux uses forward slash. Doing this manually avoids
// platform-dependent filepath.Base behaviour.
//
// maxFolders caps how many ancestors are returned (parent, grandparent,
// great-grandparent, ...) — typical use is 2.
func splitPathParts(p string, maxFolders int) (folders []string, base string) {
	lastSep := func(s string) int {
		i := -1
		for k, r := range s {
			if r == '/' || r == '\\' {
				i = k
			}
		}
		return i
	}
	i := lastSep(p)
	if i < 0 {
		return nil, p
	}
	base = p[i+1:]
	rest := p[:i]
	for rest != "" && len(folders) < maxFolders {
		j := lastSep(rest)
		if j < 0 {
			folders = append(folders, rest)
			break
		}
		folders = append(folders, rest[j+1:])
		rest = rest[:j]
	}
	return folders, base
}

// FindLabeledScenes pages through scenes that have a StashDB cross-id.
// When limit > 0, fetching stops once that many scenes have been
// collected. limit = 0 means "all of them".
//
// Scenes with no files are skipped. Scenes with multiple files use the
// first file's basename — good enough for release-name benchmarking.
func (c *Client) FindLabeledScenes(ctx context.Context, limit int) ([]LabeledScene, error) {
	const perPage = 200
	endpoint := "https://" + StashDBEndpointHost + "/graphql"
	var out []LabeledScene
	type sceneRow struct {
		ID       string    `json:"id"`
		Date     string    `json:"date"`
		StashIDs []StashID `json:"stash_ids"`
		Files    []struct {
			Path string `json:"path"`
		} `json:"files"`
		Studio *struct {
			StashIDs []StashID `json:"stash_ids"`
		} `json:"studio"`
		Performers []struct {
			ID string `json:"id"`
		} `json:"performers"`
	}
	type resp struct {
		FindScenes struct {
			Scenes []sceneRow `json:"scenes"`
		} `json:"findScenes"`
	}
	err := pagedQuery(ctx, c, "findLabeledScenes", labeledScenesQuery,
		map[string]any{"endpoint": endpoint}, perPage, func(r resp) (int, bool) {
			for _, s := range r.FindScenes.Scenes {
				if len(s.Files) == 0 {
					continue
				}
				folders, base := splitPathParts(s.Files[0].Path, 2)
				if base == "" {
					continue
				}
				ids := make([]string, 0, len(s.Performers))
				for _, p := range s.Performers {
					ids = append(ids, p.ID)
				}
				studioStashDBID := ""
				if s.Studio != nil {
					studioStashDBID = PickStashDBID(s.Studio.StashIDs)
				}
				out = append(out, LabeledScene{
					ID:              s.ID,
					Basename:        base,
					Folders:         folders,
					Date:            s.Date,
					StashDBID:       PickStashDBID(s.StashIDs),
					StudioStashDBID: studioStashDBID,
					PerformerIDs:    ids,
				})
				if limit > 0 && len(out) >= limit {
					return len(r.FindScenes.Scenes), true
				}
			}
			return len(r.FindScenes.Scenes), false
		})
	if err != nil {
		return nil, err
	}
	return out, nil
}

const findAllOwnedScenesQuery = `
query ForagerAllOwnedScenes($page: Int!, $perPage: Int!) {
  findScenes(filter: { page: $page, per_page: $perPage, sort: "id", direction: ASC }) {
    count
    scenes {
      id
      stash_ids { endpoint stash_id }
    }
  }
}`

// FindAllOwnedStashDBSceneIDs sweeps every scene in the local Stash
// library and returns the set of StashDB cross-ids present. Used by
// cache.RefreshSceneCache to mark which recent StashDB scenes the
// user already owns. One sweep replaces what would otherwise be
// ~900 per-performer queries.
//
// Scenes without a StashDB cross-id are skipped silently.
func (c *Client) FindAllOwnedStashDBSceneIDs(ctx context.Context) ([]string, error) {
	type resp struct {
		FindScenes struct {
			Scenes []struct {
				StashIDs []StashID `json:"stash_ids"`
			} `json:"scenes"`
		} `json:"findScenes"`
	}
	var out []string
	err := pagedQuery(ctx, c, "findScenes all", findAllOwnedScenesQuery, nil, 1000, func(r resp) (int, bool) {
		for _, s := range r.FindScenes.Scenes {
			if id := PickStashDBID(s.StashIDs); id != "" {
				out = append(out, id)
			}
		}
		return len(r.FindScenes.Scenes), false
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// countUnidentifiedQuery counts a subject's local scenes that carry NO StashDB
// cross-id — the ones invisible to the StashDB-based owned/missing bars.
const countUnidentifiedQuery = `
query ForagerUnidentified($filter: SceneFilterType!) {
  findScenes(scene_filter: $filter, filter: { per_page: 1 }) { count }
}`

// CountUnidentifiedScenes returns how many local scenes are tagged with the given
// performer OR studio (by LOCAL id) but have no StashDB cross-id — so they don't
// count toward owned and can surface as false "missing" on the gap bar. Pass
// exactly one of performerLocalID / studioLocalID.
func (c *Client) CountUnidentifiedScenes(ctx context.Context, performerLocalID, studioLocalID string) (int, error) {
	endpoint := "https://" + StashDBEndpointHost + "/graphql"
	filter := map[string]any{
		"stash_id_endpoint": map[string]any{"endpoint": endpoint, "modifier": "IS_NULL"},
	}
	if performerLocalID != "" {
		filter["performers"] = map[string]any{"value": []string{performerLocalID}, "modifier": "INCLUDES"}
	}
	if studioLocalID != "" {
		filter["studios"] = map[string]any{"value": []string{studioLocalID}, "modifier": "INCLUDES"}
	}
	var resp struct {
		FindScenes struct {
			Count int `json:"count"`
		} `json:"findScenes"`
	}
	if err := c.do(ctx, countUnidentifiedQuery, map[string]any{"filter": filter}, &resp); err != nil {
		return 0, err
	}
	return resp.FindScenes.Count, nil
}

const findAllScenesWithPathsQuery = `
query ForagerAllScenesWithPaths($page: Int!, $perPage: Int!) {
  findScenes(filter: { page: $page, per_page: $perPage, sort: "id", direction: ASC }) {
    scenes {
      id
      title
      stash_ids { endpoint stash_id }
      files { path size width height }
    }
  }
}`

// SceneRef is a single (scene, file-path) pairing — a scene with N files
// yields N refs sharing the same SceneID. Pack dedup uses these to find
// copies of a scene living outside the pack and, when configured to
// keep the pack copy, to know which existing scenes to remove.
type SceneRef struct {
	SceneID string
	Title   string
	Path    string
	Size    int64 // bytes; 0 if Stash didn't report it
	Width   int   // pixels; 0 if unknown
	Height  int   // pixels; 0 if unknown — the dimension the UI buckets into 1080p/2160p/…
}

// FindAllSceneStashDBIDs sweeps the library and returns a map from each
// scene's StashDB cross-id to the (scene id, file path) refs carrying
// it. Pack dedup uses it to answer "does the library already have this
// scene somewhere outside the pack I just placed?" in a single sweep,
// rather than a per-scene query. Scenes without a StashDB cross-id are
// skipped.
func (c *Client) FindAllSceneStashDBIDs(ctx context.Context) (map[string][]SceneRef, error) {
	type resp struct {
		FindScenes struct {
			Scenes []struct {
				ID       string    `json:"id"`
				Title    string    `json:"title"`
				StashIDs []StashID `json:"stash_ids"`
				Files    []struct {
					Path   string `json:"path"`
					Size   int64  `json:"size"`
					Width  int    `json:"width"`
					Height int    `json:"height"`
				} `json:"files"`
			} `json:"scenes"`
		} `json:"findScenes"`
	}
	out := map[string][]SceneRef{}
	err := pagedQuery(ctx, c, "findScenes paths", findAllScenesWithPathsQuery, nil, 1000, func(r resp) (int, bool) {
		for _, s := range r.FindScenes.Scenes {
			id := PickStashDBID(s.StashIDs)
			if id == "" {
				continue
			}
			for _, f := range s.Files {
				out[id] = append(out[id], SceneRef{SceneID: s.ID, Title: s.Title, Path: f.Path, Size: f.Size, Width: f.Width, Height: f.Height})
			}
		}
		return len(r.FindScenes.Scenes), false
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// findScenesByStashIDQuery returns every local scene carrying a specific
// StashDB cross-id at a given stash-box endpoint.
const findScenesByStashIDQuery = `
query ForagerScenesByStashID($endpoint: String!, $stashId: String!) {
  findScenes(
    scene_filter: { stash_id_endpoint: { endpoint: $endpoint, stash_id: $stashId, modifier: EQUALS } }
    filter: { page: 1, per_page: -1 }
  ) {
    scenes {
      id
      title
      files { path size width height }
    }
  }
}`

// FindSceneRefsByStashID returns the (scene id, file path) refs of every
// local scene whose StashDB cross-id equals stashID at the given
// stash-box endpoint. Pack dedup uses it to locate copies of a pack
// scene that already live elsewhere in the library, querying only the
// handful of cross-ids a pack contains instead of sweeping the whole
// library. FindAllSceneStashDBIDs is the whole-library variant, kept as
// a fallback for when no stash-box endpoint is available to query by.
func (c *Client) FindSceneRefsByStashID(ctx context.Context, endpoint, stashID string) ([]SceneRef, error) {
	if endpoint == "" || stashID == "" {
		return nil, nil
	}
	var resp struct {
		FindScenes struct {
			Scenes []struct {
				ID    string `json:"id"`
				Title string `json:"title"`
				Files []struct {
					Path   string `json:"path"`
					Size   int64  `json:"size"`
					Width  int    `json:"width"`
					Height int    `json:"height"`
				} `json:"files"`
			} `json:"scenes"`
		} `json:"findScenes"`
	}
	vars := map[string]any{"endpoint": endpoint, "stashId": stashID}
	if err := c.do(ctx, findScenesByStashIDQuery, vars, &resp); err != nil {
		return nil, fmt.Errorf("findScenes by stash_id: %w", err)
	}
	var out []SceneRef
	for _, s := range resp.FindScenes.Scenes {
		if len(s.Files) == 0 {
			out = append(out, SceneRef{SceneID: s.ID, Title: s.Title})
			continue
		}
		for _, f := range s.Files {
			out = append(out, SceneRef{SceneID: s.ID, Title: s.Title, Path: f.Path, Size: f.Size, Width: f.Width, Height: f.Height})
		}
	}
	return out, nil
}

// SceneMatch is the slim shape Phase B uses to confirm a download.
// FilePath is the first file's path on the Stash side; StashDBID is
// the cross-id we compare against the matcher's prediction.
type SceneMatch struct {
	ID    string
	Title string
	Date  string
	// FilePath is Files[0] — the scene's FIRST file, which is not
	// necessarily the one whose path matched the lookup. Anything deciding
	// whether to DESTROY this scene must read FileCount, not this.
	FilePath string
	// FileCount is how many files Stash has attached to the scene. One
	// scene can hold several: Stash files a re-download whose fingerprint
	// matches as an extra file on the existing scene rather than as a new
	// scene. sceneDestroy(delete_file) deletes ALL of them, so callers that
	// destroy need to know before they act.
	FileCount int
	// FilePaths is every attached file's path (FileCount == len). The
	// destroy package builds its plans from these, so a deletion preview
	// can show the user the complete list rather than just the first.
	FilePaths []string
	StashDBID string
}

// findScenesByPathQuery uses MATCHES_REGEX instead of INCLUDES.
//
// Stash's `path INCLUDES` filter tokenises the value on whitespace and
// treats `-` as a special "exclude" operator, so any multi-word
// release basename (e.g. `ExCoGiGirls - Livi, Megan - Girls 101 ...`)
// causes the filter to return tens of thousands of unrelated scenes.
// The poller would then pick whichever scene happened to sort first
// by created_at DESC and write its stash_id as the grab's
// actual_stashdb_id — silently mis-confirming every multi-word grab.
//
// MATCHES_REGEX evaluates the value as a plain regex against the file
// path with no tokenisation, so an escaped needle (regexp.QuoteMeta)
// gives us the substring-match semantics we actually want.
const findScenesByPathQuery = `
query ForagerFindSceneByPath($value: String!) {
  findScenes(
    scene_filter: { path: { value: $value, modifier: MATCHES_REGEX } }
    filter: { page: 1, per_page: 5, sort: "created_at", direction: DESC }
  ) {
    count
    scenes {
      id
      title
      date
      stash_ids { endpoint stash_id }
      files { path }
    }
  }
}`

// FindSceneByPathContains finds the most-recently-created scene whose
// any-file path contains the needle as a complete path segment. Used by
// the Phase B poller to match a completed qBit download against the
// scene Stash has scanned + scraped (which gives us the StashDB
// cross-id), and by the purge path to find the scene to destroy.
//
// Returns nil scene + nil err when no match (poller retries next tick).
func (c *Client) FindSceneByPathContains(ctx context.Context, needle string) (*SceneMatch, error) {
	if needle == "" {
		return nil, clienterr.ErrNotFound
	}
	// Anchor the needle as a complete path segment: directly after a
	// separator, running to a separator or end-of-path. An unanchored
	// substring lets a suffix-colliding basename match a DIFFERENT scene
	// ("scene.mp4" is a substring of "myscene.mp4"), and with the
	// newest-first sort that wrong scene wins — this lookup stamps the
	// grab's cross-id on confirm and picks the SceneDestroy target on
	// purge. Segment matching still covers both caller shapes: a placed
	// file's basename and a client job name appearing as a directory
	// component.
	pattern := `[\\/]` + regexp.QuoteMeta(strings.TrimRight(needle, `/\`)) + `([\\/]|$)`
	var resp struct {
		FindScenes struct {
			Count  int `json:"count"`
			Scenes []struct {
				ID       string    `json:"id"`
				Title    string    `json:"title"`
				Date     string    `json:"date"`
				StashIDs []StashID `json:"stash_ids"`
				Files    []struct {
					Path string `json:"path"`
				} `json:"files"`
			} `json:"scenes"`
		} `json:"findScenes"`
	}
	if err := c.do(ctx, findScenesByPathQuery, map[string]any{"value": pattern}, &resp); err != nil {
		return nil, fmt.Errorf("findScenes by path: %w", err)
	}
	if len(resp.FindScenes.Scenes) == 0 {
		return nil, clienterr.ErrNotFound
	}
	s := resp.FindScenes.Scenes[0]
	out := &SceneMatch{
		ID:        s.ID,
		Title:     s.Title,
		Date:      s.Date,
		StashDBID: PickStashDBID(s.StashIDs),
		// The query already asks for every file, so this costs nothing —
		// it was simply being discarded.
		FileCount: len(s.Files),
	}
	for _, f := range s.Files {
		out.FilePaths = append(out.FilePaths, f.Path)
	}
	if len(s.Files) > 0 {
		out.FilePath = s.Files[0].Path
	}
	return out, nil
}

// findScenesUnderPathQuery is the paginated, return-all variant of
// findScenesByPathQuery — used by the pack confirm path to enumerate
// every scene Stash has indexed under a placed pack directory.
const findScenesUnderPathQuery = `
query ForagerFindScenesUnderPath($value: String!, $page: Int!, $perPage: Int!) {
  findScenes(
    scene_filter: { path: { value: $value, modifier: MATCHES_REGEX } }
    filter: { page: $page, per_page: $perPage, sort: "path", direction: ASC }
  ) {
    count
    scenes {
      id
      title
      date
      stash_ids { endpoint stash_id }
      files { path }
    }
  }
}`

// FindScenesUnderPath returns every scene whose any-file path contains
// the needle substring (escaped as a literal). Unlike
// FindSceneByPathContains it paginates and returns the full set — the
// pack confirm path uses it to count how many of a pack's files Stash
// has scanned + identified, and (in dedup) to compare each against the
// existing library.
func (c *Client) FindScenesUnderPath(ctx context.Context, needle string) ([]SceneMatch, error) {
	if needle == "" {
		return nil, nil
	}
	// Anchor to a directory boundary: a pack's files live INSIDE the placed
	// dir, so require a path separator immediately after the needle. This
	// stops the bare substring from matching a sibling that merely shares the
	// name as a prefix (e.g. needle "comatozze" matching "comatozze (2)" or a
	// file "...comatozze....mp4"), which would pull unrelated, cross-id-less
	// scenes into the pack's identify/dedup set. (Trailing separators on the
	// needle are trimmed so the anchor doesn't require two.)
	pattern := regexp.QuoteMeta(strings.TrimRight(needle, `/\`)) + `[\\/]`
	type resp struct {
		FindScenes struct {
			Scenes []struct {
				ID       string    `json:"id"`
				Title    string    `json:"title"`
				Date     string    `json:"date"`
				StashIDs []StashID `json:"stash_ids"`
				Files    []struct {
					Path string `json:"path"`
				} `json:"files"`
			} `json:"scenes"`
		} `json:"findScenes"`
	}
	var out []SceneMatch
	err := pagedQuery(ctx, c, "findScenes under path", findScenesUnderPathQuery,
		map[string]any{"value": pattern}, 1000, func(r resp) (int, bool) {
			for _, s := range r.FindScenes.Scenes {
				m := SceneMatch{ID: s.ID, Title: s.Title, Date: s.Date,
					StashDBID: PickStashDBID(s.StashIDs), FileCount: len(s.Files)}
				for _, f := range s.Files {
					m.FilePaths = append(m.FilePaths, f.Path)
				}
				if len(s.Files) > 0 {
					m.FilePath = s.Files[0].Path
				}
				out = append(out, m)
			}
			return len(r.FindScenes.Scenes), false
		})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ScenePerformer is a performer credited on a scene, with the performer's
// LOCAL Stash id and total scene count — the count drives the pick when a
// scene has several performers (distribute to the one you collect most of).
type ScenePerformer struct {
	ID         string
	Name       string
	SceneCount int
}

// DistScene is a scene under a pack dir plus the data needed to distribute it
// into a performer folder: its file path (Stash's view — Reverse-map to
// forager's), whether it's identified, and its performers.
type DistScene struct {
	ID         string
	FilePath   string
	StashDBID  string
	Performers []ScenePerformer
}

const findScenesWithPerformersUnderPathQuery = `
query ForagerScenesWithPerformers($value: String!, $page: Int!, $perPage: Int!) {
  findScenes(
    scene_filter: { path: { value: $value, modifier: MATCHES_REGEX } }
    filter: { page: $page, per_page: $perPage, sort: "path", direction: ASC }
  ) {
    count
    scenes {
      id
      stash_ids { endpoint stash_id }
      files { path }
      performers { id name scene_count }
    }
  }
}`

// FindScenesWithPerformersUnderPath returns every scene under the needle path,
// each with its performers (+ per-performer scene counts). Used by the pack
// distribute step to hardlink each identified scene into the folder of its
// most-collected performer. Same directory-boundary anchoring as
// FindScenesUnderPath.
func (c *Client) FindScenesWithPerformersUnderPath(ctx context.Context, needle string) ([]DistScene, error) {
	if needle == "" {
		return nil, nil
	}
	pattern := regexp.QuoteMeta(strings.TrimRight(needle, `/\`)) + `[\\/]`
	type resp struct {
		FindScenes struct {
			Scenes []struct {
				ID       string    `json:"id"`
				StashIDs []StashID `json:"stash_ids"`
				Files    []struct {
					Path string `json:"path"`
				} `json:"files"`
				Performers []struct {
					ID         string `json:"id"`
					Name       string `json:"name"`
					SceneCount int    `json:"scene_count"`
				} `json:"performers"`
			} `json:"scenes"`
		} `json:"findScenes"`
	}
	var out []DistScene
	err := pagedQuery(ctx, c, "findScenes with performers under path", findScenesWithPerformersUnderPathQuery,
		map[string]any{"value": pattern}, 1000, func(r resp) (int, bool) {
			for _, s := range r.FindScenes.Scenes {
				d := DistScene{ID: s.ID, StashDBID: PickStashDBID(s.StashIDs)}
				if len(s.Files) > 0 {
					d.FilePath = s.Files[0].Path
				}
				for _, p := range s.Performers {
					d.Performers = append(d.Performers, ScenePerformer{ID: p.ID, Name: p.Name, SceneCount: p.SceneCount})
				}
				out = append(out, d)
			}
			return len(r.FindScenes.Scenes), false
		})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// MetadataScan asks Stash to scan + generate phashes for new files.
// Used by the poller right after a successful placement: shortens
// the placed → confirmed transition from "whenever Stash's scheduled
// scan runs" (hours-to-days) to "minutes".
//
// If paths is empty, Stash scans its full library — slow on a huge
// library but bounded (Stash's scan skips unchanged files cheaply, so
// the cost is dominated by new files needing phash compute, which is
// exactly what we want).
//
// Passing scoped paths is faster but requires the forager-side path
// strings to match what Stash sees on its filesystem — which they
// usually DON'T when forager runs in Docker on Linux and Stash runs
// on Windows/macOS over a NAS mount. The poller handles this via
// FORAGER_STASH_PATH_MAPPING (see config); if a placed path can't be
// mapped the poller skips the scan rather than scanning the whole
// library (see triggerPlacementScan).
//
// Returns the queued job ID. Non-fatal — if it fails, the poller's
// passive confirmation path still works on Stash's next scheduled
// scan; we just lose the latency win.
func (c *Client) MetadataScan(ctx context.Context, paths []string) (string, error) {
	q := `mutation ForagerMetadataScan($input: ScanMetadataInput!) {
  metadataScan(input: $input)
}`
	input := map[string]any{
		// Generate only the cheap, identify-critical artifacts inline: the
		// cover thumbnail (the scene card) and the perceptual hash (what
		// StashDB identify matches on). Deliberately NOT previews or sprites:
		// those are slow (preview clips transcode, sprite sheets render), and
		// Stash runs ONE serial job queue — a slow scan blocks the identify
		// job queued behind it, so forage used to confirm a grab before its
		// identify ever ran, leaving studio scenes unmatched. Keeping the scan
		// fast lets identify land promptly. Previews/sprites come from Stash's
		// scheduled Generate task (or a manual Generate) instead.
		"scanGenerateCovers":  true,
		"scanGeneratePhashes": true,
	}
	if len(paths) > 0 {
		input["paths"] = paths
	}
	var resp struct {
		MetadataScan string `json:"metadataScan"`
	}
	if err := c.do(ctx, q, map[string]any{"input": input}, &resp); err != nil {
		return "", fmt.Errorf("metadataScan: %w", err)
	}
	return resp.MetadataScan, nil
}

// StashBoxes returns the endpoint URLs of the user's configured
// stash-boxes (StashDB et al). The Identify task takes a source
// list that must reference these endpoints exactly — passing our own
// FORAGER_STASHDB_URL would silently no-op if the user's Stash is
// configured with a slightly different host or trailing-slash form.
//
// Returns the list in user-configured order; callers can pick the
// first StashDB-host entry to match what forager's predictions are
// based on.
func (c *Client) StashBoxes(ctx context.Context) ([]string, error) {
	q := `{ configuration { general { stashBoxes { endpoint } } } }`
	var resp struct {
		Configuration struct {
			General struct {
				StashBoxes []struct {
					Endpoint string `json:"endpoint"`
				} `json:"stashBoxes"`
			} `json:"general"`
		} `json:"configuration"`
	}
	if err := c.do(ctx, q, nil, &resp); err != nil {
		return nil, fmt.Errorf("stashBoxes: %w", err)
	}
	out := make([]string, 0, len(resp.Configuration.General.StashBoxes))
	for _, b := range resp.Configuration.General.StashBoxes {
		if b.Endpoint != "" {
			out = append(out, b.Endpoint)
		}
	}
	return out, nil
}

// StashBox is a user's configured stash-box connection (endpoint + key),
// as Stash itself stores it. forage reuses these so the setup wizard can
// offer the StashDB credentials the user already configured in Stash
// instead of making them paste the key a second time.
type StashBox struct {
	Endpoint string `json:"endpoint"`
	Name     string `json:"name"`
	APIKey   string `json:"api_key"`
}

// StashBoxConfigs returns the user's full stash-box configs (endpoint +
// name + api_key). Distinct from StashBoxes() (endpoints-only, used by the
// Identify task) because this carries the secret — only the setup flow
// needs it, to pre-fill the StashDB step.
func (c *Client) StashBoxConfigs(ctx context.Context) ([]StashBox, error) {
	q := `{ configuration { general { stashBoxes { endpoint name api_key } } } }`
	var resp struct {
		Configuration struct {
			General struct {
				StashBoxes []StashBox `json:"stashBoxes"`
			} `json:"general"`
		} `json:"configuration"`
	}
	if err := c.do(ctx, q, nil, &resp); err != nil {
		return nil, fmt.Errorf("stashBoxConfigs: %w", err)
	}
	return resp.Configuration.General.StashBoxes, nil
}

// MetadataIdentify enqueues Stash's Identify task scoped to the given
// scene IDs, sourcing from the supplied stash-box endpoint. Used by
// the poller right after a scene appears in Stash without a StashDB
// cross-id: lets Stash scrape title/performers/tags/cross-id so the
// scene fully populates instead of sitting in the library as a bare
// file with no metadata.
//
// stashBoxEndpoint must match one of the user's configured stash-box
// endpoints (see StashBoxes); a mismatch causes Stash to reject the
// task. Returns the queued job ID. Non-fatal failure mode: identify
// can also be triggered manually by the user from Stash's UI.
func (c *Client) MetadataIdentify(ctx context.Context, sceneIDs []string, stashBoxEndpoint string) (string, error) {
	if len(sceneIDs) == 0 {
		return "", nil
	}
	if stashBoxEndpoint == "" {
		return "", fmt.Errorf("metadataIdentify: stash-box endpoint required")
	}
	q := `mutation ForagerMetadataIdentify($input: IdentifyMetadataInput!) {
  metadataIdentify(input: $input)
}`
	input := map[string]any{
		"sceneIDs": sceneIDs,
		"sources": []map[string]any{
			{
				"source": map[string]any{
					"stash_box_endpoint": stashBoxEndpoint,
				},
			},
		},
	}
	var resp struct {
		MetadataIdentify string `json:"metadataIdentify"`
	}
	if err := c.do(ctx, q, map[string]any{"input": input}, &resp); err != nil {
		return "", fmt.Errorf("metadataIdentify: %w", err)
	}
	return resp.MetadataIdentify, nil
}

// JobStatus returns the status of a queued Stash job (e.g. an Identify or
// Scan task) by its id: one of READY, RUNNING, STOPPING, FINISHED,
// CANCELLED, FAILED. A job that has already drained out of Stash's queue is
// no longer findable — findJob returns null and this returns "" with no
// error. Used to avoid re-queuing an Identify batch while its predecessor is
// still pending or running, which would otherwise stack redundant identical
// jobs behind a backed-up serial queue.
func (c *Client) JobStatus(ctx context.Context, jobID string) (string, error) {
	if jobID == "" {
		return "", nil
	}
	q := `query ForagerFindJob($input: FindJobInput!) {
  findJob(input: $input) {
    status
  }
}`
	var resp struct {
		FindJob *struct {
			Status string `json:"status"`
		} `json:"findJob"`
	}
	if err := c.do(ctx, q, map[string]any{"input": map[string]any{"id": jobID}}, &resp); err != nil {
		return "", fmt.Errorf("findJob: %w", err)
	}
	if resp.FindJob == nil {
		return "", nil
	}
	return resp.FindJob.Status, nil
}

// MetadataGenerate generates the cosmetic artifacts the fast placement scan
// deliberately skips — the hover-preview clips and the seekbar sprite sheet —
// for the given scenes (covers + phashes are already produced by the scan).
// forage fires this AFTER identify has settled a grab, so the slow generation
// never blocks the identify queued behind it in Stash's single serial job
// queue. Stash skips any artifact that already exists, so it's safe to re-run.
// Best-effort; returns the job id.
func (c *Client) MetadataGenerate(ctx context.Context, sceneIDs []string) (string, error) {
	if len(sceneIDs) == 0 {
		return "", nil
	}
	q := `mutation ForagerMetadataGenerate($input: GenerateMetadataInput!) {
  metadataGenerate(input: $input)
}`
	input := map[string]any{
		"sceneIDs": sceneIDs,
		"previews": true,
		"sprites":  true,
	}
	var resp struct {
		MetadataGenerate string `json:"metadataGenerate"`
	}
	if err := c.do(ctx, q, map[string]any{"input": input}, &resp); err != nil {
		return "", fmt.Errorf("metadataGenerate: %w", err)
	}
	return resp.MetadataGenerate, nil
}

// SceneFile is one media file attached to a scene, as SceneFiles reports it.
type SceneFile struct {
	Path string
	Size int64
}

// SceneFilesInfo is a scene resolved by LOCAL id with its complete file
// list — the full blast radius of destroying it. This is what the destroy
// package's plans are built from when the caller starts from a scene id.
type SceneFilesInfo struct {
	ID    string
	Title string
	Files []SceneFile
}

// SceneFiles returns a scene's title and every file Stash has attached, by
// LOCAL scene id. Its whole purpose is to be consulted before a destroy:
// sceneDestroy(delete_file) removes EVERY file on the scene, so a caller
// that means "delete this copy" needs the real list, both to refuse
// multi-file scenes and to show the user exactly what will be removed.
// Returns clienterr.ErrNotFound when the scene is gone.
func (c *Client) SceneFiles(ctx context.Context, id string) (*SceneFilesInfo, error) {
	if id == "" {
		return nil, fmt.Errorf("scene id is empty")
	}
	q := `query ForagerSceneFiles($id: ID!) {
  findScene(id: $id) { id title files { path size } }
}`
	var resp struct {
		FindScene *struct {
			ID    string `json:"id"`
			Title string `json:"title"`
			Files []struct {
				Path string `json:"path"`
				Size int64  `json:"size"`
			} `json:"files"`
		} `json:"findScene"`
	}
	if err := c.do(ctx, q, map[string]any{"id": id}, &resp); err != nil {
		return nil, fmt.Errorf("findScene files: %w", err)
	}
	if resp.FindScene == nil {
		return nil, clienterr.ErrNotFound
	}
	out := &SceneFilesInfo{ID: resp.FindScene.ID, Title: resp.FindScene.Title}
	for _, f := range resp.FindScene.Files {
		out.Files = append(out.Files, SceneFile{Path: f.Path, Size: f.Size})
	}
	return out, nil
}

// SceneFileCount is SceneFiles reduced to the count, kept for callers that
// only gate on it.
func (c *Client) SceneFileCount(ctx context.Context, id string) (int, error) {
	info, err := c.SceneFiles(ctx, id)
	if err != nil {
		return 0, err
	}
	return len(info.Files), nil
}

// SceneDestroy removes a scene from Stash. deleteFile also unlinks the
// underlying media file from disk; deleteGenerated also removes the
// scene's covers/sprites/previews. Used by the grab purge to leave no
// trace of an unwanted download. Safe with a hardlinked library file:
// Stash deletes the library-side path it knows about, and any other
// hardlink (e.g. the download client's copy, if still present) keeps
// the data alive until it too is removed.
func (c *Client) SceneDestroy(ctx context.Context, id string, deleteFile, deleteGenerated bool) error {
	if id == "" {
		return fmt.Errorf("scene id is empty")
	}
	q := `mutation ForagerSceneDestroy($input: SceneDestroyInput!) {
  sceneDestroy(input: $input)
}`
	input := map[string]any{
		"id":               id,
		"delete_file":      deleteFile,
		"delete_generated": deleteGenerated,
	}
	var resp struct {
		SceneDestroy bool `json:"sceneDestroy"`
	}
	if err := c.do(ctx, q, map[string]any{"input": input}, &resp); err != nil {
		return fmt.Errorf("sceneDestroy: %w", err)
	}
	if !resp.SceneDestroy {
		return fmt.Errorf("sceneDestroy returned false for scene %s", id)
	}
	return nil
}

// Version is a trivial query used to validate the URL + API key at
// startup before we kick off a full performer sync.
func (c *Client) Version(ctx context.Context) (string, error) {
	var resp struct {
		Version struct {
			Version string `json:"version"`
		} `json:"version"`
	}
	if err := c.do(ctx, `{ version { version } }`, nil, &resp); err != nil {
		return "", err
	}
	return resp.Version.Version, nil
}

// StashDBEndpointHost is the host portion of a StashDB stash_ids
// endpoint URL. Stash stores the full GraphQL endpoint URL per
// cross-id (e.g. "https://stashdb.org/graphql"); we match on a host
// substring so trailing-slash and `/graphql` variations don't matter.
//
// Performers cross-reference multiple boxes (StashDB, FansDB, PMVStash,
// ThePornDB, JavStash). We only care about the StashDB ID here.
const StashDBEndpointHost = "stashdb.org"

// PickStashDBID returns the StashDB ID from a stash_ids array, or "" if
// the performer has no StashDB cross-reference.
func PickStashDBID(ids []StashID) string {
	for _, id := range ids {
		if strings.Contains(id.Endpoint, StashDBEndpointHost) {
			return id.StashID
		}
	}
	return ""
}

// SceneApply is the set of StashDB-sourced fields to write onto a local
// scene via ApplySceneMetadata. Empty fields are left untouched (so a
// blank StashDB title won't wipe an existing one); StashID+Endpoint set
// the StashDB cross-id; PerformerIDs / StudioID are LOCAL Stash ids the
// caller resolved (anything it couldn't map is simply omitted).
type SceneApply struct {
	StashID      string
	Endpoint     string
	Title        string
	Date         string
	URLs         []string
	PerformerIDs []string
	StudioID     string
	// CoverURL is the StashDB scene's cover image URL. Set as the local
	// scene's cover so a manually-matched scene (no phash → Stash never
	// pulled a cover) gets the right thumbnail. Stash's sceneUpdate
	// accepts a fetchable URL for cover_image.
	CoverURL string
}

// ApplySceneMetadata writes StashDB-sourced metadata onto a local scene
// (sceneUpdate). Used by the manual "match to StashDB" action when phash
// identify couldn't link the file itself (e.g. the scene has no
// fingerprint on StashDB). Only the populated fields are sent.
func (c *Client) ApplySceneMetadata(ctx context.Context, sceneID string, a SceneApply) error {
	if sceneID == "" {
		return fmt.Errorf("scene id is empty")
	}
	input := map[string]any{"id": sceneID}
	if a.StashID != "" && a.Endpoint != "" {
		// sceneUpdate replaces the whole stash_ids list, so merge: keep
		// any cross-ids from OTHER boxes the scene already has (e.g. a
		// ThePornDB id from a TPDB scrape) and add/replace only the
		// StashDB-endpoint entry. Without this, linking to StashDB would
		// wipe an existing ThePornDB id.
		merged := []map[string]any{{"endpoint": a.Endpoint, "stash_id": a.StashID}}
		existing, err := c.sceneStashIDs(ctx, sceneID)
		if err != nil {
			return err
		}
		for _, e := range existing {
			if e.Endpoint != a.Endpoint {
				merged = append(merged, map[string]any{"endpoint": e.Endpoint, "stash_id": e.StashID})
			}
		}
		input["stash_ids"] = merged
	}
	if a.Title != "" {
		input["title"] = a.Title
	}
	if a.Date != "" {
		input["date"] = a.Date
	}
	if len(a.URLs) > 0 {
		input["urls"] = a.URLs
	}
	if len(a.PerformerIDs) > 0 {
		input["performer_ids"] = a.PerformerIDs
	}
	if a.StudioID != "" {
		input["studio_id"] = a.StudioID
	}
	if a.CoverURL != "" {
		// Stash fetches cover_image when given a URL string — the scene
		// had no cover because phash never matched it on StashDB.
		input["cover_image"] = a.CoverURL
	}
	q := `mutation ForagerSceneApply($input: SceneUpdateInput!) {
  sceneUpdate(input: $input) { id }
}`
	var resp struct {
		SceneUpdate struct {
			ID string `json:"id"`
		} `json:"sceneUpdate"`
	}
	if err := c.do(ctx, q, map[string]any{"input": input}, &resp); err != nil {
		return fmt.Errorf("sceneUpdate: %w", err)
	}
	if resp.SceneUpdate.ID == "" {
		return fmt.Errorf("sceneUpdate returned no scene")
	}
	return nil
}

// AddScenePerformer adds one LOCAL Stash performer to the given scenes WITHOUT
// removing any performers they already have, in a single mutation. It uses
// bulkSceneUpdate with performer_ids mode ADD — Stash's additive primitive for
// exactly this. Returns the number of scenes Stash reported updated.
//
// Pass an EXPLICIT scene-id list, never a path/filter: a word-based path filter
// has mass-mistagged the whole library before (see the path-filter footgun).
// No-op on an empty id list.
func (c *Client) AddScenePerformer(ctx context.Context, sceneIDs []string, performerLocalID string) (int, error) {
	if len(sceneIDs) == 0 {
		return 0, nil
	}
	if performerLocalID == "" {
		return 0, fmt.Errorf("performer id is empty")
	}
	input := map[string]any{
		"ids": sceneIDs,
		"performer_ids": map[string]any{
			"ids":  []string{performerLocalID},
			"mode": "ADD",
		},
	}
	q := `mutation ForagerBulkAddPerformer($input: BulkSceneUpdateInput!) {
  bulkSceneUpdate(input: $input) { id }
}`
	var resp struct {
		BulkSceneUpdate []struct {
			ID string `json:"id"`
		} `json:"bulkSceneUpdate"`
	}
	if err := c.do(ctx, q, map[string]any{"input": input}, &resp); err != nil {
		return 0, fmt.Errorf("bulkSceneUpdate: %w", err)
	}
	return len(resp.BulkSceneUpdate), nil
}

// sceneStashIDs returns the cross-ids currently on a scene, so
// ApplySceneMetadata can merge rather than clobber them.
func (c *Client) sceneStashIDs(ctx context.Context, sceneID string) ([]StashID, error) {
	q := `query ForagerSceneStashIDs($id: ID!) {
  findScene(id: $id) { stash_ids { endpoint stash_id } }
}`
	var resp struct {
		FindScene *struct {
			StashIDs []StashID `json:"stash_ids"`
		} `json:"findScene"`
	}
	if err := c.do(ctx, q, map[string]any{"id": sceneID}, &resp); err != nil {
		return nil, fmt.Errorf("findScene stash_ids: %w", err)
	}
	if resp.FindScene == nil {
		return nil, nil
	}
	return resp.FindScene.StashIDs, nil
}

// FindStudioIDByName returns the local Stash studio id whose name equals
// the given name (case-insensitive EQUALS), or "" when none. Used to map
// a StashDB scene's studio to a local studio without creating one.
func (c *Client) FindStudioIDByName(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", nil
	}
	q := `query ForagerFindStudio($name: String!) {
  findStudios(
    studio_filter: { name: { value: $name, modifier: EQUALS } }
    filter: { page: 1, per_page: 1 }
  ) { studios { id } }
}`
	var resp struct {
		FindStudios struct {
			Studios []struct {
				ID string `json:"id"`
			} `json:"studios"`
		} `json:"findStudios"`
	}
	if err := c.do(ctx, q, map[string]any{"name": name}, &resp); err != nil {
		return "", fmt.Errorf("findStudios: %w", err)
	}
	if len(resp.FindStudios.Studios) == 0 {
		return "", nil
	}
	return resp.FindStudios.Studios[0].ID, nil
}

// ── Creating a performer from a StashDB id ──────────────────────────
//
// Discover surfaces trending StashDB scenes whose performers the user may
// not have, and the "+" on such a pill creates that performer locally.
//
// There is no "create from stash-box id" call in Stash, and the obvious
// candidates do not work: scrapeSinglePerformer's performer_id wants a LOCAL
// integer id (it re-scrapes an existing performer, and a UUID there fails
// with strconv.Atoi), and scrapePerformerURL against a stashdb.org performer
// URL returns an internal error. What does work is a stash-box query by
// NAME, whose results each carry remote_site_id — the StashDB uuid. So the
// name is only how candidates are FETCHED; the id is how one is CHOSEN.
// Nothing is created unless the requested id is among them.

// ScrapedPerformer is the subset of Stash's scraped performer fields worth
// carrying into a create. Everything is optional: StashDB rows vary.
type ScrapedPerformer struct {
	Name         string   `json:"name"`
	Disambig     string   `json:"disambiguation"`
	Gender       string   `json:"gender"`
	Birthdate    string   `json:"birthdate"`
	DeathDate    string   `json:"death_date"`
	Ethnicity    string   `json:"ethnicity"`
	Country      string   `json:"country"`
	EyeColor     string   `json:"eye_color"`
	HairColor    string   `json:"hair_color"`
	Height       string   `json:"height"`
	Weight       string   `json:"weight"`
	Measurements string   `json:"measurements"`
	FakeTits     string   `json:"fake_tits"`
	CareerStart  string   `json:"career_start"`
	CareerEnd    string   `json:"career_end"`
	Tattoos      string   `json:"tattoos"`
	Piercings    string   `json:"piercings"`
	Details      string   `json:"details"`
	Aliases      string   `json:"aliases"`
	URLs         []string `json:"urls"`
	Images       []string `json:"images"`
	RemoteSiteID string   `json:"remote_site_id"`
}

// ScrapePerformerByStashDBID asks the stash-box for performers matching name
// and returns the one whose remote_site_id is stashDBID. Nil (no error) when
// the id isn't among the candidates: that is a "cannot prove it is the right
// performer", and creating the wrong one would put a bogus record in the
// user's library.
func (c *Client) ScrapePerformerByStashDBID(ctx context.Context, endpoint, name, stashDBID string) (*ScrapedPerformer, error) {
	if endpoint == "" || name == "" || stashDBID == "" {
		return nil, fmt.Errorf("endpoint, name and stashdb id are all required")
	}
	q := `query($s:ScraperSourceInput!,$i:ScrapeSinglePerformerInput!){
		scrapeSinglePerformer(source:$s,input:$i){
			name disambiguation gender birthdate death_date ethnicity country
			eye_color hair_color height weight measurements fake_tits
			career_start career_end tattoos piercings details aliases
			urls images remote_site_id
		}
	}`
	var resp struct {
		ScrapeSinglePerformer []ScrapedPerformer `json:"scrapeSinglePerformer"`
	}
	vars := map[string]any{
		"s": map[string]any{"stash_box_endpoint": endpoint},
		"i": map[string]any{"query": name},
	}
	if err := c.do(ctx, q, vars, &resp); err != nil {
		return nil, fmt.Errorf("scrape performer: %w", err)
	}
	for i := range resp.ScrapeSinglePerformer {
		if resp.ScrapeSinglePerformer[i].RemoteSiteID == stashDBID {
			return &resp.ScrapeSinglePerformer[i], nil
		}
	}
	return nil, nil
}

// CreatePerformerFromScrape creates a local performer from a scrape,
// stamping the StashDB cross-id so Stash (and forage) can link it
// immediately rather than waiting for an identify to discover it. Returns
// the new local id.
//
// One image only: Stash's create takes a single `image`, and StashDB returns
// a gallery. The first is StashDB's primary.
func (c *Client) CreatePerformerFromScrape(ctx context.Context, endpoint, stashDBID string, p *ScrapedPerformer) (string, error) {
	if p == nil || p.Name == "" {
		return "", fmt.Errorf("nothing to create")
	}
	input := map[string]any{
		"name":      p.Name,
		"stash_ids": []map[string]string{{"endpoint": endpoint, "stash_id": stashDBID}},
	}
	// Only send what the scrape actually had: Stash validates enums and
	// dates, so an empty gender or a blank birthdate is a 422 rather than a
	// no-op.
	for k, v := range map[string]string{
		"disambiguation": p.Disambig,
		"gender":         p.Gender,
		"birthdate":      p.Birthdate,
		"death_date":     p.DeathDate,
		"ethnicity":      p.Ethnicity,
		"country":        p.Country,
		"eye_color":      p.EyeColor,
		"hair_color":     p.HairColor,
		"measurements":   p.Measurements,
		"fake_tits":      p.FakeTits,
		"career_length":  careerLength(p.CareerStart, p.CareerEnd),
		"tattoos":        p.Tattoos,
		"piercings":      p.Piercings,
		"details":        p.Details,
	} {
		if v != "" {
			input[k] = v
		}
	}
	if len(p.Images) > 0 && p.Images[0] != "" {
		input["image"] = p.Images[0]
	}
	if len(p.URLs) > 0 {
		input["urls"] = p.URLs
	}
	if p.Aliases != "" {
		input["alias_list"] = splitAliases(p.Aliases)
	}

	q := `mutation($input:PerformerCreateInput!){ performerCreate(input:$input){ id } }`
	var resp struct {
		PerformerCreate struct {
			ID string `json:"id"`
		} `json:"performerCreate"`
	}
	if err := c.do(ctx, q, map[string]any{"input": input}, &resp); err != nil {
		return "", fmt.Errorf("performerCreate: %w", err)
	}
	if resp.PerformerCreate.ID == "" {
		return "", fmt.Errorf("performerCreate returned no id")
	}
	return resp.PerformerCreate.ID, nil
}

// careerLength renders StashDB's start/end pair as the single free-text
// field Stash stores.
func careerLength(start, end string) string {
	switch {
	case start == "" && end == "":
		return ""
	case end == "":
		return start + "-"
	case start == "":
		return end
	default:
		return start + "-" + end
	}
}

// splitAliases turns StashDB's comma-separated alias blob into the list
// Stash wants, dropping blanks.
func splitAliases(s string) []string {
	var out []string
	for _, a := range strings.Split(s, ",") {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// FindPerformerByStashDBID returns the local id of the performer carrying
// this StashDB cross-id, or "" when Stash has none.
//
// Asked of Stash rather than of performer_cache, which the 12h refresh
// populates and therefore lags a just-created performer by hours. That lag
// made the "already have them?" guard useless in the case that matters most
// — clicking "+" twice — where it fell through to a create and surfaced
// Stash's raw "performer with name X already exists" instead.
func (c *Client) FindPerformerByStashDBID(ctx context.Context, stashDBID string) (string, error) {
	if stashDBID == "" {
		return "", nil
	}
	q := `query($id:String!){
		findPerformers(performer_filter:{stash_id_endpoint:{stash_id:$id, modifier:EQUALS}}){
			performers { id }
		}
	}`
	var resp struct {
		FindPerformers struct {
			Performers []struct {
				ID string `json:"id"`
			} `json:"performers"`
		} `json:"findPerformers"`
	}
	if err := c.do(ctx, q, map[string]any{"id": stashDBID}, &resp); err != nil {
		return "", fmt.Errorf("find performer by stash id: %w", err)
	}
	if len(resp.FindPerformers.Performers) == 0 {
		return "", nil
	}
	return resp.FindPerformers.Performers[0].ID, nil
}

// sceneStashIDsQuery pulls just the cross-ids of scenes labelled against one
// stash-box. Deliberately narrower than labeledScenesQuery (no files, studio or
// performers): the caller only needs to answer "do I have this scene", and the
// heavier projection would page the whole library's file lists to do it.
const sceneStashIDsQuery = `
query ForagerSceneStashIDs($page: Int!, $perPage: Int!, $endpoint: String!) {
  findScenes(
    scene_filter: { stash_id_endpoint: { endpoint: $endpoint, modifier: NOT_NULL } }
    filter: { page: $page, per_page: $perPage, sort: "id", direction: ASC }
  ) {
    count
    scenes { stash_ids { endpoint stash_id } }
  }
}`

// SceneStashIDsForEndpoint returns the stash-box ids of every local scene
// identified against `endpoint`.
//
// Used to mark scenes you already have when browsing a secondary stash-box.
// One paged sweep rather than a lookup per card: Stash's stash-id filter takes
// a single id, so checking a page of results individually would be one round
// trip per scene. The result sets are small in practice (93 and 253 for FansDB
// and JavStash on the reference library) because secondary boxes are lightly
// identified.
//
// A scene can carry ids from several boxes, so entries are filtered to the
// endpoint asked for — returning a StashDB id here would mark the wrong rows.
func (c *Client) SceneStashIDsForEndpoint(ctx context.Context, endpoint string) ([]string, error) {
	if endpoint == "" {
		return nil, nil
	}
	const perPage = 500
	var out []string
	type resp struct {
		FindScenes struct {
			Scenes []struct {
				StashIDs []StashID `json:"stash_ids"`
			} `json:"scenes"`
		} `json:"findScenes"`
	}
	err := pagedQuery(ctx, c, "sceneStashIDs", sceneStashIDsQuery,
		map[string]any{"endpoint": endpoint}, perPage, func(r resp) (int, bool) {
			for _, s := range r.FindScenes.Scenes {
				for _, id := range s.StashIDs {
					if id.Endpoint == endpoint && id.StashID != "" {
						out = append(out, id.StashID)
					}
				}
			}
			return len(r.FindScenes.Scenes), false
		})
	if err != nil {
		return nil, err
	}
	return out, nil
}
