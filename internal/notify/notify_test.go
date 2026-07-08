package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestNewNilWhenUnconfigured(t *testing.T) {
	if New("", "", "") != nil {
		t.Error("no sinks configured should yield a nil Notifier")
	}
	if New("token-only", "", "") != nil {
		t.Error("telegram token without chat id should not activate")
	}
	if New("", "", "http://hook") == nil {
		t.Error("webhook alone should activate")
	}
	if New("tok", "chat", "") == nil {
		t.Error("telegram token + chat id should activate")
	}
	// nil receiver Send is a safe no-op
	var n *Notifier
	if err := n.Send(context.Background(), "e", "text"); err != nil {
		t.Errorf("nil Send = %v, want nil", err)
	}
}

func TestSendBothSinks(t *testing.T) {
	var mu sync.Mutex
	var telegramBody, webhookBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/bottok123/sendMessage"):
			_ = json.NewDecoder(r.Body).Decode(&telegramBody)
			_, _ = w.Write([]byte(`{"ok": true}`))
		case r.URL.Path == "/hook":
			_ = json.NewDecoder(r.Body).Decode(&webhookBody)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	old := telegramAPIBase
	telegramAPIBase = srv.URL
	defer func() { telegramAPIBase = old }()

	n := New("tok123", "42", srv.URL+"/hook")
	if err := n.Send(context.Background(), "watch_available", "hello\nworld"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if telegramBody["chat_id"] != "42" || telegramBody["text"] != "hello\nworld" {
		t.Errorf("telegram payload = %v", telegramBody)
	}
	if webhookBody["event"] != "watch_available" || webhookBody["message"] != "hello\nworld" {
		t.Errorf("webhook payload = %v", webhookBody)
	}
}

// A Telegram HTTP-200 {"ok": false} must surface as an error, and transport
// errors must not leak the bot token (it lives in the URL path).
func TestSendTelegramErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok": false, "description": "chat not found"}`))
	}))
	old := telegramAPIBase
	telegramAPIBase = srv.URL
	defer func() { telegramAPIBase = old }()

	n := New("s3cret-token", "42", "")
	if err := n.Send(context.Background(), "e", "x"); err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("ok:false should error with the description, got %v", err)
	}

	srv.Close() // now force a transport error
	err := n.Send(context.Background(), "e", "x")
	if err == nil {
		t.Fatal("closed server should error")
	}
	if strings.Contains(err.Error(), "s3cret-token") {
		t.Fatalf("bot token leaked into error: %v", err)
	}
}

// TestSendPhoto verifies the photo path: Telegram gets sendPhoto with the
// URL + caption, the webhook gets an extra "image" field.
func TestSendPhoto(t *testing.T) {
	var mu sync.Mutex
	var photoBody, webhookBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendPhoto"):
			_ = json.NewDecoder(r.Body).Decode(&photoBody)
			_, _ = w.Write([]byte(`{"ok": true}`))
		case r.URL.Path == "/hook":
			_ = json.NewDecoder(r.Body).Decode(&webhookBody)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	old := telegramAPIBase
	telegramAPIBase = srv.URL
	defer func() { telegramAPIBase = old }()

	n := New("tok", "42", srv.URL+"/hook")
	if err := n.SendPhoto(context.Background(), "watch_available", "https://img/x.jpg", "caption here"); err != nil {
		t.Fatalf("SendPhoto: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if photoBody["photo"] != "https://img/x.jpg" || photoBody["caption"] != "caption here" {
		t.Errorf("sendPhoto payload = %v", photoBody)
	}
	if webhookBody["image"] != "https://img/x.jpg" || webhookBody["message"] != "caption here" {
		t.Errorf("webhook payload = %v", webhookBody)
	}
}

// TestSendPhotoFallsBackToText: when Telegram refuses the photo (dead URL,
// unsupported type), the caption must still arrive as a plain message.
func TestSendPhotoFallsBackToText(t *testing.T) {
	var mu sync.Mutex
	var textSent string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendPhoto"):
			_, _ = w.Write([]byte(`{"ok": false, "description": "wrong file identifier"}`))
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			textSent, _ = b["text"].(string)
			_, _ = w.Write([]byte(`{"ok": true}`))
		}
	}))
	defer srv.Close()

	old := telegramAPIBase
	telegramAPIBase = srv.URL
	defer func() { telegramAPIBase = old }()

	n := New("tok", "42", "")
	if err := n.SendPhoto(context.Background(), "e", "https://img/dead.jpg", "the caption"); err != nil {
		t.Fatalf("SendPhoto with fallback: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if textSent != "the caption" {
		t.Errorf("fallback text = %q, want the caption", textSent)
	}
}

// Empty photo URL degrades to a plain Send.
func TestSendPhotoNoURL(t *testing.T) {
	var mu sync.Mutex
	sent := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			sent = true
			_, _ = w.Write([]byte(`{"ok": true}`))
		} else {
			t.Errorf("unexpected call to %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	old := telegramAPIBase
	telegramAPIBase = srv.URL
	defer func() { telegramAPIBase = old }()

	n := New("tok", "42", "")
	if err := n.SendPhoto(context.Background(), "e", "", "text only"); err != nil {
		t.Fatalf("SendPhoto no URL: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !sent {
		t.Error("expected a sendMessage")
	}
}

// TestSendPhotoButtons verifies inline buttons render as a one-row
// inline_keyboard on the Telegram payload and stay off the webhook's.
func TestSendPhotoButtons(t *testing.T) {
	var mu sync.Mutex
	var photoBody, webhookBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendPhoto"):
			_ = json.NewDecoder(r.Body).Decode(&photoBody)
			_, _ = w.Write([]byte(`{"ok": true}`))
		case r.URL.Path == "/hook":
			_ = json.NewDecoder(r.Body).Decode(&webhookBody)
		}
	}))
	defer srv.Close()
	old := telegramAPIBase
	telegramAPIBase = srv.URL
	defer func() { telegramAPIBase = old }()

	n := New("tok", "42", srv.URL+"/hook")
	err := n.SendPhoto(context.Background(), "watch_available", "https://img/x.jpg", "cap",
		Button{Text: "Grab", Data: "grab:abc"}, Button{Text: "Dismiss", Data: "dismiss:abc"})
	if err != nil {
		t.Fatalf("SendPhoto: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	kb, _ := photoBody["reply_markup"].(map[string]any)
	rows, _ := kb["inline_keyboard"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 keyboard row, got %v", photoBody["reply_markup"])
	}
	row := rows[0].([]any)
	if len(row) != 2 || row[0].(map[string]any)["callback_data"] != "grab:abc" {
		t.Errorf("keyboard row = %v", row)
	}
	if _, has := webhookBody["reply_markup"]; has {
		t.Errorf("webhook payload must not carry telegram keyboards: %v", webhookBody)
	}
}

// TestUpdatesAndAnswer exercises the callback plumbing: getUpdates decodes
// callback_query entries; AnswerCallback posts the id + toast.
func TestUpdatesAndAnswer(t *testing.T) {
	var mu sync.Mutex
	var gotOffset float64
	var answered map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			gotOffset, _ = b["offset"].(float64)
			_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":7,"callback_query":{
				"id":"cbid1","from":{"id":42},"data":"grab:scene-1",
				"message":{"message_id":9,"chat":{"id":42},"caption":"the caption"}}}]}`))
		case strings.HasSuffix(r.URL.Path, "/answerCallbackQuery"):
			_ = json.NewDecoder(r.Body).Decode(&answered)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()
	old := telegramAPIBase
	telegramAPIBase = srv.URL
	defer func() { telegramAPIBase = old }()

	n := New("tok", "42", "")
	ups, err := n.Updates(context.Background(), 5)
	if err != nil {
		t.Fatalf("Updates: %v", err)
	}
	mu.Lock()
	if gotOffset != 5 {
		t.Errorf("offset sent = %v, want 5", gotOffset)
	}
	mu.Unlock()
	if len(ups) != 1 || ups[0].ID != 7 || ups[0].Callback == nil {
		t.Fatalf("updates = %+v", ups)
	}
	cb := ups[0].Callback
	if cb.Data != "grab:scene-1" || cb.From.ID != 42 || cb.Message.Caption != "the caption" {
		t.Errorf("callback = %+v", cb)
	}
	if err := n.AnswerCallback(context.Background(), cb.ID, "done"); err != nil {
		t.Fatalf("AnswerCallback: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if answered["callback_query_id"] != "cbid1" || answered["text"] != "done" {
		t.Errorf("answer payload = %v", answered)
	}
}

// TestSendFallsBackToPlainOnParseError: a bad HTML entity must degrade to
// a plain-text resend (tags stripped), never a dropped notification.
func TestSendFallsBackToPlainOnParseError(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		if _, html := b["parse_mode"]; html {
			_, _ = w.Write([]byte(`{"ok": false, "description": "Bad Request: can't parse entities: unclosed tag"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok": true}`))
	}))
	defer srv.Close()
	old := telegramAPIBase
	telegramAPIBase = srv.URL
	defer func() { telegramAPIBase = old }()

	n := New("tok", "42", "")
	if err := n.Send(context.Background(), "e", "<b>broken &amp; bold"); err != nil {
		t.Fatalf("Send with parse fallback: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("expected HTML attempt + plain retry, got %d calls", len(bodies))
	}
	if bodies[1]["text"] != "broken & bold" {
		t.Errorf("plain retry text = %q, want tags stripped + entities unescaped", bodies[1]["text"])
	}
	if _, has := bodies[1]["parse_mode"]; has {
		t.Errorf("plain retry must not set parse_mode")
	}
}

// TestURLButton: a Button with URL set marshals as a link button (no
// callback_data) — the "Watch in Stash" affordance on landed-grab messages.
func TestURLButton(t *testing.T) {
	var mu sync.Mutex
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, _ = w.Write([]byte(`{"ok": true}`))
		}
	}))
	defer srv.Close()
	old := telegramAPIBase
	telegramAPIBase = srv.URL
	defer func() { telegramAPIBase = old }()

	n := New("tok", "42", "")
	err := n.sendTelegram(context.Background(), "landed",
		[]Button{{Text: "Watch", URL: "https://stash/scenes/9"}})
	if err != nil {
		t.Fatalf("sendTelegram: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	kb, _ := body["reply_markup"].(map[string]any)
	rows, _ := kb["inline_keyboard"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 keyboard row, got %v", body["reply_markup"])
	}
	btn := rows[0].([]any)[0].(map[string]any)
	if btn["url"] != "https://stash/scenes/9" {
		t.Errorf("url button = %v", btn)
	}
	if _, has := btn["callback_data"]; has {
		t.Errorf("url button must not carry callback_data: %v", btn)
	}
}
