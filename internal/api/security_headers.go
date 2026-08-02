package api

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
)

// ── Security response headers ───────────────────────────────────────
//
// forage shipped none of these until the 2026-08 audit. The gap that
// mattered was framing: with no X-Frame-Options and no CSP
// frame-ancestors, any page could load the daemon UI in an invisible
// iframe and click-jack a logged-in user into a grab or a scene destroy.
// The session cookie is SameSite=Lax, which does NOT stop framing — Lax
// suppresses cookies on cross-site POSTs, but a framed same-site
// navigation still carries them, and every destructive action in forage
// is driven by in-page clicks rather than form posts.
//
// The headers are set on every response rather than just the SPA
// document: the JSON API benefits from nosniff (a response a browser
// sniffs as HTML is an XSS primitive), and the managed-Prowlarr reverse
// proxy is a full third-party UI that should not be framable either. The
// proxy copies upstream headers on top of ours, so a Prowlarr response
// can end up carrying both values; DENY plus SAMEORIGIN resolves to deny,
// which is what we want, and nothing in forage frames Prowlarr.

// securityHeadersMiddleware sets the headers that apply to every response.
// The Content-Security-Policy is NOT set here: it is document-specific and
// would be meaningless (or actively wrong, for the proxied Prowlarr UI) on
// anything but the SPA, so serveUI sets it.
func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// Content-type sniffing turns an API response an attacker can
		// influence into script; forage always sets an accurate type.
		h.Set("X-Content-Type-Options", "nosniff")
		// Belt-and-braces with the CSP frame-ancestors below: X-Frame-Options
		// is what covers a browser that ignores or fails to parse the CSP.
		h.Set("X-Frame-Options", "DENY")
		// no-referrer, not the usual strict-origin-when-cross-origin: the SPA
		// links out to stashdb.org, and the daemon's hostname is typically a
		// private tailnet or reverse-proxy name that has no business reaching
		// a third-party site.
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// uiCSP is the Content-Security-Policy served with the SPA document,
// computed once at init from the embedded bundle (same reasoning as
// uiETag: it must track the build, not a hand-maintained constant).
var uiCSP = buildUICSP(indexHTML)

// buildUICSP assembles the SPA's policy.
//
// The hard constraint is that the SPA is one self-contained HTML file:
// vite-plugin-singlefile inlines the whole ~400KB module script and the
// ~120KB stylesheet into <script> and <style> elements, so a textbook
// "script-src 'self'" policy blanks the entire UI. Rather than fall back
// to 'unsafe-inline' for scripts, the policy carries a sha256 hash of each
// inline script's exact text — computed from the embedded document, so it
// stays correct across rebuilds with no build step to keep in sync.
//
// Styles still need 'unsafe-inline': React writes style="" attributes on
// elements throughout the UI, and inline style ATTRIBUTES are not covered
// by hashes (that would need 'unsafe-hashes' plus a hash per distinct
// attribute value, which is not workable for computed styles).
func buildUICSP(doc []byte) string {
	// 'self' alongside the hashes so a future build that emits a real
	// same-origin script file still loads.
	script := []string{"'self'"}
	if hashes := inlineScriptHashes(doc); len(hashes) > 0 {
		script = append(script, hashes...)
	} else {
		// No inline script found. Either the bundle genuinely has none, or
		// the extraction below failed to recognise it — and if it failed,
		// a hash-only policy would serve a blank page to every user. Fail
		// open to 'unsafe-inline' instead: weaker, never broken.
		script = append(script, "'unsafe-inline'")
	}
	return strings.Join([]string{
		"default-src 'self'",
		"script-src " + strings.Join(script, " "),
		"style-src 'self' 'unsafe-inline'",
		// Scene and performer art on the Discover and Missing views comes
		// straight from StashDB's image hosts (image_url), not through the
		// daemon's /img proxy, so remote images have to be allowed. data:
		// covers the inline favicon and the chevron mask.
		"img-src 'self' data: blob: https: http:",
		"font-src 'self' data:",
		// The API base is user-configurable (Settings → forage URL, kept in
		// localStorage), so the SPA served by one daemon may legitimately be
		// pointed at another host. Pinning this to 'self' would break that.
		"connect-src *",
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; ")
}

// inlineScriptHashes returns a CSP source expression ('sha256-…') for every
// inline <script> element in doc.
//
// The scan follows the HTML parser's own script-data rule so the bytes we
// hash are exactly the bytes the browser hashes: content runs from the end
// of the start tag to the first case-insensitive "</script" followed by
// whitespace, "/" or ">". Bundlers escape that sequence inside string
// literals ("<\/script"), which is why a byte scan is sufficient here and
// a full HTML parser is not.
func inlineScriptHashes(doc []byte) []string {
	src := string(doc)
	lower := strings.ToLower(src)
	var out []string
	for i := 0; i < len(src); {
		rel := strings.Index(lower[i:], "<script")
		if rel < 0 {
			break
		}
		tagStart := i + rel
		after := tagStart + len("<script")
		if after >= len(src) || !isTagNameEnd(src[after]) {
			// "<scriptish" — not a script tag.
			i = after
			continue
		}
		tagEnd, attrs, ok := endOfStartTag(src, after)
		if !ok {
			break
		}
		bodyStart := tagEnd + 1
		bodyEnd, closeEnd, found := endOfScriptData(src, lower, bodyStart)
		if !found {
			break
		}
		// A script with a src attribute has no inline content to hash; the
		// URL it loads is governed by the 'self' source expression instead.
		if !hasSrcAttr(attrs) && bodyEnd > bodyStart {
			sum := sha256.Sum256([]byte(src[bodyStart:bodyEnd]))
			out = append(out, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
		}
		i = closeEnd
	}
	return out
}

// isTagNameEnd reports whether c terminates a tag name, i.e. whether
// "<script" was a real tag rather than the prefix of a longer name.
func isTagNameEnd(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\f', '/', '>':
		return true
	}
	return false
}

// endOfStartTag finds the ">" closing a start tag, skipping over quoted
// attribute values so an attribute containing ">" can't end the tag early.
// Returns its index plus the raw attribute text.
func endOfStartTag(src string, from int) (int, string, bool) {
	var quote byte
	for i := from; i < len(src); i++ {
		c := src[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			return i, src[from:i], true
		}
	}
	return 0, "", false
}

// endOfScriptData finds where a script element's content ends: the first
// "</script" followed by whitespace, "/" or ">". Returns the content end,
// the index just past the closing tag, and whether one was found.
func endOfScriptData(src, lower string, from int) (int, int, bool) {
	for i := from; ; {
		rel := strings.Index(lower[i:], "</script")
		if rel < 0 {
			return 0, 0, false
		}
		end := i + rel
		after := end + len("</script")
		if after < len(src) && !isTagNameEnd(src[after]) {
			i = after
			continue
		}
		closeEnd, _, ok := endOfStartTag(src, after)
		if !ok {
			return 0, 0, false
		}
		return end, closeEnd + 1, true
	}
}

// hasSrcAttr reports whether a start tag's attribute text declares src=,
// which marks the script as external rather than inline.
func hasSrcAttr(attrs string) bool {
	l := strings.ToLower(attrs)
	for {
		idx := strings.Index(l, "src")
		if idx < 0 {
			return false
		}
		// Must be a whole attribute name: preceded by a separator and
		// followed by "=" (or whitespace before one). Guards against
		// crossorigin/nomodule-style names that merely contain "src".
		before := byte(' ')
		if idx > 0 {
			before = l[idx-1]
		}
		rest := strings.TrimLeft(l[idx+3:], " \t\n\r\f")
		if isTagNameEnd(before) && strings.HasPrefix(rest, "=") {
			return true
		}
		l = l[idx+3:]
	}
}
