package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ordureconnoisseur/forager/internal/watches"
)

// Watch endpoints — track a StashDB scene to be told when a release at a
// target quality appears. The background loop (watch_loop.go) does the
// re-searching; these just manage the list and let the user grab an
// available watch.

type addWatchRequest struct {
	StashDBID     string `json:"stashdb_id"`
	Title         string `json:"title"`
	Date          string `json:"date,omitempty"`
	Studio        string `json:"studio,omitempty"`
	ImageURL      string `json:"image_url,omitempty"`
	PerformerName string `json:"performer_name,omitempty"`
	PerformerID   string `json:"performer_id,omitempty"`
	// Performers is ALL the scene's performer names (from the card), stored on
	// the watch so a re-search needs no StashDB fetch and covers every
	// performer. Optional — a bare add resolves it on first check.
	Performers []string `json:"performers,omitempty"`
	// Target resolution is vestigial (kept for request compatibility; the
	// matcher no longer filters on it).
	Target string `json:"target"`
}

func (s *Server) postWatch(w http.ResponseWriter, r *http.Request) {
	var req addWatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if req.StashDBID == "" {
		writeErr(w, http.StatusBadRequest, "stashdb_id required")
		return
	}
	// Hydrate display metadata from StashDB when the caller didn't supply
	// it. The plugin's scene card already passes title/date/studio/image,
	// so this only fires for bare adds (curl, integrations) — keeping the
	// Watching tab able to render a thumbnail regardless of entry point.
	if req.ImageURL == "" {
		if sdb := s.pool.StashDB(); sdb != nil {
			if sc, ferr := sdb.FindScene(r.Context(), req.StashDBID); ferr == nil && sc != nil {
				if req.Title == "" {
					req.Title = sc.Title
				}
				if req.Date == "" {
					req.Date = sc.Date
				}
				if req.Studio == "" && sc.Studio != nil {
					req.Studio = sc.Studio.Name
				}
				if len(sc.Images) > 0 {
					req.ImageURL = sc.Images[0].URL
				}
			}
		}
	}

	target := normalizeTarget(req.Target)
	if err := s.watches.Add(r.Context(), watches.Watch{
		StashDBID:     req.StashDBID,
		Title:         req.Title,
		Date:          req.Date,
		StudioName:    req.Studio,
		ImageURL:      req.ImageURL,
		PerformerName: req.PerformerName,
		PerformerID:   req.PerformerID,
		Performers:    req.Performers,
		Target:        target,
	}); err != nil {
		s.log.Error("watch add", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	s.log.Info("watch added", "scene", req.StashDBID, "target", target)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "target": target})
}

func (s *Server) getWatches(w http.ResponseWriter, r *http.Request) {
	// Reconcile first: drop any watch whose scene forage has since grabbed
	// (by any path — Discover, missing-scenes, a release search — not just
	// the Watching tab's own grab button). Without this a watched scene you
	// obtained elsewhere lingers in Watching forever, since only
	// postWatchGrab removed watches before. Clears the existing backlog the
	// moment the tab loads.
	s.reconcileWatches(r.Context())

	ws, err := s.watches.List(r.Context())
	if err != nil {
		s.log.Error("watch list", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	if ws == nil {
		ws = []watches.Watch{}
	}
	// Overlay the transient "searching now" flag so the UI can spin the cards
	// a manual search-now is actively re-searching.
	s.searchingMu.Lock()
	for i := range ws {
		if s.searching[ws[i].StashDBID] {
			ws[i].Searching = true
		}
	}
	s.searchingMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"watches": ws})
}

// reconcileWatches flips watches to 'grabbed' for scenes forage already has a
// grab for (queued → confirmed, the same set Discover hides), regardless of
// where the grab came from — a watch's job is "tell me when I can get this
// scene", and once a grab exists that job is done. It flips rather than
// deletes so the watch lingers in its batch and the batch's progress reads
// correctly; the user clears it (or the whole batch) when done. Preserves any
// found_* release fields already on the watch.
//
// It also runs the reverse: a 'grabbed' watch whose scene has NO live grab
// and NO owned copy flips back to 'watching'. MarkGrabbed fires as soon as
// the queued grab row exists, so an async add that later fails would
// otherwise leave the watch terminal while the scene silently never arrives
// (batch progress reads complete, nothing re-searches). The owned check
// keeps a watch resolved when the scene landed anyway — a deleted grab row
// after a confirm, or a pack that covered it. Best-effort: logs and moves on.
func (s *Server) reconcileWatches(ctx context.Context) {
	// Self-heal invalid "available but no grab link" rows back to watching, so
	// a release that slipped through before the no-URL selection guard (or any
	// future regression) re-searches instead of sitting un-grabbable. Runs
	// every tick, independent of whether anything was grabbed.
	if n, err := s.watches.ResetUngrabbableAvailable(ctx); err != nil {
		s.log.Warn("watch reset ungrabbable available", "err", err)
	} else if n > 0 {
		s.log.Info("watch reset ungrabbable available → watching", "count", n)
	}

	// A failed lookup means we can't tell live from dead — reconcile nothing.
	// An EMPTY set is meaningful (every grabbed watch is a revert candidate).
	live, covered, err := s.grabbedSceneSet(ctx)
	if err != nil {
		return
	}
	list, err := s.watches.List(ctx)
	if err != nil {
		return
	}
	// Reverting requires the scene to be fully UNcovered: no live grab AND
	// nothing pending resolution. A mismatched grab in the review queue
	// holds its watch quiet — the user's verdict decides whether the hunt
	// resumes (redo/delete removes the coverage), not the machine's.
	var stranded []watches.Watch
	for _, wt := range list {
		if wt.Status == watches.StatusGrabbed {
			if !covered[wt.StashDBID] {
				stranded = append(stranded, wt)
			}
			continue
		}
		// Forward flip stays LIVE-only: a pending mismatch must not retract
		// an already-surfaced 'available' offer (the user may still want to
		// grab it), it only stops new searching.
		if live[wt.StashDBID] {
			if err := s.watches.MarkGrabbed(ctx, wt.StashDBID,
				wt.FoundTitle, wt.FoundURL, wt.FoundIndexer, wt.FoundProtocol, wt.FoundSize); err != nil {
				s.log.Warn("watch reconcile mark grabbed", "scene", wt.StashDBID, "err", err)
				continue
			}
			s.log.Info("watch resolved — scene already grabbed", "scene", wt.StashDBID)
		}
	}
	if len(stranded) == 0 {
		return
	}
	// The owned sweep is the expensive part (memoised, but still a full
	// library fetch on a cold memo) — only pay for it when there's an actual
	// revert candidate, which is rare. On error, revert nothing: we can't
	// prove the scene didn't land.
	owned, oerr := s.ownedSceneCopies(ctx)
	if oerr != nil {
		return
	}
	for _, wt := range stranded {
		sid := wt.StashDBID
		if len(owned[sid]) > 0 {
			continue // scene landed anyway; the watch's job really is done
		}
		// When the grab of THIS watch's release died for a content-side
		// reason (missing articles, unrepairable, corrupt: the release
		// itself is dead on the wire), reverting alone re-runs the loop
		// that killed it: the re-search ranks the same release best,
		// re-offers it, and the re-grab dies identically (observed
		// 2026-07-13: the same aborted NZB grabbed 4x in 57h). Dismiss
		// instead: same revert, plus the dead release joins ignored_urls
		// so the re-search picks a DIFFERENT source. Infra-side failures
		// (NAS write error, client outage) keep the plain revert: the
		// release is fine and remains the best candidate.
		if wt.FoundURL != "" {
			if g, gerr := s.grabs.ByDownloadURL(ctx, wt.FoundURL); gerr == nil &&
				g != nil && g.Status == "failed" && contentDeadReason(g.Reason) {
				if derr := s.watches.Dismiss(ctx, sid, wt.FoundURL, wt.FoundTitle); derr != nil {
					s.log.Warn("watch dismiss dead release", "scene", sid, "err", derr)
					continue
				}
				s.log.Info("watch reverted; dead release auto-ignored",
					"scene", sid, "release", wt.FoundTitle, "reason", g.Reason)
				continue
			}
		}
		if err := s.watches.RevertGrabbed(ctx, sid); err != nil {
			s.log.Warn("watch revert grabbed", "scene", sid, "err", err)
			continue
		}
		s.log.Info("watch reverted — grab died without the scene landing; watching again", "scene", sid)
	}
}

// findWatch loads a single watch by id, or nil. Small wrapper over List —
// the watch list is tiny, so a dedicated lookup isn't worth a repo method.
func (s *Server) findWatch(ctx context.Context, id string) *watches.Watch {
	list, err := s.watches.List(ctx)
	if err != nil {
		return nil
	}
	for i := range list {
		if list[i].StashDBID == id {
			return &list[i]
		}
	}
	return nil
}

func (s *Server) deleteWatch(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.watches.Delete(r.Context(), id); err != nil {
		s.log.Error("watch delete", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// contentDeadReason reports whether a failed grab's reason marks the
// RELEASE ITSELF as dead (re-grabbing the same source is doomed), as
// opposed to an infrastructure failure the same release would survive on
// a retry. The SAB markers are SABnzbd's stable fail_message phrases
// (carried into the reason since the failure-storytelling change); the
// "gave up after" marker is the deferred-retry budget exhausting, which
// only settles after multiple real attempts and a failover pass, so the
// URL has proven itself dead. Deliberately NOT matched: "write error",
// "disk is full", client-unreachable text.
func contentDeadReason(reason string) bool {
	r := strings.ToLower(reason)
	for _, marker := range []string{
		"cannot be completed", // SAB: articles missing on every provider
		"repair failed",       // SAB: not enough par2 blocks
		"failed to verify",    // SAB: RAR verification failure
		"encrypted",           // SAB: password-protected junk
		"unwanted extension",  // SAB: policy-blocked content
		"gave up after",       // deferred-retry budget exhausted
		// qBit/libtorrent rejecting the .torrent FILE itself (PornoLab's
		// cp1251 name fields, chiefly). Deterministic per release: the
		// same bytes decline forever, and transcoding the name would
		// change the infohash and break the private tracker, so another
		// release is the only remedy. Observed looping 2026-07-14 before
		// this marker existed.
		"declined this torrent",
	} {
		if strings.Contains(r, marker) {
			return true
		}
	}
	return false
}

// postWatchGrab grabs the auto-picked (best) release recorded on a watch and
// flips it to 'grabbed' — the watch LINGERS (it is not deleted) so its batch
// progress reads correctly. The user clicks this from the Watching tab when a
// watch goes available. To grab a DIFFERENT release than the best, see
// postWatchGrabCandidate.
func (s *Server) postWatchGrab(w http.ResponseWriter, r *http.Request) {
	if err := s.grabAvailableWatch(r.Context(), chi.URLParam(r, "id")); err != nil {
		writeMappedErr(w, err, http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// grabAvailableWatch grabs a watch's auto-picked (best) release and flips it
// to 'grabbed' — the logic behind the Watching tab's grab button, shared
// with the Telegram callback path so a notification button and the UI do
// exactly the same thing. Typed grabErrors carry HTTP statuses for the
// HTTP caller; the Telegram caller just uses the message.
func (s *Server) grabAvailableWatch(ctx context.Context, id string) error {
	wt := s.findWatch(ctx, id)
	if wt == nil {
		return grabError{http.StatusNotFound, "watch not found"}
	}
	if wt.Status != watches.StatusAvailable || wt.FoundURL == "" {
		return grabError{http.StatusUnprocessableEntity, "watch has no available release yet"}
	}
	// The auto-pick was recorded when the watch went AVAILABLE, possibly
	// hours before this grab. If its indexer is now in Prowlarr's failure
	// backoff, the fetch would just defer and fail over later; grabbing
	// the best stored candidate on a healthy indexer instead lands the
	// scene on the first try. Same safety rules as the deferred failover
	// (verified, grabbable, different + non-benched indexer); any
	// protocol qualifies because no download client is committed yet.
	// Only the AUTO choice is adjusted; an explicit candidate re-pick
	// (postWatchGrabCandidate) is never second-guessed.
	pickURL, pickTitle, pickIndexer := wt.FoundURL, wt.FoundTitle, wt.FoundIndexer
	pickProtocol, pickSize := wt.FoundProtocol, wt.FoundSize
	pickConfidence := 0.0
	if disabled := s.disabledIndexers(ctx); disabled[strings.ToLower(wt.FoundIndexer)] {
		var cands []sceneRelease
		if len(wt.Candidates) > 0 {
			_ = json.Unmarshal(wt.Candidates, &cands)
		}
		if alt := chooseFailover(cands, wt.FoundIndexer, wt.FoundURL, "", disabled, s.contentDeadReleases(ctx)); alt != nil {
			s.log.Info("watch grab: auto-pick indexer is benched; using next candidate",
				"scene", id, "from", wt.FoundIndexer, "to", alt.Indexer, "release", alt.Title)
			pickURL, pickTitle, pickIndexer = alt.DownloadURL, alt.Title, alt.Indexer
			pickProtocol, pickSize = alt.Protocol, alt.Size
			pickConfidence = alt.Confidence
		}
	}
	if _, err := s.doGrab(ctx, grabRequest{
		DownloadURL:    pickURL,
		ReleaseTitle:   pickTitle,
		ReleaseSize:    pickSize,
		ReleaseIndexer: pickIndexer,
		Protocol:       pickProtocol,
		SceneID:        wt.StashDBID,
		Confidence:     pickConfidence,
		PerformerName:  wt.PerformerName,
	}); err != nil {
		return err
	}
	if err := s.watches.MarkGrabbed(ctx, id,
		pickTitle, pickURL, pickIndexer, pickProtocol, pickSize); err != nil {
		s.log.Warn("watch mark grabbed", "scene", id, "err", err)
	}
	return nil
}

// postWatchGrabCandidate grabs a SPECIFIC release from a watch's stored
// candidate list (a re-pick when the auto-chosen best isn't what the user
// wants) and flips the watch to 'grabbed'. The candidate is matched by its
// download URL against the list captured when the watch went available.
func (s *Server) postWatchGrabCandidate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		DownloadURL string `json:"download_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if req.DownloadURL == "" {
		writeErr(w, http.StatusBadRequest, "download_url required")
		return
	}
	wt := s.findWatch(r.Context(), id)
	if wt == nil {
		writeErr(w, http.StatusNotFound, "watch not found")
		return
	}
	if wt.Status != watches.StatusAvailable {
		writeErr(w, http.StatusUnprocessableEntity, "watch has no candidates to pick from")
		return
	}
	var cands []sceneRelease
	if len(wt.Candidates) > 0 {
		_ = json.Unmarshal(wt.Candidates, &cands)
	}
	var cand *sceneRelease
	for i := range cands {
		if cands[i].DownloadURL == req.DownloadURL {
			cand = &cands[i]
			break
		}
	}
	if cand == nil {
		writeErr(w, http.StatusNotFound, "candidate not found on this watch")
		return
	}
	if _, err := s.doGrab(r.Context(), grabRequest{
		DownloadURL:    cand.DownloadURL,
		ReleaseTitle:   cand.Title,
		ReleaseSize:    cand.Size,
		ReleaseIndexer: cand.Indexer,
		Protocol:       cand.Protocol,
		SceneID:        wt.StashDBID,
		Confidence:     cand.Confidence,
		PerformerName:  wt.PerformerName,
	}); err != nil {
		writeMappedErr(w, err, http.StatusBadGateway)
		return
	}
	if err := s.watches.MarkGrabbed(r.Context(), id,
		cand.Title, cand.DownloadURL, cand.Indexer, cand.Protocol, cand.Size); err != nil {
		s.log.Warn("watch mark grabbed (candidate)", "scene", id, "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// addWatchBatchRequest is the bulk-create body: many watches sharing a batch.
// Used by "watch all missing for a performer" and Discover multi-select.
type addWatchBatchRequest struct {
	BatchLabel string            `json:"batch_label"`
	Watches    []addWatchRequest `json:"watches"`
}

// postWatchBatch creates many watches at once under a generated batch id, so
// the Watching tab can group + show their collective progress. Unlike
// postWatch it does NOT hydrate each scene's display metadata from StashDB
// (that would be N round-trips); callers pass card metadata they already have,
// and the watch loop's BackfillMeta fills any gaps later.
func (s *Server) postWatchBatch(w http.ResponseWriter, r *http.Request) {
	var req addWatchBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if len(req.Watches) == 0 {
		writeErr(w, http.StatusBadRequest, "no watches")
		return
	}
	batchID := fmt.Sprintf("b%d", time.Now().UnixNano())
	ws := make([]watches.Watch, 0, len(req.Watches))
	for _, it := range req.Watches {
		if it.StashDBID == "" {
			continue
		}
		ws = append(ws, watches.Watch{
			StashDBID:     it.StashDBID,
			Title:         it.Title,
			Date:          it.Date,
			StudioName:    it.Studio,
			ImageURL:      it.ImageURL,
			PerformerName: it.PerformerName,
			PerformerID:   it.PerformerID,
			Performers:    it.Performers,
			Target:        normalizeTarget(it.Target),
			BatchID:       batchID,
			BatchLabel:    req.BatchLabel,
		})
	}
	if len(ws) == 0 {
		writeErr(w, http.StatusBadRequest, "no valid watches")
		return
	}
	if err := s.watches.AddBatch(r.Context(), ws); err != nil {
		s.log.Error("watch batch add", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	s.log.Info("watch batch added", "batch", batchID, "label", req.BatchLabel, "count", len(ws))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "batch_id": batchID, "count": len(ws)})
}

// clearBatch removes every watch in a batch (the Watching tab's per-batch
// "Clear", typically used once a collection batch is fully grabbed).
func (s *Server) clearBatch(w http.ResponseWriter, r *http.Request) {
	batchID := chi.URLParam(r, "batchId")
	if batchID == "" {
		writeErr(w, http.StatusBadRequest, "batch id required")
		return
	}
	if err := s.watches.DeleteBatch(r.Context(), batchID); err != nil {
		s.log.Error("watch batch clear", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// postWatchDismiss rejects the watch's current found release: ignore that
// exact release (by URL) going forward and flip the watch back to watching
// so the loop surfaces a different one. For the common case where the find
// is dead/over-compressed and you don't want it but still want the scene.
func (s *Server) postWatchDismiss(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	list, err := s.watches.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	var wt *watches.Watch
	for i := range list {
		if list[i].StashDBID == id {
			wt = &list[i]
			break
		}
	}
	if wt == nil {
		writeErr(w, http.StatusNotFound, "watch not found")
		return
	}
	if wt.Status != watches.StatusAvailable || wt.FoundURL == "" {
		writeErr(w, http.StatusUnprocessableEntity, "watch has no found release to dismiss")
		return
	}
	if err := s.watches.Dismiss(r.Context(), id, wt.FoundURL, wt.FoundTitle); err != nil {
		s.log.Error("watch dismiss", "scene", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	s.log.Info("watch find dismissed — back to watching", "scene", id, "ignored", wt.FoundURL)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// postWatchRedo discards a grabbed (or available) release the user decided
// was bad and re-searches the scene for a different one. It is the
// "dismiss this release AND find another" action: unlike plain Dismiss, it
// first PURGES the grab — download-client copy, any placed file/Stash scene,
// and the grab record — because otherwise reconcileWatches would just flip
// the watch straight back to 'grabbed' on the next list load (the grab still
// exists). It then ignores that release's URL and flips the watch back to
// watching; the caller kicks an immediate re-search.
func (s *Server) postWatchRedo(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	wt := s.findWatch(r.Context(), id)
	if wt == nil {
		writeErr(w, http.StatusNotFound, "watch not found")
		return
	}
	if wt.Status != watches.StatusGrabbed && wt.Status != watches.StatusAvailable {
		writeErr(w, http.StatusUnprocessableEntity, "watch has no grabbed or found release to redo")
		return
	}
	if wt.FoundURL == "" {
		writeErr(w, http.StatusUnprocessableEntity, "watch has no release to redo")
		return
	}

	// Purge the underlying grab (if any) so reconcile can't re-flip the watch
	// to grabbed and so the bad download/file is actually removed. A watch
	// stores the grabbed release's download URL in found_url, which is the
	// grab's download_url. Best-effort: if there's no grab row (e.g. the watch
	// only ever went 'available'), there's simply nothing to purge.
	var purge deleteGrabResponse
	if g, err := s.grabs.ByDownloadURL(r.Context(), wt.FoundURL); err != nil {
		s.log.Warn("watch redo: grab lookup", "scene", id, "err", err)
	} else if g != nil {
		purge = s.purgeGrab(r.Context(), g)
	}

	// Ignore this exact release going forward + flip back to watching.
	if err := s.watches.Dismiss(r.Context(), id, wt.FoundURL, wt.FoundTitle); err != nil {
		s.log.Error("watch redo dismiss", "scene", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	s.log.Info("watch redo — release discarded, back to watching",
		"scene", id, "ignored", wt.FoundURL, "purged", purge.Removed)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "purged": purge.Removed})
}

// markSearching adds or removes a set of watch ids from the in-memory
// "searching now" set (overlaid as the transient Watch.Searching flag).
func (s *Server) markSearching(ids []string, on bool) {
	s.searchingMu.Lock()
	defer s.searchingMu.Unlock()
	if s.searching == nil {
		s.searching = map[string]bool{}
	}
	for _, id := range ids {
		if on {
			s.searching[id] = true
		} else {
			delete(s.searching, id)
		}
	}
}

// searchNowConcurrency bounds how many watches a manual "search now" re-checks
// at once. Each watch runs the FULL search, whose query terms already fan out
// concurrently inside searchSceneReleases, and each scene's latency is gated by
// its slowest indexer (~20-30s). At 1 that made a 40-scene "search all" ~15
// min; 2 roughly halves it while keeping in-flight Prowlarr queries modest.
// Going much wider risks choking the torrent trackers (the failure mode that
// motivated removing collection Jobs); per-query timeouts degrade gracefully
// (a slow indexer just drops out, the scene still verifies from the rest).
const searchNowConcurrency = 2

// postWatchSearchNow kicks an immediate, bounded re-search of the still-
// watching scenes (optionally scoped to one batch via {batch_id}), bypassing
// the 30-min loop cadence so a freshly-created batch surfaces releases now.
// Runs in the background at low concurrency, ONE at a time (so clicks can't
// stack load); the watch list's `searching` flag drives the per-card spinner
// and results land via the normal list poll.
func (s *Server) postWatchSearchNow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BatchID string   `json:"batch_id"`
		IDs     []string `json:"ids"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // body optional (nothing = all watching)

	n, err := s.startWatchSearch(r.Context(), req.IDs, req.BatchID)
	if err != nil {
		if errors.Is(err, errSearchBusy) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		var ge grabError
		if errors.As(err, &ge) {
			writeErr(w, ge.status, ge.msg)
			return
		}
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "searching": n})
}

// errSearchBusy: another search-now run holds the lock. Callers treat it
// as "already being handled", not a failure.
var errSearchBusy = errors.New("a search is already running")

// startWatchSearch kicks an immediate background re-search of the scoped
// watching rows and returns how many it started. Scope, most specific
// first: an explicit id set, else a batch, else every watching row. Shared
// by the search-now endpoint and the Telegram Dismiss button (whose web
// twin, "Not this one", follows its dismiss with exactly this kick).
func (s *Server) startWatchSearch(ctx context.Context, watchIDs []string, batchID string) (int, error) {
	if s.pool.Prowlarr() == nil || s.pool.StashDB() == nil {
		return 0, grabError{http.StatusServiceUnavailable, "prowlarr and stashdb must be configured (see Settings)"}
	}
	// One search-now at a time.
	if !s.searchNowMu.TryLock() {
		return 0, errSearchBusy
	}
	list, err := s.watches.List(ctx)
	if err != nil {
		s.searchNowMu.Unlock()
		return 0, err
	}
	idSet := make(map[string]bool, len(watchIDs))
	for _, id := range watchIDs {
		idSet[id] = true
	}
	var targets []watches.Watch
	for _, wt := range list {
		if wt.Status != watches.StatusWatching {
			continue // already available / grabbed — nothing to search
		}
		if len(idSet) > 0 {
			if !idSet[wt.StashDBID] {
				continue
			}
		} else if batchID != "" && wt.BatchID != batchID {
			continue
		}
		targets = append(targets, wt)
	}
	if len(targets) == 0 {
		s.searchNowMu.Unlock()
		return 0, nil
	}
	ids := make([]string, len(targets))
	for i, t := range targets {
		ids[i] = t.StashDBID
	}
	s.markSearching(ids, true)

	go func() {
		defer s.searchNowMu.Unlock()
		defer s.markSearching(ids, false) // backstop clear on early exit
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		jobs := make(chan watches.Watch)
		var wg sync.WaitGroup
		for i := 0; i < searchNowConcurrency; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for wt := range jobs {
					if ctx.Err() != nil {
						return
					}
					s.checkWatch(ctx, wt)
					_ = s.watches.MarkChecked(ctx, wt.StashDBID)
					s.markSearching([]string{wt.StashDBID}, false) // stop this card's spinner
				}
			}()
		}
		for _, t := range targets {
			if ctx.Err() != nil {
				break
			}
			jobs <- t
		}
		close(jobs)
		wg.Wait()
		s.log.Info("watch search-now done", "batch", batchID, "count", len(targets))
	}()

	return len(targets), nil
}

// normalizeTarget validates the quality target, defaulting unknown values
// to "any".
func normalizeTarget(t string) string {
	switch t {
	case watches.Target480, watches.Target720, watches.Target1080, watches.Target4K:
		return t
	default:
		return watches.TargetAny
	}
}

// watchStatusByScene returns scene-id → watch status for badging the
// missing-scenes / discover lists. Best-effort; nil on error.
func (s *Server) watchStatusByScene(ctx context.Context) map[string]string {
	m, err := s.watches.IDs(ctx)
	if err != nil {
		return nil
	}
	return m
}
