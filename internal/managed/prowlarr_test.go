package managed

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeZip(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExtractZipStripsTopAndRefusesEscape(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "a.zip")
	writeZip(t, archive, map[string]string{
		"Prowlarr/Prowlarr.exe":      "binary",
		"Prowlarr/Definitions/x.yml": "defs",
		"Prowlarr/package_info":      "info",
	})
	dest := filepath.Join(dir, "app")
	if err := extract(archive, "a.zip", dest); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Prowlarr.exe", filepath.Join("Definitions", "x.yml")} {
		if _, err := os.Stat(filepath.Join(dest, want)); err != nil {
			t.Errorf("missing %s after extract: %v", want, err)
		}
	}

	evil := filepath.Join(dir, "evil.zip")
	writeZip(t, evil, map[string]string{"Prowlarr/../../outside.txt": "boom"})
	if err := extract(evil, "evil.zip", filepath.Join(dir, "app2")); err == nil {
		t.Fatal("zip-slip entry extracted without error")
	}
	if _, err := os.Stat(filepath.Join(dir, "outside.txt")); err == nil {
		t.Fatal("zip-slip escaped the install dir")
	}
}

func TestExtractTarGzRefusesLinks(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "Prowlarr/ok.txt", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("ok"))
	_ = tw.WriteHeader(&tar.Header{Name: "Prowlarr/evil", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink})
	_ = tw.Close()
	_ = gz.Close()
	archive := filepath.Join(dir, "a.tar.gz")
	if err := os.WriteFile(archive, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	err := extract(archive, "a.tar.gz", filepath.Join(dir, "app"))
	if err == nil || !strings.Contains(err.Error(), "link") {
		t.Fatalf("link entry not refused: %v", err)
	}
}

func TestSeedConfigRoundtrip(t *testing.T) {
	dir := t.TempDir()
	p := NewProwlarr(dir, testLogger())
	if err := p.seedConfig(); err != nil {
		t.Fatal(err)
	}
	if p.APIKey() == "" || len(p.APIKey()) != 32 {
		t.Errorf("api key = %q, want 32 hex chars", p.APIKey())
	}
	if !strings.HasPrefix(p.URL(), "http://127.0.0.1:") || !strings.HasSuffix(p.URL(), "/prowlarr") {
		t.Errorf("url = %q", p.URL())
	}
	raw, err := os.ReadFile(filepath.Join(p.configDir(), "config.xml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<BindAddress>127.0.0.1</BindAddress>",
		"<UrlBase>/prowlarr</UrlBase>",
		"<AuthenticationMethod>External</AuthenticationMethod>",
		"<ApiKey>" + p.APIKey() + "</ApiKey>",
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("config.xml missing %s:\n%s", want, raw)
		}
	}

	// Seeding again must keep the existing config (reinstall path), and a
	// fresh manager must read the same values back.
	key := p.APIKey()
	if err := p.seedConfig(); err != nil {
		t.Fatal(err)
	}
	if p.APIKey() != key {
		t.Error("re-seed regenerated the api key")
	}
	p2 := NewProwlarr(dir, testLogger())
	if err := p2.loadSeededConfig(); err != nil {
		t.Fatal(err)
	}
	if p2.APIKey() != key || p2.URL() != p.URL() {
		t.Errorf("fresh manager read %q %q, want %q %q", p2.APIKey(), p2.URL(), key, p.URL())
	}
}

// TestSuperviseAdoptsExistingListener: when something already answers on
// the managed port (an updater relaunch), the supervisor must adopt it —
// state becomes running, and no second process is spawned (the fake
// "binary" here isn't executable, so any launch attempt would error the
// state machine).
func TestSuperviseAdoptsExistingListener(t *testing.T) {
	dir := t.TempDir()
	p := NewProwlarr(dir, testLogger())

	// A live listener standing in for a running Prowlarr.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() { _ = http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})) }()
	port := l.Addr().(*net.TCPAddr).Port

	// Seed a config pointing at that port and a placeholder binary so
	// Installed() passes.
	if err := os.MkdirAll(p.configDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("<Config><BindAddress>127.0.0.1</BindAddress><Port>%d</Port><UrlBase>/prowlarr</UrlBase><ApiKey>k</ApiKey></Config>", port)
	if err := os.WriteFile(filepath.Join(p.configDir(), "config.xml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.appDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.binPath(), []byte("not a real binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.loadSeededConfig(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if p.Status().State == StateRunning {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got := p.Status().State; got != StateRunning {
		t.Fatalf("state = %s, want running (adopted listener)", got)
	}
	p.Stop()
	if got := p.Status().State; got != StateStopped {
		t.Fatalf("state after stop = %s", got)
	}
}

func TestStartWithoutInstallFails(t *testing.T) {
	p := NewProwlarr(t.TempDir(), testLogger())
	if err := p.Start(context.Background()); err == nil {
		t.Fatal("Start succeeded with nothing installed")
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
