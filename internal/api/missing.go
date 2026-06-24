package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/ordureconnoisseur/forager/internal/stash"
	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

type missingScene struct {
	StashDBID  string             `json:"stashdb_id"`
	Title      string             `json:"title"`
	Date       string             `json:"date,omitempty"`
	Studio     string             `json:"studio,omitempty"`
	StudioID   string             `json:"studio_id,omitempty"`
	Performers []missingPerformer `json:"performers"`
	URL        string             `json:"url,omitempty"`
	ImageURL   string             `json:"image_url,omitempty"`
	// GrabStatus is the in-flight grab status for this scene (queued /
	// downloading / completed / placed / scanned), set when a grab for it
	// exists but hasn't yet landed in the library. Empty for scenes with
	// no active grab. Lets the UI show "downloading…" so you don't re-grab
	// something already on the way.
	GrabStatus string `json:"grab_status,omitempty"`
	// WatchStatus is the watch state for this scene ("watching" /
	// "available") when the user is tracking it, empty otherwise. Lets the
	// card show the Track control's current state.
	WatchStatus string `json:"watch_status,omitempty"`
}

type missingPerformer struct {
	Name string `json:"name"`
	As   string `json:"as,omitempty"`
	// Aliases are alternate spellings from the user's local Stash performer
	// record — a tracker may have indexed the release under one of these.
	// Populated only where the UI offers an alias-retry (scene releases);
	// omitted elsewhere.
	Aliases []string `json:"aliases,omitempty"`
}

// ownedScene is a StashDB scene the user already has, annotated with the
// current best copy's quality — the signal for "is this worth upgrading?".
// Clicking it routes to the same release-search page as a missing scene, just
// framed as finding a better release.
type ownedScene struct {
	StashDBID  string             `json:"stashdb_id"`
	Title      string             `json:"title"`
	Date       string             `json:"date,omitempty"`
	Studio     string             `json:"studio,omitempty"`
	StudioID   string             `json:"studio_id,omitempty"`
	Performers []missingPerformer `json:"performers"`
	URL        string             `json:"url,omitempty"`
	ImageURL   string             `json:"image_url,omitempty"`
	// Resolution is the user's current best copy as a label ("1080p",
	// "2160p", …); Height is the raw pixel height (sort key); Size is the
	// file's bytes. Empty/zero when Stash reported no dimensions.
	Resolution string `json:"resolution,omitempty"`
	Height     int    `json:"height,omitempty"`
	Size       int64  `json:"size,omitempty"`
	// GrabStatus flags an in-flight upgrade grab for this scene.
	GrabStatus string `json:"grab_status,omitempty"`
}

// sceneCopyView is one local Stash scene that carries a given StashDB
// cross-id — a single "copy" in the duplicates view. A scene with multiple
// files collapses to its best file here; what the user deletes is the whole
// Stash scene (SceneID).
type sceneCopyView struct {
	SceneID    string `json:"scene_id"`
	Path       string `json:"path,omitempty"`
	Resolution string `json:"resolution,omitempty"`
	Height     int    `json:"height,omitempty"`
	Size       int64  `json:"size,omitempty"`
}

// duplicateScene is a scene the user holds 2+ separate library copies of —
// the cleanup surface (keep the best, delete the rest). Copies is sorted
// best-resolution-first.
type duplicateScene struct {
	StashDBID string          `json:"stashdb_id"`
	Title     string          `json:"title"`
	Date      string          `json:"date,omitempty"`
	Studio    string          `json:"studio,omitempty"`
	ImageURL  string          `json:"image_url,omitempty"`
	Copies    []sceneCopyView `json:"copies"`
}

// subject is who/what the missing-scenes page is about — a performer or a
// studio. The two share the entire gap-analysis pipeline; only the StashDB
// query (PerformerIDs vs StudioIDs) and the placement of grabs differ.
type subject struct {
	Kind      string `json:"kind"` // "performer" | "studio"
	LocalID   string `json:"local_id"`
	StashDBID string `json:"stashdb_id"`
	Name      string `json:"name"`
}

type missingResponse struct {
	Subject subject `json:"subject"`
	// Performer is retained for back-compat (populated only for a performer
	// subject); new code reads Subject.
	Performer struct {
		LocalID   string `json:"local_id"`
		StashDBID string `json:"stashdb_id"`
		Name      string `json:"name"`
	} `json:"performer"`
	TotalScenes int              `json:"total_scenes"`
	OwnedCount  int              `json:"owned_count"`
	Missing     []missingScene   `json:"missing"`
	Owned       []ownedScene     `json:"owned"`
	Duplicates  []duplicateScene `json:"duplicates"`
	// UnidentifiedLocal is how many of your local scenes are tagged with this
	// subject but carry NO StashDB cross-id — so they're invisible to the
	// owned/missing math above (a scene you actually hold but never identified
	// can otherwise show as "missing"). 0 when the count couldn't be fetched.
	UnidentifiedLocal int `json:"unidentified_local"`
}

// getMissingScenes returns the StashDB scenes featuring the given
// performer that aren't already in the user's local Stash library.
//
//	GET /missing-scenes?performer=<local_stash_id>
//
// Powers the planned Stash plugin's "Forage" tab on performer pages:
// show me the gap between "what StashDB knows about this performer"
// and "what I have."
func (s *Server) getMissingScenes(w http.ResponseWriter, r *http.Request) {
	performerID := r.URL.Query().Get("performer")
	studioID := r.URL.Query().Get("studio")
	if performerID == "" && studioID == "" {
		writeErr(w, http.StatusBadRequest, "performer or studio query param required")
		return
	}
	stashC := s.pool.Stash()
	stashDBC := s.pool.StashDB()
	if stashC == nil || stashDBC == nil {
		writeErr(w, http.StatusServiceUnavailable, "stash and stashdb must be configured (see Settings)")
		return
	}

	// 1. Resolve the subject (performer or studio) → its StashDB id + name,
	// then pull its full StashDB catalogue (memoised — this pagination
	// dominates a cold load). The studio param IS the studio_cache key (the
	// StashDB cross-id); a synthetic "stash:" key has no cross-id to query.
	var (
		subj   subject
		scenes []stashdb.Scene
		err    error
	)
	if studioID != "" {
		if strings.HasPrefix(studioID, "stash:") {
			writeErr(w, http.StatusUnprocessableEntity, "studio has no StashDB cross-id; can't query its catalogue")
			return
		}
		name, localID, lerr := lookupStudio(r.Context(), s.db, studioID)
		if lerr == sql.ErrNoRows {
			writeErr(w, http.StatusNotFound, "studio not found")
			return
		}
		if lerr != nil {
			s.log.Error("studio lookup", "err", lerr)
			writeErr(w, http.StatusInternalServerError, "db")
			return
		}
		subj = subject{Kind: "studio", LocalID: localID, StashDBID: studioID, Name: name}
		scenes, err = s.studioFilmography(r.Context(), stashDBC, studioID)
		if err != nil {
			s.log.Error("stashdb scenes by studio", "err", err)
			writeErr(w, http.StatusBadGateway, "stashdb: "+err.Error())
			return
		}
	} else {
		perf, perr := loadPerformerByID(r.Context(), s.db, performerID)
		if perr == sql.ErrNoRows {
			writeErr(w, http.StatusNotFound, "performer not found")
			return
		}
		if perr != nil {
			s.log.Error("performer lookup", "err", perr)
			writeErr(w, http.StatusInternalServerError, "db")
			return
		}
		stashDBPerformerID, lerr := lookupStashDBPerformerID(r.Context(), s.db, performerID)
		if lerr != nil {
			s.log.Error("stashdb cross-id lookup", "err", lerr)
			writeErr(w, http.StatusInternalServerError, "db")
			return
		}
		if stashDBPerformerID == "" {
			writeErr(w, http.StatusUnprocessableEntity, "performer has no StashDB cross-id; can't query StashDB for their filmography")
			return
		}
		subj = subject{Kind: "performer", LocalID: perf.StashID, StashDBID: stashDBPerformerID, Name: perf.Name}
		scenes, err = s.performerFilmography(r.Context(), stashDBC, stashDBPerformerID)
		if err != nil {
			s.log.Error("stashdb scenes by performer", "err", err)
			writeErr(w, http.StatusBadGateway, "stashdb: "+err.Error())
			return
		}
	}

	// 3. Set of StashDB scene ids the user owns ANYWHERE in their library
	// (not just scenes locally tagged with this performer). A scene you
	// have but never tagged with this performer still counts as owned —
	// otherwise it falsely shows as "missing". Same library-wide cross-id
	// basis as the scene cache's card count, so card and page agree.
	// Memoised (ownedTTL) so this load doesn't re-sweep the whole library.
	// Owned copies (with resolution/size) keyed by StashDB scene id. A scene
	// you own anywhere — even if never tagged with this performer — counts as
	// owned. The copies carry quality so the owned-scenes view can show what
	// you currently hold (the upgrade signal). Memoised (ownedTTL).
	ownedCopies, err := s.ownedSceneCopies(r.Context())
	if err != nil {
		s.log.Error("stash owned scene sweep", "err", err)
		writeErr(w, http.StatusBadGateway, "stash: "+err.Error())
		return
	}

	// 4. In-flight grabs by scene id, so missing scenes already being
	// grabbed show their status instead of looking un-actioned. Best
	// effort — a lookup failure just omits the annotation.
	var grabStatus map[string]string
	if s.grabs != nil {
		if gs, gerr := s.grabs.StatusByStashDBID(r.Context()); gerr == nil {
			grabStatus = gs
		} else {
			s.log.Warn("grab status lookup", "err", gerr)
		}
	}

	// Watch status by scene id, so the card's Track control reflects an
	// existing watch.
	watchStatus := s.watchStatusByScene(r.Context())

	// 4b. Drop scenes carrying a user-excluded StashDB tag (compilations,
	// PMVs, …) from the gap analysis entirely — they shouldn't clutter the
	// grid or inflate the owned/missing counts. Build a fresh slice;
	// `scenes` comes from the memoised filmography cache and must not be
	// mutated in place.
	if excluded := excludedTagSet(s.pool.Settings().ExcludedSceneTags); len(excluded) > 0 {
		kept := make([]stashdb.Scene, 0, len(scenes))
		for _, sc := range scenes {
			if !sceneHasExcludedTag(sc, excluded) {
				kept = append(kept, sc)
			}
		}
		scenes = kept
	}

	// 5. Split the filmography into owned vs missing by library membership.
	// A scene with an in-flight grab is still "missing" (not landed yet) but
	// carries its grab status; an owned scene carries its current best-copy
	// quality + any in-flight upgrade grab.
	missing := make([]missingScene, 0, len(scenes))
	owned := make([]ownedScene, 0)
	duplicates := make([]duplicateScene, 0)
	for _, sc := range scenes {
		if refs := ownedCopies[sc.ID]; len(refs) > 0 {
			os := toOwnedScene(sc, bestCopy(refs))
			if st, ok := grabStatus[sc.ID]; ok {
				os.GrabStatus = st
			}
			owned = append(owned, os)
			// 2+ distinct library scenes for the same StashDB id → a
			// duplicate the user can clean up.
			if copies := sceneCopyViews(refs); len(copies) >= 2 {
				duplicates = append(duplicates, toDuplicateScene(sc, copies))
			}
			continue
		}
		ms := toMissingScene(sc)
		if st, ok := grabStatus[sc.ID]; ok {
			ms.GrabStatus = st
		}
		if st, ok := watchStatus[sc.ID]; ok {
			ms.WatchStatus = st
		}
		missing = append(missing, ms)
	}

	// Count this subject's local scenes that lack a StashDB cross-id — they
	// don't participate in the owned/missing diff above, so surface them so the
	// bar doesn't imply you're missing scenes you actually hold un-identified.
	// Best-effort: a failure just leaves it 0.
	unidentified := 0
	if pl, sl := "", ""; subj.LocalID != "" {
		if subj.Kind == "studio" {
			sl = subj.LocalID
		} else {
			pl = subj.LocalID
		}
		if n, uerr := stashC.CountUnidentifiedScenes(r.Context(), pl, sl); uerr != nil {
			s.log.Warn("unidentified scene count", "subject", subj.LocalID, "err", uerr)
		} else {
			unidentified = n
		}
	}

	out := missingResponse{
		Subject:           subj,
		TotalScenes:       len(scenes),
		OwnedCount:        len(owned),
		Missing:           missing,
		Owned:             owned,
		Duplicates:        duplicates,
		UnidentifiedLocal: unidentified,
	}
	// Back-compat: populate the legacy `performer` block for a performer
	// subject so older clients keep working until they read `subject`.
	if subj.Kind == "performer" {
		out.Performer.LocalID = subj.LocalID
		out.Performer.StashDBID = subj.StashDBID
		out.Performer.Name = subj.Name
	}

	writeJSON(w, http.StatusOK, out)
}

// lookupStudio reads a studio's display name and local Stash id from
// studio_cache by its StashDB cross-id (the studio_cache key). Returns
// sql.ErrNoRows when the studio isn't cached.
func lookupStudio(ctx context.Context, db *sql.DB, stashDBID string) (name, localID string, err error) {
	var lid sql.NullString
	err = db.QueryRowContext(ctx,
		`SELECT name, stash_id FROM studio_cache WHERE stashdb_id = ?`, stashDBID,
	).Scan(&name, &lid)
	if err != nil {
		return "", "", err
	}
	if lid.Valid {
		localID = lid.String
	}
	return name, localID, nil
}

// lookupStashDBPerformerID reads the local performer's StashDB cross-id
// from performer_cache. Returns "" if the performer has no cross-id
// (e.g. only on TPDB or FansDB or never scraped).
func lookupStashDBPerformerID(ctx context.Context, db *sql.DB, localID string) (string, error) {
	var sid sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT stashdb_id FROM performer_cache WHERE stash_id = ?`, localID,
	).Scan(&sid)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !sid.Valid {
		return "", nil
	}
	return strings.TrimSpace(sid.String), nil
}

// excludedTagSet lowercases the configured exclude-tag list into a set for
// case-insensitive membership tests. Returns nil when nothing's excluded.
func excludedTagSet(tags []string) map[string]bool {
	if len(tags) == 0 {
		return nil
	}
	m := make(map[string]bool, len(tags))
	for _, t := range tags {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			m[t] = true
		}
	}
	return m
}

// sceneHasExcludedTag reports whether the scene carries any excluded tag.
func sceneHasExcludedTag(sc stashdb.Scene, excluded map[string]bool) bool {
	for _, t := range sc.Tags {
		if excluded[strings.ToLower(t)] {
			return true
		}
	}
	return false
}

// bestCopy returns the highest-resolution local copy of a scene — what the
// owned view shows, since the user cares about the best quality they hold.
func bestCopy(refs []stash.SceneRef) stash.SceneRef {
	best := refs[0]
	for _, r := range refs[1:] {
		if r.Height > best.Height {
			best = r
		}
	}
	return best
}

// resolutionLabel buckets a pixel height into the label users think in.
func resolutionLabel(h int) string {
	switch {
	case h >= 2160:
		return "2160p"
	case h >= 1440:
		return "1440p"
	case h >= 1080:
		return "1080p"
	case h >= 720:
		return "720p"
	case h >= 480:
		return "480p"
	case h > 0:
		return fmt.Sprintf("%dp", h)
	default:
		return ""
	}
}

// sceneCopyViews collapses the per-file refs of a StashDB cross-id into one
// entry per distinct local Stash scene (its best file), sorted
// best-resolution-first. A scene with several files is ONE copy here — what
// the duplicates view deletes is the whole scene, not a file.
func sceneCopyViews(refs []stash.SceneRef) []sceneCopyView {
	best := map[string]stash.SceneRef{}
	order := make([]string, 0, len(refs))
	for _, r := range refs {
		cur, ok := best[r.SceneID]
		if !ok {
			order = append(order, r.SceneID)
			best[r.SceneID] = r
			continue
		}
		if r.Height > cur.Height {
			best[r.SceneID] = r
		}
	}
	out := make([]sceneCopyView, 0, len(order))
	for _, id := range order {
		r := best[id]
		out = append(out, sceneCopyView{
			SceneID:    id,
			Path:       r.Path,
			Resolution: resolutionLabel(r.Height),
			Height:     r.Height,
			Size:       r.Size,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Height > out[j].Height })
	return out
}

func toDuplicateScene(s stashdb.Scene, copies []sceneCopyView) duplicateScene {
	out := duplicateScene{StashDBID: s.ID, Title: s.Title, Date: s.Date, Copies: copies}
	if s.Studio != nil {
		out.Studio = s.Studio.Name
	}
	if len(s.Images) > 0 {
		out.ImageURL = s.Images[0].URL
	}
	return out
}

func toOwnedScene(s stashdb.Scene, copy stash.SceneRef) ownedScene {
	out := ownedScene{
		StashDBID:  s.ID,
		Title:      s.Title,
		Date:       s.Date,
		Resolution: resolutionLabel(copy.Height),
		Height:     copy.Height,
		Size:       copy.Size,
	}
	if s.Studio != nil {
		out.Studio = s.Studio.Name
		out.StudioID = s.Studio.ID
	}
	for _, p := range s.Performers {
		out.Performers = append(out.Performers, missingPerformer{Name: p.Name, As: p.As})
	}
	if len(s.URLs) > 0 {
		out.URL = s.URLs[0].URL
	}
	if len(s.Images) > 0 {
		out.ImageURL = s.Images[0].URL
	}
	return out
}

func toMissingScene(s stashdb.Scene) missingScene {
	out := missingScene{
		StashDBID: s.ID,
		Title:     s.Title,
		Date:      s.Date,
	}
	if s.Studio != nil {
		out.Studio = s.Studio.Name
		out.StudioID = s.Studio.ID
	}
	for _, p := range s.Performers {
		out.Performers = append(out.Performers, missingPerformer{
			Name: p.Name,
			As:   p.As,
		})
	}
	if len(s.URLs) > 0 {
		out.URL = s.URLs[0].URL
	}
	if len(s.Images) > 0 {
		out.ImageURL = s.Images[0].URL
	}
	return out
}
