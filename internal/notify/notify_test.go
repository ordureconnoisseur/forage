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
