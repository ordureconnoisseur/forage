package api

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/ordureconnoisseur/forager/internal/configstore"
	"github.com/ordureconnoisseur/forager/internal/managed"
)

// Managed-Prowlarr endpoints: status for the wizard/Settings, an install
// trigger, and the authenticated /prowlarr/ reverse proxy that is the only
// way in — the managed instance binds to localhost and delegates auth to
// forage (AuthenticationMethod=External), so forage's own login is the
// gate for the Prowlarr UI too.

// RunManagedProwlarr boot-starts supervision of an already-installed
// managed Prowlarr and retains ctx so a wizard-triggered install can start
// supervision under the same lifetime. Called from main like the other
// loops; no-op when no manager is wired (Docker, tests).
func (s *Server) RunManagedProwlarr(ctx context.Context) {
	if s.managed == nil {
		return
	}
	s.managedMu.Lock()
	s.managedCtx = ctx
	s.managedMu.Unlock()
	if s.managed.Installed() {
		if err := s.managed.Start(ctx); err != nil {
			s.log.Error("managed prowlarr boot start", "err", err)
		}
	}
	<-ctx.Done()
	s.managed.Stop()
}

func (s *Server) getManagedProwlarr(w http.ResponseWriter, r *http.Request) {
	if s.managed == nil {
		writeJSON(w, http.StatusOK, managed.Status{Supported: false, State: managed.StateAbsent})
		return
	}
	writeJSON(w, http.StatusOK, s.managed.Status())
}

// postManagedProwlarrInstall kicks the download+install+start chain in the
// background; the UI polls GET /managed/prowlarr for progress. On success
// the managed instance's URL and API key are written into the stored
// config — from then on it's an ordinary Prowlarr connection that happens
// to live at 127.0.0.1.
func (s *Server) postManagedProwlarrInstall(w http.ResponseWriter, r *http.Request) {
	if s.managed == nil || !managed.Supported() {
		writeErr(w, http.StatusConflict, "managed Prowlarr is not available here — on Docker, use the bundled compose stack instead")
		return
	}
	s.managedMu.Lock()
	runCtx := s.managedCtx
	s.managedMu.Unlock()
	if runCtx == nil {
		writeErr(w, http.StatusServiceUnavailable, "daemon is still starting — try again in a moment")
		return
	}
	go func() {
		defer s.recoverPanic("managed prowlarr install")
		if err := s.managed.Install(runCtx); err != nil {
			return // state carries the error; the UI poll surfaces it
		}
		if err := s.managed.Start(runCtx); err != nil {
			s.log.Error("managed prowlarr start after install", "err", err)
			return
		}
		u, key := s.managed.URL(), s.managed.APIKey()
		if err := s.store.Set(configstore.Patch{ProwlarrURL: &u, ProwlarrAPIKey: &key}); err != nil {
			s.log.Error("persist managed prowlarr connection", "err", err)
			return
		}
		s.pool.Reload(s.composedConfig())
		s.log.Info("managed prowlarr wired into config", "url", u)
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

// prowlarrProxy reverse-proxies /prowlarr/* to the managed instance. It
// sits inside the authenticated route group: reaching Prowlarr's UI or
// API through forage requires forage's login, which is exactly the
// External-auth arrangement the seeded config declares.
func (s *Server) prowlarrProxy() http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			target := ""
			if s.managed != nil {
				target = s.managed.ProxyTarget()
			}
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = target
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "managed Prowlarr is not running", http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.managed == nil || s.managed.ProxyTarget() == "" {
			http.Error(w, "no managed Prowlarr here", http.StatusNotFound)
			return
		}
		// An empty Host would make the proxy dial "http://": guard above
		// covers it, but keep the URL sane for the error handler path too.
		if _, err := url.Parse("http://" + s.managed.ProxyTarget()); err != nil {
			http.Error(w, "managed Prowlarr address invalid", http.StatusInternalServerError)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}
