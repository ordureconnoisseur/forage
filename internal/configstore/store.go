// Package configstore persists UI-saved daemon configuration to
// ./data/config.json with atomic writes and rotating backups.
//
// The on-disk JSON is a partial overlay over environment variables:
// nil/missing fields fall through to env defaults at compose time
// (handled by internal/config). The first UI save creates the file;
// existing .env-only deployments keep working untouched until then.
//
// Concurrency: a single sync.RWMutex guards the in-memory cache. Reads
// are cheap and frequent (every config-dependent request); writes are
// rare (UI save), so the read-heavy split is the right shape.
package configstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

const (
	fileName    = "config.json"
	fileMode    = 0o600
	backupCount = 3
)

// StoredConfig is the on-disk JSON shape. Every field is a pointer so
// the encoding can distinguish "field absent" (fall through to env)
// from "field explicitly set to empty string" (override env with
// empty). The Patch type below is an alias of this — same shape, used
// for the /config POST body.
type StoredConfig struct {
	StashURL           *string `json:"stashUrl,omitempty"`
	StashAPIKey        *string `json:"stashApiKey,omitempty"`
	StashDBURL         *string `json:"stashdbUrl,omitempty"`
	StashDBAPIKey      *string `json:"stashdbApiKey,omitempty"`
	ProwlarrURL        *string `json:"prowlarrUrl,omitempty"`
	ProwlarrAPIKey     *string `json:"prowlarrApiKey,omitempty"`
	ProwlarrCategories *[]int  `json:"prowlarrCategories,omitempty"`
	QbitURL            *string `json:"qbitUrl,omitempty"`
	QbitUsername       *string `json:"qbitUsername,omitempty"`
	QbitPassword       *string `json:"qbitPassword,omitempty"`
	QbitCategory       *string `json:"qbitCategory,omitempty"`
	DownloadRoot       *string `json:"downloadRoot,omitempty"`
	TrashTTL           *string `json:"trashTtl,omitempty"`
	SeedMaxAge         *string `json:"seedMaxAge,omitempty"`
	SeedRatio          *string `json:"seedRatio,omitempty"`
	SeedOverrides      *string `json:"seedOverrides,omitempty"`
	SabURL             *string `json:"sabUrl,omitempty"`
	SabAPIKey          *string `json:"sabApiKey,omitempty"`
	SabCategory        *string `json:"sabCategory,omitempty"`
	LibraryRoot        *string `json:"libraryRoot,omitempty"`
	// StashPathMapping translates the forager-container path of a
	// placed file into the path Stash sees for the same file (the two
	// often differ when forager runs in Docker on Linux and Stash is
	// on Windows over a NAS mount). Used by the post-place
	// metadataScan trigger. Format "<forager-prefix>:<stash-prefix>"
	// — e.g. "/data/media/Media:Z:\\Media".
	StashPathMapping *string `json:"stashPathMapping,omitempty"`
	// SabDeleteAfterPlace: remove the SAB download after placement.
	// *bool so an explicit false round-trips (nil = fall through to
	// env/default).
	SabDeleteAfterPlace *bool `json:"sabDeleteAfterPlace,omitempty"`
	// PackDedupKeep: "existing" | "pack" | "review" | "both". Controls which copy
	// survives when a pack scene duplicates one already in the library.
	PackDedupKeep *string `json:"packDedupKeep,omitempty"`
	// ReleaseRules is the release-scoring preference list as a JSON array
	// string (scoring.Rule[]). Empty = built-in defaults.
	ReleaseRules *string `json:"releaseRules,omitempty"`
	// ReleasePrefs is the friendly (no-typing) release-ranking preferences
	// as an opaque JSON string the CLIENT owns and compiles into
	// ReleaseRules. The daemon never interprets it — it only persists and
	// round-trips it. Empty = no friendly prefs saved yet.
	ReleasePrefs *string `json:"releasePrefs,omitempty"`
	// ReleaseAdvanced flags that ReleaseRules was hand-tuned in the advanced
	// editor, so the client stops auto-recompiling them from ReleasePrefs.
	// *bool so an explicit false (back-to-simple) round-trips.
	ReleaseAdvanced *bool `json:"releaseAdvanced,omitempty"`
	// DiscoverFilters: named Discover content filters
	// ("Name=GENDER1,GENDER2;..."), deployment-specific.
	DiscoverFilters *string `json:"discoverFilters,omitempty"`
	// ExcludedSceneTags drops scenes carrying any of these StashDB tag
	// names from the gap analysis (case-insensitive). nil = no change.
	ExcludedSceneTags *[]string `json:"excludedSceneTags,omitempty"`
	// Duration fields are stored as strings ("60s", "6h") so the JSON
	// stays human-readable and round-trips cleanly through the UI.
	PollInterval  *string `json:"pollInterval,omitempty"`
	OrphanAfter   *string `json:"orphanAfter,omitempty"`
	CacheRefresh  *string `json:"cacheRefresh,omitempty"`
	AllowedOrigin *string `json:"allowedOrigin,omitempty"`
	// AdminToken is the shared secret that gates every API route. Empty
	// (or absent) = no auth. Stored here so it's UI-manageable; an env
	// FORAGER_ADMIN_TOKEN still applies when this is unset. After the
	// username+password login landed it's demoted to "the API key" — the
	// programmatic-client credential, not the human web login.
	AdminToken *string `json:"adminToken,omitempty"`
	// Username is the web-UI login name (the *arr Forms-auth model).
	// Paired with PasswordHash; both empty = no password login.
	Username *string `json:"username,omitempty"`
	// PasswordHash is the bcrypt hash of the web-UI login password —
	// NEVER the plaintext. The /config endpoint accepts a plaintext
	// `password`, hashes it here, and only the hash is ever persisted.
	PasswordHash *string `json:"passwordHash,omitempty"`
	// Telegram notification sink: bot token + chat id (both required to
	// activate). The token is a secret — /config masks it like the API keys.
	TelegramBotToken *string `json:"telegramBotToken,omitempty"`
	TelegramChatID   *string `json:"telegramChatId,omitempty"`
	// NotifyWebhookURL receives {"event","message","ts"} JSON per event
	// batch — the generic notification sink.
	NotifyWebhookURL *string `json:"notifyWebhookUrl,omitempty"`
	// StashPublicURL is the user-reachable Stash base URL that
	// notification links point at (falls back to StashURL when empty).
	StashPublicURL *string `json:"stashPublicUrl,omitempty"`
}

// Patch is the wire shape POSTed to /config. Identical to StoredConfig
// — nil means "no change", non-nil means "set". Empty string clears
// the field, causing it to fall back to env/default at compose time.
type Patch = StoredConfig

// Store persists StoredConfig with atomic writes + rotating backups.
type Store struct {
	dir    string
	mu     sync.RWMutex
	cached *StoredConfig // nil = file doesn't exist yet
}

// Open initializes a Store rooted at the given directory (typically
// the same dir that holds forager.db). Reads any existing config.json
// into memory; a missing file is fine.
func Open(dir string) (*Store, error) {
	s := &Store{dir: dir}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Get returns the currently-stored config. Returns a zero-value
// StoredConfig (all fields nil) when nothing has been saved yet.
// The return value is a shallow copy whose pointer fields are safe to
// read but must not be mutated by the caller.
func (s *Store) Get() StoredConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.cached == nil {
		return StoredConfig{}
	}
	return cloneConfig(*s.cached)
}

// Set applies a patch and persists the result. Non-nil patch fields
// overwrite the stored value; nil patch fields preserve the existing
// value. Write is atomic via tmp file + rename; the previous content
// is preserved as .bak.1 (with .bak.2 / .bak.3 holding older versions).
func (s *Store) Set(patch Patch) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	base := StoredConfig{}
	if s.cached != nil {
		base = cloneConfig(*s.cached)
	}
	applyPatch(&base, patch)

	if err := s.writeAtomic(base); err != nil {
		return err
	}
	s.cached = &base
	return nil
}

// Path returns the absolute path the Store writes to. Useful for
// diagnostics + telling the user where their JSON config lives.
func (s *Store) Path() string {
	return filepath.Join(s.dir, fileName)
}

func (s *Store) load() error {
	path := filepath.Join(s.dir, fileName)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		s.cached = nil
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var cfg StoredConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	s.cached = &cfg
	return nil
}

// writeAtomic backs up the current file, then writes new content via
// tmp+rename. Backup-first ordering means a crash mid-write leaves the
// previous content recoverable from .bak.1, and the in-progress tmp
// file is cleaned up explicitly.
func (s *Store) writeAtomic(cfg StoredConfig) error {
	if err := s.rotateBackups(); err != nil {
		return err
	}
	path := filepath.Join(s.dir, fileName)
	tmp, err := os.CreateTemp(s.dir, fileName+".tmp.*")
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	cleanup := func() { _ = os.Remove(tmp.Name()) }
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cfg); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("encode: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Chmod(tmp.Name(), fileMode); err != nil {
		cleanup()
		return fmt.Errorf("chmod tmp: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		cleanup()
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// rotateBackups shifts existing backups down (.bak.1 → .bak.2 → .bak.3,
// oldest dropped), then copies the current config.json into .bak.1.
// Copy rather than rename so the original stays in place — if the
// subsequent atomic write fails, the daemon is still serving correct
// config from the unchanged config.json.
func (s *Store) rotateBackups() error {
	for i := backupCount; i > 1; i-- {
		from := filepath.Join(s.dir, fmt.Sprintf("%s.bak.%d", fileName, i-1))
		to := filepath.Join(s.dir, fmt.Sprintf("%s.bak.%d", fileName, i))
		if _, err := os.Stat(from); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("rotate backup %d→%d: %w", i-1, i, err)
		}
	}
	cur := filepath.Join(s.dir, fileName)
	src, err := os.Open(cur)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // first save — nothing to back up
	}
	if err != nil {
		return fmt.Errorf("open current for backup: %w", err)
	}
	defer src.Close()
	bak1 := filepath.Join(s.dir, fileName+".bak.1")
	dst, err := os.OpenFile(bak1, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fileMode)
	if err != nil {
		return fmt.Errorf("create backup: %w", err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return fmt.Errorf("copy backup: %w", err)
	}
	return dst.Close()
}

// applyPatch overlays non-nil fields of patch onto base. Pointer-equal
// assignment is safe because *string is treated as immutable everywhere.
func applyPatch(base *StoredConfig, patch Patch) {
	if patch.StashURL != nil {
		base.StashURL = patch.StashURL
	}
	if patch.StashAPIKey != nil {
		base.StashAPIKey = patch.StashAPIKey
	}
	if patch.StashDBURL != nil {
		base.StashDBURL = patch.StashDBURL
	}
	if patch.StashDBAPIKey != nil {
		base.StashDBAPIKey = patch.StashDBAPIKey
	}
	if patch.ProwlarrURL != nil {
		base.ProwlarrURL = patch.ProwlarrURL
	}
	if patch.ProwlarrAPIKey != nil {
		base.ProwlarrAPIKey = patch.ProwlarrAPIKey
	}
	if patch.ProwlarrCategories != nil {
		cats := make([]int, len(*patch.ProwlarrCategories))
		copy(cats, *patch.ProwlarrCategories)
		base.ProwlarrCategories = &cats
	}
	if patch.QbitURL != nil {
		base.QbitURL = patch.QbitURL
	}
	if patch.QbitUsername != nil {
		base.QbitUsername = patch.QbitUsername
	}
	if patch.QbitPassword != nil {
		base.QbitPassword = patch.QbitPassword
	}
	if patch.DownloadRoot != nil {
		base.DownloadRoot = patch.DownloadRoot
	}
	if patch.TrashTTL != nil {
		base.TrashTTL = patch.TrashTTL
	}
	if patch.SeedMaxAge != nil {
		base.SeedMaxAge = patch.SeedMaxAge
	}
	if patch.SeedRatio != nil {
		base.SeedRatio = patch.SeedRatio
	}
	if patch.SeedOverrides != nil {
		base.SeedOverrides = patch.SeedOverrides
	}
	if patch.QbitCategory != nil {
		base.QbitCategory = patch.QbitCategory
	}
	if patch.SabURL != nil {
		base.SabURL = patch.SabURL
	}
	if patch.SabAPIKey != nil {
		base.SabAPIKey = patch.SabAPIKey
	}
	if patch.SabCategory != nil {
		base.SabCategory = patch.SabCategory
	}
	if patch.LibraryRoot != nil {
		base.LibraryRoot = patch.LibraryRoot
	}
	if patch.StashPathMapping != nil {
		base.StashPathMapping = patch.StashPathMapping
	}
	if patch.SabDeleteAfterPlace != nil {
		base.SabDeleteAfterPlace = patch.SabDeleteAfterPlace
	}
	if patch.PackDedupKeep != nil {
		base.PackDedupKeep = patch.PackDedupKeep
	}
	if patch.ReleaseRules != nil {
		base.ReleaseRules = patch.ReleaseRules
	}
	if patch.ReleasePrefs != nil {
		base.ReleasePrefs = patch.ReleasePrefs
	}
	if patch.ReleaseAdvanced != nil {
		base.ReleaseAdvanced = patch.ReleaseAdvanced
	}
	if patch.ExcludedSceneTags != nil {
		tags := append([]string(nil), (*patch.ExcludedSceneTags)...)
		base.ExcludedSceneTags = &tags
	}
	if patch.PollInterval != nil {
		base.PollInterval = patch.PollInterval
	}
	if patch.OrphanAfter != nil {
		base.OrphanAfter = patch.OrphanAfter
	}
	if patch.CacheRefresh != nil {
		base.CacheRefresh = patch.CacheRefresh
	}
	if patch.AllowedOrigin != nil {
		base.AllowedOrigin = patch.AllowedOrigin
	}
	if patch.AdminToken != nil {
		base.AdminToken = patch.AdminToken
	}
	if patch.Username != nil {
		base.Username = patch.Username
	}
	if patch.PasswordHash != nil {
		base.PasswordHash = patch.PasswordHash
	}
	if patch.TelegramBotToken != nil {
		base.TelegramBotToken = patch.TelegramBotToken
	}
	if patch.TelegramChatID != nil {
		base.TelegramChatID = patch.TelegramChatID
	}
	if patch.NotifyWebhookURL != nil {
		base.NotifyWebhookURL = patch.NotifyWebhookURL
	}
	if patch.StashPublicURL != nil {
		base.StashPublicURL = patch.StashPublicURL
	}
}

func cloneConfig(in StoredConfig) StoredConfig {
	out := in
	if in.ProwlarrCategories != nil {
		cats := make([]int, len(*in.ProwlarrCategories))
		copy(cats, *in.ProwlarrCategories)
		out.ProwlarrCategories = &cats
	}
	if in.ExcludedSceneTags != nil {
		tags := make([]string, len(*in.ExcludedSceneTags))
		copy(tags, *in.ExcludedSceneTags)
		out.ExcludedSceneTags = &tags
	}
	return out
}
