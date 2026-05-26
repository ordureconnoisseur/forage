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
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/ordureconnoisseur/forager/internal/cache"
	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/configstore"
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
	"stashApiKey":    true,
	"stashdbApiKey":  true,
	"prowlarrApiKey": true,
	"qbitPassword":   true,
	"sabApiKey":      true,
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	cfg, sources := config.Compose(s.bootstrap, s.store.Get())
	fields := map[string]configField{
		"stashUrl":           {Value: cfg.StashURL, Source: sources["stashUrl"]},
		"stashApiKey":        secretField(cfg.StashAPIKey, sources["stashApiKey"]),
		"stashdbUrl":         {Value: cfg.StashDBURL, Source: sources["stashdbUrl"]},
		"stashdbApiKey":      secretField(cfg.StashDBAPIKey, sources["stashdbApiKey"]),
		"prowlarrUrl":        {Value: cfg.ProwlarrURL, Source: sources["prowlarrUrl"]},
		"prowlarrApiKey":     secretField(cfg.ProwlarrAPIKey, sources["prowlarrApiKey"]),
		"prowlarrCategories": {Value: cfg.ProwlarrCategories, Source: sources["prowlarrCategories"]},
		"qbitUrl":            {Value: cfg.QbitURL, Source: sources["qbitUrl"]},
		"qbitUsername":       {Value: cfg.QbitUsername, Source: sources["qbitUsername"]},
		"qbitPassword":       secretField(cfg.QbitPassword, sources["qbitPassword"]),
		"qbitCategory":       {Value: cfg.QbitCategory, Source: sources["qbitCategory"]},
		"sabUrl":             {Value: cfg.SabURL, Source: sources["sabUrl"]},
		"sabApiKey":          secretField(cfg.SabAPIKey, sources["sabApiKey"]),
		"sabCategory":        {Value: cfg.SabCategory, Source: sources["sabCategory"]},
		"libraryRoot":        {Value: cfg.LibraryRoot, Source: sources["libraryRoot"]},
		"pollInterval":       {Value: cfg.PollInterval.String(), Source: sources["pollInterval"]},
		"orphanAfter":        {Value: cfg.OrphanAfter.String(), Source: sources["orphanAfter"]},
		"cacheRefresh":       {Value: cfg.CacheRefresh.String(), Source: sources["cacheRefresh"]},
		"allowedOrigin":      {Value: cfg.AllowedOrigin, Source: sources["allowedOrigin"]},
	}
	writeJSON(w, http.StatusOK, configFieldsResponse{
		Fields: fields,
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
	var patch configstore.Patch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	force := r.URL.Query().Get("force") == "true"

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

	if err := s.store.Set(patch); err != nil {
		s.log.Error("config save", "err", err)
		writeErr(w, http.StatusInternalServerError, "save failed: "+err.Error())
		return
	}

	// Pull the newly-saved config and reload the Pool. invalidate the
	// matcher only when Stash or StashDB creds actually changed —
	// avoids needlessly trashing a heavy-to-build matcher when the
	// user is just editing intervals.
	newCfg := s.composedConfig()
	stashChanged := preview.StashURL != newCfg.StashURL || preview.StashAPIKey != newCfg.StashAPIKey ||
		preview.StashDBURL != newCfg.StashDBURL || preview.StashDBAPIKey != newCfg.StashDBAPIKey
	s.pool.Reload(newCfg)
	if stashChanged {
		s.invalidateMatcher()
		// Kick a background cache refresh so the matcher rebuilds
		// against fresh data on next request.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*60*1e9)
			defer cancel()
			if sc := s.pool.Stash(); sc != nil {
				_ = cache.RefreshPerformers(ctx, sc, s.db, s.log.With("op", "performers", "trigger", "config-save"))
				_ = cache.RefreshStudios(ctx, sc, s.pool.StashDB(), s.db, s.log.With("op", "studios", "trigger", "config-save"))
			}
		}()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"results": results,
	})
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

// previewConfig returns what the composed Config would be if `patch`
// were saved — without actually persisting it. Used by /config/test
// and by /config save (for change detection).
func (s *Server) previewConfig(patch configstore.Patch) config.Config {
	merged := s.store.Get()
	// configstore.applyPatch is unexported; replay the merge logic
	// here. Simpler than exposing it — only the API layer needs this.
	if patch.StashURL != nil {
		merged.StashURL = patch.StashURL
	}
	if patch.StashAPIKey != nil {
		merged.StashAPIKey = patch.StashAPIKey
	}
	if patch.StashDBURL != nil {
		merged.StashDBURL = patch.StashDBURL
	}
	if patch.StashDBAPIKey != nil {
		merged.StashDBAPIKey = patch.StashDBAPIKey
	}
	if patch.ProwlarrURL != nil {
		merged.ProwlarrURL = patch.ProwlarrURL
	}
	if patch.ProwlarrAPIKey != nil {
		merged.ProwlarrAPIKey = patch.ProwlarrAPIKey
	}
	if patch.ProwlarrCategories != nil {
		cats := append([]int(nil), (*patch.ProwlarrCategories)...)
		merged.ProwlarrCategories = &cats
	}
	if patch.QbitURL != nil {
		merged.QbitURL = patch.QbitURL
	}
	if patch.QbitUsername != nil {
		merged.QbitUsername = patch.QbitUsername
	}
	if patch.QbitPassword != nil {
		merged.QbitPassword = patch.QbitPassword
	}
	if patch.QbitCategory != nil {
		merged.QbitCategory = patch.QbitCategory
	}
	if patch.SabURL != nil {
		merged.SabURL = patch.SabURL
	}
	if patch.SabAPIKey != nil {
		merged.SabAPIKey = patch.SabAPIKey
	}
	if patch.SabCategory != nil {
		merged.SabCategory = patch.SabCategory
	}
	if patch.LibraryRoot != nil {
		merged.LibraryRoot = patch.LibraryRoot
	}
	if patch.PollInterval != nil {
		merged.PollInterval = patch.PollInterval
	}
	if patch.OrphanAfter != nil {
		merged.OrphanAfter = patch.OrphanAfter
	}
	if patch.CacheRefresh != nil {
		merged.CacheRefresh = patch.CacheRefresh
	}
	if patch.AllowedOrigin != nil {
		merged.AllowedOrigin = patch.AllowedOrigin
	}
	cfg, _ := config.Compose(s.bootstrap, merged)
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
	if patch.SabURL != nil || patch.SabAPIKey != nil || patch.SabCategory != nil {
		out["sab"] = true
	}
	if patch.LibraryRoot != nil {
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
