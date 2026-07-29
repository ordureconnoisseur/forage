package stash

import "testing"

// TestCareerLength covers StashDB's start/end pair collapsing into the single
// free-text career field Stash stores.
func TestCareerLength(t *testing.T) {
	for _, c := range []struct{ start, end, want string }{
		{"", "", ""},
		{"2015", "", "2015-"},
		{"", "2020", "2020"},
		{"2015", "2020", "2015-2020"},
	} {
		if got := careerLength(c.start, c.end); got != c.want {
			t.Errorf("careerLength(%q,%q) = %q, want %q", c.start, c.end, got, c.want)
		}
	}
}

// TestSplitAliases pins that StashDB's comma blob becomes a clean list.
// Blanks would create empty aliases on the newly-created performer.
func TestSplitAliases(t *testing.T) {
	got := splitAliases(" Ann ,, Bee,Cee , ")
	if len(got) != 3 || got[0] != "Ann" || got[1] != "Bee" || got[2] != "Cee" {
		t.Errorf("splitAliases = %#v, want [Ann Bee Cee]", got)
	}
	if n := len(splitAliases("")); n != 0 {
		t.Errorf("empty aliases gave %d entries", n)
	}
}

// TestScrapePerformerByStashDBIDRequiresAll guards the inputs. A blank id or
// name would turn an exact "create this performer" into a name-only guess,
// which is how a same-named different performer ends up in the library.
func TestScrapePerformerByStashDBIDRequiresAll(t *testing.T) {
	c := &Client{}
	for _, tc := range []struct{ endpoint, name, id string }{
		{"", "Someone", "sdb-1"},
		{"https://stashdb.org/graphql", "", "sdb-1"},
		{"https://stashdb.org/graphql", "Someone", ""},
	} {
		if _, err := c.ScrapePerformerByStashDBID(nil, tc.endpoint, tc.name, tc.id); err == nil {
			t.Errorf("endpoint=%q name=%q id=%q: want an error, got nil",
				tc.endpoint, tc.name, tc.id)
		}
	}
}

// TestCreatePerformerFromScrapeRejectsEmpty: a nil or nameless scrape must
// not reach Stash as a create.
func TestCreatePerformerFromScrapeRejectsEmpty(t *testing.T) {
	c := &Client{}
	if _, err := c.CreatePerformerFromScrape(nil, "ep", "sdb-1", nil); err == nil {
		t.Error("nil scrape: want an error")
	}
	if _, err := c.CreatePerformerFromScrape(nil, "ep", "sdb-1", &ScrapedPerformer{}); err == nil {
		t.Error("nameless scrape: want an error")
	}
}
