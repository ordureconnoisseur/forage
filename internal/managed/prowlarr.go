// Package managed installs and supervises a Prowlarr instance so users
// don't have to: forage downloads the official portable build, seeds its
// config (localhost bind, /prowlarr url base, a forage-generated API key),
// runs it as a child process, and restarts it when it exits. The wizard's
// indexer step becomes "add your indexers" instead of "go install and wire
// up another application".
//
// Only Prowlarr's location and lifecycle are managed — its indexer
// configuration stays Prowlarr's own, reachable through forage's
// authenticated /prowlarr/ reverse proxy.
package managed

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// State is the installer/supervisor state machine, surfaced verbatim to
// the UI.
type State string

const (
	StateAbsent      State = "absent"      // nothing installed
	StateDownloading State = "downloading" // fetching the release archive
	StateInstalling  State = "installing"  // extracting + seeding config
	StateStarting    State = "starting"    // process launched, port not answering yet
	StateRunning     State = "running"
	StateStopped     State = "stopped" // installed but not supervised (daemon shutdown)
	StateError       State = "error"
)

// Prowlarr manages one installed Prowlarr instance under root/.
type Prowlarr struct {
	root string // <data>/managed/prowlarr
	log  *slog.Logger

	mu         sync.Mutex
	state      State
	detail     string // human-readable: error text, download progress
	doneBytes  int64
	totalBytes int64
	version    string
	port       int
	apiKey     string
	cancelSup  context.CancelFunc // stops the supervisor loop
	supDone    chan struct{}
}

// NewProwlarr returns a manager rooted at dataDir/managed/prowlarr. It
// reads any existing install's seeded config so URL/APIKey are usable
// immediately; it does not start anything.
func NewProwlarr(dataDir string, log *slog.Logger) *Prowlarr {
	p := &Prowlarr{
		root:  filepath.Join(dataDir, "managed", "prowlarr"),
		log:   log,
		state: StateAbsent,
	}
	if p.Installed() {
		p.state = StateStopped
		p.loadSeededConfig()
		if v, err := os.ReadFile(filepath.Join(p.root, "version.txt")); err == nil {
			// First line is the release tag; the rest is the verified
			// sha256 audit record.
			p.version, _, _ = strings.Cut(strings.TrimSpace(string(v)), "\n")
		}
	}
	return p
}

// Supported reports whether managed mode makes sense here: an official
// Prowlarr build exists for the platform and we're not inside a container
// (the Docker story is the pre-wired compose stack, not a child process
// in an Alpine image that can't run glibc binaries).
func Supported() bool {
	if assetSuffix() == "" {
		return false
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return false
	}
	return true
}

func (p *Prowlarr) appDir() string    { return filepath.Join(p.root, "app") }
func (p *Prowlarr) configDir() string { return filepath.Join(p.root, "config") }

func (p *Prowlarr) binPath() string {
	name := "Prowlarr"
	if runtime.GOOS == "windows" {
		name = "Prowlarr.exe"
	}
	return filepath.Join(p.appDir(), name)
}

// Installed reports whether a runnable Prowlarr binary is in place.
func (p *Prowlarr) Installed() bool {
	fi, err := os.Stat(p.binPath())
	return err == nil && !fi.IsDir()
}

// URL is the base URL forage's Prowlarr client should use. Empty until
// installed.
func (p *Prowlarr) URL() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.port == 0 {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d/prowlarr", p.port)
}

// APIKey returns the forage-generated key seeded into Prowlarr's config.
func (p *Prowlarr) APIKey() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.apiKey
}

// ProxyTarget returns the host:port the /prowlarr/ reverse proxy should
// dial, or "" when nothing is installed.
func (p *Prowlarr) ProxyTarget() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.port == 0 {
		return ""
	}
	return fmt.Sprintf("127.0.0.1:%d", p.port)
}

// Status is the UI-facing snapshot.
type Status struct {
	Supported bool   `json:"supported"`
	Installed bool   `json:"installed"`
	State     State  `json:"state"`
	Detail    string `json:"detail,omitempty"`
	Done      int64  `json:"doneBytes,omitempty"`
	Total     int64  `json:"totalBytes,omitempty"`
	Version   string `json:"version,omitempty"`
	URL       string `json:"url,omitempty"`
}

func (p *Prowlarr) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := Status{
		Supported: Supported(),
		// Filesystem truth, not state inference: a failed install must not
		// claim "installed" just because the state machine left absent.
		Installed: p.Installed(),
		State:     p.state,
		Detail:    p.detail,
		Done:      p.doneBytes,
		Total:     p.totalBytes,
		Version:   p.version,
	}
	if p.port != 0 {
		s.URL = fmt.Sprintf("http://127.0.0.1:%d/prowlarr", p.port)
	}
	return s
}

func (p *Prowlarr) setState(s State, detail string) {
	p.mu.Lock()
	p.state = s
	p.detail = detail
	p.mu.Unlock()
}

// Install downloads and unpacks the latest official release, seeds the
// config, and records the version. Blocking; call from a goroutine. Safe
// to re-run (a failed install can be retried; a completed one upgrades in
// place while stopped).
func (p *Prowlarr) Install(ctx context.Context) error {
	if !Supported() {
		return fmt.Errorf("managed Prowlarr is not supported on this platform")
	}
	p.mu.Lock()
	if p.state == StateDownloading || p.state == StateInstalling {
		p.mu.Unlock()
		return fmt.Errorf("install already in progress")
	}
	running := p.state == StateRunning || p.state == StateStarting
	p.mu.Unlock()
	if running {
		return fmt.Errorf("stop the managed Prowlarr before reinstalling")
	}

	fail := func(err error) error {
		p.setState(StateError, err.Error())
		p.log.Error("managed prowlarr install failed", "err", err)
		return err
	}

	p.setState(StateDownloading, "resolving latest release")
	hc := httpClientForDownloads()
	tag, asset, err := resolveLatest(ctx, hc)
	if err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(p.root, 0o755); err != nil {
		return fail(err)
	}
	archive := filepath.Join(p.root, "download.tmp")
	defer os.Remove(archive)
	p.log.Info("downloading managed prowlarr", "tag", tag, "asset", asset.Name, "bytes", asset.Size)
	// download verifies the bytes against the digest published with the
	// release before returning, so reaching the extract below means the
	// archive is the one GitHub serves for this tag. Nothing is unpacked or
	// executed on the failure path.
	sum, err := download(ctx, hc, asset, archive, func(done, total int64) {
		p.mu.Lock()
		p.doneBytes, p.totalBytes = done, total
		p.mu.Unlock()
	})
	if err != nil {
		return fail(fmt.Errorf("download %s: %w", asset.Name, err))
	}

	p.setState(StateInstalling, "extracting "+asset.Name)
	// Extract into a fresh dir and swap, so a torn extract never leaves a
	// half-runnable app dir.
	fresh := p.appDir() + ".new"
	_ = os.RemoveAll(fresh)
	if err := extract(archive, asset.Name, fresh); err != nil {
		_ = os.RemoveAll(fresh)
		return fail(fmt.Errorf("extract: %w", err))
	}
	_ = os.RemoveAll(p.appDir())
	if err := os.Rename(fresh, p.appDir()); err != nil {
		return fail(fmt.Errorf("install app dir: %w", err))
	}
	if err := p.seedConfig(); err != nil {
		return fail(fmt.Errorf("seed config: %w", err))
	}
	record := fmt.Sprintf("%s\n%s  %s\n", tag, sum, asset.Name)
	_ = os.WriteFile(filepath.Join(p.root, "version.txt"), []byte(record), 0o644)
	p.mu.Lock()
	p.version = tag
	p.mu.Unlock()
	p.setState(StateStopped, "")
	p.log.Info("managed prowlarr installed", "tag", tag, "sha256", sum)
	return nil
}

// prowlarrConfig mirrors the handful of config.xml fields we seed/read.
type prowlarrConfig struct {
	XMLName                xml.Name `xml:"Config"`
	BindAddress            string
	Port                   int
	UrlBase                string
	ApiKey                 string
	AuthenticationMethod   string
	AuthenticationRequired string
	AnalyticsEnabled       bool
}

// seedConfig writes Prowlarr's config.xml before first start: bound to
// localhost only (the world reaches it through forage's authenticated
// proxy), /prowlarr url base, auth delegated to that proxy, and an API
// key forage generates so it never has to scrape one out later. An
// existing config (reinstall/upgrade) is kept as-is.
func (p *Prowlarr) seedConfig() error {
	if err := os.MkdirAll(p.configDir(), 0o755); err != nil {
		return err
	}
	path := filepath.Join(p.configDir(), "config.xml")
	if _, err := os.Stat(path); err == nil {
		return p.loadSeededConfig()
	}
	port, err := freePort()
	if err != nil {
		return err
	}
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		return err
	}
	key := hex.EncodeToString(keyBytes)
	cfg := prowlarrConfig{
		BindAddress:            "127.0.0.1",
		Port:                   port,
		UrlBase:                "/prowlarr",
		ApiKey:                 key,
		AuthenticationMethod:   "External",
		AuthenticationRequired: "DisabledForLocalAddresses",
		AnalyticsEnabled:       false,
	}
	raw, err := xml.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return err
	}
	p.mu.Lock()
	p.port, p.apiKey = port, key
	p.mu.Unlock()
	return nil
}

// loadSeededConfig reads port + api key back from an existing config.xml.
func (p *Prowlarr) loadSeededConfig() error {
	raw, err := os.ReadFile(filepath.Join(p.configDir(), "config.xml"))
	if err != nil {
		return err
	}
	var cfg prowlarrConfig
	if err := xml.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parse config.xml: %w", err)
	}
	p.mu.Lock()
	p.port, p.apiKey = cfg.Port, cfg.ApiKey
	p.mu.Unlock()
	return nil
}

// freePort binds port 0 on loopback and reports what the kernel handed
// out — no guess-and-scan, no race against other listeners' fixed ports.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// Start launches the supervisor: run the binary, restart with backoff when
// it exits, until ctx is done or Stop is called. Returns once the process
// has been launched (StateStarting/StateRunning follow asynchronously).
func (p *Prowlarr) Start(ctx context.Context) error {
	if !p.Installed() {
		return fmt.Errorf("prowlarr is not installed")
	}
	p.mu.Lock()
	if p.cancelSup != nil {
		p.mu.Unlock()
		return nil // already supervising
	}
	supCtx, cancel := context.WithCancel(ctx)
	p.cancelSup = cancel
	done := make(chan struct{})
	p.supDone = done
	p.mu.Unlock()

	go func() {
		defer close(done)
		defer func() {
			p.mu.Lock()
			p.cancelSup = nil
			p.mu.Unlock()
		}()
		p.supervise(supCtx)
	}()
	return nil
}

// Stop halts the supervisor and waits briefly for the child to die.
func (p *Prowlarr) Stop() {
	p.mu.Lock()
	cancel, done := p.cancelSup, p.supDone
	p.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		p.log.Warn("managed prowlarr did not stop in time")
	}
	p.setState(StateStopped, "")
}

// supervise is the restart loop. Two subtleties:
//   - Prowlarr's own updater replaces the app dir and exits the process;
//     the loop restarting it IS the upgrade completing, so an exit is not
//     automatically an error.
//   - if something is already answering on our port before a launch, a
//     previous child (or an updater-spawned relaunch) is still alive —
//     adopt it instead of racing it for the port.
func (p *Prowlarr) supervise(ctx context.Context) {
	backoff := 2 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if p.portAnswering(ctx) {
			p.setState(StateRunning, "")
			// Something (an updater relaunch) already serves the port. Watch
			// until it stops answering, then resume the launch loop.
			p.waitPortGone(ctx)
			continue
		}
		p.setState(StateStarting, "")
		cmd := exec.CommandContext(ctx, p.binPath(), "-nobrowser", "-data="+p.configDir())
		cmd.Dir = p.appDir()
		cmd.Stdout = nil
		cmd.Stderr = nil
		start := time.Now()
		if err := cmd.Start(); err != nil {
			p.setState(StateError, "launch: "+err.Error())
			p.log.Error("managed prowlarr launch failed", "err", err)
		} else {
			p.log.Info("managed prowlarr started", "pid", cmd.Process.Pid, "port", p.Status().URL)
			go p.markRunningWhenUp(ctx)
			err := cmd.Wait()
			if ctx.Err() != nil {
				return
			}
			p.log.Warn("managed prowlarr exited", "err", err, "after", time.Since(start).Round(time.Second))
		}
		// Healthy-for-a-while exits (updater restarts, crashes after hours)
		// reset the backoff; rapid-fire failures grow it.
		if time.Since(start) > time.Minute {
			backoff = 2 * time.Second
		} else if backoff < time.Minute {
			backoff *= 2
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

// markRunningWhenUp polls until the HTTP port answers, then flips the
// state to running.
func (p *Prowlarr) markRunningWhenUp(ctx context.Context) {
	for i := 0; i < 60; i++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		if p.portAnswering(ctx) {
			p.setState(StateRunning, "")
			return
		}
	}
}

func (p *Prowlarr) portAnswering(ctx context.Context) bool {
	target := p.ProxyTarget()
	if target == "" {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+target+"/prowlarr/ping", nil)
	if err != nil {
		return false
	}
	hc := &http.Client{Timeout: 3 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

func (p *Prowlarr) waitPortGone(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
		if !p.portAnswering(ctx) {
			return
		}
	}
}
