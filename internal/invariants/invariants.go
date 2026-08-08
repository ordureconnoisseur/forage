// Package invariants asserts the joins forage's own data model implies, and
// reports every row where one does not hold.
//
// Why this exists at all. In a single day of reading the live database, four
// separate gaps turned up, and all four had the same shape: forage KNEW
// something and never acted on it.
//
//   - 30% of grabs were filed under Unsorted with an empty performer_name,
//     while the scene's full cast sat in watches.performers, one join away.
//   - 201 grabs sat under Unsorted whose scene Stash had ALREADY identified.
//     forage had recorded the cross-id and never revisited the folder.
//   - Adopted downloads were never put through the matcher, which forage
//     owns and had measured at 0.953 on exactly that kind of filename.
//   - The pack code path had done the right thing for years. The single-file
//     path simply never got it.
//
// Every one was found by a human reading the database on a hunch, and nothing
// in the project looked for the class. That is why four accumulated silently,
// and fixing the four does not fix the property that produced them.
//
// So this package makes the joins executable. Each invariant is a statement
// about rows that should not exist, written as the query that finds them; a
// violation names the rows and says what is inconsistent about them.
//
// # It never writes
//
// Nothing here mutates. No repair, no backfill, not even a cursor persisted
// to `meta` (the bounded checks rotate an in-memory offset instead, which
// costs a repeat of the head of the set after a restart and nothing else).
// Repair is a separate decision carrying separate risk, and a checker that
// fixes things is a checker nobody can trust to tell the truth about how much
// is broken.
//
// # Cost
//
// It runs against a live SQLite file next to the daemon's own writes, with
// ~865 grabs and ~1,038 watches, and a Stash library of ~126,000 scenes. The
// SQL invariants are whole-table scans of tables in the low thousands, which
// is microseconds. The two expensive ones are bounded and rotate:
// checkPlacedPaths does one os.Stat per row over a batch, and checkStashScenes
// does one Stash round-trip per row over a much smaller batch. Nothing here
// sweeps the Stash library; every Stash question is asked about one cross-id
// forage already recorded.
package invariants

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"
)

// Violation is one offending row, named well enough to act on without a
// second query: which table, which row, and what is inconsistent about it.
type Violation struct {
	// Kind is the subject's table ("grab", "watch", "pack_duplicate") or
	// "client_id" for the grouped duplicate check, whose subject is a
	// download-client id rather than a single row.
	Kind string `json:"kind"`
	// ID is the primary key within Kind, as text (grab ids are integers,
	// watch ids are StashDB uuids).
	ID string `json:"id"`
	// Detail says what is inconsistent, in words, including the values that
	// make it inconsistent.
	Detail string `json:"detail"`
}

// Result is one invariant's verdict for one run.
type Result struct {
	// Name is the stable identifier, "<table>.<condition>". Stable because
	// the /healthz summary is keyed by it and health checks alert on it.
	Name string `json:"name"`
	// Statement is the assertion in words: what SHOULD be true.
	Statement string `json:"statement"`
	// Count is how many violating rows this run found. For the bounded
	// checks that is within the batch scanned, not across the whole table.
	// Scanned says how wide the batch was.
	Count int `json:"count"`
	// Scanned is how many rows the check examined; set only by the bounded
	// checks, where it is the difference between "clean" and "barely looked".
	Scanned int `json:"scanned,omitempty"`
	// Superseded is rows a check found technically failing but explained by
	// something routine, and therefore deliberately not counted. Reported so
	// the number is visible rather than silently swallowed: a check that
	// quietly excuses rows is a check nobody can audit.
	Superseded int `json:"superseded,omitempty"`
	// Samples is up to SampleLimit violations. Truncated rather than
	// complete: a report is for triage, and the count carries the scale.
	Samples []Violation `json:"samples,omitempty"`
	// Skipped explains why the check did not run (mount down, Stash not
	// configured). A skipped check is NOT a passing one, and saying so is
	// the difference between "nothing is wrong" and "nothing was looked at".
	Skipped string `json:"skipped,omitempty"`
	// Error is set when the check itself failed (a bad query, a Stash
	// error that isn't per-row). Same reasoning as Skipped.
	Error string `json:"error,omitempty"`
}

// Report is one full pass over every invariant.
type Report struct {
	RanAt      int64    `json:"ranAt"`
	DurationMs int64    `json:"durationMs"`
	Violations int      `json:"violations"`
	Failing    []string `json:"failing"`
	Results    []Result `json:"results"`
}

// Summary is the path-free digest for /healthz, which is UNAUTHENTICATED:
// counts and invariant names only. Sample details carry filesystem paths and
// release titles and stay behind auth (GET /invariants, /diag).
func (r *Report) Summary() map[string]any {
	if r == nil {
		return nil
	}
	failing := map[string]int{}
	skipped := []string{}
	for _, res := range r.Results {
		if res.Count > 0 {
			failing[res.Name] = res.Count
		}
		if res.Skipped != "" || res.Error != "" {
			skipped = append(skipped, res.Name)
		}
	}
	out := map[string]any{
		"ranAt":      r.RanAt,
		"durationMs": r.DurationMs,
		"violations": r.Violations,
		"checks":     len(r.Results),
	}
	if len(failing) > 0 {
		out["failing"] = failing
	}
	// Reported so a clean-looking summary can't hide a check that never ran.
	if len(skipped) > 0 {
		out["notChecked"] = skipped
	}
	return out
}

// SceneIndex is the bounded Stash lookup checkStashScenes needs: given a
// stash-box endpoint and one StashDB cross-id, how many local scenes carry
// it. Deliberately this narrow, because the checker must not be able to
// sweep the library, and an interface that can only ask about one id cannot.
type SceneIndex interface {
	// Endpoint returns the stash-box endpoint cross-ids are recorded
	// against, or "" when none is configured (the check then skips).
	Endpoint(ctx context.Context) (string, error)
	// CountScenes returns how many local scenes carry stashDBID.
	CountScenes(ctx context.Context, endpoint, stashDBID string) (int, error)
}

const (
	// sampleLimit caps samples per invariant. Enough rows to see the
	// pattern; a report is triage, and Count carries the scale.
	sampleLimit = 20

	// placedPathBatch is how many placed grabs one run stats. Sized off the
	// reconcile pass's own scan batch (400 rows of os.Stat per 15 minutes,
	// measured acceptable against the live NAS mount) with headroom, since
	// this runs a quarter as often.
	placedPathBatch = 500

	// stashSceneBatch is how many cross-ids one run asks Stash about. Far
	// smaller than placedPathBatch because each row is a network round-trip
	// into the same Stash the poller is using for real work. Matches the
	// reconcile passes' own per-pass Stash cap.
	stashSceneBatch = 40

	// confirmStampWindow scopes the missing-confirmed_at check to recently
	// touched rows. Rows predating the confirmed_at column legitimately hold
	// 0 (ConfirmedUnlinked coalesces to updated_at for exactly that reason),
	// so an unscoped check would report a permanent, unfixable backlog and
	// train everyone to ignore it.
	confirmStampWindow = 30 * 24 * time.Hour

	// deferredStallWindow is how long past its retry time a deferred grab
	// may sit before it counts as stranded. Deliberately generous: the
	// retry loop deliberately holds retries while the download client is
	// unreachable, so a real outage must not light this up. Two days is
	// longer than any outage that gets tolerated.
	deferredStallWindow = 48 * time.Hour
)

// Checker runs the invariant suite. Construct with New and call Run on a
// schedule; Last returns the most recent report for the API layer.
type Checker struct {
	db  *sql.DB
	log *slog.Logger

	// stat probes whether a path exists. A field so tests can seed a
	// missing file without touching a real filesystem.
	stat func(string) error

	// scenes is the bounded Stash lookup; nil disables the cross-id check
	// (which then reports itself Skipped rather than passing).
	scenes SceneIndex

	// libraryOK reports whether the library mount is visible. Nil means
	// "assume visible". With the mount gone EVERY placed_path stats
	// missing, so the filesystem check must skip rather than report the
	// whole table as broken. Same latch reconcileMovedFiles uses to avoid
	// repointing the table during an outage.
	libraryOK func() bool

	// Rotating offsets for the two bounded checks. In memory on purpose:
	// persisting them would mean this package writes to the database, and
	// the only cost of losing them is re-checking the head of a set.
	placedCursor int
	stashCursor  int

	mu   sync.Mutex
	last *Report
	// prev is the last run's per-invariant counts, so Run can log the
	// TRANSITION (clean → failing) rather than only the level. A number
	// that has been non-zero for a month reads as background noise; the
	// tick it first moves is the thing worth waking up for.
	prev map[string]int
}

// New builds a Checker over the daemon's existing database handle.
//
// db MUST be the daemon's own *sql.DB. Opening a second pool against a live
// SQLite file is how WAL corruption happens, and this package has no reason
// to want its own connection: every query it runs is a read.
func New(db *sql.DB, log *slog.Logger) *Checker {
	if log == nil {
		log = slog.Default()
	}
	return &Checker{
		db:  db,
		log: log,
		stat: func(p string) error {
			_, err := os.Stat(p)
			return err
		},
	}
}

// WithStash attaches the bounded Stash lookup. Without it the cross-id
// invariant reports itself skipped.
func (c *Checker) WithStash(s SceneIndex) *Checker { c.scenes = s; return c }

// WithLibraryHealth attaches the mount latch that gates the filesystem check.
func (c *Checker) WithLibraryHealth(fn func() bool) *Checker { c.libraryOK = fn; return c }

// Last returns the most recent report, or nil before the first run.
func (c *Checker) Last() *Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// Run executes every invariant once and records the report. It never
// returns an error: a broken check is data (Result.Error), not a reason to
// fail the caller's tick.
func (c *Checker) Run(ctx context.Context) *Report {
	start := time.Now()
	now := start.Unix()

	checks := sqlChecks(now)
	results := make([]Result, 0, len(checks)+2)
	for _, chk := range checks {
		results = append(results, c.runSQL(ctx, chk))
	}
	results = append(results, c.checkPlacedPaths(ctx))
	results = append(results, c.checkStashScenes(ctx))

	rep := &Report{
		RanAt:      now,
		DurationMs: time.Since(start).Milliseconds(),
		Results:    results,
		// Empty array rather than null on a clean run, matching the
		// clientErrors convention: a consumer should not have to distinguish
		// "no failures" from "field absent".
		Failing: []string{},
	}
	for _, res := range results {
		rep.Violations += res.Count
		if res.Count > 0 {
			rep.Failing = append(rep.Failing, res.Name)
		}
	}
	sort.Strings(rep.Failing)

	c.mu.Lock()
	prev := c.prev
	counts := make(map[string]int, len(results))
	for _, res := range results {
		counts[res.Name] = res.Count
	}
	c.last, c.prev = rep, counts
	c.mu.Unlock()

	c.report(rep, prev)
	return rep
}

// report logs the run. New breakage is a Warn with the offending rows
// attached; a steady count is an Info. Visibility is the point of the
// exercise: a checker nobody reads is the weak feedback loop that produced
// the four gaps, wearing a different hat.
func (c *Checker) report(rep *Report, prev map[string]int) {
	for _, res := range rep.Results {
		if res.Error != "" {
			c.log.Warn("invariant check failed to run", "invariant", res.Name, "err", res.Error)
			continue
		}
		if res.Count == 0 {
			// A previously-failing invariant reaching zero is worth a line:
			// it is the evidence that a fix landed.
			if prev != nil && prev[res.Name] > 0 {
				c.log.Info("invariant now holds", "invariant", res.Name, "was", prev[res.Name])
			}
			continue
		}
		// prev == nil is the first run of the process, where every failing
		// invariant is "new" and warning about all of them is right: nobody
		// has seen this report yet.
		fresh := prev == nil || res.Count > prev[res.Name]
		attrs := []any{"invariant", res.Name, "rows", res.Count, "statement", res.Statement}
		if len(res.Samples) > 0 {
			attrs = append(attrs, "example", res.Samples[0].Kind+" "+res.Samples[0].ID+": "+res.Samples[0].Detail)
		}
		if fresh {
			c.log.Warn("invariant violated", attrs...)
		} else {
			c.log.Info("invariant still violated", attrs...)
		}
	}
	c.log.Info("invariant check complete",
		"violations", rep.Violations, "failing", len(rep.Failing), "ms", rep.DurationMs)
}

// sqlCheck is one invariant expressed as the query that finds its
// violations. The query MUST select exactly two text columns: the subject's
// id and the detail line. One query per invariant rather than a count query
// plus a sample query, because two queries drift and a report whose count
// disagrees with its samples is worse than no report.
type sqlCheck struct {
	name      string
	kind      string
	statement string
	query     string
	args      []any
}

func (c *Checker) runSQL(ctx context.Context, chk sqlCheck) Result {
	res := Result{Name: chk.name, Statement: chk.statement}
	rows, err := c.db.QueryContext(ctx, chk.query, chk.args...)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer rows.Close()
	for rows.Next() {
		var id, detail sql.NullString
		if err := rows.Scan(&id, &detail); err != nil {
			res.Error = err.Error()
			return res
		}
		res.Count++
		if len(res.Samples) < sampleLimit {
			res.Samples = append(res.Samples, Violation{
				Kind: chk.kind, ID: id.String, Detail: detail.String,
			})
		}
	}
	if err := rows.Err(); err != nil {
		res.Error = err.Error()
	}
	return res
}

// sqlChecks is the suite. Each entry names a join forage's code already
// makes somewhere, and asserts that the rows agree with it.
func sqlChecks(now int64) []sqlCheck {
	return []sqlCheck{
		// ── The four measured gaps, generalised ──────────────────────
		{
			// Gap 1. placementPerformer (poller/place_folder.go) fills an
			// empty performer_name from watches.performers, which holds the
			// scene's whole cast captured at watch time. Before it existed,
			// 91 of 300 sampled grabs sat in Unsorted while this exact join
			// would have named a folder for every one of them.
			name:      "grab.unfiled_though_watch_knows_cast",
			kind:      "grab",
			statement: "a placed grab with no performer folder has no watch recording its scene's cast",
			query: `
				SELECT CAST(g.id AS TEXT),
				       'placed at ' || g.placed_path ||
				       ' with no performer folder, but watch ' || w.stashdb_id ||
				       ' records cast ' || substr(w.performers, 1, 120)
				  FROM grabs g
				  JOIN watches w
				    ON w.stashdb_id = COALESCE(NULLIF(g.actual_stashdb_id, ''), g.predicted_stashdb_id)
				 WHERE COALESCE(g.performer_name, '') = ''
				   AND COALESCE(g.placed_path, '') != ''
				   AND TRIM(COALESCE(w.performers, '')) NOT IN ('', '[]', 'null')
				 ORDER BY g.id`,
		},
		{
			// Gap 2. refileIdentified (poller/refile_identified.go) moves a
			// folderless grab into its identified scene's performer folder.
			// Measured before it existed: 561 grabs under Unsorted, 203
			// identified by Stash, 201 of those still folderless.
			//
			// Known benign residue: a scene Stash has identified but has NO
			// performers attached to has no folder to offer, and
			// refileIdentified correctly leaves it in Unsorted. Answering
			// that per row costs a Stash round-trip each, so it is not
			// filtered here; the count is the signal, and it should be near
			// zero rather than in the hundreds.
			name:      "grab.unfiled_though_scene_identified",
			kind:      "grab",
			statement: "a confirmed grab whose scene has a cast member the library holds is filed under a performer",
			// The NOT EXISTS is what makes this worth reading.
			//
			// The question is whether the SCENE is filed, not whether this
			// particular row carries the name. A scene grabbed twice has two
			// rows; the second is routinely placed as a hardlink beside the
			// first, so the content sits under the performer while this row
			// still shows an empty performer_name. Without the clause the
			// check reported 86 such rows on the reference library, of which
			// every actionable one turned out to be already filed. An
			// invariant nobody can act on is one everybody learns to ignore,
			// which costs more than the check is worth.
			query: `
				SELECT CAST(g.id AS TEXT),
				       'confirmed against scene ' || g.actual_stashdb_id ||
				       ' and placed at ' || g.placed_path || ' but no performer folder'
				  FROM grabs g
				 WHERE g.status = 'confirmed'
				   AND COALESCE(g.kind, 'single') != 'pack'
				   AND COALESCE(g.actual_stashdb_id, '') != ''
				   AND COALESCE(g.placed_path, '') != ''
				   AND COALESCE(g.performer_name, '') = ''
				   AND NOT EXISTS (
				         SELECT 1 FROM grabs f
				          WHERE f.actual_stashdb_id = g.actual_stashdb_id
				            AND COALESCE(f.performer_name, '') != '')
				   -- ...and only when a performer folder was ever an option.
				   --
				   -- forage refuses to file under a performer the library does
				   -- not have: a folder that is not a Stash record is one
				   -- nothing else can reason about, never shows on the
				   -- performer page and is never counted as owned. When none
				   -- of a scene's cast is local, the bin is the CORRECT
				   -- destination, so reporting it as unfiled asks for
				   -- something the placer is right to refuse. 78 of the 82
				   -- violations on the reference library were this.
				   AND EXISTS (
				         SELECT 1
				           FROM scene_performer sp
				           JOIN performer_cache pc
				             ON pc.stashdb_id = sp.performer_stashdb_id
				          WHERE sp.scene_id = g.actual_stashdb_id
				            AND COALESCE(pc.stashdb_id, '') != '')
				 ORDER BY g.id`,
		},
		{
			// Gap 3. identifyAdopted (poller/adopt_match.go) runs an adopted
			// download's release title through the matcher and files it under
			// the matched scene's own cast. Whichever route produced the
			// prediction (a watch, or the matcher), it came with a cast
			// attached, so a predicted grab with no folder means the answer
			// was reached and dropped.
			//
			// Split from the check above rather than merged with it because
			// the two failing joins live in different code and are fixed in
			// different places; the actual_stashdb_id exclusion keeps a row
			// from being reported twice.
			name:      "grab.unfiled_though_scene_predicted",
			kind:      "grab",
			statement: "a placed grab with a predicted scene has a performer folder",
			query: `
				SELECT CAST(id AS TEXT),
				       'predicted scene ' || predicted_stashdb_id ||
				       ' (confidence ' || CAST(ROUND(COALESCE(predicted_confidence, 0), 3) AS TEXT) ||
				       ') and placed at ' || placed_path || ' but no performer folder'
				  FROM grabs
				 WHERE COALESCE(predicted_stashdb_id, '') != ''
				   AND COALESCE(actual_stashdb_id, '') = ''
				   AND COALESCE(kind, 'single') != 'pack'
				   AND COALESCE(placed_path, '') != ''
				   AND COALESCE(performer_name, '') = ''
				 ORDER BY id`,
		},
		{
			// Gap 4's shape: a counter maintained by one path and not the
			// other. advancePackDistribute and the dedup step move these
			// three in lockstep, so identified may not exceed the expected
			// file count and deduped may not exceed identified. Drift means
			// a pack confirmed against a count nobody produced, and the
			// dedup decisions were taken against it.
			name:      "grab.pack_counters_inconsistent",
			kind:      "grab",
			statement: "a pack's identified count fits its file count, and its deduped count fits its identified count",
			query: `
				SELECT CAST(id AS TEXT),
				       'pack counters disagree: files=' || pack_files ||
				       ' identified=' || pack_identified || ' deduped=' || pack_deduped
				  FROM grabs
				 WHERE COALESCE(kind, 'single') = 'pack'
				   AND ((pack_files > 0 AND pack_identified > pack_files)
				        OR pack_deduped > pack_identified)
				 ORDER BY id`,
		},

		// ── State-machine joins ──────────────────────────────────────
		{
			// A grab reaches 'downloading' or 'completed' only after
			// enrichment has attached the client's id for it, and every
			// later advance looks the download up by that id. Without one
			// the poller has nothing to ask about, so the row can never
			// leave the state it is in, and Active() keeps loading it
			// every tick forever.
			name:      "grab.live_without_client_id",
			kind:      "grab",
			statement: "a grab the download client is still working on carries that client's id",
			query: `
				SELECT CAST(id AS TEXT),
				       'status=' || status || ' on client ' || COALESCE(client, '?') ||
				       ' but no client id: nothing to look the download up by'
				  FROM grabs
				 WHERE status IN ('downloading', 'completed')
				   AND COALESCE(client_id, '') = ''
				 ORDER BY id`,
		},
		{
			// Deferred grabs are deliberately outside Active(), so the
			// deferred-retry loop is the ONLY thing that moves them. One
			// sitting days past its own retry time means that loop is not
			// reaching it, and nothing else ever will. (The loop also holds
			// retries while a client is unreachable, which is why the
			// window is two days rather than an hour.)
			name:      "grab.deferred_past_its_retry",
			kind:      "grab",
			statement: "a deferred grab is retried or settled, not left parked past its own retry time",
			query: `
				SELECT CAST(id AS TEXT),
				       'deferred since attempt ' || attempts || ', retry was due ' ||
				       CAST((? - next_retry_at) / 3600 AS TEXT) || 'h ago'
				  FROM grabs
				 WHERE status = 'deferred'
				   AND next_retry_at IS NOT NULL
				   AND next_retry_at > 0
				   AND next_retry_at < ?
				 ORDER BY id`,
			args: []any{now, now - int64(deferredStallWindow.Seconds())},
		},
		{
			// The notify loop finds newly landed scenes with
			// confirmed_at > since. A confirmed grab whose stamp is 0 is
			// invisible to it forever, so the user is never told the scene
			// arrived. Scoped to recently-touched rows because rows
			// predating the column hold 0 by design.
			name:      "grab.confirmed_without_timestamp",
			kind:      "grab",
			statement: "a recently confirmed grab carries the confirm timestamp the notify sweep reads",
			query: `
				SELECT CAST(id AS TEXT),
				       'confirmed with confirmed_at=0, so the notify sweep can never see it land'
				  FROM grabs
				 WHERE status = 'confirmed'
				   AND COALESCE(confirmed_at, 0) = 0
				   AND updated_at > ?
				 ORDER BY id`,
			args: []any{now - int64(confirmStampWindow.Seconds())},
		},
		{
			// Two LIVE grabs on one download. ByClientID returns whichever it
			// finds first, so the poller advances both rows off one torrent's
			// state, and a purge of either RemoveAll's a path the other still
			// claims.
			//
			// Scoped to grabs the poller still acts on, which is the only
			// shape that can do harm. Unscoped it reported 39 groups covering
			// 90 grabs on the reference library, every one of them settled
			// (40 confirmed, 48 mismatched, 2 orphaned) and none of them
			// fixable. That is the permanent-backlog failure this package's
			// own docs rule out: a check nobody can action is a check nobody
			// reads, and it takes the actionable lines beside it down with it.
			//
			// Those 90 are not a bug. qBit deduplicates by infohash, so the
			// same torrent grabbed twice under two release names (21 of the 39
			// groups carry an identical title, the rest are the same content
			// listed by different sites) legitimately lands both rows on one
			// client id. It is worth knowing when it happens live; it is not
			// worth reporting forever once both rows have settled.
			name:      "client_id.shared_by_several_grabs",
			kind:      "client_id",
			statement: "one download-client id backs at most one grab the poller still acts on",
			query: `
				SELECT client || ':' || client_id,
				       'download-client id shared by ' || COUNT(*) || ' live grabs: ' || GROUP_CONCAT(id)
				  FROM grabs
				 WHERE COALESCE(client_id, '') != ''
				   AND status IN ('queued', 'downloading', 'completed', 'placed', 'scanned')
				 GROUP BY client, client_id
				HAVING COUNT(*) > 1`,
		},

		// ── Cross-table orphans ──────────────────────────────────────
		{
			// ResetUngrabbableAvailable self-heals these back to 'watching'
			// on every watch tick, because 'available' with no grab link is
			// un-actionable: the Watching tab offers a button that can only
			// fail, and there is nothing to dismiss. A survivor means that
			// self-heal is not running.
			name:      "watch.available_without_release",
			kind:      "watch",
			statement: "a watch offering a release has the link to grab it",
			query: `
				SELECT stashdb_id,
				       'status=available with no found_url: the grab button has nothing to grab'
				  FROM watches
				 WHERE status = 'available'
				   AND COALESCE(found_url, '') = ''
				 ORDER BY stashdb_id`,
		},
		{
			// Subscriptions tag the watches they create 'sub:<stashdb_id>',
			// and that tag is the ONLY link between the two (there is no
			// join table). Delete the subscription and its watches keep
			// searching and grabbing under a subject the user cancelled,
			// while the rail that would show them groups by a subscription
			// that no longer exists. They are invisible and active at the
			// same time.
			name:      "watch.orphan_subscription_batch",
			kind:      "watch",
			statement: "a watch tagged to a subscription batch has a subscription to belong to",
			query: `
				SELECT w.stashdb_id,
				       'batch ' || w.batch_id || ' (status ' || w.status ||
				       ') names a subscription that no longer exists'
				  FROM watches w
				 WHERE w.batch_id LIKE 'sub:%'
				   AND NOT EXISTS (
				         SELECT 1 FROM subscriptions s
				          WHERE 'sub:' || s.stashdb_id = w.batch_id)
				 ORDER BY w.stashdb_id`,
		},
		{
			// A pending review item is a question waiting on the user, and
			// the resolve endpoint answers it through its grab. With the
			// grab gone the item can never be resolved and never leaves the
			// queue, so the review count reads permanently non-zero and
			// stops meaning anything.
			name:      "pack_duplicate.orphan_grab",
			kind:      "pack_duplicate",
			statement: "a pending duplicate review points at a grab that still exists",
			query: `
				SELECT CAST(d.id AS TEXT),
				       'pending review for scene ' || d.stashdb_id ||
				       ' but grab ' || d.grab_id || ' no longer exists'
				  FROM pack_duplicate d
				 WHERE d.status = 'pending'
				   AND NOT EXISTS (SELECT 1 FROM grabs g WHERE g.id = d.grab_id)
				 ORDER BY d.id`,
		},
	}
}

// checkPlacedPaths asserts that a grab claiming a library copy has one.
//
// A stale placed_path is not cosmetic. The seeding cull refuses to retire a
// torrent whose library copy it cannot verify, so those torrents seed with no
// way out; and a purge RemoveAll's the recorded path, finds nothing, and
// reports success while the real file survives untracked. reconcileMovedFiles
// repairs the subset it can prove; this counts the whole set, repairable or
// not, which is the number that says whether the repair is keeping up.
//
// Bounded: one os.Stat per row over a rotating batch.
func (c *Checker) checkPlacedPaths(ctx context.Context) Result {
	res := Result{
		Name: "grab.placed_path_missing",
		Statement: "a grab recording a placed file has that file on disk, " +
			"unless another confirmed grab holds the same scene",
	}
	// With the mount gone every path stats missing and this would report the
	// entire table as broken, which is the same confident wrongness the
	// library latch exists to stop reconcileMovedFiles committing.
	if c.libraryOK != nil && !c.libraryOK() {
		res.Skipped = "library mount unavailable; every placed path would stat missing"
		return res
	}
	const where = `status = 'confirmed' AND COALESCE(placed_path, '') != ''`
	var total int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM grabs WHERE `+where).Scan(&total); err != nil {
		res.Error = err.Error()
		return res
	}
	if total == 0 {
		c.placedCursor = 0
		return res
	}
	if c.placedCursor >= total {
		c.placedCursor = 0
	}
	offset := c.placedCursor
	c.placedCursor += placedPathBatch

	rows, err := c.db.QueryContext(ctx,
		`SELECT id, placed_path,
		        COALESCE(NULLIF(actual_stashdb_id, ''), COALESCE(predicted_stashdb_id, ''))
		   FROM grabs WHERE `+where+` ORDER BY id LIMIT ? OFFSET ?`,
		placedPathBatch, offset)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	// No defer: this cursor is closed explicitly below, before any sibling
	// query runs, which is the whole point.
	// Collect first, close the cursor, THEN look for siblings.
	//
	// otherCopy runs its own query, and doing that while this cursor is still
	// open deadlocks: SQLite serialises on the connection, so the inner query
	// waits for a cursor that is waiting for it. The test hung for ten minutes
	// before this was split.
	type suspect struct {
		id    int64
		path  string
		scene string
	}
	var suspects []suspect
	for rows.Next() {
		var id int64
		var path, scene string
		if err := rows.Scan(&id, &path, &scene); err != nil {
			res.Error = err.Error()
			rows.Close()
			return res
		}
		res.Scanned++
		if c.stat(path) == nil {
			continue
		}
		suspects = append(suspects, suspect{id, path, scene})
	}
	if err := rows.Err(); err != nil {
		res.Error = err.Error()
	}
	rows.Close()

	for _, sp := range suspects {
		// Superseded, not lost: another confirmed grab holds this same scene
		// and its file is there, which is what an upgrade looks like. Routine,
		// and counting it here buried the rows that meant a file had genuinely
		// gone inside the ones that meant it had been replaced.
		if sp.scene != "" && c.otherCopy(ctx, sp.scene, sp.id) {
			res.Superseded++
			continue
		}
		res.Count++
		if len(res.Samples) < sampleLimit {
			res.Samples = append(res.Samples, Violation{
				Kind: "grab", ID: fmt.Sprint(sp.id),
				Detail: "placed_path " + sp.path +
					" is not on disk, and no other confirmed grab holds this scene",
			})
		}
	}
	return res
}

// otherCopy reports whether some OTHER confirmed grab holds this same scene
// with a file that is actually on disk.
//
// This is what separates "the file is gone" from "a different release of this
// scene replaced it". The second is routine: forage grabs an upgrade, the
// dedup removes the superseded file, and the old row keeps pointing at a path
// nobody deleted maliciously. Reporting both as one number hid the four rows
// that meant the first inside a count that was mostly the second.
//
// Only ever called for a row whose own file already stat-ed missing, so the
// extra query and stats are paid on the rare case, not the sweep.
func (c *Checker) otherCopy(ctx context.Context, scene string, exclude int64) bool {
	rows, err := c.db.QueryContext(ctx, `
		SELECT placed_path FROM grabs
		 WHERE status = 'confirmed' AND COALESCE(placed_path, '') != ''
		   AND id != ?
		   AND COALESCE(NULLIF(actual_stashdb_id, ''), COALESCE(predicted_stashdb_id, '')) = ?
		 LIMIT 8`, exclude, scene)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if rows.Scan(&p) != nil || p == "" {
			continue
		}
		if c.stat(p) == nil {
			return true
		}
	}
	return false
}

// checkStashScenes asserts that a grab confirmed against a scene still has
// that scene in Stash.
//
// Confirmation is forage's claim that the file landed and Stash matched it.
// If the scene is later destroyed (a dedup, a hand cleanup, a library rebuild)
// the grab keeps making the claim, and everything downstream believes it: the
// cull thinks a library copy exists, the moved-file repair asks Stash where a
// scene is and gets nothing, the Grabs tab shows a match to a scene you cannot
// open.
//
// Bounded hard. One round-trip per row over a rotating batch of
// stashSceneBatch, each asking about ONE cross-id forage already recorded.
// The library is ~126,000 scenes and is never swept.
func (c *Checker) checkStashScenes(ctx context.Context) Result {
	res := Result{
		Name:      "grab.confirmed_scene_gone_from_stash",
		Statement: "a grab confirmed against a scene still has that scene in Stash",
	}
	if c.scenes == nil {
		res.Skipped = "stash not configured"
		return res
	}
	endpoint, err := c.scenes.Endpoint(ctx)
	if err != nil {
		res.Skipped = "stash-box endpoint unavailable: " + err.Error()
		return res
	}
	if endpoint == "" {
		res.Skipped = "no stash-box endpoint configured in Stash; cross-ids can't be resolved"
		return res
	}

	const where = `status = 'confirmed' AND COALESCE(actual_stashdb_id, '') != ''`
	var total int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM grabs WHERE `+where).Scan(&total); err != nil {
		res.Error = err.Error()
		return res
	}
	if total == 0 {
		c.stashCursor = 0
		return res
	}
	if c.stashCursor >= total {
		c.stashCursor = 0
	}
	offset := c.stashCursor
	c.stashCursor += stashSceneBatch

	rows, err := c.db.QueryContext(ctx,
		`SELECT id, actual_stashdb_id FROM grabs WHERE `+where+` ORDER BY id LIMIT ? OFFSET ?`,
		stashSceneBatch, offset)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	type row struct {
		id    int64
		scene string
	}
	var batch []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.scene); err != nil {
			rows.Close()
			res.Error = err.Error()
			return res
		}
		batch = append(batch, r)
	}
	rowsErr := rows.Err()
	// Closed before the network loop below: holding a SQLite read cursor
	// open across stashSceneBatch round-trips would pin a connection from
	// the daemon's shared pool for as long as Stash takes to answer.
	rows.Close()
	if rowsErr != nil {
		res.Error = rowsErr.Error()
		return res
	}

	var lastErr error
	for _, r := range batch {
		if ctx.Err() != nil {
			break
		}
		n, err := c.scenes.CountScenes(ctx, endpoint, r.scene)
		if err != nil {
			// Stash being unreachable is not a violation. Recording the
			// error keeps a run that looked at nothing from reading as a
			// run that found nothing.
			lastErr = err
			continue
		}
		res.Scanned++
		if n > 0 {
			continue
		}
		res.Count++
		if len(res.Samples) < sampleLimit {
			res.Samples = append(res.Samples, Violation{
				Kind: "grab", ID: fmt.Sprint(r.id),
				Detail: "confirmed against scene " + r.scene + ", which Stash no longer has",
			})
		}
	}
	if res.Scanned == 0 && lastErr != nil {
		res.Skipped = "stash unreachable: " + lastErr.Error()
	}
	return res
}
