// prowlarr-probe is a manual-use sanity check for the Prowlarr client.
// Hits /api/v1/system/status and runs one search against the
// configured indexers; prints the top results so we can see what real
// release names look like before wiring it into the daemon.
//
//	FORAGER_PROWLARR_URL=http://mini:9696 \
//	FORAGER_PROWLARR_API_KEY=... \
//	  go run ./tools/prowlarr-probe --query="adriana chechik"
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/prowlarr"
)

func main() {
	var (
		query      = flag.String("query", "", "search term (required)")
		categories = flag.String("categories", "", "comma-separated category IDs (empty = no filter)")
		limit      = flag.Int("limit", 15, "max results to print")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	if cfg.ProwlarrURL == "" {
		log.Error("FORAGER_PROWLARR_URL not set")
		os.Exit(1)
	}
	if *query == "" {
		fmt.Fprintln(os.Stderr, "usage: --query=<term>")
		os.Exit(2)
	}

	cats, err := parseCSVInts(*categories)
	if err != nil {
		log.Error("bad --categories", "err", err)
		os.Exit(2)
	}
	if len(cats) == 0 {
		cats = cfg.ProwlarrCategories
	}

	client := prowlarr.New(cfg.ProwlarrURL, cfg.ProwlarrAPIKey)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if v, err := client.Status(ctx); err != nil {
		log.Error("status probe failed", "err", err)
		os.Exit(1)
	} else {
		log.Info("prowlarr reachable", "version", v)
	}

	log.Info("searching", "term", *query, "categories", cats)
	t0 := time.Now()
	results, err := client.Search(ctx, *query, cats)
	if err != nil {
		log.Error("search failed", "err", err)
		os.Exit(1)
	}
	log.Info("search done", "count", len(results), "elapsed", time.Since(t0))

	fmt.Println()
	n := len(results)
	if n > *limit {
		n = *limit
	}
	for i := 0; i < n; i++ {
		r := results[i]
		fmt.Printf("  %2d. [%s/%s] pop=%d size=%s\n",
			i+1, r.Indexer, r.Protocol, r.Popularity, humanSize(r.Size))
		fmt.Printf("      %s\n", r.Title)
		fmt.Printf("      %s · cats=%v\n", r.PublishDate, r.Categories)
	}
}

func parseCSVInts(s string) ([]int, error) {
	if s == "" {
		return nil, nil
	}
	var out []int
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", p, err)
		}
		out = append(out, n)
	}
	return out, nil
}

func humanSize(b int64) string {
	const k = 1024
	if b < k {
		return fmt.Sprintf("%dB", b)
	}
	units := []string{"K", "M", "G", "T"}
	v := float64(b)
	i := -1
	for v >= k && i < len(units)-1 {
		v /= k
		i++
	}
	return fmt.Sprintf("%.1f%sB", v, units[i])
}
