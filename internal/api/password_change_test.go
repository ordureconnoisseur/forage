package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/configstore"
	"golang.org/x/crypto/bcrypt"
)

// pwChangeServer builds a Server whose only configured credentials are the
// ones the case under test needs.
func pwChangeServer(t *testing.T, password, adminToken, stashKey string) *Server {
	t.Helper()
	store, err := configstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hash := ""
	if password != "" {
		h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
		if err != nil {
			t.Fatal(err)
		}
		hash = string(h)
	}
	return &Server{
		bootstrap: config.BootstrapConfig{Config: config.Config{
			Username:     "ethan",
			PasswordHash: hash,
			AdminToken:   adminToken,
			StashAPIKey:  stashKey,
		}},
		store: store,
	}
}

// Replacing the login password used to need nothing but a session. A
// cookie lifted from a browser (they last 7 days and survive restarts)
// could be spent on a new password for permanent exclusive access, or on
// clearing it to lock the owner out. Each case here is a way that has to
// stay shut, or a recovery path that has to stay open.
func TestAuthorizePasswordChange(t *testing.T) {
	const current = "hunter2hunter2"
	cases := []struct {
		name       string
		password   string // currently configured password ("" = none)
		adminToken string
		stashKey   string
		authHeader string
		sent       *string // currentPassword field of the request
		want       bool
	}{
		{
			// First-time setup: demanding a current password here would make
			// the login un-settable.
			name: "no password set yet needs no proof",
			want: true,
		},
		{
			name:     "correct current password is accepted",
			password: current,
			sent:     strptr(current),
			want:     true,
		},
		{
			name:     "missing current password is refused",
			password: current,
			sent:     nil,
			want:     false,
		},
		{
			name:     "wrong current password is refused",
			password: current,
			sent:     strptr("guess"),
			want:     false,
		},
		{
			name:     "empty current password is refused",
			password: current,
			sent:     strptr(""),
			want:     false,
		},
		{
			// The admin token is forage's root secret and is readable from
			// data/config.json on the host, so it is the recovery path for a
			// forgotten password.
			name:       "admin token bearer is the recovery path",
			password:   current,
			adminToken: "admin-token",
			authHeader: "Bearer admin-token",
			want:       true,
		},
		{
			// The Stash API key satisfies the gate but is a borrowed
			// credential any Stash plugin can read; letting it mint a new
			// forage password would upgrade it into standalone permanent
			// access.
			name:       "stash api key is not a recovery path",
			password:   current,
			adminToken: "admin-token",
			stashKey:   "stash-key",
			authHeader: "Bearer stash-key",
			want:       false,
		},
		{
			name:       "a wrong bearer is not a recovery path",
			password:   current,
			adminToken: "admin-token",
			authHeader: "Bearer nope",
			want:       false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := pwChangeServer(t, c.password, c.adminToken, c.stashKey)
			req := httptest.NewRequest(http.MethodPost, "/config", nil)
			if c.authHeader != "" {
				req.Header.Set("Authorization", c.authHeader)
			}
			rec := httptest.NewRecorder()
			if got := s.authorizePasswordChange(rec, req, c.sent); got != c.want {
				t.Fatalf("authorized = %v, want %v (status %d, body %q)", got, c.want, rec.Code, rec.Body.String())
			}
			if !c.want && rec.Code != http.StatusForbidden {
				t.Errorf("refusal status = %d, want 403", rec.Code)
			}
		})
	}
}

// Clearing the password is a credential change like any other: an attacker
// on a stolen session could otherwise switch the login off entirely and
// leave the daemon open.
func TestClearingPasswordNeedsTheCurrentOne(t *testing.T) {
	s := pwChangeServer(t, "hunter2hunter2", "", "")
	req := httptest.NewRequest(http.MethodPost, "/config", nil)
	rec := httptest.NewRecorder()
	if s.authorizePasswordChange(rec, req, nil) {
		t.Fatal("clearing the password was authorized with no current password")
	}
}

// End to end through the handler: POST /config with a new password and no
// currentPassword must be refused before it reaches the store, so the
// stored config is untouched.
func TestPostConfigRefusesPasswordChangeWithoutCurrent(t *testing.T) {
	s := pwChangeServer(t, "hunter2hunter2", "", "")
	before := s.effectivePasswordHash()

	req := httptest.NewRequest(http.MethodPost, "/config",
		strings.NewReader(`{"password":"attackers-new-password"}`))
	rec := httptest.NewRecorder()
	s.postConfig(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403 (body=%q)", rec.Code, rec.Body.String())
	}
	if s.effectivePasswordHash() != before {
		t.Error("password hash changed despite the refusal")
	}
	if bcrypt.CompareHashAndPassword([]byte(s.effectivePasswordHash()), []byte("hunter2hunter2")) != nil {
		t.Error("the original password no longer works")
	}
}

// Guessing the current password at /config is guessing a password, so it
// draws on a failure budget too — otherwise a stolen cookie could brute
// force it at the one endpoint that then hands over the account.
func TestPasswordChangeGuessesAreThrottled(t *testing.T) {
	s := pwChangeServer(t, "hunter2hunter2", "", "")
	wrong := strptr("guess")
	for i := 0; i < loginMaxFailures; i++ {
		rec := httptest.NewRecorder()
		s.authorizePasswordChange(rec, httptest.NewRequest(http.MethodPost, "/config", nil), wrong)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("attempt %d: code = %d, want 403", i+1, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	s.authorizePasswordChange(rec, httptest.NewRequest(http.MethodPost, "/config", nil), wrong)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-budget code = %d, want 429", rec.Code)
	}
}
