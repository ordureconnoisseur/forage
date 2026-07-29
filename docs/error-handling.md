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

### Phase 1 - the keystone foundation: client error classification (DONE)

A small `clienterr` package: sentinel errors `ErrTransient`, `ErrNotFound`,
`ErrRejected`, plus `Transport(label, err)` (every `http.Client.Do` failure is
connection-level, so it wraps `ErrTransient` while preserving the original for
`errors.Is(context.DeadlineExceeded)`) and `Status(label, code, body)` (404 ->
`ErrNotFound`; 408/429/5xx -> `ErrTransient`; other 4xx -> `ErrRejected`; nil for
2xx). Wired into the one shared GraphQL transport (`gqlclient.Do`, covering Stash
+ StashDB) and the three REST clients (qbit, sab, prowlarr) at every transport
and status site.

This phase is deliberately ADDITIVE: it only wraps the errors clients already
return, so no caller's behavior changes (nothing does `errors.Is` on a client
error yet, and string logging still works). It is the foundation that makes
principles 1 and 2 enforceable in Phase 2.

The `(nil, nil)` "not found" contract change on the lookups
(`qbit.TorrentInfo`, `stash.FindSceneByPathContains`, `stashdb.FindScene`) is
NOT in this phase: several callers (the poller's confirm path especially) treat
`(nil, nil)` as a normal "not indexed yet, keep waiting" state, so flipping it to
`ErrNotFound` must happen with its consumers, in Phase 2.

### Phase 2 - collapse the poller compensation (DONE)

The duplicated SAB/qBit grace+revive machinery is now one path:

- One grace clock (`p.grace` + `graceElapsed`/`graceClear`) replaces the
  parallel `sabSeen`/`qbitErr` maps and their four twin helpers. Both clients
  ask the same question ("how long has this bad condition held?") and a grab
  belongs to exactly one client, so a single map keyed by grab id is safe. The
  distinct durations (`sabInflightTimeout`, `qbitErrorGrace`) stay as the
  per-condition arguments; only the mechanism unified (principle 4).
- One `repo.Recoverable(client)` replaces the byte-identical `RecoverableSab` /
  `RecoverableQbit`.
- One `applyRevive` core (clear grace + PlaceError, CAS-write, log) backs the
  two thin client wrappers `reviveSabGrab` / `reviveQbitGrab`, which now only
  compute their client-specific status/completion/name.

This is a behavior-preserving refactor: the SAB clock now measures from
first-absence rather than last-contact, a sub-one-tick difference on a 45-minute
timeout, otherwise identical. The full existing test suite (the SAB + qBit
grace/revive tests) passes unchanged, which is the regression proof.

Two items originally bundled here were dropped after closer reading:

- **`xListOK` was left as-is.** It already skips the per-tick client refresh on
  ANY list-fetch error, which is the correct safe behavior — a non-transient
  error still means "no valid list this tick, don't misread every grab as gone."
  Narrowing it to only-transient would be wrong, not better.
- **The `(nil, nil)` -> `ErrNotFound` flip moved to Phase 3.** The poller's
  confirm path deliberately treats `(nil, nil)` from `FindSceneByPathContains`
  as "not indexed yet, keep waiting" (it drives the placed -> scanned -> orphaned
  timing); the lookups aren't in the poller's hot path at all. The change is low
  value (current handling is correct) and high risk (rewires live confirm
  branching), so it belongs with a careful, separately-tested pass.

### Phase 3 - actively consume the classification + consistency cleanups (DONE)

- **Prowlarr search retry** (the first real consumer of the classification):
  `SearchScoped` retries up to 3x with backoff + jitter, gated on
  `errors.Is(err, clienterr.ErrTransient)` and only for FAST failures (a slow
  one is a slow indexer, not a blip). The one client that observably times out
  now rides a blip out instead of dropping the query.
- **`(nil, nil)` -> `ErrNotFound`** on `qbit.TorrentInfo`,
  `stash.FindSceneByPathContains`, `stashdb.FindScene`, with every consumer
  updated. The skip/collapse consumers were already correct; the 502/422/404
  handlers now guard with `!errors.Is(err, ErrNotFound)`; the poller confirm
  path treats `ErrNotFound` as "not indexed yet, keep waiting" exactly as the
  old `(nil, nil)` did. Behavior-preserving, proved by the lifecycle tests.
- **`writeMappedErr` helper** folds the four copy-pasted `errors.As(&grabError)`
  ladders into one and maps `notConfiguredErr` -> 503 (the `Matcher()` callers
  already 503'd it directly, so this just centralizes + future-proofs).
- **`pathInt64` + `grabByID` helpers** collapse the repeated parse-id / get /
  404 boilerplate across the grab handlers.
- **Shutdown waits for background goroutines** (`waitForBackground`, bounded
  3s) before the deferred `database.Close()`, so a tick mid-write no longer
  races a closed DB on exit.

Not done: an exhaustive `%w` sweep. Phase 1 already wrapped the client layer
(where `errors.Is` matters); the remaining bare `fmt.Errorf` strings are in
leaf spots that nothing matches on, so a blanket sweep is churn without payoff.

## Known residual risks (tracked, not yet fixed)

- **Size-equality file reclaim** (`placer.go`): the copy path treats equal size
  as "already placed". On a non-atomic-rename CIFS/SMB mount a same-size partial
  could be adopted as complete. Low probability, filesystem-dependent.
- **`mismatched` has no recovery path**: a corrected StashDB match leaves the
  grab `mismatched` forever (it is out of `Active()` and no sweep revives
  mismatches). Surfaces as principle 3's `recoverable: false`.
- **Shutdown is bounded, not unbounded-await.** This entry used to say
  shutdown was *not* awaited; that stopped being true. `main` tracks every
  background goroutine in a WaitGroup and `waitForBackground` joins it,
  capped at 3s so a wedged goroutine cannot hang past docker's stop grace.
  The residual is only that cap: a tick can run ~47s on the reference
  instance, so a mid-tick SIGTERM has to unwind through it. In practice it
  unwinds fast, because nearly all of that time is ctx-aware network I/O
  that fails immediately on cancel. Measured with a real SIGTERM 20s in:
  clean "poller stopping", zero "did not stop in time" warnings, zero
  "database is closed" errors. Caveat on that measurement: the probe daemon
  was unconfigured, so its tick did little work; the worst case (SIGTERM
  during a fully-loaded tick) is still argued rather than measured. Worst
  outcome remains a logged error, since the WAL is durable.
