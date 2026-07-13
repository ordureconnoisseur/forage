package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ordureconnoisseur/forager/internal/cache"
	"github.com/ordureconnoisseur/forager/internal/stashdb"
	"github.com/ordureconnoisseur/forager/internal/subscriptions"
	"github.com/ordureconnoisseur/forager/internal/watches"
)

// Subscriptions: permanent performer/studio watches. A subscription is
// the forward-looking half of collection building (a collection job
// sweeps the backlog ONCE; a subscription covers everything released
// from now on). The loop below diffs each subject's StashDB scene list
// against the subscription's watermark and creates ordinary scene
// watches for what's new; the existing watch machinery (re-search, RSS,
// available, Telegram ping, grab, deferred-retry) does everything else.
var errStashDBUnavailable = errors.New("stashdb not configured")

const (
	// subscriptionTick is the loop cadence. New-scene detection is two
	// aggregate reads per subscription (the count-sync maintains
	// last_release_unix per subject), so hourly costs almost nothing;
	// the expensive scene-list fetch runs only when the aggregates moved.
	subscriptionTick = time.Hour
)

func (s *Server) getSubscriptions(w http.ResponseWriter, r *http.Request) {
	subs, err := s.subs.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	if subs == nil {
		subs = []subscriptions.Subscription{}
	}
	// Badge counts from the subscriptions' watch batches: how many
	// watches appeared since the user last looked at the card, and how
	// many sit available (grabbable) right now. One watch-list pass.
	type subOut struct {
		subscriptions.Subscription
		NewCount   int `json:"new_count"`
		ReadyCount int `json:"ready_count"`
	}
	out := make([]subOut, len(subs))
	byBatch := map[string]*subOut{}
	for i, sub := range subs {
		out[i] = subOut{Subscription: sub}
		byBatch[sub.BatchID()] = &out[i]
	}
	if list, werr := s.watches.List(r.Context()); werr == nil {
		for _, wt := range list {
			so, ok := byBatch[wt.BatchID]
			if !ok {
				continue
			}
			if wt.CreatedAt > so.LastSeenAt {
				so.NewCount++
			}
			if wt.Status == watches.StatusAvailable {
				so.ReadyCount++
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscriptions": out})
}

func (s *Server) postSubscription(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StashDBID string `json:"stashdb_id"`
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		ImageURL  string `json:"image_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if req.StashDBID == "" || req.Name == "" {
		writeErr(w, http.StatusBadRequest, "stashdb_id and name required")
		return
	}
	if req.Kind != "studio" {
		req.Kind = "performer"
	}
	if err := s.subs.Add(r.Context(), subscriptions.Subscription{
		StashDBID: req.StashDBID, Kind: req.Kind, Name: req.Name, ImageURL: req.ImageURL,
	}); err != nil {
		s.log.Error("subscription add", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	s.log.Info("subscribed", "kind", req.Kind, "subject", req.Name, "stashdb_id", req.StashDBID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) deleteSubscription(w http.ResponseWriter, r *http.Request) {
	if err := s.subs.Remove(r.Context(), chi.URLParam(r, "id")); err != nil {
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) postSubscriptionAutoGrab(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if err := s.subs.SetAutoGrab(r.Context(), chi.URLParam(r, "id"), req.Enabled); err != nil {
		writeErr(w, http.StatusNotFound, "subscription not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) postSubscriptionSeen(w http.ResponseWriter, r *http.Request) {
	if err := s.subs.MarkSeen(r.Context(), chi.URLParam(r, "id")); err != nil {
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// RunSubscriptionLoop checks every subscription for newly released
// scenes and creates watches for them; started from main like the other
// loops. An eager first pass runs at boot (cheap when nothing changed).
func (s *Server) RunSubscriptionLoop(ctx context.Context) {
	t := time.NewTicker(subscriptionTick)
	defer t.Stop()
	tick := func() {
		defer s.recoverPanic("subscription loop")
		s.tickSubscriptions(ctx)
	}
	tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// subjectAggregates reads the count-sync's cheap change signals for one
// subject: its newest StashDB release date and its total scene count.
// Zeros when the cache has no row yet (subject never synced), which the
// caller treats as "nothing to do this pass".
func (s *Server) subjectAggregates(ctx context.Context, kind, stashdbID string) (lastRelease int64, total int) {
	table := "performer_cache"
	if kind == "studio" {
		table = "studio_cache"
	}
	_ = s.db.QueryRowContext(ctx,
		`SELECT COALESCE(last_release_unix, 0), COALESCE(total_stashdb_scenes, 0)
		 FROM `+table+` WHERE stashdb_id = ?`,
		stashdbID).Scan(&lastRelease, &total)
	return lastRelease, total
}

// tickSubscriptions runs one pass over every subscription.
func (s *Server) tickSubscriptions(ctx context.Context) {
	if s.subs == nil {
		return
	}
	subs, err := s.subs.List(ctx)
	if err != nil || len(subs) == 0 {
		return
	}
	wlist, werr := s.watches.List(ctx)
	if werr != nil {
		s.log.Warn("subscription tick: watch list unavailable", "err", werr)
		return
	}

	// Hands-free sweep FIRST, and independent of the dedup context below:
	// grabbing an already-available watch needs nothing from Stash, so a
	// Stash blip must not pause auto-grab subscriptions. Same path as the
	// UI/Telegram button (benched-indexer filter included). Watches that
	// were already available before the user enabled auto_grab are picked
	// up too, deliberately.
	for i := range subs {
		if !subs[i].AutoGrab {
			continue
		}
		for _, wt := range wlist {
			if wt.BatchID != subs[i].BatchID() || wt.Status != watches.StatusAvailable {
				continue
			}
			if err := s.grabAvailableWatch(ctx, wt.StashDBID); err != nil {
				s.log.Warn("subscription auto-grab", "subject", subs[i].Name,
					"scene", wt.StashDBID, "err", err)
				continue
			}
			s.log.Info("subscription auto-grab", "subject", subs[i].Name,
				"scene", wt.StashDBID, "release", wt.FoundTitle)
		}
	}

	// Dedup context for NEW watch creation, hoisted once per pass: scenes
	// with a live or pending grab, scenes the library already owns, and
	// scenes that already have a watch. Any error skips creation (not the
	// sweep above) rather than risk duplicate watches, and matters twice
	// over here because watches.Add UPSERTS: re-adding an existing watch
	// would reset an available one back to watching.
	live, covered, gerr := s.grabbedSceneSet(ctx)
	owned, oerr := s.ownedSceneCopies(ctx)
	if gerr != nil || oerr != nil {
		s.log.Warn("subscription tick: dedup context unavailable; skipping watch creation",
			"grabs_err", gerr, "owned_err", oerr)
		return
	}
	watchIDs := map[string]bool{}
	for _, wt := range wlist {
		watchIDs[wt.StashDBID] = true
	}

	for i := range subs {
		sub := subs[i]
		lastRelease, total := s.subjectAggregates(ctx, sub.Kind, sub.StashDBID)
		// Fetch the scene list only when either signal moved: a newer
		// release date than the watermark, or a changed scene count (a
		// same-day addition moves the count but not the date). Steady
		// state costs two cached-aggregate reads per subject per tick.
		if lastRelease <= sub.Watermark && total == sub.SeenTotal {
			_ = s.subs.AdvanceCheck(ctx, sub.StashDBID, sub.Watermark, sub.SeenTotal)
			continue
		}
		scenes, serr := s.subScenes(ctx, sub.Kind, sub.StashDBID)
		if serr != nil {
			s.log.Warn("subscription: scene fetch", "subject", sub.Name, "err", serr)
			continue // no AdvanceCheck: retry the same watermark next pass
		}
		var fresh []watches.Watch
		maxDate := sub.Watermark
		for _, sc := range scenes {
			d, derr := time.Parse("2006-01-02", sc.Date)
			if derr != nil || sc.ID == "" || sc.Title == "" {
				continue
			}
			du := d.Unix()
			if du > maxDate {
				maxDate = du
			}
			// >= watermark: two releases on the watermark day must both be
			// seen; the dedup below absorbs the re-scan of the first one.
			if du < sub.Watermark {
				continue
			}
			if watchIDs[sc.ID] || live[sc.ID] || covered[sc.ID] || len(owned[sc.ID]) > 0 {
				continue
			}
			wt := watches.Watch{
				StashDBID:  sc.ID,
				Title:      sc.Title,
				Date:       sc.Date,
				BatchID:    sub.BatchID(),
				BatchLabel: sub.Name,
				Target:     watches.TargetAny,
			}
			if sc.Studio != nil {
				wt.StudioName = sc.Studio.Name
			}
			if len(sc.Images) > 0 {
				wt.ImageURL = sc.Images[0].URL
			}
			// Placement folder: the subscribed performer for performer
			// subs; the scene's first credited performer for studio subs.
			if sub.Kind == "performer" {
				wt.PerformerName = sub.Name
				wt.PerformerID = sub.StashDBID
			} else if len(sc.Performers) > 0 {
				wt.PerformerName = sc.Performers[0].Name
			}
			for _, p := range sc.Performers {
				wt.Performers = append(wt.Performers, p.Name)
			}
			fresh = append(fresh, wt)
			watchIDs[sc.ID] = true // a later subscription in this pass must not re-add
		}
		if len(fresh) > 0 {
			if aerr := s.watches.AddBatch(ctx, fresh); aerr != nil {
				s.log.Warn("subscription: add watches", "subject", sub.Name, "err", aerr)
				continue // retry with the same watermark next pass
			}
			s.log.Info("subscription: new scenes watched",
				"subject", sub.Name, "count", len(fresh))
		}
		_ = s.subs.AdvanceCheck(ctx, sub.StashDBID, maxDate, total)
	}
}

// defaultSubScenes is the production scene source: the lazy StashDB
// scene cache. A Server field so tests can stub the fetch.
func (s *Server) defaultSubScenes(ctx context.Context, kind, stashdbID string) ([]stashdb.Scene, error) {
	sdb := s.pool.StashDB()
	if sdb == nil {
		return nil, errStashDBUnavailable
	}
	if kind == "studio" {
		return cache.ScenesForStudioLazy(ctx, s.db, sdb, stashdbID)
	}
	return cache.ScenesForPerformerLazy(ctx, s.db, sdb, stashdbID)
}
