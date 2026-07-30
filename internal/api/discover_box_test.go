package api

import (
	"testing"
	"time"

	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

func boxResult(scenes ...stashdb.Scene) *stashdb.QueryScenesResult {
	return &stashdb.QueryScenesResult{Count: len(scenes), Scenes: scenes}
}

// Scenes the library already has on THIS box are dropped. Ownership here is
// per-endpoint: a FansDB id can only be matched against FansDB cross-ids, and
// the whole point of the feed is what you do not have.
func TestBoxScenesDropsOwned(t *testing.T) {
	res := boxResult(
		stashdb.Scene{ID: "have-it", Title: "Owned"},
		stashdb.Scene{ID: "want-it", Title: "New"},
	)
	got := boxScenes(res, map[string]bool{"have-it": true}, nil, nil, false)
	if len(got) != 1 || got[0].StashDBID != "want-it" {
		t.Fatalf("got %+v, want only want-it", got)
	}
}

// A performer resolved to a local one is marked local AND carries the local
// Stash id, because that id is what the pill navigates with. Marking them
// owned without it would leave a dead pill: no "+" and nowhere to go.
func TestBoxScenesMarksResolvedPerformersLocal(t *testing.T) {
	res := boxResult(stashdb.Scene{
		ID: "s1",
		Performers: []stashdb.ScenePerformer{
			{ID: "known", Name: "Have Her", Gender: "FEMALE"},
			{ID: "new", Name: "Someone", Gender: "FEMALE"},
		},
	})
	got := boxScenes(res, nil, map[string]string{"known": "42"}, nil, false)
	if len(got) != 1 || len(got[0].Performers) != 2 {
		t.Fatalf("got %+v", got)
	}
	have, missing := got[0].Performers[0], got[0].Performers[1]
	if !have.Local || have.StashID != "42" {
		t.Errorf("resolved performer = %+v, want local with stash id 42", have)
	}
	if missing.Local || missing.StashID != "" {
		t.Errorf("unresolved performer = %+v, want not local", missing)
	}
	// The box id rides along either way: it is how the "+" adds them, and
	// how the next page's lookup recognises them.
	if have.StashDBID != "known" || missing.StashDBID != "new" {
		t.Errorf("box ids lost: %+v %+v", have, missing)
	}
}

// An unresolved performer keeps the "+". Nothing may claim ownership forage
// cannot point at a local row for.
func TestBoxScenesLeavesUnknownPerformersAddable(t *testing.T) {
	res := boxResult(stashdb.Scene{
		ID:         "s1",
		Performers: []stashdb.ScenePerformer{{ID: "p1", Name: "Someone", Gender: "FEMALE"}},
	})
	got := boxScenes(res, nil, map[string]string{"someone-else": "7"}, nil, false)
	if got[0].Performers[0].Local {
		t.Error("a performer with no local match must not be reported as local")
	}
}

// The credited name wins when the box supplies one, matching how the scene
// actually bills them.
func TestBoxScenesPrefersCreditedName(t *testing.T) {
	res := boxResult(stashdb.Scene{
		ID:         "s1",
		Performers: []stashdb.ScenePerformer{{ID: "p1", Name: "Real Name", As: "Stage Name"}},
	})
	got := boxScenes(res, nil, nil, nil, false)
	if got[0].Performers[0].Name != "Stage Name" {
		t.Fatalf("name = %q, want the credited one", got[0].Performers[0].Name)
	}
}

// hide-male-performers applies here too, and with the same rule: only a KNOWN
// male is hidden. The live query carries gender directly, so unlike the
// StashDB path this needs no backfill — but an unrecorded gender is still not
// an answer.
func TestBoxScenesHidesOnlyKnownMales(t *testing.T) {
	res := boxResult(stashdb.Scene{
		ID: "s1",
		Performers: []stashdb.ScenePerformer{
			{ID: "f", Name: "Fem", Gender: "FEMALE"},
			{ID: "m", Name: "Male", Gender: "MALE"},
			{ID: "u", Name: "Unrecorded", Gender: ""},
			{ID: "l", Name: "Lowercase", Gender: "male"},
		},
	})
	got := boxScenes(res, nil, nil, nil, true)
	var names []string
	for _, p := range got[0].Performers {
		names = append(names, p.Name)
	}
	if len(names) != 2 || names[0] != "Fem" || names[1] != "Unrecorded" {
		t.Fatalf("kept %v, want [Fem Unrecorded] (lowercase male must still be hidden)", names)
	}

	off := boxScenes(boxResult(res.Scenes[0]), nil, nil, nil, false)
	if len(off[0].Performers) != 4 {
		t.Fatalf("with the setting off all four should remain, got %d", len(off[0].Performers))
	}
}

// A scene whose whole cast is filtered still appears, and its performer list
// is an empty array rather than null so the UI does not have to special-case
// it. Same rule as the StashDB feed.
func TestBoxScenesKeepsSceneWithNoPerformersLeft(t *testing.T) {
	res := boxResult(stashdb.Scene{
		ID:         "s1",
		Performers: []stashdb.ScenePerformer{{ID: "m", Name: "Male", Gender: "MALE"}},
	})
	got := boxScenes(res, nil, nil, nil, true)
	if len(got) != 1 {
		t.Fatalf("scene dropped: %+v", got)
	}
	if got[0].Performers == nil {
		t.Error("performers must marshal as [], not null")
	}
}

// A nil result (the trending query failed, or we are past page 1) is an empty
// list, not a panic and not a null in the JSON.
func TestBoxScenesNilResult(t *testing.T) {
	got := boxScenes(nil, nil, nil, nil, false)
	if got == nil || len(got) != 0 {
		t.Fatalf("got %+v, want an empty slice", got)
	}
}

// The watch state carries over, so a scene already being tracked reads the
// same on a secondary box as it does on StashDB.
func TestBoxScenesCarriesWatchStatus(t *testing.T) {
	res := boxResult(stashdb.Scene{ID: "s1"})
	got := boxScenes(res, nil, nil, map[string]string{"s1": "watching"}, false)
	if got[0].WatchStatus != "watching" {
		t.Fatalf("watch status = %q", got[0].WatchStatus)
	}
}

// StashDB must never be routed down the live path — it is the cached feed
// everything else depends on.
func TestIsStashDBEndpoint(t *testing.T) {
	for _, c := range []struct {
		ep   string
		want bool
	}{
		{"https://stashdb.org/graphql", true},
		{"https://StashDB.org/graphql", true},
		{"https://fansdb.cc/graphql", false},
		{"https://javstash.org/graphql", false},
		{"", false},
	} {
		if got := isStashDBEndpoint(c.ep); got != c.want {
			t.Errorf("isStashDBEndpoint(%q) = %v, want %v", c.ep, got, c.want)
		}
	}
}

// An unknown, primary or unreachable endpoint yields no entry, so getDiscover
// falls through to the cached StashDB feed instead of erroring.
func TestBoxByEndpointRefusesUnusable(t *testing.T) {
	s := newDeferTestServer(t)
	s.boxes.entries = []boxEntry{
		{box: discoverBox{Endpoint: "https://stashdb.org/graphql", Primary: true}},
		{box: discoverBox{Endpoint: "https://dead.example/graphql", Unreachable: "403"}},
		{box: discoverBox{Endpoint: "https://nokey.example/graphql"}}, // no client
	}
	s.boxes.at = time.Now() // fresh, so stashBoxes() serves the cache
	for _, ep := range []string{
		"", "https://stashdb.org/graphql", "https://dead.example/graphql",
		"https://nokey.example/graphql", "https://never-heard-of-it/graphql",
	} {
		if got := s.boxByEndpoint(t.Context(), ep); got != nil {
			t.Errorf("boxByEndpoint(%q) returned %+v, want nil", ep, got.box)
		}
	}
}
