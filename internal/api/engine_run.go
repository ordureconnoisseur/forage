package api

import (
	"context"

	"github.com/ordureconnoisseur/forager/internal/engine"
)

// Built-in torrent engine lifecycle. The engine activates whenever the
// composed config says "torrents with no qBittorrent" — at boot AND right
// after a config save, so the wizard's "use the built-in downloader"
// choice takes effect immediately, no restart. A configured qBittorrent
// always wins (Pool.Torrents); the engine simply never starts alongside
// one, and turning qBit off later activates the engine on the next save
// or boot.

// RunEngine retains the daemon lifetime ctx, performs the boot-time
// activation check, and stops the engine at shutdown. No-op without a
// wired engine (tests).
func (s *Server) RunEngine(ctx context.Context) {
	if s.engine == nil {
		return
	}
	s.engineMu.Lock()
	s.engineCtx = ctx
	s.engineMu.Unlock()
	s.ensureEngine()
	<-ctx.Done()
	s.engine.Close()
}

// ensureEngine starts the engine if the current config calls for it.
// Idempotent and cheap; called at boot and after every config save.
func (s *Server) ensureEngine() {
	if s.engine == nil {
		return
	}
	s.engineMu.Lock()
	ctx := s.engineCtx
	s.engineMu.Unlock()
	if ctx == nil || ctx.Err() != nil {
		return
	}
	cfg := s.composedConfig()
	if cfg.QbitURL != "" || cfg.DownloadRoot == "" {
		return
	}
	started, err := s.engine.EnsureStarted(ctx, cfg.DownloadRoot)
	if err != nil {
		s.log.Error("torrent engine start", "err", err)
		return
	}
	if started {
		s.pool.SetTorrentEngine(s.engine)
		go func() {
			defer s.recoverPanic("torrent engine")
			s.engine.Run(ctx)
		}()
		s.log.Info("built-in torrent engine active", "downloadDir", cfg.DownloadRoot)
	}
}

// engineField is the concrete type behind s.engine; declared here to keep
// server.go's field list dependency-light.
type engineField = *engine.Engine
