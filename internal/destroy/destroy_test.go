package destroy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/stash"
)

// fakeDestroyer records what Execute asks Stash to destroy, and can fail
// selected scenes.
type fakeDestroyer struct {
	calls []string
	fail  map[string]bool
}

func (f *fakeDestroyer) SceneDestroy(_ context.Context, id string, deleteFile, deleteGenerated bool) error {
	f.calls = append(f.calls, id)
	if !deleteFile || !deleteGenerated {
		return errors.New("test expects delete_file and delete_generated")
	}
	if f.fail[id] {
		return errors.New("stash said no")
	}
	return nil
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestVetRefusesMultiFile is the core invariant: a scene holding more than
// one file is never approved, because destroying it removes every file plus
// the scene record.
func TestVetRefusesMultiFile(t *testing.T) {
	p := Vet([]Target{
		{SceneID: "solo", Files: []File{{Path: "/lib/a.mp4"}}},
		{SceneID: "multi", Files: []File{{Path: "/lib/b.mp4"}, {Path: "/lib/b-redl.mp4"}}},
		{SceneID: "bare"}, // zero files: only a DB record, nothing on disk to lose
	})
	if len(p.Approved) != 2 || p.Approved[0].SceneID != "solo" || p.Approved[1].SceneID != "bare" {
		t.Fatalf("approved = %+v, want solo and bare", p.Approved)
	}
	if len(p.Refused) != 1 || p.Refused[0].Target.SceneID != "multi" {
		t.Fatalf("refused = %+v, want the multi-file scene", p.Refused)
	}
	if r := p.Refused[0].Reason; r == "" {
		t.Error("a refusal must carry its reason — it's shown to the user")
	}
}

// TestFromRefsGroupsFilesByScene: refs arrive one per FILE (the
// FindSceneRefsByStashID shape), so two refs sharing a scene id ARE the
// multi-file signal, and losing that grouping is what disabled the guard in
// the original bug.
func TestFromRefsGroupsFilesByScene(t *testing.T) {
	refs := []stash.SceneRef{
		{SceneID: "s1", Title: "One", Path: "/lib/one.mp4", Size: 10},
		{SceneID: "s2", Title: "Two", Path: "/lib/two.mp4", Size: 20},
		{SceneID: "s1", Title: "One", Path: "/lib/one-redl.mp4", Size: 10},
	}
	targets := FromRefs(refs, nil)
	if len(targets) != 2 {
		t.Fatalf("targets = %d, want 2 scenes", len(targets))
	}
	if targets[0].SceneID != "s1" || len(targets[0].Files) != 2 {
		t.Fatalf("s1 = %+v, want both files grouped under it", targets[0])
	}
	// And the filter scopes the plan without disturbing grouping.
	only2 := FromRefs(refs, func(id string) bool { return id == "s2" })
	if len(only2) != 1 || only2[0].SceneID != "s2" {
		t.Fatalf("filtered = %+v, want just s2", only2)
	}
}

// TestExecuteTouchesOnlyApproved: refused targets must never reach Stash —
// that is the entire point of the plan.
func TestExecuteTouchesOnlyApproved(t *testing.T) {
	f := &fakeDestroyer{}
	p := Vet([]Target{
		{SceneID: "ok", Files: []File{{Path: "/lib/a.mp4"}}},
		{SceneID: "guarded", Files: []File{{Path: "/lib/b.mp4"}, {Path: "/lib/c.mp4"}}},
	})
	out := Execute(context.Background(), f, p, nil, discard(), "test")
	if len(f.calls) != 1 || f.calls[0] != "ok" {
		t.Fatalf("stash saw %v, want only the approved scene", f.calls)
	}
	if len(out.Destroyed) != 1 || out.Destroyed[0].SceneID != "ok" {
		t.Fatalf("outcome = %+v", out)
	}
}

// TestExecuteReportsFailuresAndContinues: one scene failing must not stop
// the rest (every caller's long-standing best-effort semantics), and the
// failure must be attributable.
func TestExecuteReportsFailuresAndContinues(t *testing.T) {
	f := &fakeDestroyer{fail: map[string]bool{"bad": true}}
	p := Vet([]Target{
		{SceneID: "bad", Files: []File{{Path: "/lib/x.mp4"}}},
		{SceneID: "good", Files: []File{{Path: "/lib/y.mp4"}}},
	})
	out := Execute(context.Background(), f, p, nil, discard(), "test")
	if len(out.Destroyed) != 1 || out.Destroyed[0].SceneID != "good" {
		t.Fatalf("destroyed = %+v, want good to proceed past bad's failure", out.Destroyed)
	}
	if len(out.Failed) != 1 || out.Failed[0].Target.SceneID != "bad" || out.Failed[0].Err == nil {
		t.Fatalf("failed = %+v", out.Failed)
	}
}

// TestPlanFilesListsEverything: the preview renders Plan.Files, so it must
// cover every approved file — an incomplete preview is worse than none.
func TestPlanFilesListsEverything(t *testing.T) {
	p := Vet([]Target{
		{SceneID: "a", Files: []File{{Path: "/lib/a.mp4", Size: 5}}},
		{SceneID: "b", Files: []File{{Path: "/lib/b.mp4", Size: 7}}},
	})
	files := p.Files()
	if len(files) != 2 || files[0].Path != "/lib/a.mp4" || files[1].Path != "/lib/b.mp4" {
		t.Fatalf("files = %+v", files)
	}
}

// fakeRecorder captures journal writes so the record-before-acting contract
// is a tested property.
type fakeRecorder struct {
	entries []struct {
		id      int64
		reason  string
		scene   string
		outcome string
		detail  string
	}
	finals map[int64][2]string // id -> outcome, detail
}

func (f *fakeRecorder) JournalDestruction(_ context.Context, reason string, t Target, outcome, detail string) (int64, error) {
	id := int64(len(f.entries) + 1)
	f.entries = append(f.entries, struct {
		id      int64
		reason  string
		scene   string
		outcome string
		detail  string
	}{id, reason, t.SceneID, outcome, detail})
	return id, nil
}

func (f *fakeRecorder) FinalizeDestruction(_ context.Context, id int64, outcome, detail string) error {
	if f.finals == nil {
		f.finals = map[int64][2]string{}
	}
	f.finals[id] = [2]string{outcome, detail}
	return nil
}

// TestExecuteJournals pins the journal contract: an 'intent' row exists
// before each destroy and is finalised with the real outcome; refusals are
// recorded outright. This is the crash evidence — if the process dies
// mid-destroy, the intent row says what was in flight.
func TestExecuteJournals(t *testing.T) {
	f := &fakeDestroyer{fail: map[string]bool{"bad": true}}
	rec := &fakeRecorder{}
	p := Vet([]Target{
		{SceneID: "ok", Files: []File{{Path: "/lib/a.mp4"}}},
		{SceneID: "bad", Files: []File{{Path: "/lib/x.mp4"}}},
		{SceneID: "multi", Files: []File{{Path: "/lib/m1.mp4"}, {Path: "/lib/m2.mp4"}}},
	})
	Execute(context.Background(), f, p, rec, discard(), "test surface")

	byScene := map[string]string{}
	for _, e := range rec.entries {
		byScene[e.scene] = e.outcome
		if e.reason != "test surface" {
			t.Errorf("entry for %s carries reason %q", e.scene, e.reason)
		}
	}
	if byScene["ok"] != "intent" || byScene["bad"] != "intent" {
		t.Fatalf("destroys must journal as intent first, got %v", byScene)
	}
	if byScene["multi"] != "refused" {
		t.Fatalf("refusal not journalled, got %v", byScene)
	}
	// Finalisation: ok -> destroyed, bad -> failed with the error kept.
	if got := rec.finals[1]; got[0] != "destroyed" {
		t.Errorf("ok finalised as %v, want destroyed", got)
	}
	if got := rec.finals[2]; got[0] != "failed" || got[1] == "" {
		t.Errorf("bad finalised as %v, want failed with the error", got)
	}
}
