package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

// linkServer stands in for a secondary stash-box answering the aliased
// findPerformer batch: every alias resolves to the performer whose urls are
// given in `urls`, keyed by the id passed as that alias's variable.
func linkServer(t *testing.T, urls map[string][]string, calls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			*calls++
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		out := map[string]any{}
		for alias, v := range req.Variables {
			id, _ := v.(string)
			list := []map[string]string{}
			for _, u := range urls[id] {
				list = append(list, map[string]string{"url": u})
			}
			out[alias] = map[string]any{"id": id, "urls": list}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": out})
	}))
}

func seedIdentity(s *Server, endpoint string, id *boxIdentity) {
	id.at = time.Now()
	if id.links == nil {
		id.links = map[string]string{}
	}
	s.boxIdentity.by = map[string]*boxIdentity{endpoint: id}
}

const fansDB = "https://fansdb.cc/graphql"

func boxScene(performerIDs ...string) stashdb.Scene {
	sc := stashdb.Scene{ID: "s1"}
	for _, id := range performerIDs {
		sc.Performers = append(sc.Performers, stashdb.ScenePerformer{ID: id, Name: id})
	}
	return sc
}

// Route 1: the performer is identified against this box locally. Exact, and
// it must not cost a lookup on the box.
func TestPerformersOwnedOnBoxExactMatch(t *testing.T) {
	calls := 0
	srv := linkServer(t, nil, &calls)
	defer srv.Close()

	s := newDeferTestServer(t)
	seedIdentity(s, fansDB, &boxIdentity{onBox: map[string]string{"box-1": "local-9"}})
	e := &boxEntry{box: discoverBox{Endpoint: fansDB}, client: stashdb.NewUnpaced(srv.URL, "k")}

	got := s.performersOwnedOnBox(t.Context(), e, []stashdb.Scene{boxScene("box-1")})
	if got["box-1"] != "local-9" {
		t.Fatalf("got %v, want box-1 → local-9", got)
	}
	if calls != 0 {
		t.Errorf("asked the box %d times for a performer already resolved locally", calls)
	}
}

// Route 2: the box's profile links to StashDB and the library holds that
// StashDB id. This is the route that matters, because almost nobody
// identifies performers against a secondary box.
func TestPerformersOwnedOnBoxResolvesViaStashDBURL(t *testing.T) {
	const sdbID = "41bfc3e7-efb8-496d-bc79-582943fada8d"
	srv := linkServer(t, map[string][]string{
		"box-1": {"https://onlyfans.com/someone", "https://stashdb.org/performers/" + sdbID},
		"box-2": {"https://twitter.com/someone"},
	}, nil)
	defer srv.Close()

	s := newDeferTestServer(t)
	seedIdentity(s, fansDB, &boxIdentity{
		onStashDB: map[string]string{sdbID: "local-4"},
	})
	e := &boxEntry{box: discoverBox{Endpoint: fansDB}, client: stashdb.NewUnpaced(srv.URL, "k")}

	got := s.performersOwnedOnBox(t.Context(), e, []stashdb.Scene{boxScene("box-1", "box-2")})
	if got["box-1"] != "local-4" {
		t.Errorf("box-1 = %q, want local-4 via the StashDB link", got["box-1"])
	}
	if _, ok := got["box-2"]; ok {
		t.Errorf("box-2 links to nothing on StashDB and must stay addable, got %q", got["box-2"])
	}
}

// A performer who links to a StashDB id the library does NOT have stays
// addable. Linking is not owning.
func TestPerformersOwnedOnBoxUnownedStashDBLink(t *testing.T) {
	const sdbID = "41bfc3e7-efb8-496d-bc79-582943fada8d"
	srv := linkServer(t, map[string][]string{
		"box-1": {"https://stashdb.org/performers/" + sdbID},
	}, nil)
	defer srv.Close()

	s := newDeferTestServer(t)
	seedIdentity(s, fansDB, &boxIdentity{onStashDB: map[string]string{"some-other-id": "local-4"}})
	e := &boxEntry{box: discoverBox{Endpoint: fansDB}, client: stashdb.NewUnpaced(srv.URL, "k")}

	if got := s.performersOwnedOnBox(t.Context(), e, []stashdb.Scene{boxScene("box-1")}); len(got) != 0 {
		t.Fatalf("got %v, want nothing resolved", got)
	}
}

// Links are memoised, including the negative answer. Re-browsing must not
// re-ask the box about performers it has already answered for, or every page
// view pays for the whole cast again.
func TestPerformersOwnedOnBoxMemoisesLinks(t *testing.T) {
	calls := 0
	srv := linkServer(t, map[string][]string{"box-1": {"https://x.com/nobody"}}, &calls)
	defer srv.Close()

	s := newDeferTestServer(t)
	seedIdentity(s, fansDB, &boxIdentity{})
	e := &boxEntry{box: discoverBox{Endpoint: fansDB}, client: stashdb.NewUnpaced(srv.URL, "k")}

	scenes := []stashdb.Scene{boxScene("box-1")}
	s.performersOwnedOnBox(t.Context(), e, scenes)
	if calls != 1 {
		t.Fatalf("first pass made %d requests, want 1", calls)
	}
	s.performersOwnedOnBox(t.Context(), e, scenes)
	if calls != 1 {
		t.Errorf("second pass made %d requests total, want the memo to answer", calls)
	}
}

// A box that fails the link lookup must not fail the page: everyone stays
// addable, which is the harmless direction.
func TestPerformersOwnedOnBoxSurvivesBoxFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := newDeferTestServer(t)
	seedIdentity(s, fansDB, &boxIdentity{onBox: map[string]string{"box-9": "local-1"}})
	e := &boxEntry{box: discoverBox{Endpoint: fansDB}, client: stashdb.NewUnpaced(srv.URL, "k")}

	got := s.performersOwnedOnBox(t.Context(), e,
		[]stashdb.Scene{boxScene("box-1", "box-9")})
	if got["box-9"] != "local-1" {
		t.Errorf("the exact match must survive a box outage, got %v", got)
	}
	if _, ok := got["box-1"]; ok {
		t.Errorf("an unresolvable performer must not be claimed, got %v", got)
	}
}

// The memo is bounded. Browsing deep into a large box would otherwise keep an
// entry per performer ever seen for the life of the daemon.
func TestPerformersOwnedOnBoxMemoIsBounded(t *testing.T) {
	srv := linkServer(t, nil, nil)
	defer srv.Close()

	s := newDeferTestServer(t)
	full := make(map[string]string, boxLinkMemoCap)
	for i := 0; i < boxLinkMemoCap; i++ {
		full["old-"+strings.Repeat("x", i%5)+string(rune(i))] = ""
	}
	seedIdentity(s, fansDB, &boxIdentity{links: full})
	before := len(full)
	e := &boxEntry{box: discoverBox{Endpoint: fansDB}, client: stashdb.NewUnpaced(srv.URL, "k")}

	s.performersOwnedOnBox(t.Context(), e, []stashdb.Scene{boxScene("brand-new")})
	after := len(s.boxIdentity.by[fansDB].links)
	if after >= before {
		t.Errorf("memo grew from %d to %d, want it dropped at the cap", before, after)
	}
}
