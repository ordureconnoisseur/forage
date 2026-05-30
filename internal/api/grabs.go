package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/ordureconnoisseur/forager/internal/qbit"
	"github.com/ordureconnoisseur/forager/internal/sabnzbd"
)

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
	ID                  int64         `json:"id"`
	PredictedStashDBID  string        `json:"predicted_stashdb_id,omitempty"`
	PredictedConfidence float64       `json:"predicted_confidence,omitempty"`
	ActualStashDBID     string        `json:"actual_stashdb_id,omitempty"`
	ReleaseTitle        string        `json:"release_title"`
	ReleaseSize         int64         `json:"release_size,omitempty"`
	ReleaseIndexer      string        `json:"release_indexer,omitempty"`
	DownloadURL         string        `json:"download_url,omitempty"`
	Client              string        `json:"client,omitempty"`
	ClientID            string        `json:"client_id,omitempty"`
	ClientName          string        `json:"client_name,omitempty"`
	Category            string        `json:"category,omitempty"`
	Status              string        `json:"status"`
	Reason              string        `json:"reason,omitempty"`
	PerformerName       string        `json:"performer_name,omitempty"`
	PlacedPath          string        `json:"placed_path,omitempty"`
	PlaceError          string        `json:"place_error,omitempty"`
	GrabbedAt           int64         `json:"grabbed_at"`
	UpdatedAt           int64         `json:"updated_at"`
	CompletedAt         int64         `json:"completed_at,omitempty"`
	PlacedAt            int64         `json:"placed_at,omitempty"`
	ConfirmedAt         int64         `json:"confirmed_at,omitempty"`
	Progress            *grabProgress `json:"progress,omitempty"`
	// Pack fields — kind is "pack" for performer-pack grabs, with the
	// progress counters; "single" / omitted otherwise.
	Kind           string `json:"kind,omitempty"`
	PackFiles      int    `json:"pack_files,omitempty"`
	PackIdentified int    `json:"pack_identified,omitempty"`
	PackDeduped    int    `json:"pack_deduped,omitempty"`
}

type grabsResponse struct {
	Grabs  []grabOut      `json:"grabs"`
	Totals map[string]int `json:"totals"`
}

// getGrabs returns the most-recent grabs with status totals for the
// UI's Grabs tab. Filterable by status; defaults to "any".
func (s *Server) getGrabs(w http.ResponseWriter, r *http.Request) {
	if s.grabs == nil {
		writeJSON(w, http.StatusOK, grabsResponse{Grabs: []grabOut{}, Totals: map[string]int{}})
		return
	}
	q := r.URL.Query()
	status := q.Get("status")
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))

	rows, err := s.grabs.List(r.Context(), status, limit, offset)
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
	s.enrichProgress(r, out)
	writeJSON(w, http.StatusOK, grabsResponse{Grabs: out, Totals: totals})
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
