package api

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The daemon shipped no framing defence at all: any page could iframe the
// UI and click-jack a logged-in user into a grab or a scene destroy. If
// these headers stop being sent that attack is live again.
func TestSecurityHeadersMiddleware(t *testing.T) {
	h := securityHeadersMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config", nil))

	for _, c := range []struct{ header, want string }{
		{"X-Frame-Options", "DENY"},
		{"X-Content-Type-Options", "nosniff"},
		{"Referrer-Policy", "no-referrer"},
	} {
		if got := rec.Header().Get(c.header); got != c.want {
			t.Errorf("%s = %q, want %q", c.header, got, c.want)
		}
	}
}

// The SPA document carries the CSP, and it must survive the 304
// revalidation branch too: a browser holding a cached copy updates its
// stored headers from the 304, so dropping the policy there would leave
// long-lived tabs running under whatever policy they cached.
func TestServeUISendsCSP(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.serveUI(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	policy := rec.Header().Get("Content-Security-Policy")
	if policy == "" {
		t.Fatal("no Content-Security-Policy on the SPA document")
	}
	for _, want := range []string{
		"frame-ancestors 'none'", // clickjacking, the gap this closes
		"object-src 'none'",      // plugin-based script execution
		"base-uri 'none'",        // <base> hijack of every relative API call
		"form-action 'self'",     // credential post to an attacker's host
	} {
		if !strings.Contains(policy, want) {
			t.Errorf("policy missing %q: %s", want, policy)
		}
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("If-None-Match", uiETag)
	s.serveUI(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("revalidation code = %d, want 304", rec.Code)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != policy {
		t.Errorf("304 policy = %q, want %q", got, policy)
	}
}

// The whole SPA is one inline <script>, so the policy's sha256 has to
// match the embedded bundle byte for byte. Recomputed here by an
// independent scan: if inlineScriptHashes ever drifts from what a browser
// hashes, every user gets a blank page, and this is the only test that
// catches it before they do. (Verified once against Chrome, which reported
// this exact hash as the one it required.)
func TestUICSPHashesTheEmbeddedBundle(t *testing.T) {
	doc := string(indexHTML)
	open := strings.Index(strings.ToLower(doc), "<script")
	if open < 0 {
		t.Fatal("embedded SPA has no <script> — the bundle is not what this policy was built for")
	}
	bodyStart := strings.Index(doc[open:], ">") + open + 1
	bodyEnd := strings.Index(strings.ToLower(doc[bodyStart:]), "</script") + bodyStart
	sum := sha256.Sum256([]byte(doc[bodyStart:bodyEnd]))
	want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"

	if !strings.Contains(uiCSP, want) {
		t.Errorf("policy does not carry the bundle's script hash %s: %s", want, uiCSP)
	}
	// The 'unsafe-inline' fallback exists so a parser miss can never brick
	// the UI, but it must not be what we actually ship.
	scriptSrc := uiCSP[strings.Index(uiCSP, "script-src "):]
	scriptSrc = scriptSrc[:strings.Index(scriptSrc, ";")]
	if strings.Contains(scriptSrc, "'unsafe-inline'") {
		t.Errorf("script-src fell back to 'unsafe-inline': %s", scriptSrc)
	}
}

// The extraction has to agree with an HTML parser about where a script
// starts and ends. Each of these is a case that would silently produce the
// wrong hash (and so a blank UI) if handled naively.
func TestInlineScriptHashes(t *testing.T) {
	hashOf := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	}
	cases := []struct {
		name string
		doc  string
		want []string
	}{
		{
			name: "plain inline script",
			doc:  `<html><body><script>alert(1)</script></body></html>`,
			want: []string{hashOf("alert(1)")},
		},
		{
			name: "attributes on the start tag are not part of the body",
			doc:  `<script type="module" crossorigin>go()</script>`,
			want: []string{hashOf("go()")},
		},
		{
			// vite escapes "</script" inside string literals exactly so the
			// parser doesn't stop there; an unescaped "<script>" in the body
			// must not be mistaken for a second element either.
			name: "markup inside a string literal is body, not markup",
			doc:  `<script>var s="<script>";var e="<\/script>";f()</script>`,
			want: []string{hashOf(`var s="<script>";var e="<\/script>";f()`)},
		},
		{
			// An external script has no inline body to hash — 'self' covers
			// it. Hashing the empty body would emit a source expression that
			// authorises every empty inline script on the page.
			name: "external script is skipped",
			doc:  `<script src="/app.js"></script><script>go()</script>`,
			want: []string{hashOf("go()")},
		},
		{
			name: "crossorigin does not read as src",
			doc:  `<script crossorigin>go()</script>`,
			want: []string{hashOf("go()")},
		},
		{
			name: "tag names are case-insensitive",
			doc:  `<SCRIPT>go()</SCRIPT>`,
			want: []string{hashOf("go()")},
		},
		{
			name: "a longer tag name is not a script",
			doc:  `<scriptish>nope</scriptish><script>go()</script>`,
			want: []string{hashOf("go()")},
		},
		{
			name: "two separate scripts each get a hash",
			doc:  `<script>a()</script><div></div><script>b()</script>`,
			want: []string{hashOf("a()"), hashOf("b()")},
		},
		{
			name: "no inline script at all",
			doc:  `<html><body><div id="root"></div></body></html>`,
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := inlineScriptHashes([]byte(c.doc))
			if len(got) != len(c.want) {
				t.Fatalf("got %d hashes %v, want %d %v", len(got), got, len(c.want), c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("hash %d = %s, want %s", i, got[i], c.want[i])
				}
			}
		})
	}
}

// A document whose inline script the scanner can't find must not produce a
// hash-only policy: that would serve a blank page to everyone. Fail open
// to 'unsafe-inline' instead.
func TestBuildUICSPFallsBackWhenNoScriptFound(t *testing.T) {
	policy := buildUICSP([]byte(`<html><body><div id="root"></div></body></html>`))
	if !strings.Contains(policy, "script-src 'self' 'unsafe-inline'") {
		t.Errorf("expected the unsafe-inline fallback, got: %s", policy)
	}
}
