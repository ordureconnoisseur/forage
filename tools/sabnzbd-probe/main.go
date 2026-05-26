// sabnzbd-probe is a manual sanity check for the SAB client. Hits
// /api?mode=version to confirm URL + key, then prints queue + recent
// history. Optional --add <url> queues an NZB as a smoke test.
//
//	FORAGER_SAB_URL=http://mini:8080 FORAGER_SAB_API_KEY=... \
//	  go run ./tools/sabnzbd-probe [--add <nzb-url>]
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/sabnzbd"
)

func main() {
	add := flag.String("add", "", "optional: NZB URL to add as a smoke test")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	if cfg.SabURL == "" || cfg.SabAPIKey == "" {
		log.Error("FORAGER_SAB_URL + FORAGER_SAB_API_KEY required")
		os.Exit(1)
	}

	client := sabnzbd.New(cfg.SabURL, cfg.SabAPIKey)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ver, err := client.Version(ctx)
	if err != nil {
		log.Error("version probe failed", "err", err)
		os.Exit(1)
	}
	log.Info("sab reachable", "version", ver, "category", cfg.SabCategory)

	q, err := client.Queue(ctx)
	if err != nil {
		log.Error("queue", "err", err)
	} else {
		fmt.Printf("\nqueue (%d):\n", len(q))
		for _, it := range q {
			fmt.Printf("  [%s] %.1f%% %s · cat=%s · %s\n", it.Status, it.Percentage, it.NzoID, it.Category, it.Name)
		}
	}

	h, err := client.History(ctx, 5)
	if err != nil {
		log.Error("history", "err", err)
	} else {
		fmt.Printf("\nrecent history (%d):\n", len(h))
		for _, it := range h {
			fmt.Printf("  [%s] cat=%s · %s\n", it.Status, it.Category, it.Name)
		}
	}

	if *add != "" {
		log.Info("adding NZB", "url", *add)
		nzoID, err := client.AddURL(ctx, *add, cfg.SabCategory)
		if err != nil {
			log.Error("add failed", "err", err)
			os.Exit(1)
		}
		fmt.Println("added → nzo_id:", nzoID)
	}
}
