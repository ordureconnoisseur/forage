// Per-section connectivity probes used by /config save and
// /config/test/{section}. Each probe runs against a *preview* config —
// not the live one — so the UI can validate "would these credentials
// work?" before committing them.
package api

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/placer"
	"github.com/ordureconnoisseur/forager/internal/prowlarr"
	"github.com/ordureconnoisseur/forager/internal/qbit"
	"github.com/ordureconnoisseur/forager/internal/sabnzbd"
	"github.com/ordureconnoisseur/forager/internal/stash"
	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

type probeResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

const probeTimeout = 5 * time.Second

// probeSections runs the named probes in parallel against the preview
// config and returns the keyed results. Probe failures don't stop
// other probes — each section gets its own result.
func (s *Server) probeSections(parent context.Context, cfg config.Config, sections []string) map[string]probeResult {
	if len(sections) == 0 {
		return map[string]probeResult{}
	}
	results := make(map[string]probeResult, len(sections))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, section := range sections {
		section := section
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(parent, probeTimeout)
			defer cancel()
			r := s.probeOne(ctx, cfg, section)
			mu.Lock()
			results[section] = r
			mu.Unlock()
		}()
	}
	wg.Wait()
	return results
}

func (s *Server) probeOne(ctx context.Context, cfg config.Config, section string) probeResult {
	switch section {
	case "stash":
		if cfg.StashURL == "" || cfg.StashAPIKey == "" {
			return probeResult{OK: false, Message: "url or api key missing"}
		}
		c := stash.New(cfg.StashURL, cfg.StashAPIKey)
		v, err := c.Version(ctx)
		if err != nil {
			return probeResult{OK: false, Message: err.Error()}
		}
		return probeResult{OK: true, Message: "stash " + v}
	case "stashdb":
		if cfg.StashDBAPIKey == "" {
			return probeResult{OK: false, Message: "api key missing"}
		}
		c := stashdb.New(cfg.StashDBURL, cfg.StashDBAPIKey)
		user, err := c.Me(ctx)
		if err != nil {
			return probeResult{OK: false, Message: err.Error()}
		}
		return probeResult{OK: true, Message: "authenticated as " + user}
	case "prowlarr":
		if cfg.ProwlarrURL == "" || cfg.ProwlarrAPIKey == "" {
			return probeResult{OK: false, Message: "url or api key missing"}
		}
		c := prowlarr.New(cfg.ProwlarrURL, cfg.ProwlarrAPIKey)
		v, err := c.Status(ctx)
		if err != nil {
			return probeResult{OK: false, Message: err.Error()}
		}
		return probeResult{OK: true, Message: "prowlarr " + v}
	case "qbit":
		if cfg.QbitURL == "" {
			return probeResult{OK: false, Message: "url missing"}
		}
		c := qbit.New(cfg.QbitURL, cfg.QbitUsername, cfg.QbitPassword)
		v, err := c.Version(ctx)
		if err != nil {
			return probeResult{OK: false, Message: err.Error()}
		}
		return probeResult{OK: true, Message: "qbit " + v}
	case "sab":
		if cfg.SabURL == "" || cfg.SabAPIKey == "" {
			return probeResult{OK: false, Message: "url or api key missing"}
		}
		c := sabnzbd.New(cfg.SabURL, cfg.SabAPIKey)
		v, err := c.Version(ctx)
		if err != nil {
			return probeResult{OK: false, Message: err.Error()}
		}
		return probeResult{OK: true, Message: "sab " + v}
	case "placement":
		return probePlacement(cfg)
	}
	return probeResult{OK: false, Message: "unknown section: " + section}
}

// probePlacement checks the library root exists and is writable by
// creating + deleting a tmp file. Mirrors what the placer actually
// needs at runtime.
func probePlacement(cfg config.Config) probeResult {
	if cfg.LibraryRoot == "" {
		return probeResult{OK: true, Message: "placement disabled (library root unset)"}
	}
	pl := placer.New(cfg.LibraryRoot, nil)
	if !pl.Configured() {
		return probeResult{OK: false, Message: "library root resolved to empty"}
	}
	info, err := os.Stat(cfg.LibraryRoot)
	if err != nil {
		return probeResult{OK: false, Message: "stat: " + err.Error()}
	}
	if !info.IsDir() {
		return probeResult{OK: false, Message: "path exists but is not a directory"}
	}
	probe := filepath.Join(cfg.LibraryRoot, ".forage-write-probe")
	f, err := os.Create(probe)
	if err != nil {
		return probeResult{OK: false, Message: "write probe failed: " + err.Error()}
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return probeResult{OK: true, Message: "library root writable"}
}
