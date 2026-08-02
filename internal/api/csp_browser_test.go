package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestCSPDoesNotBreakTheUI loads the real embedded SPA in a real browser,
// under the real uiCSP header, and checks two things: that React actually
// rendered, and that the browser logged no policy violation at all.
//
// It exists because a Content-Security-Policy cannot be validated by
// reading it. The failure mode of the hash approach is total and silent:
// one wrong byte and every user gets a blank page, with every Go test
// still green. So this drives Chrome.
//
// Skipped unless FORAGE_CSP_BROWSER points at a Chrome/Chromium binary, so
// CI (which has no browser) is unaffected. Run it after any change to the
// bundle or the policy:
//
//	FORAGE_CSP_BROWSER="/c/Program Files/Google/Chrome/Application/chrome.exe" \
//	  go test ./internal/api -run TestCSPDoesNotBreakTheUI -v
//
// The "policy that should break it" case is the control, and it is not
// decoration. Without it, "the page rendered" proves nothing — a run where
// Chrome ignored the header entirely would pass just as happily. It
// asserts that a policy which SHOULD blank the app does blank it, and that
// the violations get logged, which is what makes the silence in the first
// case meaningful.
//
// Recorded result on 2026-08-02, Chrome 141, bundle 520,588 bytes: the
// shipped policy rendered the full app shell (5,302 bytes of DOM under
// #root) and logged no violations; the control left #root empty and logged
// three (inline script blocked, inline style blocked, data: favicon
// blocked). Chrome's own error text for the blocked script names the hash
// it wanted, and it matched uiScriptSrc exactly — an independent check of
// the hash by the thing that has to accept it.
func TestCSPDoesNotBreakTheUI(t *testing.T) {
	chrome := os.Getenv("FORAGE_CSP_BROWSER")
	if chrome == "" {
		t.Skip("set FORAGE_CSP_BROWSER to a Chrome binary to run the browser check")
	}
	if _, err := os.Stat(chrome); err != nil {
		t.Skipf("FORAGE_CSP_BROWSER not usable: %v", err)
	}

	cases := []struct {
		name           string
		csp            string
		wantRender     bool
		wantViolations bool
	}{
		{"the policy forage ships", uiCSP, true, false},
		// Control: 'self' forbids inline scripts, so the bundle must not
		// run, #root must stay empty, and Chrome must say so.
		{"policy that should break it", "default-src 'self'; script-src 'self'", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(cspTestHandler(c.csp))
			defer srv.Close()

			dom, violations := runChrome(t, chrome, srv.URL)
			if got := renderedSomething(dom); got != c.wantRender {
				t.Errorf("app rendered = %v, want %v\npolicy: %s\nDOM near #root:\n%s",
					got, c.wantRender, c.csp, nearRoot(dom))
			}
			if got := len(violations) > 0; got != c.wantViolations {
				t.Errorf("CSP violations = %v, want %v\npolicy: %s\n%s",
					got, c.wantViolations, c.csp, strings.Join(violations, "\n"))
			}
		})
	}
}

// cspTestHandler serves the embedded SPA under a given policy, plus enough
// of /healthz for the app to get past its boot probe and paint something.
func cspTestHandler(csp string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "version": "csp-test", "unconfigured": true,
			"adminAuthRequired": false, "passwordSet": false,
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	return mux
}

// runChrome loads url in headless Chrome and returns the serialized DOM
// after scripts have run, plus any Content-Security-Policy violations the
// browser logged to the console.
func runChrome(t *testing.T, chrome, url string) (string, []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, chrome,
		"--headless=new",
		"--disable-gpu",
		"--no-sandbox",
		"--no-first-run",
		"--disable-extensions",
		"--user-data-dir="+t.TempDir(),
		"--enable-logging=stderr",
		"--v=1",
		"--virtual-time-budget=10000",
		"--dump-dom",
		url,
	)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil && out.Len() == 0 {
		t.Fatalf("chrome: %v\n%s", err, head(errb.String(), 2000))
	}
	var violations []string
	for _, line := range strings.Split(errb.String(), "\n") {
		if strings.Contains(strings.ToLower(line), "content security policy") {
			violations = append(violations, strings.TrimSpace(line))
		}
	}
	return out.String(), violations
}

// renderedSomething reports whether React mounted anything under #root.
// An empty <div id="root"></div> is exactly what a blocked bundle leaves
// behind, so the presence of children is the signal.
func renderedSomething(dom string) bool {
	inner, ok := rootInner(dom)
	return ok && strings.TrimSpace(inner) != ""
}

func rootInner(dom string) (string, bool) {
	i := strings.Index(dom, `id="root"`)
	if i < 0 {
		return "", false
	}
	rest := dom[i:]
	j := strings.Index(rest, ">")
	if j < 0 {
		return "", false
	}
	rest = rest[j+1:]
	end := strings.Index(rest, "</div>")
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}

func nearRoot(dom string) string {
	inner, ok := rootInner(dom)
	if !ok {
		return "(no #root in document)"
	}
	return head(inner, 400)
}

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return fmt.Sprintf("%s… (%d bytes total)", s[:n], len(s))
}
