package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/qbit"
	"github.com/ordureconnoisseur/forager/internal/sabnzbd"
)

// stalledAfter is how long a torrent grab can sit in a download state
// with no forward progress before /grabs flags it stalled. Surface-only
// — the poller never auto-acts on it; the user decides (abandon + pick
// another release).
const stalledAfter = 30 * time.Minute

// placeFailingAfter is how long a downloaded grab can keep failing to
// place into the library (status "completed" with a place_error, retried
// each poll) before it's surfaced as stuck. The poller keeps retrying
// regardless — this only flags it so a persistent permission/mount/path
// problem doesn't fail silently.
const placeFailingAfter = 10 * time.Minute

// grabProgress is live download state for an in-flight grab, pulled
// fresh from the download client on each /grabs poll.
type grabProgress struct {
	Percent  float64 `json:"percent"`
	SpeedBps int64   `json:"speed_bps,omitempty"`
	EtaSecs  int64   `json:"eta_secs,omitempty"`
}

// qbitEtaUnknown is the sentinel qBit returns for "no ETA yet"
// (100 days in seconds). Treat it as 0 = unknown.
const qbitEtaUnknown = 8640000

type grabOut struct {
	ID                  int64   `json:"id"`
	PredictedStashDBID  string  `json:"predicted_stashdb_id,omitempty"`
	PredictedConfidence float64 `json:"predicted_confidence,omitempty"`
	ActualStashDBID     string  `json:"actual_stashdb_id,omitempty"`
	// SceneTitle is the StashDB scene's real title (resolved server-side and
	// cached), populated only for grabs whose scene is grouped (2+ attempts)
	// so the group header shows the title instead of a bare id. Empty when
	// unknown — the UI falls back to the id.
	SceneTitle     string `json:"scene_title,omitempty"`
	ReleaseTitle   string `json:"release_title"`
	ReleaseSize    int64  `json:"release_size,omitempty"`
	ReleaseIndexer string `json:"release_indexer,omitempty"`
	DownloadURL    string `json:"download_url,omitempty"`
	Client         string `json:"client,omitempty"`
	ClientID       string `json:"client_id,omitempty"`
	ClientName     string `json:"client_name,omitempty"`
	Category       string `json:"category,omitempty"`
	Status         string `json:"status"`
	// Stalled flags a torrent grab still "downloading" that's made no
	// progress for stalledAfter — the UI badges it so the user can
	// abandon it and pick another release.
	Stalled bool `json:"stalled,omitempty"`
	// Adopted marks a grab forage picked up from a torrent the user added
	// to qBit directly (under the forager category), rather than grabbed
	// through forage. Derived: such grabs carry an info-hash but no
	// download URL (there was no Prowlarr fetch).
	Adopted bool `json:"adopted,omitempty"`
	// PlaceFailing flags a downloaded grab whose placement keeps failing
	// (status completed + place_error, past placeFailingAfter) — the file
	// is downloaded but can't get into the library.
	PlaceFailing bool   `json:"place_failing,omitempty"`
	Reason       string `json:"reason,omitempty"`
	// Deferred-retry state (status "deferred"): failed add attempts so
	// far and when the retry loop will re-drive the add.
	Attempts      int           `json:"attempts,omitempty"`
	NextRetryAt   int64         `json:"next_retry_at,omitempty"`
	PerformerName string        `json:"performer_name,omitempty"`
	PlacedPath    string        `json:"placed_path,omitempty"`
	PlaceError    string        `json:"place_error,omitempty"`
	GrabbedAt     int64         `json:"grabbed_at"`
	UpdatedAt     int64         `json:"updated_at"`
	CompletedAt   int64         `json:"completed_at,omitempty"`
	PlacedAt      int64         `json:"placed_at,omitempty"`
	ConfirmedAt   int64         `json:"confirmed_at,omitempty"`
	Progress      *grabProgress `json:"progress,omitempty"`
	// Pack fields — kind is "pack" for performer-pack grabs, with the
	// progress counters; "single" / omitted otherwise.
	Kind           string `json:"kind,omitempty"`
	PackFiles      int    `json:"pack_files,omitempty"`
	PackIdentified int    `json:"pack_identified,omitempty"`
	PackDeduped    int    `json:"pack_deduped,omitempty"`
	// PendingDuplicates is the count of unresolved review-mode dedup items
	// for this pack (PackDedupKeep="review"). The UI badges the pack and
	// loads the per-scene compare from the detail endpoint.
	PendingDuplicates int `json:"pending_duplicates,omitempty"`
}

type grabsResponse struct {
	Grabs  []grabOut      `json:"grabs"`
	Totals map[string]int `json:"totals"`
	// MatchTotal is how many grabs match the current status+q filter across
	// the whole table (not just this page), so the UI can show a result count
	// and know when "load more" has reached the end.
	MatchTotal int `json:"match_total"`
}

// isStalled reports whether a torrent grab still "downloading" has made
// no progress for stalledAfter. progress_at is stamped when progress last
// increased; before any progress is seen it's 0, so we fall back to the
// grab time (a download that never started counts from when it was added).
// Torrent-only — SAB grabs don't carry progress here.
func isStalled(g grabs.Grab) bool {
	if g.Status != "downloading" || g.Client != "qbit" || g.Progress >= 1 {
		return false
	}
	base := g.ProgressAt
	if base == 0 {
		base = g.GrabbedAt
	}
	return base > 0 && time.Since(time.Unix(base, 0)) > stalledAfter
}

// isPlaceFailing reports whether a downloaded grab has been unable to
// place into the library for a while (status completed + a place_error,
// past placeFailingAfter). The poller keeps retrying; this just surfaces
// a persistent placement problem.
func isPlaceFailing(g grabs.Grab) bool {
	return g.Status == "completed" && g.PlaceError != "" &&
		g.CompletedAt > 0 && time.Since(time.Unix(g.CompletedAt, 0)) > placeFailingAfter
}

// postAdopt force-adopts every untracked forage-category download-client
// torrent immediately, bypassing the 5-minute adoption grace.
//
//	POST /grabs/adopt
//
// Backs the Grabs "scan for new downloads" button — for when you've
// bulk-added torrents to the client manually and don't want to wait out the
// grace. Safe: forage won't re-adopt a torrent it already tracks.
func (s *Server) postAdopt(w http.ResponseWriter, r *http.Request) {
	if s.adoptNow == nil {
		writeErr(w, http.StatusServiceUnavailable, "adoption unavailable (no poller)")
		return
	}
	n, skippedRecent := s.adoptNow(r.Context())
	s.log.Info("manual adopt", "adopted", n, "skipped_recent", skippedRecent)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "adopted": n, "skippedRecent": skippedRecent})
}

// getGrabs returns the most-recent grabs with status totals for the
// UI's Grabs tab. Filterable by status; defaults to "any".
func (s *Server) getGrabs(w http.ResponseWriter, r *http.Request) {
	if s.grabs == nil {
		writeJSON(w, http.StatusOK, grabsResponse{Grabs: []grabOut{}, Totals: map[string]int{}})
		return
	}
	query := r.URL.Query()
	status := query.Get("status")
	search := query.Get("q")
	limit, _ := strconv.Atoi(query.Get("limit"))
	offset, _ := strconv.Atoi(query.Get("offset"))

	rows, err := s.grabs.List(r.Context(), status, search, limit, offset)
	if err != nil {
		s.log.Error("grabs list", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	totals, err := s.grabs.Totals(r.Context())
	if err != nil {
		s.log.Error("grabs totals", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}
	matchTotal, err := s.grabs.CountFiltered(r.Context(), status, search)
	if err != nil {
		s.log.Error("grabs count", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}

	out := make([]grabOut, 0, len(rows))
	for _, g := range rows {
		out = append(out, grabOut{
			ID:                  g.ID,
			PredictedStashDBID:  g.PredictedStashDBID,
			PredictedConfidence: g.PredictedConfidence,
			ActualStashDBID:     g.ActualStashDBID,
			ReleaseTitle:        g.ReleaseTitle,
			ReleaseSize:         g.ReleaseSize,
			ReleaseIndexer:      g.ReleaseIndexer,
			DownloadURL:         g.DownloadURL,
			Client:              g.Client,
			ClientID:            g.ClientID,
			ClientName:          g.ClientName,
			Category:            g.Category,
			Status:              g.Status,
			Stalled:             isStalled(g),
			Attempts:            g.Attempts,
			NextRetryAt:         g.NextRetryAt,
			Adopted:             g.DownloadURL == "" && g.ClientID != "",
			PlaceFailing:        isPlaceFailing(g),
			Reason:              g.Reason,
			PerformerName:       g.PerformerName,
			PlacedPath:          g.PlacedPath,
			PlaceError:          g.PlaceError,
			GrabbedAt:           g.GrabbedAt,
			UpdatedAt:           g.UpdatedAt,
			CompletedAt:         g.CompletedAt,
			PlacedAt:            g.PlacedAt,
			ConfirmedAt:         g.ConfirmedAt,
			Kind:                g.Kind,
			PackFiles:           g.PackFiles,
			PackIdentified:      g.PackIdentified,
			PackDeduped:         g.PackDeduped,
		})
	}
	// Badge packs that have pending review-mode duplicates. One grouped
	// query rather than per-row; best-effort — a failure just omits badges.
	if counts, derr := s.grabs.PendingDuplicateCounts(r.Context()); derr != nil {
		s.log.Warn("pending duplicate counts", "err", derr)
	} else if len(counts) > 0 {
		for i := range out {
			out[i].PendingDuplicates = counts[out[i].ID]
		}
	}

	s.enrichProgress(r, out)
	s.enrichSceneTitles(r, out)
	writeJSON(w, http.StatusOK, grabsResponse{Grabs: out, Totals: totals, MatchTotal: matchTotal})
}

// sceneTitleTTL: a found title is immutable, so this is effectively
// permanent for the daemon's lifetime. A miss (not found / StashDB
// unreachable) is retried after a short window so a fast-polling list
// doesn't hammer StashDB on every tick.
const (
	sceneTitleTTL     = 24 * time.Hour
	sceneTitleMissTTL = 2 * time.Minute
)

// enrichSceneTitles fills SceneTitle for grabs whose scene has 2+ attempts
// in this result — exactly the scenes the UI renders as an attempt-group,
// whose header would otherwise show a bare stashdb id. Single-attempt grabs
// don't get a header, so we skip them (no wasted StashDB lookups). Titles
// are cached (immutable), so steady-state polling resolves from memory with
// no StashDB calls.
func (s *Server) enrichSceneTitles(r *http.Request, out []grabOut) {
	// Count attempts per scene id (actual once confirmed, else predicted).
	counts := map[string]int{}
	for i := range out {
		if sid := grabSceneID(out[i]); sid != "" {
			counts[sid]++
		}
	}
	for i := range out {
		sid := grabSceneID(out[i])
		if sid == "" || counts[sid] < 2 {
			continue
		}
		if title := s.sceneTitle(r.Context(), sid); title != "" {
			out[i].SceneTitle = title
		}
	}
}

// grabSceneID returns the scene id a grab is tied to: the confirmed actual
// id when known, else the prediction. Matches the grouping key the UI uses.
func grabSceneID(g grabOut) string {
	if g.ActualStashDBID != "" {
		return g.ActualStashDBID
	}
	return g.PredictedStashDBID
}

// sceneTitle resolves a StashDB scene id to its title, memoised. Returns ""
// when StashDB isn't configured, the id isn't found, or the lookup fails —
// callers fall back to the id.
func (s *Server) sceneTitle(ctx context.Context, id string) string {
	s.sceneTitleMu.Lock()
	if s.sceneTitles == nil {
		s.sceneTitles = map[string]sceneTitleEntry{}
	}
	if e, ok := s.sceneTitles[id]; ok {
		ttl := sceneTitleTTL
		if e.title == "" {
			ttl = sceneTitleMissTTL
		}
		if time.Since(e.fetched) < ttl {
			s.sceneTitleMu.Unlock()
			return e.title
		}
	}
	s.sceneTitleMu.Unlock()

	title := ""
	if sdb := s.pool.StashDB(); sdb != nil {
		if sc, err := sdb.FindScene(ctx, id); err == nil && sc != nil {
			title = sc.Title
		}
	}
	s.sceneTitleMu.Lock()
	s.sceneTitles[id] = sceneTitleEntry{title: title, fetched: time.Now()}
	s.sceneTitleMu.Unlock()
	return title
}

// enrichProgress attaches live download progress to grabs that are
// still downloading/queued, pulled fresh from the clients (one qBit
// list + one SAB queue call, only when something is actually in
// flight). The 5s list-poll then drives a live progress readout in
// the UI without coupling the poller's slower cadence to it.
func (s *Server) enrichProgress(r *http.Request, out []grabOut) {
	anyActive := false
	for i := range out {
		if out[i].Status == "downloading" || out[i].Status == "queued" {
			anyActive = true
			break
		}
	}
	if !anyActive {
		return
	}

	qbitByHash := map[string]qbit.Torrent{}
	if qb := s.pool.Qbit(); qb != nil {
		if ts, err := qb.ListTorrents(r.Context(), qbit.ListOpts{Filter: "all"}); err == nil {
			for _, t := range ts {
				qbitByHash[strings.ToLower(t.Hash)] = t
			}
		}
	}
	sabByNzo := map[string]sabnzbd.Item{}
	if sb := s.pool.Sab(); sb != nil {
		if items, err := sb.Queue(r.Context()); err == nil {
			for _, it := range items {
				sabByNzo[it.NzoID] = it
			}
		}
	}

	for i := range out {
		if out[i].Status != "downloading" && out[i].Status != "queued" {
			continue
		}
		if out[i].ClientID == "" {
			continue
		}
		switch out[i].Client {
		case "qbit":
			if t, ok := qbitByHash[strings.ToLower(out[i].ClientID)]; ok {
				eta := t.Eta
				if eta >= qbitEtaUnknown {
					eta = 0
				}
				out[i].Progress = &grabProgress{
					Percent:  t.Progress * 100,
					SpeedBps: t.Dlspeed,
					EtaSecs:  eta,
				}
			}
		case "sabnzbd":
			if it, ok := sabByNzo[out[i].ClientID]; ok {
				out[i].Progress = &grabProgress{
					Percent:  it.Percentage,
					SpeedBps: it.SpeedBps,
					EtaSecs:  it.EtaSecs,
				}
			}
		}
	}
}
