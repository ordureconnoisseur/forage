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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
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

// ── Stateless signed sessions ───────────────────────────────────────
//
// A web login issues a self-contained, HMAC-signed token of the form
// "<expiryUnix>.<hex(mac)>", where mac = HMAC-SHA256(sessionKey, domain +
// expiryUnix). The forage_token cookie carries this token (NOT the
// password or admin token). There is no server-side session table: a
// cookie is trusted iff its signature verifies and its embedded expiry is
// still in the future. Because sessionKey is persisted in the DB and
// reloaded on boot, cookies survive a daemon restart — a redeploy no
// longer logs everyone out. This is the *arrs' Data-Protection model.
//
// Trade-off vs the old in-memory map: there's no per-session revocation,
// so logout clears the cookie but can't forcibly kill a token that was
// already captured — it stays valid until its expiry. For a single-user
// private-tailnet daemon that's the standard stateless-cookie posture
// (it's how JWT/Data-Protection auth behaves too). The revocation lever
// is rotating sessionKey (rotateSessionKey), which kills every
// outstanding cookie at once; the daemon does that automatically when a
// login credential changes (postConfig).

// sessionDomain separates this HMAC's input from any other use of the
// same key, so a signature minted here can't be replayed in another
// context.
const sessionDomain = "forage-session.v1:"

// loadOrCreateSessionKey returns the persisted cookie-signing key from the
// meta table, generating and storing a fresh 32-byte CSPRNG key on first
// run. On any DB error it falls back to an ephemeral in-process key and
// logs a warning: auth still works, but cookies won't survive a restart
// (the pre-existing behaviour), which is strictly no worse than before.
// sessionKeyMetaKey is the meta-table row holding the signing key.
const sessionKeyMetaKey = "session_signing_key"

func loadOrCreateSessionKey(db *sql.DB, log *slog.Logger) []byte {
	var stored string
	err := db.QueryRow(`SELECT value FROM meta WHERE key = ?`, sessionKeyMetaKey).Scan(&stored)
	if err == nil {
		if b, derr := hex.DecodeString(stored); derr == nil && len(b) == 32 {
			return b
		}
		// Corrupt/short value — fall through and mint a fresh one.
	} else if !errors.Is(err, sql.ErrNoRows) {
		// Read FAILED (locked, I/O) — distinct from "no key yet". Don't
		// fall through: minting would overwrite a possibly-good stored key
		// and log every session out over a transient error. Degrade to an
		// ephemeral key for this process instead.
		if log != nil {
			log.Warn("could not read session signing key; using ephemeral key", "err", err)
		}
		return ephemeralSessionKey()
	}

	b := make([]byte, 32)
	if _, rerr := rand.Read(b); rerr != nil {
		if log != nil {
			log.Warn("could not generate session signing key", "err", rerr)
		}
		return ephemeralSessionKey()
	}
	if _, werr := db.Exec(`
		INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, sessionKeyMetaKey, hex.EncodeToString(b)); werr != nil && log != nil {
		log.Warn("could not persist session signing key; using ephemeral key", "err", werr)
	}
	return b
}

// ephemeralSessionKey mints a process-lifetime key for the degraded path
// where the DB can't store one. Cookies signed with it die on restart.
func ephemeralSessionKey() []byte {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return b
}

// currentSessionKey reads the signing key under the rotation lock.
func (s *Server) currentSessionKey() []byte {
	s.sessionKeyMu.RLock()
	defer s.sessionKeyMu.RUnlock()
	return s.sessionKey
}

// rotateSessionKey mints + persists a fresh signing key and swaps it in,
// which invalidates every outstanding session cookie at once — the
// revocation lever for stateless sessions. Called when a login credential
// changes (password set/cleared, API key changed) so a credential rotation
// also revokes existing logins. Keeps the old key if a new one can't be
// generated; if persisting fails the in-memory swap still happens (the
// revocation takes effect now) but a restart reverts to the stored key.
func (s *Server) rotateSessionKey() {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		s.log.Warn("session key rotation failed; existing sessions remain valid", "err", err)
		return
	}
	if _, err := s.db.Exec(`
		INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, sessionKeyMetaKey, hex.EncodeToString(b)); err != nil {
		s.log.Warn("could not persist rotated session key; revocation lasts until restart", "err", err)
	}
	s.sessionKeyMu.Lock()
	s.sessionKey = b
	s.sessionKeyMu.Unlock()
	s.log.Info("session signing key rotated; all existing sessions invalidated")
}

// signSession returns the cookie value for an expiry: "<expUnix>.<hexmac>".
func (s *Server) signSession(exp time.Time) string {
	payload := strconv.FormatInt(exp.Unix(), 10)
	mac := hmac.New(sha256.New, s.currentSessionKey())
	mac.Write([]byte(sessionDomain + payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil))
}

// newSession mints a signed token valid for sessionTTL. The (string,
// error) signature is retained for its callers; with stateless tokens it
// never errors.
func (s *Server) newSession() (string, error) {
	return s.signSession(time.Now().Add(sessionTTL)), nil
}

// sessionValid reports whether value is a well-formed, correctly-signed,
// unexpired session token. Empty/malformed values are never valid.
func (s *Server) sessionValid(value string) bool {
	payload, sig, ok := strings.Cut(value, ".")
	if !ok || payload == "" || sig == "" {
		return false
	}
	provided, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, s.currentSessionKey())
	mac.Write([]byte(sessionDomain + payload))
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return false
	}
	exp, err := strconv.ParseInt(payload, 10, 64)
	if err != nil {
		return false
	}
	return time.Now().Before(time.Unix(exp, 0))
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
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}

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
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
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

// deleteSession is logout: it overwrites the forage_token cookie with an
// expired one (it's HttpOnly, so client JS can't clear it itself), which
// removes the credential from the browser. With stateless tokens there's
// no server-side record to delete; a token already captured stays valid
// until its expiry (rotate sessionKey to revoke en masse). Public +
// unauthenticated: removing your own browser's credential is harmless, and
// a locked-out client has nothing valid to present anyway.
func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
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
