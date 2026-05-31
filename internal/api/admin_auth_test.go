package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/configstore"
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
	store, err := configstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Server{
				bootstrap: config.BootstrapConfig{Config: config.Config{AdminToken: c.token}},
				store:     store,
			}
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

// TestAdminAuthCookie verifies the <img>-auth path: the gate accepts the
// token from the forage_token cookie (which browsers attach to image
// requests) as well as the Authorization header.
func TestAdminAuthCookie(t *testing.T) {
	sentinel := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	store, err := configstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		cookie string
		want   int
	}{
		{"correct cookie reaches handler", "secret", http.StatusOK},
		{"wrong cookie → 401", "nope", http.StatusUnauthorized},
		{"empty cookie → 401", "", http.StatusUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Server{
				bootstrap: config.BootstrapConfig{Config: config.Config{AdminToken: "secret"}},
				store:     store,
			}
			h := s.adminAuthMiddleware(sentinel)
			req := httptest.NewRequest(http.MethodGet, "/img/performer/1", nil)
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: c.cookie})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Errorf("status = %d, want %d", rec.Code, c.want)
			}
		})
	}
}

// TestPostSession verifies the cookie handshake: no token → no-op; correct
// token → Set-Cookie; wrong token → 401 and no cookie.
func TestPostSession(t *testing.T) {
	store, err := configstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hasCookie := func(rec *httptest.ResponseRecorder) bool {
		for _, c := range rec.Result().Cookies() {
			if c.Name == sessionCookieName && c.Value != "" {
				return true
			}
		}
		return false
	}

	// No token configured → 200, required:false, no cookie.
	t.Run("open daemon is a no-op", func(t *testing.T) {
		s := &Server{store: store}
		req := httptest.NewRequest(http.MethodPost, "/session", strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		s.postSession(rec, req)
		if rec.Code != http.StatusOK || hasCookie(rec) {
			t.Errorf("open daemon: code=%d cookie=%v, want 200 + no cookie", rec.Code, hasCookie(rec))
		}
	})

	// Token configured: correct → cookie; wrong → 401, no cookie.
	for _, c := range []struct {
		name   string
		body   string
		want   int
		cookie bool
	}{
		{"correct token sets cookie", `{"token":"secret"}`, http.StatusOK, true},
		{"wrong token → 401", `{"token":"nope"}`, http.StatusUnauthorized, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := &Server{
				bootstrap: config.BootstrapConfig{Config: config.Config{AdminToken: "secret"}},
				store:     store,
			}
			req := httptest.NewRequest(http.MethodPost, "/session", strings.NewReader(c.body))
			rec := httptest.NewRecorder()
			s.postSession(rec, req)
			if rec.Code != c.want {
				t.Errorf("code = %d, want %d", rec.Code, c.want)
			}
			if hasCookie(rec) != c.cookie {
				t.Errorf("cookie set = %v, want %v", hasCookie(rec), c.cookie)
			}
		})
	}
}

// TestDeleteSession verifies logout clears the cookie: the response carries
// a forage_token cookie with a negative MaxAge (expiry in the past), which
// instructs the browser to delete it. Works regardless of configured token.
func TestDeleteSession(t *testing.T) {
	store, err := configstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		bootstrap: config.BootstrapConfig{Config: config.Config{AdminToken: "secret"}},
		store:     store,
	}
	req := httptest.NewRequest(http.MethodDelete, "/session", nil)
	rec := httptest.NewRecorder()
	s.deleteSession(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Errorf("expected an expiring %s cookie, got none", sessionCookieName)
	}
}

// TestEffectiveAdminToken checks the precedence the gate relies on: a
// non-empty UI-managed (config.json) token overrides the env token; an
// unset stored token falls through to env.
func TestEffectiveAdminToken(t *testing.T) {
	str := func(s string) *string { return &s }
	cases := []struct {
		name   string
		env    string
		stored *string
		want   string
	}{
		{"neither set", "", nil, ""},
		{"env only", "env-tok", nil, "env-tok"},
		{"stored only", "", str("ui-tok"), "ui-tok"},
		{"stored overrides env", "env-tok", str("ui-tok"), "ui-tok"},
		{"empty stored falls through to env", "env-tok", str(""), "env-tok"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store, err := configstore.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if c.stored != nil {
				if err := store.Set(configstore.Patch{AdminToken: c.stored}); err != nil {
					t.Fatal(err)
				}
			}
			s := &Server{
				bootstrap: config.BootstrapConfig{Config: config.Config{AdminToken: c.env}},
				store:     store,
			}
			if got := s.effectiveAdminToken(); got != c.want {
				t.Errorf("effectiveAdminToken() = %q, want %q", got, c.want)
			}
		})
	}
}
