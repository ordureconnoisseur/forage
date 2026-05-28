package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ordureconnoisseur/forager/internal/api"
	"github.com/ordureconnoisseur/forager/internal/cache"
	"github.com/ordureconnoisseur/forager/internal/clientpool"
	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/configstore"
	"github.com/ordureconnoisseur/forager/internal/db"
	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/placer"
	"github.com/ordureconnoisseur/forager/internal/poller"
)

// Version is set at build time via -ldflags "-X main.Version=v0.1.0".
var Version = "dev"

func main() {
	bootstrap := config.LoadBootstrap()
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: bootstrap.LogLevel}))
	log.Info("forager starting", "version", Version, "addr", bootstrap.ListenAddr)

	database, err := db.Open(bootstrap.DBPath)
	if err != nil {
		log.Error("open db", "err", err)
		os.Exit(1)
	}
	defer database.Close()

	// configstore lives next to forager.db in the same data dir.
	store, err := configstore.Open(filepath.Dir(bootstrap.DBPath))
	if err != nil {
		log.Error("open configstore", "err", err)
		os.Exit(1)
	}

	cfg, _ := config.Compose(bootstrap, store.Get())

	// Pool owns every outbound client. Reload swaps the live refs
	// atomically; consumers read from accessors per-use so hot-swaps
	// propagate without restart. SetPlacer threads a named logger into
	// the placer; subsequent Reload calls preserve the logger via the
	// new placer instance (the placer ctor takes a logger param).
	pool := clientpool.New()
	pool.SetPlacer(placer.New(cfg.LibraryRoot, log.With("component", "placer")))
	pool.Reload(cfg)

	probeCtx, probeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	logBootProbes(probeCtx, pool, log)
	probeCancel()

	if config.IsConfigured(cfg) {
		log.Info("daemon configured via env / config.json")
	} else {
		log.Warn("daemon UNCONFIGURED — set Stash + StashDB credentials via the plugin Settings panel or .env")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Cache refresh goroutines hold the *Pool, not individual clients,
	// so hot-swapped Stash credentials reach the next tick automatically.
	go maybeRefreshOnBoot(ctx, pool, database, log.With("component", "cache"), cfg.CacheRefresh)
	go runRefreshTicker(ctx, pool, database, log.With("component", "cache"), cfg.CacheRefresh)
	// Trending refreshes on its own 1h cadence — StashDB's trending
	// list changes faster than the 12h performer-filtered cache.
	go runTrendingTicker(ctx, pool, database, log.With("component", "trending"))

	grabsRepo := grabs.NewRepo(database)
	// Phase B grabs poller — always start; the poller itself short-circuits
	// when no download clients are configured (pool.Qbit() / Sab() = nil).
	p := poller.New(grabsRepo, pool, log.With("component", "poller"),
		cfg.PollInterval, cfg.OrphanAfter)
	go p.Run(ctx)

	server := api.New(api.Options{
		DB:        database,
		Pool:      pool,
		Bootstrap: bootstrap,
		Store:     store,
		Grabs:     grabsRepo,
		Log:       log.With("component", "api"),
		Version:   Version,
	})

	httpServer := &http.Server{
		Addr:              bootstrap.ListenAddr,
		Handler:           server.Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Info("listening", "addr", bootstrap.ListenAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}

// logBootProbes hits each configured client at startup so the logs
// surface auth/connectivity issues immediately rather than at first
// request. Each probe is best-effort — failures log a warning, don't
// exit.
func logBootProbes(ctx context.Context, pool *clientpool.Pool, log *slog.Logger) {
	if sc := pool.Stash(); sc != nil {
		if ver, err := sc.Version(ctx); err != nil {
			log.Warn("stash probe failed", "err", err)
		} else {
			log.Info("stash reachable", "version", ver)
		}
	} else {
		log.Info("stash not configured")
	}
	if sdb := pool.StashDB(); sdb != nil {
		if user, err := sdb.Me(ctx); err != nil {
			log.Warn("stashdb probe failed", "err", err)
		} else {
			log.Info("stashdb reachable", "user", user)
		}
	} else {
		log.Info("stashdb not configured")
	}
	if pc := pool.Prowlarr(); pc != nil {
		if ver, err := pc.Status(ctx); err != nil {
			log.Warn("prowlarr probe failed", "err", err)
		} else {
			log.Info("prowlarr reachable", "version", ver)
		}
	} else {
		log.Info("prowlarr not configured")
	}
	settings := pool.Settings()
	if qb := pool.Qbit(); qb != nil {
		if ver, err := qb.Version(ctx); err != nil {
			log.Warn("qbit probe failed", "err", err)
		} else {
			log.Info("qbit reachable", "version", ver, "category", settings.QbitCategory)
		}
	} else {
		log.Info("qbit not configured")
	}
	if sb := pool.Sab(); sb != nil {
		if ver, err := sb.Version(ctx); err != nil {
			log.Warn("sab probe failed", "err", err)
		} else {
			log.Info("sab reachable", "version", ver, "category", settings.SabCategory)
		}
	} else {
		log.Info("sab not configured")
	}
	if pool.Placer().Configured() {
		log.Info("placer configured", "library_root", pool.Placer().LibraryRoot())
	} else {
		log.Info("placer not configured")
	}
}

// maybeRefreshOnBoot refreshes performer + studio + scene caches if
// any has never been populated or is older than the configured
// interval. Scenes piggy-back on the same interval; if it gets too
// slow we can split it into its own cadence later.
func maybeRefreshOnBoot(ctx context.Context, pool *clientpool.Pool, database *sql.DB, log *slog.Logger, interval time.Duration) {
	sc := pool.Stash()
	if sc == nil {
		return // daemon is unconfigured — refresh will retry on next interval tick
	}
	perfAt, _ := cache.PerformerRefreshedAt(ctx, database)
	studAt, _ := cache.StudioRefreshedAt(ctx, database)
	scenesAt, _ := cache.ScenesRefreshedAt(ctx, database)
	cutoff := time.Now().Add(-interval).Unix()
	if perfAt < cutoff {
		if err := cache.RefreshPerformers(ctx, sc, database, log.With("op", "performers")); err != nil {
			log.Error("boot performer refresh failed", "err", err)
		}
	}
	if studAt < cutoff {
		if err := cache.RefreshStudios(ctx, sc, pool.StashDB(), database, log.With("op", "studios")); err != nil {
			log.Error("boot studio refresh failed", "err", err)
		}
	}
	if scenesAt < cutoff {
		sdb := pool.StashDB()
		if sdb != nil {
			if err := cache.RefreshSceneCache(ctx, sc, sdb, database, log.With("op", "scenes")); err != nil {
				log.Error("boot scene refresh failed", "err", err)
			}
		}
	}
}

// runTrendingTicker refreshes StashDB's TRENDING scenes on a 1h
// cadence — faster than the 12h performer-filtered refresh because
// trending changes quickly. Boot-fires immediately so the trending
// list is populated even on a fresh daemon.
func runTrendingTicker(ctx context.Context, pool *clientpool.Pool, database *sql.DB, log *slog.Logger) {
	const interval = 1 * time.Hour
	tick := func() {
		sdb := pool.StashDB()
		if sdb == nil {
			return
		}
		if err := cache.RefreshTrending(ctx, pool.Stash(), sdb, database, log); err != nil {
			log.Error("trending refresh failed", "err", err)
		}
	}
	tick()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
		}
	}
}

// runRefreshTicker triggers a cache refresh every `interval`. Reads the
// Stash client from the Pool on each tick so hot-swapped credentials
// take effect without restarting the goroutine. Skips ticks when
// Stash isn't configured yet.
func runRefreshTicker(ctx context.Context, pool *clientpool.Pool, database *sql.DB, log *slog.Logger, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sc := pool.Stash()
			if sc == nil {
				continue
			}
			if err := cache.RefreshPerformers(ctx, sc, database, log.With("op", "performers")); err != nil {
				log.Error("ticker performer refresh failed", "err", err)
			}
			if err := cache.RefreshStudios(ctx, sc, pool.StashDB(), database, log.With("op", "studios")); err != nil {
				log.Error("ticker studio refresh failed", "err", err)
			}
			// Scene cache piggy-backs on the same tick. Needs StashDB
			// too — skip the run when it's not configured.
			if sdb := pool.StashDB(); sdb != nil {
				if err := cache.RefreshSceneCache(ctx, sc, sdb, database, log.With("op", "scenes")); err != nil {
					log.Error("ticker scene refresh failed", "err", err)
				}
			}
		}
	}
}
