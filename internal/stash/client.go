package stash

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 60 * time.Second},
	}
}

type gqlError struct {
	Message string `json:"message"`
}

func (c *Client) do(ctx context.Context, query string, vars map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/graphql", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("ApiKey", c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("stash graphql %d: %s", resp.StatusCode, raw)
	}
	var wrap struct {
		Data   json.RawMessage `json:"data"`
		Errors []gqlError      `json:"errors,omitempty"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return fmt.Errorf("decode: %w (body=%s)", err, raw)
	}
	if len(wrap.Errors) > 0 {
		return fmt.Errorf("stash graphql errors: %+v", wrap.Errors)
	}
	if out != nil {
		if err := json.Unmarshal(wrap.Data, out); err != nil {
			return fmt.Errorf("decode data: %w", err)
		}
	}
	return nil
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
}

type Studio struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Aliases  []string  `json:"aliases"`
	StashIDs []StashID `json:"stash_ids"`
}

const performersQuery = `
query ForagerAllPerformers($page: Int!, $perPage: Int!) {
  findPerformers(filter: { page: $page, per_page: $perPage, sort: "id", direction: ASC }) {
    count
    performers { id name alias_list stash_ids { endpoint stash_id } scene_count favorite }
  }
}`

const studiosQuery = `
query ForagerAllStudios($page: Int!, $perPage: Int!) {
  findStudios(filter: { page: $page, per_page: $perPage, sort: "id", direction: ASC }) {
    count
    studios { id name aliases stash_ids { endpoint stash_id } }
  }
}`

// FindPerformers pages through every performer in the user's Stash
// library. Pagination terminates when an empty page is returned, which
// is safer than relying on count for libraries that grow mid-fetch.
func (c *Client) FindPerformers(ctx context.Context) ([]Performer, error) {
	const perPage = 1000
	var all []Performer
	for page := 1; ; page++ {
		var resp struct {
			FindPerformers struct {
				Count      int         `json:"count"`
				Performers []Performer `json:"performers"`
			} `json:"findPerformers"`
		}
		if err := c.do(ctx, performersQuery, map[string]any{"page": page, "perPage": perPage}, &resp); err != nil {
			return nil, fmt.Errorf("findPerformers page %d: %w", page, err)
		}
		if len(resp.FindPerformers.Performers) == 0 {
			break
		}
		all = append(all, resp.FindPerformers.Performers...)
		if len(resp.FindPerformers.Performers) < perPage {
			break
		}
	}
	return all, nil
}

func (c *Client) FindStudios(ctx context.Context) ([]Studio, error) {
	const perPage = 1000
	var all []Studio
	for page := 1; ; page++ {
		var resp struct {
			FindStudios struct {
				Count   int      `json:"count"`
				Studios []Studio `json:"studios"`
			} `json:"findStudios"`
		}
		if err := c.do(ctx, studiosQuery, map[string]any{"page": page, "perPage": perPage}, &resp); err != nil {
			return nil, fmt.Errorf("findStudios page %d: %w", page, err)
		}
		if len(resp.FindStudios.Studios) == 0 {
			break
		}
		all = append(all, resp.FindStudios.Studios...)
		if len(resp.FindStudios.Studios) < perPage {
			break
		}
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
	for page := 1; ; page++ {
		var resp struct {
			FindScenes struct {
				Count  int        `json:"count"`
				Scenes []sceneRow `json:"scenes"`
			} `json:"findScenes"`
		}
		vars := map[string]any{"page": page, "perPage": perPage, "endpoint": endpoint}
		if err := c.do(ctx, labeledScenesQuery, vars, &resp); err != nil {
			return nil, fmt.Errorf("findLabeledScenes page %d: %w", page, err)
		}
		if len(resp.FindScenes.Scenes) == 0 {
			break
		}
		for _, s := range resp.FindScenes.Scenes {
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
				return out, nil
			}
		}
		if len(resp.FindScenes.Scenes) < perPage {
			break
		}
	}
	return out, nil
}

// OwnedScene is what /missing-scenes needs to know about a scene
// already in the user's library: its local ID + StashDB cross-id (so
// we can dedupe against the StashDB scene list for the same
// performer). Used by FindScenesByPerformer.
type OwnedScene struct {
	ID        string
	StashDBID string
}

const findScenesByPerformerQuery = `
query ForagerScenesByPerformer($performerID: ID!, $page: Int!, $perPage: Int!) {
  findScenes(
    scene_filter: { performers: { value: [$performerID], modifier: INCLUDES } }
    filter: { page: $page, per_page: $perPage }
  ) {
    count
    scenes {
      id
      stash_ids { endpoint stash_id }
    }
  }
}`

// FindScenesByPerformer pages through every scene in local Stash that
// features the given LOCAL performer ID and returns each scene's StashDB
// cross-id when present. Used by /missing-scenes to compute the gap
// between "what StashDB has for this performer" and "what's in the
// user's library."
func (c *Client) FindScenesByPerformer(ctx context.Context, localPerformerID string) ([]OwnedScene, error) {
	if localPerformerID == "" {
		return nil, nil
	}
	const perPage = 1000
	var out []OwnedScene
	for page := 1; ; page++ {
		var resp struct {
			FindScenes struct {
				Count  int `json:"count"`
				Scenes []struct {
					ID       string    `json:"id"`
					StashIDs []StashID `json:"stash_ids"`
				} `json:"scenes"`
			} `json:"findScenes"`
		}
		vars := map[string]any{
			"performerID": localPerformerID,
			"page":        page,
			"perPage":     perPage,
		}
		if err := c.do(ctx, findScenesByPerformerQuery, vars, &resp); err != nil {
			return nil, fmt.Errorf("findScenes by performer (page %d): %w", page, err)
		}
		if len(resp.FindScenes.Scenes) == 0 {
			break
		}
		for _, s := range resp.FindScenes.Scenes {
			out = append(out, OwnedScene{
				ID:        s.ID,
				StashDBID: PickStashDBID(s.StashIDs),
			})
		}
		if len(resp.FindScenes.Scenes) < perPage {
			break
		}
	}
	return out, nil
}

const findAllOwnedScenesQuery = `
query ForagerAllOwnedScenes($page: Int!, $perPage: Int!) {
  findScenes(filter: { page: $page, per_page: $perPage }) {
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
// ~900 per-performer queries (each performer would otherwise need
// their own FindScenesByPerformer round-trip).
//
// Scenes without a StashDB cross-id are skipped silently.
func (c *Client) FindAllOwnedStashDBSceneIDs(ctx context.Context) ([]string, error) {
	const perPage = 1000
	var out []string
	for page := 1; ; page++ {
		var resp struct {
			FindScenes struct {
				Count  int `json:"count"`
				Scenes []struct {
					ID       string    `json:"id"`
					StashIDs []StashID `json:"stash_ids"`
				} `json:"scenes"`
			} `json:"findScenes"`
		}
		vars := map[string]any{"page": page, "perPage": perPage}
		if err := c.do(ctx, findAllOwnedScenesQuery, vars, &resp); err != nil {
			return nil, fmt.Errorf("findScenes all (page %d): %w", page, err)
		}
		if len(resp.FindScenes.Scenes) == 0 {
			break
		}
		for _, s := range resp.FindScenes.Scenes {
			if id := PickStashDBID(s.StashIDs); id != "" {
				out = append(out, id)
			}
		}
		if len(resp.FindScenes.Scenes) < perPage {
			break
		}
	}
	return out, nil
}

// SceneMatch is the slim shape Phase B uses to confirm a download.
// FilePath is the first file's path on the Stash side; StashDBID is
// the cross-id we compare against the matcher's prediction.
type SceneMatch struct {
	ID        string
	Title     string
	Date      string
	FilePath  string
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
// any-file path contains the needle substring. Used by the Phase B
// poller to match a completed qBit download against the scene Stash
// has scanned + scraped (which gives us the StashDB cross-id).
//
// Returns nil scene + nil err when no match (poller retries next tick).
func (c *Client) FindSceneByPathContains(ctx context.Context, needle string) (*SceneMatch, error) {
	if needle == "" {
		return nil, nil
	}
	pattern := regexp.QuoteMeta(needle)
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
		return nil, nil
	}
	s := resp.FindScenes.Scenes[0]
	out := &SceneMatch{
		ID:        s.ID,
		Title:     s.Title,
		Date:      s.Date,
		StashDBID: PickStashDBID(s.StashIDs),
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
	pattern := regexp.QuoteMeta(needle)
	const perPage = 1000
	var out []SceneMatch
	for page := 1; ; page++ {
		var resp struct {
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
		vars := map[string]any{"value": pattern, "page": page, "perPage": perPage}
		if err := c.do(ctx, findScenesUnderPathQuery, vars, &resp); err != nil {
			return nil, fmt.Errorf("findScenes under path (page %d): %w", page, err)
		}
		if len(resp.FindScenes.Scenes) == 0 {
			break
		}
		for _, s := range resp.FindScenes.Scenes {
			m := SceneMatch{ID: s.ID, Title: s.Title, Date: s.Date, StashDBID: PickStashDBID(s.StashIDs)}
			if len(s.Files) > 0 {
				m.FilePath = s.Files[0].Path
			}
			out = append(out, m)
		}
		if len(resp.FindScenes.Scenes) < perPage {
			break
		}
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
// FORAGER_STASH_PATH_MAPPING (see config); if no mapping is
// configured we fall back to a full-library scan.
//
// Returns the queued job ID. Non-fatal — if it fails, the poller's
// passive confirmation path still works on Stash's next scheduled
// scan; we just lose the latency win.
func (c *Client) MetadataScan(ctx context.Context, paths []string) (string, error) {
	q := `mutation ForagerMetadataScan($input: ScanMetadataInput!) {
  metadataScan(input: $input)
}`
	input := map[string]any{
		// Ask Stash to fully process new files: cover thumbnail,
		// perceptual hash, seekbar sprite sheet, hover preview. These
		// are all things the user expects to see on the scene card
		// without manually running Generate afterwards. Stash skips
		// any artifact that already exists, so re-scanning is cheap.
		"scanGenerateCovers":   true,
		"scanGeneratePhashes":  true,
		"scanGeneratePreviews": true,
		"scanGenerateSprites":  true,
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
