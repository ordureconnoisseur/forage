# Matcher accuracy: what is measured, and what is not

forage identifies a release in two steps:

1. **Match**: build queries from the release name, retrieve candidate scenes
   from StashDB, score and rank them.
2. **Verify**: a pure decision over the candidates Match returned: is this
   scene *the* scene?

The quoted accuracy number (recall 0.953 on 1,570 confirmed search-grabs) is
both steps together. Until now only the second step had a regression gate.

## The two gates

| | frozen replay | full pipeline |
| --- | --- | --- |
| guards | Verify | Match **and** Verify |
| input | recorded candidates, `internal/matcher/testdata/corpus-replay.json.gz` | live StashDB |
| floors | `internal/matcher/corpus_gate_test.go` | `internal/matcher/pipeline_floors.json` |
| cost | ~6s, no network, no keys | ~26 minutes, StashDB + a live daemon |
| when | every push | weekly, against your own instance |

The frozen replay is fast because Verify is pure: given the candidates, it
touches nothing else, so recording each release's candidates once lets every
later run replay them offline. That is also its limit. **A regression in query
construction, retrieval or the confidence weights is invisible to it**, because
the candidates were recorded before the change. If Match stopped retrieving the
right scene entirely, the replay gate would stay green.

## Why the full pipeline is not a push job, or even a cloud job

Running Match for real needs three things a GitHub-hosted runner cannot have:

1. **StashDB credentials and time.** Two round trips per release; 1,570 entries
   is about 26 minutes.
2. **The daemon's SQLite caches.** `matcher.New` loads the performer and studio
   corpora out of `forager.db`. Without them entity recognition does nothing,
   so a bench on an empty database measures a matcher that does not exist.
3. **The daemon's grab history.** The corpus is built from confirmed grabs via
   the daemon's `/grabs` API. That daemon is on a tailnet, not the public
   internet, and the corpus itself is real scene ids from a real library.

So the scheduled workflow (`.github/workflows/matcher-bench.yml`) does the half
it honestly can (re-runs the frozen gate, and checks the recording has not
gone stale) and states plainly in its run summary when the other half did not run.
The full pipeline is measured either by hand or from a self-hosted runner.

## Running the full pipeline

From a machine that can reach the forage container:

```sh
make bench            # measure and gate
make bench-refresh    # ... and re-record the committed replay fixture
```

If docker lives on another host:

```sh
FORAGE_BENCH_HOST=mini make bench
```

The script builds a fresh corpus from confirmed grabs, runs live Match+Verify
over it, prints recall / clean / false-verify rate against the recorded floors,
and exits non-zero on a breach. Other knobs are documented at the top of
`scripts/matcher-bench.sh`.

**`-include-downloads` must stay off.** It folds qBit/SAB download names into
the corpus. Those are post-renamed *filenames*; in production the matcher only
ever sees Prowlarr release titles at search time. Every verifier threshold was
originally fitted against a filename corpus, and that is why the reported
accuracy was wrong by a factor for weeks. The script never passes the flag.

## The floors

`internal/matcher/pipeline_floors.json` is embedded into the bench binary (it
runs as `docker exec forager /matcher-bench`, where there is no repo tree to
read a file from). It holds:

- `min_entries`: an absolute count, because a corpus that quietly collapsed to
  40 rows sails past every *rate* below it.
- `min_recall`, `min_clean`, `max_false_verify_rate`: rates, because the
  corpus is rebuilt each run and grows as you grab more, so an absolute cap on
  false verifies would tighten itself into a false alarm.

Slack is deliberately wider than the replay gate's. The replay gate scores a
fixed recording, so its numbers are deterministic; this one depends on StashDB,
a third party that edits and deletes scenes. A gate that cries wolf is a gate
that gets muted.

`clean` is the honest single number: the fraction of releases that verified the
right scene **and nothing else**. An entry where the correct scene verifies
alongside two wrong ones has not been identified.

Ratchet the floors when a change earns it, in the same commit as the run that
justifies it.

## Refreshing the frozen fixture

A frozen fixture is a snapshot of a moving distribution. StashDB gains and
edits scenes; the corpus is rebuilt from your grabs, whose mix of indexers and
release groups shifts. Left alone long enough, the per-push gate becomes a
benchmark of input nobody sees any more, still passing.

`make bench-refresh` re-records it. The run writes two files:

- `internal/matcher/testdata/corpus-replay.json.gz`: the candidate recording.
- `internal/matcher/testdata/corpus-replay.meta.json`: the sidecar holding that run's
  numbers and the verifier config that produced them.

The sidecar is what the gate asserts against, so a refresh is one command
rather than a fixture edit plus a hand-copy of four constants into
`corpus_gate_test.go`. Three tests keep it honest:

- `TestCorpusFixtureMatchesTheLiveRun`: replaying the fixture under the
  recorded config must reproduce the live run's numbers. If it does not, every
  offline experiment is measuring a different verifier from the one that ran.
- `TestFixtureSidecarDescribesEveryVerifierKnob`: the recorded config must
  name every `VerifyConfig` field. Add a knob without re-recording and this
  says so, instead of the recording silently using that field's zero value.
- `TestReplayFixtureIsNotStale`: skipped unless `FORAGE_FIXTURE_MAX_AGE_DAYS`
  is set. The weekly workflow sets it; push CI does not, because a test that
  turns red on a calendar date breaks the build for someone who changed
  nothing, and a build that goes red for no reason is one people learn to force
  through.

`bench-refresh` will not write the fixture if the run breached a floor.
Recording a regressed run as the new baseline is how a gate stops being a gate.

## Wiring up the self-hosted runner (optional)

Register a runner on the host with the forage container, then set two repo
variables:

- `BENCH_RUNNER`: the runner label. Its presence is what enables the
  `full-pipeline` job; unset, the job is skipped rather than queueing for a day
  against a runner that does not exist.
- `BENCH_CONTAINER`: container name, if it is not `forager`.

The runner needs docker access and nothing else: the credentials the bench uses
are already in the running container's environment.
