package managed

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Release resolution + download + extraction for the managed Prowlarr
// install. Everything comes from Prowlarr's official GitHub releases over
// HTTPS; the downloaded bytes are SHA256-summed and the digest recorded
// beside the install so "what exactly did forage download" always has an
// answer.

const prowlarrReleasesAPI = "https://api.github.com/repos/Prowlarr/Prowlarr/releases/latest"

// assetSuffix maps GOOS/GOARCH to the official portable asset name suffix.
// Empty means the platform has no official build → managed mode unsupported.
func assetSuffix() string {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "windows/amd64":
		return "windows-core-x64.zip"
	case "windows/arm64":
		return "windows-core-arm64.zip"
	case "linux/amd64":
		return "linux-core-x64.tar.gz"
	case "linux/arm64":
		return "linux-core-arm64.tar.gz"
	case "darwin/amd64":
		return "osx-core-x64.tar.gz"
	case "darwin/arm64":
		return "osx-core-arm64.tar.gz"
	}
	return ""
}

type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

type release struct {
	Tag    string         `json:"tag_name"`
	Assets []releaseAsset `json:"assets"`
}

// resolveLatest asks GitHub for the newest Prowlarr release and picks this
// platform's portable asset.
func resolveLatest(ctx context.Context, hc *http.Client) (tag string, asset releaseAsset, err error) {
	suffix := assetSuffix()
	if suffix == "" {
		return "", releaseAsset{}, fmt.Errorf("no official Prowlarr build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", prowlarrReleasesAPI, nil)
	if err != nil {
		return "", releaseAsset{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", releaseAsset{}, fmt.Errorf("query releases: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", releaseAsset{}, fmt.Errorf("query releases: %d: %s", resp.StatusCode, body)
	}
	var rel release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", releaseAsset{}, fmt.Errorf("decode release: %w", err)
	}
	for _, a := range rel.Assets {
		if strings.HasSuffix(a.Name, suffix) {
			return rel.Tag, a, nil
		}
	}
	return "", releaseAsset{}, fmt.Errorf("release %s has no %s asset", rel.Tag, suffix)
}

// download streams the asset to destPath, reporting progress via onBytes
// (done, total) and returning the SHA256 of everything written.
func download(ctx context.Context, hc *http.Client, asset releaseAsset, destPath string, onBytes func(done, total int64)) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", asset.URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(destPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	var done int64
	buf := make([]byte, 256*1024)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return "", werr
			}
			_, _ = h.Write(buf[:n])
			done += int64(n)
			if onBytes != nil {
				onBytes(done, asset.Size)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", fmt.Errorf("download read: %w", rerr)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), f.Close()
}

// extract unpacks the archive into destDir, stripping the top-level
// "Prowlarr" directory the archives carry so destDir IS the app dir.
// Rejects entries that would escape destDir (zip-slip). assetName decides
// the format — the archive sits on disk under a temp name, so the PATH
// suffix says nothing (dispatching on it sent Windows' .zip through the
// gzip reader on the first live install).
func extract(archivePath, assetName, destDir string) error {
	if strings.HasSuffix(assetName, ".zip") {
		return extractZip(archivePath, destDir)
	}
	return extractTarGz(archivePath, destDir)
}

// stripTop removes the leading "Prowlarr/" path segment. Entries outside
// it (there are none in practice) keep their name.
func stripTop(name string) string {
	name = strings.TrimPrefix(filepath.ToSlash(name), "./")
	if rest, ok := strings.CutPrefix(name, "Prowlarr/"); ok {
		return rest
	}
	return name
}

// securePath joins name under destDir, refusing traversal outside it.
func securePath(destDir, name string) (string, error) {
	p := filepath.Join(destDir, filepath.FromSlash(name))
	if rel, err := filepath.Rel(destDir, p); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry escapes install dir: %q", name)
	}
	return p, nil
}

func extractZip(archivePath, destDir string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		name := stripTop(f.Name)
		if name == "" {
			continue
		}
		p, err := securePath(destDir, name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode().Perm()|0o200)
		if err != nil {
			rc.Close()
			return err
		}
		_, cerr := io.Copy(out, rc)
		rc.Close()
		if err := out.Close(); cerr == nil {
			cerr = err
		}
		if cerr != nil {
			return cerr
		}
	}
	return nil
}

func extractTarGz(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := stripTop(hdr.Name)
		if name == "" {
			continue
		}
		p, perr := securePath(destDir, name)
		if perr != nil {
			return perr
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode).Perm()|0o200)
			if err != nil {
				return err
			}
			_, cerr := io.Copy(out, tr)
			if err := out.Close(); cerr == nil {
				cerr = err
			}
			if cerr != nil {
				return cerr
			}
		case tar.TypeSymlink, tar.TypeLink:
			// The Prowlarr archives don't carry links; refusing beats
			// following one somewhere surprising.
			return fmt.Errorf("archive contains link entry %q — refusing", hdr.Name)
		}
	}
}

// httpClientForDownloads: generous timeout for a couple-hundred-MB fetch,
// but bounded — a wedged CDN connection must not hang the installer state
// machine forever.
func httpClientForDownloads() *http.Client {
	return &http.Client{Timeout: 15 * time.Minute}
}
