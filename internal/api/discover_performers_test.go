package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

// scenesWith builds a ranking: one scene per argument, in order, each holding
// the named performers. "name" and "name:MALE" are both accepted.
func scenesWith(rows ...[]string) []stashdb.Scene {
	out := make([]stashdb.Scene, 0, len(rows))
	for _, names := range rows {
		var sc stashdb.Scene
		for _, n := range names {
			gender := "FEMALE"
			if i := strings.IndexByte(n, ':'); i >= 0 {
				n, gender = n[:i], n[i+1:]
			}
			sc.Performers = append(sc.Performers,
				stashdb.ScenePerformer{ID: "id-" + n, Name: n, Gender: gender})
		}
		out = append(out, sc)
	}
	return out
}

func rankedNames(list []performerTally) []string {
	out := make([]string, 0, len(list))
	for _, t := range list {
		out = append(out, t.P.Name)
	}
	return out
}

// The whole point of the derived lens: recurrence across the trending scenes
// is the ranking. StashDB exposes no such sort, so if this ordering is wrong
// there is nothing else keeping it honest.
func TestRankTrendingPerformersOrdersByRecurrence(t *testing.T) {
	got := rankedNames(rankTrendingPerformers(scenesWith(
		[]string{"Ana", "Bea"},
		[]string{"Ana"},
		[]string{"Cat"},
		[]string{"Ana", "Cat"},
	), nil, false))
	want := []string{"Ana", "Cat", "Bea"} // 3, 2, 1
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// Ties go to whoever appears higher up the ranking. Without this, two people
// on two trending scenes each are ordered by Go's map iteration, which is
// randomised: the strip would reshuffle on every refresh for no reason.
func TestRankTrendingPerformersBreaksTiesByRank(t *testing.T) {
	scenes := scenesWith(
		[]string{"Early"},
		[]string{"Late"},
		[]string{"Early"},
		[]string{"Late"},
	)
	for i := 0; i < 20; i++ {
		got := rankedNames(rankTrendingPerformers(scenes, nil, false))
		if len(got) != 2 || got[0] != "Early" {
			t.Fatalf("run %d: order = %v, want Early first (both have 2)", i, got)
		}
	}
}

// Both filters the strip shares. Someone already in the library has nothing to
// offer a "who should I add" list, and hide-male is a user setting that has to
// reach every surface or it is not a setting.
func TestRankTrendingPerformersFilters(t *testing.T) {
	scenes := scenesWith([]string{"Ana", "Bob:MALE", "Cat"}, []string{"Ana", "Cat"})

	got := rankedNames(rankTrendingPerformers(scenes, nil, false))
	if len(got) != 3 {
		t.Fatalf("unfiltered = %v, want all three", got)
	}
	got = rankedNames(rankTrendingPerformers(scenes, nil, true))
	for _, n := range got {
		if n == "Bob" {
			t.Error("hideMale did not drop the male performer")
		}
	}
	got = rankedNames(rankTrendingPerformers(scenes, map[string]bool{"id-Ana": true}, false))
	for _, n := range got {
		if n == "Ana" {
			t.Error("a performer already in the library was offered for adding")
		}
	}
}

// A performer with no id or no name cannot be added, so a card for them is a
// dead control.
func TestRankTrendingPerformersSkipsUnusable(t *testing.T) {
	scenes := []stashdb.Scene{{Performers: []stashdb.ScenePerformer{
		{ID: "", Name: "No Id"},
		{ID: "id-x", Name: ""},
		{ID: "id-ok", Name: "Fine"},
	}}}
	got := rankedNames(rankTrendingPerformers(scenes, nil, false))
	if len(got) != 1 || got[0] != "Fine" {
		t.Errorf("got %v, want just the usable one", got)
	}
}

const dismissID = "41bfc3e7-efb8-496d-bc79-582943fada8d"

func dismiss(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.postDismissPerformer(rec, httptest.NewRequest(
		http.MethodPost, "/discover/performers/dismiss", strings.NewReader(body)))
	return rec
}

// Dismissals survive a restart, which is the only reason to store them at all.
func TestDismissPerformerPersistsAndUndoes(t *testing.T) {
	s := newDeferTestServer(t)

	if rec := dismiss(t, s, `{"stashdb_id":"`+dismissID+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("dismiss: %d %s", rec.Code, rec.Body.String())
	}
	set, err := loadDismissedPerformers(t.Context(), s.db)
	if err != nil || !set[dismissID] {
		t.Fatalf("not stored: set=%v err=%v", set, err)
	}

	if rec := dismiss(t, s, `{"stashdb_id":"`+dismissID+`","undo":true}`); rec.Code != http.StatusOK {
		t.Fatalf("undo: %d %s", rec.Code, rec.Body.String())
	}
	set, err = loadDismissedPerformers(t.Context(), s.db)
	if err != nil || set[dismissID] {
		t.Fatalf("undo did not remove it: set=%v err=%v", set, err)
	}
}

// Re-dismissing must not grow the list. It is a set, and a list that grows
// every time someone taps the same card would reach the cap on one performer.
func TestDismissPerformerDeduplicates(t *testing.T) {
	s := newDeferTestServer(t)
	for i := 0; i < 5; i++ {
		dismiss(t, s, `{"stashdb_id":"`+dismissID+`"}`)
	}
	rec := dismiss(t, s, `{"stashdb_id":"`+dismissID+`"}`)
	var out struct {
		Dismissed int `json:"dismissed"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Dismissed != 1 {
		t.Errorf("dismissed = %d after six taps on one card, want 1", out.Dismissed)
	}
}

// Anything that is not a StashDB id is refused, so the list cannot fill with
// values that could never match a performer.
func TestDismissPerformerRefusesNonIDs(t *testing.T) {
	s := newDeferTestServer(t)
	for _, body := range []string{
		`{"stashdb_id":""}`, `{"stashdb_id":"nope"}`, `{}`, `not json`,
	} {
		if rec := dismiss(t, s, body); rec.Code != http.StatusBadRequest {
			t.Errorf("body %q -> %d, want 400", body, rec.Code)
		}
	}
}

// Dismissals are applied when the strip is SERVED, and the surplus each lens
// holds is what closes the gap they leave. A dismissal that shrank the strip
// would empty it over a few taps and not refill for an hour.
func TestServableDropsDismissedAndBackfills(t *testing.T) {
	s := newDeferTestServer(t)
	full := make([]discoverPerformer2, 0, performerPickStore)
	for i := 0; i < performerPickStore; i++ {
		full = append(full, discoverPerformer2{
			StashDBID: strings.Replace(dismissID, "41bf", string(rune('a'+i/10))+
				string(rune('0'+i%10))+"bf", 1),
			Name: "P" + string(rune('a'+i)),
		})
	}
	in := &discoverPerformersResponse{Trending: full, Debut: full, Active: full}

	got := s.servable(t.Context(), in)
	if len(got.Trending) != performerPickCount {
		t.Fatalf("served %d, want %d", len(got.Trending), performerPickCount)
	}

	dismiss(t, s, `{"stashdb_id":"`+full[0].StashDBID+`"}`)
	got = s.servable(t.Context(), in)
	if len(got.Trending) != performerPickCount {
		t.Errorf("after one dismissal served %d, want the strip refilled to %d",
			len(got.Trending), performerPickCount)
	}
	for _, p := range got.Trending {
		if p.StashDBID == full[0].StashDBID {
			t.Error("a dismissed performer was served anyway")
		}
	}
}
