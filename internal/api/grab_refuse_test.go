package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/ordureconnoisseur/forager/internal/clientpool"
	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/db"
	"github.com/ordureconnoisseur/forager/internal/grabs"
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

// screenServer is a Server wired to a fake qBit + SAB, so the add attempts can
// be driven SYNCHRONOUSLY (addTorrentAttempt / addUsenetAttempt, not the
// go-routine wrappers) and their effect on the grab row asserted without a
// race.
type screenServer struct {
	*Server
	mu       sync.Mutex
	body     []byte // what the fake indexer serves at /download
	addCalls int    // qBit adds
	sabAdds  int    // SAB addurl calls
}

func (s *screenServer) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addCalls, s.sabAdds
}

func newScreenServer(t *testing.T, body []byte) (*screenServer, string) {
	t.Helper()
	ss := &screenServer{body: body}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v2/auth/login":
			io.WriteString(w, "Ok.")
		case r.URL.Path == "/api/v2/torrents/add":
			ss.mu.Lock()
			ss.addCalls++
			ss.mu.Unlock()
			io.WriteString(w, "Ok.")
		case r.URL.Path == "/download":
			ss.mu.Lock()
			b := ss.body
			ss.mu.Unlock()
			_, _ = w.Write(b)
		case r.URL.Query().Get("mode") == "addurl":
			ss.mu.Lock()
			ss.sabAdds++
			ss.mu.Unlock()
			io.WriteString(w, `{"status":true,"nzo_ids":["SABnzbd_nzo_test"]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	dbh, err := db.Open(filepath.Join(t.TempDir(), "forager.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { dbh.Close() })
	pool := clientpool.New()
	pool.Reload(config.Config{QbitURL: srv.URL, SabURL: srv.URL, SabAPIKey: "k"})
	ss.Server = &Server{
		db:    dbh,
		pool:  pool,
		grabs: grabs.NewRepo(dbh),
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return ss, srv.URL
}

func (s *screenServer) queuedGrab(t *testing.T, client string) int64 {
	t.Helper()
	id, err := s.grabs.Insert(context.Background(), grabs.Grab{
		ReleaseTitle: "Some.Release.XXX.1080p", Client: client, Status: "queued",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	return id
}

// The grab layer's half of the pre-download screen: a refused release must
// settle as failed with a reason the user can read, and must not be deferred
// for retry, because no backoff or indexer failover can put a video into a release
// that has none.
func TestTorrentGrabRefusedBeforeDownloading(t *testing.T) {
	s, base := newScreenServer(t, bencodeMultiFile("Totally.Legit.XXX.1080p", "Setup.exe", "Read Me.txt"))
	id := s.queuedGrab(t, "qbit")

	s.addTorrentAttempt(base+"/download", "forage", "Totally.Legit.XXX.1080p", id, 1)

	g, err := s.grabs.Get(context.Background(), id)
	if err != nil || g == nil {
		t.Fatalf("get grab: %v", err)
	}
	if g.Status != "failed" {
		t.Fatalf("status = %q, want failed (reason=%q)", g.Status, g.Reason)
	}
	if !strings.HasPrefix(g.Reason, grabs.RefusedPrefix) {
		t.Errorf("reason = %q, want the refusal prefix so refusal reads differently from failure", g.Reason)
	}
	if g.NextRetryAt != 0 || g.FailKind != "" {
		t.Errorf("grab was parked for retry (next_retry_at=%d fail_kind=%q); a refusal is terminal",
			g.NextRetryAt, g.FailKind)
	}
	if adds, _ := s.counts(); adds != 0 {
		t.Errorf("qbit add called %d times; the release must never reach the client", adds)
	}
	// The bulk "retry all failed" must skip it too, or it re-downloads the
	// same junk on the next sweep.
	if !contentDeadReason(g.Reason) {
		t.Errorf("contentDeadReason(%q) = false; retry-all would re-drive a refused release", g.Reason)
	}
}

// A release with a video in it goes through untouched. This is the case a
// wrong screen would break, and it is the expensive one to get wrong.
func TestTorrentGrabWithVideoIsAdded(t *testing.T) {
	s, base := newScreenServer(t, bencodeMultiFile("Real.Release.XXX.1080p", "scene.mp4", "Setup.exe"))
	id := s.queuedGrab(t, "qbit")

	s.addTorrentAttempt(base+"/download", "forage", "Real.Release.XXX.1080p", id, 1)

	g, _ := s.grabs.Get(context.Background(), id)
	if g.Status != "queued" {
		t.Fatalf("status = %q, want it left queued for the poller (reason=%q)", g.Status, g.Reason)
	}
	if adds, _ := s.counts(); adds != 1 {
		t.Errorf("qbit add called %d times, want 1", adds)
	}
}

// Usenet is the other "unknown is not empty" case: forage never sees an NZB's
// file list (SAB fetches the NZB itself), so nothing on the usenet path may be
// refused for lack of video.
func TestUsenetGrabIsNeverScreened(t *testing.T) {
	s, base := newScreenServer(t, nil)
	id := s.queuedGrab(t, "sabnzbd")

	s.addUsenetAttempt(base+"/nzb?id=1", "forage", "Some.Release.XXX.1080p", id, 1)

	g, _ := s.grabs.Get(context.Background(), id)
	if g.Status != "queued" {
		t.Fatalf("status = %q, want queued: an NZB exposes no file list to judge (reason=%q)", g.Status, g.Reason)
	}
	if g.ClientID != "SABnzbd_nzo_test" {
		t.Errorf("client_id = %q, want the nzo_id: the add must have gone through", g.ClientID)
	}
	if _, sabAdds := s.counts(); sabAdds != 1 {
		t.Errorf("sab addurl called %d times, want 1", sabAdds)
	}
}

// uploadTorrent posts data to the manual .torrent endpoint.
func uploadTorrent(t *testing.T, s *Server, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("torrent", "upload.torrent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatal(err)
	}
	_ = mw.Close()
	req := httptest.NewRequest("POST", "/grab/torrent", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	s.postGrabTorrent(rec, req)
	return rec
}

// The hand-uploaded .torrent path parses the same metadata, so it screens the
// same way, and being a live request it can tell the user to their face
// instead of leaving a failed grab to find later.
func TestManualTorrentUploadRefusedWhenNoVideo(t *testing.T) {
	s, _ := newScreenServer(t, nil)

	rec := uploadTorrent(t, s.Server, bencodeMultiFile("Totally.Legit", "Setup.exe", "Read Me.txt"))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no video") {
		t.Errorf("body = %s, want it to say why the upload was refused", rec.Body.String())
	}
	if adds, _ := s.counts(); adds != 0 {
		t.Errorf("qbit add called %d times; a refused upload must not reach the client", adds)
	}
	all, err := s.grabs.List(context.Background(), "any", "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Errorf("a refused upload left %d grab rows behind", len(all))
	}
}

// And the upload that is fine still lands.
func TestManualTorrentUploadWithVideoIsAccepted(t *testing.T) {
	s, _ := newScreenServer(t, nil)

	rec := uploadTorrent(t, s.Server, bencodeMultiFile("Real.Release", "scene.mp4", "info.nfo"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if adds, _ := s.counts(); adds != 1 {
		t.Errorf("qbit add called %d times, want 1", adds)
	}
}
