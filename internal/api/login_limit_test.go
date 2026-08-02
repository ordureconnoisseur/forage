package api

import (
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

// throttleServer builds a password-gated Server whose limiter runs on a
// fake clock, so the window can be advanced without sleeping.
func throttleServer(t *testing.T, clock *time.Time) *Server {
	t.Helper()
	store, err := configstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("hunter2hunter2"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		bootstrap: config.BootstrapConfig{Config: config.Config{
			Username:     "ethan",
			PasswordHash: string(hash),
			AdminToken:   "admin-token",
			StashAPIKey:  "stash-key",
		}},
		store: store,
	}
	s.logins.now = func() time.Time { return *clock }
	return s
}

func postLoginBody(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.postLogin(rec, httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body)))
	return rec
}

// Guessing used to be free: an attacker could post passwords as fast as
// bcrypt answered, forever. After the budget is spent the endpoint must
// refuse without even checking, and it must hand back a Retry-After so a
// client knows when to come back.
func TestLoginThrottleBlocksAfterBudget(t *testing.T) {
	now := time.Now()
	s := throttleServer(t, &now)

	for i := 0; i < loginMaxFailures; i++ {
		if rec := postLoginBody(t, s, `{"username":"ethan","password":"wrong"}`); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: code = %d, want 401", i+1, rec.Code)
		}
	}
	rec := postLoginBody(t, s, `{"username":"ethan","password":"wrong"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-budget code = %d, want 429 (body=%q)", rec.Code, rec.Body.String())
	}
	ra, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || ra <= 0 || time.Duration(ra)*time.Second > loginFailureWindow+time.Second {
		t.Errorf("Retry-After = %q, want a positive value within the window", rec.Header().Get("Retry-After"))
	}

	// The block covers the correct password too — the throttle runs before
	// any credential check, which is the whole point (it also means an
	// attacker can't use a lucky guess to escape it).
	if rec := postLoginBody(t, s, `{"username":"ethan","password":"hunter2hunter2"}`); rec.Code != http.StatusTooManyRequests {
		t.Errorf("correct password while blocked: code = %d, want 429", rec.Code)
	}

	// Nothing is permanent: the budget refills on its own so a legitimate
	// user who fat-fingered ten passwords is never locked out for good.
	now = now.Add(loginFailureWindow + time.Second)
	if rec := postLoginBody(t, s, `{"username":"ethan","password":"hunter2hunter2"}`); rec.Code != http.StatusOK {
		t.Errorf("after the window: code = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
}

// A successful login clears the record. Without this a user who mistypes
// nine times, gets in, then mistypes once more days later would be locked
// out by failures that are long past.
func TestLoginSuccessClearsFailures(t *testing.T) {
	now := time.Now()
	s := throttleServer(t, &now)

	for i := 0; i < loginMaxFailures-1; i++ {
		postLoginBody(t, s, `{"username":"ethan","password":"wrong"}`)
	}
	if rec := postLoginBody(t, s, `{"username":"ethan","password":"hunter2hunter2"}`); rec.Code != http.StatusOK {
		t.Fatalf("login code = %d, want 200", rec.Code)
	}
	for i := 0; i < loginMaxFailures-1; i++ {
		if rec := postLoginBody(t, s, `{"username":"ethan","password":"wrong"}`); rec.Code != http.StatusUnauthorized {
			t.Fatalf("post-success attempt %d: code = %d, want 401 (budget was not reset)", i+1, rec.Code)
		}
	}
}

// Bearer-key guessing at POST /session draws on its own bucket. Sharing
// one would let a plugin stuck retrying a stale API key throttle the human
// out of the web login.
func TestSessionThrottleIsSeparateFromLogin(t *testing.T) {
	now := time.Now()
	s := throttleServer(t, &now)

	for i := 0; i < loginMaxFailures; i++ {
		rec := httptest.NewRecorder()
		s.postSession(rec, httptest.NewRequest(http.MethodPost, "/session", strings.NewReader(`{"token":"nope"}`)))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: code = %d, want 401", i+1, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	s.postSession(rec, httptest.NewRequest(http.MethodPost, "/session", strings.NewReader(`{"token":"nope"}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-budget /session code = %d, want 429", rec.Code)
	}
	if rec := postLoginBody(t, s, `{"username":"ethan","password":"hunter2hunter2"}`); rec.Code != http.StatusOK {
		t.Errorf("/login code = %d, want 200 — /session failures must not spend the login budget", rec.Code)
	}
}

// Buckets are per client address, so one attacker cannot deny the login to
// everyone else by burning a shared budget.
func TestLoginThrottleIsPerClient(t *testing.T) {
	now := time.Now()
	s := throttleServer(t, &now)

	attempt := func(addr, body string) int {
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		s.postLogin(rec, req)
		return rec.Code
	}
	for i := 0; i <= loginMaxFailures; i++ {
		attempt("203.0.113.5:5000", `{"username":"ethan","password":"wrong"}`)
	}
	if code := attempt("203.0.113.5:5000", `{"username":"ethan","password":"wrong"}`); code != http.StatusTooManyRequests {
		t.Fatalf("attacker code = %d, want 429", code)
	}
	if code := attempt("198.51.100.9:5000", `{"username":"ethan","password":"hunter2hunter2"}`); code != http.StatusOK {
		t.Errorf("other client code = %d, want 200", code)
	}
}
