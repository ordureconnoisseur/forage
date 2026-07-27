# Bulletproofing forage

Objective: a user should be unable to lose data, and forage should be unable
to lie about what it did. "Bulletproof" decomposes into four enforceable
properties, each phase below serving at least one:

1. **Bounded blast radius** — no operation can destroy more than what the
   user asked about, and destruction is reversible for a window.
2. **One door per dangerous thing** — every irreversible action goes through
   a single choke point that enforces the invariants, so a new call site
   can't quietly skip them (the recurring bug class of 2026-07: five
   `SceneDestroy` sites, three guarded, two forgotten).
3. **Adversarial verification** — the failure modes are tested on purpose,
   not discovered in production. Includes fault injection, fuzzing, and a
   regression-gated matcher benchmark.
4. **Loud, recoverable failure** — when something does break, the record
   (journal, audit log, backups) is good enough to say exactly what happened
   and to undo it.

Perfection is the objective; the sequencing below is by expected damage
prevented per unit of work. Phase 0 is hours; phases 1–3 are the real
engineering; 4–5 are ongoing posture.

Status legend: ☐ todo · ◐ partial · ☑ done

---

## Phase 0 — close the two open destroy holes (immediate)

The multi-file-scene guard added 2026-07-26 covers three of five
`SceneDestroy(delete_file=true)` call sites. Two remain unguarded:

- ☑ **Pack purge** (`internal/api/grab_detail.go`): purging a pack
  enumerates every scene under its directory and destroys each. A scene
  holding a second file — including one that lives *outside* the pack —
  loses that file too. Fix: skip multi-file scenes exactly as the single
  path does; the disk sweep already removes the pack's own files, and Stash
  drops vanished files on its next scan.
- ☑ **Poller auto-dedup** (`internal/poller/grabs.go` `dedupPack`
  destroy closure): the only *automatic* destroy in the codebase — runs
  with no user action when `packDedupKeep` is `existing` or `pack` (the
  live setting on the reference instance is `existing`). It already holds
  one ref **per file** from `FindSceneRefsByStashID`, so the per-scene file
  count is in hand; refuse any scene with >1 and log why, same as the
  resolve endpoint.
- ☑ One test per surface, plus a **meta-test**: grep the tree for
  `SceneDestroy(` and assert every call site is in the audited list, so a
  sixth site can't appear unguarded (this is what turns "I fixed the ones I
  found" into "there are no others").

Acceptance: zero `SceneDestroy(…, true, …)` call sites without a file-count
check; suite green; deployed to mini before the next pack grab runs.

## Phase 1 — one door for deletion

- ☑ **`internal/destroy`: a single destroy façade.** All five
  surfaces call it instead of `stash.Client.SceneDestroy` / `os.RemoveAll`
  directly. It owns the invariants: multi-file refusal, "kept side must
  exist" (generalising the resolve endpoint's `keptAlive`), and
  path-inside-library validation for direct file removals. New call sites
  get the guards by construction. Enforce with a lint/CI check that the raw
  primitives are only imported by the façade.
- ☑ **Destruction journal** (`destruction_log` + GET /destructions): surface, grab/scene ids, paths,
  sizes, requested-by (user click vs poller), outcome — written *before*
  acting, finalised after. Answers "what did forage delete and why" without
  archaeology. Surfaced read-only in Settings.
- ☑ **Trash, not unlink.** Deletion is now: reverse-map + stat every file
  (any miss → disclosed permanent fallback), RENAME into the trash
  (rollback on any failure), THEN destroy the scene metadata — every step
  before the Stash write is undoable. Trash lives BESIDE the library
  (`<parent>/.forage-trash/<date>/…`), same filesystem, outside Stash's
  scan. Daily TTL sweep (default 7d, `FORAGER_TRASH_TTL`, 0 disables),
  every final unlink journalled. `POST /destructions/{id}/restore` reverses
  a trashed entry's exact moves and rescans. The 172-file class of incident
  is now one API call per entry.
- ☑ **Dry-run mode** for the automatic path: `packDedupKeep` gained the
  `log-only` value that journals what dedup *would* destroy without acting.
  Recommended default for new installs' first week.

Acceptance: `git grep 'SceneDestroy\|os.RemoveAll' internal/` hits only the
façade and the placer's own temp-file handling; a deleted file is
recoverable from trash for the TTL; the journal reproduces every deletion in
a soak week.

## Phase 2 — adversarial verification

- ◐ CI runs gofmt + vet + full suite. Add:
  - ☑ `go test -race ./...` in CI (verified green in a Linux container).
  - ☑ **Native fuzzing** for the parsing surfaces that eat hostile input:
    `Tokenize`, `ExtractJAVCodes`, the date reader, `pathmap`. Seed corpora
    from real release names; short fuzz in CI, long fuzz nightly.
  - ☐ **Matcher regression gate**: a small *synthetic* corpus committed to
    the repo (the real one stays private per policy), bench in CI, fail on
    P@1 drop beyond an epsilon. The private-corpus run on mini remains the
    release gate (existing rule: run matcher-bench + --verify after any
    matcher change).
- ☑ **Fault injection on the lifecycle rig.** chaos_test.go wraps every
  fake client with seeded faults (500s, garbage-with-200, truncated bodies,
  dropped connections; 14 seeds in CI) and asserts: every observed change is
  inside the transitive closure of the state machine, the row never
  vanishes, ZERO destroys, and — sharpest — CONVERGENCE: faults are
  transport-level and every transport failure is guarded, so once the storm
  stops the grab must end confirmed. Anything else is a hole in a guard.
- ☑ **State-machine invariant checker**: legalSteps in chaos_test.go is
  the enumerated spec (poller-scoped), checked as a transitive closure on
  every chaos tick. Extending it to the API surfaces remains open.

Acceptance: race + fuzz + chaos green in CI; a deliberately-introduced
guard regression (mutation test on the destroy façade) fails the build.

## Phase 3 — runtime self-defense

- ☑ **DB backups**: nightly + at-boot snapshot of the PRECIOUS tables only
  (`BackupPrecious`; 19 MB vs the 738 MB full DB), three generations,
  restorability tested. Originally scoped as full-file snapshots + one before every schema
  migration (`db.Open` runs migrations; snapshot first), rotate N. The
  config store already has rotating `.bak`s — the grabs/watches DB, which
  holds the irreplaceable state, has none. Document restore.
- ☑ **Library-health latch**: the poller stats the library root every tick
  — placement pauses while it's gone (placing onto a missing mount writes
  into the bare mount point, shadowed when the real mount returns), and the
  destroy path refuses OUTRIGHT on ErrLibraryUnavailable rather than
  falling back to a permanent Stash-side delete, because during an outage
  "file missing" stops being evidence of anything. Surfaced in /healthz as
  poller.libraryOk. UI badge remains open.
- ◐ **Fix the four residual risks** in error-handling.md: `mismatched`
  recovery SHIPPED (the reconcile pass confirms a mismatch the user
  corrected in Stash back to the predicted id; a third-id re-identify stays
  mismatched on purpose). Remaining: size-equality reclaim on CIFS (extend the reconcile
  pass — it already re-checks settled grabs — to notice a mismatched grab
  whose scene now carries the predicted id); premature-heal `RemoveAll`
  clearing `PlacedPath` on failure; awaited shutdown.
- ☑ **/healthz depth**: poller block with lastTickAt/lastTickMs +
  libraryOk (+ libraryError), so a wedged daemon or dropped mount is
  visible remotely. Journal counts remain open.
- ☐ **StashDB request budget** with backoff — protect the upstream that
  everything depends on from forage's own fan-out on big libraries.

Acceptance: kill -9 mid-tick, unmount the library mid-place, and corrupt a
response in each client — in all three cases the daemon recovers on its own
and the journal/DB explain what happened.

## Phase 4 — survive other people's machines

- ☐ **Windows-native path audit**: the binary ships for Windows now;
  placer/pathmap are exercised on Linux paths daily but Windows separators,
  drive letters and UNC paths only incidentally. Table-driven tests across
  both separators for placer, pathmap, packNeedle, sameParentDir.
- ☐ **Version matrix**: contract tests from recorded fixtures for Stash
  0.26→0.31, qBit 4.x/5.x (the Resume rename is already handled — that's
  the pattern), SAB 3.x/4.x. Document the supported floor in the README.
- ☐ **GHCR unblock** (stale package linked to the old repo; needs owner
  scopes) so `docker pull` works.
- ☐ **Bug-report ergonomics**: issue templates; a `/healthz?diag=1`
  redacted diagnostics bundle (versions, config sources with secrets
  masked, last errors) a tester can paste; FAQ for the three setup
  mistakes (wrong container path, different filesystems, category save
  path).
- ☐ **Panic surfacing**: poller panics are recovered and logged; persist
  the last one and badge it in the UI so a silent nightly crash-loop is
  visible.

## Phase 5 — the standard that keeps it true

- ☐ **Two-keys rule, codified**: any irreversible action requires two
  *independent* confirmations of state (e.g. file-count + kept-side-alive),
  documented in CONTRIBUTING and enforced at the façade.
- ☐ **Definition of done** for anything touching files or destroys: journal
  coverage, an invariant test, a chaos run, and the meta-test list updated.
- ☐ **Release checklist**: suite + race + fuzz + bench green → deploy to
  mini → soak ≥3 days with journal review → tag. No same-day
  code-and-release.

---

## Sequencing

| Phase | Size | Blocks a public tester? |
|---|---|---|
| 0 | hours | **yes — do before sharing a download link** |
| 1 | days | trash + journal: no, but do before advertising widely |
| 2 | days | no |
| 3 | days | no |
| 4 | ongoing | GHCR + Windows paths: before promoting the Windows binary |
| 5 | policy | no |

The honest definition of success: not "no bugs", but *no bug can cost a user
more than an apology*. Phase 0+1 get there for data; 2+3 get there for
correctness claims; 4+5 keep it true as the code and audience grow.
