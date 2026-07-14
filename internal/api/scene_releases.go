package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/ordureconnoisseur/forager/internal/clienterr"
	"github.com/ordureconnoisseur/forager/internal/matcher"
	"github.com/ordureconnoisseur/forager/internal/prowlarr"
	"github.com/ordureconnoisseur/forager/internal/scoring"
	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

// packRe flags a release that is a multi-scene PACK / collection rather than a
// single scene — e.g. a performer "MegaPACK", a "Performer Pack", a
// "Collection", or a "50 scenes" bundle. Such a release can share a performer
// name with a single-scene watch and so verify on title overlap (the performer
// name appears in both titles), but grabbing it pulls tens of GB of the wrong
// thing. SiteRip/compilation are deliberately NOT here — a single scene is
// often released as a SiteRip, and a compilation can be a legitimate StashDB
// scene in its own right.
var packRe = regexp.MustCompile(`(?i)(mega[\s._-]?pack|\bpack\b|\bcollection\b|\b\d{2,}\s?(scene|video|clip)s\b)`)

// isPackRelease reports whether a release title looks like a multi-scene pack.
func isPackRelease(title string) bool { return packRe.MatchString(title) }

// linkSpamRe flags a release whose title advertises a streaming / file-locker
// link or a "watch full video" CTA. These come from tube-aggregator indexers
// (e.g. MyPornClub) and are SEO/affiliate spam: the torrent itself is a short
// teaser or a crushed low-bitrate rip, and the title shoves a stream host to
// drive traffic there. A real scene/p2p release title never contains a URL or
// "watch full video", so this is a near-zero-false-positive junk signal. We
// match an explicit URL, the CTA phrasing, and the common file-locker hosts
// (which sometimes appear without a scheme).
var linkSpamRe = regexp.MustCompile(`(?i)(https?://|watch[\s._-]+full[\s._-]+video|\b(lulustream|luluvid|streamtape|mixdrop|doodstream|dood\.|filemoon|streamlare|vidoza|streamsb|earnvids|vidguard|streamwish)\b)`)

// isLinkSpamRelease reports whether a release title is streaming-link spam
// rather than a real downloadable scene.
func isLinkSpamRelease(title string) bool { return linkSpamRe.MatchString(title) }

// imageSetRe flags a photo/image set rather than a video. Studios release the
// same scene's stills as a separate "iMAGESET" / "photoset" pack that shares
// the performer + date, so it verifies against a scene watch on that overlap —
// but a watch tracks the VIDEO, and grabbing the gallery is never what the user
// wants. Requires the literal "...set" token so a stray "pic"/"image" in a real
// title can't trip it.
var imageSetRe = regexp.MustCompile(`(?i)\b(image|photo|pic)[\s._-]?sets?\b`)

// isImageSetRelease reports whether a release is a photo set, not a video.
func isImageSetRelease(title string) bool { return imageSetRe.MatchString(title) }

type sceneReleasesResponse struct {
	Scene struct {
		StashDBID  string             `json:"stashdb_id"`
		Title      string             `json:"title"`
		Date       string             `json:"date,omitempty"`
		Studio     string             `json:"studio,omitempty"`
		ImageURL   string             `json:"image_url,omitempty"`
		Performers []missingPerformer `json:"performers"`
	} `json:"scene"`
	Releases []sceneRelease `json:"releases"`
}

type sceneRelease struct {
	Title       string  `json:"title"`
	Indexer     string  `json:"indexer"`
	Protocol    string  `json:"protocol"`
	Size        int64   `json:"size"`
	Popularity  int     `json:"popularity"`
	Seeders     int     `json:"seeders"`
	Grabs       int     `json:"grabs"`
	PublishDate string  `json:"publish_date"`
	InfoURL     string  `json:"info_url"`
	DownloadURL string  `json:"download_url"`
	Verified    bool    `json:"verified"` // matcher confirms this release is the target scene
	Confidence  float64 `json:"confidence"`
	// Failure history for THIS exact release (title+indexer+size): how
	// many grabs of it died for a content-side reason, and the last
	// reason. The UI badges these so a release that already died 8
	// times reads as dead before the 9th Grab click.
	FailedCount  int    `json:"failed_count,omitempty"`
	FailedReason string `json:"failed_reason,omitempty"`
	// When a release is NOT verified because the matcher thinks it's a
	// different scene, these name that scene so the UI can warn the
	// user ("this looks like X, not the scene you're viewing").
	BestMatchID    string  `json:"best_match_id,omitempty"`
	BestMatchTitle string  `json:"best_match_title,omitempty"`
	BestMatchConf  float64 `json:"best_match_conf,omitempty"`
	// Reasons is the matcher's per-component breakdown for the viewed
	// scene against this release (performers/studio/date/title/tracks) —
	// the same strings the matcher scores on. Drives the "why did this
	// match?" expander. Empty when the viewed scene wasn't a candidate.
	Reasons []string `json:"reasons,omitempty"`
	// Score is the user's release-preference score for this release (sum
	// of matched rules); Rejected is true when a reject rule matched (the
	// release is hidden from auto-selection). ScoreHits is the per-rule
	// breakdown for the "why this score?" display.
	Score     int           `json:"score"`
	Rejected  bool          `json:"rejected,omitempty"`
	ScoreHits []scoring.Hit `json:"score_hits,omitempty"`
}

// getSceneReleases finds Prowlarr releases for a specific StashDB
// scene. This is the scene-targeted lookup the Stash plugin uses when
// the user clicks a missing-scene card.
//
//	GET /scenes/{stashdb_id}/releases?min_seeders=N
//
// Flow:
//  1. Look up the scene's metadata (title, performers, studio, date).
//  2. Query Prowlarr targeted on the scene title — this is what Phase
//     3 of /search already does well, just promoted to the headline
//     query rather than a refinement.
//  3. Run the matcher in verification mode: a release is `verified`
//     when its top candidate's scene_id matches the target.
//  4. Return releases ranked verified-first, then popularity-desc.
func (s *Server) getSceneReleases(w http.ResponseWriter, r *http.Request) {
	prowlarrC := s.pool.Prowlarr()
	stashDBC := s.pool.StashDB()
	if prowlarrC == nil || stashDBC == nil {
		writeErr(w, http.StatusServiceUnavailable, "prowlarr and stashdb must be configured (see Settings)")
		return
	}
	prowlarrCats := s.pool.Settings().ProwlarrCategories
	id := chi.URLParam(r, "id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "scene id required")
		return
	}
	// Optional context: the performer whose page the user came from, and
	// an explicit alias override to retry under a specific spelling.
	ctxPerformer := strings.TrimSpace(r.URL.Query().Get("performer"))
	aliasOverride := strings.TrimSpace(r.URL.Query().Get("alias"))
	// lean mode: the collection fan-out runs this endpoint for EVERY
	// missing scene concurrently, so the full multi-spelling × studio/year
	// query set (6+ per scene) overwhelms Prowlarr/the trackers — context
	// deadlines, mass "search failed". In lean mode we emit far fewer
	// queries per scene (the collection already covers the performer
	// broadly across all their scenes, so per-scene breadth is wasteful).
	lean := r.URL.Query().Get("lean") == "1"

	scene, err := stashDBC.FindScene(r.Context(), id)
	if err != nil && !errors.Is(err, clienterr.ErrNotFound) {
		s.log.Error("findScene", "err", err)
		writeErr(w, http.StatusBadGateway, "stashdb: "+err.Error())
		return
	}
	if scene == nil {
		writeErr(w, http.StatusNotFound, "scene not found on stashdb")
		return
	}
	if scene.Title == "" {
		writeErr(w, http.StatusUnprocessableEntity, "scene has no title to search by")
		return
	}

	m, err := s.Matcher(r.Context())
	if err != nil {
		s.log.Error("matcher init", "err", err)
		writeErr(w, http.StatusServiceUnavailable, "matcher unavailable: "+err.Error())
		return
	}

	// Build the performer query names, scoped to the ONE performer the
	// user is browsing (not every performer on the scene — that dragged in
	// the male lead and missed the female-named release). We try that
	// performer's library name AND their StashDB spellings (canonical +
	// scene-credited "as"), because a tracker may index under any of them
	// — e.g. owned as "Summer Cline" but released as "Summer Kline". An
	// explicit alias override (manual retry) takes precedence. Falls back
	// to the scene's own performers when there's no context performer.
	perfNames := s.scenePerformerNames(r.Context(), scene, ctxPerformer, aliasOverride)

	// Fan out several scene-derived Prowlarr queries, not just the title.
	// Trackers (esp. PornoLab) name a release by studio + performer, NOT
	// the StashDB marketing title, so a title-only query returns nothing
	// for those. The matcher then verifies which hits are this scene, so
	// over-broad queries are harmless. Queries run concurrently; results
	// merge + dedup by grab URL.
	releases, err := s.searchSceneReleases(r.Context(), prowlarrC, scene, perfNames, prowlarrCats, lean)
	if err != nil {
		s.log.Error("prowlarr search", "err", err, "scene", scene.Title)
		writeErr(w, http.StatusBadGateway, "prowlarr: "+err.Error())
		return
	}

	// Verify which releases are this scene + shape them for the UI.
	out := s.verifyReleases(r.Context(), m, id, scene.Title, releases)

	// Annotate failure history before ranking so the UI can badge dead
	// releases (ranking itself is deliberately untouched: a dead badge
	// informs; silently re-ordering would hide why).
	dead := s.contentDeadReleases(r.Context())
	for i := range out {
		if e, ok := dead[deadReleaseKey(out[i].Title, out[i].Indexer, out[i].Size)]; ok {
			out[i].FailedCount = e.Count
			out[i].FailedReason = e.LastReason
		}
	}

	// Rank: verified-first, then non-rejected, then the canonical betterRelease
	// comparator (resolution by score, then match confidence within a tier, then
	// availability/size, indexer/protocol nudge last). Sharing betterRelease
	// keeps this list's leading release identical to the watcher's auto-pick.
	rankReleases(out)

	resp := sceneReleasesResponse{Releases: out}
	resp.Scene.StashDBID = scene.ID
	resp.Scene.Title = scene.Title
	resp.Scene.Date = scene.Date
	if scene.Studio != nil {
		resp.Scene.Studio = scene.Studio.Name
	}
	if len(scene.Images) > 0 {
		resp.Scene.ImageURL = scene.Images[0].URL
	}
	for _, p := range scene.Performers {
		resp.Scene.Performers = append(resp.Scene.Performers, missingPerformer{
			Name:    p.Name,
			As:      p.As,
			Aliases: s.performerAliases(r.Context(), p.Name),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// performerAliases returns alternate spellings for a performer from the
// user's local Stash record (performer_cache.aliases), matched by name.
// These power the scene-release alias-retry chips — a tracker may have
// indexed the release under any of them. The performer's own name is
// excluded (it's already the primary). Best-effort: nil on miss/error.
func (s *Server) performerAliases(ctx context.Context, name string) []string {
	if name == "" {
		return nil
	}
	var aliasesJSON sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT aliases FROM performer_cache WHERE name = ? COLLATE NOCASE LIMIT 1`,
		name).Scan(&aliasesJSON)
	if err != nil || !aliasesJSON.Valid || aliasesJSON.String == "" {
		return nil
	}
	var aliases []string
	if json.Unmarshal([]byte(aliasesJSON.String), &aliases) != nil {
		return nil
	}
	out := make([]string, 0, len(aliases))
	for _, a := range aliases {
		a = strings.TrimSpace(a)
		if a != "" && !strings.EqualFold(a, name) {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// maxPerformerNames caps how many spellings of the browsed performer we
// query, so we don't explode the fan-out chasing every alias.
const maxPerformerNames = 3

// scenePerformerNames returns the spellings to query for, scoped to the
// performer the user is browsing. Priority:
//  1. an explicit alias override (manual retry) — used alone;
//  2. else the browsed performer's library name + their StashDB
//     canonical/credited spellings (a tracker may use any of them);
//  3. else (no context performer) fall back to the scene's own
//     performers, capped.
//
// Capped at maxPerformerNames.
func (s *Server) scenePerformerNames(ctx context.Context, scene *stashdb.Scene, ctxPerformer, aliasOverride string) []string {
	if aliasOverride != "" {
		return []string{aliasOverride}
	}

	seen := map[string]bool{}
	var out []string
	add := func(n string) {
		n = strings.TrimSpace(n)
		if n == "" || seen[strings.ToLower(n)] || len(out) >= maxPerformerNames {
			return
		}
		seen[strings.ToLower(n)] = true
		out = append(out, n)
	}

	if ctxPerformer != "" {
		// The library name first (what the user calls them).
		add(ctxPerformer)
		// Then this performer's StashDB spellings on THIS scene — match by
		// the owned performer's cross-id so we pick the right person, not
		// every performer on the scene.
		if sid, _ := s.performerStashDBIDByName(ctx, ctxPerformer); sid != "" {
			for _, p := range scene.Performers {
				if p.ID == sid {
					add(p.Name) // StashDB canonical
					add(p.As)   // scene-credited spelling
				}
			}
		}
		return out
	}

	// No context performer (e.g. opened from Discover without one): fall
	// back to the scene's listed performers.
	for _, p := range scene.Performers {
		add(p.Name)
	}
	return out
}

// performerStashDBIDByName resolves a local performer's StashDB cross-id
// from performer_cache by display name (case-insensitive). "" if unknown.
func (s *Server) performerStashDBIDByName(ctx context.Context, name string) (string, error) {
	var sid sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT stashdb_id FROM performer_cache WHERE name = ? COLLATE NOCASE AND stashdb_id != '' LIMIT 1`,
		name).Scan(&sid)
	if err != nil {
		return "", err
	}
	return sid.String, nil
}

// sceneSearchTerms derives the Prowlarr query strings from a scene plus
// the chosen performer name spellings. Per (performer × studio) and
// (performer × year), plus the title and each bare performer name.
// Deduped, blanks dropped.
//
// lean trims to the two most-productive terms (primary performer × studio,
// and the title) — for the collection fan-out, which runs this for every
// missing scene concurrently and can't afford 6 queries each.
func sceneSearchTerms(scene *stashdb.Scene, perfNames []string, lean bool) []string {
	studio := ""
	if scene.Studio != nil {
		studio = scene.Studio.Name
	}
	year := ""
	if len(scene.Date) >= 4 {
		year = scene.Date[:4]
	}

	var candidates []string
	if lean {
		// Just enough to find the scene without flooding Prowlarr: the
		// primary performer + studio (the most productive single query),
		// and the title.
		if len(perfNames) > 0 && studio != "" {
			candidates = append(candidates, perfNames[0]+" "+studio)
		} else if len(perfNames) > 0 {
			candidates = append(candidates, perfNames[0])
		}
		candidates = append(candidates, scene.Title)
	} else {
		for _, p := range perfNames {
			if studio != "" {
				candidates = append(candidates, p+" "+studio)
			}
			if year != "" {
				candidates = append(candidates, p+" "+year)
			}
		}
		candidates = append(candidates, scene.Title) // English-named trackers
		for _, p := range perfNames {
			candidates = append(candidates, p) // bare performer — broadest net
		}
	}

	seen := map[string]bool{}
	var out []string
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" || seen[strings.ToLower(c)] {
			continue
		}
		seen[strings.ToLower(c)] = true
		out = append(out, c)
	}
	return out
}

// releaseContentKey identifies the same underlying release across queries
// even when the indexer hands back a different (tokenised) download URL
// each time: same normalised title + indexer + size + protocol.
func releaseContentKey(r prowlarr.Release) string {
	title := strings.ToLower(strings.Join(strings.Fields(r.Title), " "))
	return title + "|" + strings.ToLower(r.Indexer) + "|" +
		strconv.FormatInt(r.Size, 10) + "|" + r.Protocol
}

// grabbable reports whether a release can actually be downloaded right
// now. A torrent with zero seeders can't; usenet has no seeder notion so
// it's always considered grabbable.
func grabbable(r sceneRelease) bool {
	return !(r.Protocol == "torrent" && r.Seeders == 0)
}

// seedTier buckets torrent seed health so a marginal file-size edge can't
// promote a barely-seeded release over a well-seeded one of equal score:
// 0 = dead, 1 = risky (1-4), 2 = ok (5-19), 3 = healthy (20+). Usenet has no
// seeders, so it's treated as healthy — its deliverability doesn't decay.
func seedTier(r sceneRelease) int {
	if r.Protocol != "torrent" {
		return 3
	}
	switch {
	case r.Seeders >= 20:
		return 3
	case r.Seeders >= 5:
		return 2
	case r.Seeders >= 1:
		return 1
	default:
		return 0
	}
}

// rankReleases sorts a verified release list into the canonical order
// every consumer shows and picks from: verified first, then non-rejected,
// then the betterRelease comparator. ONE definition on purpose, so the
// interactive Grab view, the watcher's auto-pick, and the deferred-retry
// failover can never silently disagree about which release is "best".
func rankReleases(out []sceneRelease) {
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Verified != out[j].Verified {
			return out[i].Verified
		}
		if out[i].Rejected != out[j].Rejected {
			return !out[i].Rejected // non-rejected first
		}
		return betterRelease(out[i], out[j])
	})
}

// verifyReleases runs the matcher over a set of Prowlarr releases and
// shapes each into a sceneRelease (verified flag, confidence, best-other
// scene, reason breakdown) for the target scene. Shared by the
// /scenes/{id}/releases endpoint (the interactive Grab view) and the watch
// loop, so the verify/badge logic — and the candidate list a watch stores
// for re-pick — is identical to the interactive view.
// javCodeConf is the confidence assigned to a release verified purely by a
// matching JAV code — same floor the matcher uses for a code auto-verify.
const javCodeConf = 0.85

// javCodeMatches reports whether the release title and the target scene title
// share a JAV code (e.g. release "OAE302" and scene "OAE-302" → "oae-302").
// A code uniquely identifies a JAV scene, so a shared code is a definitive
// match — the basis for accepting bare-code indexer results (OneJAV).
func javCodeMatches(relTitle, sceneTitle string) bool {
	rel := matcher.ExtractJAVCodes(relTitle)
	if len(rel) == 0 {
		return false
	}
	scene := matcher.ExtractJAVCodes(sceneTitle)
	if len(scene) == 0 {
		return false
	}
	have := make(map[string]bool, len(scene))
	for _, c := range scene {
		have[c] = true
	}
	for _, c := range rel {
		if have[c] {
			return true
		}
	}
	return false
}

func (s *Server) verifyReleases(ctx context.Context, m *matcher.Matcher, sceneID, sceneTitle string, releases []prowlarr.Release) []sceneRelease {
	scorer := s.releaseScorer()
	titles := make([]string, len(releases))
	for i, rel := range releases {
		titles[i] = rel.Title
	}
	out := make([]sceneRelease, len(releases))
	for res := range m.MatchStream(ctx, titles, searchMatcherConcurrency) {
		rel := releases[res.Index]
		var conf float64
		verified := false
		var bestOtherID, bestOtherTitle string
		var bestOtherConf float64
		var reasons []string
		if res.Err == nil && len(res.Candidates) > 0 {
			vr := matcher.Verify(res.Candidates, sceneID, sceneTitle, rel.Title)
			verified = vr.Verified
			conf = vr.Confidence
			for _, c := range res.Candidates {
				if c.Scene.ID == sceneID {
					reasons = c.Reasons
					break
				}
			}
			if top := res.Candidates[0]; !verified && top.Scene.ID != sceneID {
				bestOtherID = top.Scene.ID
				bestOtherTitle = top.Scene.Title
				bestOtherConf = top.Confidence
			}
		}
		// JAV-code shortcut. Some indexers (OneJAV) title results as the bare
		// code with no performer/studio/date ("OAE302"), so the matcher
		// gathers no candidate scenes and can't verify them — they'd vanish.
		// But a JAV code uniquely identifies the scene: if the release's code
		// matches the target scene's, it IS this scene. Verify directly. (The
		// code extractor normalises "OAE302" and "OAE-302" to the same form.)
		if !verified && javCodeMatches(rel.Title, sceneTitle) {
			verified = true
			if conf < javCodeConf {
				conf = javCodeConf
			}
			reasons = append(reasons, "jav code matches scene")
			bestOtherID, bestOtherTitle, bestOtherConf = "", "", 0
		}
		// A multi-scene PACK is never a single scene, even when it shares the
		// performer name (which inflates title overlap enough to verify). Strip
		// the verification so it can't be auto-grabbed for a single-scene watch
		// — grabbing a performer megapack for one scene pulls tens of GB of the
		// wrong content.
		if verified && isPackRelease(rel.Title) {
			verified = false
			reasons = append(reasons, "looks like a multi-scene pack, not this scene")
		}
		// A title advertising a streaming link / "watch full video" is tube
		// spam — a teaser or crushed rip, not the full release. Un-verify so it
		// can't be auto-grabbed or stored as a watch candidate.
		if verified && isLinkSpamRelease(rel.Title) {
			verified = false
			reasons = append(reasons, "title advertises a streaming link — looks like spam, not the full release")
		}
		// A photo/image set shares the scene's performer + date and so verifies
		// on that overlap, but a watch tracks the VIDEO — never surface the
		// gallery as a grabbable candidate.
		if verified && isImageSetRelease(rel.Title) {
			verified = false
			reasons = append(reasons, "looks like a photo image set, not the video scene")
		}
		sc := scorer.Score(rel.Title, rel.Indexer, rel.Protocol)
		out[res.Index] = sceneRelease{
			Title:          rel.Title,
			Indexer:        rel.Indexer,
			Protocol:       rel.Protocol,
			Size:           rel.Size,
			Popularity:     rel.Popularity,
			Seeders:        rel.Seeders,
			Grabs:          rel.Grabs,
			PublishDate:    rel.PublishDate,
			InfoURL:        rel.InfoURL,
			DownloadURL:    rel.GrabURL(),
			Verified:       verified,
			Confidence:     conf,
			BestMatchID:    bestOtherID,
			BestMatchTitle: bestOtherTitle,
			BestMatchConf:  bestOtherConf,
			Reasons:        reasons,
			Score:          sc.Score,
			Rejected:       sc.Rejected,
			ScoreHits:      sc.Hits,
		}
	}
	return out
}

// searchSceneReleases runs the scene's derived queries against Prowlarr
// SEQUENTIALLY and returns the merged, deduped release set. A single query
// failing doesn't fail the whole search — we return whatever the others found.
//
// Sequential, not concurrent, on purpose: Prowlarr fans out to all indexers in
// parallel WITHIN one query, but rate-limits repeat requests to the SAME
// indexer (~2s apart). Every derived term hits every indexer, so firing the
// terms concurrently doesn't finish any faster — Prowlarr just queues them at
// the per-indexer rate limit — while dumping N× the load at once and (under a
// deadline) getting the later terms cancelled, the work already wasted. Running
// them one at a time is the same wall-clock under the rate limit, lets each
// term complete, and stops firing the instant the caller's deadline/cancel
// trips (so a slow scene doesn't keep hammering Prowlarr after we've given up).
func (s *Server) searchSceneReleases(ctx context.Context, pc *prowlarr.Client, scene *stashdb.Scene, perfNames []string, cats []int, lean bool) ([]prowlarr.Release, error) {
	terms := sceneSearchTerms(scene, perfNames, lean)
	if len(terms) == 0 {
		return nil, nil
	}
	var (
		merged   []prowlarr.Release
		seen     = map[string]bool{}
		firstErr error
	)
	for _, term := range terms {
		// Stop firing once the caller has given up — no point queuing more
		// rate-limited work against a deadline we've already blown.
		if ctx.Err() != nil {
			break
		}
		rels, err := pc.Search(ctx, term, cats)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			s.log.Warn("scene release query", "term", term, "err", err)
			continue
		}
		for _, rel := range rels {
			// Dedup by grab URL (cheap, catches exact repeats) AND by a
			// content key (title+indexer+size+protocol). Trackers like
			// PornoLab hand back the same torrent with a different
			// tokenised download URL across queries, so the URL key
			// alone leaves visible duplicate rows.
			urlKey := rel.GrabURL()
			if urlKey == "" {
				urlKey = rel.Title
			}
			ck := "c|" + releaseContentKey(rel)
			uk := "u|" + urlKey
			if seen[uk] || seen[ck] {
				continue
			}
			seen[uk] = true
			seen[ck] = true
			merged = append(merged, rel)
		}
	}
	// Only surface an error when every query failed (nothing to show).
	if len(merged) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return merged, nil
}
