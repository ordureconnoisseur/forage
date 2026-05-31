package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/config"
)

// TestAdminAuthMiddleware verifies the gate that now fronts every data and
// action route: no token configured → open (backward compatible); token
// configured → only a matching Bearer reaches the handler.
func TestAdminAuthMiddleware(t *testing.T) {
	sentinel := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("reached"))
	})

	cases := []struct {
		name       string
		token      string // configured FORAGER_ADMIN_TOKEN ("" = unset)
		authHeader string
		want       int
	}{
		{"unset token is a no-op (open)", "", "", http.StatusOK},
		{"unset token ignores any header", "", "Bearer whatever", http.StatusOK},
		{"set token, no header → 401", "secret", "", http.StatusUnauthorized},
		{"set token, wrong token → 401", "secret", "Bearer nope", http.StatusUnauthorized},
		{"set token, missing Bearer prefix → 401", "secret", "secret", http.StatusUnauthorized},
		{"set token, correct Bearer → reaches handler", "secret", "Bearer secret", http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Server{bootstrap: config.BootstrapConfig{Config: config.Config{AdminToken: c.token}}}
			h := s.adminAuthMiddleware(sentinel)
			req := httptest.NewRequest(http.MethodGet, "/performers", nil)
			if c.authHeader != "" {
				req.Header.Set("Authorization", c.authHeader)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Errorf("status = %d, want %d (body=%q)", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}
