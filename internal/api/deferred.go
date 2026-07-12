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
	// grab to failed with the final error in Reason.
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
	} else {
		g.Status = "deferred"
		g.Reason = reason
		g.NextRetryAt = time.Now().Add(deferBackoff(g.Attempts)).Unix()
		// Record which stage failed so the retry loop knows whether an
		// indexer failover is worth attempting: a failed .torrent fetch
		// means the INDEXER couldn't serve the release (another indexer's
		// copy of the same scene may work right now), while a client-side
		// failure means the release is fine and the retry should re-drive
		// it unchanged.
		if errors.Is(err, qbit.ErrIndexerFetch) {
			g.FailKind = "indexer"
		} else {
			g.FailKind = "client"
		}
	}
	if uerr := s.grabs.Update(ctx, *g); uerr != nil && !errors.Is(uerr, grabs.ErrStaleUpdate) {
		s.log.Warn("defer grab", "grab_id", grabID, "err", uerr)
	}
	if g.Status == "deferred" {
		s.log.Warn("grab deferred for retry",
			"grab_id", grabID, "attempt", g.Attempts, "max", deferMaxAttempts,
			"retry_at", time.Unix(g.NextRetryAt, 0).Format(time.RFC3339), "err", err)
	} else {
		s.log.Error("grab failed after exhausting retries",
			"grab_id", grabID, "attempts", g.Attempts, "err", err)
	}
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

// tickDeferredRetries retries up to deferredBatch due grabs. While a
// grab's download client is confirmed unreachable (the Pool's health
// prober, the same signal behind the UI banner), its retry is skipped
// WITHOUT consuming an attempt: attempts are spent on real tries only,
// so the budget survives an arbitrarily long outage and the grab
// re-drives within a tick of the client coming back.
func (s *Server) tickDeferredRetries(ctx context.Context) {
	if s.grabs == nil {
		return
	}
	due, err := s.grabs.DeferredDue(ctx, time.Now().Unix(), deferredBatch)
	if err != nil {
		s.log.Warn("deferred retry: list due", "err", err)
		return
	}
	for i := range due {
		g := due[i]
		if s.clientRetryBlocked(&g) {
			continue // hold the grab, attempt budget untouched
		}
		// Indexer-side failure: the fetch through Prowlarr failed, so the
		// same release will likely fail again. Try switching the grab to
		// the scene's best verified release on a different, non-benched
		// indexer before re-driving (see failover.go). The row mutates in
		// place, keeping the scene linkage; on no alternative the original
		// release retries unchanged, exactly as before.
		failedOver := ""
		if g.FailKind == "indexer" && s.resolveFailover != nil {
			if alt := s.resolveFailover(ctx, failoverGrab{
				ID:                 g.ID,
				Client:             g.Client,
				Kind:               g.Kind,
				PredictedStashDBID: g.PredictedStashDBID,
				PerformerName:      g.PerformerName,
				ReleaseIndexer:     g.ReleaseIndexer,
				DownloadURL:        g.DownloadURL,
			}); alt != nil {
				if uerr := s.applyGrabUpdate(ctx, g.ID, func(fresh *grabs.Grab) {
					if fresh.Status != "deferred" {
						return // user retried/deleted meanwhile; leave it
					}
					fresh.DownloadURL = alt.DownloadURL
					fresh.ReleaseTitle = alt.Title
					fresh.ReleaseIndexer = alt.Indexer
					fresh.ReleaseSize = alt.Size
					// The old release's pinned hash (if any) describes a
					// torrent that never landed; the new release re-pins
					// via retryGrab's magnet path or the fetched bytes.
					fresh.ClientID = ""
					fresh.ClientName = ""
				}); uerr != nil {
					s.log.Warn("failover: switch release", "grab_id", g.ID, "err", uerr)
				} else {
					failedOver = alt.Indexer
					s.log.Info("failover: switching release",
						"grab_id", g.ID, "from", g.ReleaseIndexer, "to", alt.Indexer,
						"release", alt.Title)
					if fresh, gerr := s.grabs.Get(ctx, g.ID); gerr == nil && fresh != nil {
						g = *fresh // retryGrab must see the switched row
					}
				}
			}
		}
		if rerr := s.retryGrab(ctx, &g, false); rerr != nil {
			s.log.Warn("deferred retry", "grab_id", g.ID, "err", rerr)
			continue
		}
		if failedOver != "" {
			// Stamp the visible story: the generic "auto-retrying" reason
			// retryGrab set doesn't say the release changed. Best-effort.
			_ = s.applyGrabUpdate(ctx, g.ID, func(fresh *grabs.Grab) {
				if fresh.Status == "queued" {
					fresh.Reason = fmt.Sprintf("failed over to %s (attempt %d/%d)",
						failedOver, fresh.Attempts+1, deferMaxAttempts)
				}
			})
		}
		s.log.Info("deferred grab retrying",
			"grab_id", g.ID, "attempt", g.Attempts+1, "max", deferMaxAttempts,
			"release", g.ReleaseTitle, "failover", failedOver != "")
	}
}
