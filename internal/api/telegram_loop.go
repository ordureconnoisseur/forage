package api

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/ordureconnoisseur/forager/internal/notify"
)

// The Telegram callback loop makes notification buttons DO things: the
// notify loop's watch-available messages carry Grab/Dismiss inline
// buttons, and this loop long-polls getUpdates for the taps, authorizes
// them against the configured chat id, and runs exactly the same code the
// Watching tab's buttons run. The update offset persists in the meta table
// so a restart neither replays a day of old taps nor drops pending ones —
// and the actions are idempotent anyway (grab dedups by download URL,
// dismiss re-ignores an already-ignored URL).
//
// forager's bot token is dedicated to forager. Telegram permits ONE
// getUpdates consumer per token; this loop is it. (The goonerdl bot's
// token must never be polled here — see the shared-token history.)

const telegramOffsetMetaKey = "telegram_update_offset"

// telegramIdleWait paces the loop when Telegram is unconfigured or a poll
// fails — no hot spin, quick pickup once config arrives.
const telegramIdleWait = 30 * time.Second

// RunTelegramLoop long-polls for button taps until ctx is cancelled.
// Started from main like the other loops; a no-op sleep cycle when the
// Telegram sink isn't configured.
func (s *Server) RunTelegramLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		n := s.pool.Notifier()
		if !n.TelegramEnabled() {
			sleepCtx(ctx, telegramIdleWait)
			continue
		}
		func() {
			defer s.recoverPanic("telegram loop")
			offset := s.telegramOffset(ctx)
			ups, err := n.Updates(ctx, offset)
			if err != nil {
				if ctx.Err() == nil {
					s.log.Warn("telegram getUpdates", "err", err)
					sleepCtx(ctx, telegramIdleWait)
				}
				return
			}
			for _, u := range ups {
				if u.Callback != nil {
					s.handleTelegramCallback(ctx, n, u.Callback)
				}
				offset = u.ID + 1
			}
			if len(ups) > 0 {
				s.setNotifyWatermark(ctx, telegramOffsetMetaKey, offset)
			}
		}()
	}
}

// telegramOffset loads the persisted getUpdates offset (0 = from the
// oldest pending update; Telegram only retains updates ~24h, so a fresh
// daemon can't replay ancient taps).
func (s *Server) telegramOffset(ctx context.Context) int64 {
	var stored string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, telegramOffsetMetaKey).Scan(&stored)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.log.Warn("telegram offset read", "err", err)
		}
		return 0
	}
	v, _ := strconv.ParseInt(stored, 10, 64)
	return v
}

// handleTelegramCallback authorizes and executes one button tap, then
// acknowledges it (every callback MUST be answered or the client spins)
// and stamps the outcome onto the message.
func (s *Server) handleTelegramCallback(ctx context.Context, n *notify.Notifier, cb *notify.Callback) {
	// Authorization: only taps from the configured chat act. Anyone else
	// who somehow found the bot gets a silent ack and no side effects.
	if strconv.FormatInt(cb.From.ID, 10) != n.ChatID() {
		s.log.Warn("telegram callback from unauthorized user", "from", cb.From.ID)
		_ = n.AnswerCallback(ctx, cb.ID, "")
		return
	}
	action, sceneID, ok := strings.Cut(cb.Data, ":")
	if !ok || sceneID == "" {
		_ = n.AnswerCallback(ctx, cb.ID, "unrecognized button")
		return
	}

	var toast, outcome string
	switch action {
	case "grab":
		if err := s.grabAvailableWatch(ctx, sceneID); err != nil {
			toast = "Grab failed: " + err.Error()
		} else {
			toast = "Grabbed — downloading"
			outcome = "✅ Grabbed"
		}
	case "dismiss":
		wt := s.findWatch(ctx, sceneID)
		switch {
		case wt == nil:
			toast = "watch not found"
		case wt.FoundURL == "":
			toast = "nothing to dismiss"
		default:
			if err := s.watches.Dismiss(ctx, sceneID, wt.FoundURL); err != nil {
				toast = "Couldn't skip that release: " + err.Error()
			} else {
				toast = "Skipping that release — searching for another"
				outcome = "✖ Not this one — searching for another release"
				// The web UI's "Not this one" follows its dismiss with an
				// immediate re-search of just this scene; do the same so
				// the button is a true twin. Best-effort: busy (another
				// search running) or unconfigured just means the
				// background loop picks it up on its normal cadence.
				if _, serr := s.startWatchSearch(ctx, []string{sceneID}, ""); serr != nil && !errors.Is(serr, errSearchBusy) {
					s.log.Warn("telegram dismiss re-search", "scene", sceneID, "err", serr)
				}
			}
		}
	default:
		toast = "unrecognized button"
	}

	if err := n.AnswerCallback(ctx, cb.ID, toast); err != nil {
		s.log.Warn("telegram answer callback", "err", err)
	}
	if outcome != "" {
		if err := n.FinalizeMessage(ctx, cb, outcome); err != nil {
			s.log.Warn("telegram finalize message", "err", err)
		}
	}
	// Log every tap, not just successes — a failed grab tap otherwise
	// leaves no trace anywhere but a toast the user may have missed.
	s.log.Info("telegram button action",
		"action", action, "scene", sceneID, "ok", outcome != "", "result", toast)
}

// sleepCtx sleeps d or until ctx is cancelled, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
