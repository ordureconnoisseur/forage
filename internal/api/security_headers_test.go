package api

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSecurityHeadersOnEveryResponse: the headers have to be on the
// responses an attacker would use, not just the happy ones — the SPA
// document, the unauthenticated /healthz, a 401, and the CORS preflight
// that corsMiddleware answers before any handler runs.
func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	s := gatedServer(t)
	router := s.Router()

	cases := []struct {
		name string
		req  *http.Request
	}{
		{"SPA document", httptest.NewRequest(http.MethodGet, "/", nil)},
		{"unauthenticated healthz", httptest.NewRequest(http.MethodGet, "/healthz", nil)},
		{"401 from the gate", httptest.NewRequest(http.MethodGet, "/watches", nil)},
		{"CORS preflight", httptest.NewRequest(http.MethodOptions, "/watches", nil)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, c.req)
			for header, want := range map[string]string{
				"X-Content-Type-Options": "nosniff",
				"Referrer-Policy":        "no-referrer",
				"X-Frame-Options":        "DENY",
			} {
				if got := rec.Header().Get(header); got != want {
					t.Errorf("%s = %q, want %q (status %d)", header, got, want, rec.Code)
				}
			}
		})
	}
}

// TestSPACarriesCSPIncludingOn304: a revalidation sends no body, but the
// browser reuses the cached document, so a 304 that dropped the policy
// would leave that reused document unprotected — and after the no-cache
// ETag work landed, the 304 is the common case, not the rare one.
func TestSPACarriesCSPIncludingOn304(t *testing.T) {
	s := gatedServer(t)
	router := s.Router()

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("no Content-Security-Policy on the SPA document")
	}
	if !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("policy has no frame-ancestors 'none' — clickjacking is what this is for: %s", csp)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("If-None-Match", rec.Header().Get("ETag"))
	rec304 := httptest.NewRecorder()
	router.ServeHTTP(rec304, req)
	if rec304.Code != http.StatusNotModified {
		t.Fatalf("revalidation status = %d, want 304", rec304.Code)
	}
	if got := rec304.Header().Get("Content-Security-Policy"); got != csp {
		t.Errorf("304 policy = %q, want the same as the 200's", got)
	}
}

// TestCSPIsNotSetOnAPIResponses. The SPA's script-src names one hash; the
// managed-Prowlarr UI proxied through /prowlarr/* runs scripts that are
// not in it. A global CSP would break that UI, which is why the policy is
// attached in serveUI rather than in the middleware. This pins that
// decision so nobody "tidies" it into the middleware later.
func TestCSPIsNotSetOnAPIResponses(t *testing.T) {
	s := gatedServer(t)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if got := rec.Header().Get("Content-Security-Policy"); got != "" {
		t.Fatalf("CSP on a non-SPA response: %q", got)
	}
}

// TestUICSPIsHashed is the canary for the fail-open in
// security_headers.go. If a future bundle stops matching the shape the
// extraction assumes, script-src silently degrades to 'unsafe-inline' —
// the app keeps working, which is the right call at runtime, but nobody
// would ever notice. Here, CI notices.
func TestUICSPIsHashed(t *testing.T) {
	if !uiCSPHashed {
		t.Fatalf("script-src fell back to %s: the embedded bundle no longer has "+
			"exactly one inline script, so the CSP no longer restricts scripts. "+
			"Re-read uiInlineScript against the new bundle shape.", uiScriptSrc)
	}
	if strings.Contains(uiCSP, "'unsafe-eval'") {
		t.Error("policy allows unsafe-eval")
	}
	// Per directive, not by scanning the whole policy: style-src legitimately
	// carries 'unsafe-inline' (React writes style attributes), and a naive
	// substring search from "script-src" to the end of the string finds it
	// there and reports a script-src problem that does not exist.
	if got := cspDirective(uiCSP, "script-src"); strings.Contains(got, "'unsafe-inline'") {
		t.Errorf("script-src allows unsafe-inline: %q", got)
	}
	for _, want := range []string{"frame-ancestors 'none'", "object-src 'none'", "base-uri 'self'"} {
		if !strings.Contains(uiCSP, want) {
			t.Errorf("policy is missing %q", want)
		}
	}
}

// cspDirective returns one directive's value from a policy string.
func cspDirective(policy, name string) string {
	for _, d := range strings.Split(policy, ";") {
		d = strings.TrimSpace(d)
		if strings.HasPrefix(d, name+" ") {
			return strings.TrimPrefix(d, name+" ")
		}
	}
	return ""
}

// TestUIInlineScriptSliceMatchesTheBrowser recomputes the hash the way a
// browser would, from an independent scan, and requires the same answer.
//
// It is worth being explicit about what this does and does not prove.
// Both scans stop the script body at the first "</script", which is not a
// shared assumption but the HTML parser's actual rule, so agreeing there
// is meaningful. What it cannot catch is a bundle whose SHAPE changed in a
// way both scans read identically and wrongly. Only a browser can settle
// that, which is what TestCSPDoesNotBreakTheUI is for.
func TestUIInlineScriptSliceMatchesTheBrowser(t *testing.T) {
	doc := string(indexHTML)
	if n := strings.Count(doc, "</script"); n != 1 {
		t.Fatalf("document has %d '</script' occurrences; the extraction assumes 1", n)
	}
	open := strings.Index(doc, "<script")
	if open < 0 {
		t.Fatal("no <script in the document")
	}
	body := doc[open+strings.Index(doc[open:], ">")+1 : strings.Index(doc, "</script")]
	sum := sha256.Sum256([]byte(body))
	want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	if uiScriptSrc != want {
		t.Fatalf("script-src = %s, independent scan says %s", uiScriptSrc, want)
	}
}

// TestUIInlineScriptRejectsShapesItCannotRead: the fail-open must actually
// fire on the cases that would otherwise produce a confidently wrong hash
// and a blank UI for every user.
func TestUIInlineScriptRejectsShapesItCannotRead(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{"two inline scripts", `<html><script>a()</script><script>b()</script></html>`},
		{"an external script alongside", `<html><script src="x.js"></script><script>a()</script></html>`},
		{"no script at all", `<html><body>hi</body></html>`},
		{"unterminated script", `<html><script>a()`},
		{"empty script body", `<html><script></script></html>`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := uiInlineScript([]byte(c.doc)); ok {
				t.Fatal("extraction claimed success on a shape it cannot read")
			}
		})
	}
	// And the shape it can: one inline script, with an escaped "</script"
	// inside a string literal, which is what today's bundle contains.
	doc := `<html><script type="module">var s = "<script><\/script>";</script></html>`
	body, ok := uiInlineScript([]byte(doc))
	if !ok {
		t.Fatal("extraction rejected the shape the bundle actually has")
	}
	if string(body) != `var s = "<script><\/script>";` {
		t.Fatalf("body = %q", body)
	}
}
