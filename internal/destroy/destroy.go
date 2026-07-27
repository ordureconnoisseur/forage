// Package destroy is the single door through which forage removes Stash
// scenes and their media files.
//
// Every surface that deletes — the performer page's dedup button, the grab
// purge, the duplicate-review resolve, and the poller's automatic pack
// dedup — builds a Plan here, and a Plan is the only thing Execute accepts.
// That gives three properties no per-call-site guard can:
//
//   - The invariants live in exactly one place (Vet). A new delete surface
//     gets them by construction, instead of by whoever writes it remembering
//     — which is how five SceneDestroy call sites ended up with three
//     guards (2026-07-26).
//   - What the user is SHOWN is what will HAPPEN. A Plan lists every file
//     path that will be removed and every scene that was refused, and the
//     same Plan is then executed verbatim — the preview cannot drift from
//     the action, because they are one value.
//   - Every destruction is logged with its full file paths BEFORE it runs,
//     so even the automatic (poller) surface leaves a record of exactly
//     what it was about to remove and why.
//
// The invariant Vet enforces today is the multi-file rule: Stash's
// sceneDestroy(delete_file) removes EVERY file attached to a scene, and
// Stash files a re-download with a matching fingerprint as a second file on
// the existing scene — so destroying a scene that holds more than one file
// deletes copies beyond the one in question, plus the scene record itself
// (tags, o-counter, markers, watch history). Such scenes are refused, with
// the evidence attached. Callers fall back to removing only their own file
// from disk, which Stash reconciles on its next scan.
package destroy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ordureconnoisseur/forager/internal/stash"
)

// File is one on-disk media file a destruction would remove.
type File struct {
	Path string `json:"path"`
	Size int64  `json:"size,omitempty"` // bytes; 0 when Stash didn't report it
}

// Target is one Stash scene considered for destruction, carrying every
// file Stash has attached to it — the full blast radius, not just the file
// the caller had in mind.
type Target struct {
	SceneID string `json:"scene_id"`
	Title   string `json:"title,omitempty"`
	Files   []File `json:"files"`
}

// Refusal is a target the plan rejected, with the reason and the files
// that refusing protected. Surfaces show these as "kept, because…" — a
// refusal is information for the user, never a silent skip.
type Refusal struct {
	Target Target `json:"target"`
	Reason string `json:"reason"`
}

// Plan is a vetted destruction: what will be removed, and what was refused.
// It is both the preview a UI renders and the exact input Execute acts on.
type Plan struct {
	Approved []Target  `json:"approved"`
	Refused  []Refusal `json:"refused,omitempty"`
}

// Empty reports whether the plan would destroy nothing.
func (p Plan) Empty() bool { return len(p.Approved) == 0 }

// Files returns every file path the plan will remove, for display.
func (p Plan) Files() []File {
	var out []File
	for _, t := range p.Approved {
		out = append(out, t.Files...)
	}
	return out
}

// Vet applies the destruction invariants to candidate targets and returns
// the resulting plan. It never errors: an unacceptable target becomes a
// Refusal, because "cannot destroy this" is an answer for the user, not an
// exception for the caller.
func Vet(candidates []Target) Plan {
	var p Plan
	for _, t := range candidates {
		if t.SceneID == "" {
			continue
		}
		if len(t.Files) > 1 {
			p.Refused = append(p.Refused, Refusal{
				Target: t,
				Reason: fmt.Sprintf(
					"scene has %d files attached and destroying it would delete all of them",
					len(t.Files)),
			})
			continue
		}
		p.Approved = append(p.Approved, t)
	}
	return p
}

// FromRefs groups one-ref-per-FILE SceneRefs (the shape
// stash.FindSceneRefsByStashID and the whole-library sweep both return)
// into per-scene Targets, keeping only scenes the filter accepts. A nil
// filter accepts everything.
//
// This is how the poller and the review resolve build targets without any
// extra Stash round-trip: the refs they already hold carry the file count
// implicitly, as duplicate scene ids.
func FromRefs(refs []stash.SceneRef, include func(sceneID string) bool) []Target {
	byScene := map[string]*Target{}
	var order []string
	for _, ref := range refs {
		if ref.SceneID == "" || (include != nil && !include(ref.SceneID)) {
			continue
		}
		t, ok := byScene[ref.SceneID]
		if !ok {
			t = &Target{SceneID: ref.SceneID, Title: ref.Title}
			byScene[ref.SceneID] = t
			order = append(order, ref.SceneID)
		}
		// A ref with no path still counts as a file: it means Stash has a
		// file it didn't report a path for, and undercounting here is what
		// turns the multi-file guard off exactly when it's needed.
		t.Files = append(t.Files, File{Path: ref.Path, Size: ref.Size})
	}
	out := make([]Target, 0, len(order))
	for _, id := range order {
		out = append(out, *byScene[id])
	}
	return out
}

// SceneDestroyer is the one Stash operation Execute needs. Satisfied by
// *stash.Client; narrowed to an interface so tests exercise Execute
// without a live Stash.
type SceneDestroyer interface {
	SceneDestroy(ctx context.Context, id string, deleteFile, deleteGenerated bool) error
}

// Recorder journals destructions durably (the destruction_log table;
// satisfied by *grabs.Repo). Execute writes an 'intent' row BEFORE each
// destroy — so a crash mid-destroy still leaves evidence of what was in
// flight — finalises it with the outcome after, and records refusals
// outright.
//
// A journal failure is logged loudly but never blocks the destruction: the
// user asked for a deletion, and a full disk must not quietly wedge every
// purge and dedup behind an audit write.
type Recorder interface {
	JournalDestruction(ctx context.Context, reason string, t Target, outcome, detail string) (int64, error)
	FinalizeDestruction(ctx context.Context, id int64, outcome, detail string) error
}

// Failure is one approved target Stash refused to destroy.
type Failure struct {
	Target Target
	Err    error
}

// Outcome reports what Execute actually did.
type Outcome struct {
	Destroyed []Target
	Failed    []Failure
}

// Executor carries everything a destruction needs: the Stash client, the
// journal, the log, and — when enabled — the trash configuration that turns
// permanent deletion into a recoverable move.
type Executor struct {
	Stash SceneDestroyer
	Rec   Recorder     // may be nil (tests)
	Log   *slog.Logger // may be nil
	Trash *TrashConfig // nil = permanent deletes (sceneDestroy delete_file=true)
}

// Execute destroys every approved target in the plan, journalling and
// logging each with its full file paths BEFORE acting so the record exists
// even if the process dies mid-destroy. Refused targets are never touched
// (and are journalled as such). Best-effort across targets: one failure
// doesn't stop the rest, and the outcome says exactly which went through.
//
// With Trash configured, a target's files are RENAMED into the trash and
// only the scene's metadata is destroyed — sequenced moves-first so every
// step before the Stash write is undone on failure (files renamed back).
// A target whose files can't be reached from the daemon (mapping gap,
// already-gone file) falls back to the permanent path, and the journal
// entry says so: a delete the user asked for must not be refused over a
// mapping, but it must never be silently downgraded either.
//
// reason names the surface ("grab purge", "pack dedup", …) so a
// destruction is always attributable.
func (e Executor) Execute(ctx context.Context, p Plan, reason string) Outcome {
	journal := func(t Target, outcome, detail string) int64 {
		if e.Rec == nil {
			return 0
		}
		id, err := e.Rec.JournalDestruction(ctx, reason, t, outcome, detail)
		if err != nil && e.Log != nil {
			e.Log.Warn("destruction journal write failed", "reason", reason,
				"scene", t.SceneID, "err", err)
		}
		return id
	}
	finalize := func(id int64, outcome, detail string) {
		if e.Rec == nil || id == 0 {
			return
		}
		if err := e.Rec.FinalizeDestruction(ctx, id, outcome, detail); err != nil && e.Log != nil {
			e.Log.Warn("destruction journal finalize failed", "id", id, "err", err)
		}
	}

	var out Outcome
	for _, t := range p.Approved {
		paths := make([]string, 0, len(t.Files))
		for _, f := range t.Files {
			paths = append(paths, f.Path)
		}
		if e.Log != nil {
			e.Log.Info("destroying scene", "reason", reason,
				"scene", t.SceneID, "title", t.Title, "files", paths, "trash", e.Trash != nil)
		}
		jid := journal(t, "intent", "")

		permanentWhy := ""
		if e.Trash != nil {
			moves, terr := e.Trash.trashTargetFiles(t, time.Now())
			if terr == nil {
				// Files are safely in trash; now drop the scene record. On
				// failure the moves are undone, leaving the world exactly as
				// it was.
				if err := e.Stash.SceneDestroy(ctx, t.SceneID, false, true); err != nil {
					if rerr := restoreMoves(moves); rerr != nil && e.Log != nil {
						e.Log.Error("trash rollback failed — files left in trash",
							"scene", t.SceneID, "err", rerr)
					}
					if e.Log != nil {
						e.Log.Warn("scene destroy failed", "reason", reason, "scene", t.SceneID, "err", err)
					}
					finalize(jid, "failed", err.Error())
					out.Failed = append(out.Failed, Failure{Target: t, Err: err})
					continue
				}
				movesJSON, _ := json.Marshal(moves)
				finalize(jid, "trashed", string(movesJSON))
				out.Destroyed = append(out.Destroyed, t)
				continue
			}
			if errors.Is(terr, ErrLibraryUnavailable) {
				// The mount is gone, not the file. Refuse rather than
				// degrade: a permanent Stash-side delete during an outage
				// would destroy files that are merely invisible right now.
				if e.Log != nil {
					e.Log.Warn("destroy refused: library unavailable",
						"reason", reason, "scene", t.SceneID, "err", terr)
				}
				finalize(jid, "failed", terr.Error())
				out.Failed = append(out.Failed, Failure{Target: t, Err: terr})
				continue
			}
			// Disclosed downgrade: fall through to the permanent path with
			// the reason kept for the journal.
			permanentWhy = "trash unavailable: " + terr.Error()
			if e.Log != nil {
				e.Log.Warn("trash unavailable; deleting permanently",
					"reason", reason, "scene", t.SceneID, "why", terr)
			}
		}

		if err := e.Stash.SceneDestroy(ctx, t.SceneID, true, true); err != nil {
			if e.Log != nil {
				e.Log.Warn("scene destroy failed", "reason", reason, "scene", t.SceneID, "err", err)
			}
			finalize(jid, "failed", err.Error())
			out.Failed = append(out.Failed, Failure{Target: t, Err: err})
			continue
		}
		finalize(jid, "destroyed", permanentWhy)
		out.Destroyed = append(out.Destroyed, t)
	}
	for _, r := range p.Refused {
		if e.Log != nil {
			e.Log.Info("scene destroy refused", "reason", reason,
				"scene", r.Target.SceneID, "why", r.Reason, "files", len(r.Target.Files))
		}
		journal(r.Target, "refused", r.Reason)
	}
	return out
}
