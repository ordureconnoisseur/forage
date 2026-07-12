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

	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/notify"
	"github.com/ordureconnoisseur/forager/internal/pathmap"
	"github.com/ordureconnoisseur/forager/internal/scoring"
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
	metaNotifyWatchFoundAt   = "notify_watch_found_at"
	metaNotifyGrabFailedAt   = "notify_grab_failed_at"
	metaNotifyGrabLandedAt   = "notify_grab_landed_at"
	metaNotifyGrabDeferredAt = "notify_grab_deferred_at"
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
	s.notifyDeferredGrabs(ctx, now)
	s.notifyLandedGrabs(ctx, now)
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
			lines = append(lines, "• "+notify.Escape(truncateLine(watchLabel(wt), notifyTitleMax)))
			if wt.FoundAt > maxAt {
				maxAt = wt.FoundAt
			}
		}
		text := notifyDigest(fmt.Sprintf("🎬 <b>forage: %d watched scenes have a release ready to grab</b>", len(lines)), lines)
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
		caption := buildWatchCaption(wt)
		// Inline actions, handled by the Telegram callback loop, labeled to
		// match the Watching tab: Grab runs the same code as its grab
		// button; "Not this one" ignores this release and immediately
		// re-searches for a different one (callback data keeps the
		// internal "dismiss" verb so older messages' buttons stay valid).
		buttons := []notify.Button{
			{Text: "⬇ Grab", Data: "grab:" + wt.StashDBID},
			{Text: "✖ Not this one", Data: "dismiss:" + wt.StashDBID},
		}
		if err := s.pool.Notifier().SendPhoto(ctx, "watch_available", wt.ImageURL, caption, buttons...); err != nil {
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
// dropping whichever parts are missing. Plain text — the bulk digest's
// per-line form; escape before embedding.
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

// buildWatchCaption renders the per-scene notification as cleanly
// separated, labeled lines (Telegram-HTML; every dynamic value escaped):
//
//	🎬 <scene title, bold>
//	👤 <performer>
//	🏛 <studio · date>
//
//	Release: <release name, truncated>
//	Quality: <1080p · torrent>
//	Size:    <1.9GB>
//	Indexer: <PornoLab>
func buildWatchCaption(wt watches.Watch) string {
	title := wt.Title
	if title == "" {
		title = wt.StashDBID
	}
	lines := []string{"🎬 <b>" + notify.Escape(truncateLine(title, 80)) + "</b>"}
	if wt.PerformerName != "" {
		lines = append(lines, "👤 "+notify.Escape(wt.PerformerName))
	}
	var meta []string
	if wt.StudioName != "" {
		meta = append(meta, wt.StudioName)
	}
	if wt.Date != "" {
		meta = append(meta, wt.Date)
	}
	if len(meta) > 0 {
		lines = append(lines, "🏛 "+notify.Escape(strings.Join(meta, " · ")))
	}
	lines = append(lines, "")
	if wt.FoundTitle != "" {
		lines = append(lines, "<b>Release:</b> "+notify.Escape(truncateLine(wt.FoundTitle, notifyTitleMax)))
	}
	var quality []string
	if res := scoring.Resolution(wt.FoundTitle); res != "" {
		quality = append(quality, res)
	}
	if wt.FoundProtocol != "" {
		quality = append(quality, wt.FoundProtocol)
	}
	if len(quality) > 0 {
		lines = append(lines, "<b>Quality:</b> "+notify.Escape(strings.Join(quality, " · ")))
	}
	if wt.FoundSize > 0 {
		lines = append(lines, "<b>Size:</b> "+humanBytes(wt.FoundSize))
	}
	if wt.FoundIndexer != "" {
		lines = append(lines, "<b>Indexer:</b> "+notify.Escape(wt.FoundIndexer))
	}
	return strings.Join(lines, "\n")
}

// notifyTitleMax / notifyReasonMax bound one release title / failure reason
// in a message line. Scene-release names (RuTracker performer rosters, JAV
// titles) run to whole paragraphs; a notification is a headline, not the
// record — the Grabs UI has the full strings.
const (
	notifyTitleMax  = 60
	notifyReasonMax = 90
)

// notifyDeferredGrabs digests grabs that just entered the deferred-retry
// flow, so a Telegram-first user learns about an indexer or client
// problem when it STARTS, not ~81 minutes later when budgets exhaust
// into the failed digest (and never at all for grabs that eventually
// land, or for client outages the UI banner only shows in the browser).
// Fires once per grab, on its FIRST deferral (Attempts == 1): re-defers
// bump updated_at but carry higher attempt counts, and the story's
// ending arrives via the landed or failed digests. Same watermark
// mechanics as notifyFailedGrabs; grouped by reason because a deferral
// burst usually has ONE cause.
func (s *Server) notifyDeferredGrabs(ctx context.Context, now int64) {
	wm, ok := s.notifyWatermark(ctx, metaNotifyGrabDeferredAt, now)
	if !ok {
		return
	}
	deferred, err := s.grabs.List(ctx, "deferred", "", 200, 0)
	if err != nil {
		return
	}
	var reasons []string // first-seen order
	titles := map[string][]string{}
	maxAt := wm
	for _, g := range deferred {
		if g.UpdatedAt <= wm || g.Attempts != 1 {
			continue
		}
		r := truncateLine(g.Reason, notifyReasonMax)
		if _, seen := titles[r]; !seen {
			reasons = append(reasons, r)
		}
		titles[r] = append(titles[r], "• "+notify.Escape(truncateLine(g.ReleaseTitle, notifyTitleMax)))
		if g.UpdatedAt > maxAt {
			maxAt = g.UpdatedAt
		}
	}
	if len(reasons) == 0 {
		return
	}
	total := 0
	for _, r := range reasons {
		total += len(titles[r])
	}
	var text string
	if len(reasons) == 1 {
		headline := fmt.Sprintf("⏳ <b>forage: %s deferred, retrying automatically</b>: %s",
			plural(total, "grab"), notify.Escape(reasons[0]))
		text = notifyDigest(headline, titles[reasons[0]])
	} else {
		var lines []string
		for _, r := range reasons {
			lines = append(lines, "<b>"+notify.Escape(r)+":</b>")
			lines = append(lines, titles[r]...)
		}
		text = notifyDigest(fmt.Sprintf("⏳ <b>forage: %s deferred, retrying automatically</b>",
			plural(total, "grab")), lines)
	}
	if err := s.pool.Notifier().Send(ctx, "grabs_deferred", text); err != nil {
		s.log.Warn("notify send failed; will retry", "event", "grabs_deferred", "err", err)
		return
	}
	s.setNotifyWatermark(ctx, metaNotifyGrabDeferredAt, maxAt)
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
	// Group by reason: a batch usually fails for ONE cause (a tracker
	// outage, qBit erroring), and repeating it on every line is what turned
	// the digest into a wall of text.
	var reasons []string // first-seen order
	titles := map[string][]string{}
	maxAt := wm
	for _, g := range failed {
		if g.UpdatedAt <= wm {
			continue
		}
		r := truncateLine(g.Reason, notifyReasonMax)
		if _, seen := titles[r]; !seen {
			reasons = append(reasons, r)
		}
		titles[r] = append(titles[r], "• "+notify.Escape(truncateLine(g.ReleaseTitle, notifyTitleMax)))
		if g.UpdatedAt > maxAt {
			maxAt = g.UpdatedAt
		}
	}
	if len(reasons) == 0 {
		return
	}
	var text string
	total := 0
	for _, r := range reasons {
		total += len(titles[r])
	}
	if len(reasons) == 1 {
		headline := fmt.Sprintf("❌ <b>forage: %s failed</b> — %s", plural(total, "grab"), notify.Escape(reasons[0]))
		text = notifyDigest(headline, titles[reasons[0]])
	} else {
		// Mixed causes: reason as a sub-heading per group, lines capped
		// across the whole message.
		var lines []string
		for _, r := range reasons {
			lines = append(lines, "<b>"+notify.Escape(r)+":</b>")
			lines = append(lines, titles[r]...)
		}
		text = notifyDigest(fmt.Sprintf("❌ <b>forage: %s failed</b>", plural(total, "grab")), lines)
	}
	if err := s.pool.Notifier().Send(ctx, "grabs_failed", text); err != nil {
		s.log.Warn("notify send failed; will retry", "event", "grabs_failed", "err", err)
		return
	}
	s.setNotifyWatermark(ctx, metaNotifyGrabFailedAt, maxAt)
}

// notifyLandedGrabs announces single-scene grabs that finished the whole
// pipeline — downloaded, placed, scanned into Stash, identify settled
// ('confirmed') — with a link straight to the scene so the notification is
// a "go watch it" tap. Same watermark/photo-vs-digest mechanics as
// notifyAvailableWatches; packs are excluded (their many-scene landing has
// its own UI).
func (s *Server) notifyLandedGrabs(ctx context.Context, now int64) {
	wm, ok := s.notifyWatermark(ctx, metaNotifyGrabLandedAt, now)
	if !ok {
		return
	}
	landed, err := s.grabs.ConfirmedSince(ctx, wm)
	if err != nil || len(landed) == 0 {
		return
	}

	// Bulk case: one text digest, all-or-nothing watermark advance.
	if len(landed) > notifyPhotoMax {
		maxAt := wm
		var lines []string
		for _, g := range landed {
			title, _, _, _ := s.landedSceneMeta(ctx, g)
			label := title
			if g.PerformerName != "" {
				label = g.PerformerName + " — " + label
			}
			lines = append(lines, "• "+notify.Escape(truncateLine(label, notifyTitleMax)))
			if g.ConfirmedAt > maxAt {
				maxAt = g.ConfirmedAt
			}
		}
		text := notifyDigest(fmt.Sprintf("✅ <b>forage: %s landed in Stash</b>", plural(len(lines), "scene")), lines)
		if err := s.pool.Notifier().Send(ctx, "grab_landed", text); err != nil {
			s.log.Warn("notify send failed; will retry", "event", "grab_landed", "err", err)
			return
		}
		s.setNotifyWatermark(ctx, metaNotifyGrabLandedAt, maxAt)
		return
	}

	// Few scenes: one message each — cover photo, labeled caption, and a
	// Watch button linking into Stash when the scene resolves.
	lastOK := wm
	for _, g := range landed {
		title, studio, date, imageURL := s.landedSceneMeta(ctx, g)
		lines := []string{"✅ <b>" + notify.Escape(truncateLine(title, 80)) + "</b> — in your library"}
		if g.PerformerName != "" {
			lines = append(lines, "👤 "+notify.Escape(g.PerformerName))
		}
		var meta []string
		if studio != "" {
			meta = append(meta, studio)
		}
		if date != "" {
			meta = append(meta, date)
		}
		if len(meta) > 0 {
			lines = append(lines, "🏛 "+notify.Escape(strings.Join(meta, " · ")))
		}
		lines = append(lines, "", "<b>Release:</b> "+notify.Escape(truncateLine(g.ReleaseTitle, notifyTitleMax)))
		var buttons []notify.Button
		if link := s.stashSceneLink(ctx, g); link != "" {
			buttons = append(buttons, notify.Button{Text: "▶ Watch in Stash", URL: link})
		}
		if err := s.pool.Notifier().SendPhoto(ctx, "grab_landed", imageURL, strings.Join(lines, "\n"), buttons...); err != nil {
			s.log.Warn("notify send failed; will retry", "event", "grab_landed", "grab", g.ID, "err", err)
			if lastOK > wm && lastOK < g.ConfirmedAt {
				s.setNotifyWatermark(ctx, metaNotifyGrabLandedAt, lastOK)
			}
			return
		}
		lastOK = g.ConfirmedAt
	}
	s.setNotifyWatermark(ctx, metaNotifyGrabLandedAt, lastOK)
}

// landedSceneMeta resolves display metadata for a landed grab from the
// persistent StashDB scene cache (title, studio, date, cover). Falls back
// to the release's own strings when the grab never got a cross-id ("in
// library (scanned)" settles) or the cache hasn't seen the scene.
func (s *Server) landedSceneMeta(ctx context.Context, g grabs.Grab) (title, studio, date, imageURL string) {
	if g.ActualStashDBID != "" {
		_ = s.db.QueryRowContext(ctx, `
			SELECT COALESCE(title,''), COALESCE(studio_name,''), COALESCE(release_date,''), COALESCE(image_url,'')
			FROM stashdb_scene WHERE stashdb_id = ?`, g.ActualStashDBID).
			Scan(&title, &studio, &date, &imageURL)
	}
	if title == "" {
		if title = pathmap.Base(g.PlacedPath); title == "" {
			title = g.ReleaseTitle
		}
	}
	return title, studio, date, imageURL
}

// stashSceneLink builds the user-facing "watch it" URL for a landed grab:
// the placed file's scene looked up live in Stash (fresh — the scene was
// created moments ago, ahead of any cache), joined onto StashPublicURL
// (the address the user's devices can reach) or StashURL as fallback.
// Empty when anything along the way is missing — the notification then
// just goes out without a button.
func (s *Server) stashSceneLink(ctx context.Context, g grabs.Grab) string {
	cfg := s.bootstrap.Config // env-only fallback (store absent in tests)
	if s.store != nil {
		cfg = s.composedConfig()
	}
	base := cfg.StashPublicURL
	if base == "" {
		base = cfg.StashURL
	}
	stashC := s.pool.Stash()
	if base == "" || stashC == nil {
		return ""
	}
	needle := pathmap.Base(g.PlacedPath)
	if needle == "" {
		needle = g.ClientName
	}
	scene, err := stashC.FindSceneByPathContains(ctx, needle)
	if err != nil || scene == nil || scene.ID == "" {
		return ""
	}
	return base + "/scenes/" + scene.ID
}

// truncateLine caps a string at max runes with an ellipsis, and flattens
// newlines so one item can't break the digest's line-per-item shape.
func truncateLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
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
