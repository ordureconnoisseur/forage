package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ordureconnoisseur/forager/internal/watches"
)

// The notify loop pushes actionable transitions to the external sinks
// (Telegram / webhook — see internal/notify): a watch that found a release
// (a grab is one click away) and grabs that failed. It is deliberately
// poll-based rather than hooked into every transition site: the poller and
// api flip statuses in a dozen places, and a watermark sweep over the two
// tables catches them all, survives restarts, and can't slow a grab down.
//
// Watermarks live in the meta table (notify_watch_found_at /
// notify_grab_failed_at). A missing watermark initializes to NOW — turning
// notifications on must not replay history. A failed send leaves the
// watermark so the next tick retries (a partial failure across two sinks
// can re-send to the sink that succeeded; a rare duplicate beats a silent
// drop). Events are batched into one message per category per tick, so a
// bulk retry-all that fails 50 grabs is one message, not fifty.

const notifyInterval = 2 * time.Minute

const (
	metaNotifyWatchFoundAt = "notify_watch_found_at"
	metaNotifyGrabFailedAt = "notify_grab_failed_at"
)

// notifyDigestMax bounds how many per-item lines one message carries; the
// remainder collapses into a count.
const notifyDigestMax = 6

// RunNotifyLoop ticks until ctx is cancelled. Started from main like the
// watch/RSS loops.
func (s *Server) RunNotifyLoop(ctx context.Context) {
	t := time.NewTicker(notifyInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			func() {
				defer s.recoverPanic("notify loop")
				s.tickNotify(ctx)
			}()
		}
	}
}

func (s *Server) tickNotify(ctx context.Context) {
	n := s.pool.Notifier()
	if n == nil {
		return // notifications unconfigured
	}
	now := time.Now().Unix()
	s.notifyAvailableWatches(ctx, now)
	s.notifyFailedGrabs(ctx, now)
}

// notifyPhotoMax is the most per-scene photo messages one tick sends. Past
// it (a "watch all missing" batch coming available together) everything
// collapses into the single text digest instead — an album of 50 covers is
// spam, not signal.
const notifyPhotoMax = 4

func (s *Server) notifyAvailableWatches(ctx context.Context, now int64) {
	wm, ok := s.notifyWatermark(ctx, metaNotifyWatchFoundAt, now)
	if !ok {
		return
	}
	list, err := s.watches.List(ctx)
	if err != nil {
		return
	}
	var fresh []watches.Watch
	for _, wt := range list {
		if wt.Status == watches.StatusAvailable && wt.FoundAt > wm {
			fresh = append(fresh, wt)
		}
	}
	if len(fresh) == 0 {
		return
	}

	// Bulk case: one text digest, all-or-nothing watermark advance.
	if len(fresh) > notifyPhotoMax {
		maxAt := wm
		var lines []string
		for _, wt := range fresh {
			lines = append(lines, "• "+watchLabel(wt))
			if wt.FoundAt > maxAt {
				maxAt = wt.FoundAt
			}
		}
		text := notifyDigest(fmt.Sprintf("🎬 forage: %d watched scenes have a release ready to grab", len(lines)), lines)
		if err := s.pool.Notifier().Send(ctx, "watch_available", text); err != nil {
			s.log.Warn("notify send failed; will retry", "event", "watch_available", "err", err)
			return // keep the watermark — retry next tick
		}
		s.setNotifyWatermark(ctx, metaNotifyWatchFoundAt, maxAt)
		return
	}

	// Few scenes: one message each, with the StashDB cover when the watch
	// carries one (SendPhoto degrades to text if Telegram can't fetch it).
	// Process in found_at order and advance the watermark past each success
	// so a mid-batch failure retries only the remainder — except when the
	// failed item shares its timestamp with a success, where advancing
	// would skip it: there we keep the old watermark and accept a duplicate
	// resend over a dropped notification.
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].FoundAt < fresh[j].FoundAt })
	lastOK := wm
	for _, wt := range fresh {
		caption := "🎬 Release ready: " + watchLabel(wt)
		if wt.FoundTitle != "" {
			caption += "\n" + wt.FoundTitle
		}
		if err := s.pool.Notifier().SendPhoto(ctx, "watch_available", wt.ImageURL, caption); err != nil {
			s.log.Warn("notify send failed; will retry", "event", "watch_available", "scene", wt.StashDBID, "err", err)
			if lastOK > wm && lastOK < wt.FoundAt {
				s.setNotifyWatermark(ctx, metaNotifyWatchFoundAt, lastOK)
			}
			return
		}
		lastOK = wt.FoundAt
	}
	s.setNotifyWatermark(ctx, metaNotifyWatchFoundAt, lastOK)
}

// watchLabel renders a watch as "Performer — Title (Studio · date)",
// dropping whichever parts are missing.
func watchLabel(wt watches.Watch) string {
	name := wt.Title
	if name == "" {
		name = wt.StashDBID
	}
	if wt.PerformerName != "" {
		name = wt.PerformerName + " — " + name
	}
	var meta []string
	if wt.StudioName != "" {
		meta = append(meta, wt.StudioName)
	}
	if wt.Date != "" {
		meta = append(meta, wt.Date)
	}
	if len(meta) > 0 {
		name += " (" + strings.Join(meta, " · ") + ")"
	}
	return name
}

func (s *Server) notifyFailedGrabs(ctx context.Context, now int64) {
	wm, ok := s.notifyWatermark(ctx, metaNotifyGrabFailedAt, now)
	if !ok {
		return
	}
	failed, err := s.grabs.List(ctx, "failed", "", 200, 0)
	if err != nil {
		return
	}
	var lines []string
	maxAt := wm
	for _, g := range failed {
		if g.UpdatedAt <= wm {
			continue
		}
		line := "• " + g.ReleaseTitle
		if g.Reason != "" {
			line += " — " + g.Reason
		}
		lines = append(lines, line)
		if g.UpdatedAt > maxAt {
			maxAt = g.UpdatedAt
		}
	}
	if len(lines) == 0 {
		return
	}
	text := notifyDigest(fmt.Sprintf("❌ forage: %d grab(s) failed", len(lines)), lines)
	if err := s.pool.Notifier().Send(ctx, "grabs_failed", text); err != nil {
		s.log.Warn("notify send failed; will retry", "event", "grabs_failed", "err", err)
		return
	}
	s.setNotifyWatermark(ctx, metaNotifyGrabFailedAt, maxAt)
}

// notifyDigest joins a headline with up to notifyDigestMax item lines,
// collapsing the tail into a count.
func notifyDigest(headline string, lines []string) string {
	if len(lines) > notifyDigestMax {
		rest := len(lines) - notifyDigestMax
		lines = append(lines[:notifyDigestMax], fmt.Sprintf("…and %d more", rest))
	}
	return headline + "\n" + strings.Join(lines, "\n")
}

// notifyWatermark reads a watermark from the meta table. A missing row is
// initialized to `now` and reported not-ready: notifications turning on (or
// the very first run after this feature ships) must not replay the entire
// backlog of past events.
func (s *Server) notifyWatermark(ctx context.Context, key string, now int64) (int64, bool) {
	var stored string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		s.setNotifyWatermark(ctx, key, now)
		return 0, false
	}
	if err != nil {
		return 0, false // transient read failure — skip this tick, don't reset
	}
	v, perr := strconv.ParseInt(stored, 10, 64)
	if perr != nil {
		s.setNotifyWatermark(ctx, key, now)
		return 0, false
	}
	return v, true
}

func (s *Server) setNotifyWatermark(ctx context.Context, key string, v int64) {
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, key, strconv.FormatInt(v, 10)); err != nil {
		s.log.Warn("notify watermark write", "key", key, "err", err)
	}
}
