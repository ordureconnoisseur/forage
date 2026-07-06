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
	"io"
	"net/http"
	"strings"
	"time"
)

// telegramAPIBase is a var so tests can point the client at a fake server.
var telegramAPIBase = "https://api.telegram.org"

type Notifier struct {
	telegramToken  string
	telegramChatID string
	webhookURL     string
	http           *http.Client
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
	}
	if telegramOK {
		n.telegramToken, n.telegramChatID = telegramToken, telegramChatID
	}
	return n
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
		if err := n.sendTelegram(ctx, text); err != nil {
			errs = append(errs, fmt.Errorf("telegram: %w", err))
		}
	}
	if n.webhookURL != "" {
		if err := n.sendWebhook(ctx, event, text, ""); err != nil {
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
func (n *Notifier) SendPhoto(ctx context.Context, event, photoURL, caption string) error {
	if n == nil || caption == "" {
		return nil
	}
	if photoURL == "" {
		return n.Send(ctx, event, caption)
	}
	var errs []error
	if n.telegramToken != "" {
		if perr := n.sendTelegramPhoto(ctx, photoURL, caption); perr != nil {
			if terr := n.sendTelegram(ctx, caption); terr != nil {
				errs = append(errs, fmt.Errorf("telegram: %w", terr))
			}
		}
	}
	if n.webhookURL != "" {
		if err := n.sendWebhook(ctx, event, caption, photoURL); err != nil {
			errs = append(errs, fmt.Errorf("webhook: %w", err))
		}
	}
	return errors.Join(errs...)
}

// sendTelegram posts a plain-text sendMessage. No parse_mode: release
// titles are full of characters MarkdownV2 would demand escaping for, and
// a failed parse drops the whole message.
func (n *Notifier) sendTelegram(ctx context.Context, text string) error {
	payload, _ := json.Marshal(map[string]any{
		"chat_id":                  n.telegramChatID,
		"text":                     text,
		"disable_web_page_preview": true,
	})
	u := telegramAPIBase + "/bot" + n.telegramToken + "/sendMessage"
	body, err := n.post(ctx, u, payload)
	if err != nil {
		// Never leak the bot token (it's in the URL path).
		return errors.New(strings.ReplaceAll(err.Error(), n.telegramToken, "REDACTED"))
	}
	var resp struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if jerr := json.Unmarshal(body, &resp); jerr == nil && !resp.OK {
		return fmt.Errorf("sendMessage refused: %s", resp.Description)
	}
	return nil
}

// sendTelegramPhoto posts sendPhoto with a URL Telegram fetches itself.
// Caption limit is 1024 chars (ours are short). Same no-parse_mode and
// token-redaction reasoning as sendTelegram.
func (n *Notifier) sendTelegramPhoto(ctx context.Context, photoURL, caption string) error {
	payload, _ := json.Marshal(map[string]any{
		"chat_id": n.telegramChatID,
		"photo":   photoURL,
		"caption": caption,
	})
	u := telegramAPIBase + "/bot" + n.telegramToken + "/sendPhoto"
	body, err := n.post(ctx, u, payload)
	if err != nil {
		return errors.New(strings.ReplaceAll(err.Error(), n.telegramToken, "REDACTED"))
	}
	var resp struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if jerr := json.Unmarshal(body, &resp); jerr == nil && !resp.OK {
		return fmt.Errorf("sendPhoto refused: %s", resp.Description)
	}
	return nil
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
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}
