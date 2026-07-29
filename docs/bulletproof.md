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

Acceptance: every `SceneDestroy` / `os.RemoveAll` in `internal/` is either
the façade or a `TestOneDoor` allowlist entry carrying a comment saying why.
The test is the acceptance criterion, not the raw grep: legitimate scoped
removals have grown to six sites (grab purge, performer re-file, trash TTL
sweep, premature-placement heal, managed-Prowlarr installer, torrent engine
payload delete), so the grep alone reads as a regression when it isn't.
Also: a deleted file is recoverable from trash for the TTL; the journal
reproduces every deletion in a soak week.

## Phase 2 — adversarial verification

- ◐ CI runs gofmt + vet + full suite. Add:
  - ☑ `go test -race ./...` in CI (verified green in a Linux container).
  - ☑ **Dependency vulnerability gate**: blocking `govulncheck` in CI,
    added 2026-07-28 after an audit found GO-2026-5970 (infinite loop in
    x/text `norm`) reachable from `fold`, the first statement of both
    `Tokenize` and `ExtractJAVCodes`. The fuzz corpus below was hardening
    those two parsers while a known hang sat one frame deeper, inside the
    dependency they both call first: fuzzing cannot see that, and only a
    hand audit did. `go.mod` carries a `toolchain` line so the standard
    library is inside the same gate.
  - ☑ **Native fuzzing** for the parsing surfaces that eat hostile input:
    `Tokenize`, `ExtractJAVCodes`, the date reader, `pathmap`. Seed corpora
    from real release names; short fuzz in CI, long fuzz nightly.
  - ☑ **Matcher pipeline gate in CI**: pipeline_smoke_test.go runs the
    FULL Match path (tokenize → entity scan → StashDB fan-out → score →
    rank) against a synthetic corpus + fake StashDB GraphQL server, and
    pins that canonical release shapes rank their scene first. Scoped
    deliberately as a wiring gate, not an accuracy gate: a statistically
    meaningful P@1 epsilon needs a realistic corpus, and the realistic
    corpus stays private — so the accuracy gate remains the private-corpus
    matcher-bench on the live instance, codified in CONTRIBUTING's
    release checklist.
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
- ◐ **Fix the four residual risks** in error-handling.md. `mismatched`
  recovery SHIPPED (the reconcile pass confirms a mismatch the user
  corrected in Stash back to the predicted id; a third-id re-identify stays
  mismatched on purpose). Premature-heal `RemoveAll` SHIPPED: a failed
  removal now keeps `placed_path` instead of clearing it regardless, so the
  pointer to a partial it could not delete survives and the heal's own
  condition re-arms the retry on the next tick. Staying `placed` is load
  bearing there — `downloading` plus a live path is exactly what Step 2's
  "placed_path set" heal lifts back, so the pair would fight within one
  tick. Remaining: size-equality reclaim on CIFS (extend the reconcile
  pass — it already re-checks settled grabs — to notice a mismatched grab
  whose scene now carries the predicted id); awaited shutdown.
- ☑ **/healthz depth**: poller block with lastTickAt/lastTickMs +
  libraryOk (+ libraryError), so a wedged daemon or dropped mount is
  visible remotely. Journal tallies shipped too: a `destructions` block
  with per-outcome counts (all-time + last 24h; counts only — the endpoint
  is unauthenticated).
- ☑ **StashDB request budget** with backoff: every query funnels through a
  token bucket (4 req/s steady, burst 4) plus a cool-down that doubles on
  transient failures, jumps to 30s on an explicit 429, caps at 5min, and
  clears on the first success. Queue waits killed by the caller's context
  classify transient, so nothing terminal can be concluded from them.

Acceptance: kill -9 mid-tick, unmount the library mid-place, and corrupt a
response in each client — in all three cases the daemon recovers on its own
and the journal/DB explain what happened.

## Deferred — the built-in torrent engine

Removed 2026-07-28, to come back when it can be held to the standard on this
page rather than alongside it.

It was the one subsystem carrying none of the properties above. It deleted
files (`DeleteTorrent(deleteFiles=true)`) with a hand-written containment
check rather than through the Phase 1 façade; it had no chaos coverage as a
pool client and no contract fixtures, while qBit and SAB have both; and an
audit found two lifecycle bugs in it that the suite could not see, one of
which left the piece-completion lock stranded so a single transient start
failure silently disabled downloads for the rest of the process.

The cost was disproportionate to that. It pulled **216 of 298 packages** in
the build graph and 488 of 545 `go.sum` lines, for a backend dormant on any
install that has qBittorrent, which is the configuration the wizard now
recommends and the full compose pre-wires. With `govulncheck` gating CI,
that surface was also the most likely future source of red builds on
unrelated work.

What stayed, so restoring it is a pool-level change and not a rewrite:
`clientpool.TorrentClient` (the qBit-dialect interface the poller and API
consume) and `Pool.Torrents()`. What has to come back with it: the engine
package, its wiring in `main.go` / `api` / the pool, the wizard's downloads
choice, the `engine_torrents` table (deliberately not dropped from existing
databases), and this section's replacement in Phase 2 and 4.

Restoring it means, at minimum: routing its payload delete through the
destroy façade, chaos coverage as a pool client, contract fixtures, and
journalling the two orphan paths (metainfo persisted before `AddTorrent`
succeeds, so a failure leaves a torrent that re-adds on the next `Start`
with no DB row; magnet rows stranded when metadata never arrives inside the
30-minute window).

## Phase 4 — survive other people's machines

- ☑ **Windows-native path audit**: table-driven tests across both
  separators for placer, pathmap, packNeedle, sameParentDir. Found and
  fixed a real one: `Translate` rejected every file under a correctly
  matched Windows-native prefix (backslash never accepted as the path
  boundary), silently degrading scoped scans to full-library scans on the
  shipped .exe. Also verified live: daemon run on the Windows PC against
  real Stash, NTFS probes, backups.
- ◐ **Version matrix**: download clients DONE — contract tests from
  fixtures recorded against real containers (qBit 4.6.7 + 5.2.0, SAB
  3.7.2 + 4.5.1 + 5.0.4; testdata/contract/ in each client package, with
  the recording rig documented in the test headers). The recording
  IMMEDIATELY found a real bug: qBit 4.x answers 409 to editCategory when
  the savePath is unchanged, so EnsureCategory's idempotent re-save
  failed on 4.x — fixed with a read-back verification of the desired
  state. Also pinned: the 5.x start/resume rename, pausedDL→stoppedDL,
  completion_on 0 vs -1 for incomplete, SAB 5.x UUID nzo_ids, and SAB's
  status-less set_config success echo. Supported floor in the README.
  Remaining: the Stash GraphQL matrix (0.26→0.31) — needs per-version
  instances with the setup mutation driven, a separate session's work.
- ☐ **GHCR unblock** (stale package linked to the old repo; needs owner
  scopes) so `docker pull` works.
- ☑ **Bug-report ergonomics**: issue templates (version/install/diag/
  logs/journal-rows prompts); diagnostics bundle as authenticated
  `GET /diag` (versions, config field sources through the same masking as
  /config — one place to keep the secrets rule — client reachability,
  poller health, grab totals, journal tallies, last panic with stack);
  docs/troubleshooting.md covering the three setup mistakes (wrong
  container path, different filesystems, category save path), linked from
  README and the issue template.
- ☑ **Panic surfacing**: every recover site (poller tick, api background
  loops, main.go cache refreshers) persists the panic to the meta table
  (paniclog; part of the precious backup set). /healthz carries
  `lastPanic` {at, in} — value/stack stay behind auth in /diag — and the
  UI raises a dismissible banner for crashes newer than 48h (dismiss is
  per-panic, so a new crash re-raises it).

## Phase 5 — the standard that keeps it true

- ☑ **Two-keys rule, codified**: CONTRIBUTING.md documents the rule with
  the three enforced examples (destroy plan+stat, cull ownership+placed
  stat, sweep dated-dir+TTL) as the pattern to copy; the façade and
  meta-test enforce the single door.
- ☑ **Definition of done** for anything touching files or destroys:
  CONTRIBUTING.md — journal coverage, an invariant test, a chaos run, and
  the meta-test list updated as part of the same PR.
- ☑ **Release checklist**: CONTRIBUTING.md — suite + race + fuzz green,
  matcher bench on any matcher change, deploy to the live instance, soak
  ≥3 days with journal review, then tag. No same-day code-and-release.

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
