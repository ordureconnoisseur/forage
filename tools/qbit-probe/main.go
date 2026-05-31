// qbit-probe is a manual-use sanity check for the qBit client. Hits
// /api/v2/app/version (so we know the URL + auth work), then optionally
// adds a torrent URL passed via --add and reports the result.
//
//	FORAGER_QBIT_URL=http://mini:8083 \
//	  go run ./tools/qbit-probe
//
//	FORAGER_QBIT_URL=http://mini:8083 \
//	  go run ./tools/qbit-probe --add 'magnet:?xt=urn:btih:...'
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/qbit"
)

func main() {
	add := flag.String("add", "", "optional: torrent URL or magnet to add as a smoke test")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	if cfg.QbitURL == "" {
		log.Error("FORAGER_QBIT_URL not set")
		os.Exit(1)
	}

	client := qbit.New(cfg.QbitURL, cfg.QbitUsername, cfg.QbitPassword)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ver, err := client.Version(ctx)
	if err != nil {
		log.Error("version probe failed", "err", err)
		os.Exit(1)
	}
	log.Info("qbit reachable", "version", ver, "category", cfg.QbitCategory)

	if *add == "" {
		return
	}

	log.Info("adding torrent", "url", *add)
	if hash, err := client.AddTorrent(ctx, *add, cfg.QbitCategory); err != nil {
		log.Error("add failed", "err", err)
		os.Exit(1)
	} else {
		log.Info("added", "hash", hash)
	}
	fmt.Println("added — check qBit's UI")
}
