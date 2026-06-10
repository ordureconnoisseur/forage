package api

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/configstore"
	"github.com/ordureconnoisseur/forager/internal/db"
	"golang.org/x/crypto/bcrypt"
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

// TestAdminAuthCookie verifies the <img>-auth path: the gate accepts a
// forage_token cookie that carries a VALID server-side session id (the new
// model — the cookie no longer carries the raw token). A made-up cookie
// value, or the raw token, is rejected.
func TestAdminAuthCookie(t *testing.T) {
	sentinel := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	store, err := configstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		bootstrap: config.BootstrapConfig{Config: config.Config{AdminToken: "secret"}},
		store:     store,
	}
	// Mint a real session; its id is the only cookie value that passes.
	validID, err := s.newSession()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		cookie string
		want   int
	}{
		{"valid session id reaches handler", validID, http.StatusOK},
		{"raw token is no longer a valid cookie → 401", "secret", http.StatusUnauthorized},
		{"made-up cookie → 401", "nope", http.StatusUnauthorized},
		{"empty cookie → 401", "", http.StatusUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
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

// TestPasswordGatesWithoutToken verifies a password-only daemon (no admin
// token) is gated: authRequired is true off the password alone, and a
// valid session cookie is the way through.
func TestPasswordGatesWithoutToken(t *testing.T) {
	sentinel := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	store, err := configstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	s := &Server{
		bootstrap: config.BootstrapConfig{Config: config.Config{
			Username:     "ethan",
			PasswordHash: string(hash),
		}},
		store: store,
	}
	if !s.authRequired() {
		t.Fatal("password set but authRequired() is false")
	}
	h := s.adminAuthMiddleware(sentinel)
	// No credential → 401.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/performers", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no-credential status = %d, want 401", rec.Code)
	}
	// Valid session cookie → through.
	id, err := s.newSession()
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/performers", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: id})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("valid-session status = %d, want 200", rec.Code)
	}
}

// TestPostLogin verifies the username+password login: a correct pair sets a
// forage_token cookie whose value is a random session id (NOT the
// password), and a wrong pair is 401 with no cookie. Also confirms the
// issued session is actually valid against the gate.
func TestPostLogin(t *testing.T) {
	store, err := configstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte("hunter2hunter2"), bcrypt.MinCost)
	s := &Server{
		bootstrap: config.BootstrapConfig{Config: config.Config{
			Username:     "ethan",
			PasswordHash: string(hash),
		}},
		store: store,
	}
	cookieValue := func(rec *httptest.ResponseRecorder) string {
		for _, c := range rec.Result().Cookies() {
			if c.Name == sessionCookieName {
				return c.Value
			}
		}
		return ""
	}

	cases := []struct {
		name string
		body string
		want int
		set  bool
	}{
		{"correct credentials", `{"username":"ethan","password":"hunter2hunter2"}`, http.StatusOK, true},
		{"wrong password", `{"username":"ethan","password":"nope"}`, http.StatusUnauthorized, false},
		{"wrong username", `{"username":"mallory","password":"hunter2hunter2"}`, http.StatusUnauthorized, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(c.body))
			rec := httptest.NewRecorder()
			s.postLogin(rec, req)
			if rec.Code != c.want {
				t.Fatalf("code = %d, want %d (body=%q)", rec.Code, c.want, rec.Body.String())
			}
			cv := cookieValue(rec)
			if c.set {
				if cv == "" {
					t.Fatal("expected a forage_token cookie, got none")
				}
				if cv == "hunter2hunter2" {
					t.Error("cookie value is the password — must be a session id")
				}
				if !s.sessionValid(cv) {
					t.Error("issued session id is not valid against the gate")
				}
			} else if cv != "" {
				t.Errorf("expected no cookie on failure, got %q", cv)
			}
		})
	}
}

// TestLogoutInvalidatesSession proves logout drops the session SERVER-SIDE:
// after DELETE /session, replaying the old cookie value is rejected by the
// gate (not merely cleared from the browser).
// TestLogoutClearsCookie: with stateless tokens, logout's job is to strip
// the credential from the browser by emitting an expiring Set-Cookie. It
// no longer revokes server-side (that's the documented trade-off), so we
// assert the clearing cookie rather than token invalidation.
func TestLogoutClearsCookie(t *testing.T) {
	store, err := configstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		bootstrap: config.BootstrapConfig{Config: config.Config{AdminToken: "secret"}},
		store:     store,
	}
	id, err := s.newSession()
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/session", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: id})
	rec := httptest.NewRecorder()
	s.deleteSession(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	var cleared *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			cleared = c
		}
	}
	if cleared == nil {
		t.Fatal("logout did not emit a forage_token cookie")
	}
	if cleared.MaxAge >= 0 || cleared.Value != "" {
		t.Errorf("logout cookie should expire the credential: MaxAge=%d value=%q",
			cleared.MaxAge, cleared.Value)
	}
}

// TestSessionTokenStateless covers the signed-cookie primitive: a genuine
// token verifies, a tampered/foreign/expired one doesn't, and — the whole
// point of this change — a token survives being re-checked under a server
// rebuilt with the SAME signing key (i.e. across a daemon restart), while
// a different key rejects it.
func TestSessionTokenStateless(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	s := &Server{sessionKey: key}

	good, _ := s.newSession()
	if !s.sessionValid(good) {
		t.Fatal("freshly minted token should verify")
	}

	// Tampered signature. Replace the last character with one that is
	// GUARANTEED different — a fixed "0" made the test a 1-in-64 flake,
	// since a genuine signature ending in '0' produced an identical,
	// untampered token that (correctly) verified.
	flip := "0"
	if good[len(good)-1] == '0' {
		flip = "1"
	}
	if s.sessionValid(good[:len(good)-1] + flip) {
		t.Error("token with a flipped signature byte should be rejected")
	}
	// Tampered payload (bump the expiry without re-signing).
	payload, sig, _ := strings.Cut(good, ".")
	exp, _ := strconv.ParseInt(payload, 10, 64)
	forged := strconv.FormatInt(exp+86400, 10) + "." + sig
	if s.sessionValid(forged) {
		t.Error("token with an unsigned, extended expiry should be rejected")
	}
	// Malformed shapes.
	for _, bad := range []string{"", "nopayload", ".sig", "payload.", "a.b.c"} {
		if s.sessionValid(bad) {
			t.Errorf("malformed token %q should be rejected", bad)
		}
	}
	// Expired but correctly signed.
	if s.sessionValid(s.signSession(time.Now().Add(-time.Minute))) {
		t.Error("expired token should be rejected")
	}

	// Restart survival: a server with the same persisted key still trusts
	// the cookie; a server with a different key does not.
	same := &Server{sessionKey: key}
	if !same.sessionValid(good) {
		t.Error("token should survive a restart that reloads the same signing key")
	}
	other := &Server{sessionKey: append([]byte{0xff}, key[1:]...)}
	if other.sessionValid(good) {
		t.Error("token must not verify under a different signing key")
	}
}

// TestLoadOrCreateSessionKey: the key is generated once and then stable
// across reloads from the same DB (so cookies persist), and two distinct
// DBs get distinct keys.
func TestLoadOrCreateSessionKey(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	k1 := loadOrCreateSessionKey(d, nil)
	if len(k1) != 32 {
		t.Fatalf("key length = %d, want 32", len(k1))
	}
	k2 := loadOrCreateSessionKey(d, nil)
	if !bytes.Equal(k1, k2) {
		t.Error("reloading the same DB should return the identical persisted key")
	}
	d2, err := db.Open(filepath.Join(t.TempDir(), "k2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if bytes.Equal(k1, loadOrCreateSessionKey(d2, nil)) {
		t.Error("a separate DB should get its own key")
	}
}

// TestRotateSessionKeyRevokesSessions: rotating the signing key (what a
// credential change triggers via postConfig) must invalidate every
// previously-minted cookie, keep newly-minted ones working, and persist
// the rotated key so the revocation survives a restart.
func TestRotateSessionKeyRevokesSessions(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "rot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := &Server{
		db:         d,
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessionKey: loadOrCreateSessionKey(d, nil),
	}

	old, _ := s.newSession()
	if !s.sessionValid(old) {
		t.Fatal("pre-rotation cookie should verify")
	}
	s.rotateSessionKey()
	if s.sessionValid(old) {
		t.Error("pre-rotation cookie must be revoked by the rotation")
	}
	fresh, _ := s.newSession()
	if !s.sessionValid(fresh) {
		t.Error("post-rotation cookie should verify")
	}
	if !bytes.Equal(s.currentSessionKey(), loadOrCreateSessionKey(d, nil)) {
		t.Error("rotated key was not persisted (a restart would un-revoke)")
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
