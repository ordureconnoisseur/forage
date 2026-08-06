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
	gql   *gqlclient.Client
	pacer *pacer
}

func New(baseURL, apiKey string) *Client {
	return &Client{gql: gqlclient.New(baseURL, apiKey, "stashdb"), pacer: newPacer()}
}

// NewUnpaced is New without the request budget — for tests driving a
// local fake where pacing only slows the suite. Production callers use
// New: the budget exists to protect the real, shared stashdb.org.
func NewUnpaced(baseURL, apiKey string) *Client {
	c := New(baseURL, apiKey)
	c.pacer.rate = 1e9
	c.pacer.burst = 1e9
	c.pacer.tokens = 1e9
	return c
}

// do funnels every StashDB query through the request budget (see pacer):
// wait for a slot, run, feed the outcome back for the back-off. A context
// that dies in the queue is classified transient like any other timeout —
// a caller must retry later, never conclude anything terminal from it.
func (c *Client) do(ctx context.Context, query string, vars map[string]any, out any) error {
	if err := c.pacer.wait(ctx); err != nil {
		return clienterr.Transport("stashdb budget", err)
	}
	err := c.gql.Do(ctx, query, vars, out)
	c.pacer.observe(err)
	return err
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
	// absent/unparseable). Stored on the persistent scene cache row.
	Updated int64
	// Fingerprints are the file hashes the box holds for this scene. Only
	// populated when SceneQuery.WithFingerprints asks for them, because a
	// popular scene can carry ninety of these and the cached StashDB feed
	// has no use for any of them.
	Fingerprints []Fingerprint
}

// Fingerprint is one file hash a stash-box holds for a scene. The hash is
// computed from the file's content, so unlike a scene id it means the same
// thing on every box, which makes it the only reliable way to ask "is this
// FansDB scene the same release StashDB calls something else".
type Fingerprint struct {
	Hash      string `json:"hash"`
	Algorithm string `json:"algorithm"`
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
	// Gender as the box reports it. Only the live secondary-box path reads
	// this; the StashDB feed resolves gender from performer_cache and the
	// backfill table, because its scenes are served from cache rows written
	// before this field existed.
	Gender string `json:"-"`
}

// scenePerformerWire is the GraphQL response shape — the StashDB
// `performers` field on a scene is a list of `ScenePerformerType`
// objects, each wrapping the underlying Performer plus an `as` field
// (the credited stage name on that scene).
type scenePerformerWire struct {
	Performer struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Gender string `json:"gender"`
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
	Updated      string        `json:"updated"`
	Fingerprints []Fingerprint `json:"fingerprints"`
}

func (w sceneWire) toScene() Scene {
	s := Scene{
		ID:           w.ID,
		Title:        w.Title,
		Date:         w.Date,
		Studio:       w.Studio,
		Images:       w.Images,
		Fingerprints: w.Fingerprints,
	}
	for _, p := range w.Performers {
		s.Performers = append(s.Performers, ScenePerformer{
			ID:     p.Performer.ID,
			Name:   p.Performer.Name,
			As:     p.As,
			Gender: p.Performer.Gender,
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
  performers { performer { id name gender } as }
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
	// WithFingerprints asks for each scene's file hashes as well. Off by
	// default: a scene can carry ninety of them, and the only caller that
	// wants them is the secondary-box feed, which uses them to recognise a
	// release the library already holds under a StashDB id.
	WithFingerprints bool
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

// Same query with the file hashes attached. A separate document rather than a
// flag inside sceneFields because sceneFields also feeds the cached StashDB
// paths, and every row they write would grow by hashes nothing reads.
const queryScenesWithFingerprintsGQL = `
query ForagerQueryScenesFP($input: SceneQueryInput!) {
  queryScenes(input: $input) {
    count
    scenes {
      ` + sceneFields + `
      fingerprints { hash algorithm }
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

// querySceneIDsGQL is the minimal projection used for COUNTING: just the total
// `count` and each scene's id + date. ~10x smaller payload than the full scene
// fragment — enough to compute a subject's total + owned (intersect ids with the
// owned set) + last-release, without downloading bodies.
const querySceneIDsGQL = `
query ForagerSceneIDs($input: SceneQueryInput!) {
  queryScenes(input: $input) {
    count
    scenes { id date }
  }
}`

// SceneCount is the lightweight count result for one subject: the StashDB total,
// the scene ids (to intersect with the owned set), and the newest release date.
type SceneCount struct {
	Total           int
	IDs             []string
	LastReleaseUnix int64
}

// QuerySceneIDs paginates a subject's scenes pulling ONLY id + date (and the
// total `count`). For the lazy cache's count pass: total + owned (caller
// intersects IDs with the owned set) + last release, without fetching bodies.
func (c *Client) QuerySceneIDs(ctx context.Context, q SceneQuery, hardCap int) (SceneCount, error) {
	if q.PerPage == 0 {
		q.PerPage = 100
	}
	if q.Page == 0 {
		q.Page = 1
	}
	sort := q.Sort
	if sort == "" {
		sort = "DATE"
	}
	var out SceneCount
	for {
		input := map[string]any{"page": q.Page, "per_page": q.PerPage, "sort": sort, "direction": "DESC"}
		if len(q.PerformerIDs) > 0 {
			input["performers"] = map[string]any{"value": q.PerformerIDs, "modifier": "INCLUDES_ALL"}
		}
		if len(q.StudioIDs) > 0 {
			input["studios"] = map[string]any{"value": q.StudioIDs, "modifier": "INCLUDES"}
		}
		var resp struct {
			QueryScenes struct {
				Count  int `json:"count"`
				Scenes []struct {
					ID   string `json:"id"`
					Date string `json:"date"`
				} `json:"scenes"`
			} `json:"queryScenes"`
		}
		if err := c.do(ctx, querySceneIDsGQL, map[string]any{"input": input}, &resp); err != nil {
			return SceneCount{}, err
		}
		out.Total = resp.QueryScenes.Count
		if len(resp.QueryScenes.Scenes) == 0 {
			break
		}
		for _, s := range resp.QueryScenes.Scenes {
			out.IDs = append(out.IDs, s.ID)
			if t, err := time.Parse("2006-01-02", s.Date); err == nil && t.Unix() > out.LastReleaseUnix {
				out.LastReleaseUnix = t.Unix()
			}
		}
		if len(resp.QueryScenes.Scenes) < q.PerPage {
			break
		}
		if hardCap > 0 && len(out.IDs) >= hardCap {
			break
		}
		q.Page++
	}
	return out, nil
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
	doc := queryScenesGQL
	if q.WithFingerprints {
		doc = queryScenesWithFingerprintsGQL
	}
	if err := c.do(ctx, doc, map[string]any{"input": input}, &resp); err != nil {
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

// ── queryPerformers ──────────────────────────────────────────────────

// PerformerProfile is the wider projection used to browse performers, as
// opposed to Performer above, which exists only to resolve names for the
// matcher and carries nothing a card would show.
type PerformerProfile struct {
	ID             string
	Name           string
	Disambiguation string
	Gender         string
	SceneCount     int
	// ImageURL is the tallest portrait StashDB holds, or "" when it holds
	// none. Portraits and square avatars are mixed together in `images` with
	// no flag distinguishing them, so the choice is made by shape.
	ImageURL string
}

// PerformerQuery is one page of queryPerformers.
//
// Sort is the interesting part. StashDB has no TRENDING sort for performers
// (the enum offers it for scenes only), and POPULARITY is career volume: it
// answers with the men who have four thousand scenes, every time, forever.
// The two that carry any sense of "now" are DEBUT, whose first scene has just
// landed, and LAST_SCENE, who released most recently.
type PerformerQuery struct {
	Page    int
	PerPage int
	// Sort is a PerformerSortEnum value: "DEBUT", "LAST_SCENE", "POPULARITY",
	// "SCENE_COUNT", "NAME"… Empty falls through to "LAST_SCENE".
	Sort string
	// Gender filters server-side ("FEMALE", "MALE", …). Empty means any.
	Gender string
}

type QueryPerformersResult struct {
	Count      int
	Performers []PerformerProfile
}

const queryPerformersGQL = `
query ForagerQueryPerformers($input: PerformerQueryInput!) {
  queryPerformers(input: $input) {
    count
    performers {
      id
      name
      disambiguation
      gender
      scene_count
      images { url width height }
    }
  }
}`

type performerWire struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Disambiguation string `json:"disambiguation"`
	Gender         string `json:"gender"`
	SceneCount     int    `json:"scene_count"`
	Images         []struct {
		URL    string `json:"url"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	} `json:"images"`
}

func (w performerWire) toProfile() PerformerProfile {
	p := PerformerProfile{
		ID:             w.ID,
		Name:           w.Name,
		Disambiguation: w.Disambiguation,
		Gender:         w.Gender,
		SceneCount:     w.SceneCount,
	}
	// Prefer the tallest portrait. A performer's images are a mixed bag:
	// 1440x2160 publicity shots next to 400x400 avatars, in no stated order,
	// with nothing marking which is which. Taking images[0] puts a square
	// avatar in a 3:4 card about a third of the time.
	best := -1
	for _, im := range w.Images {
		if im.URL == "" {
			continue
		}
		score := im.Height
		if im.Width > 0 && im.Height > im.Width {
			score += im.Height // portraits outrank squares of the same height
		}
		if score > best {
			best, p.ImageURL = score, im.URL
		}
	}
	return p
}

// QueryPerformers runs one page of queryPerformers.
func (c *Client) QueryPerformers(ctx context.Context, q PerformerQuery) (*QueryPerformersResult, error) {
	if q.PerPage == 0 {
		q.PerPage = 25
	}
	if q.Page == 0 {
		q.Page = 1
	}
	sort := q.Sort
	if sort == "" {
		sort = "LAST_SCENE"
	}
	input := map[string]any{
		"page":      q.Page,
		"per_page":  q.PerPage,
		"sort":      sort,
		"direction": "DESC",
	}
	if q.Gender != "" {
		input["gender"] = q.Gender
	}
	var resp struct {
		QueryPerformers struct {
			Count      int             `json:"count"`
			Performers []performerWire `json:"performers"`
		} `json:"queryPerformers"`
	}
	if err := c.do(ctx, queryPerformersGQL, map[string]any{"input": input}, &resp); err != nil {
		return nil, err
	}
	out := &QueryPerformersResult{Count: resp.QueryPerformers.Count}
	for _, w := range resp.QueryPerformers.Performers {
		out.Performers = append(out.Performers, w.toProfile())
	}
	return out, nil
}

// FindPerformerProfilesByID fetches the card-shaped projection for a set of
// ids, alias-batched for the same reason FindPerformersByID is: queryPerformers
// has no ids filter, so a bulk fetch by id is a pile of findPerformer calls
// packed into one document.
func (c *Client) FindPerformerProfilesByID(ctx context.Context, ids []string) (map[string]PerformerProfile, error) {
	out := make(map[string]PerformerProfile, len(ids))
	for start := 0; start < len(ids); start += findPerformerChunk {
		end := start + findPerformerChunk
		if end > len(ids) {
			end = len(ids)
		}
		var b strings.Builder
		b.WriteString("query ForagerFindPerformerProfiles {\n")
		for i, id := range ids[start:end] {
			fmt.Fprintf(&b, "  p%d: findPerformer(id: %q) { id name disambiguation"+
				" gender scene_count images { url width height } }\n", i, id)
		}
		b.WriteString("}")
		var resp map[string]*performerWire
		if err := c.do(ctx, b.String(), nil, &resp); err != nil {
			return out, err
		}
		for _, p := range resp {
			if p == nil || p.ID == "" {
				continue
			}
			out[p.ID] = p.toProfile()
		}
	}
	return out, nil
}
