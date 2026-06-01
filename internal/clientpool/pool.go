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
	"sync/atomic"

	"github.com/ordureconnoisseur/forager/internal/config"
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
	// ExcludedSceneTags: StashDB tag names whose scenes are dropped from
	// the missing-scenes gap analysis (case-insensitive).
	ExcludedSceneTags []string
}

// New returns an empty Pool. Reload it before using.
func New() *Pool {
	p := &Pool{}
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
	// placer.Configured() returns true. Pass a nil logger here; main.go
	// can wire a real one via SetPlacer if it wants logs.
	p.placer.Store(placer.New(cfg.LibraryRoot, nil))

	p.settings.Store(&Settings{
		QbitCategory:        cfg.QbitCategory,
		SabCategory:         cfg.SabCategory,
		LibraryRoot:         cfg.LibraryRoot,
		ProwlarrCategories:  append([]int(nil), cfg.ProwlarrCategories...),
		StashPathMapping:    cfg.StashPathMapping,
		SabDeleteAfterPlace: cfg.SabDeleteAfterPlace,
		PackDedupKeep:       cfg.PackDedupKeep,
		ExcludedSceneTags:   append([]string(nil), cfg.ExcludedSceneTags...),
	})
}

// SetPlacer overrides the placer the Pool constructs by default,
// allowing the caller to pass a logger-equipped instance. Reload()
// then keeps this same instance updated by reconstructing — call
// SetPlacer once at boot if you want named logging from the placer.
func (p *Pool) SetPlacer(pl *placer.Placer) {
	p.placer.Store(pl)
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

// Settings returns a snapshot of the non-client config knobs. Always
// non-nil after the first Reload().
func (p *Pool) Settings() Settings {
	s := p.settings.Load()
	if s == nil {
		return Settings{}
	}
	return *s
}
