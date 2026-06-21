# Error handling: principles and plan

This is the reference for how forager handles failure. It exists because error
handling here grew reactively: each production incident (a SAB false-fail, a
qBit false-fail, a placement race) got its own targeted patch instead of fitting
a shared model. This document is the model. New error-handling code should be
held against the principles below; if a fix does not fit one of them, that is a
signal the principle (or the fix) needs rethinking, not that the fix should ship
as another special case.

## Diagnosis (2026-06-21 audit)

The "bandaid" feeling is real but localized. It traces to a single missing
abstraction:

> **No external client classifies its errors.** A transient timeout, a
> permanent rejection, and "not found" all reach the caller as either an opaque
> `error` string or a bare `(nil, nil)`. The poller cannot tell "the download is
> gone" from "qBit did not answer this tick", so it compensates structurally.

Everything that feels like a bandaid is that compensation: the
`qbitListOK`/`sabListsOK` guard booleans, the family of grace timers
(`qbitErrorGrace`, `sabInflightTimeout`, `qbitLinkTimeout`, ...), and the
near-identical SAB-vs-qBit "false-fail then revive" machinery built twice. Every
false-fail incident was the same bug surfacing through a different client,
because the fix lived in the caller instead of at the boundary.

### Layer scorecard

| Layer | State | Verdict |
|-------|-------|---------|
| External clients (qbit/sab/stash/stashdb/prowlarr) | Individually fine (timeouts set, ctx respected) but zero error classification, no sentinels, "not found" = `(nil,nil)` | Root cause |
| Poller (grab state machine) | Transient-vs-terminal decided in 5+ places, 3 mechanisms, 4 timing bases; SAB/qBit grace+revive duplicated ~200 lines | Bandaid epicenter, downstream of the client gap |
| API / repo / DB / startup | Real shared vocabulary (`writeErr`, typed `grabError`, `applyGrabUpdate` CAS); correct 502/503/409/422 discipline; panic recovery complete | Solid |
| Placer / matcher / support | Placer is already stage->verify->commit (atomic copy, resume-safe, race-guarded); configstore/torrentmeta defensively written | Solid |

## Principles

These are the rules that, had they existed, would have prevented the incidents
we have patched.

1. **Classify at the boundary.** Every external call returns a *classified*
   error: transient, not-found, or rejected/permanent. Callers branch with
   `errors.Is`, never on string contents and never on `(nil, nil)`.

2. **Transient never advances to a terminal state.** A transient error means
   "unknown, retry", full stop. Never move a grab to `failed` (or any terminal
   state) on a transient error. This single rule is the entire false-fail class,
   eliminated at the source.

3. **Terminal transitions are explicit and recoverable-by-default.** Every move
   to `failed` / `mismatched` / `orphaned` should be expressible in one table
   with an explicit `recoverable` flag, so "where can a grab strand?" is
   answered by reading the table, not by archaeology. A non-recoverable terminal
   state is a deliberate choice, written down, not an emergent gap.

4. **One grace primitive, not per-incident timers.** Time-based "is it dead
   yet?" decisions use one shared clock abstraction, not a new map + const per
   client per situation.

5. **Degrade, do not abort, on partial failure.** When several sources
   contribute to a result (the matcher's StashDB queries, the cache's
   per-performer refresh), one source failing degrades the result; it does not
   discard the whole. Abort only when nothing usable remains.

6. **Side effects are idempotent and verified before recording success.**
   Placement, and any disk/client mutation, must be re-runnable and must verify
   completion before recording success. The placer already embodies this; the
   principle generalizes to any new mutation.

## Plan

Dependency-ordered. Phase 0 is done; the rest are sequenced for when there is
appetite.

### Phase 0 - the two genuine bugs (DONE)

- **`retryGrab` optimistic-lock bypass** (`api/grab.go`). It wrote with a bare
  `Update` from the handler's stale snapshot, so a poller tick during the retry
  lost the CAS and surfaced as a spurious 500 (single retry) or a silently
  skipped grab (bulk retry-all). Now routes both writes through
  `applyGrabUpdate` (Get -> apply -> CAS, retried on a stale rev). The SAB
  `AddURL` side effect stays outside the retrying closure so it cannot
  double-enqueue.
- **Matcher all-or-nothing abort** (`matcher.go`). One failed StashDB query
  discarded the candidates the other queries already found, tanking recall on a
  transient blip. Now degrades: a failure is fatal only when zero candidates
  survived (`matchOutcomeErr`). Mirrors the cache layer's existing
  minority-tolerant policy.

### Phase 1 - the keystone: client error classification

A small `clienterr` package: sentinel errors `ErrTransient`, `ErrNotFound`,
`ErrRejected`, plus a `Classify(resp, err)` helper mapping
`context.DeadlineExceeded` / `net.Error.Timeout()` / connection-refused / 5xx ->
`ErrTransient`; 404 (and GraphQL "not found") -> `ErrNotFound`; 4xx/auth ->
`ErrRejected`. Wrap it into all five clients. Replace the `(nil, nil)`
"not found" convention on the lookups (`qbit.TorrentInfo`,
`stash.FindSceneByPathContains`, `stashdb.FindScene`) with `ErrNotFound`. This
is what makes principles 1 and 2 enforceable.

### Phase 2 - collapse the poller compensation

On top of Phase 1: `xListOK` booleans become
`errors.Is(err, clienterr.ErrTransient)`; one generic grace clock replaces the
parallel `sabSeen`/`qbitErr` maps and their twin helpers; one
`classifyClientState` verdict and one `reviveGrab` replace the duplicated
SAB/qBit machinery; one `Recoverable(client)` query replaces the two copies.
Deletes ~200 lines of duplication. This is where the bandaid feel actually
disappears.

### Phase 3 - consistency cleanups

- A single error-to-HTTP mapping helper (also finishes wiring the dead
  `notConfiguredErr` type to a 503).
- A `grabByID` helper to erase the ~15 copy-pasted parse-id / get / 404 blocks.
- Await the background goroutines (poller, watch loop, cache tickers) on
  shutdown before `database.Close()`, so the exit path stops racing the DB.
- Add bounded retry + backoff to the Prowlarr search (the one client that
  observably times out and currently has zero retry), gated on `ErrTransient`.
- Uniform `%w` error wrapping so `errors.Is` works end to end.

## Known residual risks (tracked, not yet fixed)

- **Size-equality file reclaim** (`placer.go`): the copy path treats equal size
  as "already placed". On a non-atomic-rename CIFS/SMB mount a same-size partial
  could be adopted as complete. Low probability, filesystem-dependent.
- **`mismatched` has no recovery path**: a corrected StashDB match leaves the
  grab `mismatched` forever (it is out of `Active()` and no sweep revives
  mismatches). Surfaces as principle 3's `recoverable: false`.
- **Premature-heal `RemoveAll` failure** clears `PlacedPath` regardless, which
  can orphan a partial file on disk with no grab pointing at it.
- **Non-awaited shutdown** can log "database is closed" if a tick is mid-write
  when the process exits (harmless; WAL is durable).
