package api

import (
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ordureconnoisseur/forager/internal/config"
)

// imageProxyClient streams Stash media. Short timeout — images are small and
// local; a hung Stash shouldn't hold a connection for the GraphQL client's
// full 60s.
var imageProxyClient = &http.Client{Timeout: 20 * time.Second}

// proxyStashImage streams an image from the user's Stash through the daemon,
// authenticating server-side with the stored Stash API key. This is what lets
// the standalone web app (served from the daemon's own origin) render
// performer portraits and scene screenshots without the browser ever holding
// Stash credentials or reaching Stash directly. These routes sit under
// adminAuthMiddleware, so when an admin token is set the browser must carry it
// — via the forage_token cookie, since <img> tags can't send an Authorization
// header.
func (s *Server) proxyStashImage(w http.ResponseWriter, r *http.Request, stashPath string) {
	cfg, _ := config.Compose(s.bootstrap, s.store.Get())
	if cfg.StashURL == "" {
		writeErr(w, http.StatusNotFound, "stash not configured")
		return
	}
	url := strings.TrimRight(cfg.StashURL, "/") + stashPath
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "build request")
		return
	}
	if cfg.StashAPIKey != "" {
		req.Header.Set("ApiKey", cfg.StashAPIKey)
	}
	resp, err := imageProxyClient.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "stash unreachable")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		writeErr(w, resp.StatusCode, "stash image unavailable")
		return
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	// Portraits/screenshots are effectively immutable for a session; let the
	// browser cache them. private — they're behind the admin token.
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, resp.Body)
}

func (s *Server) getPerformerImage(w http.ResponseWriter, r *http.Request) {
	s.proxyStashImage(w, r, "/performer/"+chi.URLParam(r, "id")+"/image")
}

func (s *Server) getSceneScreenshot(w http.ResponseWriter, r *http.Request) {
	s.proxyStashImage(w, r, "/scene/"+chi.URLParam(r, "id")+"/screenshot")
}
