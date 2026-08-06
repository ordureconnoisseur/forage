package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

// One scene, cheaply, by StashDB id.
//
// This exists for the mismatch panel. When a grab goes out for scene A and the
// file that lands is scene B, the panel used to print two bare UUIDs, and the
// hero image above it showed B without saying so. Two thirty-six character
// hex strings do not tell anyone that these are different films; you cannot
// look at 019faec1… and 019baa2e… and know which one you meant to download.
// So both sides get a picture and a title, and for that the UI needs to be
// able to ask "what is this id".
//
// Everything heavier already existed and none of it fits. /scenes/{id}/releases
// returns exactly this shape but also fans out across every Prowlarr indexer
// first, which is minutes of work for a thumbnail.

const sceneCardTTL = 6 * time.Hour

type sceneCard struct {
	StashDBID  string `json:"stashdb_id"`
	Title      string `json:"title,omitempty"`
	Date       string `json:"date,omitempty"`
	StudioName string `json:"studio_name,omitempty"`
	ImageURL   string `json:"image_url,omitempty"`
	// Performers is names only. The card is for recognising a scene at a
	// glance, not for navigating anywhere.
	Performers []string `json:"performers,omitempty"`
	// cast keeps the genders the wire form drops, so "hide male performers"
	// can be applied when the card is SERVED rather than when it is cached.
	// Baked in at cache time, a setting change would take six hours to reach
	// a card, and the same cached row would be right for one user and wrong
	// for the same user an hour later.
	cast []cardPerformer
}

type cardPerformer struct {
	name   string
	gender string
}

type sceneCardCache struct {
	mu sync.Mutex
	by map[string]sceneCardEntry
}

type sceneCardEntry struct {
	at   time.Time
	card *sceneCard
}

// getSceneCard serves GET /scenes/{id}/card.
func (s *Server) getSceneCard(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !validImageID(id) { // StashDB ids are UUIDs; so is this shape check
		http.Error(w, "not a stashdb id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()

	s.sceneCards.mu.Lock()
	if got, ok := s.sceneCards.by[id]; ok && time.Since(got.at) < sceneCardTTL {
		s.sceneCards.mu.Unlock()
		writeJSON(w, http.StatusOK, s.servableCard(got.card))
		return
	}
	s.sceneCards.mu.Unlock()

	card, err := s.sceneCardByID(ctx, id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "could not look up that scene: "+shortErr(err))
		return
	}
	if card == nil {
		writeErr(w, http.StatusNotFound, "StashDB has no scene with that id")
		return
	}
	s.sceneCards.mu.Lock()
	if s.sceneCards.by == nil {
		s.sceneCards.by = map[string]sceneCardEntry{}
	}
	s.sceneCards.by[id] = sceneCardEntry{at: time.Now(), card: card}
	s.sceneCards.mu.Unlock()
	writeJSON(w, http.StatusOK, s.servableCard(card))
}

// sceneCardByID reads the local cache first and asks StashDB only on a miss.
//
// The cache holds scenes featuring a local performer inside a 90-day window,
// so it answers for a predicted scene most of the time (a prediction came from
// a watch, which came from a performer you follow) and never for an arbitrary
// one. Hence the fallback rather than a cache-only lookup: a panel that shows
// a picture for one side and a bare id for the other is worse than showing
// neither, because the missing one looks like the broken one.
func (s *Server) sceneCardByID(ctx context.Context, id string) (*sceneCard, error) {
	var c sceneCard
	err := s.db.QueryRowContext(ctx, `
		SELECT stashdb_id, COALESCE(title,''), COALESCE(release_date,''),
		       COALESCE(studio_name,''), COALESCE(image_url,'')
		  FROM recent_scene_cache WHERE stashdb_id = ?`, id).
		Scan(&c.StashDBID, &c.Title, &c.Date, &c.StudioName, &c.ImageURL)
	if err == nil && c.Title != "" {
		return &c, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.log.Warn("scene card: cache read", "id", id, "err", err)
	}

	sdb := s.pool.StashDB()
	if sdb == nil {
		return nil, errors.New("StashDB is not configured")
	}
	sc, err := sdb.FindScene(ctx, id)
	if err != nil {
		return nil, err
	}
	if sc == nil {
		return nil, nil
	}
	out := &sceneCard{StashDBID: sc.ID, Title: sc.Title, Date: sc.Date}
	if sc.Studio != nil {
		out.StudioName = sc.Studio.Name
	}
	if len(sc.Images) > 0 {
		out.ImageURL = sc.Images[0].URL
	}
	for _, p := range sc.Performers {
		name := p.Name
		if p.As != "" {
			name = p.As
		}
		if name != "" {
			out.cast = append(out.cast, cardPerformer{name: name, gender: p.Gender})
		}
	}
	return out, nil
}

// servableCard applies "hide male performers" on the way out.
//
// The card's cast is also what the mismatch panel offers to re-file under, so
// leaving men in it does not merely show a name the user asked not to see: it
// offers to file a scene into a folder for someone they have chosen never to
// have. The setting has to reach here or it is not a setting.
func (s *Server) servableCard(in *sceneCard) *sceneCard {
	out := *in
	out.Performers = nil
	hideMale := s.pool.Settings().HideMalePerformers
	for _, p := range in.cast {
		if hideMale && strings.EqualFold(strings.TrimSpace(p.gender), "MALE") {
			continue
		}
		out.Performers = append(out.Performers, p.name)
	}
	return &out
}
