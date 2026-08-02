package managed

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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

	// The digest is remote text, and the refusal it produces goes into
	// Status.Detail, out through GET /managed/prowlarr, and into the UI's
	// "Install failed: {status.detail}" line. So the error must not carry
	// however many bytes the release API felt like sending.
	t.Run("oversize digest is clipped out of the error", func(t *testing.T) {
		_, err := releaseAsset{Name: "Prowlarr.zip", Digest: strings.Repeat("A", 100000)}.wantSHA256()
		if err == nil {
			t.Fatal("a 100k digest was accepted")
		}
		if len(err.Error()) > 300 {
			t.Fatalf("error is %d bytes; remote digest is not being bounded", len(err.Error()))
		}
	})
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

// installableArchive builds a real archive in this platform's release
// format: the shape extract() expects, carrying a file at the path
// Installed() stats plus a sibling, so extracting it produces a genuinely
// "installed" tree.
//
// It has to be genuinely installable or the mismatch case in
// TestInstallVerifiesBeforeExtracting proves nothing. If these bytes were
// junk, extract() would refuse them on its own and the app dir would be
// absent whether or not the digest was ever checked, so the test would pass
// for a reason that has nothing to do with the ordering. The matching-digest
// case below is what holds this honest: it asserts these same bytes DO
// install, so the only thing stopping the mismatch case is the digest.
func installableArchive(t *testing.T) []byte {
	t.Helper()
	binName := "Prowlarr"
	if runtime.GOOS == "windows" {
		binName = "Prowlarr.exe"
	}
	entries := map[string]string{
		"Prowlarr/" + binName:        "pretend ELF/PE",
		"Prowlarr/Definitions/x.yml": "defs",
	}
	var buf bytes.Buffer
	if strings.HasSuffix(assetSuffix(), ".zip") {
		zw := zip.NewWriter(&buf)
		for name, content := range entries {
			w, err := zw.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(w, content); err != nil {
				t.Fatal(err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range entries {
		hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestInstallVerifiesBeforeExtracting drives Install() itself, against a
// local stand-in for the GitHub releases API, because the ordering is the
// whole point of the digest check and the ordering only exists inside
// Install: resolve, download-and-verify, and only then extract into the app
// dir the supervisor will exec out of.
//
// What each case would catch. The matching-digest case proves the archive is
// installable, without which the other two are vacuous. The mismatched case
// fails if anyone reorders Install to extract before checking download's
// error, or adds an "install it anyway" fallback: the served bytes extract
// perfectly well, so an app dir appearing here means unverified bytes were
// unpacked into the path binPath() points at. The absent-digest case fails
// if the refusal ever moves to after the fetch, since it counts asset
// requests and expects none.
//
// It does not cover: the real GitHub API's response shape (that was probed
// by hand, see the wantSHA256 comment), or the supervisor actually exec'ing
// what got extracted.
func TestInstallVerifiesBeforeExtracting(t *testing.T) {
	if !Supported() {
		t.Skip("managed mode unsupported on this platform")
	}
	archive := installableArchive(t)
	sum := sha256.Sum256(archive)
	realSum := hex.EncodeToString(sum[:])
	const tag = "v2.5.2.5491"
	assetName := "Prowlarr.master." + tag + "." + assetSuffix()

	tests := []struct {
		name          string
		digest        string
		wantErr       string
		wantInstalled bool
		wantFetches   int64
	}{
		{
			name:          "published digest matches, archive installs",
			digest:        "sha256:" + realSum,
			wantInstalled: true,
			wantFetches:   1,
		},
		{
			// The substitution case: bytes that extract fine but are not the
			// ones the release published.
			name:        "published digest is for other bytes",
			digest:      "sha256:" + strings.Repeat("11", 32),
			wantErr:     "failed sha256 verification",
			wantFetches: 1,
		},
		{
			name:        "release publishes no digest",
			digest:      "",
			wantErr:     "unverified",
			wantFetches: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var assetFetches atomic.Int64
			mux := http.NewServeMux()
			var srv *httptest.Server
			mux.HandleFunc("/asset", func(w http.ResponseWriter, r *http.Request) {
				assetFetches.Add(1)
				_, _ = w.Write(archive)
			})
			mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
				rel := release{Tag: tag, Assets: []releaseAsset{
					// A decoy that must not be picked, so the test also
					// exercises suffix matching rather than "first asset wins".
					{Name: "Prowlarr." + tag + ".changelog.txt", URL: srv.URL + "/asset", Digest: "sha256:" + realSum},
					{
						Name:   assetName,
						URL:    srv.URL + "/asset",
						Size:   int64(len(archive)),
						Digest: tc.digest,
					},
				}}
				_ = json.NewEncoder(w).Encode(rel)
			})
			srv = httptest.NewServer(mux)
			defer srv.Close()

			p := NewProwlarr(t.TempDir(), testLogger())
			p.releasesURL = srv.URL + "/releases/latest"

			err := p.Install(context.Background())
			if got := assetFetches.Load(); got != tc.wantFetches {
				t.Errorf("fetched the asset %d times, want %d", got, tc.wantFetches)
			}

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Install failed on a correctly digested archive: %v", err)
				}
				if !p.Installed() {
					t.Fatal("Install reported success but left no runnable binary")
				}
				if st := p.Status(); st.State != StateStopped || st.Version != tag {
					t.Errorf("state = %s version = %q, want stopped %s", st.State, st.Version, tag)
				}
				record, rerr := os.ReadFile(filepath.Join(p.root, "version.txt"))
				if rerr != nil || !strings.Contains(string(record), realSum) {
					t.Errorf("version.txt did not record the verified sha256: %v %q", rerr, record)
				}
				return
			}

			if err == nil {
				t.Fatal("Install succeeded on an archive it could not verify")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %v does not mention %q", err, tc.wantErr)
			}
			// This string is what Status.Detail shows the user; Install used
			// to wrap an error that already named the asset, so the name
			// landed in it twice.
			if n := strings.Count(err.Error(), assetName); n > 1 {
				t.Errorf("asset name appears %d times in the UI-facing error %q", n, err)
			}
			// The assertions that matter: nothing unverified reached the
			// directory the supervisor launches out of.
			if p.Installed() {
				t.Error("a failed verification left a runnable binary at binPath()")
			}
			for _, dir := range []string{p.appDir(), p.appDir() + ".new"} {
				if _, serr := os.Stat(dir); serr == nil {
					t.Errorf("%s exists despite verification failing", dir)
				}
			}
			if st := p.Status(); st.State != StateError || st.Installed {
				t.Errorf("state = %s installed = %v, want error/false", st.State, st.Installed)
			}
		})
	}
}
