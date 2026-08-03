// Package vpnguard watches the torrent client's network health — the
// missing safety layer of the gluetun-sidecar pattern forage runs on. The failure mode it
// exists for: gluetun-porn dies or is recreated or loses its tunnel, the client's
// WebUI stays reachable (published port / docker-proxy), and every
// torrent silently stalls — or worse, a misconfigured recovery leaks
// traffic. qBit's own transfer info is the tell: connection_status
// "disconnected" with a drained DHT means the client has no network at
// all.
//
// The guard's one intervention: while the client is down, AUTOMATED
// torrent grabs pause (usenet and manual grabs are untouched) and the
// transition is notified loudly. It never touches the client itself.
package vpnguard

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/notify"
	"github.com/ordureconnoisseur/forager/internal/qbit"
)

// Status is the guard's current verdict.
type Status struct {
	State     string `json:"state"` // ok | degraded | down | unconfigured
	Detail    string `json:"detail"`
	DHTNodes  int    `json:"dhtNodes"`
	CheckedAt int64  `json:"checkedAt"`
	// DownSince is set while state == down (the notify + UI banner
	// timestamp).
	DownSince int64 `json:"downSince,omitempty"`
}

// Guard holds the latest verdict; safe for concurrent readers.
type Guard struct {
	mu     sync.RWMutex
	status Status
}

func New() *Guard {
	return &Guard{status: Status{State: "unconfigured"}}
}

func (g *Guard) Status() Status {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.status
}

// TorrentGrabsAllowed is the wanted loop's gate: automation may hand
// the torrent client work unless the client is known-down.
// Unconfigured and degraded states stay permissive — the guard blocks
// on certainty, not suspicion.
func (g *Guard) TorrentGrabsAllowed() bool {
	return g.Status().State != "down"
}

// Run checks every interval and notifies on state transitions. Call in
// a goroutine from main; respects ctx.
func (g *Guard) Run(ctx context.Context, cfgFn func() config.Config, interval time.Duration, log *slog.Logger) {
	defer func() {
		if r := recover(); r != nil && log != nil {
			log.Error("vpnguard panicked", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	if interval <= 0 {
		interval = 2 * time.Minute
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		g.checkOnce(ctx, cfgFn(), log)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (g *Guard) checkOnce(ctx context.Context, cfg config.Config, log *slog.Logger) {
	prev := g.Status()
	next := g.evaluate(ctx, cfg)
	// Carry the down-since timestamp through consecutive down checks.
	if next.State == "down" {
		next.DownSince = prev.DownSince
		if next.DownSince == 0 {
			next.DownSince = time.Now().Unix()
		}
	}

	g.mu.Lock()
	g.status = next
	g.mu.Unlock()

	if prev.State == next.State {
		return
	}
	if log != nil {
		log.Info("vpnguard state", "from", prev.State, "to", next.State, "detail", next.Detail)
	}
	// Loud on the transitions that matter, once per transition. A first
	// check that lands on "down" (prev unconfigured) is startup noise,
	// not an outage.
	var key, msg string
	switch {
	case next.State == "down" && prev.State != "unconfigured":
		key = "vpnguard-down"
		msg = "⚠️ Torrent client has NO network (" + notify.Escape(next.Detail) + "). VPN tunnel down? Automated torrent grabs are paused until it recovers."
	case prev.State == "down" && next.State == "ok":
		key = "vpnguard-up"
		msg = "✅ Torrent client network recovered (" + notify.Escape(next.Detail) + "). Automated torrent grabs resume."
	default:
		return
	}
	_ = notify.New(cfg.TelegramBotToken, cfg.TelegramChatID, cfg.NotifyWebhookURL).Send(ctx, key, msg)
}

// evaluate performs one health read. Reachability failures are DOWN
// (in the sidecar pattern an unreachable API usually means the netns
// died); "firewalled" is degraded-but-working (no inbound, outbound
// fine).
func (g *Guard) evaluate(ctx context.Context, cfg config.Config) Status {
	now := time.Now().Unix()
	if cfg.QbitURL == "" {
		return Status{State: "unconfigured", Detail: "no torrent client configured", CheckedAt: now}
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c := qbit.New(cfg.QbitURL, cfg.QbitUsername, cfg.QbitPassword)
	ti, err := c.GetTransferInfo(cctx)
	if err != nil {
		return Status{State: "down", Detail: "client unreachable: " + err.Error(), CheckedAt: now}
	}
	st := Status{DHTNodes: ti.DHTNodes, CheckedAt: now}
	switch ti.ConnectionStatus {
	case "connected":
		st.State = "ok"
		st.Detail = fmt.Sprintf("connected, %d DHT nodes", ti.DHTNodes)
	case "firewalled":
		st.State = "degraded"
		st.Detail = fmt.Sprintf("firewalled (outbound only), %d DHT nodes", ti.DHTNodes)
	default: // "disconnected" and anything unknown
		st.State = "down"
		st.Detail = "client reports " + ti.ConnectionStatus
	}
	return st
}
