package stashdb

import "testing"

// The URL parse is the whole basis for recognising a performer you own on a
// box that has no cross-id field, so it has to be strict about the host. A
// link that merely ENDS in a uuid is not a StashDB id, and reading one as such
// would mark the wrong person as owned.
func TestStashDBPerformerID(t *testing.T) {
	const id = "41bfc3e7-efb8-496d-bc79-582943fada8d"
	for _, c := range []struct {
		in, want string
	}{
		{"https://stashdb.org/performers/" + id, id},
		{"http://stashdb.org/performers/" + id, id},
		{"https://www.stashdb.org/performers/" + id, id},
		{"https://StashDB.org/performers/" + id, id},
		{"https://stashdb.org/performers/" + id + "/scenes", id},
		{"https://stashdb.org/performers/" + id + "?tab=scenes", id},
		{"https://stashdb.org/performers/" + id + "#top", id},
		{"https://stashdb.org/performers/" + id + "/", id},
		// Uppercase ids are the same id; normalising means the map lookup
		// against local cross-ids does not miss on case alone.
		{"https://stashdb.org/performers/41BFC3E7-EFB8-496D-BC79-582943FADA8D", id},

		// Not StashDB, however uuid-shaped.
		{"https://fansdb.cc/performers/" + id, ""},
		{"https://theporndb.net/performers/" + id, ""},
		{"https://notstashdb.org/performers/" + id, ""},
		{"https://stashdb.org.evil.example/performers/" + id, ""},
		// StashDB, but not a performer.
		{"https://stashdb.org/scenes/" + id, ""},
		{"https://stashdb.org/studios/" + id, ""},
		// Not a uuid.
		{"https://stashdb.org/performers/riley-reid", ""},
		{"https://stashdb.org/performers/", ""},
		{"https://stashdb.org/performers/41bfc3e7efb8496dbc79582943fada8d", ""},
		{"https://stashdb.org/performers/41bfc3e7-efb8-496d-bc79-582943fadaZZ", ""},
		{"", ""},
		{"   ", ""},
		{"onlyfans.com/someone", ""},
	} {
		if got := StashDBPerformerID(c.in); got != c.want {
			t.Errorf("StashDBPerformerID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLooksLikeUUID(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"41bfc3e7-efb8-496d-bc79-582943fada8d", true},
		{"41BFC3E7-EFB8-496D-BC79-582943FADA8D", true},
		{"41bfc3e7-efb8-496d-bc79-582943fada8", false},   // short
		{"41bfc3e7-efb8-496d-bc79-582943fada8dd", false}, // long
		{"41bfc3e7:efb8-496d-bc79-582943fada8d", false},  // wrong separator
		{"41bfc3e7-efb8-496d-bc79-582943fadag8", false},  // non-hex
		{"", false},
	} {
		if got := looksLikeUUID(c.in); got != c.want {
			t.Errorf("looksLikeUUID(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
