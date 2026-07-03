package api

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
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
	"github.com/ordureconnoisseur/forager/internal/rss"
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
	rss       *rss.Repo     // never nil; RSS-sync watermark store
	log       *slog.Logger
	version   string
	adoptNow  func(context.Context) (int, int) // force-adopt callback (poller.AdoptNow) → (adopted, skippedRecent); may be nil

	// torrentGate spaces out .torrent fetches (addTorrentAsync) so a bulk
	// grab or bulk-retry doesn't burst the indexer into HTTP 429s. Zero value
	// is ready to use.
	torrentGate fetchGate

	// sabGate does the same for usenet adds (addUsenetAsync): SAB fetches the
	// NZB from the indexer during addurl, so a bulk grab bursts the indexer
	// exactly like torrent fetches do. Zero value is ready to use.
	sabGate fetchGate

	refreshMu sync.Mutex
	// sceneSyncMu (TryLock) enforces ONE background scene-cache sync at a time.
	sceneSyncMu sync.Mutex

	// searchNow state. searchNowMu (TryLock) enforces ONE manual "search now"
	// at a time, so repeated clicks can't stack concurrent Prowlarr load (the
	// flood-protection that matters most here). searching is the set of watch
	// ids currently being re-searched by that run — overlaid onto the watch
	// list as a transient `searching` flag so the UI can show a spinner.
	searchNowMu sync.Mutex
	searchingMu sync.Mutex
	searching   map[string]bool

	// Lazy-constructed matcher — needs a populated cache, so we wait
	// until the first request that uses it. matcherMu serialises
	// construction attempts; on error we retry next request rather than
	// caching the failure. Invalidated by /config save when Stash or
	// StashDB credentials change.
	matcherMu sync.Mutex
	matcher   *matcher.Matcher

	// ownedCopies memoises StashDB scene id → the local copies the user owns,
	// each carrying resolution/size (via the enriched FindAllSceneStashDBIDs
	// sweep), memoised for ownedTTL. Backs the performer page's missing/owned/
	// dupes views. The lock is held across the (slow) library sweep so
	// concurrent loads coalesce onto one fetch rather than each launching their
	// own.
	ownedCopiesMu      sync.Mutex
	ownedCopies        map[string][]stash.SceneRef
	ownedCopiesFetched time.Time

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
	// Backs the Grabs "scan for new downloads" button. Returns the count
	// adopted and the count skipped only for being too fresh.
	AdoptNow func(context.Context) (int, int)
}

func New(opts Options) *Server {
	return &Server{
		db:         opts.DB,
		pool:       opts.Pool,
		bootstrap:  opts.Bootstrap,
		store:      opts.Store,
		grabs:      opts.Grabs,
		watches:    opts.Watches,
		rss:        rss.NewRepo(opts.DB),
		log:        opts.Log,
		version:    opts.Version,
		adoptNow:   opts.AdoptNow,
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
		r.Get("/studios", s.getStudios)
		r.Post("/refresh", s.postRefresh)
		r.Post("/refresh/performers", s.postRefreshPerformers)
		r.Post("/refresh/studios", s.postRefreshStudios)
		r.Post("/refresh/scenes", s.postRefreshScenes)
		r.Post("/match", s.postMatch)
		r.Get("/search", s.getSearch)
		r.Post("/grab", s.postGrab)
		r.Post("/grab/torrent", s.postGrabTorrent)
		r.Post("/grab/torrent/inspect", s.postGrabTorrentInspect)
		r.Get("/grabs", s.getGrabs)
		r.Post("/grabs/adopt", s.postAdopt)
		r.Post("/grabs/retry-failed", s.postRetryAllFailed)
		r.Get("/grabs/{id}/detail", s.getGrabDetail)
		r.Get("/grabs/{id}/pack-scenes", s.getPackScenes)
		r.Post("/grabs/{id}/apply-performer", s.postApplyPerformer)
		r.Get("/grabs/{id}/match-candidates", s.getGrabMatchCandidates)
		r.Post("/grabs/{id}/match", s.postGrabMatch)
		r.Post("/grabs/{id}/performer", s.postGrabPerformer)
		r.Post("/grabs/{id}/retry", s.postGrabRetry)
		r.Delete("/grabs/{id}", s.deleteGrab)
		r.Post("/duplicates/{id}/resolve", s.postResolveDuplicate)
		r.Post("/watches", s.postWatch)
		r.Post("/watches/batch", s.postWatchBatch)
		r.Post("/watches/search-now", s.postWatchSearchNow)
		r.Get("/watches", s.getWatches)
		r.Delete("/watches/batch/{batchId}", s.clearBatch)
		r.Delete("/watches/{id}", s.deleteWatch)
		r.Post("/watches/{id}/grab", s.postWatchGrab)
		r.Post("/watches/{id}/grab-candidate", s.postWatchGrabCandidate)
		r.Post("/watches/{id}/dismiss", s.postWatchDismiss)
		r.Post("/watches/{id}/redo", s.postWatchRedo)
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
		r.Get("/img/studio/{id}", s.getStudioImage)
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

// ownedSceneCopies returns StashDB scene id → the local copies the user owns,
// each with resolution/size, memoised for ownedTTL. Backs the performer
// page's missing/owned/dupes views (it carries file metadata). The lock is
// held across the sweep so concurrent loads coalesce onto one fetch.
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

// invalidateOwned drops the owned-copies memo so the next /missing-scenes load
// reflects a just-deleted copy (the duplicates-cleanup destroy path) instead
// of serving a stale owned/duplicate list for up to ownedTTL.
func (s *Server) invalidateOwned() {
	s.ownedCopiesMu.Lock()
	s.ownedCopies = nil
	s.ownedCopiesMu.Unlock()
}

// performerFilmography returns a performer's full StashDB filmography via the
// LAZY loader: served from the persistent cache when fresh, otherwise fetched
// from StashDB on this visit, cached, and stamped (see
// cache.ScenesForSubjectLazy). No eager pre-fetch.
func (s *Server) performerFilmography(ctx context.Context, sdb *stashdb.Client, stashDBPerformerID string) ([]stashdb.Scene, error) {
	return cache.ScenesForPerformerLazy(ctx, s.db, sdb, stashDBPerformerID)
}

// studioFilmography is the studio analogue of performerFilmography.
func (s *Server) studioFilmography(ctx context.Context, sdb *stashdb.Client, stashDBStudioID string) ([]stashdb.Scene, error) {
	return cache.ScenesForStudioLazy(ctx, s.db, sdb, stashDBStudioID)
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

// pathInt64 reads a chi URL path param as an int64, writing a 400 and
// returning ok=false when it's missing or non-numeric. Centralizes the parse
// every resource handler repeats.
func pathInt64(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	v, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad "+name)
		return 0, false
	}
	return v, true
}

// grabByID resolves the {id} path param to a grab, writing the right error
// (400 bad id / 500 db / 404 not found) and returning ok=false on any miss, so
// handlers collapse the parse + fetch + 404 boilerplate to a single call.
func (s *Server) grabByID(w http.ResponseWriter, r *http.Request) (*grabs.Grab, bool) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return nil, false
	}
	g, err := s.grabs.Get(r.Context(), id)
	if err != nil {
		s.log.Error("grab get", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return nil, false
	}
	if g == nil {
		writeErr(w, http.StatusNotFound, "grab not found")
		return nil, false
	}
	return g, true
}

// writeMappedErr renders err with the right HTTP status: a grabError carries
// its own; a notConfiguredErr is 503; anything else uses defaultStatus. Folds
// the errors.As ladders that were copy-pasted across the grab/job/watch
// handlers into one place.
func writeMappedErr(w http.ResponseWriter, err error, defaultStatus int) {
	var ge grabError
	if errors.As(err, &ge) {
		writeErr(w, ge.status, ge.msg)
		return
	}
	var nc notConfiguredErr
	if errors.As(err, &nc) {
		writeErr(w, http.StatusServiceUnavailable, nc.Error())
		return
	}
	writeErr(w, defaultStatus, err.Error())
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
