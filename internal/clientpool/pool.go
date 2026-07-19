// Package clientpool is the hot-swap layer for forager's outbound
// clients. Each downstream client (stash, stashdb, prowlarr, qbit,
// sab, placer) is held in an atomic.Pointer; consumers call the
// accessor on every use and get the current value with no locking.
//
// This exists because the previous model — passing *X.Client by value
// at construction — meant the poller and cache-refresh goroutines
// held private references that no amount of swapping in api.Server
// would reach. Routing every dependency through the Pool lets a
// single Reload() instantly propagate to every consumer.
//
// Reload is the only mutating call. It rebuilds each client from the
// new Config and stores it atomically. Clients for unconfigured
// sections are stored as nil — accessors return nil to callers,
// which then surface a 503 to the user.
package clientpool

import (
	"log/slog"
	"sync/atomic"

	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/notify"
	"github.com/ordureconnoisseur/forager/internal/placer"
	"github.com/ordureconnoisseur/forager/internal/prowlarr"
	"github.com/ordureconnoisseur/forager/internal/qbit"
	"github.com/ordureconnoisseur/forager/internal/sabnzbd"
	"github.com/ordureconnoisseur/forager/internal/stash"
	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

// Pool owns the live client references. One per daemon process —
// constructed in main.go and threaded everywhere downstream.
type Pool struct {
	stash    atomic.Pointer[stash.Client]
	stashDB  atomic.Pointer[stashdb.Client]
	prowlarr atomic.Pointer[prowlarr.Client]
	qbit     atomic.Pointer[qbit.Client]
	sab      atomic.Pointer[sabnzbd.Client]
	placer   atomic.Pointer[placer.Placer]
	notifier atomic.Pointer[notify.Notifier]

	// placerLog is the logger every constructed placer carries, wired
	// once at boot via SetPlacerLogger and reused by each Reload's
	// reconstruction.
	placerLog atomic.Pointer[slog.Logger]

	// qbitHealth / sabHealth accumulate the background reachability
	// probes for the download clients (see health.go). They live here,
	// next to the client pointers they describe, so Reload can reset
	// them when it swaps the clients: a verdict about the old client is
	// never attributed to the new one. healthKick (buffered, size 1)
	// wakes RunHealthProbes right after a Reload so a config fix is
	// re-verified within seconds instead of one full probe interval.
	qbitHealth probeState
	sabHealth  probeState
	healthKick chan struct{}

	// snapshot of the categories + library root the clients were
	// built from. Cheap to copy under reads; held atomically alongside
	// the clients so callers see a consistent view of "what client and
	// what config it expects."
	settings atomic.Pointer[Settings]
}

// Settings exposes the config knobs the rest of the daemon needs but
// that aren't baked into the clients themselves: download-client
// category names, the placer's library root for diagnostics, etc.
// Snapshot pattern keeps callers from racing against a Reload.
type Settings struct {
	QbitCategory       string
	SabCategory        string
	LibraryRoot        string
	ProwlarrCategories []int
	// StashPathMapping translates forager-side paths to Stash-side
	// paths for scoped metadataScan calls. Format
	// "<forager-prefix>:<stash-prefix>". When empty the poller
	// triggers full-library scans instead of scoped ones.
	StashPathMapping string
	// SabDeleteAfterPlace tells the poller to delete a SAB download
	// (history + files) once it's been placed into the library.
	SabDeleteAfterPlace bool
	// PackDedupKeep: "existing" | "pack" | "review" | "both" — which copy survives
	// when a pack scene duplicates one already in the library.
	PackDedupKeep string
	// AllowedOrigin is the CORS allow value ("" = same-origin only). It
	// lives in the snapshot because the CORS middleware reads it on EVERY
	// request — composing the full config per request was the alternative.
	AllowedOrigin string
	// ExcludedSceneTags: StashDB tag names whose scenes are dropped from
	// the missing-scenes gap analysis (case-insensitive).
	ExcludedSceneTags []string
	// DiscoverFilters: raw named-filter config for Discover
	// ("Name=GENDER1,GENDER2;..."); parsed in the api layer.
	DiscoverFilters string
}

// New returns an empty Pool. Reload it before using.
func New() *Pool {
	p := &Pool{healthKick: make(chan struct{}, 1)}
	p.settings.Store(&Settings{})
	return p
}

// Reload rebuilds every client from cfg and swaps them in atomically.
// Sections without enough configuration to build a client (missing
// URL or required key) get a nil pointer — accessors return nil to
// callers, which surface 503s.
//
// Cheap and safe to call from any goroutine. Existing in-flight
// requests on prior clients keep their reference until they finish;
// the old client is garbage-collected once no goroutine holds it.
func (p *Pool) Reload(cfg config.Config) {
	if cfg.StashURL != "" && cfg.StashAPIKey != "" {
		p.stash.Store(stash.New(cfg.StashURL, cfg.StashAPIKey))
	} else {
		p.stash.Store(nil)
	}
	if cfg.StashDBAPIKey != "" {
		p.stashDB.Store(stashdb.New(cfg.StashDBURL, cfg.StashDBAPIKey))
	} else {
		p.stashDB.Store(nil)
	}
	if cfg.ProwlarrURL != "" && cfg.ProwlarrAPIKey != "" {
		p.prowlarr.Store(prowlarr.New(cfg.ProwlarrURL, cfg.ProwlarrAPIKey))
	} else {
		p.prowlarr.Store(nil)
	}
	if cfg.QbitURL != "" {
		p.qbit.Store(qbit.New(cfg.QbitURL, cfg.QbitUsername, cfg.QbitPassword))
	} else {
		p.qbit.Store(nil)
	}
	if cfg.SabURL != "" && cfg.SabAPIKey != "" {
		p.sab.Store(sabnzbd.New(cfg.SabURL, cfg.SabAPIKey))
	} else {
		p.sab.Store(nil)
	}
	// Placer is unusual — it has no remote endpoint, so its "client" is
	// always constructed; the LibraryRoot field decides whether
	// placer.Configured() returns true. The logger wired at boot via
	// SetPlacerLogger survives every reconstruction; this used to pass a
	// hard nil, silently discarding main.go's logger on the first Reload
	// despite SetPlacer's documented contract.
	p.placer.Store(placer.New(cfg.LibraryRoot, p.placerLog.Load()))

	// notify.New returns nil when neither sink is configured — the same
	// nil-means-off contract as the other clients.
	p.notifier.Store(notify.New(cfg.TelegramBotToken, cfg.TelegramChatID, cfg.NotifyWebhookURL))

	p.settings.Store(&Settings{
		QbitCategory:        cfg.QbitCategory,
		SabCategory:         cfg.SabCategory,
		LibraryRoot:         cfg.LibraryRoot,
		ProwlarrCategories:  append([]int(nil), cfg.ProwlarrCategories...),
		StashPathMapping:    cfg.StashPathMapping,
		SabDeleteAfterPlace: cfg.SabDeleteAfterPlace,
		PackDedupKeep:       cfg.PackDedupKeep,
		AllowedOrigin:       cfg.AllowedOrigin,
		ExcludedSceneTags:   append([]string(nil), cfg.ExcludedSceneTags...),
		DiscoverFilters:     cfg.DiscoverFilters,
	})

	// The swapped-in clients invalidate the reachability verdicts, which
	// were measured against the old ones. Reset to optimistic-unprobed
	// and wake the prober so the new clients are verified within seconds
	// (non-blocking send: a pending kick already covers this Reload; the
	// nil-channel case only arises for a zero-value Pool in tests, where
	// the default branch makes it a no-op).
	p.qbitHealth.reset()
	p.sabHealth.reset()
	select {
	case p.healthKick <- struct{}{}:
	default:
	}
}

// SetPlacerLogger wires the logger every constructed placer carries —
// both the one stored immediately and every reconstruction a later
// Reload performs. Call once at boot, before the first Reload.
func (p *Pool) SetPlacerLogger(root string, log *slog.Logger) {
	p.placerLog.Store(log)
	p.placer.Store(placer.New(root, log))
}

// Stash returns the current Stash client, or nil if unconfigured.
func (p *Pool) Stash() *stash.Client { return p.stash.Load() }

// StashDB returns the current StashDB client, or nil if unconfigured.
func (p *Pool) StashDB() *stashdb.Client { return p.stashDB.Load() }

// Prowlarr returns the current Prowlarr client, or nil if unconfigured.
func (p *Pool) Prowlarr() *prowlarr.Client { return p.prowlarr.Load() }

// Qbit returns the current qBit client, or nil if unconfigured.
func (p *Pool) Qbit() *qbit.Client { return p.qbit.Load() }

// Sab returns the current SAB client, or nil if unconfigured.
func (p *Pool) Sab() *sabnzbd.Client { return p.sab.Load() }

// Placer returns the current Placer. Always non-nil; check Configured().
func (p *Pool) Placer() *placer.Placer { return p.placer.Load() }

// Notifier returns the current external-notification sink, or nil when
// notifications are unconfigured (notify.Notifier methods are nil-safe).
func (p *Pool) Notifier() *notify.Notifier { return p.notifier.Load() }

// Settings returns a snapshot of the non-client config knobs. Always
// non-nil after the first Reload().
func (p *Pool) Settings() Settings {
	s := p.settings.Load()
	if s == nil {
		return Settings{}
	}
	return *s
}
