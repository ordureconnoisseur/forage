# The invariant checker

## Why

In one day of reading the live database, four separate gaps turned up. All
four were the same bug, not four bugs:

1. 30% of grabs were filed into an "Unsorted" folder because `performer_name`
   was empty, while the scene's cast sat in `watches.performers`, one join
   away.
2. 201 grabs sat in Unsorted whose scene Stash had **already** identified.
   forage had recorded the cross-id and never revisited the folder.
3. Adopted downloads were never run through the matcher, despite forage
   owning a matcher measured at 0.953 on exactly that kind of filename.
4. The pack code path had done the right thing for years. The single-file
   path simply never got it.

Every one was a join that was never made, and every one was found by a human
reading the database on a hunch, weeks or months after it started leaking.
Nothing in the project looked for that class, which is why four of them
accumulated silently. Fixing the four does not fix the property that produced
four.

`internal/invariants` is that property, made executable. Each invariant is a
statement about rows that should not exist, written as the query that finds
them, and each violation names the rows and what is inconsistent about them.

## Rules it plays by

**It never writes.** No repair, no backfill, not even a persisted cursor.
Repair is a separate decision with separate risk, and a checker that fixes
things is a checker nobody can trust to tell the truth about how much is
broken. `TestCheckerNeverWrites` asserts this byte for byte: it seeds data
that violates nearly everything, runs a full pass, and diffs every table.

**It uses the daemon's own handles.** The checker takes the daemon's
`*sql.DB`; opening a second pool against a live SQLite file is how WAL
corruption happens. Its window onto Stash is an interface that can only ask
about one cross-id at a time, so it is structurally incapable of sweeping the
126,000-scene library.

**A skipped check is not a passing one.** With the library mount down every
`placed_path` stats missing, so that check refuses to run rather than report
the whole table as broken; with Stash unreachable the cross-id check reports
nothing rather than "every scene is gone". Both say so, and the names appear
under `notChecked` in the digest, because "nothing is wrong" and "nothing was
looked at" must not look alike.

## Where to read it

| Surface | Auth | What it carries |
| --- | --- | --- |
| `GET /healthz` &rarr; `invariants` | none | `ranAt`, `violations`, `failing` (name &rarr; count), `notChecked`. Counts and names only: samples quote library paths and release titles, and this endpoint is public. |
| `GET /invariants` | yes | The full report: every assertion, its verdict, and up to 20 sample rows each with what is wrong. |
| `GET /diag` &rarr; `invariants` | yes | The same full report, inside the paste-into-a-bug-report bundle. |
| logs | n/a | One `WARN` per invariant the moment its count rises (with an example row), `INFO` while it holds steady, and `INFO` when a previously-failing invariant reaches zero. |

The suite runs inside the poller tick on a 30-minute cadence, spliced in above
the nothing-active early return: an idle library is precisely when a silent
inconsistency is the only thing left to find.

## The invariants

Derived from `internal/poller/grabs.go`, `internal/grabs`, `internal/watches`
and `internal/db/schema.sql`. Each names a join forage's code already makes
somewhere, and asserts the rows agree with it.

### The four gaps, generalised

| Name | Asserts | What breaks when it fails |
| --- | --- | --- |
| `grab.unfiled_though_watch_knows_cast` | a placed grab with no performer folder has no watch recording its scene's cast | gap 1. Files pile up in the junk drawer while the answer sits in `watches.performers` |
| `grab.unfiled_though_scene_identified` | a confirmed grab whose scene Stash identified has a performer folder | gap 2. `refileIdentified` is not reaching them |
| `grab.unfiled_though_scene_predicted` | a placed grab with a predicted scene has a performer folder | gap 3. The matcher answered and the answer was dropped |
| `grab.pack_counters_inconsistent` | a pack's identified count fits its file count, and its deduped count fits its identified count | gap 4's shape: counters one path maintains and another does not. Dedup destroys files on a bad tally |

`grab.unfiled_though_scene_identified` has one known benign residue: a scene
Stash has identified but attached no performers to has no folder to offer, and
`refileIdentified` correctly leaves it in Unsorted. Answering that per row
costs a Stash round-trip each, so it is not filtered out. The count is the
signal, and it should be near zero rather than in the hundreds.

### State machine

| Name | Asserts | What breaks when it fails |
| --- | --- | --- |
| `grab.placed_path_missing` | a grab recording a placed file has that file on disk | the seeding cull refuses to retire a torrent it cannot verify, and a purge deletes nothing while reporting success |
| `grab.confirmed_scene_gone_from_stash` | a grab confirmed against a scene still has that scene in Stash | the grab keeps claiming a library copy that no longer exists, and everything downstream believes it |
| `grab.live_without_client_id` | a grab the client is still working on carries that client's id | the poller loads it every tick forever with nothing to look the download up by |
| `grab.deferred_past_its_retry` | a deferred grab is retried or settled, not parked past its own retry time | deferred grabs are outside `Active()`, so one the retry loop stops reaching is stranded |
| `grab.confirmed_without_timestamp` | a recently confirmed grab carries the stamp the notify sweep reads | the user is never told the scene landed |
| `client_id.shared_by_several_grabs` | one download-client id backs at most one grab | two grabs advance off one torrent, and purging either deletes a path the other claims |

### Cross-table

| Name | Asserts | What breaks when it fails |
| --- | --- | --- |
| `watch.available_without_release` | a watch offering a release has the link to grab it | the Watching tab shows a button that can only fail, with nothing to dismiss |
| `watch.orphan_subscription_batch` | a watch tagged to a subscription batch has a subscription to belong to | watches keep searching and grabbing for a cancelled subscription, invisible in the rail that groups by it |
| `pack_duplicate.orphan_grab` | a pending duplicate review points at a grab that still exists | an unresolvable item sits in the queue forever and the pending count stops meaning anything |

## Cost

The SQL invariants scan tables in the low thousands (865 grabs, 1,038 watches
on the reference instance), which is microseconds. The two expensive ones are
bounded and rotate an in-memory cursor:

- `grab.placed_path_missing`: 500 rows of `os.Stat` per pass. Sized off
  `reconcileMovedFiles`, which does 400 per 15 minutes against the same NAS
  mount, with headroom because this runs a quarter as often.
- `grab.confirmed_scene_gone_from_stash`: 40 Stash round-trips per pass, the
  same per-pass Stash cap the reconcile passes use, each about a single
  cross-id forage already recorded.

`Result.Scanned` reports how wide the batch actually was, so a bounded check
that found nothing can be told apart from one that barely looked.

## Adding one

Most invariants are a `sqlCheck`: a name, a subject kind, the assertion in
words, and one query selecting `(id, detail)`. One query rather than a count
plus a sample query, because two queries drift and a report whose count
disagrees with its samples is worse than no report.

Two rules for a new entry:

- It must be justifiable from the code, not from a hunch. Name the function
  whose join it asserts, and say in the comment what breaks in the real world
  when it fails.
- It must be quiet on correct data. Add a case to `TestInvariants` with a
  violating row **and** a clean row; the clean row is asserted against the
  whole report, not just the invariant under test. An invariant that fires on
  correct data is worse than one that misses, because a report that cries
  wolf gets ignored exactly like no report at all, and being ignored is how
  the original four survived.
