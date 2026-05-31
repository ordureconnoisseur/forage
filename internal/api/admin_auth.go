// adminAuthMiddleware gates /config* routes with an optional Bearer
// token. When FORAGER_ADMIN_TOKEN is unset (the common tailnet-only
// case) the middleware is a no-op; when set, requests without a
// matching Authorization header get 401.
package api

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

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
		auth := r.Header.Get("Authorization")
		provided := strings.TrimPrefix(auth, "Bearer ")
		if provided == auth || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "admin token required")
			return
		}
		next.ServeHTTP(w, r)
	})
}
