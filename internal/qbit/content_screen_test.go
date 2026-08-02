package qbit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/clienterr"
	"github.com/ordureconnoisseur/forager/internal/torrentmeta"
)

// bencodeMultiFile builds a multi-file .torrent with the given file names, so
// a test can state a release's contents in one line.
func bencodeMultiFile(name string, files ...string) []byte {
	var b strings.Builder
	b.WriteString("d4:infod5:filesl")
	for _, f := range files {
		fmt.Fprintf(&b, "d6:lengthi100e4:pathl%d:%see", len(f), f)
	}
	fmt.Fprintf(&b, "e4:name%d:%see", len(name), name)
	return []byte(b.String())
}

// screenRig serves a fixed .torrent body at /download and counts calls to
// qBit's add endpoint — the difference between "refused before spending the
// bandwidth" and "downloaded the junk anyway".
type screenRig struct {
	mu       sync.Mutex
	body     []byte
	addCalls int
}

func (s *screenRig) added() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addCalls
}

func (s *screenRig) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			io.WriteString(w, "Ok.")
		case "/api/v2/torrents/add":
			s.mu.Lock()
			s.addCalls++
			s.mu.Unlock()
			io.WriteString(w, "Ok.")
		case "/download":
			s.mu.Lock()
			b := s.body
			s.mu.Unlock()
			_, _ = w.Write(b)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// A release whose file list holds no video can only download in full, place
// nothing, fail confirmation and get culled hours later. It must be refused
// before qBit is ever told about it: the point of the check is that those
// bytes are never spent.
func TestAddTorrentRefusesReleaseWithNoVideo(t *testing.T) {
	rig := &screenRig{body: bencodeMultiFile("Totally.Legit.XXX.1080p", "Setup.exe", "Read Me.txt")}
	srv := rig.server()
	defer srv.Close()

	c := New(srv.URL, "", "")
	_, err := c.AddTorrent(context.Background(), srv.URL+"/download", "forage")
	if !errors.Is(err, torrentmeta.ErrNoVideo) {
		t.Fatalf("err = %v, want torrentmeta.ErrNoVideo in the chain", err)
	}
	if errors.Is(err, clienterr.ErrTransient) {
		t.Error("a refusal must not classify transient, or the deferred loop keeps retrying a release that can never work")
	}
	if errors.Is(err, ErrIndexerFetch) {
		t.Error("a refusal must not read as an indexer failure, or the failover switches to another copy of the same junk")
	}
	if n := rig.added(); n != 0 {
		t.Errorf("qbit add was called %d times; the release must never reach the client", n)
	}
}

// The counterpart, and the risk this change carries: an ordinary release
// carries junk alongside the video and the placer's allowlist drops it at
// placement. Refusing that here would cost the user the scene.
func TestAddTorrentAcceptsVideoWithJunk(t *testing.T) {
	rig := &screenRig{body: bencodeMultiFile("Real.Release.XXX.1080p", "scene.mp4", "Setup.exe", "info.nfo")}
	srv := rig.server()
	defer srv.Close()

	c := New(srv.URL, "", "")
	if _, err := c.AddTorrent(context.Background(), srv.URL+"/download", "forage"); err != nil {
		t.Fatalf("a release with a video in it must be added: %v", err)
	}
	if n := rig.added(); n != 1 {
		t.Errorf("qbit add called %d times, want 1", n)
	}
}

// A rar'd release's file list says "no video" and is wrong — the video is
// inside the archive. Refusing on the list alone would drop legitimate packed
// releases.
func TestAddTorrentAcceptsArchivedRelease(t *testing.T) {
	rig := &screenRig{body: bencodeMultiFile("Packed.Release", "rls.rar", "rls.r00", "rls.sfv")}
	srv := rig.server()
	defer srv.Close()

	c := New(srv.URL, "", "")
	if _, err := c.AddTorrent(context.Background(), srv.URL+"/download", "forage"); err != nil {
		t.Fatalf("a packed release must still be added: %v", err)
	}
	if n := rig.added(); n != 1 {
		t.Errorf("qbit add called %d times, want 1", n)
	}
}

// A magnet carries no file list until its metadata resolves, so there is
// nothing to screen and nothing may be refused. It must reach qBit untouched.
func TestAddTorrentMagnetIsNotScreened(t *testing.T) {
	rig := &screenRig{}
	srv := rig.server()
	defer srv.Close()

	c := New(srv.URL, "", "")
	magnet := "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=Some.Release"
	if _, err := c.AddTorrent(context.Background(), magnet, "forage"); err != nil {
		t.Fatalf("a magnet must never be refused for having no file list: %v", err)
	}
	if n := rig.added(); n != 1 {
		t.Errorf("qbit add called %d times, want 1", n)
	}
}
