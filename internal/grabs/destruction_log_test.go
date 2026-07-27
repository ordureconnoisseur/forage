package grabs

import (
	"context"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/destroy"
)

// TestDestructionJournalRoundtrip covers the SQL half of the journal: an
// intent row lands with its full file list, finalisation rewrites the
// outcome, and the list comes back newest-first with files decoded.
func TestDestructionJournalRoundtrip(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()

	id, err := r.JournalDestruction(ctx, "grab purge", destroy.Target{
		SceneID: "183951",
		Title:   "Liora Vane Is Unboxed and Unleashed",
		Files: []destroy.File{
			{Path: `Z:\Media\Liora Vane\a.mp4`, Size: 1517077800},
		},
	}, "intent", "")
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	if _, err := r.JournalDestruction(ctx, "pack dedup", destroy.Target{
		SceneID: "999",
		Files:   []destroy.File{{Path: "/lib/m1.mp4"}, {Path: "/lib/m2.mp4"}},
	}, "refused", "scene has 2 files attached"); err != nil {
		t.Fatalf("journal refusal: %v", err)
	}

	if err := r.FinalizeDestruction(ctx, id, "destroyed", ""); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	entries, err := r.ListDestructions(ctx, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	// Newest first: the refusal, then the finalised destroy.
	if entries[0].Outcome != "refused" || entries[0].Detail == "" || len(entries[0].Files) != 2 {
		t.Errorf("refusal row = %+v", entries[0])
	}
	if entries[1].Outcome != "destroyed" || entries[1].SceneID != "183951" {
		t.Errorf("destroy row = %+v", entries[1])
	}
	if len(entries[1].Files) != 1 || entries[1].Files[0].Size != 1517077800 {
		t.Errorf("files did not round-trip: %+v", entries[1].Files)
	}

	// The /healthz tallies: grouped by outcome, and a future since-cutoff
	// excludes everything (rows were created "now").
	counts, err := r.CountDestructionsByOutcome(ctx, 0)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if counts["destroyed"] != 1 || counts["refused"] != 1 {
		t.Errorf("counts = %v, want destroyed:1 refused:1", counts)
	}
	future, err := r.CountDestructionsByOutcome(ctx, entries[0].CreatedAt+3600)
	if err != nil {
		t.Fatalf("count since: %v", err)
	}
	if len(future) != 0 {
		t.Errorf("future-cutoff counts = %v, want empty", future)
	}
}
