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
	id, ok := numericID(chi.URLParam(r, "id"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	s.proxyStashImage(w, r, "/performer/"+id+"/image")
}

func (s *Server) getSceneScreenshot(w http.ResponseWriter, r *http.Request) {
	id, ok := numericID(chi.URLParam(r, "id"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	s.proxyStashImage(w, r, "/scene/"+id+"/screenshot")
}

// numericID constrains a path param that gets concatenated into a
// Stash-side URL to a plain integer (Stash ids are integers). The proxy
// attaches the stored ApiKey, so the id must not be able to steer the
// request anywhere but the two image endpoints.
func numericID(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return "", false
		}
	}
	return s, true
}
