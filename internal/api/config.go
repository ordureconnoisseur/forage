// /config endpoints — UI-driven daemon configuration.
//
// Wire shape (camelCase JSON keys throughout) matches what the plugin
// Settings panel sends; secrets are masked on GET so the actual values
// never leave the daemon unless the user explicitly typed a new one
// into the form (in which case the field is set on POST).
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ordureconnoisseur/forager/internal/cache"
	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/configstore"
	"github.com/ordureconnoisseur/forager/internal/pathmap"
	"github.com/ordureconnoisseur/forager/internal/stash"
	"golang.org/x/crypto/bcrypt"
)

// configFieldsResponse is what GET /config returns. Each field comes
// with a value (secrets masked) and a source indicator so the UI can
// show "this is coming from env, not your saved config" hints.
type configFieldsResponse struct {
	Fields  map[string]configField `json:"fields"`
	Section map[string]bool        `json:"sectionConfigured"`
}

type configField struct {
	Value  any                `json:"value"`
	Source config.FieldSource `json:"source"`
	// HasSecret indicates a value is set but masked — UI shows
	// "••••••" placeholder rather than rendering the masked string
	// literally, and treats empty submit as "leave unchanged."
	HasSecret bool `json:"hasSecret,omitempty"`
}

var sensitiveFields = map[string]bool{
	"stashApiKey":      true,
	"stashdbApiKey":    true,
	"prowlarrApiKey":   true,
	"qbitPassword":     true,
	"sabApiKey":        true,
	"adminToken":       true,
	"passwordHash":     true,
	"telegramBotToken": true,
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, configFieldsResponse{
		Fields: s.configFields(),
		Section: map[string]bool{
			"stash":     s.pool.Stash() != nil,
			"stashdb":   s.pool.StashDB() != nil,
			"prowlarr":  s.pool.Prowlarr() != nil,
			"qbit":      s.pool.Qbit() != nil,
			"sab":       s.pool.Sab() != nil,
			"placement": s.pool.Placer().Configured(),
		},
	})
}

// configFields renders the composed config as the masked field map — the
// single place secret masking happens, shared by GET /config and the /diag
// bundle so a new secret can't leak from one while masked in the other.
func (s *Server) configFields() map[string]configField {
	cfg, sources := config.Compose(s.bootstrap, s.store.Get())
	return map[string]configField{
		"stashUrl":            {Value: cfg.StashURL, Source: sources["stashUrl"]},
		"stashApiKey":         secretField(cfg.StashAPIKey, sources["stashApiKey"]),
		"stashdbUrl":          {Value: cfg.StashDBURL, Source: sources["stashdbUrl"]},
		"stashdbApiKey":       secretField(cfg.StashDBAPIKey, sources["stashdbApiKey"]),
		"prowlarrUrl":         {Value: cfg.ProwlarrURL, Source: sources["prowlarrUrl"]},
		"prowlarrApiKey":      secretField(cfg.ProwlarrAPIKey, sources["prowlarrApiKey"]),
		"prowlarrCategories":  {Value: cfg.ProwlarrCategories, Source: sources["prowlarrCategories"]},
		"qbitUrl":             {Value: cfg.QbitURL, Source: sources["qbitUrl"]},
		"qbitUsername":        {Value: cfg.QbitUsername, Source: sources["qbitUsername"]},
		"qbitPassword":        secretField(cfg.QbitPassword, sources["qbitPassword"]),
		"qbitCategory":        {Value: cfg.QbitCategory, Source: sources["qbitCategory"]},
		"sabUrl":              {Value: cfg.SabURL, Source: sources["sabUrl"]},
		"sabApiKey":           secretField(cfg.SabAPIKey, sources["sabApiKey"]),
		"sabCategory":         {Value: cfg.SabCategory, Source: sources["sabCategory"]},
		"downloadRoot":        {Value: cfg.DownloadRoot, Source: sources["downloadRoot"]},
		"trashTtl":            {Value: cfg.TrashTTL.String(), Source: sources["trashTtl"]},
		"seedMaxAge":          {Value: cfg.SeedMaxAge.String(), Source: sources["seedMaxAge"]},
		"seedRatio":           {Value: cfg.SeedRatio, Source: sources["seedRatio"]},
		"seedOverrides":       {Value: cfg.SeedOverrides, Source: sources["seedOverrides"]},
		"libraryRoot":         {Value: cfg.LibraryRoot, Source: sources["libraryRoot"]},
		"stashPathMapping":    {Value: cfg.StashPathMapping, Source: sources["stashPathMapping"]},
		"sabDeleteAfterPlace": {Value: cfg.SabDeleteAfterPlace, Source: sources["sabDeleteAfterPlace"]},
		"packDedupKeep":       {Value: cfg.PackDedupKeep, Source: sources["packDedupKeep"]},
		"releaseRules":        {Value: cfg.ReleaseRules, Source: sources["releaseRules"]},
		"releasePrefs":        {Value: cfg.ReleasePrefs, Source: sources["releasePrefs"]},
		"releaseAdvanced":     {Value: cfg.ReleaseAdvanced, Source: sources["releaseAdvanced"]},
		"excludedSceneTags":   {Value: cfg.ExcludedSceneTags, Source: sources["excludedSceneTags"]},
		"hideMalePerformers":  {Value: cfg.HideMalePerformers, Source: sources["hideMalePerformers"]},
		"pollInterval":        {Value: cfg.PollInterval.String(), Source: sources["pollInterval"]},
		"orphanAfter":         {Value: cfg.OrphanAfter.String(), Source: sources["orphanAfter"]},
		"cacheRefresh":        {Value: cfg.CacheRefresh.String(), Source: sources["cacheRefresh"]},
		"allowedOrigin":       {Value: cfg.AllowedOrigin, Source: sources["allowedOrigin"]},
		"adminToken":          secretField(cfg.AdminToken, sources["adminToken"]),
		"username":            {Value: cfg.Username, Source: sources["username"]},
		// passwordHash is masked like any secret; the UI only reads its
		// hasSecret flag (to show "password is set"), and writes a
		// plaintext `password` field that the daemon hashes — the hash
		// itself never round-trips to the client.
		"passwordHash":     secretField(cfg.PasswordHash, sources["passwordHash"]),
		"telegramBotToken": secretField(cfg.TelegramBotToken, sources["telegramBotToken"]),
		"telegramChatId":   {Value: cfg.TelegramChatID, Source: sources["telegramChatId"]},
		"notifyWebhookUrl": {Value: cfg.NotifyWebhookURL, Source: sources["notifyWebhookUrl"]},
		"stashPublicUrl":   {Value: cfg.StashPublicURL, Source: sources["stashPublicUrl"]},
	}
}

// secretField returns a masked view of a sensitive value. Returns an
// empty Value when unset so the UI knows the field is blank (not
// "••••••"). When set, returns "" + HasSecret=true; the UI renders
// its own placeholder.
func secretField(val string, src config.FieldSource) configField {
	if val == "" {
		return configField{Value: "", Source: src}
	}
	return configField{Value: "", Source: src, HasSecret: true}
}

// postConfig applies a patch to the stored config. Probes each
// changed section in parallel before persisting; on any probe failure
// returns 422 with per-section results unless ?force=true.
func (s *Server) postConfig(w http.ResponseWriter, r *http.Request) {
	// The wire body is the stored patch plus a write-only plaintext
	// `password`. We hash the password into PasswordHash here and never
	// persist the plaintext — StoredConfig has no Password field, so it
	// can't reach disk even by accident.
	var body struct {
		configstore.Patch
		Password *string `json:"password"`
		// CurrentPassword is write-only proof of ownership for a credential
		// change (see credentials.go). Never stored, never echoed.
		CurrentPassword string `json:"currentPassword"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	patch := body.Patch
	if body.Password != nil && *body.Password == "" {
		// Empty password clears the hash → turns password login off
		// (falls back to env/default at compose time, like other
		// cleared fields). Free to compute, so fold it in now and let the
		// value comparison below judge it like any other field.
		empty := ""
		patch.PasswordHash = &empty
	}
	// A NEW password is deliberately NOT hashed yet. bcrypt is ~100ms, and
	// hashing before the guard would hand an unproven caller that much of
	// the daemon's CPU per request, for free, on a route they can retry as
	// fast as they like: exactly the work the throttle exists to refuse.
	// So declare it a credential change without computing it, and hash
	// only once the caller has proven they may.
	settingPassword := body.Password != nil && *body.Password != ""
	force := r.URL.Query().Get("force") == "true"

	// Credential guard, BEFORE any probe or save: a patch that would move
	// any value the auth gate accepts as proof of identity has to prove
	// ownership first. Computed by comparing the composed config as it is
	// against the composed config as the patch would leave it, so it does
	// not matter which wire field carried the change: `password`,
	// `passwordHash`, `adminToken`, `stashApiKey`, or something added
	// later. See credentials.go for why it is done by value and not by
	// field name.
	oldCfg := s.composedConfig()
	changedCreds := changedCredentials(oldCfg, s.previewConfig(patch))
	if settingPassword {
		// Always a change, even if it is the same password: the hash is
		// re-salted, so there is no such thing as a no-op here.
		changedCreds = alsoChanged(changedCreds, "passwordHash")
	}
	if len(changedCreds) > 0 {
		// currentPassword is checked with bcrypt and a caller holding a
		// session cookie can retry it forever, so it is a guessing surface
		// in its own right and gets its own budget. Its own, not the login
		// form's: a locked-out attacker here must not be able to stop the
		// owner signing in.
		if s.throttled(scopeConfig, w, r) {
			return
		}
		proof := s.provenCredentialChange(r, body.CurrentPassword)
		if proof == proofNone {
			s.noteAuthFailure(scopeConfig, r)
			s.logAuth(r, slog.LevelWarn, "credential-change-refused",
				"fields", strings.Join(changedCreds, ","))
			writeCredentialProofRequired(w, changedCreds)
			return
		}
		s.noteAuthSuccess(scopeConfig, r)
		s.logCredentialChange(r, changedCreds, proof)
	}

	// Proven, so the bcrypt cost is now the caller's to spend.
	if settingPassword {
		hashed, err := bcrypt.GenerateFromPassword([]byte(*body.Password), bcrypt.DefaultCost)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "could not hash password")
			return
		}
		h := string(hashed)
		patch.PasswordHash = &h
	}

	// Compose what the resulting config WOULD be if we applied the
	// patch, then probe the changed sections.
	preview := s.previewConfig(patch)
	results := s.probeSections(r.Context(), preview, sectionsTouchedBy(patch))

	failed := false
	for _, ok := range results {
		if !ok.OK {
			failed = true
			break
		}
	}
	if failed && !force {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":   "probe failed for one or more sections",
			"results": results,
		})
		return
	}

	// oldCfg was snapshotted before the probes ran, above: the stashChanged
	// comparison below must be old-vs-new. `preview` is useless for that:
	// it is already oldStored+patch, which is exactly what the store holds
	// after Set, so comparing it against the post-save compose was always
	// false and a Stash/StashDB credential change never invalidated the
	// matcher: every matcher-backed path kept clients built from the old
	// key until the daemon restarted.
	if err := s.store.Set(patch); err != nil {
		s.log.Error("config save", "err", err)
		writeErr(w, http.StatusInternalServerError, "save failed: "+err.Error())
		return
	}

	// Any credential change rotates the session signing key, revoking every
	// outstanding cookie, since the change wouldn't mean much if a session
	// minted under the old credential kept working. The caller's own
	// browser gets a fresh cookie in the same response so saving a new
	// password doesn't dump them at the login screen; everyone else stays
	// revoked. (The request proved ownership above, so re-issuing is safe.)
	//
	// This keys off changedCreds, not off which wire field was set. The
	// previous condition (`body.Password != nil || patch.AdminToken != nil`)
	// meant a password swapped in via the raw `passwordHash` field revoked
	// nothing at all: the attacker's own session survived their own
	// takeover.
	if len(changedCreds) > 0 {
		s.rotateSessionKey()
		if c, cerr := r.Cookie(sessionCookieName); cerr == nil && c.Value != "" {
			if id, serr := s.newSession(); serr == nil {
				setSessionCookie(w, r, id)
			}
		}
	}

	// Pull the newly-saved config and reload the Pool. invalidate the
	// matcher only when Stash or StashDB creds actually changed —
	// avoids needlessly trashing a heavy-to-build matcher when the
	// user is just editing intervals.
	newCfg := s.composedConfig()
	stashChanged := oldCfg.StashURL != newCfg.StashURL || oldCfg.StashAPIKey != newCfg.StashAPIKey ||
		oldCfg.StashDBURL != newCfg.StashDBURL || oldCfg.StashDBAPIKey != newCfg.StashDBAPIKey
	s.pool.Reload(newCfg)
	if stashChanged {
		s.invalidateMatcher()
		// Kick a background cache refresh so the matcher rebuilds
		// against fresh data on next request.
		go func() {
			defer s.recoverPanic("config-save cache refresh")
			ctx, cancel := context.WithTimeout(context.Background(), 5*60*1e9)
			defer cancel()
			if sc := s.pool.Stash(); sc != nil {
				_ = cache.RefreshPerformers(ctx, sc, s.pool.StashDB(), s.db, s.log.With("op", "performers", "trigger", "config-save"))
				_ = cache.RefreshStudios(ctx, sc, s.pool.StashDB(), s.db, s.log.With("op", "studios", "trigger", "config-save"))
			}
		}()
	}

	// Switching "hide male performers" ON needs genders for the un-owned
	// performers on scene cards, and nothing else in forage fetches those.
	// Waiting for the 5m ticker would leave the setting looking broken for
	// the first few minutes after saving it, so kick a pass now.
	if !oldCfg.HideMalePerformers && newCfg.HideMalePerformers {
		go func() {
			defer s.recoverPanic("config-save gender backfill")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if sdb := s.pool.StashDB(); sdb != nil {
				_, _ = cache.BackfillGenders(ctx, sdb, s.db,
					s.log.With("op", "genders", "trigger", "config-save"),
					cache.GenderBackfillLimit)
			}
		}()
	}

	// Create (or repoint) the forage category in whichever download clients
	// are configured, so the user never has to go and do it by hand. This is
	// after the Pool reload so the clients are built from the just-saved
	// credentials.
	catResults := s.ensureDownloadCategories(r.Context(), newCfg)
	// And keep that same folder out of Stash's library, for the layout where
	// it lives inside the library root.
	excl := s.ensureStashExclusion(r.Context(), newCfg)

	out := map[string]any{
		"ok":      true,
		"results": results,
	}
	if len(catResults) > 0 {
		out["categories"] = catResults
	}
	if excl != "" {
		out["stash_exclusion"] = excl
	}
	writeJSON(w, http.StatusOK, out)
}

// ensureDownloadCategories points each configured download client's forage
// category at the download folder, creating it if it isn't there.
//
// Setting that category up by hand was the one cross-app step the setup wizard
// had to hand back to the user — "go into qBittorrent, make a category, set its
// save path, come back and type the name here" — and getting it wrong puts
// finished downloads outside the filesystem forage hardlinks from, which is the
// most confusing possible failure: everything appears configured, downloads
// complete, and nothing ever lands in the library.
//
// Best-effort and non-fatal: the config is already saved, and a client that
// refuses (older qBit, a read-only SAB config) still works if the user makes
// the category themselves. Results are reported so the UI can say what
// happened rather than leaving it invisible.
//
// Skipped entirely when no download folder is set, since the category's whole
// purpose here is to point somewhere specific.
func (s *Server) ensureDownloadCategories(ctx context.Context, cfg config.Config) map[string]string {
	if strings.TrimSpace(cfg.DownloadRoot) == "" {
		return nil
	}
	out := map[string]string{}
	if qb := s.pool.Qbit(); qb != nil && cfg.QbitCategory != "" {
		if err := qb.EnsureCategory(ctx, cfg.QbitCategory, cfg.DownloadRoot); err != nil {
			s.log.Warn("ensure qbit category", "category", cfg.QbitCategory, "err", err)
			out["qbit"] = "failed: " + err.Error()
		} else {
			s.log.Info("qbit category ready", "category", cfg.QbitCategory, "path", cfg.DownloadRoot)
			out["qbit"] = "ready"
		}
	}
	if sb := s.pool.Sab(); sb != nil && cfg.SabCategory != "" {
		if err := sb.EnsureCategory(ctx, cfg.SabCategory, cfg.DownloadRoot); err != nil {
			s.log.Warn("ensure sab category", "category", cfg.SabCategory, "err", err)
			out["sab"] = "failed: " + err.Error()
		} else {
			s.log.Info("sab category ready", "category", cfg.SabCategory, "path", cfg.DownloadRoot)
			out["sab"] = "ready"
		}
	}
	return out
}

func (s *Server) postConfigTest(w http.ResponseWriter, r *http.Request) {
	section := chi.URLParam(r, "section")
	var patch configstore.Patch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	preview := s.previewConfig(patch)
	results := s.probeSections(r.Context(), preview, []string{section})
	writeJSON(w, http.StatusOK, map[string]any{
		"section": section,
		"result":  results[section],
	})
}

// postStashDBFromStash reads the user's StashDB connection (endpoint +
// api_key) straight from their Stash so the setup wizard can pre-fill the
// StashDB step instead of making them paste the key again — they've already
// configured it in Stash. Takes the Stash URL + key in the body (the wizard
// calls this right after the Stash test passes, before anything's saved).
// Returns {found, url, api_key} for the StashDB box; found:false when Stash
// has no stashdb.org box configured. The user still confirms before it's used.
// api_key is only included when the caller supplied the Stash credentials in
// the body — see callerHasStashKey below.
func (s *Server) postStashDBFromStash(w http.ResponseWriter, r *http.Request) {
	var body struct {
		StashURL    string `json:"stashUrl"`
		StashAPIKey string `json:"stashApiKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	// Fall back to the already-configured Stash if the body omits them
	// (e.g. re-running setup on a configured daemon).
	cfg := s.composedConfig()
	url := strings.TrimSpace(body.StashURL)
	if url == "" {
		url = cfg.StashURL
	}
	// callerHasStashKey: the caller supplied the Stash key themselves
	// (first-run wizard), proving they hold Stash access — anyone with the
	// Stash key can read the StashDB key out of Stash's own config anyway,
	// so echoing it back adds nothing. The fallback path instead runs on
	// the daemon's SAVED credentials, where echoing would convert plain
	// forage access (possibly open-mode) into a plaintext StashDB key —
	// the one hole in the secrets-are-masked rule. There the response
	// omits api_key and the wizard simply skips the pre-fill.
	callerHasStashKey := body.StashAPIKey != ""
	key := body.StashAPIKey
	if key == "" {
		key = cfg.StashAPIKey
	}
	if url == "" || key == "" {
		writeErr(w, http.StatusBadRequest, "stash url + api key required")
		return
	}
	if err := checkProbeURL(url); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	sc := stash.New(url, key)
	boxes, err := sc.StashBoxConfigs(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "couldn't read stash config: "+err.Error())
		return
	}
	for _, b := range boxes {
		if strings.Contains(strings.ToLower(b.Endpoint), stash.StashDBEndpointHost) && b.APIKey != "" {
			// Hand back the GraphQL endpoint's base (strip /graphql) as the
			// URL the StashDB field expects, plus the key when permitted.
			stashdbURL := strings.TrimSuffix(b.Endpoint, "/graphql")
			resp := map[string]any{
				"found": true,
				"url":   stashdbURL,
			}
			if callerHasStashKey {
				resp["api_key"] = b.APIKey
			}
			writeJSON(w, http.StatusOK, resp)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"found": false})
}

// previewConfig returns what the composed Config would be if `patch`
// were saved, without actually persisting it. Used by /config/test, by
// the credential guard, and by /config save (for change detection).
//
// It delegates the merge to configstore.Merged rather than replaying it.
// The replay this replaced enumerated fields by hand and had already
// drifted (downloadRoot, trashTtl, seedMaxAge, seedRatio, seedOverrides,
// releasePrefs and releaseAdvanced were missing from it, so a /config/test
// never saw them). That drift is survivable for a probe; it is not
// survivable for the credential guard, which has to be sure it is looking
// at the same merge Set will perform.
func (s *Server) previewConfig(patch configstore.Patch) config.Config {
	cfg, _ := config.Compose(s.bootstrap, configstore.Merged(s.store.Get(), patch))
	return cfg
}

// sectionsTouchedBy returns the set of section names whose fields
// appear in the patch. Drives which probes get run.
func sectionsTouchedBy(patch configstore.Patch) []string {
	out := map[string]bool{}
	if patch.StashURL != nil || patch.StashAPIKey != nil {
		out["stash"] = true
	}
	if patch.StashDBURL != nil || patch.StashDBAPIKey != nil {
		out["stashdb"] = true
	}
	if patch.ProwlarrURL != nil || patch.ProwlarrAPIKey != nil || patch.ProwlarrCategories != nil {
		out["prowlarr"] = true
	}
	if patch.QbitURL != nil || patch.QbitUsername != nil || patch.QbitPassword != nil || patch.QbitCategory != nil {
		out["qbit"] = true
	}
	if patch.SabURL != nil || patch.SabAPIKey != nil || patch.SabCategory != nil || patch.SabDeleteAfterPlace != nil {
		out["sab"] = true
	}
	if patch.LibraryRoot != nil || patch.StashPathMapping != nil {
		out["placement"] = true
	}
	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	return keys
}

// joinIssues collapses a list of strings into a single human-readable
// message for the UI's result line.
func joinIssues(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "; ")
}

// ensureStashExclusion keeps forage's download folder out of Stash's library,
// when that folder sits inside a path Stash scans.
//
// Only then: with the download root a SIBLING of the library, Stash never
// walks it and an exclusion would be noise. It becomes essential the moment
// downloads live inside the library root, which is the layout worth
// recommending, because it is the only one where a single folder from the user
// guarantees one filesystem and therefore working hardlinks.
//
// forage writes the pattern rather than documenting it because Stash's
// exclusion is a raw regex matched against a raw path and it fails SILENTLY.
// The reference library's own hand-written rule used forward slashes against
// Windows paths and excluded nothing at all; 29,045 images the user had ruled
// out were indexed anyway, with no error and no count to notice. A rule that
// important should not depend on someone getting a regex right, in the same
// way the qBit and SAB categories above are not left to be typed by hand.
//
// Best-effort and non-fatal, like the categories: the config is saved either
// way, and the result is reported so the UI can say what happened.
func (s *Server) ensureStashExclusion(ctx context.Context, cfg config.Config) string {
	dl := strings.TrimSpace(cfg.DownloadRoot)
	if dl == "" {
		return ""
	}
	sc := s.pool.Stash()
	if sc == nil {
		return ""
	}
	// Stash's namespace, not forage's: the daemon says /data/porn/downloads
	// where Stash says Z:\downloads, and the pattern is matched against what
	// STASH stores.
	stashDL := pathmap.Translate(dl, s.pool.Settings().StashPathMapping)
	if stashDL == "" {
		stashDL = dl
	}
	libCfg, err := sc.LibraryConfig(ctx)
	if err != nil {
		s.log.Warn("stash exclusion: read library config", "err", err)
		return "failed: " + err.Error()
	}
	inside := false
	for _, p := range libCfg.Paths {
		if stash.PathInside(stashDL, p) {
			inside = true
			break
		}
	}
	if !inside {
		return "not needed (download folder is outside every Stash library path)"
	}
	pattern := stash.ExcludePattern(stashDL)
	if pattern == "" {
		return ""
	}
	added, err := sc.AddVideoExclude(ctx, pattern)
	if err != nil {
		s.log.Warn("stash exclusion: add", "pattern", pattern, "err", err)
		return "failed: " + err.Error()
	}
	if !added {
		return "already excluded"
	}
	s.log.Info("stash exclusion added", "pattern", pattern, "path", stashDL)
	return "added " + pattern
}
