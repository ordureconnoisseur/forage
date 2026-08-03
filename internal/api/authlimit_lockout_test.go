package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The failure this test exists for, measured on the branch before the fix:
// 25 gate failures, then a SUCCESSFUL login, then a request carrying the
// brand-new valid cookie was answered 429 with Retry-After 300. Correct
// credentials were refused for the rest of the window, which is a lockout,
// and forage deliberately has no lockout.
//
// throttled() runs before requestAuthorized, so once the gate scope is over
// budget nothing can ever call noteAuthSuccess on it. Scope separation lets
// the owner log IN; only this reset lets them get back to work.
func TestLoginSuccessClearsAGateLockout(t *testing.T) {
	s := &Server{}
	req := func() *http.Request {
		r := httptest.NewRequest("GET", "/config", nil)
		r.RemoteAddr = "203.0.113.9:5555"
		return r
	}

	// Burn the gate budget, as a tab with an expired cookie does.
	for i := 0; i < forwardedFailBudget+5; i++ {
		s.noteAuthFailure(scopeGate, req())
	}
	if !s.throttled(scopeGate, httptest.NewRecorder(), req()) {
		t.Fatal("gate should be over budget after exceeding it")
	}

	// The owner logs in. That is a correct password, the strongest evidence
	// available that this is not an attacker.
	s.noteAuthSuccess(scopeLogin, req())

	if s.throttled(scopeGate, httptest.NewRecorder(), req()) {
		t.Error("a correct password must clear the gate: refusing a valid credential is a lockout")
	}
}

// The reset must not become a way to buy guesses. Reaching it costs a correct
// password, and the login scope keeps its own budget.
func TestLoginSuccessDoesNotRefillTheLoginBudget(t *testing.T) {
	s := &Server{}
	req := func() *http.Request {
		r := httptest.NewRequest("POST", "/login", nil)
		r.RemoteAddr = "203.0.113.9:5555"
		return r
	}
	for i := 0; i < forwardedFailBudget+1; i++ {
		s.noteAuthFailure(scopeLogin, req())
	}
	s.noteAuthSuccess(scopeGate, req()) // a non-login success must not help
	if !s.throttled(scopeLogin, httptest.NewRecorder(), req()) {
		t.Error("only a login success may clear scopes; the login budget itself stays spent")
	}
}

// A different client must not be freed by someone else's login.
func TestLoginSuccessOnlyClearsItsOwnClient(t *testing.T) {
	s := &Server{}
	mk := func(addr string) *http.Request {
		r := httptest.NewRequest("GET", "/config", nil)
		r.RemoteAddr = addr
		return r
	}
	for i := 0; i < forwardedFailBudget+1; i++ {
		s.noteAuthFailure(scopeGate, mk("203.0.113.9:1111"))
	}
	s.noteAuthSuccess(scopeLogin, mk("198.51.100.4:2222"))
	if !s.throttled(scopeGate, httptest.NewRecorder(), mk("203.0.113.9:1111")) {
		t.Error("one client's login must not clear another client's throttle")
	}
}
