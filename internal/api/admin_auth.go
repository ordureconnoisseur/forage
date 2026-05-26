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

func (s *Server) adminAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := s.bootstrap.AdminToken
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
