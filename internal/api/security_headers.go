// Security response headers.
//
// forage served none of these, so its UI could be framed by any page and
// clickjacked: a hostile tab on the same browser could stack an invisible
// frame of forage over its own buttons and have you click "delete" on your
// own library. Nothing in forage is framed by design — the Stash launcher
// opens the daemon in a new tab (plugin/public/forage.entry.js uses
// target="_blank"), so denying framing outright costs nothing.
//
// Two headers are global (every response, including the managed-Prowlarr
// proxy) and one is not:
//
//   - X-Content-Type-Options / Referrer-Policy / X-Frame-Options are set on
//     every response by securityHeadersMiddleware. Harmless everywhere;
//     Prowlarr may set its own copies through the proxy, and a duplicate
//     nosniff or a conflicting X-Frame-Options both resolve the safe way.
//   - Content-Security-Policy is set ONLY on the SPA document (serveUI).
//     Setting it globally would apply forage's script policy to
//     Prowlarr's UI coming back through /prowlarr/*, whose scripts are not
//     in forage's hash list, and break it.
package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
)

// securityHeadersMiddleware sets the headers that are safe on every
// response.
//
//   - X-Content-Type-Options: nosniff — the API answers JSON, and the
//     image proxy streams bytes it fetched from Stash. Without nosniff a
//     browser is free to re-interpret either as HTML.
//   - Referrer-Policy: no-referrer — forage URLs carry performer and scene
//     ids, and the UI links out to Stash and to indexers. No third party
//     needs to learn which performer's page you were on.
//   - X-Frame-Options: DENY — the belt to CSP frame-ancestors' braces,
//     and the only one of the two that covers non-SPA responses.
func (s *Server) securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// ── Content-Security-Policy for the SPA ─────────────────────────────
//
// The SPA is one self-contained file: vite-plugin-singlefile inlines the
// entire bundle into a single <script type="module"> and the whole
// stylesheet into a single <style>. So the naive policy — script-src
// 'self' — blanks the app completely, and script-src 'unsafe-inline'
// permits exactly the thing CSP exists to stop.
//
// The way out is a hash: CSP matches an inline script by the sha256 of its
// exact body text, which is computed here at init from the embedded
// document, so it tracks every rebuild automatically with nothing to
// remember. Styles keep 'unsafe-inline' because React writes style
// attributes on elements at runtime and no hash can cover those.

// uiInlineScript returns the body of the document's single inline script —
// the bytes a browser hashes for CSP — and whether the document actually
// looks the way this extraction assumes.
//
// The end of a script element is defined by the HTML parser as the first
// "</script", so taking the first one is not a guess, it is the same rule
// the browser applies. The start is the only real assumption, and it is
// checked: the document must contain exactly ONE "</script", which means
// exactly one script element, which means the first "<script" is
// necessarily that element's tag. (Today's bundle contains a second
// "<script" at byte 109443, inside a React string literal written as
// `<script><\/script>`; because its closer is escaped it counts as script
// body, exactly as a browser reads it, and the "</script" count stays 1.)
func uiInlineScript(doc []byte) ([]byte, bool) {
	if bytes.Count(doc, []byte("</script")) != 1 {
		return nil, false
	}
	open := bytes.Index(doc, []byte("<script"))
	if open < 0 {
		return nil, false
	}
	tagEnd := bytes.IndexByte(doc[open:], '>')
	if tagEnd < 0 {
		return nil, false
	}
	body := doc[open+tagEnd+1 : bytes.Index(doc, []byte("</script"))]
	if len(body) == 0 {
		return nil, false
	}
	return body, true
}

// uiScriptSrc is the script-src value for the SPA, and uiCSPHashed records
// whether it came out as a hash.
//
// The fallback matters: if a future bundle stops matching the shape above
// (a second inline script, an external one), hashing whatever the scan
// sliced would ship a confidently wrong hash and a blank UI for every
// user. Failing open to 'unsafe-inline' keeps the app working and keeps
// frame-ancestors, object-src and base-uri — the clickjacking controls,
// which is what this change is for — fully in force. CI notices instead:
// TestUICSPIsHashed fails the build the moment the policy silently
// weakens.
var uiScriptSrc, uiCSPHashed = func() (string, bool) {
	body, ok := uiInlineScript(indexHTML)
	if !ok {
		return "'unsafe-inline'", false
	}
	sum := sha256.Sum256(body)
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'", true
}()

// uiCSP is the policy served with the SPA document.
//
// Why each directive is what it is:
//
//	default-src 'self'      backstop for anything not named below —
//	                        covers media-src, worker-src, manifest-src.
//	                        Verified there is no <video>/<audio>, no
//	                        `new Worker`, and no manifest in plugin/src.
//	script-src <hash>       the inlined bundle, and nothing else. No
//	                        'unsafe-eval': there is no eval or
//	                        `new Function` anywhere in plugin/src.
//	style-src 'unsafe-inline'  the inlined <style>, plus the style
//	                        attributes React sets at runtime, which a
//	                        hash cannot cover.
//	img-src 'self' data: https:  portraits and screenshots are proxied
//	                        through the daemon ('self'), the favicon is a
//	                        data: URI, and older grab records still carry
//	                        absolute StashDB CDN URLs.
//	connect-src 'self' https: http:  the API base is user-configurable
//	                        (Settings → Connection), so it is not
//	                        necessarily this origin. Deliberately loose,
//	                        and honestly so: this directive is not doing
//	                        much work here.
//	frame-ancestors 'none'  the clickjacking control this whole file is
//	                        for.
//	frame-src 'none'        forage frames nothing.
//	object-src 'none'       no plugins, ever.
//	base-uri 'self'         stops an injected <base> re-pointing relative
//	                        URLs.
//	form-action 'self'      the app posts via fetch; no form should
//	                        submit anywhere else.
var uiCSP = "default-src 'self'; " +
	"script-src " + uiScriptSrc + "; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: https:; " +
	"font-src 'self' data:; " +
	"connect-src 'self' https: http:; " +
	"frame-ancestors 'none'; " +
	"frame-src 'none'; " +
	"object-src 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'"
