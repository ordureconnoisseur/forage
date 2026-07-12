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
	"time"

	_ "modernc.org/sqlite"

	"github.com/ordureconnoisseur/forager/internal/clientpool"
	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/db"
	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/watches"
)

// TestNotifyLoopWatermarks pins the loop's contract: a missing watermark
// initializes to now WITHOUT sending (no backlog replay); events newer than
// the watermark send exactly once as a single digest message; a second tick
// with no new events sends nothing.
func TestNotifyLoopWatermarks(t *testing.T) {
	ctx := context.Background()
	dbh, err := db.Open(t.TempDir() + "/n.db")
	if err != nil {
		t.Fatal(err)
	}
	defer dbh.Close()

	var mu sync.Mutex
	var messages []map[string]any
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		messages = append(messages, m)
		mu.Unlock()
	}))
	defer hook.Close()

	pool := clientpool.New()
	pool.Reload(config.Config{NotifyWebhookURL: hook.URL})

	s := &Server{
		db:      dbh,
		pool:    pool,
		grabs:   grabs.NewRepo(dbh),
		watches: watches.NewRepo(dbh),
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// A watch that went available and a failed grab BEFORE the first tick:
	// both must be swallowed by watermark initialization, not replayed.
	if err := s.watches.Add(ctx, watches.Watch{StashDBID: "old", Title: "Old Scene", Target: watches.TargetAny}); err != nil {
		t.Fatal(err)
	}
	if err := s.watches.MarkAvailable(ctx, "old", "Old.Release", "http://dl/old", "idx", "torrent", 1, nil); err != nil {
		t.Fatal(err)
	}
	s.tickNotify(ctx)
	mu.Lock()
	if len(messages) != 0 {
		t.Fatalf("first tick must initialize watermarks silently, sent %d message(s)", len(messages))
	}
	mu.Unlock()

	// New events after the watermark: one available watch + one failed grab.
	time.Sleep(1100 * time.Millisecond) // found_at/updated_at are unix seconds; step past the watermark
	if err := s.watches.Add(ctx, watches.Watch{StashDBID: "fresh", Title: "Fresh Scene", PerformerName: "Hazel Moore", Target: watches.TargetAny}); err != nil {
		t.Fatal(err)
	}
	if err := s.watches.MarkAvailable(ctx, "fresh", "Fresh.Release.1080p", "http://dl/fresh", "idx", "torrent", 1, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.grabs.Insert(ctx, grabs.Grab{ReleaseTitle: "Dead.Release", Status: "failed", Reason: "tracker said no"}); err != nil {
		t.Fatal(err)
	}
	s.tickNotify(ctx)

	mu.Lock()
	got := len(messages)
	var events []string
	for _, m := range messages {
		events = append(events, m["event"].(string))
	}
	mu.Unlock()
	if got != 2 {
		t.Fatalf("expected 2 messages (one per category), got %d (%v)", got, events)
	}

	// No new events → no new messages.
	s.tickNotify(ctx)
	mu.Lock()
	defer mu.Unlock()
	if len(messages) != 2 {
		t.Errorf("idle tick re-sent: %d messages total", len(messages))
	}
}

// TestNotifyLoopSendsSceneImage: an available watch carrying a StashDB
// cover URL notifies per-scene with the image attached (webhook "image"
// field; Telegram side is covered in internal/notify).
func TestNotifyLoopSendsSceneImage(t *testing.T) {
	ctx := context.Background()
	dbh, err := db.Open(t.TempDir() + "/ni.db")
	if err != nil {
		t.Fatal(err)
	}
	defer dbh.Close()

	var mu sync.Mutex
	var messages []map[string]any
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		messages = append(messages, m)
		mu.Unlock()
	}))
	defer hook.Close()

	pool := clientpool.New()
	pool.Reload(config.Config{NotifyWebhookURL: hook.URL})
	s := &Server{
		db:      dbh,
		pool:    pool,
		grabs:   grabs.NewRepo(dbh),
		watches: watches.NewRepo(dbh),
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	s.tickNotify(ctx) // initialize watermarks

	time.Sleep(1100 * time.Millisecond)
	if err := s.watches.Add(ctx, watches.Watch{StashDBID: "img-scene", Title: "Covered Scene", Target: watches.TargetAny}); err != nil {
		t.Fatal(err)
	}
	if err := s.watches.BackfillMeta(ctx, "img-scene", "Covered Scene", "2026-07-05", "Tushy Raw", "https://stashdb.org/images/abc"); err != nil {
		t.Fatal(err)
	}
	if err := s.watches.MarkAvailable(ctx, "img-scene", "Covered.Release.2160p", "http://dl/x", "idx", "torrent", 1, nil); err != nil {
		t.Fatal(err)
	}
	s.tickNotify(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	m := messages[0]
	if m["image"] != "https://stashdb.org/images/abc" {
		t.Errorf("image = %v, want the stashdb cover URL", m["image"])
	}
	msg, _ := m["message"].(string)
	if !strings.Contains(msg, "Covered Scene") || !strings.Contains(msg, "Tushy Raw") || !strings.Contains(msg, "Covered.Release.2160p") {
		t.Errorf("caption missing details: %q", msg)
	}
}

// TestNotifyFailedGrabsGroupedAndTruncated pins the failure-digest shape:
// paragraph-length release titles truncate, and a batch failing for one
// cause states the reason ONCE in the headline instead of per line.
func TestNotifyFailedGrabsGroupedAndTruncated(t *testing.T) {
	ctx := context.Background()
	dbh, err := db.Open(t.TempDir() + "/nf.db")
	if err != nil {
		t.Fatal(err)
	}
	defer dbh.Close()

	var mu sync.Mutex
	var messages []map[string]any
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		messages = append(messages, m)
		mu.Unlock()
	}))
	defer hook.Close()

	pool := clientpool.New()
	pool.Reload(config.Config{NotifyWebhookURL: hook.URL})
	s := &Server{
		db:      dbh,
		pool:    pool,
		grabs:   grabs.NewRepo(dbh),
		watches: watches.NewRepo(dbh),
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	s.tickNotify(ctx) // initialize watermarks

	time.Sleep(1100 * time.Millisecond)
	longTitle := "Hookup Hotshot: Be A Slut, Do Whatever U Want (Bryan Gozzling, Evil Angel) [2016, Gonzo, 1080p, WEB-DL] (Split Scenes) (Zoey Laine, Carmen Callaway, Chloe Coutoure, Lily Rader) RD: 20.06.2016."
	for _, title := range []string{longTitle, "Short.Release.2160p", "[MV] Natasha Nixx"} {
		if _, err := s.grabs.Insert(ctx, grabs.Grab{ReleaseTitle: title, Status: "failed", Reason: "qbit state=error"}); err != nil {
			t.Fatal(err)
		}
	}
	s.tickNotify(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(messages) != 1 {
		t.Fatalf("expected 1 digest message, got %d", len(messages))
	}
	msg, _ := messages[0]["message"].(string)
	if !strings.Contains(msg, "3 grabs failed — qbit state=error") {
		t.Errorf("headline should carry the shared reason once: %q", msg)
	}
	if strings.Count(msg, "qbit state=error") != 1 {
		t.Errorf("reason repeated per line: %q", msg)
	}
	if strings.Contains(msg, "Lily Rader") {
		t.Errorf("long title not truncated: %q", msg)
	}
	if !strings.Contains(msg, "…") {
		t.Errorf("expected ellipsis on the truncated title: %q", msg)
	}
	for _, line := range strings.Split(msg, "\n") {
		if n := len([]rune(line)); n > 100 {
			t.Errorf("line too long (%d runes): %q", n, line)
		}
	}
}

// TestBuildWatchCaption pins the structured caption: labeled lines with
// bold HTML labels, resolution parsed from the release name, human size,
// indexer, and escaped dynamic content.
func TestBuildWatchCaption(t *testing.T) {
	wt := watches.Watch{
		StashDBID:     "sid",
		Title:         "Oil Massage <Deluxe> & More",
		PerformerName: "Kenzie Reeves",
		StudioName:    "ATK Girlfriends",
		Date:          "2018-05-24",
		FoundTitle:    "ATKGirlfriends.18.05.24.Kenzie.Reeves.XXX.1080p.MP4-KTR",
		FoundProtocol: "torrent",
		FoundIndexer:  "PornoLab",
		FoundSize:     2 << 30, // 2GB
	}
	got := buildWatchCaption(wt)
	for _, want := range []string{
		"🎬 <b>Oil Massage &lt;Deluxe&gt; &amp; More</b>",
		"👤 Kenzie Reeves",
		"🏛 ATK Girlfriends · 2018-05-24",
		"<b>Release:</b> ATKGirlfriends.18.05.24.Kenzie.Reeves.XXX.1080p.MP4-KTR",
		"<b>Quality:</b> 1080p · torrent",
		"<b>Size:</b> 2.0GB",
		"<b>Indexer:</b> PornoLab",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("caption missing %q:\n%s", want, got)
		}
	}
	// Field order: header block, blank separator, release facts.
	if !strings.Contains(got, "2018-05-24\n\n<b>Release:</b>") {
		t.Errorf("expected blank line between header and release facts:\n%s", got)
	}
}

// TestNotifyLandedGrabs pins the "scene landed in Stash" sweep: a grab
// confirmed before the watermark initializes silently; one confirmed after
// it notifies once (with scene metadata resolved from the StashDB scene
// cache); packs and mismatches never notify; an idle tick re-sends nothing.
func TestNotifyLandedGrabs(t *testing.T) {
	ctx := context.Background()
	dbh, err := db.Open(t.TempDir() + "/nl.db")
	if err != nil {
		t.Fatal(err)
	}
	defer dbh.Close()

	var mu sync.Mutex
	var messages []map[string]any
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		messages = append(messages, m)
		mu.Unlock()
	}))
	defer hook.Close()

	pool := clientpool.New()
	pool.Reload(config.Config{NotifyWebhookURL: hook.URL})
	s := &Server{
		db:      dbh,
		pool:    pool,
		grabs:   grabs.NewRepo(dbh),
		watches: watches.NewRepo(dbh),
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// Scene metadata in the cache so the message carries the real title.
	if _, err := dbh.ExecContext(ctx, `
		INSERT INTO stashdb_scene (stashdb_id, title, studio_name, release_date, image_url)
		VALUES ('scene-1', 'Happy Accident', 'Mom Comes First', '2025-12-20', 'https://img/cover.jpg')`); err != nil {
		t.Fatal(err)
	}

	// Confirmed BEFORE the first tick: swallowed by watermark init.
	if _, err := s.grabs.Insert(ctx, grabs.Grab{
		ReleaseTitle: "Old.Release", Status: "confirmed",
		ActualStashDBID: "scene-old", ConfirmedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	s.tickNotify(ctx)
	mu.Lock()
	if len(messages) != 0 {
		t.Fatalf("first tick must initialize watermark silently, sent %d", len(messages))
	}
	mu.Unlock()

	time.Sleep(1100 * time.Millisecond) // confirmed_at is unix seconds; step past the watermark
	now := time.Now().Unix()
	// The landing that must notify — plus a pack and a mismatch that must not.
	if _, err := s.grabs.Insert(ctx, grabs.Grab{
		ReleaseTitle: "MomComesFirst.Danielle.Renae.Happy.Accident.1080p", Status: "confirmed",
		ActualStashDBID: "scene-1", PerformerName: "Danielle Renae",
		PlacedPath: "/data/porn/Media/Danielle Renae/happy.mp4", ConfirmedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.grabs.Insert(ctx, grabs.Grab{
		ReleaseTitle: "Some.Pack", Status: "confirmed", Kind: "pack", ConfirmedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.grabs.Insert(ctx, grabs.Grab{
		ReleaseTitle: "Wrong.Scene", Status: "mismatched", ConfirmedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	s.tickNotify(ctx)

	mu.Lock()
	if len(messages) != 1 {
		t.Fatalf("expected exactly 1 landed message, got %d: %v", len(messages), messages)
	}
	m := messages[0]
	if m["event"] != "grab_landed" {
		t.Errorf("event = %v, want grab_landed", m["event"])
	}
	text, _ := m["message"].(string)
	if !strings.Contains(text, "Happy Accident") || !strings.Contains(text, "Danielle Renae") {
		t.Errorf("message missing scene metadata: %q", text)
	}
	if img, _ := m["image"].(string); img != "https://img/cover.jpg" {
		t.Errorf("image = %q, want the cached cover", img)
	}
	mu.Unlock()

	// Idle tick: nothing new.
	s.tickNotify(ctx)
	mu.Lock()
	defer mu.Unlock()
	if len(messages) != 1 {
		t.Errorf("idle tick re-sent: %d messages total", len(messages))
	}
}

// The deferred digest fires once per grab, on its FIRST deferral only:
// re-defers (higher attempt counts) stay silent, and the ending arrives
// via the landed/failed digests instead.
func TestNotifyDeferredGrabsFirstDeferralOnly(t *testing.T) {
	ctx := context.Background()
	dbh, err := db.Open(t.TempDir() + "/nd.db")
	if err != nil {
		t.Fatal(err)
	}
	defer dbh.Close()

	var mu sync.Mutex
	var messages []map[string]any
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		messages = append(messages, m)
		mu.Unlock()
	}))
	defer hook.Close()

	pool := clientpool.New()
	pool.Reload(config.Config{NotifyWebhookURL: hook.URL})
	s := &Server{
		db:      dbh,
		pool:    pool,
		grabs:   grabs.NewRepo(dbh),
		watches: watches.NewRepo(dbh),
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	s.tickNotify(ctx) // initialize watermarks
	time.Sleep(1100 * time.Millisecond)

	// First deferral: attempts=1 -> one digest.
	id, err := s.grabs.Insert(ctx, grabs.Grab{ReleaseTitle: "Blipped.Release", Status: "queued"})
	if err != nil {
		t.Fatal(err)
	}
	g, _ := s.grabs.Get(ctx, id)
	g.Status = "deferred"
	g.Attempts = 1
	g.Reason = "torrent add: fetch torrent 429"
	g.NextRetryAt = time.Now().Add(time.Minute).Unix()
	if err := s.grabs.Update(ctx, *g); err != nil {
		t.Fatal(err)
	}
	s.tickNotify(ctx)

	mu.Lock()
	if len(messages) != 1 || messages[0]["event"] != "grabs_deferred" {
		t.Fatalf("want 1 grabs_deferred message, got %d (%v)", len(messages), messages)
	}
	mu.Unlock()

	// Re-deferral (attempts=2) bumps updated_at but must stay silent.
	time.Sleep(1100 * time.Millisecond)
	g, _ = s.grabs.Get(ctx, id)
	g.Attempts = 2
	g.NextRetryAt = time.Now().Add(5 * time.Minute).Unix()
	if err := s.grabs.Update(ctx, *g); err != nil {
		t.Fatal(err)
	}
	s.tickNotify(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(messages) != 1 {
		t.Fatalf("re-deferral must not re-notify: got %d messages", len(messages))
	}
}
