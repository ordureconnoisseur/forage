package poller

import (
	"testing"
	"time"
)

// The judgement this whole pass exists to make. "Smaller in the library" and
// "the only copy" are one comparison apart, and they are the difference
// between reclaiming a stranded re-encode original and deleting something
// irreplaceable.
func TestClassifyAgainstLibrary(t *testing.T) {
	const dl = 3_000_000_000
	for _, c := range []struct {
		name    string
		library []int64
		want    ReconcileVerdict
	}{
		{"nothing in the library is the ONLY copy", nil, VerdictOrphan},
		{"empty slice is the same thing", []int64{}, VerdictOrphan},
		{"a smaller library copy is a re-encode leftover",
			[]int64{900_000_000}, VerdictSuperseded},
		{"identical size is the same content",
			[]int64{dl}, VerdictDuplicate},
		{"a bigger library copy is not this file's replacement",
			[]int64{5_000_000_000}, VerdictVariant},
		{"identical wins over smaller: the library demonstrably has THIS file",
			[]int64{900_000_000, dl}, VerdictDuplicate},
		{"several bigger ones are still just variants",
			[]int64{4e9, 5e9}, VerdictVariant},
		{"one smaller among bigger ones is still superseded",
			[]int64{5_000_000_000, 800_000_000}, VerdictSuperseded},
		// A zero-size library entry is a stat that failed or a truncated file.
		// It must never count as "the library has a smaller copy", or a broken
		// file would mark a good download redundant.
		{"a zero-length library file proves nothing", []int64{0}, VerdictVariant},
	} {
		if got := classifyAgainstLibrary(dl, c.library); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// The cursor is the entire reason this can run inside the daemon instead of
// walking 323,539 library files every time.
func TestReconcileCandidateHonoursTheCursor(t *testing.T) {
	cursor := time.Now().Add(-24 * time.Hour)
	older := cursor.Add(-time.Hour)
	newer := cursor.Add(time.Hour)

	if reconcileCandidate("/dl/complete/a.mp4", older, cursor) {
		t.Error("a file untouched since the last pass must not be re-examined")
	}
	if !reconcileCandidate("/dl/complete/a.mp4", newer, cursor) {
		t.Error("a file written since the last pass is a candidate")
	}
	// A zero cursor is a first run: everything qualifies.
	if !reconcileCandidate("/dl/complete/a.mp4", older, time.Time{}) {
		t.Error("with no cursor, the first pass considers everything")
	}
}

func TestReconcileCandidateSkipsWhatCannotBeJudged(t *testing.T) {
	newer := time.Now()
	for _, p := range []string{
		"/data/porn/downloads/incomplete/x/a.mp4", // still downloading
		"/dl/x/__ADMIN__/SABnzbd_nzf_abc",         // SAB working file
		"/dl/half.mp4.part",
		"/dl/half.mp4.!qB",
		"/dl/complete/cover.jpg", // not media
		"/dl/complete/notes.nfo",
	} {
		if reconcileCandidate(p, newer, time.Time{}) {
			t.Errorf("%s should not be a candidate", p)
		}
	}
	if !reconcileCandidate("/dl/complete/real.MP4", newer, time.Time{}) {
		t.Error("extension matching must be case-insensitive")
	}
}
