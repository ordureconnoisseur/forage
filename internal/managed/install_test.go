package managed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// TestWantSHA256 covers the parse of GitHub's per-asset digest field. What
// breaks in the real world if these fail: a shape we do not understand gets
// treated as a checksum (or as "no checksum needed"), and forage extracts
// and executes an archive nobody checked.
func TestWantSHA256(t *testing.T) {
	const good = "c6233cd942aad3c382c2660ad0004f942a3cd54c4fb8b805e14d2cd1d6b2b264"
	tests := []struct {
		name   string
		digest string
		want   string
		wantOK bool
	}{
		// The live shape: v2.5.2.5491's osx-app-core-arm64.zip, whose
		// digest matched a real download of the asset byte for byte.
		{"published sha256", "sha256:" + good, good, true},
		// GitHub reports lowercase hex; accept uppercase rather than fail an
		// install over a cosmetic difference, but normalise it so the
		// comparison against our own hex encoding cannot miss.
		{"uppercase hex", "sha256:" + strings.ToUpper(good), good, true},
		// Older assets predate the digest field. Fail closed: no digest
		// means no install, never an unverified install.
		{"absent", "", "", false},
		{"unknown algorithm", "sha512:" + good + good, "", false},
		{"bare hex without prefix", good, "", false},
		{"truncated", "sha256:" + good[:32], "", false},
		{"non-hex body", "sha256:" + strings.Repeat("z", 64), "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := releaseAsset{Name: "Prowlarr.zip", Digest: tc.digest}.wantSHA256()
			if tc.wantOK != (err == nil) {
				t.Fatalf("wantSHA256(%q) err = %v, wantOK = %v", tc.digest, err, tc.wantOK)
			}
			if got != tc.want {
				t.Fatalf("wantSHA256(%q) = %q, want %q", tc.digest, got, tc.want)
			}
		})
	}
}

// TestPickAssetRejectsUndigestedAsset: the platform's asset is present but
// carries no digest. Refusing at resolve time is what keeps a digest-less
// release from costing a couple hundred megabytes of download before the
// install aborts anyway.
func TestPickAssetRejectsUndigestedAsset(t *testing.T) {
	rel := release{
		Tag: "v2.5.2.5491",
		Assets: []releaseAsset{
			{Name: "Prowlarr.master.2.5.2.5491.linux-core-x64.tar.gz", URL: "https://example.invalid/a"},
		},
	}
	if _, err := pickAsset(rel, "linux-core-x64.tar.gz"); err == nil {
		t.Fatal("pickAsset accepted an asset with no published digest")
	} else if !strings.Contains(err.Error(), "unverified") {
		t.Fatalf("error does not say why the install was refused: %v", err)
	}

	rel.Assets[0].Digest = "sha256:" + strings.Repeat("ab", 32)
	got, err := pickAsset(rel, "linux-core-x64.tar.gz")
	if err != nil {
		t.Fatalf("pickAsset rejected a properly digested asset: %v", err)
	}
	if got.Name != rel.Assets[0].Name {
		t.Fatalf("picked %q", got.Name)
	}
}

// TestDownloadVerifiesPublishedDigest is the one that matters: a served
// body that is not what the release published must abort, and must abort
// as an error rather than as a file the caller goes on to extract and exec.
func TestDownloadVerifiesPublishedDigest(t *testing.T) {
	body := []byte("pretend this is a Prowlarr release archive")
	sum := sha256.Sum256(body)
	realSum := hex.EncodeToString(sum[:])

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	tests := []struct {
		name       string
		digest     string
		wantErr    string
		wantFetch  bool // did we even go to the network?
		wantSumOut string
	}{
		{
			name:       "matching digest installs",
			digest:     "sha256:" + realSum,
			wantFetch:  true,
			wantSumOut: realSum,
		},
		{
			// A substituted or corrupted artifact. The published digest is
			// for other bytes, so this must not come back as a usable path.
			name:      "mismatched digest aborts",
			digest:    "sha256:" + strings.Repeat("00", 32),
			wantErr:   "failed sha256 verification",
			wantFetch: true,
		},
		{
			// No digest to check against is not permission to skip the
			// check, and it should cost no bandwidth.
			name:      "absent digest aborts before fetching",
			digest:    "",
			wantErr:   "unverified",
			wantFetch: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := hits.Load()
			dest := filepath.Join(t.TempDir(), "download.tmp")
			asset := releaseAsset{
				Name:   "Prowlarr.test.linux-core-x64.tar.gz",
				URL:    srv.URL + "/asset",
				Size:   int64(len(body)),
				Digest: tc.digest,
			}
			got, err := download(context.Background(), srv.Client(), asset, dest, nil)
			fetched := hits.Load() > before
			if fetched != tc.wantFetch {
				t.Errorf("fetched = %v, want %v", fetched, tc.wantFetch)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("download failed: %v", err)
				}
				if got != tc.wantSumOut {
					t.Fatalf("returned sum %q, want %q", got, tc.wantSumOut)
				}
				on, rerr := os.ReadFile(dest)
				if rerr != nil || string(on) != string(body) {
					t.Fatalf("verified download did not land on disk: %v", rerr)
				}
				return
			}
			if err == nil {
				t.Fatal("download returned no error for an unverifiable asset")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %v does not mention %q", err, tc.wantErr)
			}
			if got != "" {
				t.Fatalf("returned a checksum %q alongside a verification failure", got)
			}
		})
	}
}

// TestInstallAbortsBeforeExtractOnDigestMismatch walks the caller: a bad
// download must leave nothing installed. If this regresses, forage unpacks
// and then supervises (exec's) an archive that failed its checksum.
func TestInstallAbortsBeforeExtractOnDigestMismatch(t *testing.T) {
	if !Supported() {
		t.Skip("managed mode unsupported on this platform")
	}
	body := []byte("not the published archive")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	p := NewProwlarr(dir, testLogger())
	if err := os.MkdirAll(p.root, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(p.root, "download.tmp")
	asset := releaseAsset{
		Name:   "Prowlarr.test." + assetSuffix(),
		URL:    srv.URL + "/asset",
		Size:   int64(len(body)),
		Digest: "sha256:" + strings.Repeat("11", 32),
	}
	if _, err := download(context.Background(), srv.Client(), asset, archive, nil); err == nil {
		t.Fatal("download accepted bytes that do not match the published digest")
	}
	if p.Installed() {
		t.Fatal("a failed verification left a runnable binary in place")
	}
	if _, err := os.Stat(p.appDir()); err == nil {
		t.Fatal("app dir was created despite verification failing")
	}
}
