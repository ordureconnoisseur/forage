// Package config layers environment variables (the bootstrap path)
// with UI-saved JSON (configstore) into a single Config struct that
// the rest of the daemon reads.
//
// Precedence on each field: configstore JSON → env var → hardcoded
// default. Compose() produces the final Config plus a per-field
// Sources map the /config endpoint surfaces to the UI ("env is
// winning over JSON for this field" indicator).
//
// 2026-05-26 refactor split this from a single env-only loader. The
// required-field check that used to exit at boot is now relaxed —
// the daemon stays up without Stash credentials and dependent
// endpoints return 503 until the user saves through the UI.
package config

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ordureconnoisseur/forager/internal/configstore"
)

// Config is the final composed view callers read. Identical shape to
// what the daemon used to load from env directly — refactor keeps
// downstream code unchanged.
type Config struct {
	ListenAddr         string
	DBPath             string
	StashURL           string
	StashAPIKey        string
	StashDBURL         string
	StashDBAPIKey      string
	ProwlarrURL        string
	ProwlarrAPIKey     string
	ProwlarrCategories []int
	QbitURL            string
	QbitUsername       string
	QbitPassword       string
	QbitCategory       string
	SabURL             string
	SabAPIKey          string
	SabCategory        string
	LibraryRoot        string
	// StashPathMapping translates forager-container paths to Stash's
	// view of the same files, for scoped metadataScan calls after
	// placement. Format: "<forager-prefix>:<stash-prefix>" (e.g.
	// "/data/media/Media:Z:\\Media"). When unset, forager triggers a
	// full-library scan after each placement — slower but always
	// works regardless of mount layout differences.
	StashPathMapping string
	// SabDeleteAfterPlace removes a SAB download (history entry +
	// downloaded files) once forage has placed it into the library.
	// Usenet doesn't seed, so the SAB copy is redundant after
	// placement — this is the *arr-style "remove completed download"
	// behaviour. Safe: placement hardlinks/copies into the library
	// first, so deleting the SAB source leaves the library file
	// intact. qBit grabs are never touched (torrents keep seeding).
	SabDeleteAfterPlace bool
	// PackDedupKeep controls what pack download-then-dedup does when a
	// pack scene duplicates one already in the library:
	//   "existing" — keep the existing copy, remove the pack's (default)
	//   "pack"     — keep the pack's copy, remove the existing
	//   "both"     — keep both (dedup disabled)
	PackDedupKeep string
	// ReleaseRules is the user's release-scoring preference list as a JSON
	// array of scoring.Rule ({label,pattern,points,reject}). Empty string
	// = use the built-in defaults. Stored as JSON so the rule-editor UI
	// round-trips it without a bespoke encoding.
	ReleaseRules  string
	PollInterval  time.Duration
	OrphanAfter   time.Duration
	CacheRefresh  time.Duration
	LogLevel      slog.Level
	AllowedOrigin string
	// AdminToken gates /config* endpoints when set. Boot-only; not
	// editable via the UI (that would let the UI lock itself out).
	AdminToken string
}

// BootstrapConfig is the env-loaded layer. Same fields as Config —
// values come straight from os.Getenv with defaults. `set` tracks
// which env vars were actually provided (vs. fell back to default),
// so Compose can attribute fields to "env" vs "default".
type BootstrapConfig struct {
	Config
	set map[string]bool
}

// FieldSource identifies where a composed field's value came from.
// Surfaced through /config so the UI can show "this value is coming
// from .env, not config.json" hints.
type FieldSource string

const (
	SourceJSON    FieldSource = "json"
	SourceEnv     FieldSource = "env"
	SourceDefault FieldSource = "default"
)

// Sources records the resolution source per Config field. Keys are
// JSON-style camelCase to match what the UI deals with.
type Sources map[string]FieldSource

// LoadBootstrap reads environment variables and defaults into a
// BootstrapConfig. Never returns an error — missing required fields
// no longer fail boot; the daemon advertises itself as unconfigured
// instead. Tracks which vars were set so Compose can distinguish env
// values from defaults.
func LoadBootstrap() BootstrapConfig {
	b := BootstrapConfig{set: map[string]bool{}}
	b.ListenAddr = b.envOr("FORAGER_LISTEN_ADDR", "127.0.0.1:7979", "listenAddr")
	b.DBPath = b.envOr("FORAGER_DB_PATH", "forager.db", "dbPath")
	b.StashURL = strings.TrimRight(b.envOr("FORAGER_STASH_URL", "", "stashUrl"), "/")
	b.StashAPIKey = b.envOr("FORAGER_STASH_API_KEY", "", "stashApiKey")
	b.StashDBURL = strings.TrimRight(b.envOr("FORAGER_STASHDB_URL", "https://stashdb.org", "stashdbUrl"), "/")
	b.StashDBAPIKey = b.envOr("FORAGER_STASHDB_API_KEY", "", "stashdbApiKey")
	b.ProwlarrURL = strings.TrimRight(b.envOr("FORAGER_PROWLARR_URL", "", "prowlarrUrl"), "/")
	b.ProwlarrAPIKey = b.envOr("FORAGER_PROWLARR_API_KEY", "", "prowlarrApiKey")
	b.ProwlarrCategories = parseCSVInts(b.envOr("FORAGER_PROWLARR_CATEGORIES", "6000,6010,6020,6030,6040", "prowlarrCategories"))
	b.QbitURL = strings.TrimRight(b.envOr("FORAGER_QBIT_URL", "", "qbitUrl"), "/")
	b.QbitUsername = b.envOr("FORAGER_QBIT_USERNAME", "", "qbitUsername")
	b.QbitPassword = b.envOr("FORAGER_QBIT_PASSWORD", "", "qbitPassword")
	b.QbitCategory = b.envOr("FORAGER_QBIT_CATEGORY", "manual", "qbitCategory")
	b.SabURL = strings.TrimRight(b.envOr("FORAGER_SAB_URL", "", "sabUrl"), "/")
	b.SabAPIKey = b.envOr("FORAGER_SAB_API_KEY", "", "sabApiKey")
	b.SabCategory = b.envOr("FORAGER_SAB_CATEGORY", "manual", "sabCategory")
	b.LibraryRoot = strings.TrimRight(b.envOr("FORAGER_LIBRARY_ROOT", "", "libraryRoot"), "/")
	b.StashPathMapping = b.envOr("FORAGER_STASH_PATH_MAPPING", "", "stashPathMapping")
	b.SabDeleteAfterPlace = b.envBool("FORAGER_SAB_DELETE_AFTER_PLACE", true, "sabDeleteAfterPlace")
	b.PackDedupKeep = normalizePackKeep(b.envOr("FORAGER_PACK_DEDUP_KEEP", "existing", "packDedupKeep"))
	b.ReleaseRules = b.envOr("FORAGER_RELEASE_RULES", "", "releaseRules")
	b.PollInterval = b.envDuration("FORAGER_POLL_INTERVAL", 60*time.Second, "pollInterval")
	b.OrphanAfter = b.envDuration("FORAGER_ORPHAN_AFTER", 6*time.Hour, "orphanAfter")
	b.CacheRefresh = b.envDuration("FORAGER_CACHE_REFRESH", 6*time.Hour, "cacheRefresh")
	b.LogLevel = envLogLevel("FORAGER_LOG_LEVEL", slog.LevelInfo)
	b.AllowedOrigin = b.envOr("FORAGER_ALLOWED_ORIGIN", "*", "allowedOrigin")
	b.AdminToken = os.Getenv("FORAGER_ADMIN_TOKEN")
	return b
}

// Compose overlays a configstore StoredConfig on the BootstrapConfig
// and returns the final Config plus per-field Sources. Stored fields
// (non-nil pointers) win; otherwise fall through to env / default.
func Compose(b BootstrapConfig, stored configstore.StoredConfig) (Config, Sources) {
	out := b.Config
	src := Sources{}

	// Helper: resolve a string field. Stored pointer wins when non-nil
	// (empty-string is a valid "clear" → falls back to env). Env wins
	// when the env var was actually set; otherwise default.
	str := func(field string, stored *string, envVal, defaultVal string) string {
		if stored != nil && *stored != "" {
			src[field] = SourceJSON
			return *stored
		}
		if b.set[field] {
			src[field] = SourceEnv
			return envVal
		}
		src[field] = SourceDefault
		return defaultVal
	}

	// boolean: stored pointer wins when non-nil (an explicit false is
	// a real choice, unlike the empty-string sentinel strings use).
	boolean := func(field string, stored *bool, envVal, defaultVal bool) bool {
		if stored != nil {
			src[field] = SourceJSON
			return *stored
		}
		if b.set[field] {
			src[field] = SourceEnv
			return envVal
		}
		src[field] = SourceDefault
		return defaultVal
	}

	dur := func(field string, stored *string, envVal, defaultVal time.Duration) time.Duration {
		if stored != nil && *stored != "" {
			if d, err := time.ParseDuration(*stored); err == nil {
				src[field] = SourceJSON
				return d
			}
			// Bad duration in JSON — log via source as default and
			// fall through (caller can fix it via the UI).
		}
		if b.set[field] {
			src[field] = SourceEnv
			return envVal
		}
		src[field] = SourceDefault
		return defaultVal
	}

	out.StashURL = str("stashUrl", stored.StashURL, b.StashURL, "")
	out.StashAPIKey = str("stashApiKey", stored.StashAPIKey, b.StashAPIKey, "")
	out.StashDBURL = str("stashdbUrl", stored.StashDBURL, b.StashDBURL, "https://stashdb.org")
	out.StashDBAPIKey = str("stashdbApiKey", stored.StashDBAPIKey, b.StashDBAPIKey, "")
	out.ProwlarrURL = str("prowlarrUrl", stored.ProwlarrURL, b.ProwlarrURL, "")
	out.ProwlarrAPIKey = str("prowlarrApiKey", stored.ProwlarrAPIKey, b.ProwlarrAPIKey, "")
	if stored.ProwlarrCategories != nil {
		out.ProwlarrCategories = append([]int(nil), (*stored.ProwlarrCategories)...)
		src["prowlarrCategories"] = SourceJSON
	} else if b.set["prowlarrCategories"] {
		src["prowlarrCategories"] = SourceEnv
	} else {
		src["prowlarrCategories"] = SourceDefault
	}
	out.QbitURL = str("qbitUrl", stored.QbitURL, b.QbitURL, "")
	out.QbitUsername = str("qbitUsername", stored.QbitUsername, b.QbitUsername, "")
	out.QbitPassword = str("qbitPassword", stored.QbitPassword, b.QbitPassword, "")
	out.QbitCategory = str("qbitCategory", stored.QbitCategory, b.QbitCategory, "manual")
	out.SabURL = str("sabUrl", stored.SabURL, b.SabURL, "")
	out.SabAPIKey = str("sabApiKey", stored.SabAPIKey, b.SabAPIKey, "")
	out.SabCategory = str("sabCategory", stored.SabCategory, b.SabCategory, "manual")
	out.LibraryRoot = str("libraryRoot", stored.LibraryRoot, b.LibraryRoot, "")
	out.StashPathMapping = str("stashPathMapping", stored.StashPathMapping, b.StashPathMapping, "")
	out.SabDeleteAfterPlace = boolean("sabDeleteAfterPlace", stored.SabDeleteAfterPlace, b.SabDeleteAfterPlace, true)
	out.PackDedupKeep = normalizePackKeep(str("packDedupKeep", stored.PackDedupKeep, b.PackDedupKeep, "existing"))
	out.ReleaseRules = str("releaseRules", stored.ReleaseRules, b.ReleaseRules, "")
	out.PollInterval = dur("pollInterval", stored.PollInterval, b.PollInterval, 60*time.Second)
	out.OrphanAfter = dur("orphanAfter", stored.OrphanAfter, b.OrphanAfter, 6*time.Hour)
	out.CacheRefresh = dur("cacheRefresh", stored.CacheRefresh, b.CacheRefresh, 6*time.Hour)
	out.AllowedOrigin = str("allowedOrigin", stored.AllowedOrigin, b.AllowedOrigin, "*")
	return out, src
}

// IsConfigured returns true when the bare minimum to serve real data
// is set (Stash + StashDB credentials). When false, /healthz reports
// unconfigured: true and dependent endpoints return 503.
func IsConfigured(c Config) bool {
	return c.StashURL != "" && c.StashAPIKey != "" && c.StashDBAPIKey != ""
}

// Load is a convenience for the standalone probe tools in tools/*
// which need a fully-validated env-only config to do anything useful.
// main.go uses LoadBootstrap() directly so it can boot unconfigured
// and let the user fix things through the UI.
func Load() (Config, error) {
	b := LoadBootstrap()
	cfg, _ := Compose(b, configstore.StoredConfig{})
	if cfg.StashURL == "" {
		return cfg, errMissing("FORAGER_STASH_URL")
	}
	if cfg.StashAPIKey == "" {
		return cfg, errMissing("FORAGER_STASH_API_KEY")
	}
	if cfg.StashDBAPIKey == "" {
		return cfg, errMissing("FORAGER_STASHDB_API_KEY")
	}
	return cfg, nil
}

type missingEnvErr struct{ key string }

func (e missingEnvErr) Error() string { return e.key + " is required" }

func errMissing(key string) error { return missingEnvErr{key: key} }

func (b *BootstrapConfig) envOr(key, def, field string) string {
	if v := os.Getenv(key); v != "" {
		b.set[field] = true
		return v
	}
	return def
}

func (b *BootstrapConfig) envDuration(key string, def time.Duration, field string) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	b.set[field] = true
	return d
}

func (b *BootstrapConfig) envBool(key string, def bool, field string) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		b.set[field] = true
		return true
	case "0", "false", "no", "off":
		b.set[field] = true
		return false
	}
	return def
}

// normalizePackKeep clamps the pack dedup preference to a known value,
// defaulting to "existing" for anything unrecognised (empty, typo).
func normalizePackKeep(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "pack":
		return "pack"
	case "both":
		return "both"
	default:
		return "existing"
	}
}

// parseCSVInts converts "6000,6010,6020" to []int{6000,6010,6020}.
// Empty / unparseable entries are dropped silently — the env var is
// best-effort config, not user input.
func parseCSVInts(s string) []int {
	if s == "" {
		return nil
	}
	var out []int
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	return out
}

func envLogLevel(key string, def slog.Level) slog.Level {
	switch strings.ToLower(os.Getenv(key)) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return def
}
