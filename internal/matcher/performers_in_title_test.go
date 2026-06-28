package matcher

import "testing"

// TestPerformersInTitle pins the RSS pre-filter primitive: it must report the
// canonical NAMES of corpus performers found in a release title (local scan, no
// StashDB), including via accent-folding, and return nothing when no corpus
// performer appears.
func TestPerformersInTitle(t *testing.T) {
	corpus := []Entity{
		{ID: "p1", Name: "Renée Gaillard"},
		{ID: "p2", Name: "Kenzie Reeves"},
	}
	names := make(map[string]string, len(corpus))
	for _, p := range corpus {
		names[p.ID] = p.Name
	}
	m := &Matcher{
		perfScanner:  NewScanner(corpus, DefaultScannerOptions()),
		perfNameByID: names,
	}

	got := m.PerformersInTitle("ATKGirlfriends.18.10.31.Kenzie.Reeves.XXX.1080p.MP4-KTR")
	if len(got) != 1 || got[0] != "Kenzie Reeves" {
		t.Fatalf("PerformersInTitle = %v, want [Kenzie Reeves]", got)
	}
	// Accent-folded: title has no accent, corpus name does.
	got = m.PerformersInTitle("Renee.Gaillard.Goes.Hard.XXX.1080p.MP4")
	if len(got) != 1 || got[0] != "Renée Gaillard" {
		t.Fatalf("PerformersInTitle (accent) = %v, want [Renée Gaillard]", got)
	}
	// No watched performer present -> empty (the release the pre-filter drops).
	if got := m.PerformersInTitle("SomeStudio.Random.Person.XXX.1080p"); len(got) != 0 {
		t.Errorf("PerformersInTitle (no match) = %v, want empty", got)
	}
}
