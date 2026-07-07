package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/ordureconnoisseur/forager/internal/clientpool"
	"github.com/ordureconnoisseur/forager/internal/db"
	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/notify"
	"github.com/ordureconnoisseur/forager/internal/watches"
)

// fakeBotAPI records answerCallbackQuery and edit calls.
type fakeBotAPI struct {
	mu       sync.Mutex
	answers  []map[string]any
	edits    []map[string]any
	handler  http.Handler
	shutdown func()
}

func newFakeBotAPI(t *testing.T) *fakeBotAPI {
	t.Helper()
	f := &fakeBotAPI{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/answerCallbackQuery"):
			f.answers = append(f.answers, b)
		case strings.Contains(r.URL.Path, "/editMessage"):
			f.edits = append(f.edits, b)
		}
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	restore := notify.SetTelegramAPIBaseForTest(srv.URL)
	t.Cleanup(func() { restore(); srv.Close() })
	return f
}

func (f *fakeBotAPI) lastAnswerText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.answers) == 0 {
		return "<no answer>"
	}
	txt, _ := f.answers[len(f.answers)-1]["text"].(string)
	return txt
}

// TestTelegramCallbackHandling pins the button semantics: an unauthorized
// tap is acked but acts on nothing; Dismiss on an available watch ignores
// the release, flips it back to watching, and finalizes the message; Grab
// on a watch with nothing available reports the failure in the toast.
func TestTelegramCallbackHandling(t *testing.T) {
	ctx := context.Background()
	dbh, err := db.Open(t.TempDir() + "/tg.db")
	if err != nil {
		t.Fatal(err)
	}
	defer dbh.Close()
	bot := newFakeBotAPI(t)

	s := &Server{
		db:      dbh,
		pool:    clientpool.New(),
		grabs:   grabs.NewRepo(dbh),
		watches: watches.NewRepo(dbh),
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	n := notify.New("tok", "42", "")

	if err := s.watches.Add(ctx, watches.Watch{StashDBID: "scene-1", Title: "Scene One", Target: watches.TargetAny}); err != nil {
		t.Fatal(err)
	}
	if err := s.watches.MarkAvailable(ctx, "scene-1", "Rel.1080p", "http://dl/rel", "idx", "torrent", 1, nil); err != nil {
		t.Fatal(err)
	}

	cb := func(fromID int64, data string) *notify.Callback {
		c := &notify.Callback{ID: "cb", Data: data}
		c.From.ID = fromID
		c.Message.MessageID = 5
		c.Message.Chat.ID = 42
		c.Message.Caption = "original caption"
		return c
	}

	// Unauthorized user: acked, watch untouched.
	s.handleTelegramCallback(ctx, n, cb(99, "dismiss:scene-1"))
	if wt := s.findWatch(ctx, "scene-1"); wt.Status != watches.StatusAvailable {
		t.Fatalf("unauthorized tap mutated the watch: %q", wt.Status)
	}

	// Authorized dismiss: release ignored, back to watching, message edited.
	s.handleTelegramCallback(ctx, n, cb(42, "dismiss:scene-1"))
	wt := s.findWatch(ctx, "scene-1")
	if wt.Status != watches.StatusWatching {
		t.Fatalf("after dismiss: status=%q, want watching", wt.Status)
	}
	if !strings.Contains(bot.lastAnswerText(), "Dismissed") {
		t.Errorf("dismiss toast = %q", bot.lastAnswerText())
	}
	bot.mu.Lock()
	edits := len(bot.edits)
	var editedCaption string
	if edits > 0 {
		editedCaption, _ = bot.edits[len(bot.edits)-1]["caption"].(string)
	}
	bot.mu.Unlock()
	if edits != 1 || !strings.Contains(editedCaption, "Dismissed") {
		t.Errorf("expected one caption edit with the outcome, got %d (%q)", edits, editedCaption)
	}

	// Grab with nothing available (just dismissed): failure lands in the toast.
	s.handleTelegramCallback(ctx, n, cb(42, "grab:scene-1"))
	if !strings.Contains(bot.lastAnswerText(), "no available release") {
		t.Errorf("grab-unavailable toast = %q", bot.lastAnswerText())
	}
}
