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
	// DownloadRoot is where the download clients put finished files. forage
	// uses it to CREATE the forage category in qBit/SAB pointing at that
	// folder, so the user never has to configure the client by hand. One
	// field covers both clients: the hardlink requirement already pushes
	// every setup toward a single completed-downloads folder.
	DownloadRoot string
	LibraryRoot  string
	// TrashTTL is how long deleted files stay recoverable in the trash
	// before the sweep unlinks them for real. 0 disables the trash and
	// restores permanent deletion.
	TrashTTL time.Duration
	// Seeding cull: a placed torrent is removed from qBit (with its files —
	// the library keeps its own hardlink) once EITHER threshold is met,
	// whichever comes first. SeedMaxAge 0 disables the age rule, SeedRatio 0
	// the ratio rule; both 0 disables culling entirely. Defaults satisfy the
	// common private-tracker hit-and-run rules (≥72h or 1.0).
	SeedMaxAge time.Duration
	SeedRatio  float64
	// DeadAfter retires a download that has made no progress for this long.
	// The seeding cull only ever looks at FINISHED torrents, so without this
	// a download that stops is invisible to forage forever. Measured from the
	// last byte received, not from when it was added, so a slow torrent is
	// never mistaken for a dead one. 0 disables the check.
	DeadAfter time.Duration
	// DeadDownloads is what to do with them: "remove" deletes the torrent and
	// its partial data, anything else (the default) reports and touches
	// nothing. Report-first because deleting a partial is unrecoverable and
	// this pass runs unattended.
	DeadDownloads string
	// StagingDisk says the download folder is on a DIFFERENT disk from the
	// library on purpose: the common "download to a fast local SSD, then
	// move to the NAS" layout. forage detects the split either way (the
	// hardlink probe cannot be fooled); this only records that it is
	// intended, so setup reports it as a supported mode rather than nagging
	// about a misconfiguration.
	//
	// It changes nothing about placement. What it changes is the advice:
	// hardlinks are impossible across mounts, so every placement copies, the
	// download copy is a REAL second copy rather than a free extra name, and
	// the seeding cull is the only thing that ever reclaims the staging disk.
	StagingDisk bool
	// SeedOverrides is a JSON array of per-indexer cull thresholds, e.g.
	// [{"indexer":"PornoLab","maxAge":"720h","ratio":2.0}]. A field omitted
	// in an override inherits the global; an explicit zero disables that
	// rule for the indexer. Matching is case-insensitive on the grab's
	// release_indexer; adopted grabs (no indexer) use the globals.
	SeedOverrides string
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
	// StashIgnoreScreenshots has forage keep Stash's IMAGE excludes pointed
	// at the screenshot folders packs ship beside their videos (Screens,
	// Screenlists, Covers, Proof, scr). Off by default: those are the user's
	// own files and forage should not decide unasked what their library
	// indexes. On, because writing the rule by hand fails silently — a
	// pattern with the wrong separator matches nothing and Stash never says
	// so, which is how 29,045 of them reached one library.
	StashIgnoreScreenshots bool
	// PackDedupKeep controls what pack download-then-dedup does when a
	// pack scene duplicates one already in the library:
	//   "existing" — keep the existing copy, remove the pack's (default)
	//   "pack"     — keep the pack's copy, remove the existing (automatic;
	//                gated on verified scan coverage since it deletes originals)
	//   "review"   — destroy nothing automatically; record each collision for
	//                the user to resolve per scene in the Grabs UI
	//   "both"     — keep both (dedup disabled)
	PackDedupKeep string
	// ReleaseRules is the user's release-scoring preference list as a JSON
	// array of scoring.Rule ({label,pattern,points,reject}). Empty string
	// = use the built-in defaults. Stored as JSON so the rule-editor UI
	// round-trips it without a bespoke encoding.
	ReleaseRules string
	// ReleasePrefs is the friendly (no-typing) release-ranking preferences,
	// an opaque JSON string the client owns and compiles into ReleaseRules.
	// The daemon stores and round-trips it but never interprets it — scoring
	// always runs off ReleaseRules. Empty = no friendly prefs saved yet.
	ReleasePrefs string
	// ReleaseAdvanced marks ReleaseRules as hand-tuned in the advanced
	// editor, so the client stops auto-recompiling them from ReleasePrefs.
	ReleaseAdvanced bool
	// DiscoverFilters configures named content filters for the Discover
	// page, format "Name=GENDER1,GENDER2;Name2=...", where genders are
	// Stash performer gender enums. Deployment-specific configuration:
	// the mechanism ships dormant (no filters, no UI chips) unless set.
	DiscoverFilters string
	// HideMalePerformers drops MALE performers from the performer list and
	// from the performer pills on scene cards. Off by default: it is a
	// personal-taste filter, not a correctness one, and it only ever hides
	// a performer whose gender is KNOWN — an unrecorded gender is not an
	// answer, so those stay visible.
	HideMalePerformers bool
	// ExcludedSceneTags is a list of StashDB tag names whose scenes are
	// dropped from the missing-scenes gap analysis (and its counts) —
	// e.g. "Compilation", "Music Video". Matched case-insensitively.
	// Empty = no filtering.
	ExcludedSceneTags []string
	PollInterval      time.Duration
	OrphanAfter       time.Duration
	// PUID/PGID/Umask are the ownership contract for files forage creates,
	// so a container running as root does not leave a library the human (or
	// Stash) cannot write. Env-only and read once at startup: changing the
	// uid a running daemon writes as is not a thing to do live, and every
	// other *arr-style deployment configures these the same way. -1 means
	// "inherit the process defaults", which is what a bare-binary install
	// wants.
	PUID         int
	PGID         int
	Umask        string
	CacheRefresh time.Duration
	LogLevel     slog.Level
	// AllowedOrigin is the CORS allowlist: empty (the default) emits no
	// CORS headers at all, so browsers hold the API to same-origin — the
	// right shape for the daemon-served UI. Set an origin (the old
	// coupled-plugin mode, where the Stash origin called the API
	// directly) or "*" to open it up. "*" used to be the default, but
	// combined with open mode (no admin token) it let any website a LAN
	// browser visited drive destructive POSTs cross-origin.
	AllowedOrigin string
	// AdminToken gates every API route except / and /healthz when set
	// (UI-managed via config.json, or FORAGER_ADMIN_TOKEN). Empty = open.
	// Now demoted to "the API key" — the programmatic-client credential —
	// since the human web login is Username + Password.
	AdminToken string
	// Username is the web-UI login name (the *arr Forms-auth model).
	// Paired with PasswordHash.
	Username string
	// PasswordHash is the bcrypt hash of the web-UI login password (never
	// the plaintext). Empty = no password login. UI-managed via
	// config.json, or FORAGER_PASSWORD_HASH for headless setups.
	PasswordHash string
	// TelegramBotToken + TelegramChatID configure the Telegram
	// notification sink (both required for it to activate): the notify
	// loop pushes actionable events — a watch that found a release, grabs
	// that failed — via the bot's sendMessage. Empty = Telegram off.
	TelegramBotToken string
	TelegramChatID   string
	// NotifyWebhookURL is the generic notification sink: each event batch
	// is POSTed there as {"event","message","ts"} JSON (ntfy, Home
	// Assistant, a Discord shim, ...). Empty = webhook off.
	NotifyWebhookURL string
	// StashPublicURL is the Stash base URL notifications LINK to — the
	// address the user's devices can reach (a public/tunneled hostname),
	// as opposed to StashURL, which is what the daemon itself calls.
	// Empty = links fall back to StashURL.
	StashPublicURL string
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
	b.ProwlarrCategories = parseCSVInts(b.envOr("FORAGER_PROWLARR_CATEGORIES", "6000,6010,6030,6040,6041,6045,6047,6050,6080", "prowlarrCategories"))
	b.QbitURL = strings.TrimRight(b.envOr("FORAGER_QBIT_URL", "", "qbitUrl"), "/")
	b.QbitUsername = b.envOr("FORAGER_QBIT_USERNAME", "", "qbitUsername")
	b.QbitPassword = b.envOr("FORAGER_QBIT_PASSWORD", "", "qbitPassword")
	b.QbitCategory = b.envOr("FORAGER_QBIT_CATEGORY", "forage", "qbitCategory")
	b.SabURL = strings.TrimRight(b.envOr("FORAGER_SAB_URL", "", "sabUrl"), "/")
	b.SabAPIKey = b.envOr("FORAGER_SAB_API_KEY", "", "sabApiKey")
	b.SabCategory = b.envOr("FORAGER_SAB_CATEGORY", "forage", "sabCategory")
	b.DownloadRoot = strings.TrimRight(b.envOr("FORAGER_DOWNLOAD_ROOT", "", "downloadRoot"), "/")
	b.LibraryRoot = strings.TrimRight(b.envOr("FORAGER_LIBRARY_ROOT", "", "libraryRoot"), "/")
	b.TrashTTL = b.envDuration("FORAGER_TRASH_TTL", 7*24*time.Hour, "trashTtl")
	b.SeedMaxAge = b.envDuration("FORAGER_SEED_MAX_AGE", 7*24*time.Hour, "seedMaxAge")
	b.SeedRatio = b.envFloat("FORAGER_SEED_RATIO", 1.0, "seedRatio")
	b.DeadAfter = b.envDuration("FORAGER_DEAD_AFTER", 30*24*time.Hour, "deadAfter")
	b.DeadDownloads = b.envOr("FORAGER_DEAD_DOWNLOADS", "report", "deadDownloads")
	b.StagingDisk = b.envBool("FORAGER_STAGING_DISK", false, "stagingDisk")
	b.SeedOverrides = b.envOr("FORAGER_SEED_OVERRIDES", "", "seedOverrides")
	b.StashPathMapping = b.envOr("FORAGER_STASH_PATH_MAPPING", "", "stashPathMapping")
	b.SabDeleteAfterPlace = b.envBool("FORAGER_SAB_DELETE_AFTER_PLACE", true, "sabDeleteAfterPlace")
	b.StashIgnoreScreenshots = b.envBool("FORAGER_STASH_IGNORE_SCREENSHOTS", false, "stashIgnoreScreenshots")
	b.PackDedupKeep = normalizePackKeep(b.envOr("FORAGER_PACK_DEDUP_KEEP", "existing", "packDedupKeep"))
	b.ReleaseRules = b.envOr("FORAGER_RELEASE_RULES", "", "releaseRules")
	b.ReleasePrefs = b.envOr("FORAGER_RELEASE_PREFS", "", "releasePrefs")
	b.ReleaseAdvanced = b.envBool("FORAGER_RELEASE_ADVANCED", false, "releaseAdvanced")
	b.ExcludedSceneTags = parseCSVStrings(b.envOr("FORAGER_EXCLUDED_SCENE_TAGS", "", "excludedSceneTags"))
	b.DiscoverFilters = b.envOr("FORAGER_DISCOVER_FILTERS", "", "discoverFilters")
	b.HideMalePerformers = b.envBool("FORAGER_HIDE_MALE_PERFORMERS", false, "hideMalePerformers")
	b.PUID = b.envInt("FORAGER_PUID", -1, "puid")
	b.PGID = b.envInt("FORAGER_PGID", -1, "pgid")
	b.Umask = b.envOr("FORAGER_UMASK", "", "umask")
	b.PollInterval = b.envDuration("FORAGER_POLL_INTERVAL", 60*time.Second, "pollInterval")
	b.OrphanAfter = b.envDuration("FORAGER_ORPHAN_AFTER", 6*time.Hour, "orphanAfter")
	b.CacheRefresh = b.envDuration("FORAGER_CACHE_REFRESH", 6*time.Hour, "cacheRefresh")
	b.LogLevel = envLogLevel("FORAGER_LOG_LEVEL", slog.LevelInfo)
	b.AllowedOrigin = b.envOr("FORAGER_ALLOWED_ORIGIN", "", "allowedOrigin")
	b.AdminToken = os.Getenv("FORAGER_ADMIN_TOKEN")
	b.Username = b.envOr("FORAGER_USERNAME", "", "username")
	b.PasswordHash = b.envOr("FORAGER_PASSWORD_HASH", "", "passwordHash")
	b.TelegramBotToken = b.envOr("FORAGER_TELEGRAM_BOT_TOKEN", "", "telegramBotToken")
	b.TelegramChatID = b.envOr("FORAGER_TELEGRAM_CHAT_ID", "", "telegramChatId")
	b.NotifyWebhookURL = b.envOr("FORAGER_NOTIFY_WEBHOOK_URL", "", "notifyWebhookUrl")
	b.StashPublicURL = strings.TrimRight(b.envOr("FORAGER_STASH_PUBLIC_URL", "", "stashPublicUrl"), "/")
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

	flt := func(field string, stored *string, envVal, defaultVal float64) float64 {
		if stored != nil && *stored != "" {
			if f, err := strconv.ParseFloat(*stored, 64); err == nil {
				src[field] = SourceJSON
				return f
			}
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
	out.QbitCategory = str("qbitCategory", stored.QbitCategory, b.QbitCategory, "forage")
	out.SabURL = str("sabUrl", stored.SabURL, b.SabURL, "")
	out.SabAPIKey = str("sabApiKey", stored.SabAPIKey, b.SabAPIKey, "")
	out.SabCategory = str("sabCategory", stored.SabCategory, b.SabCategory, "forage")
	out.DownloadRoot = str("downloadRoot", stored.DownloadRoot, b.DownloadRoot, "")
	out.LibraryRoot = str("libraryRoot", stored.LibraryRoot, b.LibraryRoot, "")
	out.TrashTTL = dur("trashTtl", stored.TrashTTL, b.TrashTTL, 7*24*time.Hour)
	out.SeedMaxAge = dur("seedMaxAge", stored.SeedMaxAge, b.SeedMaxAge, 7*24*time.Hour)
	out.SeedRatio = flt("seedRatio", stored.SeedRatio, b.SeedRatio, 1.0)
	out.DeadAfter = dur("deadAfter", stored.DeadAfter, b.DeadAfter, 30*24*time.Hour)
	out.DeadDownloads = str("deadDownloads", stored.DeadDownloads, b.DeadDownloads, "report")
	out.StagingDisk = boolean("stagingDisk", stored.StagingDisk, b.StagingDisk, false)
	out.SeedOverrides = str("seedOverrides", stored.SeedOverrides, b.SeedOverrides, "")
	out.StashPathMapping = str("stashPathMapping", stored.StashPathMapping, b.StashPathMapping, "")
	out.SabDeleteAfterPlace = boolean("sabDeleteAfterPlace", stored.SabDeleteAfterPlace, b.SabDeleteAfterPlace, true)
	out.StashIgnoreScreenshots = boolean("stashIgnoreScreenshots", stored.StashIgnoreScreenshots, b.StashIgnoreScreenshots, false)
	out.PackDedupKeep = normalizePackKeep(str("packDedupKeep", stored.PackDedupKeep, b.PackDedupKeep, "existing"))
	out.ReleaseRules = str("releaseRules", stored.ReleaseRules, b.ReleaseRules, "")
	out.ReleasePrefs = str("releasePrefs", stored.ReleasePrefs, b.ReleasePrefs, "")
	out.ReleaseAdvanced = boolean("releaseAdvanced", stored.ReleaseAdvanced, b.ReleaseAdvanced, false)
	out.DiscoverFilters = str("discoverFilters", stored.DiscoverFilters, b.DiscoverFilters, "")
	out.HideMalePerformers = boolean("hideMalePerformers", stored.HideMalePerformers, b.HideMalePerformers, false)
	if stored.ExcludedSceneTags != nil {
		out.ExcludedSceneTags = append([]string(nil), (*stored.ExcludedSceneTags)...)
		src["excludedSceneTags"] = SourceJSON
	} else if b.set["excludedSceneTags"] {
		src["excludedSceneTags"] = SourceEnv
	} else {
		src["excludedSceneTags"] = SourceDefault
	}
	out.PollInterval = dur("pollInterval", stored.PollInterval, b.PollInterval, 60*time.Second)
	out.OrphanAfter = dur("orphanAfter", stored.OrphanAfter, b.OrphanAfter, 6*time.Hour)
	out.CacheRefresh = dur("cacheRefresh", stored.CacheRefresh, b.CacheRefresh, 6*time.Hour)
	out.AllowedOrigin = str("allowedOrigin", stored.AllowedOrigin, b.AllowedOrigin, "")
	out.AdminToken = str("adminToken", stored.AdminToken, b.AdminToken, "")
	out.Username = str("username", stored.Username, b.Username, "")
	out.PasswordHash = str("passwordHash", stored.PasswordHash, b.PasswordHash, "")
	out.TelegramBotToken = str("telegramBotToken", stored.TelegramBotToken, b.TelegramBotToken, "")
	out.TelegramChatID = str("telegramChatId", stored.TelegramChatID, b.TelegramChatID, "")
	out.NotifyWebhookURL = str("notifyWebhookUrl", stored.NotifyWebhookURL, b.NotifyWebhookURL, "")
	out.StashPublicURL = strings.TrimRight(str("stashPublicUrl", stored.StashPublicURL, b.StashPublicURL, ""), "/")
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

// envInt reads an integer setting. A malformed value falls back to the
// default rather than failing startup: a typo in PUID should not stop the
// daemon booting, it should just leave ownership alone.
func (b *BootstrapConfig) envInt(key string, def int, field string) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	b.set[field] = true
	return n
}

func (b *BootstrapConfig) envFloat(key string, def float64, field string) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	b.set[field] = true
	return f
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
	case "review":
		return "review"
	case "log-only":
		// Dry run: plan the dedup and log exactly what WOULD be destroyed
		// (full file paths), destroy nothing. The recommended first-week
		// setting for a new install — the journal of would-be deletions is
		// how you learn to trust the automatic mode before arming it.
		return "log-only"
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

// parseCSVStrings converts "Compilation, Music Video" to a trimmed,
// empty-dropped []string. Used for the excluded-scene-tags env var.
func parseCSVStrings(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
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
