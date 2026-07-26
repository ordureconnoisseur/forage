package qbit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// categoryRig records the category calls a client makes, so a test can assert
// both WHICH endpoint was used and what save path was sent.
type categoryRig struct {
	mu           sync.Mutex
	calls        []string          // endpoint paths, in order
	form         map[string]string // last form values seen
	createStatus int               // status createCategory answers with
	editStatus   int
}

func (rig *categoryRig) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/auth/login" {
			_, _ = w.Write([]byte("Ok."))
			return
		}
		_ = r.ParseForm()
		rig.mu.Lock()
		rig.calls = append(rig.calls, r.URL.Path)
		rig.form = map[string]string{}
		for k, v := range r.PostForm {
			if len(v) > 0 {
				rig.form[k] = v[0]
			}
		}
		status := rig.createStatus
		if r.URL.Path == "/api/v2/torrents/editCategory" {
			status = rig.editStatus
		}
		rig.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (rig *categoryRig) seen() ([]string, map[string]string) {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	return append([]string{}, rig.calls...), rig.form
}

// TestEnsureCategoryCreates is the happy path this exists for: forage makes
// the category itself, pointing at the download folder, so setup never has to
// send the user into qBittorrent to do it by hand.
func TestEnsureCategoryCreates(t *testing.T) {
	rig := &categoryRig{}
	srv := rig.server(t)
	c := New(srv.URL, "user", "pass")

	if err := c.EnsureCategory(context.Background(), "forage", "/data/media/downloads/complete"); err != nil {
		t.Fatalf("EnsureCategory: %v", err)
	}
	calls, form := rig.seen()
	if len(calls) == 0 || calls[len(calls)-1] != "/api/v2/torrents/createCategory" {
		t.Fatalf("calls = %v, want createCategory", calls)
	}
	if form["category"] != "forage" {
		t.Errorf("category = %q, want forage", form["category"])
	}
	if form["savePath"] != "/data/media/downloads/complete" {
		t.Errorf("savePath = %q, want the download folder", form["savePath"])
	}
}

// TestEnsureCategoryRepointsExisting: qBit answers 409 when the category is
// already there. That must not be an error — and it must not be left alone
// either. A category that exists but points somewhere ELSE is the subtler
// version of the same misconfiguration, and silently accepting it is how
// downloads land outside the filesystem forage hardlinks from.
func TestEnsureCategoryRepointsExisting(t *testing.T) {
	rig := &categoryRig{createStatus: http.StatusConflict}
	srv := rig.server(t)
	c := New(srv.URL, "user", "pass")

	if err := c.EnsureCategory(context.Background(), "forage", "/new/path"); err != nil {
		t.Fatalf("an existing category must not be an error: %v", err)
	}
	calls, form := rig.seen()
	if len(calls) < 2 || calls[len(calls)-1] != "/api/v2/torrents/editCategory" {
		t.Fatalf("calls = %v, want createCategory then editCategory", calls)
	}
	if form["savePath"] != "/new/path" {
		t.Errorf("savePath = %q — the existing category must be repointed", form["savePath"])
	}
}

// TestEnsureCategoryReportsRealFailure: when BOTH calls fail the cause is
// something else (unreachable, bad credentials, an older qBit without these
// endpoints), and that has to surface rather than being swallowed by the
// already-exists fallback.
func TestEnsureCategoryReportsRealFailure(t *testing.T) {
	rig := &categoryRig{createStatus: http.StatusForbidden, editStatus: http.StatusForbidden}
	srv := rig.server(t)
	c := New(srv.URL, "user", "pass")

	if err := c.EnsureCategory(context.Background(), "forage", "/path"); err == nil {
		t.Fatal("expected an error when both create and edit fail")
	}
}

// TestEnsureCategoryRejectsEmptyName guards the degenerate call rather than
// asking qBit to create a nameless category.
func TestEnsureCategoryRejectsEmptyName(t *testing.T) {
	rig := &categoryRig{}
	srv := rig.server(t)
	c := New(srv.URL, "user", "pass")

	if err := c.EnsureCategory(context.Background(), "", "/path"); err == nil {
		t.Fatal("expected an error for an empty category name")
	}
	if calls, _ := rig.seen(); len(calls) != 0 {
		t.Errorf("made %v; an empty name must not reach qBit", calls)
	}
}
