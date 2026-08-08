package invariants

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ordureconnoisseur/forager/internal/db"
)

// ── seeding helpers ──────────────────────────────────────────────────

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	dbh, err := db.Open(t.TempDir() + "/inv.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { dbh.Close() })
	return dbh
}

type cols map[string]any

// insertGrab writes one grab row, defaults overlaid by over. Raw SQL rather
// than grabs.Repo.Insert because these tests need columns the repo owns and
// never lets a caller set (status, confirmed_at, the pack counters). The
// whole point is to seed states the writer is supposed to make impossible.
func insertGrab(t *testing.T, dbh *sql.DB, id int64, over cols) {
	t.Helper()
	now := time.Now().Unix()
	c := cols{
		"id":            id,
		"release_title": fmt.Sprintf("Release.%d.1080p", id),
		"status":        "confirmed",
		"client":        "qbit",
		"kind":          "single",
		"grabbed_at":    now,
		"updated_at":    now,
	}
	for k, v := range over {
		c[k] = v
	}
	// A confirmed grab carries its confirm stamp unless a case deliberately
	// clears it. Applied after the overlay so a case that sets status
	// 'confirmed' gets a consistent row without restating this every time,
	// and so the missing-stamp invariant doesn't fire on every other case's
	// control row.
	if c["status"] == "confirmed" {
		if _, set := c["confirmed_at"]; !set {
			c["confirmed_at"] = now
		}
	}
	insertRow(t, dbh, "grabs", c)
}

func insertWatch(t *testing.T, dbh *sql.DB, id string, over cols) {
	t.Helper()
	c := cols{
		"stashdb_id": id,
		"title":      "Watch " + id,
		"status":     "watching",
		"created_at": time.Now().Unix(),
	}
	for k, v := range over {
		c[k] = v
	}
	insertRow(t, dbh, "watches", c)
}

// localPerformer makes sceneID's cast include a performer the library holds,
// which is the precondition for expecting a performer folder at all: forage
// refuses to file under a performer Stash does not have.
func localPerformer(t *testing.T, dbh *sql.DB, sceneID, performerID string) {
	t.Helper()
	insertRow(t, dbh, "scene_performer", cols{
		"scene_id": sceneID, "performer_stashdb_id": performerID,
	})
	insertRow(t, dbh, "performer_cache", cols{
		"stash_id": "local-" + performerID, "stashdb_id": performerID,
		"name": "Performer " + performerID, "refreshed_at": time.Now().Unix(),
	})
}

func insertRow(t *testing.T, dbh *sql.DB, table string, c cols) {
	t.Helper()
	names := make([]string, 0, len(c))
	for k := range c {
		names = append(names, k)
	}
	sort.Strings(names) // deterministic SQL, so a failure is reproducible
	args := make([]any, 0, len(names))
	holders := make([]string, 0, len(names))
	for _, n := range names {
		args = append(args, c[n])
		holders = append(holders, "?")
	}
	q := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		table, strings.Join(names, ", "), strings.Join(holders, ", "))
	if _, err := dbh.Exec(q, args...); err != nil {
		t.Fatalf("insert into %s: %v (%s)", table, err, q)
	}
}

// newTestChecker builds a checker whose filesystem probe treats any path
// containing "GONE" as missing and everything else as present, so a test can
// seed a vanished file without one.
func newTestChecker(dbh *sql.DB) *Checker {
	c := New(dbh, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.stat = func(p string) error {
		if strings.Contains(p, "GONE") {
			return errors.New("no such file")
		}
		return nil
	}
	return c
}

func resultFor(t *testing.T, rep *Report, name string) Result {
	t.Helper()
	for _, r := range rep.Results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no result named %q in report (have %d results)", name, len(rep.Results))
	return Result{}
}

// violatedIDs returns every subject id the whole report flagged, so a test
// can assert its CLEAN row was flagged by nothing at all rather than merely
// by not the invariant under test.
func violatedIDs(rep *Report) map[string]bool {
	out := map[string]bool{}
	for _, r := range rep.Results {
		for _, v := range r.Samples {
			out[v.Kind+" "+v.ID] = true
		}
	}
	return out
}

// ── the suite ────────────────────────────────────────────────────────

// TestInvariants seeds, per invariant, one row that violates it and one that
// satisfies it, and asserts the checker separates them.
//
// The clean row is checked against the WHOLE report, not just the invariant
// under test: an invariant that fires on correct data is worse than one that
// misses, because the four gaps this package exists to catch went unnoticed
// for months and a noisy report is how that happens again.
//
// Some rows legitimately violate two invariants at once (a placed grab with a
// known scene and no folder breaks both the watch-cast join and the
// identified-scene one). That is not double-reporting: they are two different
// joins with two different fix sites, and both should say so.
func TestInvariants(t *testing.T) {
	now := time.Now().Unix()

	cases := []struct {
		invariant string
		// breaks says what goes wrong in the real world when this assertion
		// stops holding, which is the reason each case is worth its cost.
		breaks string
		seed   func(t *testing.T, dbh *sql.DB)
		// wantID is the subject the invariant must flag, "<kind> <id>".
		wantID string
		// cleanIDs must be flagged by NO invariant in the report.
		cleanIDs []string
	}{
		{
			invariant: "grab.unfiled_though_watch_knows_cast",
			breaks: "30% of grabs pile up in an Unsorted junk drawer while the " +
				"scene's cast sits in watches.performers one join away",
			seed: func(t *testing.T, dbh *sql.DB) {
				insertWatch(t, dbh, "sc-cast", cols{"performers": `["Kenzie Reeves","Angela White"]`})
				insertWatch(t, dbh, "sc-cast-ok", cols{"performers": `["Riley Reid"]`})
				insertGrab(t, dbh, 1, cols{
					"predicted_stashdb_id": "sc-cast",
					"placed_path":          "/lib/Unsorted/one.mp4",
					"performer_name":       "",
				})
				insertGrab(t, dbh, 2, cols{
					"predicted_stashdb_id": "sc-cast-ok",
					"placed_path":          "/lib/Riley Reid/two.mp4",
					"performer_name":       "Riley Reid",
				})
			},
			wantID:   "grab 1",
			cleanIDs: []string{"grab 2"},
		},
		{
			invariant: "grab.unfiled_though_scene_identified",
			breaks: "201 confirmed grabs sat under Unsorted whose scene Stash had " +
				"already identified; nothing ever revisited the folder",
			seed: func(t *testing.T, dbh *sql.DB) {
				// The scene's cast includes someone the library HAS, so a
				// performer folder was a real option and the bin is wrong.
				localPerformer(t, dbh, "sc-10", "pf-10")
				insertGrab(t, dbh, 10, cols{
					"status": "confirmed", "actual_stashdb_id": "sc-10",
					"placed_path": "/lib/Unsorted/ten.mp4", "performer_name": "",
					"confirmed_at": now,
				})
				insertGrab(t, dbh, 11, cols{
					"status": "confirmed", "actual_stashdb_id": "sc-11",
					"placed_path": "/lib/Angela White/eleven.mp4", "performer_name": "Angela White",
					"confirmed_at": now,
				})
			},
			wantID:   "grab 10",
			cleanIDs: []string{"grab 11"},
		},
		{
			invariant: "grab.unfiled_though_scene_identified",
			breaks: "the check fires on a scene that IS filed, because a second " +
				"grab of it was hardlinked into the bin beside the filed copy; " +
				"86 such rows on the reference library, none of them actionable, " +
				"and an invariant nobody can act on is one everybody ignores",
			seed: func(t *testing.T, dbh *sql.DB) {
				// One scene, two grabs: the first carries the performer, the
				// second is the hardlink in the bin with no name on it.
				localPerformer(t, dbh, "sc-20", "pf-20")
				localPerformer(t, dbh, "sc-22", "pf-22")
				insertGrab(t, dbh, 20, cols{
					"status": "confirmed", "actual_stashdb_id": "sc-20",
					"placed_path": "/lib/Hazel Moore/twenty.mp4", "performer_name": "Hazel Moore",
					"confirmed_at": now,
				})
				insertGrab(t, dbh, 21, cols{
					"status": "confirmed", "actual_stashdb_id": "sc-20",
					"placed_path": "/lib/Unfiled/twenty.mp4", "performer_name": "",
					"confirmed_at": now,
				})
				// A scene nothing has filed: still a genuine violation.
				insertGrab(t, dbh, 22, cols{
					"status": "confirmed", "actual_stashdb_id": "sc-22",
					"placed_path": "/lib/Unfiled/twentytwo.mp4", "performer_name": "",
					"confirmed_at": now,
				})
			},
			wantID:   "grab 22",
			cleanIDs: []string{"grab 20", "grab 21"},
		},
		{
			invariant: "grab.unfiled_though_scene_identified",
			breaks: "the check demands a performer folder for a scene whose cast " +
				"the library does not have, which is the one thing the placer is " +
				"RIGHT to refuse — a folder that is not a Stash record never " +
				"appears on the performer page and is never counted as owned, so " +
				"the bin is the correct destination and the alarm is false",
			seed: func(t *testing.T, dbh *sql.DB) {
				// sc-30's cast is entirely performers the library does not
				// have: no scene_performer row joins performer_cache.
				insertRow(t, dbh, "scene_performer", cols{
					"scene_id": "sc-30", "performer_stashdb_id": "pf-not-local",
				})
				insertGrab(t, dbh, 30, cols{
					"status": "confirmed", "actual_stashdb_id": "sc-30",
					"placed_path": "/lib/Unfiled/thirty.mp4", "performer_name": "",
					"confirmed_at": now,
				})
				// sc-31 has one the library DOES hold, so it is a real miss.
				localPerformer(t, dbh, "sc-31", "pf-31")
				insertGrab(t, dbh, 31, cols{
					"status": "confirmed", "actual_stashdb_id": "sc-31",
					"placed_path": "/lib/Unfiled/thirtyone.mp4", "performer_name": "",
					"confirmed_at": now,
				})
			},
			wantID:   "grab 31",
			cleanIDs: []string{"grab 30"},
		},
		{
			invariant: "grab.unfiled_though_scene_predicted",
			breaks: "an adopted download the matcher identified is filed nowhere, " +
				"even though the matched scene came with its own cast attached",
			seed: func(t *testing.T, dbh *sql.DB) {
				insertGrab(t, dbh, 20, cols{
					"status": "placed", "predicted_stashdb_id": "sc-20",
					"predicted_confidence": 0.95,
					"placed_path":          "/lib/Unsorted/twenty.mp4", "performer_name": "",
				})
				insertGrab(t, dbh, 21, cols{
					"status": "placed", "predicted_stashdb_id": "sc-21",
					"predicted_confidence": 0.95,
					"placed_path":          "/lib/Lena Paul/tone.mp4", "performer_name": "Lena Paul",
				})
			},
			wantID:   "grab 20",
			cleanIDs: []string{"grab 21"},
		},
		{
			invariant: "grab.pack_counters_inconsistent",
			breaks: "a pack confirms and then deduplicates against a scene count " +
				"no step actually produced, so files are destroyed on a bad tally",
			seed: func(t *testing.T, dbh *sql.DB) {
				insertGrab(t, dbh, 30, cols{
					"kind": "pack", "pack_files": 5, "pack_identified": 7, "pack_deduped": 0,
					"performer_name": "Pack Performer",
				})
				insertGrab(t, dbh, 31, cols{
					"kind": "pack", "pack_files": 5, "pack_identified": 4, "pack_deduped": 2,
					"performer_name": "Pack Performer",
				})
			},
			wantID:   "grab 30",
			cleanIDs: []string{"grab 31"},
		},
		{
			invariant: "grab.live_without_client_id",
			breaks: "the poller loads the grab every tick forever and can never " +
				"advance it: there is no id to ask the download client about",
			seed: func(t *testing.T, dbh *sql.DB) {
				insertGrab(t, dbh, 40, cols{"status": "downloading", "client_id": ""})
				insertGrab(t, dbh, 41, cols{"status": "downloading", "client_id": "hash41"})
			},
			wantID:   "grab 40",
			cleanIDs: []string{"grab 41"},
		},
		{
			invariant: "grab.deferred_past_its_retry",
			breaks: "deferred grabs are outside Active(), so one the retry loop " +
				"stops reaching is stranded with nothing left to move it",
			seed: func(t *testing.T, dbh *sql.DB) {
				insertGrab(t, dbh, 50, cols{
					"status": "deferred", "attempts": 3,
					"next_retry_at": now - int64((72 * time.Hour).Seconds()),
				})
				insertGrab(t, dbh, 51, cols{
					"status": "deferred", "attempts": 1, "next_retry_at": now + 600,
				})
			},
			wantID:   "grab 50",
			cleanIDs: []string{"grab 51"},
		},
		{
			invariant: "grab.confirmed_without_timestamp",
			breaks: "the notify sweep selects on confirmed_at, so the user is " +
				"never told the scene landed",
			seed: func(t *testing.T, dbh *sql.DB) {
				insertGrab(t, dbh, 60, cols{
					"status": "confirmed", "confirmed_at": 0, "updated_at": now,
					"performer_name": "Someone",
				})
				insertGrab(t, dbh, 61, cols{
					"status": "confirmed", "confirmed_at": now, "updated_at": now,
					"performer_name": "Someone",
				})
				// Predates the confirmed_at column: 0 is correct there, and
				// reporting it would make the invariant permanently, unfixably
				// red. This row is the reason the check carries a window.
				insertGrab(t, dbh, 62, cols{
					"status": "confirmed", "confirmed_at": 0,
					"updated_at": now - int64((90 * 24 * time.Hour).Seconds()),
					"grabbed_at": now - int64((90 * 24 * time.Hour).Seconds()),
					// nolint: the folder is set so this row can't trip the
					// unfiled invariants and muddy the clean assertion.
					"performer_name": "Someone",
				})
			},
			wantID:   "grab 60",
			cleanIDs: []string{"grab 61", "grab 62"},
		},
		{
			invariant: "client_id.shared_by_several_grabs",
			breaks: "two grabs advance off one torrent's state, and purging " +
				"either deletes a path the other still claims as its copy",
			seed: func(t *testing.T, dbh *sql.DB) {
				// LIVE, so the poller still advances both off one torrent.
				insertGrab(t, dbh, 70, cols{"client_id": "dupehash", "status": "downloading", "performer_name": "A"})
				insertGrab(t, dbh, 71, cols{"client_id": "dupehash", "status": "placed", "performer_name": "A"})
				insertGrab(t, dbh, 72, cols{"client_id": "solohash", "status": "downloading", "performer_name": "A"})
				// SETTLED duplicates are not a violation: qBit deduplicates by
				// infohash, so the same torrent grabbed twice under two release
				// names legitimately shares a client id, and once both rows have
				// finished nothing can act on them. Reporting those forever is
				// how a check stops being read.
				insertGrab(t, dbh, 73, cols{"client_id": "oldhash", "status": "confirmed", "performer_name": "A"})
				insertGrab(t, dbh, 74, cols{"client_id": "oldhash", "status": "mismatched", "performer_name": "A"})
			},
			wantID:   "client_id qbit:dupehash",
			cleanIDs: []string{"client_id qbit:solohash", "client_id qbit:oldhash"},
		},
		{
			invariant: "watch.available_without_release",
			breaks: "the Watching tab shows a grab button that can only fail, " +
				"and there is no release to dismiss to make it re-search",
			seed: func(t *testing.T, dbh *sql.DB) {
				insertWatch(t, dbh, "w-bad", cols{"status": "available", "found_url": ""})
				insertWatch(t, dbh, "w-ok", cols{
					"status": "available", "found_url": "http://indexer/dl/1",
					"found_title": "Something.1080p",
				})
			},
			wantID:   "watch w-bad",
			cleanIDs: []string{"watch w-ok"},
		},
		{
			invariant: "watch.orphan_subscription_batch",
			breaks: "watches keep searching and grabbing for a subscription the " +
				"user cancelled, while the rail that would show them cannot",
			seed: func(t *testing.T, dbh *sql.DB) {
				insertRow(t, dbh, "subscriptions", cols{
					"stashdb_id": "alive", "kind": "performer", "name": "Alive",
					"created_at": now,
				})
				insertWatch(t, dbh, "w-orphan", cols{"batch_id": "sub:gone"})
				insertWatch(t, dbh, "w-kept", cols{"batch_id": "sub:alive"})
			},
			wantID:   "watch w-orphan",
			cleanIDs: []string{"watch w-kept"},
		},
		{
			invariant: "pack_duplicate.orphan_grab",
			breaks: "an unresolvable review item sits in the queue forever, so " +
				"the pending count stops meaning anything actionable",
			seed: func(t *testing.T, dbh *sql.DB) {
				insertGrab(t, dbh, 100, cols{"kind": "pack", "performer_name": "P"})
				insertRow(t, dbh, "pack_duplicate", cols{
					"id": 1, "grab_id": 999, "stashdb_id": "sc-orphan",
					"pack_scene_id": "s1", "status": "pending", "created_at": now,
					"existing_copies": `[{"scene_id":"s9","path":"/lib/old.mp4"}]`,
				})
				insertRow(t, dbh, "pack_duplicate", cols{
					"id": 2, "grab_id": 100, "stashdb_id": "sc-live",
					"pack_scene_id": "s2", "status": "pending", "created_at": now,
					"existing_copies": `[{"scene_id":"s8","path":"/lib/old2.mp4"}]`,
				})
			},
			wantID:   "pack_duplicate 1",
			cleanIDs: []string{"pack_duplicate 2"},
		},
		{
			invariant: "grab.placed_path_missing",
			breaks: "the seeding cull refuses to retire a torrent it cannot " +
				"verify, and a purge deletes nothing while reporting success",
			seed: func(t *testing.T, dbh *sql.DB) {
				insertGrab(t, dbh, 80, cols{
					"status": "confirmed", "placed_path": "/lib/GONE/eighty.mp4",
					"performer_name": "Someone", "confirmed_at": now,
				})
				insertGrab(t, dbh, 81, cols{
					"status": "confirmed", "placed_path": "/lib/Someone/eightyone.mp4",
					"performer_name": "Someone", "confirmed_at": now,
				})
			},
			wantID:   "grab 80",
			cleanIDs: []string{"grab 81"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.invariant, func(t *testing.T) {
			dbh := openDB(t)
			tc.seed(t, dbh)

			rep := newTestChecker(dbh).Run(context.Background())
			res := resultFor(t, rep, tc.invariant)

			if res.Error != "" {
				t.Fatalf("%s errored: %s", tc.invariant, res.Error)
			}
			if res.Skipped != "" {
				t.Fatalf("%s skipped (%s); it should have run", tc.invariant, res.Skipped)
			}
			if res.Count != 1 {
				t.Fatalf("%s counted %d violations, want 1 (breaks: %s)\nsamples: %+v",
					tc.invariant, res.Count, tc.breaks, res.Samples)
			}
			got := res.Samples[0].Kind + " " + res.Samples[0].ID
			if got != tc.wantID {
				t.Errorf("%s flagged %q, want %q", tc.invariant, got, tc.wantID)
			}
			if res.Samples[0].Detail == "" {
				t.Errorf("%s reported no detail; a violation nobody can act on is "+
					"the weak feedback loop this package exists to replace", tc.invariant)
			}

			flagged := violatedIDs(rep)
			for _, clean := range tc.cleanIDs {
				if flagged[clean] {
					t.Errorf("%s: correct row %q was flagged; a report that cries wolf "+
						"gets ignored exactly like no report at all", tc.invariant, clean)
				}
			}
		})
	}
}

// TestStashSceneInvariant covers the one invariant that needs Stash: a grab
// confirmed against a scene the library no longer holds. Split out because
// its evidence is a network answer, not a row.
func TestStashSceneInvariant(t *testing.T) {
	now := time.Now().Unix()
	dbh := openDB(t)
	insertGrab(t, dbh, 90, cols{
		"status": "confirmed", "actual_stashdb_id": "sc-gone",
		"placed_path": "/lib/X/gone.mp4", "performer_name": "X", "confirmed_at": now,
	})
	insertGrab(t, dbh, 91, cols{
		"status": "confirmed", "actual_stashdb_id": "sc-here",
		"placed_path": "/lib/X/here.mp4", "performer_name": "X", "confirmed_at": now,
	})

	c := newTestChecker(dbh).WithStash(stubScenes{
		endpoint: "https://stashdb.org/graphql",
		counts:   map[string]int{"sc-here": 1, "sc-gone": 0},
	})
	res := resultFor(t, c.Run(context.Background()), "grab.confirmed_scene_gone_from_stash")

	if res.Count != 1 || len(res.Samples) != 1 || res.Samples[0].ID != "90" {
		t.Fatalf("want exactly grab 90 flagged, got count=%d samples=%+v", res.Count, res.Samples)
	}
	if res.Scanned != 2 {
		t.Errorf("scanned = %d, want 2; a run that looked at less than it claims "+
			"reads as clean when it is merely blind", res.Scanned)
	}
}

// TestStashUnreachableIsNotAViolation: Stash being down must never be read as
// "the scene is gone". Getting this wrong reports every confirmed grab in the
// library as broken during a Stash restart, which is how a checker teaches
// people to ignore it.
func TestStashUnreachableIsNotAViolation(t *testing.T) {
	dbh := openDB(t)
	insertGrab(t, dbh, 90, cols{
		"status": "confirmed", "actual_stashdb_id": "sc-any",
		"placed_path": "/lib/X/a.mp4", "performer_name": "X",
	})

	c := newTestChecker(dbh).WithStash(stubScenes{
		endpoint: "https://stashdb.org/graphql",
		err:      errors.New("dial tcp 127.0.0.1:9999: connection refused"),
	})
	res := resultFor(t, c.Run(context.Background()), "grab.confirmed_scene_gone_from_stash")

	if res.Count != 0 {
		t.Fatalf("count = %d, want 0: an unreachable Stash is not a missing scene", res.Count)
	}
	if res.Skipped == "" {
		t.Error("a run that reached no answers must say so; silence here is " +
			"indistinguishable from a clean library")
	}
}

// TestPlacedPathSkippedWhenMountDown: with the library mount gone every
// placed_path stats missing. Reporting that is not a finding, it is the whole
// table, and it is the same confident wrongness the mount latch already stops
// reconcileMovedFiles committing.
func TestPlacedPathSkippedWhenMountDown(t *testing.T) {
	dbh := openDB(t)
	insertGrab(t, dbh, 80, cols{
		"status": "confirmed", "placed_path": "/lib/GONE/x.mp4", "performer_name": "X",
	})

	c := newTestChecker(dbh).WithLibraryHealth(func() bool { return false })
	res := resultFor(t, c.Run(context.Background()), "grab.placed_path_missing")

	if res.Count != 0 {
		t.Fatalf("count = %d, want 0 while the mount is down", res.Count)
	}
	if res.Skipped == "" {
		t.Error("the check must record that it skipped; a silent zero here says " +
			"'every file is present' during an outage")
	}
}

// TestCheckerNeverWrites is the load-bearing test of this package. The
// checker reports; it does not repair. A checker that quietly fixes things
// cannot be trusted to tell the truth about how much is broken, so this
// asserts byte-for-byte that a full run leaves every table exactly as it
// found it, violations and all.
func TestCheckerNeverWrites(t *testing.T) {
	now := time.Now().Unix()
	dbh := openDB(t)
	// Seed something that violates nearly everything, so the run has maximum
	// temptation to tidy up.
	insertWatch(t, dbh, "sc-cast", cols{"performers": `["Kenzie Reeves"]`})
	insertWatch(t, dbh, "w-bad", cols{"status": "available", "found_url": ""})
	insertWatch(t, dbh, "w-orphan", cols{"batch_id": "sub:gone"})
	insertGrab(t, dbh, 1, cols{
		"predicted_stashdb_id": "sc-cast", "placed_path": "/lib/GONE/one.mp4",
		"performer_name": "", "status": "confirmed", "confirmed_at": 0, "updated_at": now,
	})
	insertGrab(t, dbh, 2, cols{"status": "downloading", "client_id": ""})
	insertRow(t, dbh, "pack_duplicate", cols{
		"id": 1, "grab_id": 999, "stashdb_id": "sc-orphan", "pack_scene_id": "s1",
		"status": "pending", "created_at": now,
		"existing_copies": `[{"scene_id":"s9"}]`,
	})

	before := snapshot(t, dbh)
	rep := newTestChecker(dbh).Run(context.Background())
	if rep.Violations == 0 {
		t.Fatal("seeded data violated nothing; the no-write assertion below would be vacuous")
	}
	if got := snapshot(t, dbh); got != before {
		t.Fatalf("the checker modified the database.\nbefore: %s\nafter:  %s", before, got)
	}
}

// snapshot serialises every row of the tables the checker touches, so any
// insert, update or delete it made shows up as a difference.
func snapshot(t *testing.T, dbh *sql.DB) string {
	t.Helper()
	var sb strings.Builder
	for _, table := range []string{"grabs", "watches", "subscriptions", "pack_duplicate", "meta"} {
		rows, err := dbh.Query("SELECT * FROM " + table + " ORDER BY 1")
		if err != nil {
			t.Fatalf("snapshot %s: %v", table, err)
		}
		colNames, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatalf("snapshot %s columns: %v", table, err)
		}
		sb.WriteString(table + ":")
		for rows.Next() {
			vals := make([]any, len(colNames))
			ptrs := make([]any, len(colNames))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				t.Fatalf("snapshot %s scan: %v", table, err)
			}
			sb.WriteString(fmt.Sprintf("%v;", vals))
		}
		rows.Close()
	}
	return sb.String()
}

// TestSummaryLeaksNoDetail: /healthz is unauthenticated, so the digest it
// serves may carry counts and invariant names but never a sample, because
// samples quote library paths and release titles.
func TestSummaryLeaksNoDetail(t *testing.T) {
	dbh := openDB(t)
	insertGrab(t, dbh, 80, cols{
		"status": "confirmed", "placed_path": "/lib/GONE/secret-title.mp4",
		"performer_name": "X", "confirmed_at": time.Now().Unix(),
	})

	rep := newTestChecker(dbh).Run(context.Background())
	blob, err := json.Marshal(rep.Summary())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "secret-title") || strings.Contains(string(blob), "/lib/") {
		t.Fatalf("summary carries sample detail into the unauthenticated endpoint: %s", blob)
	}
	var got map[string]any
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatal(err)
	}
	failing, ok := got["failing"].(map[string]any)
	if !ok || failing["grab.placed_path_missing"] == nil {
		t.Fatalf("summary must still name the failing invariant and its count: %s", blob)
	}
}

// TestSummaryNamesUncheckedInvariants: a check that could not run is not a
// check that passed. Without this the digest reads "0 violations" on a daemon
// where Stash has been unconfigured for a month.
func TestSummaryNamesUncheckedInvariants(t *testing.T) {
	dbh := openDB(t)
	rep := newTestChecker(dbh).Run(context.Background()) // no Stash attached
	sum := rep.Summary()
	names, _ := sum["notChecked"].([]string)
	found := false
	for _, n := range names {
		if n == "grab.confirmed_scene_gone_from_stash" {
			found = true
		}
	}
	if !found {
		t.Fatalf("notChecked should name the skipped Stash invariant, got %v", sum)
	}
}

// TestBoundedChecksRotate: the two expensive checks must cover the whole
// table over successive runs rather than re-checking the head of it. Without
// rotation a violation past the first batch is never found on a big instance,
// which is precisely how the original gaps stayed invisible.
func TestBoundedChecksRotate(t *testing.T) {
	dbh := openDB(t)
	total := placedPathBatch + 5
	for i := 1; i <= total; i++ {
		path := fmt.Sprintf("/lib/X/f%d.mp4", i)
		if i == total {
			path = "/lib/GONE/last.mp4" // the row only a second pass can reach
		}
		insertGrab(t, dbh, int64(i), cols{
			"status": "confirmed", "placed_path": path, "performer_name": "X",
			"confirmed_at": time.Now().Unix(),
		})
	}

	c := newTestChecker(dbh)
	first := resultFor(t, c.Run(context.Background()), "grab.placed_path_missing")
	if first.Scanned != placedPathBatch {
		t.Fatalf("first pass scanned %d, want the full batch %d", first.Scanned, placedPathBatch)
	}
	if first.Count != 0 {
		t.Fatalf("first pass found %d violations, want 0: the bad row is past the batch", first.Count)
	}
	second := resultFor(t, c.Run(context.Background()), "grab.placed_path_missing")
	if second.Count != 1 {
		t.Fatalf("second pass found %d violations, want 1; the cursor did not advance", second.Count)
	}
}

// ── stubs ────────────────────────────────────────────────────────────

type stubScenes struct {
	endpoint string
	counts   map[string]int
	err      error
}

func (s stubScenes) Endpoint(context.Context) (string, error) { return s.endpoint, nil }

func (s stubScenes) CountScenes(_ context.Context, _, stashDBID string) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	return s.counts[stashDBID], nil
}

// A file missing because a better release replaced it is routine; a file
// missing with nothing else holding the scene is the alarming case. Counting
// them together hid the rows that meant the second inside a number that was
// mostly the first, and the report read as "seven files are gone" when none
// were.
func TestPlacedPathMissingSeparatesSupersededFromLost(t *testing.T) {
	dbh := openDB(t)
	const scene = "sc-upgraded"
	// Superseded: this file is GONE, but a sibling grab for the same scene
	// has one that is not.
	insertGrab(t, dbh, 1, cols{"actual_stashdb_id": scene, "placed_path": "/lib/GONE-old.mp4"})
	insertGrab(t, dbh, 2, cols{"actual_stashdb_id": scene, "placed_path": "/lib/upgrade.mp4"})
	// Genuinely lost: nothing else holds this scene.
	insertGrab(t, dbh, 3, cols{"actual_stashdb_id": "sc-lonely", "placed_path": "/lib/GONE-lost.mp4"})

	res := newTestChecker(dbh).checkPlacedPaths(t.Context())

	if res.Count != 1 {
		t.Errorf("Count = %d, want 1 (only the row with no surviving copy)", res.Count)
	}
	if res.Superseded != 1 {
		t.Errorf("Superseded = %d, want 1 (the row an upgrade replaced)", res.Superseded)
	}
	if len(res.Samples) != 1 || res.Samples[0].ID != "3" {
		t.Errorf("samples = %+v, want only grab 3", res.Samples)
	}
}
