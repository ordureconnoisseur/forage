// stashdb-probe is a manual-use tool: it hits StashDB with a hand-picked
// scene's performer + studio + date and prints the candidate matches,
// then runs the equivalent full-text searchScenes and prints those too.
//
// Used to verify the QueryScenes / SearchScenes methods work end-to-end
// against the real API before the matcher consumes them.
//
//	FORAGER_STASHDB_API_KEY=... \
//	  go run ./tools/stashdb-probe \
//	    --performer=<stashdb-performer-uuid> \
//	    --studio=<stashdb-studio-uuid> \
//	    --date=2024-05-15 \
//	    --search="Brazzers Adriana Chechik 2024.05.15"
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

func main() {
	var (
		performerCSV = flag.String("performer", "", "comma-separated StashDB performer UUIDs (INCLUDES_ALL)")
		studioCSV    = flag.String("studio", "", "comma-separated StashDB studio UUIDs")
		date         = flag.String("date", "", "YYYY-MM-DD exact-match date")
		search       = flag.String("search", "", "term for full-text searchScenes")
		limit        = flag.Int("limit", 10, "max results per call")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}

	client := stashdb.New(cfg.StashDBURL, cfg.StashDBAPIKey)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if *performerCSV != "" || *studioCSV != "" || *date != "" {
		q := stashdb.SceneQuery{
			PerformerIDs: splitCSV(*performerCSV),
			StudioIDs:    splitCSV(*studioCSV),
			Date:         *date,
			PerPage:      *limit,
		}
		log.Info("queryScenes", "performers", q.PerformerIDs, "studios", q.StudioIDs, "date", q.Date)
		res, err := client.QueryScenes(ctx, q)
		if err != nil {
			log.Error("queryScenes failed", "err", err)
		} else {
			fmt.Printf("\n=== queryScenes — count=%d, showing %d ===\n", res.Count, len(res.Scenes))
			printScenes(res.Scenes)
		}
	}

	if *search != "" {
		log.Info("searchScenes", "term", *search)
		res, err := client.SearchScenes(ctx, *search, *limit)
		if err != nil {
			log.Error("searchScenes failed", "err", err)
		} else {
			fmt.Printf("\n=== searchScenes — %d results ===\n", len(res))
			printScenes(res)
		}
	}

	if *performerCSV == "" && *studioCSV == "" && *date == "" && *search == "" {
		fmt.Println("usage: pass at least one of --performer / --studio / --date / --search")
		os.Exit(2)
	}
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func printScenes(scenes []stashdb.Scene) {
	for i, s := range scenes {
		studio := "—"
		if s.Studio != nil {
			studio = s.Studio.Name
		}
		perfs := make([]string, 0, len(s.Performers))
		for _, p := range s.Performers {
			n := p.Name
			if p.As != "" && p.As != p.Name {
				n += " (as " + p.As + ")"
			}
			perfs = append(perfs, n)
		}
		fmt.Printf("  %2d. %s — %s [%s]\n", i+1, s.Date, s.Title, studio)
		fmt.Printf("      id=%s  performers=%s\n", s.ID, strings.Join(perfs, ", "))
	}
}
