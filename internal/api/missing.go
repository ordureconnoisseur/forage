package api

import (
	"context"
	"database/sql"
	"net/http"
	"strings"

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
}

type missingPerformer struct {
	Name string `json:"name"`
	As   string `json:"as,omitempty"`
}

type missingResponse struct {
	Performer struct {
		LocalID   string `json:"local_id"`
		StashDBID string `json:"stashdb_id"`
		Name      string `json:"name"`
	} `json:"performer"`
	TotalScenes int            `json:"total_scenes"`
	OwnedCount  int            `json:"owned_count"`
	Missing     []missingScene `json:"missing"`
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
	localID := r.URL.Query().Get("performer")
	if localID == "" {
		writeErr(w, http.StatusBadRequest, "performer query param required")
		return
	}
	stashC := s.pool.Stash()
	stashDBC := s.pool.StashDB()
	if stashC == nil || stashDBC == nil {
		writeErr(w, http.StatusServiceUnavailable, "stash and stashdb must be configured (see Settings)")
		return
	}

	// 1. Resolve local performer → StashDB cross-id. Without one we
	// can't query StashDB for the performer's filmography.
	perf, err := loadPerformerByID(r.Context(), s.db, localID)
	if err == sql.ErrNoRows {
		writeErr(w, http.StatusNotFound, "performer not found")
		return
	}
	if err != nil {
		s.log.Error("performer lookup", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	stashDBPerformerID, err := lookupStashDBPerformerID(r.Context(), s.db, localID)
	if err != nil {
		s.log.Error("stashdb cross-id lookup", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	if stashDBPerformerID == "" {
		writeErr(w, http.StatusUnprocessableEntity, "performer has no StashDB cross-id; can't query StashDB for their filmography")
		return
	}

	// 2. Pull every StashDB scene featuring this performer (memoised per
	// performer — this pagination dominates a cold load).
	scenes, err := s.performerFilmography(r.Context(), stashDBC, stashDBPerformerID)
	if err != nil {
		s.log.Error("stashdb scenes by performer", "err", err)
		writeErr(w, http.StatusBadGateway, "stashdb: "+err.Error())
		return
	}

	// 3. Set of StashDB scene ids the user owns ANYWHERE in their library
	// (not just scenes locally tagged with this performer). A scene you
	// have but never tagged with this performer still counts as owned —
	// otherwise it falsely shows as "missing". Same library-wide cross-id
	// basis as the scene cache's card count, so card and page agree.
	// Memoised (ownedTTL) so this load doesn't re-sweep the whole library.
	ownedSet, err := s.ownedStashDBSet(r.Context())
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

	// 5. Diff. Anything in `scenes` whose ID isn't in `ownedSet`. A scene
	// with an in-flight grab is still "missing" (not in the library yet)
	// but carries its grab status so the UI can flag it.
	missing := make([]missingScene, 0, len(scenes))
	for _, sc := range scenes {
		if ownedSet[sc.ID] {
			continue
		}
		ms := toMissingScene(sc)
		if st, ok := grabStatus[sc.ID]; ok {
			ms.GrabStatus = st
		}
		missing = append(missing, ms)
	}

	out := missingResponse{
		TotalScenes: len(scenes),
		OwnedCount:  len(scenes) - len(missing),
		Missing:     missing,
	}
	out.Performer.LocalID = perf.StashID
	out.Performer.StashDBID = stashDBPerformerID
	out.Performer.Name = perf.Name

	writeJSON(w, http.StatusOK, out)
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

