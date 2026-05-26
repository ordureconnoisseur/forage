package api

import (
	"net/http"
	"strconv"
)

type grabOut struct {
	ID                  int64   `json:"id"`
	PredictedStashDBID  string  `json:"predicted_stashdb_id,omitempty"`
	PredictedConfidence float64 `json:"predicted_confidence,omitempty"`
	ActualStashDBID     string  `json:"actual_stashdb_id,omitempty"`
	ReleaseTitle        string  `json:"release_title"`
	ReleaseSize         int64   `json:"release_size,omitempty"`
	ReleaseIndexer      string  `json:"release_indexer,omitempty"`
	DownloadURL         string  `json:"download_url,omitempty"`
	Client              string  `json:"client,omitempty"`
	ClientID            string  `json:"client_id,omitempty"`
	ClientName          string  `json:"client_name,omitempty"`
	Category            string  `json:"category,omitempty"`
	Status              string  `json:"status"`
	Reason              string  `json:"reason,omitempty"`
	PerformerName       string  `json:"performer_name,omitempty"`
	PlacedPath          string  `json:"placed_path,omitempty"`
	PlaceError          string  `json:"place_error,omitempty"`
	GrabbedAt           int64   `json:"grabbed_at"`
	UpdatedAt           int64   `json:"updated_at"`
	CompletedAt         int64   `json:"completed_at,omitempty"`
	PlacedAt            int64   `json:"placed_at,omitempty"`
	ConfirmedAt         int64   `json:"confirmed_at,omitempty"`
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
		})
	}
	writeJSON(w, http.StatusOK, grabsResponse{Grabs: out, Totals: totals})
}
