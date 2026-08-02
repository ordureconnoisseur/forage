package invariants

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestSuiteCostAtLibraryScale is the regression guard on cost.
//
// The suite runs inside the poller tick, on the same single goroutine that
// does placement and confirmation, against the live SQLite file. A whole-table
// scan of a few thousand rows is free; a cartesian join, or an invariant that
// forgets to bound itself, is not, and it would be paid by real work waiting
// behind it.
//
// Seeded at the reference instance's scale (865 grabs, 1,038 watches, rounded
// up). Measured at 3.7ms when written, so the ceiling is a 500x margin: this
// fails on an accidental O(n^2), not on a slow machine.
func TestSuiteCostAtLibraryScale(t *testing.T) {
	const (
		grabCount  = 900
		watchCount = 1100
		ceiling    = 2 * time.Second
	)

	dbh := openDB(t)
	now := time.Now().Unix()
	tx, err := dbh.Begin()
	if err != nil {
		t.Fatal(err)
	}
	// Every row is internally consistent, so the pass finds nothing and pays
	// the full scan cost on all of them. A seed full of violations would exit
	// checks early on some rows and understate the real cost.
	for i := 1; i <= grabCount; i++ {
		if _, err := tx.Exec(`
			INSERT INTO grabs (id, release_title, status, client, client_id, kind,
			                   grabbed_at, updated_at, confirmed_at, placed_path,
			                   performer_name, actual_stashdb_id, predicted_stashdb_id)
			VALUES (?,?,'confirmed','qbit',?,'single',?,?,?,?,?,?,?)`,
			i, fmt.Sprintf("Release.%d.1080p", i), fmt.Sprintf("hash%d", i),
			now, now, now,
			fmt.Sprintf("/lib/P%d/f%d.mp4", i%50, i), fmt.Sprintf("P%d", i%50),
			fmt.Sprintf("sc-%d", i), fmt.Sprintf("sc-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	// More watches than grabs, and every one carrying a cast, so the
	// grabs-to-watches join has the widest set it will meet in practice.
	for i := 1; i <= watchCount; i++ {
		if _, err := tx.Exec(`
			INSERT INTO watches (stashdb_id, title, status, performers, created_at)
			VALUES (?, 'T', 'watching', '["A","B"]', ?)`,
			fmt.Sprintf("sc-%d", i), now); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	rep := newTestChecker(dbh).Run(context.Background())
	elapsed := time.Since(start)

	if rep.Violations != 0 {
		t.Fatalf("consistent data reported %d violations: %v", rep.Violations, rep.Failing)
	}
	if elapsed > ceiling {
		t.Fatalf("a full pass took %s over %d grabs and %d watches, ceiling %s; "+
			"this runs on the poller's own goroutine, ahead of placement work",
			elapsed, grabCount, watchCount, ceiling)
	}
	t.Logf("full pass over %d grabs and %d watches: %s", grabCount, watchCount, elapsed)
}
