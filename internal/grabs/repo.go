// Package grabs persists Phase B grab tracking: every user-initiated
// forager → qBit add is stored as a row that the background poller
// advances through queued → downloading → completed →
// confirmed/mismatched/orphaned.
//
// Repo is intentionally thin: a few CRUD methods, no behaviour. The
// state machine lives in internal/poller.
package grabs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrStaleUpdate is returned by Update when the row changed since the caller
// loaded it (optimistic-lock miss on updated_at). The poller's tick holds a
// grab snapshot across seconds of network I/O before writing; this lets a
// concurrent API mutation (performer reassign, manual match) win instead of
// being silently clobbered by the stale tick write. The poller treats it as
// benign (re-loads next tick); API handlers re-Get + reapply + retry once.
var ErrStaleUpdate = errors.New("grabs: update lost optimistic lock (row changed since load)")

// RefusedPrefix marks a Reason that records forage's own DECISION not to
// take a release (junk content), as distinct from something that failed.
// Both writers use it — the api's pre-download screen of a fetched .torrent
// and the poller's post-download screen of a single file — so the user can
// tell refusal from failure at a glance, and so the retry paths can key on
// one string: re-driving a refused release only re-downloads the same junk.
const RefusedPrefix = "refused: "

// Grab is the in-memory shape; columns map 1:1 onto the SQLite schema.
// All optional fields are pointer-or-zero so they can be NULL on disk.
type Grab struct {
	ID int64
	// Source is the stash-box endpoint that issued this grab's scene ids.
	// "" = StashDB. Carried from the watch so enrichment asks the box the
	// id actually came from.
	Source              string
	PredictedStashDBID  string
	PredictedConfidence float64
	ReleaseTitle        string
	ReleaseSize         int64
	ReleaseIndexer      string
	DownloadURL         string
	Client              string // "qbit" | "sabnzbd"
	ClientID            string // qBit info_hash or SAB nzo_id
	ClientName          string // on-disk filename the client reports
	Category            string
	Status              string
	ActualStashDBID     string
	Reason              string
	// PerformerName is the directory under <library_root> the file is
	// placed into. Captured at /grab time so placement is predictable
	// regardless of how StashDB orders the scene's performer list.
	PerformerName string
	// PlacedPath is the final on-disk location after the placer
	// hardlinks/copies the file out of the download client's complete dir.
	PlacedPath string
	// PlaceError is the most recent placement failure reason. The
	// placer retries on subsequent ticks; the field is cleared on success.
	PlaceError  string
	GrabbedAt   int64
	UpdatedAt   int64
	CompletedAt int64
	PlacedAt    int64
	ConfirmedAt int64
	// Kind is "single" (the default — one release → one scene) or "pack"
	// (one release → many of a performer's scenes). Pack grabs follow a
	// distinct confirm path in the poller: enumerate every placed file's
	// scene, drive identify across all of them, then dedup against the
	// existing library.
	Kind string
	// Pack progress counters (0 for single grabs):
	//   PackFiles      expected video count (from the parsed .torrent at grab time)
	//   PackIdentified scenes Stash has cross-id'd to StashDB so far
	//   PackDeduped    scenes removed because the library already had them
	PackFiles      int
	PackIdentified int
	PackDeduped    int
	// Progress is the download's 0..1 completion as last seen from the
	// client; ProgressAt is the unix time it last increased. The poller
	// maintains both so stalled grabs (no progress for a while) can be
	// surfaced. Torrent-only; SAB grabs leave these at 0.
	Progress   float64
	ProgressAt int64
	// Attempts counts failed add attempts for the deferred-retry flow
	// (status "deferred"); NextRetryAt is the unix time the retry loop
	// may re-drive the add. Both zero for grabs that never deferred.
	Attempts    int
	NextRetryAt int64
	// FailKind is which stage the deferred failure happened in:
	// "indexer" (the .torrent fetch failed; a failover to another
	// indexer's release may rescue it) or "client" (the download client
	// couldn't take it; retry the same release). Empty when not deferred.
	FailKind string
	// Rev is the optimistic-lock version, bumped by Update. Carry the value
	// you loaded back into Update; if the row's rev advanced meanwhile (a
	// concurrent write), Update returns ErrStaleUpdate rather than clobber.
	Rev int64
}

// Repo is the persistence boundary. One per *sql.DB.
type Repo struct {
	db *sql.DB
}

func NewRepo(db *sql.DB) *Repo {
	return &Repo{db: db}
}

// Insert records a new grab at the moment the user clicks Grab and the
// torrent is successfully handed to qBit. status defaults to "queued"
// — the poller fills in the qBit hash + later transitions.
func (r *Repo) Insert(ctx context.Context, g Grab) (int64, error) {
	now := time.Now().Unix()
	if g.Status == "" {
		g.Status = "queued"
	}
	if g.Client == "" {
		g.Client = "qbit"
	}
	if g.GrabbedAt == 0 {
		g.GrabbedAt = now
	}
	if g.Kind == "" {
		g.Kind = "single"
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO grabs (
		  predicted_stashdb_id, predicted_confidence, release_title,
		  release_size, release_indexer, download_url,
		  client, client_id, client_name, category, status,
		  actual_stashdb_id, reason,
		  performer_name, placed_path, place_error,
		  grabbed_at, updated_at, completed_at, placed_at, confirmed_at,
		  kind, pack_files, pack_identified, pack_deduped,
		  progress, progress_at, source
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nullString(g.PredictedStashDBID), nullFloat(g.PredictedConfidence), g.ReleaseTitle,
		nullInt(g.ReleaseSize), nullString(g.ReleaseIndexer), nullString(g.DownloadURL),
		g.Client, nullString(g.ClientID), nullString(g.ClientName),
		nullString(g.Category), g.Status,
		nullString(g.ActualStashDBID), nullString(g.Reason),
		nullString(g.PerformerName), nullString(g.PlacedPath), nullString(g.PlaceError),
		g.GrabbedAt, now,
		nullInt(g.CompletedAt), nullInt(g.PlacedAt), nullInt(g.ConfirmedAt),
		g.Kind, g.PackFiles, g.PackIdentified, g.PackDeduped,
		g.Progress, g.ProgressAt, g.Source,
	)
	if err != nil {
		return 0, fmt.Errorf("grabs insert: %w", err)
	}
	return res.LastInsertId()
}

// Update writes the mutable fields back to disk and bumps updated_at.
// The poller calls this after each tick's status transition. Caller is
// responsible for setting reason / timestamps before calling. grabbed_at
// is in the SET list because retry re-arms it (the SAB register grace,
// qBit link timeout, and pickRecent window are all keyed on it); every
// caller round-trips a loaded row, so otherwise it writes back unchanged.
// The release identity columns (title/size/indexer/download_url) are in
// the SET list because the deferred-retry failover switches a grab to a
// different release of the same scene; the round-trip argument makes
// them no-ops for every other caller.
// The lifecycle timestamps follow the struct like every other column —
// they used to be COALESCE-guarded (zero meant "keep"), which made them
// impossible to CLEAR: a retry couldn't reset completed_at, so the new
// attempt was judged against the previous attempt's completion clock and
// the orphan window could elapse instantly.
// Update writes the full grab row, optimistically locked on rev: the WHERE
// matches only if the row's rev still equals the one the caller loaded, and
// the update bumps rev. A concurrent write (the poller tick holding a
// snapshot across seconds of I/O vs an API mutation) advances rev, so the
// loser matches 0 rows and gets ErrStaleUpdate instead of silently
// overwriting the winner's columns. rev is monotonic (not time-based), so
// even writes within the same instant are distinguished.
func (r *Repo) Update(ctx context.Context, g Grab) error {
	now := time.Now().Unix()
	res, err := r.db.ExecContext(ctx, `
		UPDATE grabs SET
		  release_title = ?, release_size = ?, release_indexer = ?,
		  download_url = ?, predicted_confidence = ?,
		  client_id = ?, client_name = ?, status = ?,
		  actual_stashdb_id = ?, reason = ?,
		  performer_name = ?, placed_path = ?, place_error = ?,
		  grabbed_at = ?,
		  updated_at = ?,
		  completed_at = ?,
		  placed_at = ?,
		  confirmed_at = ?,
		  pack_files = ?, pack_identified = ?, pack_deduped = ?,
		  progress = ?, progress_at = ?,
		  attempts = ?, next_retry_at = ?, fail_kind = ?,
		  rev = rev + 1
		WHERE id = ? AND rev = ?`,
		g.ReleaseTitle, nullInt(g.ReleaseSize), nullString(g.ReleaseIndexer),
		nullString(g.DownloadURL), nullFloat(g.PredictedConfidence),
		nullString(g.ClientID), nullString(g.ClientName), g.Status,
		nullString(g.ActualStashDBID), nullString(g.Reason),
		nullString(g.PerformerName), nullString(g.PlacedPath), nullString(g.PlaceError),
		g.GrabbedAt,
		now,
		nullInt(g.CompletedAt), nullInt(g.PlacedAt), nullInt(g.ConfirmedAt),
		g.PackFiles, g.PackIdentified, g.PackDeduped,
		g.Progress, g.ProgressAt,
		g.Attempts, nullInt(g.NextRetryAt), nullString(g.FailKind),
		g.ID, g.Rev,
	)
	if err != nil {
		return fmt.Errorf("grabs update id=%d: %w", g.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrStaleUpdate
	}
	return nil
}

// Active returns grabs the poller still cares about — anything not in
// a terminal state.
//
//   - placed: file is on disk but not yet seen in Stash; the poller
//     re-checks each tick until it confirms or orphans.
//   - scanned: Stash has the file but hasn't attached a StashDB
//     cross-id yet (identify pending); we keep polling to detect when
//     the identify completes.
//   - orphaned: Stash didn't pick up the file within the orphan
//     window. Stays in Active so a later scan/identify (manual or
//     scheduled) can promote it back to scanned/confirmed instead of
//     leaving the user with a permanent false orphan label.
//   - tagging: a re-filed pack waiting for its rescan to land so the
//     poller can apply the pack performer to its scenes (advancePackTag).
//   - distributing: a studio/mixed pack (no performer) whose identified
//     scenes are being sorted into performer folders (advancePackDistribute).
//
// 'deferred' is deliberately absent: a deferred grab's add never landed
// in the client, so the link-timeout sweep would false-fail it while it
// waits for its retry slot. The deferred-retry loop owns that state and
// re-enters it here via retryGrab (status back to queued).
func (r *Repo) Active(ctx context.Context) ([]Grab, error) {
	return r.query(ctx, `
		SELECT * FROM grabs
		WHERE status IN ('queued', 'downloading', 'completed', 'placed', 'scanned', 'orphaned', 'tagging', 'distributing')
		ORDER BY grabbed_at ASC`)
}

// ConfirmedUnlinked returns settled single grabs that hold a placed file but
// no StashDB cross-id — the ones the poller's reconcile pass re-checks.
//
// These left Active() for good (it excludes 'confirmed'), yet their link can
// still arrive afterwards: Stash's identify is a serial queued job that
// routinely lands after the settle grace expires, and the user can identify a
// scene by hand at any time. Nothing would ever notice, so the grab keeps
// claiming it was never matched.
//
// Scoped to `since` (a confirm-time floor, falling back to updated_at for
// rows predating the ConfirmedAt stamp) because a file still unlinked after
// weeks genuinely isn't on StashDB, and re-querying it forever costs a Stash
// round-trip per pass for nothing. Packs are excluded: they carry no single
// cross-id, and advancePackConfirm owns their per-scene identify state.
//
// Newest-first with limit/offset so the caller can rotate through a large
// backlog a bounded batch at a time.
func (r *Repo) ConfirmedUnlinked(ctx context.Context, limit, offset int) ([]Grab, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return r.query(ctx, `
		SELECT * FROM grabs
		WHERE status = 'confirmed'
		  AND COALESCE(kind, 'single') != 'pack'
		  AND COALESCE(placed_path, '') != ''
		  AND COALESCE(actual_stashdb_id, '') = ''
		ORDER BY COALESCE(NULLIF(confirmed_at, 0), updated_at) DESC
		LIMIT ? OFFSET ?`, limit, offset)
}

// Mismatched returns mismatched single grabs, oldest-checked last. A
// mismatch is the machine's verdict, but the USER can overrule it in Stash
// — re-identify the scene to the predicted id — and until the reconcile
// pass learned to look, nothing ever noticed: mismatched grabs are out of
// Active() and stayed mismatched forever (the "no recovery path" residual
// in docs/error-handling.md).
//
// That fix originally carried a 14-day window, which closed the hole for a
// fortnight and left it open after. Measured on the reference instance:
// 134 of 148 mismatched grabs (91%) had aged past it, averaging 27 days, so
// the recovery pass could only ever see 14 of them. A user correcting a
// match a month later is the NORMAL case, not an edge one — the window
// assumed people tidy their library on the same schedule they download.
// Paginated instead, with the caller rotating a cursor.
func (r *Repo) Mismatched(ctx context.Context, limit, offset int) ([]Grab, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return r.query(ctx, `
		SELECT * FROM grabs
		WHERE status = 'mismatched'
		  AND COALESCE(kind, 'single') != 'pack'
		  AND COALESCE(placed_path, '') != ''
		ORDER BY updated_at DESC
		LIMIT ? OFFSET ?`, limit, offset)
}

// CountMismatched counts what Mismatched would return across every page, so
// the recovery cursor knows when it has wrapped.
func (r *Repo) CountMismatched(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM grabs
		WHERE status = 'mismatched'
		  AND COALESCE(kind, 'single') != 'pack'
		  AND COALESCE(placed_path, '') != ''`).Scan(&n)
	return n, err
}

// ConfirmedPlacedLinked returns confirmed grabs that have BOTH a placed
// path and a StashDB cross-id, oldest-checked first. These are the grabs
// whose placed_path can be re-derived from Stash when a file moves under
// forage: the cross-id says which scene, and Stash knows where that
// scene's file is now.
//
// Deliberately NOT bounded by a recency window, unlike the passes above. A
// file is usually moved long after the grab settles — the stale pointers
// that prompted this were 20 to 40 days old — so a window would exclude
// exactly the rows that need repairing. The cursor paginates instead.
func (r *Repo) ConfirmedPlacedLinked(ctx context.Context, limit, offset int) ([]Grab, error) {
	// Deliberately a higher ceiling than the sibling queries. Theirs is 200
	// because each row they return drives a Stash lookup; this pass only
	// os.Stats each row and calls Stash for the rare miss, so it wants to
	// scan wide. Clamping to 50 here would silently make a wider scan
	// NARROWER than the default batch it was meant to widen.
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	return r.query(ctx, `
		SELECT * FROM grabs
		WHERE status = 'confirmed'
		  AND COALESCE(placed_path, '') != ''
		  AND COALESCE(actual_stashdb_id, '') != ''
		ORDER BY id
		LIMIT ? OFFSET ?`, limit, offset)
}

// CountConfirmedPlacedLinked counts what ConfirmedPlacedLinked would return
// across every page, so the repair cursor knows when it has wrapped.
func (r *Repo) CountConfirmedPlacedLinked(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM grabs
		WHERE status = 'confirmed'
		  AND COALESCE(placed_path, '') != ''
		  AND COALESCE(actual_stashdb_id, '') != ''`).Scan(&n)
	return n, err
}

// The 14-day window both of these used to carry is gone. It assumed a file
// still unlinked after two weeks is "genuinely not on StashDB", which
// measurement contradicted: of 25 sampled unlinked grabs on the reference
// instance, 10 (40%) already carried a stash_id in Stash — identify simply
// ran later than the window, and forage had stopped looking. Those grabs
// averaged 27 days old, so nothing would ever have revisited them.
//
// Cost is unchanged per pass: the caller still takes reconcileBatch rows and
// rotates a cursor, and the lookup is against the LOCAL Stash, not the
// rate-limited StashDB budget. Widening the set lengthens a rotation; it
// does not raise the per-tick cost.

// CountConfirmedUnlinked counts what ConfirmedUnlinked would return across
// every page, so the reconcile cursor knows when it has wrapped.
func (r *Repo) CountConfirmedUnlinked(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM grabs
		WHERE status = 'confirmed'
		  AND COALESCE(kind, 'single') != 'pack'
		  AND COALESCE(placed_path, '') != ''
		  AND COALESCE(actual_stashdb_id, '') = ''`).Scan(&n)
	return n, err
}

// StatusByStashDBID returns a map of StashDB scene id → grab status for
// every grab that resolves to a StashDB id, so the missing-scenes view
// can mark scenes already grabbed/in-flight. Keyed by the actual cross-id
// when known (confirmed), else the predicted one. When several grabs map
// to the same scene id, the most advanced status wins (so a confirmed
// grab isn't masked by a later failed retry of the same scene).
func (r *Repo) StatusByStashDBID(ctx context.Context) (map[string]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT COALESCE(NULLIF(predicted_stashdb_id, ''), ''),
		       COALESCE(NULLIF(actual_stashdb_id, ''), ''), status
		FROM grabs
		WHERE COALESCE(NULLIF(actual_stashdb_id, ''), predicted_stashdb_id) IS NOT NULL
		  AND COALESCE(NULLIF(actual_stashdb_id, ''), predicted_stashdb_id) != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	record := func(sid, status string) {
		if sid == "" {
			return
		}
		if cur, ok := out[sid]; !ok || statusRank(status) > statusRank(cur) {
			out[sid] = status
		}
	}
	for rows.Next() {
		var predicted, actual, status string
		if rows.Scan(&predicted, &actual, &status) != nil {
			continue
		}
		// Actual-else-predicted: the scene this grab's file IS (or, until
		// identified, is expected to be).
		if actual != "" {
			record(actual, status)
		} else {
			record(predicted, status)
		}
		// A mismatched grab ALSO stamps its PREDICTED scene: the download
		// was made FOR that scene and now sits pending human review. The
		// watch/discover layers use this to hold the scene quiet until the
		// user resolves the mismatch (redo/delete resumes the hunt), rather
		// than re-offering releases for a scene whose acquisition is in
		// limbo. statusRank(mismatched)=0, so any live grab for the same
		// scene still wins the entry.
		if status == "mismatched" && actual != "" {
			record(predicted, status)
		}
	}
	return out, rows.Err()
}

// statusRank orders grab statuses by pipeline progress so the most
// advanced grab for a scene wins in StatusByStashDBID. failed/orphaned
// rank below everything so a dead retry never masks a live/confirmed grab.
func statusRank(s string) int {
	switch s {
	case "confirmed":
		return 8
	case "scanned":
		return 7
	case "placed":
		return 6
	case "completed":
		return 5
	case "downloading":
		return 4
	case "queued", "deferred":
		// deferred ranks with queued: the add hasn't landed yet but the
		// retry loop will re-drive it, so the scene is still in-flight
		// and watch/discover must not re-offer releases for it.
		return 3
	case "orphaned":
		return 2 // in limbo, revivable — outranks failed, below live
	case "mismatched":
		return 1 // pending human review — outranks failed, below live
	default: // failed, unknown
		return 0
	}
}

// HasLiveGrabForRelease reports whether a non-failed grab already exists for
// this exact release title — i.e. forager already has, or is downloading,
// this content. Adoption uses it to skip re-adopting a release it already
// grabbed: the StashDB-cross-id dedup can't see non-StashDB content (OnlyFans
// siterips and the like, which never resolve to a StashDB scene), so without
// this an identical re-download landing under the forage category gets placed
// as a second copy. failed/orphaned/mismatched are excluded so a genuine
// re-grab after a dead attempt still proceeds.
func (r *Repo) HasLiveGrabForRelease(ctx context.Context, title string) (bool, error) {
	if title == "" {
		return false, nil
	}
	var n int
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(1) FROM grabs
		WHERE release_title = ?
		  AND status IN ('queued','deferred','downloading','completed','placed','scanned','confirmed')`,
		title).Scan(&n)
	return n > 0, err
}

// DeferredDue returns deferred grabs whose retry time has arrived,
// soonest first, capped at limit: the deferred-retry loop's work list.
// The cap bounds one tick's worth of re-adds (a SAB re-add is a
// synchronous call); anything past it is picked up next tick.
//
// excludeClients drops whole client kinds from the batch: grabs held by
// a client outage keep the OLDEST next_retry_at values, so without the
// exclusion they would fill the LIMIT window on every tick and starve
// due grabs on the healthy client for the outage's duration.
//
// A NULL next_retry_at counts as due, deliberately: no current writer
// produces a deferred row without a schedule, but if one ever slips
// through, "retry it now" recovers the grab, whereas filtering it out
// would strand a zombie invisible to both this loop and the poller.
func (r *Repo) DeferredDue(ctx context.Context, now int64, limit int, excludeClients []string) ([]Grab, error) {
	if limit <= 0 {
		limit = 10
	}
	where := `status = 'deferred' AND (next_retry_at IS NULL OR next_retry_at <= ?)`
	args := []any{now}
	for _, c := range excludeClients {
		where += ` AND client != ?`
		args = append(args, c)
	}
	args = append(args, limit)
	return r.query(ctx, `
		SELECT * FROM grabs
		WHERE `+where+`
		ORDER BY next_retry_at ASC LIMIT ?`, args...)
}

// ConfirmedSince returns single-scene grabs that reached 'confirmed' after
// the given unix time, oldest first — the notify loop's "scene landed in
// Stash" sweep. Packs are excluded: their many-scene landing has its own
// UI and doesn't reduce to one watch link.
func (r *Repo) ConfirmedSince(ctx context.Context, since int64) ([]Grab, error) {
	return r.query(ctx, `
		SELECT * FROM grabs
		WHERE status = 'confirmed' AND kind = 'single' AND confirmed_at > ?
		ORDER BY confirmed_at ASC LIMIT 50`, since)
}

// List returns the most recent grabs first, narrowed by status (unless ""
// or "any") and by a free-text query q that matches release_title,
// performer_name, release_indexer, or client_name (case-insensitive
// substring). Used by the GET /grabs endpoint. q is what lets the UI search
// the WHOLE grab history rather than just the newest page it holds in memory.
func (r *Repo) List(ctx context.Context, status, q string, limit, offset int) ([]Grab, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	where, args := grabFilter(status, q)
	args = append(args, limit, offset)
	return r.query(ctx,
		`SELECT * FROM grabs `+where+` ORDER BY grabbed_at DESC LIMIT ? OFFSET ?`, args...)
}

// CountFiltered returns how many grabs match the same status+q filter List
// applies, ignoring limit/offset — so the UI can show a result count and tell
// when it has paged to the end of the matches.
func (r *Repo) CountFiltered(ctx context.Context, status, q string) (int, error) {
	where, args := grabFilter(status, q)
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM grabs `+where, args...).Scan(&n)
	return n, err
}

// grabFilter builds the shared WHERE clause (and bound args) for List and
// CountFiltered. An empty/"any" status and an empty q each drop out; with
// neither set it returns a bare "" clause (whole table). q is matched as a
// case-insensitive substring across the human-searchable text columns; it's
// always parameterized, so a `%`/`_` in the query reads as a LIKE wildcard
// (acceptable for a search box) but can never inject.
func grabFilter(status, q string) (string, []any) {
	var conds []string
	var args []any
	switch {
	case status == "" || status == "any":
		// whole table
	case status == unmatchedFilter:
		// Pseudo-status: a grab that settled 'confirmed' without ever being
		// linked to a StashDB scene ("in library (scanned)" / "in library; no
		// StashDB match"). Not a column value, so it splits the confirmed
		// rows rather than adding a new status. Mirrors Totals below and the
		// UI's isUnmatched, so the chip count and the filtered list agree.
		conds = append(conds, unmatchedCond)
	case status == "confirmed":
		// The complement, so confirmed + unmatched partition the confirmed
		// rows exactly and the two chip counts still sum to the old total.
		conds = append(conds, "status = 'confirmed' AND NOT ("+unmatchedCond+")")
	default:
		conds = append(conds, "status = ?")
		args = append(args, status)
	}
	if s := strings.TrimSpace(q); s != "" {
		like := "%" + s + "%"
		conds = append(conds,
			"(release_title LIKE ? OR performer_name LIKE ? OR release_indexer LIKE ? OR client_name LIKE ?)")
		args = append(args, like, like, like, like)
	}
	if len(conds) == 0 {
		return "", args
	}
	return "WHERE " + strings.Join(conds, " AND "), args
}

// CountRecentFailed counts grabs that failed at/after `since` (unix). Old
// failures aren't actionable, so the notification badge only counts recent
// ones rather than the all-time failed total.
func (r *Repo) CountRecentFailed(ctx context.Context, since int64) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM grabs WHERE status = 'failed' AND updated_at >= ?`, since).Scan(&n)
	return n, err
}

// unmatchedFilter is the pseudo-status for a grab that settled 'confirmed'
// with no StashDB cross-id, and unmatchedCond is the SQL that identifies one.
// Shared by grabFilter and Totals so a chip's count can never disagree with
// the list that chip filters to. Packs are excluded: they carry no single
// cross-id (advancePackConfirm tracks their scenes individually), so a pack
// is not "unmatched" for having an empty actual_stashdb_id.
const (
	unmatchedFilter = "unmatched"
	unmatchedCond   = `status = 'confirmed'
		  AND COALESCE(kind, 'single') != 'pack'
		  AND COALESCE(actual_stashdb_id, '') = ''`
)

// Totals returns a status → count map for the UI's top-of-page strip.
//
// 'confirmed' is reported SPLIT: the returned "confirmed" counts only grabs
// actually linked to a StashDB scene, and the rest land under "unmatched".
// The two partition the confirmed rows, so summing every value still gives
// the true grand total — which is what the UI's "all" chip does.
func (r *Repo) Totals(ctx context.Context) (map[string]int, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM grabs GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var unmatched int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM grabs WHERE `+unmatchedCond).Scan(&unmatched); err != nil {
		return nil, err
	}
	if unmatched > 0 {
		out[unmatchedFilter] = unmatched
		// Take them out of 'confirmed' rather than double-counting.
		if out["confirmed"] >= unmatched {
			out["confirmed"] -= unmatched
		} else {
			out["confirmed"] = 0
		}
	}
	return out, rows.Err()
}

// ByDownloadURL is the lookup the search UI uses to mark a release as
// "already grabbed" — same Prowlarr download URL has been clicked before.
func (r *Repo) ByDownloadURL(ctx context.Context, url string) (*Grab, error) {
	if url == "" {
		return nil, nil
	}
	grabs, err := r.query(ctx, `
		SELECT * FROM grabs WHERE download_url = ? ORDER BY grabbed_at DESC LIMIT 1`, url)
	if err != nil || len(grabs) == 0 {
		return nil, err
	}
	g := grabs[0]
	return &g, nil
}

// LiveByRelease finds a non-failed grab for the same release. Three
// identities, widening in order:
//
//  1. exact download URL — but Prowlarr URLs embed a rotating encrypted
//     `link` parameter, so the same release gets a fresh URL on every
//     search and this alone misses re-offers;
//  2. (release title + indexer) — the stable identity one indexer gives a
//     release, which catches those re-offers;
//  3. release title alone, but ONLY against a grab whose file is already on
//     disk (placed_path set).
//
// Rule 3 exists because a release cross-posted to several indexers is the
// same content under the same title, and rules 1-2 both miss it: forage
// re-downloaded Bang.YNGR.26.07.10.Liora.Vane.XXX.1080p.MP4-WRB from
// NZBFinder 14 days after taking the identical file from NZBgeek, wasting
// the transfer and leaving two byte-identical copies in the library.
//
// It is deliberately gated on placed_path rather than simply dropping the
// indexer from rule 2, because "same title, different indexer" is also the
// standard RECOVERY move: when one indexer's Usenet post is incomplete or
// unrepairable, fetching the same release from another is exactly what you
// want to do. Requiring the earlier grab to have landed separates the two
// cases cleanly — an in-flight, stalled or dead attempt (no placed file)
// never blocks a second source, while content that's already on disk is
// never fetched twice. Nothing is gained by re-downloading a file you have.
//
// Returns (nil, nil) when nothing live matches.
func (r *Repo) LiveByRelease(ctx context.Context, url, title, indexer string) (*Grab, error) {
	// A bare title is enough now (rule 3 needs no indexer), so only the
	// everything-empty case is unanswerable.
	if url == "" && title == "" {
		return nil, nil
	}
	grabs, err := r.query(ctx, `
		SELECT * FROM grabs
		WHERE status != 'failed'
		  AND (
		        download_url = ?
		     OR (
		          release_title = ? AND ? != ''
		          AND (release_indexer = ? OR COALESCE(placed_path, '') != '')
		        )
		      )
		ORDER BY grabbed_at DESC LIMIT 1`, url, title, title, indexer)
	if err != nil || len(grabs) == 0 {
		return nil, err
	}
	g := grabs[0]
	return &g, nil
}

// ByClientID returns the newest grab for a download-client item (qBit
// info-hash / SAB nzo id), or (nil, nil) when forage doesn't track it —
// the seeding cull's ownership check: an untracked torrent is NEVER
// forage's to delete, whatever category it sits in.
func (r *Repo) ByClientID(ctx context.Context, client, clientID string) (*Grab, error) {
	if client == "" || clientID == "" {
		return nil, nil
	}
	rows, err := r.query(ctx, `
		SELECT * FROM grabs
		WHERE client = ? AND LOWER(client_id) = LOWER(?)
		ORDER BY grabbed_at DESC LIMIT 1`, client, clientID)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	g := rows[0]
	return &g, nil
}

// Get returns a single grab by id, or (nil, nil) when not found.
func (r *Repo) Get(ctx context.Context, id int64) (*Grab, error) {
	grabs, err := r.query(ctx, `SELECT * FROM grabs WHERE id = ? LIMIT 1`, id)
	if err != nil || len(grabs) == 0 {
		return nil, err
	}
	g := grabs[0]
	return &g, nil
}

// Delete removes a grab row. Used by the purge flow after the file +
// Stash scene + download-client copy have been torn down.
func (r *Repo) Delete(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM grabs WHERE id = ?`, id)
	return err
}

// Recoverable returns grabs for the given client (e.g. "sabnzbd", "qbit")
// marked failed but not yet placed, keyed by client_id (SAB nzo_id / qBit
// info-hash). The adoption sweep cross-references these against the client's
// live state: when a failed grab's download is in fact still present and
// healthy/completed, the failure was spurious (a transient client error past
// the grace window, or a queue-visibility flap) and the download would
// otherwise be stranded — Active() excludes failed grabs and adoption skips
// known ids, so it can't be re-picked-up as a fresh grab. Scoped to unplaced
// failures: a grab with a placed_path already reached the library and is healed
// on its placed path, not re-downloaded.
func (r *Repo) Recoverable(ctx context.Context, client string) (map[string]Grab, error) {
	rows, err := r.query(ctx, `
		SELECT * FROM grabs
		WHERE client = ? AND status = 'failed'
		  AND (placed_path IS NULL OR placed_path = '')
		  AND client_id IS NOT NULL AND client_id != ''`, client)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Grab, len(rows))
	for _, g := range rows {
		out[g.ClientID] = g
	}
	return out, nil
}

// KnownClientIDs returns the set of download-client ids (qBit info_hashes
// / SAB nzo_ids) that already back a grab. The poller's adoption path
// uses it to skip torrents forage is already tracking.
func (r *Repo) KnownClientIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT client_id FROM grabs WHERE client_id IS NOT NULL AND client_id != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil && id != "" {
			out[id] = true
		}
	}
	return out, rows.Err()
}

// query is the shared SELECT path. The column order MUST match the
// scanRow helper.
func (r *Repo) query(ctx context.Context, sql string, args ...any) ([]Grab, error) {
	const cols = `
		id, predicted_stashdb_id, predicted_confidence, release_title,
		release_size, release_indexer, download_url,
		client, client_id, client_name, category, status, actual_stashdb_id,
		reason, performer_name, placed_path, place_error,
		grabbed_at, updated_at, completed_at, placed_at, confirmed_at,
		kind, pack_files, pack_identified, pack_deduped,
		progress, progress_at, attempts, next_retry_at, fail_kind, rev,
		COALESCE(source,'')`
	// Inject column list into the SELECT *.
	sql = replaceFirstStar(sql, cols)
	rows, err := r.db.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Grab
	for rows.Next() {
		g, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func scanRow(rows *sql.Rows) (Grab, error) {
	var g Grab
	var (
		predictedID, releaseIndexer, downloadURL, clientID, clientName, category, actualID, reason sql.NullString
		performerName, placedPath, placeError                                                      sql.NullString
		predictedConfidence                                                                        sql.NullFloat64
		releaseSize, completedAt, placedAt, confirmedAt, nextRetryAt                               sql.NullInt64
		kind, failKind                                                                             sql.NullString
	)
	err := rows.Scan(&g.ID,
		&predictedID, &predictedConfidence, &g.ReleaseTitle,
		&releaseSize, &releaseIndexer, &downloadURL,
		&g.Client, &clientID, &clientName, &category, &g.Status, &actualID,
		&reason, &performerName, &placedPath, &placeError,
		&g.GrabbedAt, &g.UpdatedAt, &completedAt, &placedAt, &confirmedAt,
		&kind, &g.PackFiles, &g.PackIdentified, &g.PackDeduped,
		&g.Progress, &g.ProgressAt, &g.Attempts, &nextRetryAt, &failKind, &g.Rev,
		&g.Source)
	if err != nil {
		return g, err
	}
	g.NextRetryAt = nextRetryAt.Int64
	g.FailKind = failKind.String
	g.Kind = kind.String
	if g.Kind == "" {
		g.Kind = "single"
	}
	g.PredictedStashDBID = predictedID.String
	g.PredictedConfidence = predictedConfidence.Float64
	g.ReleaseSize = releaseSize.Int64
	g.ReleaseIndexer = releaseIndexer.String
	g.DownloadURL = downloadURL.String
	g.ClientID = clientID.String
	g.ClientName = clientName.String
	g.Category = category.String
	g.ActualStashDBID = actualID.String
	g.Reason = reason.String
	g.PerformerName = performerName.String
	g.PlacedPath = placedPath.String
	g.PlaceError = placeError.String
	g.CompletedAt = completedAt.Int64
	g.PlacedAt = placedAt.Int64
	g.ConfirmedAt = confirmedAt.Int64
	return g, nil
}

func replaceFirstStar(sql, cols string) string {
	const tag = "*"
	for i := 0; i < len(sql); i++ {
		if sql[i] == tag[0] {
			return sql[:i] + cols + sql[i+1:]
		}
	}
	return sql
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(i int64) any {
	if i == 0 {
		return nil
	}
	return i
}

func nullFloat(f float64) any {
	if f == 0 {
		return nil
	}
	return f
}
