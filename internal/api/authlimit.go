// Credential-guessing throttle.
//
// WHAT IT COVERS. Every path that checks a credential, not just the login
// form. That distinction is the whole reason this file is shaped the way
// it is: an earlier attempt fronted only POST /login and POST /session,
// while every Bearer client in forage authenticates through
// adminAuthMiddleware → requestAuthorized, which had no throttle at all.
// Measured on that branch: 500 requests to /config with a wrong Bearer
// produced 500 × 401 and zero 429s. The four scopes below are the four
// places a secret is checked; there are no others (see the route table in
// server.go, and TestThrottleCoversEveryCredentialCheck).
//
// NO LOCKOUT. A budget refills by itself: the window is fixed from the
// first failure, so being throttled ends within authFailWindow of that
// first bad attempt no matter how many more arrive, and a successful
// credential check clears the fine-grained record immediately. Nothing
// here ever needs an administrator to undo it.
//
// THE KEY, AND WHY IT IS NOT JUST clientIP(). clientIP honours the
// left-most X-Forwarded-For entry when the transport peer is loopback or
// RFC1918 — which is exactly the deployment the README recommends
// (`tailscale serve`, or a reverse proxy, talking to 127.0.0.1). Proxies
// that append rather than replace (Go's httputil.ReverseProxy, nginx's
// $proxy_add_x_forwarded_for, Caddy) leave that entry client-controlled,
// so a per-clientIP budget there is bypassed by rotating one header. So
// every failure is counted twice:
//
//   - against the FORWARDED key (scope + peer + clientIP). Fine-grained,
//     and correct when the proxy is honest — one bad client behind your
//     proxy does not spend anyone else's budget.
//   - against the PEER key (scope + transport peer). Un-spoofable, because
//     it is the address the TCP connection actually came from. This is the
//     ceiling that a header-rotating attacker cannot get around.
//
// The honest cost of that: behind a reverse proxy, every client shares one
// peer key, so a sustained spray through your proxy can make you wait too,
// for up to authFailWindow. There is no way to avoid that and still have a
// limit an attacker cannot step around by inventing a header — the daemon
// genuinely cannot tell the two of you apart. It is bounded, it expires on
// its own, and the alternative (a control that looks like protection and
// isn't) is worse.
//
// MEMORY. Fixed. Two arrays of fixed length, allocated once on the first
// failure and never grown: at most clientSlots + peerSlots records exist
// at any time, whatever an attacker does. A key hashes to one slot and
// evicts whatever was there if that record has expired or belongs to
// another key. This replaces the obvious sketch — a map plus a sweep of
// expired entries — which does not bound anything, because inside one
// window nothing is expired: an attacker rotating X-Forwarded-For grows it
// without limit and makes every failure pay an O(n) scan under the mutex.
//
// Collisions are the price. Two live keys landing in the same slot evict
// each other, which weakens the limit for both (each keeps resetting the
// other's count). The peer and forwarded tables are separate so that a
// key an attacker CHOOSES (the forwarded one) can never evict a key they
// cannot choose (their own peer address) — otherwise grinding a
// forwarded value that collides with your own peer slot would reset the
// ceiling that is supposed to hold you.
package api

import (
	"hash/fnv"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	// authFailWindow is how long a failure counts against a budget, timed
	// from the first failure in the window (not the most recent), so more
	// attempts cannot extend a throttle already in progress.
	authFailWindow = 5 * time.Minute

	// forwardedFailBudget is the per-client allowance. Set well above what
	// a browser can spend by accident and far below anything useful for
	// guessing: the worst legitimate case is a tab whose 7-day cookie
	// expired mid-session, which spends one failure per in-flight poll
	// before the client's 401 handler routes it to the login gate — a
	// handful, not twenty. Scopes are separate (see authScope), so those
	// gate failures cannot spend the login form's budget.
	forwardedFailBudget = 20

	// peerFailBudget is the un-spoofable ceiling per transport peer. Six
	// times the per-client budget: enough headroom that several genuine
	// clients behind one proxy do not collide, low enough that a spray is
	// capped at ~35k attempts a day against a 32-byte token.
	peerFailBudget = 120

	// Table sizes. Powers of two, sized so a home deployment never
	// collides and an attacker cannot make them grow.
	clientSlots = 2048
	peerSlots   = 512
)

// authScope separates the credential checks that share this limiter, so a
// browser burning the gate's budget with a stale cookie cannot stop its
// owner from logging in to fix it. Each scope is one place a secret is
// compared.
type authScope string

const (
	scopeGate    authScope = "gate"    // adminAuthMiddleware → requestAuthorized
	scopeLogin   authScope = "login"   // POST /login (bcrypt)
	scopeSession authScope = "session" // POST /session (token → cookie)
	scopeConfig  authScope = "config"  // POST /config currentPassword (bcrypt)
)

// failSlot is one occupied slot. An empty key means never used.
type failSlot struct {
	key     string
	count   int
	resetAt time.Time
}

// authLimiter is the fixed-size failure ledger. Zero value is ready to
// use; the tables are allocated on the first recorded failure, so a daemon
// (or a test) that never fails a credential check never pays for them.
type authLimiter struct {
	mu         sync.Mutex
	client     []failSlot
	peer       []failSlot
	nowForTest func() time.Time
}

func (l *authLimiter) now() time.Time {
	if l.nowForTest != nil {
		return l.nowForTest()
	}
	return time.Now()
}

// slot resolves a key to its slot plus whether that slot currently holds a
// live record for this key. Caller holds the mutex.
func slot(table []failSlot, key string, now time.Time) (*failSlot, bool) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	s := &table[h.Sum64()%uint64(len(table))]
	return s, s.key == key && now.Before(s.resetAt)
}

// status reports whether this key pair is currently over budget and, if
// so, how long until it is not. Never allocates: an attacker cannot make
// the daemon grow anything by being throttled.
func (l *authLimiter) status(forwardedKey, peerKey string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.client == nil {
		return false, 0
	}
	now := l.now()
	if s, live := slot(l.client, forwardedKey, now); live && s.count >= forwardedFailBudget {
		return true, s.resetAt.Sub(now)
	}
	if s, live := slot(l.peer, peerKey, now); live && s.count >= peerFailBudget {
		return true, s.resetAt.Sub(now)
	}
	return false, 0
}

// fail records one failed credential check against both keys.
func (l *authLimiter) fail(forwardedKey, peerKey string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.client == nil {
		l.client = make([]failSlot, clientSlots)
		l.peer = make([]failSlot, peerSlots)
	}
	now := l.now()
	bump(l.client, forwardedKey, now)
	bump(l.peer, peerKey, now)
}

func bump(table []failSlot, key string, now time.Time) {
	s, live := slot(table, key, now)
	if !live {
		*s = failSlot{key: key, count: 1, resetAt: now.Add(authFailWindow)}
		return
	}
	s.count++
}

// succeed clears the fine-grained record after a credential check passes,
// so someone who mistyped their password a dozen times and then got it
// right is not left waiting.
//
// It deliberately does NOT clear the peer record. Behind a shared proxy
// the owner's own successful traffic would otherwise wipe an attacker's
// count on every poll, and the un-spoofable ceiling would never hold. The
// peer record decays on time alone.
func (l *authLimiter) succeed(forwardedKey string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.client == nil {
		return
	}
	if s, live := slot(l.client, forwardedKey, l.now()); live {
		*s = failSlot{}
	}
}

// authKeys builds this request's (forwarded, peer) keys for a scope.
func authKeys(scope authScope, r *http.Request) (string, string) {
	ip, peer := clientIP(r)
	return string(scope) + "|" + peer + "|" + ip, string(scope) + "|" + peer
}

// throttled answers a credential check that is over budget with 429 +
// Retry-After and reports true, so the caller returns without evaluating
// the credential. Refusing to evaluate is the point: a limiter that still
// checks the guess and only relabels the answer 429 slows nothing down,
// and on the bcrypt paths it would leave the CPU burn intact too.
func (s *Server) throttled(scope authScope, w http.ResponseWriter, r *http.Request) bool {
	fwd, peer := authKeys(scope, r)
	over, wait := s.authLimit.status(fwd, peer)
	if !over {
		return false
	}
	secs := int(wait.Seconds()) + 1
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	s.logAuth(r, slog.LevelWarn, "auth-throttled",
		"scope", string(scope), "retryAfterSeconds", secs)
	writeErr(w, http.StatusTooManyRequests,
		"too many failed attempts — try again in "+strconv.Itoa(secs)+"s")
	return true
}

// noteAuthFailure and noteAuthSuccess record the outcome of a credential
// check for a scope.
func (s *Server) noteAuthFailure(scope authScope, r *http.Request) {
	fwd, peer := authKeys(scope, r)
	s.authLimit.fail(fwd, peer)
}

func (s *Server) noteAuthSuccess(scope authScope, r *http.Request) {
	fwd, _ := authKeys(scope, r)
	s.authLimit.succeed(fwd)
}
