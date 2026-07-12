// Package notify pushes forage events to external sinks so the user hears
// about actionable state without opening the UI: a watch that found a
// release (grab is a click away), or grabs that failed. Two sinks, both
// optional and independent:
//
//   - Telegram: bot token + chat id → sendMessage. The zero-infrastructure
//     option for a phone push.
//   - Webhook: a URL that receives a small JSON POST per event batch, for
//     anything else (ntfy, Discord via a shim, Home Assistant, ...).
//
// Mirrors internal/qbit + friends: raw net/http, no third-party deps. The
// Notifier is immutable — it lives in the clientpool and is rebuilt on
// config save, so hot-swapped credentials propagate like every other
// client's.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
	"time"
)

// telegramAPIBase is a var so tests can point the client at a fake server.
var telegramAPIBase = "https://api.telegram.org"

// SetTelegramAPIBaseForTest points every Notifier at a fake Bot API server
// and returns a restore func. For tests in other packages (the api
// callback loop); production code must never call it.
func SetTelegramAPIBaseForTest(u string) (restore func()) {
	old := telegramAPIBase
	telegramAPIBase = u
	return func() { telegramAPIBase = old }
}

type Notifier struct {
	telegramToken  string
	telegramChatID string
	webhookURL     string
	http           *http.Client
	// pollHTTP serves getUpdates long-polls (see Updates) — needs a budget
	// past the long-poll window, unlike the tight send client.
	pollHTTP *http.Client
}

// Button is one inline-keyboard button on a Telegram message. Data is the
// callback payload (Telegram caps it at 64 bytes) delivered back via
// Updates when the user taps it; URL instead makes the button a plain
// link the client opens directly (no callback round-trip). Set exactly
// one of the two. Buttons only render on the Telegram sink; the webhook
// payload is unaffected.
type Button struct {
	Text string
	Data string
	URL  string
}

// New builds a Notifier from whichever sink credentials are set. Returns
// nil when neither sink is fully configured — callers treat a nil Notifier
// as "notifications off", the same contract as the pool's other clients.
func New(telegramToken, telegramChatID, webhookURL string) *Notifier {
	telegramOK := telegramToken != "" && telegramChatID != ""
	if !telegramOK && webhookURL == "" {
		return nil
	}
	n := &Notifier{
		webhookURL: webhookURL,
		http:       &http.Client{Timeout: 15 * time.Second},
		pollHTTP:   &http.Client{Timeout: updatesLongPoll + 15*time.Second},
	}
	if telegramOK {
		n.telegramToken, n.telegramChatID = telegramToken, telegramChatID
	}
	return n
}

// TelegramEnabled reports whether the Telegram sink is configured — the
// callback loop only runs (and getUpdates is only polled) when it is.
func (n *Notifier) TelegramEnabled() bool {
	return n != nil && n.telegramToken != ""
}

// ChatID exposes the configured Telegram chat id so the callback loop can
// authorize incoming taps against it.
func (n *Notifier) ChatID() string {
	if n == nil {
		return ""
	}
	return n.telegramChatID
}

// Send pushes one message to every configured sink. event is a stable
// machine tag ("watch_available", "grabs_failed"); text is the
// human-readable message (may be multi-line). Sink failures are joined and
// returned for the caller to log — a notification must never fail the flow
// that raised it, so callers log and move on.
func (n *Notifier) Send(ctx context.Context, event, text string) error {
	if n == nil || text == "" {
		return nil
	}
	var errs []error
	if n.telegramToken != "" {
		if err := n.sendTelegram(ctx, text, nil); err != nil {
			errs = append(errs, fmt.Errorf("telegram: %w", err))
		}
	}
	if n.webhookURL != "" {
		if err := n.sendWebhook(ctx, event, stripHTML(text), ""); err != nil {
			errs = append(errs, fmt.Errorf("webhook: %w", err))
		}
	}
	return errors.Join(errs...)
}

// SendPhoto pushes a photo (by public URL) with a caption to every
// configured sink. Telegram fetches the URL server-side (sendPhoto), so
// the daemon never downloads the image; if Telegram can't take the photo
// (dead URL, unsupported type, over its 5MB URL limit) the caption
// degrades to a plain text message so the notification is never lost. The
// webhook receives the URL as an extra "image" field. An empty photoURL
// is just Send.
func (n *Notifier) SendPhoto(ctx context.Context, event, photoURL, caption string, buttons ...Button) error {
	if n == nil || caption == "" {
		return nil
	}
	var errs []error
	if n.telegramToken != "" {
		perr := errors.New("no photo")
		if photoURL != "" {
			perr = n.sendTelegramPhoto(ctx, photoURL, caption, buttons)
		}
		if perr != nil {
			if terr := n.sendTelegram(ctx, caption, buttons); terr != nil {
				errs = append(errs, fmt.Errorf("telegram: %w", terr))
			}
		}
	}
	if n.webhookURL != "" {
		if err := n.sendWebhook(ctx, event, stripHTML(caption), photoURL); err != nil {
			errs = append(errs, fmt.Errorf("webhook: %w", err))
		}
	}
	return errors.Join(errs...)
}

// Message text contract: Send/SendPhoto text is Telegram-HTML restricted
// to <b> tags. Callers MUST run every dynamic value (titles, reasons,
// names) through Escape; the webhook sink receives the same text with the
// tags stripped and entities unescaped. HTML over MarkdownV2 because
// escaping is three entities instead of eighteen punctuation characters —
// and a parse failure still degrades to a plain-text resend rather than a
// dropped notification.

// Escape makes a dynamic string safe to embed in Send/SendPhoto text.
func Escape(s string) string { return html.EscapeString(s) }

// stripHTML converts the Telegram-HTML text to plain text for the webhook
// sink: drops the <b> tags we emit and unescapes entities.
func stripHTML(s string) string {
	s = strings.ReplaceAll(s, "<b>", "")
	s = strings.ReplaceAll(s, "</b>", "")
	return html.UnescapeString(s)
}

// sendTelegram posts a sendMessage in HTML parse mode, retrying once as
// plain text if Telegram rejects the entities — a malformed message must
// degrade, never drop.
func (n *Notifier) sendTelegram(ctx context.Context, text string, buttons []Button) error {
	body := map[string]any{
		"chat_id":                  n.telegramChatID,
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	if kb := inlineKeyboard(buttons); kb != nil {
		body["reply_markup"] = kb
	}
	err := n.telegramCall(ctx, "sendMessage", body)
	if err != nil && strings.Contains(err.Error(), "parse entities") {
		delete(body, "parse_mode")
		body["text"] = stripHTML(text)
		err = n.telegramCall(ctx, "sendMessage", body)
	}
	return err
}

// inlineKeyboard renders buttons as one keyboard row; nil for none.
func inlineKeyboard(buttons []Button) map[string]any {
	if len(buttons) == 0 {
		return nil
	}
	row := make([]map[string]any, 0, len(buttons))
	for _, b := range buttons {
		if b.URL != "" {
			row = append(row, map[string]any{"text": b.Text, "url": b.URL})
			continue
		}
		row = append(row, map[string]any{"text": b.Text, "callback_data": b.Data})
	}
	return map[string]any{"inline_keyboard": [][]map[string]any{row}}
}

// telegramCall posts one Bot API method, decoding the ok/description
// envelope and never leaking the bot token (it lives in the URL path).
func (n *Notifier) telegramCall(ctx context.Context, method string, body map[string]any) error {
	_, err := n.telegramCallBody(ctx, method, body, n.http)
	return err
}

func (n *Notifier) telegramCallBody(ctx context.Context, method string, body map[string]any, hc *http.Client) ([]byte, error) {
	payload, _ := json.Marshal(body)
	u := telegramAPIBase + "/bot" + n.telegramToken + "/" + method
	respBody, err := n.postWith(ctx, hc, u, payload)
	if err != nil {
		return nil, errors.New(strings.ReplaceAll(err.Error(), n.telegramToken, "REDACTED"))
	}
	var resp struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if jerr := json.Unmarshal(respBody, &resp); jerr == nil && !resp.OK {
		return respBody, fmt.Errorf("%s refused: %s", method, resp.Description)
	}
	return respBody, nil
}

// sendTelegramPhoto posts sendPhoto with a URL Telegram fetches itself.
// Caption limit is 1024 chars (ours are short). Same no-parse_mode and
// token-redaction reasoning as sendTelegram.
func (n *Notifier) sendTelegramPhoto(ctx context.Context, photoURL, caption string, buttons []Button) error {
	body := map[string]any{
		"chat_id":    n.telegramChatID,
		"photo":      photoURL,
		"caption":    caption,
		"parse_mode": "HTML",
	}
	if kb := inlineKeyboard(buttons); kb != nil {
		body["reply_markup"] = kb
	}
	err := n.telegramCall(ctx, "sendPhoto", body)
	if err != nil && strings.Contains(err.Error(), "parse entities") {
		delete(body, "parse_mode")
		body["caption"] = stripHTML(caption)
		err = n.telegramCall(ctx, "sendPhoto", body)
	}
	return err
}

// ── Callback plumbing (getUpdates long-poll) ────────────────────────
//
// The notify loop's messages can carry inline buttons; taps arrive as
// callback_query updates that SOMETHING must poll for. forager's bot token
// is dedicated to forager (Telegram allows exactly one getUpdates
// consumer per token), so the daemon long-polls it via Updates. The api
// layer owns the loop, the offset watermark, and what the buttons DO —
// this package just speaks the Bot API.

// updatesLongPoll is the getUpdates server-side hold. Telegram answers
// early when an update arrives; a full window costs one idle round trip.
const updatesLongPoll = 25 * time.Second

// Update is one getUpdates entry, projected to what the callback loop
// needs. Non-callback updates (someone texting the bot) decode with a nil
// Callback and are skipped by the consumer — but still advance the offset.
type Update struct {
	ID       int64     `json:"update_id"`
	Callback *Callback `json:"callback_query"`
}

// Callback is a button tap: who tapped (From.ID, for authorization),
// which button (Data), and the message it lives on (for the outcome edit).
type Callback struct {
	ID   string `json:"id"`
	From struct {
		ID int64 `json:"id"`
	} `json:"from"`
	Data    string `json:"data"`
	Message struct {
		MessageID int64 `json:"message_id"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		Caption string `json:"caption"` // set on photo messages
		Text    string `json:"text"`    // set on plain-text messages
	} `json:"message"`
}

// Updates long-polls getUpdates from offset, returning whatever arrived
// within the window (possibly nothing). Only callback_query updates are
// requested — the bot has no conversational surface.
func (n *Notifier) Updates(ctx context.Context, offset int64) ([]Update, error) {
	body, err := n.telegramCallBody(ctx, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         int(updatesLongPoll / time.Second),
		"allowed_updates": []string{"callback_query"},
	}, n.pollHTTP)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result []Update `json:"result"`
	}
	if jerr := json.Unmarshal(body, &resp); jerr != nil {
		return nil, fmt.Errorf("decode getUpdates: %w", jerr)
	}
	return resp.Result, nil
}

// AnswerCallback acknowledges a button tap (stops the client's spinner)
// with an optional toast. Must be called for EVERY callback, even ones we
// refuse to act on.
func (n *Notifier) AnswerCallback(ctx context.Context, callbackID, toast string) error {
	body := map[string]any{"callback_query_id": callbackID}
	if toast != "" {
		body["text"] = toast
	}
	return n.telegramCall(ctx, "answerCallbackQuery", body)
}

// FinalizeMessage appends an outcome line to the message a callback came
// from and strips its buttons (an edit without reply_markup removes the
// keyboard), leaving a permanent record of what the tap did. Handles both
// photo messages (caption edit) and plain-text ones.
func (n *Notifier) FinalizeMessage(ctx context.Context, cb *Callback, outcome string) error {
	if cb == nil || cb.Message.MessageID == 0 {
		return nil
	}
	if cb.Message.Caption != "" {
		return n.telegramCall(ctx, "editMessageCaption", map[string]any{
			"chat_id":    cb.Message.Chat.ID,
			"message_id": cb.Message.MessageID,
			"caption":    cb.Message.Caption + "\n" + outcome,
		})
	}
	return n.telegramCall(ctx, "editMessageText", map[string]any{
		"chat_id":    cb.Message.Chat.ID,
		"message_id": cb.Message.MessageID,
		"text":       cb.Message.Text + "\n" + outcome,
	})
}

// sendWebhook posts {"event", "message", "ts"} as JSON, plus "image"
// (a public URL) when the event carries one.
func (n *Notifier) sendWebhook(ctx context.Context, event, text, imageURL string) error {
	body := map[string]any{
		"event":   event,
		"message": text,
		"ts":      time.Now().Unix(),
	}
	if imageURL != "" {
		body["image"] = imageURL
	}
	payload, _ := json.Marshal(body)
	_, err := n.post(ctx, n.webhookURL, payload)
	return err
}

func (n *Notifier) post(ctx context.Context, url string, payload []byte) ([]byte, error) {
	return n.postWith(ctx, n.http, url, payload)
}

func (n *Notifier) postWith(ctx context.Context, hc *http.Client, url string, payload []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// 1MB bound: a getUpdates batch carries full message objects and can
	// far exceed an error page's size; truncating it would corrupt the
	// JSON. Still bounded so a hostile webhook endpoint can't balloon us.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}
