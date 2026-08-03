package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/configstore"
	"golang.org/x/crypto/bcrypt"
)

// forgedIP returns the i-th of a large supply of distinct, PARSEABLE
// addresses for the spoofing tests. Parseable matters: clientIP runs the
// forwarded value through net.ParseIP and silently falls back to the peer
// when it fails, so a made-up string that is not an IP produces one shared
// key and a test that proves nothing. The first cut of these tests used
// "198.51.100.7:1234", which is exactly that mistake: every one of 50,000
// "distinct" keys collapsed onto the peer's.
func forgedIP(i int) string {
	return "10." + strconv.Itoa(i>>16&255) + "." +
		strconv.Itoa(i>>8&255) + "." + strconv.Itoa(i&255)
}

// gatedServer is a token-gated daemon with a password too, so all four
// credential-checking paths are live.
func gatedServer(t *testing.T) *Server {
	t.Helper()
	s := credServer(t)
	tok := "the-real-token"
	if err := s.store.Set(configstore.Patch{AdminToken: &tok}); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestGateThrottlesBearerGuessing is the measurement the previous attempt
// failed. On that branch the throttle fronted only POST /login and POST
// /session; a reviewer sent 500 requests to /config with a wrong Bearer
// and got 500 x 401 and zero 429s, because every real Bearer client
// authenticates through adminAuthMiddleware, which had no throttle at all.
//
// This drives the same shape: wrong Bearers at a gated route, from one
// address, and requires that they stop being evaluated.
func TestGateThrottlesBearerGuessing(t *testing.T) {
	s := gatedServer(t)
	h := s.adminAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	var codes []int
	for i := 0; i < 500; i++ {
		r := httptest.NewRequest(http.MethodGet, "/config", nil)
		r.RemoteAddr = "203.0.113.9:5555"
		r.Header.Set("Authorization", "Bearer guess-"+strconv.Itoa(i))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		codes = append(codes, rec.Code)
	}
	var got401, got429 int
	for _, c := range codes {
		switch c {
		case http.StatusUnauthorized:
			got401++
		case http.StatusTooManyRequests:
			got429++
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	if got429 == 0 {
		t.Fatalf("500 wrong Bearers produced %d x 401 and zero 429s: the gate is not throttled",
			got401)
	}
	if got401 > forwardedFailBudget {
		t.Errorf("%d guesses were evaluated before the throttle bit, budget is %d",
			got401, forwardedFailBudget)
	}
	t.Logf("500 wrong Bearers: %d x 401, %d x 429", got401, got429)
}

// TestThrottleSurvivesForwardedForSpoofing is the other half of what the
// previous attempt got wrong. clientIP honours the left-most
// X-Forwarded-For when the peer is loopback or RFC1918 (the deployment
// the README recommends), so a limiter keyed on it alone is stepped around
// by rotating one header. Measured on that branch: 200 wrong-password
// posts from 127.0.0.1 with a rotating X-Forwarded-For produced zero 429s
// and 200 distinct buckets.
func TestThrottleSurvivesForwardedForSpoofing(t *testing.T) {
	s := gatedServer(t)
	h := s.adminAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	var got429 int
	for i := 0; i < 400; i++ {
		r := httptest.NewRequest(http.MethodGet, "/config", nil)
		// Loopback peer: exactly the `tailscale serve` / reverse-proxy
		// arrangement, where the forwarded header IS trusted.
		r.RemoteAddr = "127.0.0.1:44444"
		// A brand-new forwarded address every time, so the fine-grained
		// budget is never reached and only the peer ceiling can stop this.
		r.Header.Set("X-Forwarded-For", forgedIP(i))
		r.Header.Set("Authorization", "Bearer guess-"+strconv.Itoa(i))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code == http.StatusTooManyRequests {
			got429++
		}
	}
	if got429 == 0 {
		t.Fatal("400 guesses with a rotating X-Forwarded-For produced zero 429s: " +
			"the throttle is keyed on a value the client controls")
	}
	t.Logf("rotating X-Forwarded-For: %d/400 refused by the peer ceiling", got429)
}

// TestThrottleMemoryIsBounded: the obvious implementation (a map plus a
// sweep of expired records) grows without limit under exactly the attack
// above, because inside one window nothing is expired: a reviewer
// measured 50,000 retained records and ~0.28ms per failure at that size on
// the previous attempt. This one is two fixed arrays, so the bound is
// structural rather than hopeful.
func TestThrottleMemoryIsBounded(t *testing.T) {
	s := gatedServer(t)
	for i := 0; i < 50_000; i++ {
		r := httptest.NewRequest(http.MethodGet, "/config", nil)
		r.RemoteAddr = "127.0.0.1:44444"
		r.Header.Set("X-Forwarded-For", forgedIP(i))
		s.noteAuthFailure(scopeGate, r)
	}
	s.authLimit.mu.Lock()
	client, peer := len(s.authLimit.client), len(s.authLimit.peer)
	var live int
	for _, sl := range s.authLimit.client {
		if sl.key != "" {
			live++
		}
	}
	s.authLimit.mu.Unlock()
	if client != clientSlots || peer != peerSlots {
		t.Fatalf("tables grew: client=%d peer=%d, want %d/%d",
			client, peer, clientSlots, peerSlots)
	}
	if live > clientSlots {
		t.Fatalf("live records = %d, cannot exceed %d", live, clientSlots)
	}
	// Sanity check on the fixture itself: if the forged addresses had
	// collapsed onto one key (see forgedIP) barely any slot would be
	// occupied and the bound above would be vacuously true.
	if live < clientSlots/2 {
		t.Fatalf("only %d of %d slots occupied: the forged keys are not distinct, "+
			"so this test is not exercising the attack", live, clientSlots)
	}
	t.Logf("50,000 distinct forged keys: %d fixed client slots, %d occupied", client, live)
}

// TestThrottleCoversEveryCredentialCheck drives all four places a secret
// is compared and requires each to end in a 429. The scopes exist so a
// browser burning one cannot stop its owner using another, so each is
// pushed over its own budget independently.
func TestThrottleCoversEveryCredentialCheck(t *testing.T) {
	const addr = "203.0.113.42:1000"

	t.Run("gate", func(t *testing.T) {
		s := gatedServer(t)
		h := s.adminAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
		last := drive(t, forwardedFailBudget+1, func() *httptest.ResponseRecorder {
			r := httptest.NewRequest(http.MethodGet, "/config", nil)
			r.RemoteAddr = addr
			r.Header.Set("Authorization", "Bearer nope")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			return rec
		})
		requireThrottled(t, last)
	})

	t.Run("login", func(t *testing.T) {
		s := gatedServer(t)
		last := drive(t, forwardedFailBudget+1, func() *httptest.ResponseRecorder {
			r := httptest.NewRequest(http.MethodPost, "/login",
				strings.NewReader(`{"username":"owner","password":"nope"}`))
			r.RemoteAddr = addr
			rec := httptest.NewRecorder()
			s.postLogin(rec, r)
			return rec
		})
		requireThrottled(t, last)
	})

	t.Run("session", func(t *testing.T) {
		s := gatedServer(t)
		last := drive(t, forwardedFailBudget+1, func() *httptest.ResponseRecorder {
			r := httptest.NewRequest(http.MethodPost, "/session",
				strings.NewReader(`{"token":"nope"}`))
			r.RemoteAddr = addr
			rec := httptest.NewRecorder()
			s.postSession(rec, r)
			return rec
		})
		requireThrottled(t, last)
	})

	t.Run("config currentPassword", func(t *testing.T) {
		s := gatedServer(t)
		last := drive(t, forwardedFailBudget+1, func() *httptest.ResponseRecorder {
			r := cookieRequest(t, s, `{"password":"x","currentPassword":"guess"}`)
			r.RemoteAddr = addr
			rec := httptest.NewRecorder()
			s.postConfig(rec, r)
			return rec
		})
		requireThrottled(t, last)
	})
}

func drive(t *testing.T, n int, once func() *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	var rec *httptest.ResponseRecorder
	for i := 0; i < n; i++ {
		rec = once()
	}
	return rec
}

func requireThrottled(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body=%s)", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 without a Retry-After header")
	}
}

// TestScopesDoNotShareBudgets is the no-lockout property that matters most
// in practice: a tab whose cookie expired spends the gate's budget on its
// in-flight polls, and its owner must still be able to sign in and fix it.
func TestScopesDoNotShareBudgets(t *testing.T) {
	s := gatedServer(t)
	h := s.adminAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	for i := 0; i < forwardedFailBudget*3; i++ {
		r := httptest.NewRequest(http.MethodGet, "/watches", nil)
		r.RemoteAddr = "203.0.113.7:2000"
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "expired.deadbeef"})
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	r := httptest.NewRequest(http.MethodPost, "/login",
		strings.NewReader(`{"username":"owner","password":"`+ownerPassword+`"}`))
	r.RemoteAddr = "203.0.113.7:2000"
	rec := httptest.NewRecorder()
	s.postLogin(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("login after the gate's budget was spent: status = %d, want 200 (body=%s)",
			rec.Code, rec.Body.String())
	}
}

// TestSuccessClearsTheClientRecord: mistyping a password up to the budget
// and then getting it right must leave no wait behind. Without this the
// control would be a lockout in everything but name.
func TestSuccessClearsTheClientRecord(t *testing.T) {
	s := gatedServer(t)
	const addr = "203.0.113.11:3000"
	login := func(pw string) int {
		r := httptest.NewRequest(http.MethodPost, "/login",
			strings.NewReader(`{"username":"owner","password":"`+pw+`"}`))
		r.RemoteAddr = addr
		rec := httptest.NewRecorder()
		s.postLogin(rec, r)
		return rec.Code
	}
	for i := 0; i < forwardedFailBudget-1; i++ {
		if code := login("wrong"); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i, code)
		}
	}
	if code := login(ownerPassword); code != http.StatusOK {
		t.Fatalf("correct password: status = %d, want 200", code)
	}
	// The record is gone, so the full budget is available again.
	for i := 0; i < forwardedFailBudget-1; i++ {
		if code := login("wrong"); code != http.StatusUnauthorized {
			t.Fatalf("after the successful login, attempt %d: status = %d, want 401", i, code)
		}
	}
}

// TestPeerRecordSurvivesSuccess is the reason succeed() clears only the
// fine-grained record. Behind a shared proxy the owner's own successful
// traffic would otherwise wipe an attacker's count on every poll, and the
// un-spoofable ceiling would never hold.
func TestPeerRecordSurvivesSuccess(t *testing.T) {
	var l authLimiter
	for i := 0; i < peerFailBudget; i++ {
		l.fail("gate|127.0.0.1|198.51.100."+strconv.Itoa(i), "gate|127.0.0.1")
	}
	l.succeed("gate|127.0.0.1|10.0.0.1")
	if over, _ := l.status("gate|127.0.0.1|10.0.0.1", "gate|127.0.0.1"); !over {
		t.Fatal("a success cleared the peer ceiling")
	}
}

// TestThrottleExpiresWithoutIntervention is the "no lockout" guarantee,
// stated as a property: the window runs from the FIRST failure, so piling
// on more attempts cannot extend a throttle already in progress, and it
// ends on its own.
func TestThrottleExpiresWithoutIntervention(t *testing.T) {
	now := time.Now()
	l := authLimiter{nowForTest: func() time.Time { return now }}
	const fwd, peer = "login|1.2.3.4|1.2.3.4", "login|1.2.3.4"

	for i := 0; i < forwardedFailBudget; i++ {
		l.fail(fwd, peer)
	}
	if over, _ := l.status(fwd, peer); !over {
		t.Fatal("budget not spent")
	}
	// Four more minutes of hammering must not push the release time out.
	for m := 1; m <= 4; m++ {
		now = now.Add(time.Minute)
		for i := 0; i < 50; i++ {
			l.fail(fwd, peer)
		}
	}
	now = now.Add(time.Minute + time.Second) // 5m1s past the FIRST failure
	if over, wait := l.status(fwd, peer); over {
		t.Fatalf("still throttled %v after the window should have closed", wait)
	}
}

// TestOpenDaemonIsNotThrottled: with no credential configured nothing is
// checked, so there is nothing to guess and no reason to refuse anyone.
func TestOpenDaemonIsNotThrottled(t *testing.T) {
	store, err := configstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		bootstrap: config.BootstrapConfig{},
		store:     store,
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	h := s.adminAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for i := 0; i < forwardedFailBudget*3; i++ {
		r := httptest.NewRequest(http.MethodGet, "/watches", nil)
		r.RemoteAddr = "203.0.113.30:1"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("open daemon returned %d on request %d", rec.Code, i)
		}
	}
}

// TestBcryptIsNotEvaluatedWhenThrottled pins the reason the throttle
// refuses to evaluate rather than just relabelling the answer 429: on the
// password paths, evaluating is ~100ms of bcrypt that an attacker gets to
// spend for free. A limiter that still ran the compare would slow nothing
// down and would leave the CPU burn intact.
func TestBcryptIsNotEvaluatedWhenThrottled(t *testing.T) {
	s := gatedServer(t)
	// A deliberately expensive hash, so an evaluation is unmistakable.
	slow, err := bcrypt.GenerateFromPassword([]byte(ownerPassword), 12)
	if err != nil {
		t.Fatal(err)
	}
	h := string(slow)
	if err := s.store.Set(configstore.Patch{PasswordHash: &h}); err != nil {
		t.Fatal(err)
	}
	const addr = "203.0.113.55:9"
	post := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/login",
			strings.NewReader(`{"username":"owner","password":"nope"}`))
		r.RemoteAddr = addr
		rec := httptest.NewRecorder()
		s.postLogin(rec, r)
		return rec
	}
	for i := 0; i < forwardedFailBudget; i++ {
		post()
	}
	start := time.Now()
	rec := post()
	elapsed := time.Since(start)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	// cost-12 bcrypt is hundreds of milliseconds; a refusal that skipped it
	// is sub-millisecond. 20ms is a wide margin either way, so this does
	// not turn into a flaky timing test on a loaded machine.
	if elapsed > 20*time.Millisecond {
		t.Fatalf("throttled request took %v: the bcrypt compare still ran", elapsed)
	}
}
