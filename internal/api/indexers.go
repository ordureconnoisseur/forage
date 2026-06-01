package api

import (
	"context"
	"net/http"
	"time"
)

// indexerOut is the slim shape the friendly release-prefs UI needs to
// render its drag-to-rank indexer list: a name, the protocol badge
// (torrent / usenet), and whether the indexer is enabled in Prowlarr.
type indexerOut struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Enabled  bool   `json:"enabled"`
}

// getIndexers lists the user's configured Prowlarr indexers for the
// friendly release-ranking UI. When Prowlarr isn't configured we return
// 200 with an empty list (not 503) so the UI can simply hide the indexer
// section and still offer resolution + protocol preferences.
func (s *Server) getIndexers(w http.ResponseWriter, r *http.Request) {
	out := []indexerOut{}
	prowlarrC := s.pool.Prowlarr()
	if prowlarrC == nil {
		writeJSON(w, http.StatusOK, map[string]any{"indexers": out})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	idx, err := prowlarrC.Indexers(ctx)
	if err != nil {
		s.log.Error("getIndexers", "err", err)
		writeErr(w, http.StatusBadGateway, "could not list Prowlarr indexers: "+err.Error())
		return
	}
	for _, i := range idx {
		out = append(out, indexerOut{Name: i.Name, Protocol: i.Protocol, Enabled: i.Enable})
	}
	writeJSON(w, http.StatusOK, map[string]any{"indexers": out})
}
