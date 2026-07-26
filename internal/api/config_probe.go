// Per-section connectivity probes used by /config save and
// /config/test/{section}. Each probe runs against a *preview* config —
// not the live one — so the UI can validate "would these credentials
// work?" before committing them.
package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ordureconnoisseur/forager/internal/clienterr"
	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/placer"
	"github.com/ordureconnoisseur/forager/internal/prowlarr"
	"github.com/ordureconnoisseur/forager/internal/qbit"
	"github.com/ordureconnoisseur/forager/internal/sabnzbd"
	"github.com/ordureconnoisseur/forager/internal/stash"
	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

// checkProbeURL rejects probe targets that aren't plain http(s) URLs with a
// host. Probing user-supplied INTERNAL urls is this feature's whole job (the
// admin is telling us where their Prowlarr/qBit live), so this is not an
// SSRF allowlist — it just refuses scheme smuggling and garbage before we
// build a client around it. Anything beyond that (who may reach these
// endpoints at all) is the auth middleware's problem.
func checkProbeURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("not a valid url")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("url must be http(s) with a host")
	}
	return nil
}

type probeResult struct {
	OK bool `json:"ok"`
	// Message is written for a human: what went wrong and what to check.
	// Raw client errors ("dial tcp 127.0.0.1:9696: connectex: No connection
	// could be made because the target machine actively refused it") are
	// accurate but read like a stack trace, and a first-time user who
	// mistyped a port does not reliably get "the port is wrong" out of one.
	Message string `json:"message,omitempty"`
	// Detail is that raw error, kept so the diagnosis isn't lost — the UI
	// shows it behind a toggle.
	Detail string `json:"detail,omitempty"`
}

// probeFail turns a client error into something a person can act on, keeping
// the original as Detail. service is the product name as the user knows it
// ("Prowlarr"), target the address they typed.
func probeFail(service, target string, err error) probeResult {
	detail := err.Error()
	var msg string
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		msg = fmt.Sprintf("%s didn't answer within %s. Is it running, and reachable from where forage is?",
			service, probeTimeout)
	case isConnectionError(err):
		msg = fmt.Sprintf("Couldn't reach %s at %s. Check it's running and that the address and port are right.",
			service, target)
	case errors.Is(err, clienterr.ErrNotFound):
		msg = fmt.Sprintf("Something answered at %s, but it isn't %s's API. Usually a wrong port, or a missing base path.",
			target, service)
	case errors.Is(err, clienterr.ErrRejected):
		msg = fmt.Sprintf("%s refused the request. That almost always means the API key is wrong.", service)
	default:
		msg = fmt.Sprintf("Couldn't talk to %s at %s.", service, target)
	}
	return probeResult{OK: false, Message: msg, Detail: detail}
}

// stashdbTarget names StashDB's address for an error message. The URL is
// optional in config (empty means the public stashdb.org endpoint), and
// "Couldn't reach StashDB at ." reads like a bug.
func stashdbTarget(u string) string {
	if u == "" {
		return "stashdb.org"
	}
	return u
}

// isConnectionError reports whether the failure happened before any HTTP
// response — refused, DNS failure, unroutable. Those all mean "the address is
// wrong or the service is down", which is a different fix from a bad key.
func isConnectionError(err error) bool {
	var opErr *net.OpError
	var dnsErr *net.DNSError
	return errors.As(err, &opErr) || errors.As(err, &dnsErr)
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
		if err := checkProbeURL(cfg.StashURL); err != nil {
			return probeResult{OK: false, Message: err.Error()}
		}
		c := stash.New(cfg.StashURL, cfg.StashAPIKey)
		v, err := c.Version(ctx)
		if err != nil {
			return probeFail("Stash", cfg.StashURL, err)
		}
		return probeResult{OK: true, Message: "stash " + v}
	case "stashdb":
		if cfg.StashDBAPIKey == "" {
			return probeResult{OK: false, Message: "api key missing"}
		}
		if cfg.StashDBURL != "" {
			if err := checkProbeURL(cfg.StashDBURL); err != nil {
				return probeResult{OK: false, Message: err.Error()}
			}
		}
		c := stashdb.New(cfg.StashDBURL, cfg.StashDBAPIKey)
		user, err := c.Me(ctx)
		if err != nil {
			return probeFail("StashDB", stashdbTarget(cfg.StashDBURL), err)
		}
		return probeResult{OK: true, Message: "authenticated as " + user}
	case "prowlarr":
		if cfg.ProwlarrURL == "" || cfg.ProwlarrAPIKey == "" {
			return probeResult{OK: false, Message: "url or api key missing"}
		}
		if err := checkProbeURL(cfg.ProwlarrURL); err != nil {
			return probeResult{OK: false, Message: err.Error()}
		}
		c := prowlarr.New(cfg.ProwlarrURL, cfg.ProwlarrAPIKey)
		v, err := c.Status(ctx)
		if err != nil {
			return probeFail("Prowlarr", cfg.ProwlarrURL, err)
		}
		return probeResult{OK: true, Message: "prowlarr " + v}
	case "qbit":
		if cfg.QbitURL == "" {
			return probeResult{OK: false, Message: "url missing"}
		}
		if err := checkProbeURL(cfg.QbitURL); err != nil {
			return probeResult{OK: false, Message: err.Error()}
		}
		c := qbit.New(cfg.QbitURL, cfg.QbitUsername, cfg.QbitPassword)
		v, err := c.Version(ctx)
		if err != nil {
			return probeFail("qBittorrent", cfg.QbitURL, err)
		}
		return probeResult{OK: true, Message: "qbit " + v}
	case "sab":
		if cfg.SabURL == "" || cfg.SabAPIKey == "" {
			return probeResult{OK: false, Message: "url or api key missing"}
		}
		if err := checkProbeURL(cfg.SabURL); err != nil {
			return probeResult{OK: false, Message: err.Error()}
		}
		c := sabnzbd.New(cfg.SabURL, cfg.SabAPIKey)
		v, err := c.Version(ctx)
		if err != nil {
			return probeFail("SABnzbd", cfg.SabURL, err)
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
		if os.IsNotExist(err) {
			return probeResult{
				OK:      false,
				Message: "That folder doesn't exist. Create it first, or check the path — inside Docker it has to be the path as the container sees it.",
				Detail:  err.Error(),
			}
		}
		return probeResult{OK: false, Message: "Couldn't read that folder.", Detail: err.Error()}
	}
	if !info.IsDir() {
		return probeResult{OK: false, Message: "That path is a file, not a folder."}
	}
	probe := filepath.Join(cfg.LibraryRoot, ".forage-write-probe")
	f, err := os.Create(probe)
	if err != nil {
		return probeResult{
			OK:      false,
			Message: "That folder exists but forage can't write to it. Check the permissions on it.",
			Detail:  err.Error(),
		}
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return probeResult{OK: true, Message: "library root writable"}
}
