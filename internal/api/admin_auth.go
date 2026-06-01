// adminAuthMiddleware gates every data/action route once the daemon has
// a credential configured. forage uses the two-credential model the
// *arrs use:
//
//   - Username + password → the human web-UI login (POST /login). On
//     success the daemon issues a random server-side session id and sets
//     it as the forage_token cookie; the cookie carries the session id,
//     never the password.
//   - Admin token ("the API key") → for programmatic clients, sent as
//     `Authorization: Bearer <token>`. It also backs the legacy
//     token→cookie handshake (POST /session) for key-only clients whose
//     <img> loads can't attach an Authorization header.
//
// When neither a password nor a token is set the middleware is a no-op
// (the common private-tailnet case): the daemon is open.
package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// sessionCookieName carries a web-login session id for requests a browser
// can't attach an Authorization header to — chiefly <img> loads (performer
// portraits, scene screenshots proxied through the daemon) and navigation.
// Mirrors how Sonarr/Radarr protect their media-cover routes once auth is
// enabled.
const sessionCookieName = "forage_token"

// sessionTTL is how long a web-login session stays valid. Matches the
// cookie MaxAge so the two expire together.
const sessionTTL = 7 * 24 * time.Hour

// effectiveAdminToken is the API key the gate accepts as a Bearer
// credential: the UI-managed config.json value when set, else the env
// FORAGER_ADMIN_TOKEN. Mirrors Compose's precedence (a non-empty stored
// value overrides env) without building the full Sources map on every
// request.
func (s *Server) effectiveAdminToken() string {
	if st := s.store.Get().AdminToken; st != nil && *st != "" {
		return *st
	}
	return s.bootstrap.AdminToken
}

// effectiveUsername / effectivePasswordHash resolve the web-login
// credentials with the same JSON-over-env precedence as the API key.
func (s *Server) effectiveUsername() string {
	if u := s.store.Get().Username; u != nil && *u != "" {
		return *u
	}
	return s.bootstrap.Username
}

func (s *Server) effectivePasswordHash() string {
	if h := s.store.Get().PasswordHash; h != nil && *h != "" {
		return *h
	}
	return s.bootstrap.PasswordHash
}

// authRequired reports whether the gate enforces a credential: true when
// EITHER a password OR an API key is configured. When neither is set the
// daemon is open (current behaviour preserved).
func (s *Server) authRequired() bool {
	return s.effectivePasswordHash() != "" || s.effectiveAdminToken() != ""
}

func (s *Server) adminAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authRequired() {
			next.ServeHTTP(w, r)
			return
		}
		if s.requestAuthorized(r) {
			next.ServeHTTP(w, r)
			return
		}
		writeErr(w, http.StatusUnauthorized, "authentication required")
	})
}

// requestAuthorized accepts a request via the two credential paths, in
// order:
//  1. Authorization: Bearer <adminToken> — the API-key path (clients),
//     when an admin token is configured. Constant-time compared.
//  2. forage_token cookie that is a valid (unexpired) session id — the web
//     path, incl. <img> loads and navigation.
func (s *Server) requestAuthorized(r *http.Request) bool {
	if token := s.effectiveAdminToken(); token != "" {
		auth := r.Header.Get("Authorization")
		if provided := strings.TrimPrefix(auth, "Bearer "); provided != auth &&
			subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1 {
			return true
		}
	}
	if c, err := r.Cookie(sessionCookieName); err == nil && s.sessionValid(c.Value) {
		return true
	}
	return false
}

// ── Session store ───────────────────────────────────────────────────
//
// A web login issues a cryptographically-random session id, tracked here
// with a 7-day expiry. The forage_token cookie carries this id (NOT the
// password or token). In-memory: a daemon restart drops every session and
// users re-login — conventional and acceptable for a single-user daemon.

// newSession mints a random session id, records its expiry, and returns
// the id (hex). 32 bytes of CSPRNG → 64 hex chars, unguessable.
func (s *Server) newSession() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b)
	s.sessionMu.Lock()
	if s.sessions == nil {
		s.sessions = map[string]time.Time{}
	}
	s.sessions[id] = time.Now().Add(sessionTTL)
	s.sessionMu.Unlock()
	return id, nil
}

// sessionValid reports whether id names a live (unexpired) session,
// pruning it if it has expired. Empty ids are never valid.
func (s *Server) sessionValid(id string) bool {
	if id == "" {
		return false
	}
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	exp, ok := s.sessions[id]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.sessions, id)
		return false
	}
	return true
}

// dropSession invalidates a session id server-side (logout). After this,
// presenting the same cookie value gets 401 — the credential is gone, not
// merely cleared from the browser.
func (s *Server) dropSession(id string) {
	if id == "" {
		return
	}
	s.sessionMu.Lock()
	delete(s.sessions, id)
	s.sessionMu.Unlock()
}

// setSessionCookie writes the forage_token cookie carrying a session id.
// Secure on https (direct TLS or behind a proxy that forwarded https),
// HttpOnly so client JS can't read it, SameSite=Lax, 7-day MaxAge.
func setSessionCookie(w http.ResponseWriter, r *http.Request, value string) {
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   int(sessionTTL / time.Second),
	})
}

// ── Endpoints ───────────────────────────────────────────────────────

// postLogin is the human web login: bcrypt-compare the posted
// username+password against the configured credentials and, on success,
// issue a session + set the forage_token cookie. Public route. Runs the
// bcrypt compare regardless of whether the username matched so the timing
// (and the error) doesn't leak which half was wrong.
func (s *Server) postLogin(w http.ResponseWriter, r *http.Request) {
	hash := s.effectivePasswordHash()
	if hash == "" {
		// No password configured — password login isn't available on this
		// daemon (it may still accept the API key via POST /session).
		writeErr(w, http.StatusUnauthorized, "password login is not enabled")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	userOK := subtle.ConstantTimeCompare([]byte(body.Username), []byte(s.effectiveUsername())) == 1
	passOK := bcrypt.CompareHashAndPassword([]byte(hash), []byte(body.Password)) == nil
	if !userOK || !passOK {
		writeErr(w, http.StatusUnauthorized, "incorrect username or password")
		return
	}

	id, err := s.newSession()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not start session")
		return
	}
	setSessionCookie(w, r, id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// postSession is the API-key→cookie handshake for clients that
// authenticate by Bearer token rather than password: it validates the
// posted token, then issues a session id and sets the forage_token cookie
// so the client's same-origin <img> requests pass the gate. Public route:
// when no admin token is configured it's a 200 no-op (open mode, or a
// password-only daemon where the cookie comes from /login instead).
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
	id, err := s.newSession()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not start session")
		return
	}
	setSessionCookie(w, r, id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "required": true})
}

// deleteSession is logout: it invalidates the session id server-side (so
// the old cookie value can never be replayed) AND overwrites the cookie
// with an expired one (it's HttpOnly, so client JS can't clear it itself).
// Public + unauthenticated: removing your own browser's credential is
// harmless, and a locked-out client has nothing valid to present anyway.
func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		s.dropSession(c.Value)
	}
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
