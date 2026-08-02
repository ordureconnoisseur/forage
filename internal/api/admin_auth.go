// adminAuthMiddleware gates every data/action route once the daemon has
// a credential configured. forage uses the two-credential model the
// *arrs use:
//
//   - Username + password → the human web-UI login (POST /login). On
//     success the daemon issues a random server-side session id and sets
//     it as the forage_token cookie; the cookie carries the session id,
//     never the password.
//   - Admin token ("the API key") or the Stash API key → for programmatic
//     clients, sent as `Authorization: Bearer <token>`. Either also backs
//     the token→cookie handshake (POST /session) for key-only clients whose
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
	"io"
	"log/slog"
	"net"
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

// ── Trusted proxies ─────────────────────────────────────────────────
//
// X-Forwarded-* headers are client input. They decide two things here —
// whether the session cookie gets the Secure flag, and which address the
// auth log attributes an attempt to — so they're honoured only when the
// request actually arrived from an address a reverse proxy could
// plausibly occupy: loopback, RFC1918, ULA, or link-local. That's the
// same set Sonarr allows via ForwardedHeadersOptions.KnownIPNetworks.
//
// CGNAT (100.64.0.0/10, where Tailscale addresses live) is deliberately
// NOT trusted. forage's usual arrangement puts `tailscale serve` in the
// same netns proxying to 127.0.0.1, so the proxy reads as loopback and
// is covered; trusting the whole CGNAT range instead would let any
// tailnet peer forge its own identity in the auth log.

// remoteHost strips the port from a RemoteAddr, tolerating the
// bare-address form some transports produce.
func remoteHost(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// fromTrustedProxy reports whether the request's transport-level peer sits
// in a network a reverse proxy could occupy. False for anything routable
// from the wider internet, so a direct client's forwarded headers are
// ignored rather than believed.
func fromTrustedProxy(r *http.Request) bool {
	ip := net.ParseIP(remoteHost(r.RemoteAddr))
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// clientIP resolves the address to attribute a request to, plus the direct
// transport peer. Behind a trusted proxy the left-most X-Forwarded-For
// entry wins; otherwise the header is ignored entirely. The entry is
// re-serialised through net.ParseIP, so a malformed or injected value
// never reaches the log as-is.
//
// peer is returned alongside and logged whenever it differs, so even a
// forged header can't erase the ground truth of who actually connected.
// That's the property a fail2ban jail needs and the reason this returns
// two values rather than collapsing to one.
func clientIP(r *http.Request) (ip, peer string) {
	peer = remoteHost(r.RemoteAddr)
	if !fromTrustedProxy(r) {
		return peer, peer
	}
	first, _, _ := strings.Cut(r.Header.Get("X-Forwarded-For"), ",")
	if parsed := net.ParseIP(strings.TrimSpace(first)); parsed != nil {
		return parsed.String(), peer
	}
	return peer, peer
}

// cookieSecure decides the session cookie's Secure flag: direct TLS, or a
// trusted proxy reporting it terminated https.
func cookieSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return fromTrustedProxy(r) &&
		strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// ── Auth audit log ──────────────────────────────────────────────────
//
// Every credential check that fails, and every one that succeeds, emits a
// line carrying the client address. forage has no login lockout (neither
// do the *arrs), so this log IS the control that makes a password spray
// visible and gives fail2ban something to match on. Records look like:
//
//	msg=auth-failure ip=192.168.50.9 username=admin
//	msg=auth-unauthorized ip=100.64.1.2 peer=127.0.0.1 method=GET path=/config
//
// so a jail can key on `msg=auth-failure .* ip=<HOST>`.

// discardLog backstops the handful of unit tests that build a bare Server
// without a logger; auditing must never be the thing that panics a request.
var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

func (s *Server) authLogger() *slog.Logger {
	if s.log == nil {
		return discardLog
	}
	return s.log
}

// maxLoggedLen bounds attacker-controlled strings (usernames, paths) so a
// flood of oversized values can't bloat the log. slog's handlers already
// quote-escape control characters, which covers injection.
const maxLoggedLen = 96

func logSafe(v string) string {
	if len(v) > maxLoggedLen {
		return v[:maxLoggedLen] + "..."
	}
	return v
}

// logAuth emits one audit record for an authentication event.
func (s *Server) logAuth(r *http.Request, level slog.Level, event string, extra ...any) {
	ip, peer := clientIP(r)
	args := []any{"ip", ip}
	if peer != ip {
		args = append(args, "peer", peer)
	}
	s.authLogger().Log(r.Context(), level, event, append(args, extra...)...)
}

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

// effectiveStashAPIKey resolves the configured Stash API key (the key the
// daemon already holds to act on Stash's behalf), JSON-over-env. It's
// accepted as a Bearer credential so a same-origin Stash plugin (e.g.
// binge), which can read this key live via Stash's GraphQL under the
// user's session, can drive forage without a separate token to paste or
// store. "Can talk to Stash" == "can talk to forage"; rotating the Stash
// key rotates this access too.
func (s *Server) effectiveStashAPIKey() string {
	if k := s.store.Get().StashAPIKey; k != nil && *k != "" {
		return *k
	}
	return s.bootstrap.StashAPIKey
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
			// Open daemon: no credential is checked, so there is nothing to
			// guess and nothing to throttle.
			next.ServeHTTP(w, r)
			return
		}
		// This is the path every Bearer client authenticates on — the API
		// key, the Stash key, and the session cookie are all compared here
		// — so this is where the throttle has to be. Fronting only /login
		// and /session leaves token guessing unlimited.
		if s.throttled(scopeGate, w, r) {
			return
		}
		if s.requestAuthorized(r) {
			s.noteAuthSuccess(scopeGate, r)
			next.ServeHTTP(w, r)
			return
		}
		s.noteAuthFailure(scopeGate, r)
		// Info, not Warn: a browser whose cookie just expired trips this
		// on every poll, and that shouldn't read as an attack. The
		// credential-check failures below are the Warn-level signal.
		s.logAuth(r, slog.LevelInfo, "auth-unauthorized",
			"method", r.Method, "path", logSafe(r.URL.Path))
		writeErr(w, http.StatusUnauthorized, "authentication required")
	})
}

// requestAuthorized accepts a request via these credential paths, in
// order:
//  1. Authorization: Bearer <adminToken> — the API-key path (clients),
//     when an admin token is configured. Constant-time compared.
//  2. Authorization: Bearer <stashApiKey> — the same-Stash-trust path: a
//     plugin that can read Stash's own API key (authorized by the user's
//     Stash session) drives forage with it, no separate token needed.
//  3. forage_token cookie that is a valid (unexpired) session id — the web
//     path, incl. <img> loads and navigation.
func (s *Server) requestAuthorized(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if provided := strings.TrimPrefix(auth, "Bearer "); provided != auth {
		if token := s.effectiveAdminToken(); token != "" &&
			subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1 {
			return true
		}
		if key := s.effectiveStashAPIKey(); key != "" &&
			subtle.ConstantTimeCompare([]byte(provided), []byte(key)) == 1 {
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
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   cookieSecure(r),
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
	// Ahead of the hash lookup and the bcrypt compare: over budget, the
	// guess is not evaluated at all, which is what stops both the guessing
	// and the ~100ms of bcrypt each guess would otherwise cost the daemon.
	if s.throttled(scopeLogin, w, r) {
		return
	}
	hash := s.effectivePasswordHash()
	if hash == "" {
		// No password configured — password login isn't available on this
		// daemon (it may still accept the API key via POST /session).
		s.noteAuthFailure(scopeLogin, r)
		s.logAuth(r, slog.LevelInfo, "auth-failure", "method", "password",
			"reason", "password login not enabled")
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
		s.noteAuthFailure(scopeLogin, r)
		// The attempted username is logged (spray detection needs it) but
		// NOT which half was wrong — that stays as undisclosed in the log
		// as it is in the response.
		s.logAuth(r, slog.LevelWarn, "auth-failure", "method", "password",
			"username", logSafe(body.Username))
		writeErr(w, http.StatusUnauthorized, "incorrect username or password")
		return
	}
	// Getting it right clears the record, so a run of mistypes followed by
	// the correct password leaves no wait behind.
	s.noteAuthSuccess(scopeLogin, r)

	id, err := s.newSession()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not start session")
		return
	}
	setSessionCookie(w, r, id)
	s.logAuth(r, slog.LevelInfo, "auth-success", "method", "password",
		"username", logSafe(body.Username))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// postSession is the API-key→cookie handshake for clients that
// authenticate by Bearer token rather than password: it validates the
// posted token, then issues a session id and sets the forage_token cookie
// so the client's same-origin <img> requests pass the gate. It accepts the
// same two key credentials as requestAuthorized — the admin token and the
// Stash API key — because a Stash-key client (binge) whose API calls pass
// the gate still needs the cookie for <img> loads, which can't carry a
// Bearer header; on a password-gated daemon with no admin token the Stash
// key is the ONLY way such a client can mint one. Public route: when auth
// isn't enforced it's a 200 no-op (open mode; nothing gates images).
func (s *Server) postSession(w http.ResponseWriter, r *http.Request) {
	if !s.authRequired() {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "required": false})
		return
	}
	// The cheapest guessing surface in the daemon: no bcrypt in its path,
	// and a success hands back a 7-day cookie.
	if s.throttled(scopeSession, w, r) {
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	// Run both compares unconditionally so the timing doesn't leak which
	// credential (if either) matched — same discipline as postLogin.
	token, stashKey := s.effectiveAdminToken(), s.effectiveStashAPIKey()
	tokenOK := token != "" &&
		subtle.ConstantTimeCompare([]byte(body.Token), []byte(token)) == 1
	keyOK := stashKey != "" &&
		subtle.ConstantTimeCompare([]byte(body.Token), []byte(stashKey)) == 1
	if !tokenOK && !keyOK {
		s.noteAuthFailure(scopeSession, r)
		// No token value in the log, not even a prefix: it's a bearer
		// secret, and a near-miss would be as good as the real thing.
		s.logAuth(r, slog.LevelWarn, "auth-failure", "method", "token")
		writeErr(w, http.StatusUnauthorized, "invalid token")
		return
	}
	s.noteAuthSuccess(scopeSession, r)
	id, err := s.newSession()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not start session")
		return
	}
	setSessionCookie(w, r, id)
	credential := "adminToken"
	if !tokenOK {
		credential = "stashApiKey"
	}
	s.logAuth(r, slog.LevelInfo, "auth-success", "method", "token",
		"credential", credential)
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
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   cookieSecure(r),
		MaxAge:   -1,
	})
	// Debug, not Info: the route is public and unauthenticated, so anyone
	// can call it in a loop. Useful when tracing a session, not a signal.
	s.logAuth(r, slog.LevelDebug, "auth-logout")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
