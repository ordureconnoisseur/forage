package poller

import (
	"testing"
	"time"

	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/qbit"
)

const day = 24 * time.Hour

func at(d time.Duration, now time.Time) int64 { return now.Add(-d).Unix() }

// The rule that matters: idleness is measured from the last byte received,
// not from when the torrent was added. A slow torrent is not a dead one.
func TestDeadDueMeasuresIdlenessNotAge(t *testing.T) {
	now := time.Now()
	old := &grabs.Grab{GrabbedAt: at(200*day, now), ProgressAt: at(1*time.Hour, now)}
	tor := qbit.Torrent{State: "downloading", Progress: 0.4}
	if due, why := deadDue(tor, old, 30*day, now); due {
		t.Errorf("a torrent that moved an hour ago is alive, however old: %s", why)
	}

	// And the reverse: queued recently, but hasn't moved in months.
	stuck := &grabs.Grab{GrabbedAt: at(90*day, now), ProgressAt: at(60*day, now)}
	due, why := deadDue(tor, stuck, 30*day, now)
	if !due {
		t.Fatal("60 days without a byte is dead")
	}
	if why == "" {
		t.Error("a retirement must say why")
	}
}

// A torrent that never moved at all is the deadest case, and has no
// progress_at to measure from.
func TestDeadDueFallsBackToGrabTime(t *testing.T) {
	now := time.Now()
	g := &grabs.Grab{GrabbedAt: at(45*day, now)} // ProgressAt zero: never moved
	if due, _ := deadDue(qbit.Torrent{State: "stalledDL"}, g, 30*day, now); !due {
		t.Error("never progressed in 45 days is dead")
	}
	// With no clock at all, judge nothing.
	if due, _ := deadDue(qbit.Torrent{State: "stalledDL"}, &grabs.Grab{}, 30*day, now); due {
		t.Error("no timestamps means no verdict, not a deletion")
	}
}

// Finished torrents belong to the seeding cull. Both passes acting on one
// torrent is how a file gets deleted twice for two different reasons.
func TestDeadDueIgnoresCompletedTorrents(t *testing.T) {
	now := time.Now()
	g := &grabs.Grab{ProgressAt: at(90*day, now)}
	if due, _ := deadDue(qbit.Torrent{Progress: 1, State: "stalledUP"}, g, 30*day, now); due {
		t.Error("a completed torrent is the seeding cull's business, not this one")
	}
}

// metaDL is a different failure with nothing on disk yet, and a magnet that
// takes a while to resolve must not be swept up by a rule about downloads
// that stopped.
func TestDeadDueIgnoresMetadataFetching(t *testing.T) {
	now := time.Now()
	g := &grabs.Grab{GrabbedAt: at(90*day, now)}
	for _, state := range []string{"metaDL", "metadl", "MetaDL"} {
		if due, _ := deadDue(qbit.Torrent{State: state}, g, 30*day, now); due {
			t.Errorf("state %q is metadata fetching, not a dead download", state)
		}
	}
}

// Zero disables the check outright, and a nil grab is never forage's.
func TestDeadDueDisabled(t *testing.T) {
	now := time.Now()
	g := &grabs.Grab{ProgressAt: at(365*day, now)}
	if due, _ := deadDue(qbit.Torrent{State: "stalledDL"}, g, 0, now); due {
		t.Error("DeadAfter 0 must disable the check entirely")
	}
	if due, _ := deadDue(qbit.Torrent{State: "stalledDL"}, nil, 30*day, now); due {
		t.Error("no grab means not forage's torrent")
	}
}

// Exactly at the threshold counts, one second under does not.
func TestDeadDueBoundary(t *testing.T) {
	now := time.Now()
	tor := qbit.Torrent{State: "stalledDL"}
	if due, _ := deadDue(tor, &grabs.Grab{ProgressAt: at(30*day, now)}, 30*day, now); !due {
		t.Error("at the threshold is due")
	}
	if due, _ := deadDue(tor, &grabs.Grab{ProgressAt: at(30*day-time.Minute, now)}, 30*day, now); due {
		t.Error("a minute under the threshold is not due")
	}
}

// An idle-time rule cannot tell a torrent stuck at 98% from one stuck at 0%,
// and they are entirely different things. A 99.4%-complete archive on this
// library reached a delete list on exactly that confusion.
func TestNearlyDoneProtectsAlmostFinishedDownloads(t *testing.T) {
	for _, c := range []struct {
		progress, floor float64
		keep            bool
	}{
		{0.994, 0.9, true}, // the archive that prompted this
		{0.985, 0.9, true},
		{0.900, 0.9, true},  // exactly at the floor is protected
		{0.899, 0.9, false}, // just under is not
		{0.000, 0.9, false}, // never started: the unambiguous case
		{0.994, 0, false},   // floor disabled: idleness alone decides
		{0.000, 0, false},
	} {
		if got := nearlyDone(c.progress, c.floor); got != c.keep {
			t.Errorf("nearlyDone(%.3f, %.2f) = %v, want %v", c.progress, c.floor, got, c.keep)
		}
	}
}
