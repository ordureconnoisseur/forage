package api

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/ordureconnoisseur/forager/internal/cache"
	"github.com/ordureconnoisseur/forager/internal/clientpool"
	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/configstore"
	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/matcher"
)

//go:embed ui/index.html
var indexHTML []byte

// Server wires the HTTP layer over the daemon's shared state. After
// the 2026-05-26 hot-swap refactor, clients live in the Pool — handlers
// read fresh references via pool.X() on every request rather than
// holding stale ones across config saves.
type Server struct {
	db        *sql.DB
	pool      *clientpool.Pool
	bootstrap config.BootstrapConfig
	store     *configstore.Store
	grabs     *grabs.Repo // never nil
	log       *slog.Logger
	version   string

	refreshMu sync.Mutex

	// Lazy-constructed matcher — needs a populated cache, so we wait
	// until the first request that uses it. matcherMu serialises
	// construction attempts; on error we retry next request rather than
	// caching the failure. Invalidated by /config save when Stash or
	// StashDB credentials change.
	matcherMu sync.Mutex
	matcher   *matcher.Matcher
}

type Options struct {
	DB        *sql.DB
	Pool      *clientpool.Pool
	Bootstrap config.BootstrapConfig
	Store     *configstore.Store
	Grabs     *grabs.Repo
	Log       *slog.Logger
	Version   string
}

func New(opts Options) *Server {
	return &Server{
		db:        opts.DB,
		pool:      opts.Pool,
		bootstrap: opts.Bootstrap,
		store:     opts.Store,
		grabs:     opts.Grabs,
		log:       opts.Log,
		version:   opts.Version,
	}
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(s.corsMiddleware)
	r.Get("/", s.serveUI)
	r.Get("/healthz", s.healthz)
	r.Get("/performers", s.getPerformers)
	r.Post("/refresh", s.postRefresh)
	r.Post("/match", s.postMatch)
	r.Get("/search", s.getSearch)
	r.Post("/grab", s.postGrab)
	r.Get("/grabs", s.getGrabs)
	r.Get("/grabs/{id}/detail", s.getGrabDetail)
	r.Delete("/grabs/{id}", s.deleteGrab)
	r.Get("/missing-scenes", s.getMissingScenes)
	r.Get("/scenes/{id}/releases", s.getSceneReleases)
	r.Get("/discover", s.getDiscover)

	// /config routes guarded by optional Bearer-token middleware. The
	// middleware is a no-op when FORAGER_ADMIN_TOKEN is unset.
	r.Group(func(r chi.Router) {
		r.Use(s.adminAuthMiddleware)
		r.Get("/config", s.getConfig)
		r.Post("/config", s.postConfig)
		r.Post("/config/test/{section}", s.postConfigTest)
	})
	return r
}

// Matcher returns the lazy-constructed Matcher, building it on the
// first call. Subsequent calls reuse the same instance. Returns 503
// semantics (nil, error) when StashDB isn't configured yet.
func (s *Server) Matcher(ctx context.Context) (*matcher.Matcher, error) {
	s.matcherMu.Lock()
	defer s.matcherMu.Unlock()
	if s.matcher != nil {
		return s.matcher, nil
	}
	sdb := s.pool.StashDB()
	if sdb == nil {
		return nil, errNotConfigured("stashdb")
	}
	m, err := matcher.New(ctx, s.db, sdb)
	if err != nil {
		return nil, err
	}
	s.matcher = m
	return s.matcher, nil
}

// invalidateMatcher clears the cached matcher, forcing the next
// request to rebuild it. Called by /config save when Stash or
// StashDB credentials change.
func (s *Server) invalidateMatcher() {
	s.matcherMu.Lock()
	s.matcher = nil
	s.matcherMu.Unlock()
}

func (s *Server) serveUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	perfAt, _ := cache.PerformerRefreshedAt(r.Context(), s.db)
	studAt, _ := cache.StudioRefreshedAt(r.Context(), s.db)
	var perfCount, studCount int
	_ = s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM performer_cache`).Scan(&perfCount)
	_ = s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM studio_cache`).Scan(&studCount)
	settings := s.pool.Settings()
	cfg := s.composedConfig()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                   true,
		"version":              s.version,
		"performerCount":       perfCount,
		"studioCount":          studCount,
		"performerRefreshedAt": perfAt,
		"studioRefreshedAt":    studAt,
		"stashConfigured":      s.pool.Stash() != nil,
		"stashdbConfigured":    s.pool.StashDB() != nil,
		"prowlarrConfigured":   s.pool.Prowlarr() != nil,
		"qbitConfigured":       s.pool.Qbit() != nil,
		"qbitCategory":         settings.QbitCategory,
		"sabConfigured":        s.pool.Sab() != nil,
		"sabCategory":          settings.SabCategory,
		"placerConfigured":     s.pool.Placer().Configured(),
		"libraryRoot":          s.pool.Placer().LibraryRoot(),
		"unconfigured":         !config.IsConfigured(cfg),
		"adminAuthRequired":    s.bootstrap.AdminToken != "",
	})
}

// composedConfig recomputes the current config (bootstrap overlaid
// with stored JSON). Cheap — both reads are mutex-guarded but rare.
func (s *Server) composedConfig() config.Config {
	cfg, _ := config.Compose(s.bootstrap, s.store.Get())
	return cfg
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// errNotConfigured is returned by handlers that need a client the
// Pool can't supply (because the relevant section's URL/key is unset).
type notConfiguredErr struct{ section string }

func (e notConfiguredErr) Error() string { return e.section + " not configured" }
func errNotConfigured(section string) error {
	return notConfiguredErr{section: section}
}

// corsMiddleware reads the allowed-origin from the Pool's snapshot on
// every request so changes from the /config endpoint propagate without
// a restart.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowed := s.composedConfig().AllowedOrigin
		origin := r.Header.Get("Origin")
		switch {
		case allowed == "*":
			w.Header().Set("Access-Control-Allow-Origin", "*")
		case origin != "" && origin == allowed:
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
