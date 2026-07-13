package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/ordureconnoisseur/forager/internal/clienterr"
	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/qbit"
)

// Deferred-retry flow: a grab whose add failed TRANSIENTLY (download
// client unreachable, indexer 5xx) parks in status "deferred" instead of
// failing outright, and RunDeferredRetryLoop re-drives the add with
// exponential backoff. This is what lets grabs made during a client
// outage land on their own once the outage clears, instead of dying and
// waiting for a manual retry. Permanent errors (bad torrent, 404, auth
// rejection) still fail immediately, exactly as before.
const (
	// deferMaxAttempts is the total add-attempt budget (the initial add
	// plus deferred retries). The 5th consecutive failure settles the
	// grab to failed with the final error in Reason. Distinct from
	// grab.go's addMaxAttempts (the in-process rate-limit retry INSIDE
	// one attempt); the two multiply, see the note there.
	deferMaxAttempts = 5
	// deferredTick is the retry loop's cadence. Backoff times are minutes,
	// so minute-level scheduling precision is plenty.
	deferredTick = 60 * time.Second
	// deferredBatch bounds how many due grabs one tick re-drives. A SAB
	// re-add is a synchronous call with a long client timeout, so the cap
	// keeps one slow client from wedging the tick; the remainder is due
	// again next tick.
	deferredBatch = 5
)

// deferBackoff returns how long to wait after the given number of failed
// attempts: 1m, 5m, 15m, then 60m for every later attempt.
func deferBackoff(attempts int) time.Duration {
	switch attempts {
	case 1:
		return time.Minute
	case 2:
		return 5 * time.Minute
	case 3:
		return 15 * time.Minute
	default:
		return time.Hour
	}
}

// shouldDeferAdd reports whether a failed add is worth an automatic
// retry. Only transient failures qualify. SAB timeouts are carved out:
// a slow addurl may have enqueued the NZB despite the error, so a
// re-add would duplicate the download (the same rule the in-process
// rate-limit retry follows); connection-level failures (refused, reset,
// DNS) never reached SAB and are safe to retry. qBit re-adds are
// hash-pinned and idempotent, so every transient qualifies, timeouts
// included.
//
// Known residual window (accepted): a qBit add that TIMED OUT but
// actually landed leaves a torrent in qBit with no linked grab while
// the grab sits deferred. Normally the first retry (1m) re-adds
// idempotently and links it long before the adoption sweep's 5m grace;
// the gap needs health-prober flapping to hold retries past the grace
// WHILE qBit stays listable, whereupon the sweep adopts the torrent as
// a second grab sharing the download. Judged rare enough to document
// rather than complicate adoption over.
func shouldDeferAdd(client string, err error) bool {
	if !errors.Is(err, clienterr.ErrTransient) {
		return false
	}
	if client == "sabnzbd" {
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			return false
		}
	}
	return true
}

// deferOrFailGrab is the terminal error handler for an add attempt:
// transient failures defer the grab for an automatic retry (until the
// attempt budget runs out), everything else fails it immediately via
// failGrab. Mirrors failGrab's structure and guards: best-effort, and
// only a grab in 'queued' (async add) or still 'deferred' (retryGrab's
// synchronous SAB re-add, which runs before the row flips to queued) is
// touched, so a poller that already advanced the row (the add actually
// landed and our response read tripped) is never regressed.
func (s *Server) deferOrFailGrab(ctx context.Context, grabID int64, reason string, err error) {
	if s.grabs == nil || grabID == 0 {
		return
	}
	// CAS-retry loop, not a single bare Update: unlike failGrab, a lost
	// write here does NOT converge to an equivalent outcome. If the defer
	// is silently dropped (a poller pickRecent stamp or user edit bumping
	// rev in the window), the row stays 'queued' with no in-flight add and
	// no loop owning it, and the link-timeout sweep later hard-fails it,
	// which is exactly the outage hard-fail this flow exists to prevent.
	// The guards re-run on every iteration, so a concurrent ADVANCE (the
	// add actually landed) still aborts cleanly.
	for attempt := 0; attempt < 3; attempt++ {
		g, gerr := s.grabs.Get(ctx, grabID)
		if gerr != nil || g == nil {
			return
		}
		if g.Status != "queued" && g.Status != "deferred" {
			s.log.Info("skip defer/fail: grab already advanced", "grab_id", grabID, "status", g.Status)
			return
		}
		if !shouldDeferAdd(g.Client, err) {
			s.failGrab(ctx, grabID, reason)
			return
		}
		g.Attempts++
		if g.Attempts >= deferMaxAttempts {
			g.Status = "failed"
			g.Reason = fmt.Sprintf("%s (gave up after %d attempts)", reason, g.Attempts)
			g.NextRetryAt = 0
			g.FailKind = ""
		} else {
			g.Status = "deferred"
			g.Reason = reason
			g.NextRetryAt = time.Now().Add(deferBackoff(g.Attempts)).Unix()
			// Record which stage failed so the retry loop knows whether an
			// indexer failover is worth attempting: a failed .torrent fetch
			// means the INDEXER couldn't serve the release (another
			// indexer's copy of the same scene may work right now), while a
			// client-side failure means the release is fine and the retry
			// should re-drive it unchanged.
			if errors.Is(err, qbit.ErrIndexerFetch) {
				g.FailKind = "indexer"
			} else {
				g.FailKind = "client"
			}
		}
		uerr := s.grabs.Update(ctx, *g)
		if errors.Is(uerr, grabs.ErrStaleUpdate) {
			continue // someone wrote between Get and Update; reload + re-judge
		}
		if uerr != nil {
			s.log.Warn("defer grab", "grab_id", grabID, "err", uerr)
			return
		}
		if g.Status == "deferred" {
			s.log.Warn("grab deferred for retry",
				"grab_id", grabID, "attempt", g.Attempts, "max", deferMaxAttempts,
				"retry_at", time.Unix(g.NextRetryAt, 0).Format(time.RFC3339), "err", err)
		} else {
			s.log.Error("grab failed after exhausting retries",
				"grab_id", grabID, "attempts", g.Attempts, "err", err)
		}
		return
	}
	s.log.Warn("defer grab: lost the optimistic lock 3x; leaving row as-is", "grab_id", grabID)
}

// clientRetryBlocked reports whether a deferred grab's download client
// is currently unable to take a retry: unconfigured (nil in the pool),
// or confirmed unreachable by the Pool's health prober (the same signal
// behind the UI banner). Blocked retries are skipped WITHOUT consuming
// an attempt, so the budget is spent on real tries only and the grab
// survives an arbitrarily long outage.
func (s *Server) clientRetryBlocked(g *grabs.Grab) bool {
	switch g.Client {
	case "qbit":
		if s.pool.Qbit() == nil {
			return true
		}
		h := s.pool.QbitHealth()
		return h.Probed && !h.OK
	case "sabnzbd":
		if s.pool.Sab() == nil {
			return true
		}
		h := s.pool.SabHealth()
		return h.Probed && !h.OK
	}
	return false
}

// RunDeferredRetryLoop re-drives deferred grabs whose backoff has
// elapsed. Launched from main like the other server loops; exits on ctx
// cancellation.
func (s *Server) RunDeferredRetryLoop(ctx context.Context) {
	t := time.NewTicker(deferredTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			func() {
				defer s.recoverPanic("deferred retry loop")
				s.tickDeferredRetries(ctx)
			}()
		}
	}
}

// tickDeferredRetries retries up to deferredBatch due grabs: hold while
// the client is down, maybe switch the release (indexer failover), then
// re-drive. While a grab's download client is confirmed unreachable (the
// Pool's health prober, the same signal behind the UI banner), its retry
// is skipped WITHOUT consuming an attempt: attempts are spent on real
// tries only, so the budget survives an arbitrarily long outage and the
// grab re-drives within a tick of the client coming back.
func (s *Server) tickDeferredRetries(ctx context.Context) {
	if s.grabs == nil {
		return
	}
	// Blocked CLIENTS are excluded in the query, not just per-grab:
	// held grabs keep the oldest next_retry_at values, so with a plain
	// LIMIT they would monopolise the batch for the whole outage and
	// starve due grabs on the healthy client. (clientRetryBlocked below
	// stays as the per-grab recheck for health flips mid-tick.)
	var exclude []string
	if s.clientRetryBlocked(&grabs.Grab{Client: "qbit"}) {
		exclude = append(exclude, "qbit")
	}
	if s.clientRetryBlocked(&grabs.Grab{Client: "sabnzbd"}) {
		exclude = append(exclude, "sabnzbd")
	}
	due, err := s.grabs.DeferredDue(ctx, time.Now().Unix(), deferredBatch, exclude)
	if err != nil {
		s.log.Warn("deferred retry: list due", "err", err)
		return
	}
	for i := range due {
		g := due[i]
		if s.clientRetryBlocked(&g) {
			continue // hold the grab, attempt budget untouched
		}
		reason, switched := s.maybeFailOverRelease(ctx, &g)
		if rerr := s.retryGrabWithReason(ctx, &g, false, reason); rerr != nil {
			s.log.Warn("deferred retry", "grab_id", g.ID, "err", rerr)
			continue
		}
		s.log.Info("deferred grab retrying",
			"grab_id", g.ID, "attempt", g.Attempts+1, "max", deferMaxAttempts,
			"failover", switched)
	}
}

// maybeFailOverRelease switches an indexer-failed grab to the scene's
// best verified release on a different, non-benched indexer before its
// re-drive (see failover.go). It returns the reason line to stamp on the
// re-queued row ("" = failover wasn't applicable; retryGrab writes its
// default) and whether the release actually switched. When the resolver
// RAN and found no qualifying alternative, the reason says so: the user
// staring at a struggling grab deserves to know the failover looked and
// came up empty, not to wonder why the feature did nothing.
//
// Fires exactly ONCE per grab, on the retry after the SECOND consecutive
// indexer failure (Attempts == 2): the first retry re-drives the
// original release, because a single 429 is often momentary and the
// original was the ranked-best pick (possibly the user's explicit
// choice). Single-shot also prevents an A-to-B-to-A ping-pong between
// two struggling indexers from burning the whole attempt budget, and
// caps the resolution cost (a full scene search + matcher verify) at one
// per grab. The row mutates in place: scene linkage kept, client link
// cleared, so Phase-B confirmation is unaffected.
func (s *Server) maybeFailOverRelease(ctx context.Context, g *grabs.Grab) (string, bool) {
	if g.FailKind != "indexer" || g.Attempts != 2 || s.resolveFailover == nil {
		return "", false
	}
	// Without the failed indexer's name the "different indexer" pick is
	// meaningless: the fresh search may return the SAME failing release
	// under a re-tokenised download URL and "fail over" to it.
	if g.ReleaseIndexer == "" {
		return "", false
	}
	alt := s.resolveFailover(ctx, g)
	if alt == nil {
		return fmt.Sprintf("auto-retrying (attempt %d/%d; no alternative source found)",
			g.Attempts+1, deferMaxAttempts), false
	}
	if uerr := s.applyGrabUpdate(ctx, g.ID, func(fresh *grabs.Grab) {
		if fresh.Status != "deferred" {
			return // user retried/deleted meanwhile; leave it
		}
		fresh.DownloadURL = alt.DownloadURL
		fresh.ReleaseTitle = alt.Title
		fresh.ReleaseIndexer = alt.Indexer
		fresh.ReleaseSize = alt.Size
		// The row's stored confidence must describe the release it now
		// points at, not the certainty of the abandoned pick.
		fresh.PredictedConfidence = alt.Confidence
		// The old release's pinned hash (if any) describes a torrent
		// that never landed; the new release re-pins via retryGrab.
		fresh.ClientID = ""
		fresh.ClientName = ""
	}); uerr != nil {
		s.log.Warn("failover: switch release", "grab_id", g.ID, "err", uerr)
		return "", false
	}
	s.log.Info("failover: switching release",
		"grab_id", g.ID, "from", g.ReleaseIndexer, "to", alt.Indexer, "release", alt.Title)
	// retryGrab claims and drives the FRESH row, so the switched values
	// reach the client even though our local snapshot is stale.
	return fmt.Sprintf("failed over to %s (attempt %d/%d)", alt.Indexer, g.Attempts+1, deferMaxAttempts), true
}
