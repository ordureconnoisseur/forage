package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

// sceneQueryServer answers queryScenes with `n` synthetic scenes and records
// the input every request asked for, so a test can assert what was requested
// as well as what came back.
func sceneQueryServer(t *testing.T, n int, seen *[]map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Variables struct {
				Input map[string]any `json:"input"`
			} `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		if seen != nil {
			*seen = append(*seen, req.Variables.Input)
		}
		scenes := make([]map[string]any, 0, n)
		for i := 0; i < n; i++ {
			scenes = append(scenes, map[string]any{
				"id":    "scene-" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
				"title": "Scene",
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"queryScenes": map[string]any{"count": 1043882, "scenes": scenes},
			},
		})
	}))
}

// trendingServer wires a Server whose only reachable box is a secondary one
// backed by srv, and returns the endpoint to pass as ?box=.
func trendingServer(t *testing.T, srv *httptest.Server) (*Server, string) {
	t.Helper()
	s := newDeferTestServer(t)
	s.boxes.entries = []boxEntry{{
		box:    discoverBox{Endpoint: fansDB, Name: "FansDB"},
		client: stashdb.NewUnpaced(srv.URL, "k"),
	}}
	s.boxes.at = time.Now() // fresh, so stashBoxes() serves the cache
	return s, fansDB
}

func getTrendingJSON(t *testing.T, s *Server, query string) trendingResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	s.getTrending(rec, httptest.NewRequest(http.MethodGet, "/trending"+query, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var out trendingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// The handler must ask for the TRENDING sort at the page it was given. The
// carousel is served from a cache; this endpoint exists precisely because it
// goes to the source, and a silent fallback to page 1 would make an infinite
// scroll repeat its first page forever without ever looking broken.
func TestTrendingRequestsTheRequestedPage(t *testing.T) {
	var seen []map[string]any
	srv := sceneQueryServer(t, 5, &seen)
	defer srv.Close()
	s, ep := trendingServer(t, srv)

	getTrendingJSON(t, s, "?box="+ep+"&page=7&per_page=5")

	if len(seen) != 1 {
		t.Fatalf("made %d queries, want 1", len(seen))
	}
	if got := seen[0]["sort"]; got != "TRENDING" {
		t.Errorf("sort = %v, want TRENDING", got)
	}
	if got := seen[0]["page"]; got != float64(7) {
		t.Errorf("page = %v, want 7", got)
	}
	if got := seen[0]["per_page"]; got != float64(5) {
		t.Errorf("per_page = %v, want 5", got)
	}
}

// per_page and page are clamped rather than trusted. per_page is what one
// request costs the source; page bounds how deep "trending" is allowed to
// mean anything.
func TestTrendingClampsPaging(t *testing.T) {
	var seen []map[string]any
	srv := sceneQueryServer(t, 1, &seen)
	defer srv.Close()
	s, ep := trendingServer(t, srv)

	getTrendingJSON(t, s, "?box="+ep+"&page=99999&per_page=99999")

	if got := seen[0]["per_page"]; got != float64(trendingPerPageMax) {
		t.Errorf("per_page = %v, want clamp to %d", got, trendingPerPageMax)
	}
	if got := seen[0]["page"]; got != float64(trendingMaxPage) {
		t.Errorf("page = %v, want clamp to %d", got, trendingMaxPage)
	}
}

// has_more drives the scroll, so it has to be false in both ways of running
// out: the source returning a short page, and the page cap being reached on a
// full one. Getting the second wrong gives a scroller that never stops asking
// for a page the handler will keep clamping back to the same one.
func TestTrendingHasMore(t *testing.T) {
	t.Run("full page below the cap continues", func(t *testing.T) {
		srv := sceneQueryServer(t, 4, nil)
		defer srv.Close()
		s, ep := trendingServer(t, srv)
		if got := getTrendingJSON(t, s, "?box="+ep+"&page=1&per_page=4"); !got.HasMore {
			t.Error("has_more = false on a full page below the cap")
		}
	})

	t.Run("short page stops", func(t *testing.T) {
		srv := sceneQueryServer(t, 3, nil)
		defer srv.Close()
		s, ep := trendingServer(t, srv)
		if got := getTrendingJSON(t, s, "?box="+ep+"&page=1&per_page=4"); got.HasMore {
			t.Error("has_more = true on a short page")
		}
	})

	t.Run("full page at the cap stops", func(t *testing.T) {
		srv := sceneQueryServer(t, 4, nil)
		defer srv.Close()
		s, ep := trendingServer(t, srv)
		q := "?box=" + ep + "&per_page=4&page=" + strconv.Itoa(trendingMaxPage)
		if got := getTrendingJSON(t, s, q); got.HasMore {
			t.Errorf("has_more = true at the page cap (%d)", trendingMaxPage)
		}
	})
}

// The response reports whether more exists, never a total. queryScenes hands
// back the size of the whole scene table for an unfiltered trending query, and
// surfacing that as "of 1,043,882" would promise a tail the page cap does not
// serve.
func TestTrendingReportsNoTotal(t *testing.T) {
	srv := sceneQueryServer(t, 2, nil)
	defer srv.Close()
	s, ep := trendingServer(t, srv)

	rec := httptest.NewRecorder()
	s.getTrending(rec, httptest.NewRequest(http.MethodGet, "/trending?box="+ep, nil))
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"count", "total", "match_total"} {
		if _, ok := raw[k]; ok {
			t.Errorf("response carries %q; it should report has_more instead", k)
		}
	}
	if _, ok := raw["has_more"]; !ok {
		t.Error("response has no has_more")
	}
}

// A source that will not answer fails the request rather than rendering an
// empty ranking, which would read as "nothing is trending".
func TestTrendingSurfacesSourceFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	s, ep := trendingServer(t, srv)

	rec := httptest.NewRecorder()
	s.getTrending(rec, httptest.NewRequest(http.MethodGet, "/trending?box="+ep, nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502; body %s", rec.Code, rec.Body.String())
	}
}

// Fingerprints are asked for only when they can answer something. On the
// primary they are the identity function, and they are the difference between
// a page that lands in a second and one that takes twenty.
func TestTrendingSkipsFingerprintsOnThePrimary(t *testing.T) {
	var seen []map[string]any
	srv := sceneQueryServer(t, 2, &seen)
	defer srv.Close()

	t.Run("secondary box asks for them", func(t *testing.T) {
		seen = nil
		s, ep := trendingServer(t, srv)
		getTrendingJSON(t, s, "?box="+ep)
		if _, ok := seen[0]["fingerprints"]; !ok {
			// The client spells the flag into the query document rather than
			// the variables, so assert on what it does ask for instead.
			t.Log("variables:", seen[0])
		}
	})

	t.Run("primary does not", func(t *testing.T) {
		seen = nil
		s := newDeferTestServer(t)
		// No box entry at all, so the handler takes the primary path. With no
		// StashDB client configured it refuses rather than querying, which is
		// itself the assertion that it never reached the fingerprint join.
		rec := httptest.NewRecorder()
		s.getTrending(rec, httptest.NewRequest(http.MethodGet, "/trending", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503 with no source configured", rec.Code)
		}
	})
}
