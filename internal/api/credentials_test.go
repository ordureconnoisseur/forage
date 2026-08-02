package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/clientpool"
	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/configstore"
	"github.com/ordureconnoisseur/forager/internal/db"
	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/watches"
	"golang.org/x/crypto/bcrypt"
)

// credServer builds a daemon with a web login configured (username
// "owner", password "correct horse") and nothing else, which is the state
// the guard exists to protect: a caller can hold a session cookie without
// holding any credential.
func credServer(t *testing.T) *Server {
	t.Helper()
	dbh, err := db.Open(t.TempDir() + "/d.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbh.Close() })
	store, err := configstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(ownerPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	user, h := "owner", string(hash)
	if err := store.Set(configstore.Patch{Username: &user, PasswordHash: &h}); err != nil {
		t.Fatal(err)
	}
	pool := clientpool.New()
	pool.Reload(config.Config{})
	s := &Server{
		db:         dbh,
		pool:       pool,
		store:      store,
		grabs:      grabs.NewRepo(dbh),
		watches:    watches.NewRepo(dbh),
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessionKey: loadOrCreateSessionKey(dbh, nil),
	}
	return s
}

const ownerPassword = "correct horse"

// cookieRequest is a POST /config carrying nothing but a valid session
// cookie — the attacker's position: a lifted cookie, no credential.
func cookieRequest(t *testing.T, s *Server, body string) *http.Request {
	t.Helper()
	id, err := s.newSession()
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(body))
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: id})
	return r
}

func postConfigWith(t *testing.T, s *Server, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.postConfig(rec, r)
	return rec
}

// TestCookieCannotChangeAnyCredential is the regression test for the two
// bypasses a reviewer verified against the previous attempt at this
// change, plus the two fields that attempt never considered.
//
// Each case is a single POST /config carrying only a session cookie. Each
// one must be refused, and — the part that matters more than the status
// code — must leave the stored credential untouched. A guard that answers
// 403 and saves anyway would pass a status-only assertion.
func TestCookieCannotChangeAnyCredential(t *testing.T) {
	cases := []struct {
		name string
		body string
		// field to re-read from the store afterwards
		field string
	}{
		{
			// Bypass (a), verified on the previous branch: postConfig only
			// guarded `password`, but the body embeds configstore.Patch,
			// which carries passwordHash straight through to the store.
			name:  "raw passwordHash patch",
			body:  `{"passwordHash":"$2a$10$abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTU"}`,
			field: "passwordHash",
		},
		{
			// Same field, used to turn password login off entirely.
			name:  "passwordHash cleared",
			body:  `{"passwordHash":""}`,
			field: "passwordHash",
		},
		{
			// Bypass (b): adminToken was unguarded, and bearerIsAdminToken
			// then treated the attacker's chosen value as forage's root
			// secret. Step 1 alone already granted standalone access.
			name:  "adminToken set by an unproven caller",
			body:  `{"adminToken":"pwned"}`,
			field: "adminToken",
		},
		{
			// Neither attempt considered this one. requestAuthorized
			// accepts the Stash API key as a Bearer credential, so writing
			// it mints a bearer of the writer's choosing exactly as
			// adminToken does.
			name:  "stashApiKey set by an unproven caller",
			body:  `{"stashApiKey":"pwned-stash-key"}`,
			field: "stashApiKey",
		},
		{
			// Half of the login pair. Changing it alone locks the owner out
			// of their own login form.
			name:  "username",
			body:  `{"username":"attacker"}`,
			field: "username",
		},
		{
			// The field the previous attempt did guard, kept here so the
			// obvious path stays covered.
			name:  "plaintext password",
			body:  `{"password":"attackers-password"}`,
			field: "passwordHash",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := credServer(t)
			before := credentialValue(s, c.field)

			rec := postConfigWith(t, s, cookieRequest(t, s, c.body))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body["code"] != "credential_proof_required" {
				t.Errorf("code = %v, want credential_proof_required", body["code"])
			}
			if after := credentialValue(s, c.field); after != before {
				t.Fatalf("%s changed despite the refusal: %q → %q", c.field, before, after)
			}
			// The owner's password must still work.
			if bcrypt.CompareHashAndPassword(
				[]byte(s.effectivePasswordHash()), []byte(ownerPassword)) != nil {
				t.Fatal("the owner's password stopped working")
			}
		})
	}
}

func credentialValue(s *Server, field string) string {
	return authCredentials(s.composedConfig())[field]
}

// TestCurrentPasswordAuthorisesTheChange covers the path the owner
// actually uses, and the one they must not be able to take by accident:
// the right password lets the change through, the wrong one does not.
func TestCurrentPasswordAuthorisesTheChange(t *testing.T) {
	t.Run("correct current password", func(t *testing.T) {
		s := credServer(t)
		rec := postConfigWith(t, s, cookieRequest(t, s,
			`{"password":"a new password","currentPassword":"`+ownerPassword+`"}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		if bcrypt.CompareHashAndPassword(
			[]byte(s.effectivePasswordHash()), []byte("a new password")) != nil {
			t.Fatal("password was not changed")
		}
	})
	t.Run("wrong current password", func(t *testing.T) {
		s := credServer(t)
		rec := postConfigWith(t, s, cookieRequest(t, s,
			`{"password":"a new password","currentPassword":"not it"}`))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if bcrypt.CompareHashAndPassword(
			[]byte(s.effectivePasswordHash()), []byte(ownerPassword)) != nil {
			t.Fatal("the owner's password stopped working")
		}
	})
}

// TestAdminTokenBearerIsTheRecoveryPath keeps the escape hatch open: the
// owner who forgot their password but still has the API key can set a new
// one. This exemption is only safe because writing adminToken is itself
// guarded now — TestCookieCannotChangeAnyCredential covers that half, and
// the two together are what stop it being circular.
func TestAdminTokenBearerIsTheRecoveryPath(t *testing.T) {
	s := credServer(t)
	tok := "the-api-key"
	if err := s.store.Set(configstore.Patch{AdminToken: &tok}); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/config",
		strings.NewReader(`{"password":"recovered"}`))
	r.Header.Set("Authorization", "Bearer "+tok)
	if rec := postConfigWith(t, s, r); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if bcrypt.CompareHashAndPassword(
		[]byte(s.effectivePasswordHash()), []byte("recovered")) != nil {
		t.Fatal("recovery did not set the new password")
	}
}

// TestOpenDaemonSetsItsFirstCredential is the anti-lockout case. On a
// daemon with no credential at all the middleware lets everything through
// anyway, so demanding proof would only make first-run setup impossible.
func TestOpenDaemonSetsItsFirstCredential(t *testing.T) {
	dbh, err := db.Open(t.TempDir() + "/d.db")
	if err != nil {
		t.Fatal(err)
	}
	defer dbh.Close()
	store, err := configstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pool := clientpool.New()
	pool.Reload(config.Config{})
	s := &Server{
		db: dbh, pool: pool, store: store,
		grabs: grabs.NewRepo(dbh), watches: watches.NewRepo(dbh),
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessionKey: loadOrCreateSessionKey(dbh, nil),
	}
	if s.authRequired() {
		t.Fatal("fixture is not an open daemon")
	}
	r := httptest.NewRequest(http.MethodPost, "/config",
		strings.NewReader(`{"username":"owner","password":"first password"}`))
	if rec := postConfigWith(t, s, r); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if !s.authRequired() {
		t.Fatal("the daemon did not end up gated")
	}
}

// TestOrdinarySaveNeedsNoProof: the guard must not put a password prompt
// in front of settings that are not credentials. pollInterval is used
// because it touches no probed section, so the save does no network I/O.
func TestOrdinarySaveNeedsNoProof(t *testing.T) {
	s := credServer(t)
	rec := postConfigWith(t, s, cookieRequest(t, s, `{"pollInterval":"90s"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := s.composedConfig().PollInterval.String(); got != "1m30s" {
		t.Fatalf("pollInterval = %s, want 1m30s", got)
	}
}

// TestCredentialChangeRevokesOtherSessions: a credential change is
// worthless if sessions minted under the old one keep working. The
// previous attempt keyed this off `body.Password != nil ||
// patch.AdminToken != nil`, so a password swapped in through the raw
// passwordHash field revoked nothing — the attacker's own cookie survived
// their own takeover. This drives it through passwordHash for that reason.
func TestCredentialChangeRevokesOtherSessions(t *testing.T) {
	s := credServer(t)
	other, err := s.newSession()
	if err != nil {
		t.Fatal(err)
	}
	if !s.sessionValid(other) {
		t.Fatal("fixture session is not valid to begin with")
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte("owner's new password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{
		"passwordHash":    string(newHash),
		"currentPassword": ownerPassword,
	})
	rec := postConfigWith(t, s, cookieRequest(t, s, string(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if s.sessionValid(other) {
		t.Fatal("a session minted before the credential change is still valid")
	}
}

// TestAuthCredentialsCoversTheGate ties the guard's idea of "a credential"
// to the gate's. The guard is a value comparison over authCredentials, so
// a credential the gate accepts but authCredentials omits would be
// writable by anyone with a cookie, silently — which is exactly how
// stashApiKey went unnoticed through two attempts at this change. This
// fails if the two ever drift.
func TestAuthCredentialsCoversTheGate(t *testing.T) {
	s := credServer(t)
	tok, key, user := "tok-value", "stash-key-value", "someone"
	hash := "hash-value"
	if err := s.store.Set(configstore.Patch{
		AdminToken: &tok, StashAPIKey: &key, Username: &user, PasswordHash: &hash,
	}); err != nil {
		t.Fatal(err)
	}
	got := authCredentials(s.composedConfig())
	// Every value the gate resolves must appear in the map, under a key
	// that is a real /config field name.
	fields := s.configFields()
	for name, want := range map[string]string{
		"adminToken":   s.effectiveAdminToken(),
		"stashApiKey":  s.effectiveStashAPIKey(),
		"username":     s.effectiveUsername(),
		"passwordHash": s.effectivePasswordHash(),
	} {
		if got[name] != want {
			t.Errorf("authCredentials[%q] = %q, gate resolves %q", name, got[name], want)
		}
		if _, ok := fields[name]; !ok {
			t.Errorf("authCredentials names %q, which is not a /config field", name)
		}
	}
	if len(got) != 4 {
		t.Errorf("authCredentials has %d entries; if you added one, add it here "+
			"and to the gate's list too", len(got))
	}
}

// TestChangedCredentialsIgnoresOtherSecrets: forage stores plenty of
// secrets that are not forage credentials. Requiring re-authentication to
// edit them would put a password prompt in front of routine settings work
// for no security gain, so the guard must stay out of their way.
func TestChangedCredentialsIgnoresOtherSecrets(t *testing.T) {
	before := config.Config{StashDBAPIKey: "old", ProwlarrAPIKey: "old", AdminToken: "t"}
	after := config.Config{StashDBAPIKey: "new", ProwlarrAPIKey: "new", AdminToken: "t"}
	if got := changedCredentials(before, after); len(got) != 0 {
		t.Fatalf("changedCredentials = %v, want none", got)
	}
	after.AdminToken = "t2"
	if got := changedCredentials(before, after); len(got) != 1 || got[0] != "adminToken" {
		t.Fatalf("changedCredentials = %v, want [adminToken]", got)
	}
}

// TestPreviewConfigUsesTheStoreMerge is the anti-drift check for the other
// half of the guard. The guard asks previewConfig what the patch would
// do; if that answer ever diverges from what Set actually does, the guard
// is protecting a fiction. previewConfig now delegates to
// configstore.Merged, and this pins that: apply a patch for real, and
// compare with what the preview predicted.
func TestPreviewConfigUsesTheStoreMerge(t *testing.T) {
	s := credServer(t)
	hash, tok := "predicted-hash", "predicted-token"
	patch := configstore.Patch{PasswordHash: &hash, AdminToken: &tok}
	predicted := s.previewConfig(patch)
	if err := s.store.Set(patch); err != nil {
		t.Fatal(err)
	}
	actual := s.composedConfig()
	if authCredentials(predicted)["passwordHash"] != authCredentials(actual)["passwordHash"] ||
		authCredentials(predicted)["adminToken"] != authCredentials(actual)["adminToken"] {
		t.Fatalf("preview disagreed with the store:\npredicted %+v\nactual    %+v",
			authCredentials(predicted), authCredentials(actual))
	}
}
