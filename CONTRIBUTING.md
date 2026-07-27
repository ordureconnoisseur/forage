# Contributing

forage moves and deletes people's files. That shapes everything below: the
rules are not style preferences, they are the reason the project can offer
one-click deletion without fear. PRs that weaken them will be asked to
change, however good the feature is.

## The two-keys rule

Any irreversible action requires **two independent confirmations of
state** before it runs. One signal — however convincing — is never enough
to destroy a file, because every signal in this system (a client response,
a filesystem stat, a Stash lookup) has a failure mode where it lies.

Examples already enforced, as the pattern to copy:

- **Scene destroy** (`internal/destroy`): the executed plan is the vetted
  plan (key 1: the file list was resolved and refused if >1 file per
  scene), and files are stat'd daemon-side before anything moves (key 2:
  the thing we think we're deleting is the thing that's there).
- **Seeding cull** (`internal/poller/cull.go`): the torrent must belong to
  a forage grab (key 1: `ByClientID` row exists) AND the placed file must
  stat at its recorded path (key 2: the library copy is real) before the
  client copy is retired.
- **Trash sweep** (`internal/destroy/trash.go`): only dated directories
  under the trash root (key 1: it's ours), only past the whole-day TTL
  cutoff (key 2: it's expired) — and a root stat failure aborts the sweep,
  because a dropped mount must never look like an empty trash.

The generalisation: when you find yourself writing `if <one check> {
destroy }`, stop and find the second, *independent* check — one whose
failure mode is different from the first's.

## One door for deletion

All file-destroying paths go through `internal/destroy`. The meta-test
(`internal/destroy/onedoor_test.go`) walks the tree and fails the build on
any `SceneDestroy(..., true, ...)` or `os.RemoveAll` outside its
allowlist. If your feature needs to delete something, route it through the
façade (journal, trash, refusal semantics come free); do not add an
allowlist entry without a review conversation.

## Definition of done — anything touching files or destroys

A change that moves, links, renames, or deletes files is done when:

1. **Journal coverage**: every outcome (acted, refused, failed) writes a
   `destruction_log` row via the Recorder, intent-before-act.
2. **An invariant test**: not just "the happy path works" — a test that
   pins what must *never* happen (wrong file deleted, both copies gone,
   refusal silently skipped).
3. **A chaos run**: `go test ./internal/poller/ -run Chaos` still
   converges. If your change adds a lifecycle step, teach `legalSteps`
   about it — the checker is the state-machine spec.
4. **Meta-test updated**: if you legitimately added a destroy call site,
   the onedoor allowlist change is part of the PR and called out in the
   description.

## Error classification

External-client failures are classified once (`internal/clienterr`):
`ErrTransient` / `ErrNotFound` / `ErrRejected`. The contract, from
`docs/error-handling.md`: **nothing terminal may be concluded from a
transient failure.** A timeout is not a missing download; a 503 is not a
gone scene. If you call a client, decide with `errors.Is`, never by
parsing message strings.

## Release checklist

No same-day code-and-release. A release is cut when:

1. Full suite green, including `-race` and the fuzz smoke (CI does both).
2. Any matcher change: `matcher-bench` (+`--verify`) against the private
   corpus — P@1 and verify recall must not regress.
3. Deployed to the maintainer's live instance and **soaked ≥3 days**.
4. Soak review: destruction journal shows only expected outcomes; poller
   `phasesMs` sane; `lastPanic` absent (or explained and fixed).
5. Tag `vX.Y.Z` — the release workflow builds the binaries, plugin
   bundle, and checksums.

## Practicalities

- Go code is `gofmt`-clean; CI enforces it. Run `go vet ./...` before
  pushing.
- Plugin: `cd plugin && npx tsc --noEmit && npm run build`.
- Commit messages explain *why*; the diff already says what.
- Small, single-concern PRs land fastest.
