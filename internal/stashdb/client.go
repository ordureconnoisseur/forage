package stashdb

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ordureconnoisseur/forager/internal/clienterr"
	"github.com/ordureconnoisseur/forager/internal/gqlclient"
)

// Client talks to a StashDB endpoint (default https://stashdb.org).
type Client struct {
	gql *gqlclient.Client
}

func New(baseURL, apiKey string) *Client {
	return &Client{gql: gqlclient.New(baseURL, apiKey, "stashdb")}
}

func (c *Client) do(ctx context.Context, query string, vars map[string]any, out any) error {
	return c.gql.Do(ctx, query, vars, out)
}

// Me returns the authenticated user's name. Used as a low-cost auth
// probe at boot: a 401/403 here means the API key is wrong, and we'd
// rather find out before pulling thousands of records.
func (c *Client) Me(ctx context.Context) (string, error) {
	var resp struct {
		Me struct {
			Name string `json:"name"`
		} `json:"me"`
	}
	if err := c.do(ctx, `{ me { name } }`, nil, &resp); err != nil {
		return "", err
	}
	return resp.Me.Name, nil
}

// ── Scene types ──────────────────────────────────────────────────────
//
// The shapes below are the slim projection the matcher cares about,
// not the full StashDB Scene type. Add fields as the matcher's scoring
// layer grows.

type Scene struct {
	ID         string
	Title      string
	Date       string
	Studio     *SceneStudio
	Performers []ScenePerformer
	URLs       []SceneURL
	Images     []SceneImage
	// Tags are the StashDB tag names on the scene (lowercased on the
	// wire side is not done — callers match case-insensitively). Used to
	// filter out noise like compilations / PMVs from the gap analysis.
	Tags []string
	// Updated is StashDB's `updated` timestamp parsed to unix seconds (0 if
	// absent/unparseable). The persistent scene cache's delta sync sorts by
	// UPDATED_AT and stops once it reaches scenes older than its watermark.
	Updated int64
}

// SceneImage is a single image associated with a scene. StashDB
// typically returns one or more — the first is conventionally the
// poster / wide thumbnail (1920×1080 or similar).
type SceneImage struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type SceneStudio struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ScenePerformer struct {
	ID   string `json:"-"`
	Name string `json:"-"`
	As   string `json:"as"`
}

// scenePerformerWire is the GraphQL response shape — the StashDB
// `performers` field on a scene is a list of `ScenePerformerType`
// objects, each wrapping the underlying Performer plus an `as` field
// (the credited stage name on that scene).
type scenePerformerWire struct {
	Performer struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"performer"`
	As string `json:"as"`
}

type SceneURL struct {
	URL  string `json:"url"`
	Type string `json:"-"`
}

type sceneURLWire struct {
	URL  string `json:"url"`
	Site struct {
		Name string `json:"name"`
	} `json:"site"`
}

type sceneWire struct {
	ID         string               `json:"id"`
	Title      string               `json:"title"`
	Date       string               `json:"date"`
	Studio     *SceneStudio         `json:"studio"`
	Performers []scenePerformerWire `json:"performers"`
	URLs       []sceneURLWire       `json:"urls"`
	Images     []SceneImage         `json:"images"`
	Tags       []struct {
		Name string `json:"name"`
	} `json:"tags"`
	Updated string `json:"updated"`
}

func (w sceneWire) toScene() Scene {
	s := Scene{
		ID:     w.ID,
		Title:  w.Title,
		Date:   w.Date,
		Studio: w.Studio,
		Images: w.Images,
	}
	for _, p := range w.Performers {
		s.Performers = append(s.Performers, ScenePerformer{
			ID:   p.Performer.ID,
			Name: p.Performer.Name,
			As:   p.As,
		})
	}
	for _, u := range w.URLs {
		s.URLs = append(s.URLs, SceneURL{URL: u.URL, Type: u.Site.Name})
	}
	for _, t := range w.Tags {
		if t.Name != "" {
			s.Tags = append(s.Tags, t.Name)
		}
	}
	if w.Updated != "" {
		if t, err := time.Parse(time.RFC3339, w.Updated); err == nil {
			s.Updated = t.Unix()
		}
	}
	return s
}

const sceneFields = `
  id
  title
  date
  updated
  studio { id name }
  performers { performer { id name } as }
  urls { url site { name } }
  images { url width height }
  tags { name }
`

// ── searchScenes ─────────────────────────────────────────────────────

// SearchScenes hands a release-name-shaped string to StashDB's
// full-text search and returns the ranked candidates. This is Phase A
// "Track B" — complementary to the structured QueryScenes path.
//
// limit caps the page size (default 25 if ≤ 0). StashDB ranks results
// by relevance internally; we return them in that order.
func (c *Client) SearchScenes(ctx context.Context, term string, limit int) ([]Scene, error) {
	if limit <= 0 {
		limit = 25
	}
	q := `
query ForagerSearchScenes($term: String!, $limit: Int!) {
  searchScenes(term: $term, limit: $limit) {
    count
    scenes {
      ` + sceneFields + `
    }
  }
}`
	var resp struct {
		SearchScenes struct {
			Count  int         `json:"count"`
			Scenes []sceneWire `json:"scenes"`
		} `json:"searchScenes"`
	}
	if err := c.do(ctx, q, map[string]any{"term": term, "limit": limit}, &resp); err != nil {
		return nil, err
	}
	out := make([]Scene, 0, len(resp.SearchScenes.Scenes))
	for _, w := range resp.SearchScenes.Scenes {
		out = append(out, w.toScene())
	}
	return out, nil
}

// ── queryScenes ──────────────────────────────────────────────────────

// SceneQuery filters StashDB's structured scene index. Empty fields
// are omitted from the GraphQL input. PerformerIDs uses INCLUDES_ALL
// semantics (scene must contain *all* listed performers); StudioIDs
// uses INCLUDES (scene's studio is any of the listed). Date is an
// exact-match filter — for ±N day windows the caller should call
// QueryScenes multiple times or omit Date and post-filter.
type SceneQuery struct {
	PerformerIDs []string
	StudioIDs    []string
	Date         string // YYYY-MM-DD; exact match if non-empty
	Page         int
	PerPage      int
	// Sort overrides the default "DATE" sort. Common values: "DATE",
	// "TRENDING", "CREATED_AT". Empty falls through to the default.
	Sort string
}

// QueryScenesResult is what StashDB returns from queryScenes — the
// match count plus the paged scene list.
type QueryScenesResult struct {
	Count  int
	Scenes []Scene
}

const queryScenesGQL = `
query ForagerQueryScenes($input: SceneQueryInput!) {
  queryScenes(input: $input) {
    count
    scenes {
      ` + sceneFields + `
    }
  }
}`

// FindScene returns a single scene by its StashDB UUID, or nil if no
// such scene exists. Used by /scenes/{id}/releases to look up the
// target scene's title + studio + date before searching Prowlarr.
func (c *Client) FindScene(ctx context.Context, id string) (*Scene, error) {
	if id == "" {
		return nil, clienterr.ErrNotFound
	}
	q := `
query ForagerFindScene($id: ID!) {
  findScene(id: $id) {
    ` + sceneFields + `
  }
}`
	var resp struct {
		FindScene *sceneWire `json:"findScene"`
	}
	if err := c.do(ctx, q, map[string]any{"id": id}, &resp); err != nil {
		return nil, err
	}
	if resp.FindScene == nil {
		return nil, clienterr.ErrNotFound
	}
	s := resp.FindScene.toScene()
	return &s, nil
}

// QueryAllScenes loops QueryScenes through every page until the result
// set is exhausted. Use sparingly — a popular performer can have
// hundreds of scenes (≥10 round-trips). hardCap stops the loop early
// for defensive purposes; 0 means no cap.
func (c *Client) QueryAllScenes(ctx context.Context, q SceneQuery, hardCap int) ([]Scene, error) {
	if q.PerPage == 0 {
		q.PerPage = 50
	}
	if q.Page == 0 {
		q.Page = 1
	}
	var all []Scene
	for {
		page := q
		res, err := c.QueryScenes(ctx, page)
		if err != nil {
			return nil, err
		}
		if len(res.Scenes) == 0 {
			break
		}
		all = append(all, res.Scenes...)
		if len(res.Scenes) < q.PerPage {
			break
		}
		if hardCap > 0 && len(all) >= hardCap {
			all = all[:hardCap]
			break
		}
		q.Page++
	}
	return all, nil
}

func (c *Client) QueryScenes(ctx context.Context, q SceneQuery) (*QueryScenesResult, error) {
	if q.PerPage == 0 {
		q.PerPage = 25
	}
	if q.Page == 0 {
		q.Page = 1
	}
	sort := q.Sort
	if sort == "" {
		sort = "DATE"
	}
	input := map[string]any{
		"page":      q.Page,
		"per_page":  q.PerPage,
		"sort":      sort,
		"direction": "DESC",
	}
	if len(q.PerformerIDs) > 0 {
		input["performers"] = map[string]any{
			"value":    q.PerformerIDs,
			"modifier": "INCLUDES_ALL",
		}
	}
	if len(q.StudioIDs) > 0 {
		input["studios"] = map[string]any{
			"value":    q.StudioIDs,
			"modifier": "INCLUDES",
		}
	}
	if q.Date != "" {
		input["date"] = map[string]any{
			"value":    q.Date,
			"modifier": "EQUALS",
		}
	}

	var resp struct {
		QueryScenes struct {
			Count  int         `json:"count"`
			Scenes []sceneWire `json:"scenes"`
		} `json:"queryScenes"`
	}
	if err := c.do(ctx, queryScenesGQL, map[string]any{"input": input}, &resp); err != nil {
		return nil, err
	}
	out := &QueryScenesResult{Count: resp.QueryScenes.Count}
	for _, w := range resp.QueryScenes.Scenes {
		out.Scenes = append(out.Scenes, w.toScene())
	}
	return out, nil
}

// ── queryStudios ─────────────────────────────────────────────────────

// Studio is the slim projection used to enrich the local studio_cache
// with StashDB-side aliases (and the parent studio's name, which is
// effectively another alias — e.g. "LegalPorno" is the parent of
// "American Anal", and release names that say "LegalPorno" should
// match American Anal scenes).
type Studio struct {
	ID         string
	Name       string
	Aliases    []string
	ParentName string
}

const queryStudiosGQL = `
query ForagerQueryStudios($input: StudioQueryInput!) {
  queryStudios(input: $input) {
    count
    studios {
      id
      name
      aliases
      parent { id name }
    }
  }
}`

type studioWire struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Aliases []string `json:"aliases"`
	Parent  *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"parent"`
}

func (w studioWire) toStudio() Studio {
	s := Studio{ID: w.ID, Name: w.Name, Aliases: append([]string(nil), w.Aliases...)}
	if w.Parent != nil {
		s.ParentName = w.Parent.Name
	}
	return s
}

// QueryAllStudios paginates through every StashDB studio and returns
// the slim Studio projection (id, name, aliases, parent name). Used
// once per studio-cache refresh to enrich Stash-side aliases (which
// can be stale or partial) with StashDB's current view + parent name.
//
// StashDB has on the order of low-thousands of studios; per_page=100
// puts the full sweep around 30-50 requests.
func (c *Client) QueryAllStudios(ctx context.Context) ([]Studio, error) {
	page := 1
	perPage := 100
	var out []Studio
	for {
		input := map[string]any{
			"page":      page,
			"per_page":  perPage,
			"sort":      "NAME",
			"direction": "ASC",
		}
		var resp struct {
			QueryStudios struct {
				Count   int          `json:"count"`
				Studios []studioWire `json:"studios"`
			} `json:"queryStudios"`
		}
		if err := c.do(ctx, queryStudiosGQL, map[string]any{"input": input}, &resp); err != nil {
			return nil, err
		}
		if len(resp.QueryStudios.Studios) == 0 {
			break
		}
		for _, w := range resp.QueryStudios.Studios {
			out = append(out, w.toStudio())
		}
		if len(resp.QueryStudios.Studios) < perPage {
			break
		}
		page++
	}
	return out, nil
}

// ── findPerformer (batched) ──────────────────────────────────────────

// Performer is the slim projection used to enrich performer_cache with
// StashDB-side names: the canonical name (which the local Stash record
// may spell differently — "Summer Cline" locally vs "Summer Kline" on
// StashDB) plus StashDB's alias list.
type Performer struct {
	ID      string
	Name    string
	Aliases []string
}

// findPerformerChunk is how many findPerformer lookups we pack into one
// GraphQL request via field aliases. StashDB's queryPerformers has no
// ids filter, so alias-batching single lookups is the only way to bulk
// fetch by id without one round-trip per performer.
const findPerformerChunk = 40

// FindPerformersByID fetches StashDB performers for the given ids,
// keyed by id. Unknown ids are simply absent from the result (StashDB
// returns null for them), not an error.
func (c *Client) FindPerformersByID(ctx context.Context, ids []string) (map[string]Performer, error) {
	out := make(map[string]Performer, len(ids))
	for start := 0; start < len(ids); start += findPerformerChunk {
		end := start + findPerformerChunk
		if end > len(ids) {
			end = len(ids)
		}
		var b strings.Builder
		b.WriteString("query ForagerFindPerformers {\n")
		for i, id := range ids[start:end] {
			fmt.Fprintf(&b, "  p%d: findPerformer(id: %q) { id name aliases }\n", i, id)
		}
		b.WriteString("}")
		var resp map[string]*struct {
			ID      string   `json:"id"`
			Name    string   `json:"name"`
			Aliases []string `json:"aliases"`
		}
		if err := c.do(ctx, b.String(), nil, &resp); err != nil {
			return nil, err
		}
		for _, p := range resp {
			if p == nil || p.ID == "" {
				continue
			}
			out[p.ID] = Performer{ID: p.ID, Name: p.Name, Aliases: p.Aliases}
		}
	}
	return out, nil
}
