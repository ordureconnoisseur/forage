package api

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/ordureconnoisseur/forager/internal/cache"
	"github.com/ordureconnoisseur/forager/internal/clientpool"
	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/configstore"
	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/matcher"
	"github.com/ordureconnoisseur/forager/internal/scoring"
	"github.com/ordureconnoisseur/forager/internal/stash"
	"github.com/ordureconnoisseur/forager/internal/stashdb"
	"github.com/ordureconnoisseur/forager/internal/watches"
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
	grabs     *grabs.Repo   // never nil
	watches   *watches.Repo // never nil
	log       *slog.Logger
	version   string
	adoptNow  func(context.Context) int // force-adopt callback (poller.AdoptNow); may be nil

	// torrentGate spaces out .torrent fetches (addTorrentAsync) so a bulk
	// grab or bulk-retry doesn't burst the indexer into HTTP 429s. Zero value
	// is ready to use.
	torrentGate fetchGate

	refreshMu sync.Mutex

	// Lazy-constructed matcher — needs a populated cache, so we wait
	// until the first request that uses it. matcherMu serialises
	// construction attempts; on error we retry next request rather than
	// caching the failure. Invalidated by /config save when Stash or
	// StashDB credentials change.
	matcherMu sync.Mutex
	matcher   *matcher.Matcher

	// ownedCache memoises the full-library owned StashDB-id set. The sweep
	// (FindAllOwnedStashDBSceneIDs) paginates the entire Stash library —
	// seconds for a big library — and /missing-scenes needs it on every
	// load. A short TTL keeps loads instant without going stale in any way
	// that matters (you don't un-own scenes mid-session).
	ownedMu      sync.Mutex
	ownedSet     map[string]bool
	ownedFetched time.Time

	// ownedCopies memoises StashDB scene id → the local copies the user owns,
	// each carrying resolution/size (via the enriched FindAllSceneStashDBIDs
	// sweep). Backs the performer page's "owned scenes" upgrade view, which
	// shows the current quality per scene. Same TTL + lock-across-sweep
	// coalescing as ownedSet.
	ownedCopiesMu      sync.Mutex
	ownedCopies        map[string][]stash.SceneRef
	ownedCopiesFetched time.Time

	// filmoCache memoises each performer's full StashDB filmography
	// (QueryAllScenes), keyed by StashDB performer id. The query paginates
	// a prolific performer's entire catalogue — seconds — and dominates a
	// cold /missing-scenes load. Per-entry TTL; a performer's StashDB
	// filmography changes rarely.
	filmoMu    sync.Mutex
	filmoCache map[string]filmoEntry

	// jobs is the in-memory registry of server-side collection crawls
	// (multi-scene "complete the collection" runs that survive the
	// browser closing). Lost on daemon restart; queued grabs persist.
	jobs *jobStore

	// scorer ranks releases by the user's preference rules (config
	// ReleaseRules). Lazily built from the composed config, rebuilt on a
	// /config save. Guarded by scorerMu.
	scorerMu  sync.Mutex
	scorer    *scoring.Scorer
	scorerSrc string // the ReleaseRules JSON the cached scorer was built from

	// sessionKey signs the stateless forage_token cookie (HMAC-SHA256).
	// Persisted in the meta table and loaded on boot, so cookies stay valid
	// across daemon restarts — a redeploy no longer logs everyone out. This
	// mirrors the *arrs' ASP.NET Data-Protection model: there's no
	// server-side session table; a cookie is trusted iff its signature
	// verifies against this key and its embedded expiry is in the future.
	// 32 bytes of CSPRNG; rotating it (or losing the DB) invalidates every
	// outstanding cookie at once — rotateSessionKey does exactly that when
	// a login credential changes. Guarded by sessionKeyMu (read on every
	// gated request, swapped on rotation).
	sessionKeyMu sync.RWMutex
	sessionKey   []byte

	// sceneTitles memoises StashDB scene id → display title, so the Grabs
	// list can label a scene-attempt group with its real title instead of a
	// bare id. Titles are immutable, so entries effectively never go stale;
	// a negative result (id not found / StashDB down) is cached briefly so a
	// fast-polling list doesn't hammer StashDB. Guarded by sceneTitleMu.
	sceneTitleMu sync.Mutex
	sceneTitles  map[string]sceneTitleEntry
}

type sceneTitleEntry struct {
	title   string
	fetched time.Time
}

type filmoEntry struct {
	scenes  []stashdb.Scene
	fetched time.Time
}

type Options struct {
	DB        *sql.DB
	Pool      *clientpool.Pool
	Bootstrap config.BootstrapConfig
	Store     *configstore.Store
	Grabs     *grabs.Repo
	Watches   *watches.Repo
	Log       *slog.Logger
	Version   string
	// AdoptNow force-adopts untracked forage-category download-client
	// torrents immediately (poller.AdoptNow), bypassing the adoption grace.
	// Backs the Grabs "scan for new downloads" button. Returns the count.
	AdoptNow func(context.Context) int
}

func New(opts Options) *Server {
	return &Server{
		db:         opts.DB,
		pool:       opts.Pool,
		bootstrap:  opts.Bootstrap,
		store:      opts.Store,
		grabs:      opts.Grabs,
		watches:    opts.Watches,
		log:        opts.Log,
		version:    opts.Version,
		adoptNow:   opts.AdoptNow,
		jobs:       newJobStore(),
		sessionKey: loadOrCreateSessionKey(opts.DB, opts.Log),
	}
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(s.corsMiddleware)

	// Public, unauthenticated routes. /healthz must stay open so the
	// plugin can probe reachability and read adminAuthRequired (to know
	// whether to prompt for a token) before any token is entered. The
	// served UI bundle isn't sensitive — it's the public plugin assets,
	// and the API calls it makes carry the token like any other client.
	r.Get("/", s.serveUI)
	r.Get("/healthz", s.healthz)
	// Username+password web login. Public: bcrypt-validates the posted
	// credentials, and on success issues a server-side session + the
	// forage_token cookie. The primary human login path.
	r.Post("/login", s.postLogin)
	// Establishes the forage_token cookie from the API key (admin token)
	// for clients that authenticate by Bearer key rather than password —
	// chiefly so their <img> loads pass the gate. Self-validates the posted
	// token, so it's safe to leave outside the gate.
	r.Post("/session", s.postSession)
	// Clears the forage_token cookie (logout). Public — it only removes the
	// caller's own credential, and a locked-out client has no valid token.
	r.Delete("/session", s.deleteSession)

	// Everything else — all data and action routes — is gated by the
	// optional admin token. The middleware is a no-op when
	// FORAGER_ADMIN_TOKEN is unset (the common private-network case); when
	// set, every route below requires a matching Bearer token. CORS
	// preflight (OPTIONS) is answered by corsMiddleware before this runs,
	// so browsers aren't blocked.
	r.Group(func(r chi.Router) {
		r.Use(s.adminAuthMiddleware)
		r.Get("/performers", s.getPerformers)
		r.Post("/refresh", s.postRefresh)
		r.Post("/refresh/performers", s.postRefreshPerformers)
		r.Post("/match", s.postMatch)
		r.Get("/search", s.getSearch)
		r.Post("/grab", s.postGrab)
		r.Post("/grab/torrent", s.postGrabTorrent)
		r.Post("/grab/torrent/inspect", s.postGrabTorrentInspect)
		r.Get("/grabs", s.getGrabs)
		r.Post("/grabs/adopt", s.postAdopt)
		r.Post("/grabs/retry-failed", s.postRetryAllFailed)
		r.Get("/grabs/{id}/detail", s.getGrabDetail)
		r.Post("/grabs/{id}/match", s.postGrabMatch)
		r.Post("/grabs/{id}/performer", s.postGrabPerformer)
		r.Post("/grabs/{id}/retry", s.postGrabRetry)
		r.Delete("/grabs/{id}", s.deleteGrab)
		r.Post("/duplicates/{id}/resolve", s.postResolveDuplicate)
		r.Post("/jobs", s.postCollectionJob)
		r.Get("/jobs", s.getCollectionJobs)
		r.Get("/jobs/{id}", s.getCollectionJobDetail)
		r.Post("/jobs/{id}/grab", s.postCollectionJobGrab)
		r.Delete("/jobs/{id}", s.deleteCollectionJob)
		r.Post("/watches", s.postWatch)
		r.Get("/watches", s.getWatches)
		r.Delete("/watches/{id}", s.deleteWatch)
		r.Post("/watches/{id}/grab", s.postWatchGrab)
		r.Post("/watches/{id}/dismiss", s.postWatchDismiss)
		r.Get("/missing-scenes", s.getMissingScenes)
		r.Get("/performers/{id}/packs", s.getPerformerPacks)
		r.Get("/scenes/{id}/releases", s.getSceneReleases)
		r.Post("/scenes/{id}/destroy", s.postDestroyScene)
		r.Get("/discover", s.getDiscover)
		r.Get("/indexers", s.getIndexers)
		r.Get("/download-setup", s.getDownloadSetup)
		r.Get("/notifications", s.getNotifications)
		// Stash image proxy — performer portraits + scene screenshots,
		// fetched server-side with the stored Stash API key so the browser
		// never needs Stash creds. Gated like everything else (cookie auth
		// for <img>).
		r.Get("/img/performer/{id}", s.getPerformerImage)
		r.Get("/img/scene/{id}/screenshot", s.getSceneScreenshot)
		r.Get("/config", s.getConfig)
		r.Post("/config", s.postConfig)
		r.Post("/config/test/{section}", s.postConfigTest)
		r.Post("/config/stashdb-from-stash", s.postStashDBFromStash)
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

// ownedTTL is how long a fetched owned-set is reused before re-sweeping.
const ownedTTL = 60 * time.Second

// ownedStashDBSet returns the set of StashDB scene ids the user owns
// anywhere in their library, memoised for ownedTTL. The underlying sweep
// paginates the whole Stash library, so without this every
// /missing-scenes load paid seconds for data that barely changes. The
// lock is held across the (slow) sweep so concurrent loads coalesce onto
// one fetch rather than each launching their own.
func (s *Server) ownedStashDBSet(ctx context.Context) (map[string]bool, error) {
	s.ownedMu.Lock()
	defer s.ownedMu.Unlock()
	if s.ownedSet != nil && time.Since(s.ownedFetched) < ownedTTL {
		return s.ownedSet, nil
	}
	sc := s.pool.Stash()
	if sc == nil {
		return nil, errNotConfigured("stash")
	}
	ids, err := sc.FindAllOwnedStashDBSceneIDs(ctx)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	s.ownedSet = set
	s.ownedFetched = time.Now()
	return set, nil
}

// ownedSceneCopies returns StashDB scene id → the local copies the user owns,
// each with resolution/size, memoised for ownedTTL. Heavier than
// ownedStashDBSet (it carries file metadata), so it's a separate memo used
// only by the performer page's owned-scenes view. The lock is held across the
// sweep so concurrent loads coalesce onto one fetch.
func (s *Server) ownedSceneCopies(ctx context.Context) (map[string][]stash.SceneRef, error) {
	s.ownedCopiesMu.Lock()
	defer s.ownedCopiesMu.Unlock()
	if s.ownedCopies != nil && time.Since(s.ownedCopiesFetched) < ownedTTL {
		return s.ownedCopies, nil
	}
	sc := s.pool.Stash()
	if sc == nil {
		return nil, errNotConfigured("stash")
	}
	copies, err := sc.FindAllSceneStashDBIDs(ctx)
	if err != nil {
		return nil, err
	}
	s.ownedCopies = copies
	s.ownedCopiesFetched = time.Now()
	return s.ownedCopies, nil
}

// invalidateOwned drops both owned-set memos so the next /missing-scenes load
// reflects a just-deleted copy (the duplicates-cleanup destroy path) instead
// of serving a stale owned/duplicate list for up to ownedTTL.
func (s *Server) invalidateOwned() {
	s.ownedMu.Lock()
	s.ownedSet = nil
	s.ownedMu.Unlock()
	s.ownedCopiesMu.Lock()
	s.ownedCopies = nil
	s.ownedCopiesMu.Unlock()
}

// filmoTTL is how long a performer's cached StashDB filmography is reused.
// Longer than ownedTTL: a performer's StashDB catalogue is very stable
// (new scenes arrive over days, not seconds), and there are hundreds of
// performers, so a short window would re-fetch on every revisit.
const filmoTTL = 10 * time.Minute

// performerFilmography returns a performer's full StashDB filmography,
// memoised per performer for filmoTTL. The QueryAllScenes pagination
// dominates a cold /missing-scenes load; caching it makes revisiting (or
// re-opening for the scoped multi-select grab) instant. The lock is held
// across the fetch so concurrent loads of the same performer coalesce.
func (s *Server) performerFilmography(ctx context.Context, sdb *stashdb.Client, stashDBPerformerID string) ([]stashdb.Scene, error) {
	s.filmoMu.Lock()
	defer s.filmoMu.Unlock()
	if s.filmoCache == nil {
		s.filmoCache = map[string]filmoEntry{}
	}
	if e, ok := s.filmoCache[stashDBPerformerID]; ok && time.Since(e.fetched) < filmoTTL {
		return e.scenes, nil
	}
	scenes, err := sdb.QueryAllScenes(ctx, stashdb.SceneQuery{
		PerformerIDs: []string{stashDBPerformerID},
		PerPage:      50,
	}, 5000) // hardCap matches the scene-cache cap so card/page counts agree
	if err != nil {
		return nil, err
	}
	s.filmoCache[stashDBPerformerID] = filmoEntry{scenes: scenes, fetched: time.Now()}
	return scenes, nil
}

// releaseScorer returns the Scorer for the user's current release rules,
// rebuilding it when the rules (config ReleaseRules JSON) change. An empty
// or unparseable rules string falls back to scoring.DefaultRules so
// scoring always works.
func (s *Server) releaseScorer() *scoring.Scorer {
	raw := s.composedConfig().ReleaseRules
	s.scorerMu.Lock()
	defer s.scorerMu.Unlock()
	if s.scorer != nil && s.scorerSrc == raw {
		return s.scorer
	}
	rules := scoring.DefaultRules()
	if strings.TrimSpace(raw) != "" {
		var parsed []scoring.Rule
		if err := json.Unmarshal([]byte(raw), &parsed); err == nil && len(parsed) > 0 {
			rules = parsed
		} else if err != nil {
			s.log.Warn("release rules parse failed; using defaults", "err", err)
		}
	}
	s.scorer = scoring.New(rules)
	s.scorerSrc = raw
	return s.scorer
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
		// No libraryRoot here: /healthz is deliberately unauthenticated (the
		// UI probes it pre-login) and a host filesystem path is more than an
		// anonymous caller should learn. The UI never read it from here; the
		// authenticated /config carries it for Settings/Setup.
		"placerConfigured": s.pool.Placer().Configured(),
		"unconfigured":     !config.IsConfigured(cfg),
		// adminAuthRequired is true when EITHER a password or an API key
		// (admin token) is set — the UI keys its login gate off this.
		"adminAuthRequired": s.authRequired(),
		// passwordSet lets the UI choose which login form to show:
		// username+password (the default) vs the API-key fallback for a
		// token-only daemon.
		"passwordSet": s.effectivePasswordHash() != "",
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
// a restart. The snapshot read is an atomic pointer load — this used to
// run a full config.Compose (every field resolved, fresh maps, duration
// parses) per request, on a path the UI polls every five seconds.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowed := s.pool.Settings().AllowedOrigin
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
