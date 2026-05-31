// adminAuthMiddleware gates /config* routes with an optional Bearer
// token. When FORAGER_ADMIN_TOKEN is unset (the common tailnet-only
// case) the middleware is a no-op; when set, requests without a
// matching Authorization header get 401.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// sessionCookieName carries the admin token for requests a browser can't
// attach an Authorization header to — chiefly <img> loads (performer
// portraits, scene screenshots proxied through the daemon). Mirrors how
// Sonarr/Radarr protect their media-cover routes once auth is enabled.
const sessionCookieName = "forage_token"

// effectiveAdminToken is the token the gate enforces: the UI-managed
// config.json value when set, else the env FORAGER_ADMIN_TOKEN. Mirrors
// Compose's precedence (a non-empty stored value overrides env) without
// building the full Sources map on every request.
func (s *Server) effectiveAdminToken() string {
	if st := s.store.Get().AdminToken; st != nil && *st != "" {
		return *st
	}
	return s.bootstrap.AdminToken
}

func (s *Server) adminAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := s.effectiveAdminToken()
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		if s.requestHasValidToken(r, token) {
			next.ServeHTTP(w, r)
			return
		}
		writeErr(w, http.StatusUnauthorized, "admin token required")
	})
}

// requestHasValidToken accepts the token from the Authorization: Bearer
// header (the fetch() path) OR the forage_token cookie (the <img>/navigation
// path — browsers won't attach an Authorization header to an image request).
func (s *Server) requestHasValidToken(r *http.Request, token string) bool {
	auth := r.Header.Get("Authorization")
	if provided := strings.TrimPrefix(auth, "Bearer "); provided != auth &&
		subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1 {
		return true
	}
	if c, err := r.Cookie(sessionCookieName); err == nil &&
		subtle.ConstantTimeCompare([]byte(c.Value), []byte(token)) == 1 {
		return true
	}
	return false
}

// postSession establishes the forage_token cookie so same-origin <img>
// requests authenticate without an Authorization header. Public route: when
// no admin token is configured it's a 200 no-op (open mode); otherwise the
// posted token must match before the cookie is set.
func (s *Server) postSession(w http.ResponseWriter, r *http.Request) {
	token := s.effectiveAdminToken()
	if token == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "required": false})
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if subtle.ConstantTimeCompare([]byte(body.Token), []byte(token)) != 1 {
		writeErr(w, http.StatusUnauthorized, "invalid token")
		return
	}
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   7 * 24 * 60 * 60,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "required": true})
}
