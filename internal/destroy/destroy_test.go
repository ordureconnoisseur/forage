package destroy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/seeding"
	"github.com/ordureconnoisseur/forager/internal/stash"
)

// fakeDestroyer records what Execute asks Stash to destroy, and can fail
// selected scenes.
type fakeDestroyer struct {
	calls []string
	// deleteFileArgs records the delete_file flag per call — the trash
	// tests assert metadata-only destroys, the permanent tests the opposite.
	deleteFileArgs []bool
	fail           map[string]bool
}

func (f *fakeDestroyer) SceneDestroy(_ context.Context, id string, deleteFile, deleteGenerated bool) error {
	f.calls = append(f.calls, id)
	f.deleteFileArgs = append(f.deleteFileArgs, deleteFile)
	if !deleteGenerated {
		return errors.New("test expects delete_generated")
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
	out := Executor{Stash: f, Log: discard()}.Execute(context.Background(), p, "test")
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
	out := Executor{Stash: f, Log: discard()}.Execute(context.Background(), p, "test")
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
	Executor{Stash: f, Rec: rec, Log: discard()}.Execute(context.Background(), p, "test surface")

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

// A file a torrent is serving is refused however safe it looks by every other
// measure. This is the invariant that did not exist when ten torrents on the
// reference library were broken by one batch move.
func TestVetWithRefusesASeededFile(t *testing.T) {
	seeded := seeding.New([]string{
		"/data/porn/downloads/complete/release.mp4",
		"/data/porn/downloads/complete/a pack",
	}, seeding.DefaultMinDepth)

	candidates := []Target{
		{SceneID: "1", Title: "seeded single", Files: []File{
			{Path: "/data/porn/downloads/complete/release.mp4"}}},
		{SceneID: "2", Title: "inside a seeded pack", Files: []File{
			{Path: "/data/porn/downloads/complete/a pack/scene.mp4"}}},
		{SceneID: "3", Title: "not seeded", Files: []File{
			{Path: "/data/porn/Media/Performer/scene.mp4"}}},
	}

	p := VetWith(candidates, seeded)
	if len(p.Approved) != 1 || p.Approved[0].SceneID != "3" {
		t.Fatalf("approved %+v, want only the unseeded scene 3", p.Approved)
	}
	if len(p.Refused) != 2 {
		t.Fatalf("refused %d, want 2", len(p.Refused))
	}
	for _, r := range p.Refused {
		if !strings.Contains(r.Reason, "seeding") {
			t.Errorf("refusal for %s reads %q, want it to name seeding as the cause",
				r.Target.SceneID, r.Reason)
		}
	}
}

// The multi-file rule still applies, and it is reported as itself rather than
// being masked by the newer check.
func TestVetWithKeepsTheMultiFileRefusal(t *testing.T) {
	p := VetWith([]Target{{SceneID: "1", Files: []File{
		{Path: "/a.mp4"}, {Path: "/b.mp4"}}}}, seeding.New(nil, seeding.DefaultMinDepth))
	if len(p.Refused) != 1 {
		t.Fatalf("refused %d, want 1", len(p.Refused))
	}
	if !strings.Contains(p.Refused[0].Reason, "files attached") {
		t.Errorf("reason = %q, want the multi-file rule", p.Refused[0].Reason)
	}
}

// No seeding information must not quietly become "nothing is seeding" in a way
// that CHANGES behaviour relative to plain Vet: both refuse nothing extra, so
// a qBittorrent outage degrades to today's behaviour rather than freezing
// every delete surface in forage.
func TestVetWithNoSeedingInfoMatchesVet(t *testing.T) {
	candidates := []Target{
		{SceneID: "1", Files: []File{{Path: "/data/porn/downloads/complete/x.mp4"}}},
	}
	for name, got := range map[string]Plan{
		"nil set":   VetWith(candidates, nil),
		"empty set": VetWith(candidates, seeding.New([]string{""}, seeding.DefaultMinDepth)),
		"plain Vet": Vet(candidates),
	} {
		if len(got.Approved) != 1 || len(got.Refused) != 0 {
			t.Errorf("%s: approved %d refused %d, want 1 and 0",
				name, len(got.Approved), len(got.Refused))
		}
	}
}
