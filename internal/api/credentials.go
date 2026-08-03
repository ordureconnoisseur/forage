// Credential changes through POST /config.
//
// The problem this file exists to solve: forage's session cookie is a
// 7-day bearer of full API access, and POST /config could rewrite the
// daemon's own login credentials. So anyone who got hold of a cookie
// could turn a borrowed week into permanent, exclusive access (set their
// own password, or set an API key only they know) and lock the owner out
// on the way past.
//
// The naive guard ("if the request sets the `password` field, ask for the
// current one") does not work here, and an earlier attempt shipped exactly
// that and was bypassable in one request. Two reasons:
//
//   - The wire body embeds configstore.Patch, which carries its own
//     `passwordHash` field. Set that and the store writes it directly,
//     never touching the `password` field the guard was watching.
//   - `adminToken` was not on the watch list at all, and the admin token
//     is the daemon's root secret: writing it hands the writer a bearer
//     credential of their own choosing.
//
// So the guard here does not look at field names at all. It computes the
// set of values the auth gate WOULD accept after the patch is applied,
// compares it with the set it accepts now, and requires proof of ownership
// if any of them moved. A new field can only escape it by being both a
// credential AND absent from authCredentials, which
// TestAuthCredentialsCoversTheGate is there to catch.
package api

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/ordureconnoisseur/forager/internal/config"
	"golang.org/x/crypto/bcrypt"
)

// authCredentials lists every value the auth gate accepts as proof of
// identity, keyed by its /config field name.
//
// This is the single definition of "a credential" in forage. The gate
// (requestAuthorized, postLogin, postSession) accepts exactly these:
//
//   - adminToken:  Bearer, the root secret (admin_auth.go requestAuthorized)
//   - stashApiKey: Bearer, the same-Stash-trust path (same function). It is
//     a foreign secret forage stores for its own use, but the gate treats it
//     as proof of identity, so writing it mints a bearer credential just as
//     surely as writing adminToken does.
//   - username / passwordHash: the pair POST /login checks.
//
// Not listed, deliberately: every other stored secret (stashdbApiKey,
// prowlarrApiKey, qbitPassword, sabApiKey, telegramBotToken). Those are
// credentials for OTHER services. Losing one is bad, but it does not grant
// access to forage, so changing one does not need to be re-authorised,
// and making it need to would put a password prompt in front of routine
// settings edits.
func authCredentials(c config.Config) map[string]string {
	return map[string]string{
		"adminToken":   c.AdminToken,
		"stashApiKey":  c.StashAPIKey,
		"username":     c.Username,
		"passwordHash": c.PasswordHash,
	}
}

// changedCredentials names the credentials whose effective value differs
// between two composed configs, sorted so the message is stable.
func changedCredentials(before, after config.Config) []string {
	b, a := authCredentials(before), authCredentials(after)
	var out []string
	for name, bv := range b {
		if a[name] != bv {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// alsoChanged adds name to a changed-credentials list, keeping it sorted
// and free of duplicates. Used for the one credential change that cannot
// be detected by comparing values, because computing the new value is the
// expensive thing we are refusing to do before the guard runs: a new
// plaintext password.
func alsoChanged(changed []string, name string) []string {
	for _, c := range changed {
		if c == name {
			return changed
		}
	}
	out := append(changed, name)
	sort.Strings(out)
	return out
}

// credentialProof describes how a request proved it may change a
// credential, for the audit log. Empty when it did not.
type credentialProof string

const (
	proofNone        credentialProof = ""
	proofOpenDaemon  credentialProof = "no-credential-configured"
	proofPassword    credentialProof = "current-password"
	proofAdminToken  credentialProof = "admin-token"
	proofStashAPIKey credentialProof = "stash-api-key"
)

// provenCredentialChange reports how (if at all) this request proved it is
// entitled to change a credential.
//
// Accepted proofs, in order:
//
//  1. No credential is configured at all. The daemon is wide open (the
//     middleware lets every request through), so there is no owner to
//     prove yourself to and nothing to protect. This is the first-run and
//     open-tailnet case, and it is why setting a password for the first
//     time on a fresh daemon works without one.
//  2. `currentPassword` in the body matches the configured hash. The
//     normal path: the owner typed their password into Settings.
//  3. Authorization: Bearer <admin token>. Holding the root secret is
//     already total control, and it is the recovery path when the password
//     is forgotten. Crucially this is no longer circular: writing
//     adminToken is itself a credential change, so a caller who only has a
//     cookie can no longer install a token of their own and then present
//     it as proof.
//  4. Authorization: Bearer <Stash API key>. Same reasoning: the gate
//     already accepts it as full access, and it is not obtainable from
//     inside a hijacked session (GET /config masks it).
//
// A session cookie is NOT proof. That is the whole point: a cookie is what
// an attacker is assumed to have.
func (s *Server) provenCredentialChange(r *http.Request, currentPassword string) credentialProof {
	if !s.authRequired() {
		return proofOpenDaemon
	}
	if hash := s.effectivePasswordHash(); hash != "" && currentPassword != "" {
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(currentPassword)) == nil {
			return proofPassword
		}
	}
	auth := r.Header.Get("Authorization")
	if provided := strings.TrimPrefix(auth, "Bearer "); provided != auth {
		if token := s.effectiveAdminToken(); token != "" &&
			subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1 {
			return proofAdminToken
		}
		if key := s.effectiveStashAPIKey(); key != "" &&
			subtle.ConstantTimeCompare([]byte(provided), []byte(key)) == 1 {
			return proofStashAPIKey
		}
	}
	return proofNone
}

// writeCredentialProofRequired answers a refused credential change. The
// body names the fields and carries a stable `code` so the UI can put a
// "current password" prompt in front of the retry instead of showing a raw
// error string. 403 (not 401) on purpose: the request WAS authenticated,
// it just is not authorised for this, and the client's 401 handler would
// otherwise bounce a logged-in user to the login gate.
func writeCredentialProofRequired(w http.ResponseWriter, fields []string) {
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error": "changing " + strings.Join(fields, ", ") +
			" needs your current password (or the API key as a Bearer token)",
		"code":   "credential_proof_required",
		"fields": fields,
	})
}

// logCredentialChange records who changed which credentials and what they
// proved. A credential change is the single most security-relevant thing
// that can happen to this daemon, and until now it left no trace at all.
func (s *Server) logCredentialChange(r *http.Request, fields []string, proof credentialProof) {
	s.logAuth(r, slog.LevelWarn, "credential-change",
		"fields", strings.Join(fields, ","), "proof", string(proof))
}
