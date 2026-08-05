package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"sync"
	"time"
)

// Caching the thumbnails Discover shows.
//
// Discover mounts ~390 cards, each pointing straight at stashdb.org. Measured
// on a live page: every request returns 200 and nothing is broken, but the
// browser fetches a handful at a time from one host and six screens of
// scrolling took about fifteen seconds to decode 120 of them. Nothing is
// failing; it is simply slow, and slow every single visit, because the browser
// re-fetches from a host that is not close and not fast.
//
// The images never change — a StashDB image id addresses fixed bytes — so the
// first fetch is the only one that ever needs to leave the machine.
//
// The security shape is the part worth reading. A handler that fetches "the
// URL in this query parameter" is an open relay: anyone who can reach forage
// can make forage fetch anything reachable FROM forage, which on this
// deployment means the whole home network, the Docker host and any cloud
// metadata endpoint. So this does not take a URL at all. It takes an image ID
// and rebuilds the URL against a fixed host, which makes the dangerous shape
// unrepresentable rather than merely rejected.

const (
	// imageCacheHost is the only origin this will ever fetch from.
	imageCacheHost = "https://stashdb.org/images/"
	// imageMaxBytes caps one image. StashDB covers are tens to hundreds of
	// KB; a megabyte is generous and stops a bad response filling the disk.
	imageMaxBytes = 8 << 20
	// imageCacheTTL is how long a browser may reuse its own copy. The bytes
	// behind an id never change, so this is only about revisiting.
	imageCacheTTL = 30 * 24 * time.Hour
)

// stashdbImageID matches the id shape StashDB uses (a UUID). Anything else is
// refused before it can reach the network.
func validImageID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, r := range id {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// imageCache serves StashDB images from disk, fetching each one once.
type imageCache struct {
	dir    string
	client *http.Client
	// inflight collapses concurrent misses for the same id: a fresh Discover
	// page asks for dozens at once and a reload asks for the same dozens
	// again, and fetching each twice would be the opposite of the point.
	inflight sync.Map
}

func newImageCache(dir string) *imageCache {
	return &imageCache{
		dir: dir,
		client: &http.Client{
			Timeout: 30 * time.Second,
			// A redirect is how a fixed host turns back into an arbitrary
			// one. StashDB serves these directly; anything trying to send us
			// elsewhere is refused.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("redirect refused: this cache only fetches from its fixed host")
			},
		},
	}
}

func (c *imageCache) path(id string) string {
	// Hash rather than the raw id: the id is already validated, but a name
	// derived by hashing cannot contain a separator whatever the input.
	sum := sha256.Sum256([]byte(id))
	h := hex.EncodeToString(sum[:])
	// One level of fan-out so a directory does not grow to tens of thousands
	// of entries, which some filesystems handle badly.
	return filepath.Join(c.dir, h[:2], h)
}

// get returns the cached bytes, fetching them on a miss.
func (c *imageCache) get(id string) ([]byte, error) {
	p := c.path(id)
	if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
		return b, nil
	}
	// Collapse concurrent misses for this id.
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	actual, loaded := c.inflight.LoadOrStore(id, ch)
	if loaded {
		r := <-actual.(chan result)
		// Put it back for any other waiter.
		actual.(chan result) <- r
		return r.b, r.err
	}
	defer c.inflight.Delete(id)

	b, err := c.fetch(id)
	if err == nil {
		if mkErr := os.MkdirAll(filepath.Dir(p), 0o755); mkErr == nil {
			// Write to a temp sibling and rename, so a crash mid-write cannot
			// leave a truncated image that every later request then serves.
			tmp := p + ".part"
			if os.WriteFile(tmp, b, 0o644) == nil {
				_ = os.Rename(tmp, p)
			}
		}
	}
	r := result{b, err}
	ch <- r
	return r.b, r.err
}

func (c *imageCache) fetch(id string) ([]byte, error) {
	resp, err := c.client.Get(imageCacheHost + url.PathEscape(id))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("upstream: " + resp.Status)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "image/") {
		return nil, errors.New("upstream did not return an image")
	}
	return io.ReadAll(io.LimitReader(resp.Body, imageMaxBytes))
}

// getImage serves GET /img/stashdb/{id}.
//
//	/img/stashdb/79531ea8-c4f5-4ce4-9305-1be7ae783d8f
//
// An ID, never a URL: see the package note above on why that distinction is
// the whole security argument.
func (s *Server) getImage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !validImageID(id) {
		http.Error(w, "not an image id", http.StatusBadRequest)
		return
	}
	if s.images == nil {
		http.Error(w, "image cache unavailable", http.StatusServiceUnavailable)
		return
	}
	b, err := s.images.get(id)
	if err != nil {
		s.log.Warn("image cache miss could not be filled", "id", id, "err", err)
		http.Error(w, "image unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age="+
		strconv.FormatInt(int64(imageCacheTTL.Seconds()), 10)+", immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}
