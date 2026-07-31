// refile-unsorted reports which files sitting in the library's Unsorted folder
// could be moved under a performer, and (only when told to) moves them.
//
//	docker exec forager /refile-unsorted                 # dry run, prints a plan
//	docker exec forager /refile-unsorted --apply         # actually moves them
//
// Dry run is the default and --apply is the only way past it, because the
// alternative is a tool that reorganises a library the first time someone runs
// it to see what it does.
//
// Where the names come from: a grab that landed in Unsorted still knows which
// scene it was, and watches.performers holds that scene's cast. Same source
// the placer now uses at grab time (see poller/place_folder.go); this applies
// it to the backlog that accumulated before it did.
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/db"
)

type plan struct {
	grabID    int64
	from      string
	performer string
	local     bool
}

func main() {
	apply := flag.Bool("apply", false, "actually move the files; without this the tool only prints what it would do")
	limit := flag.Int("limit", 0, "stop after N candidates (0 = all)")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		die("config: %v", err)
	}
	if cfg.LibraryRoot == "" {
		die("no library root configured; nothing to re-file")
	}
	dbh, err := db.Open(cfg.DBPath)
	if err != nil {
		die("open db: %v", err)
	}
	defer dbh.Close()

	// Only grabs the library actually holds under Unsorted. A grab whose file
	// has since been moved by hand is not ours to second-guess.
	rows, err := dbh.Query(`
		SELECT g.id, g.placed_path, g.predicted_stashdb_id, g.actual_stashdb_id
		FROM grabs g
		WHERE g.placed_path LIKE '%/Unsorted/%'
		ORDER BY g.id`)
	if err != nil {
		die("query grabs: %v", err)
	}
	defer rows.Close()

	// Drain the cursor FIRST. forage sets MaxOpenConns(1) — correct for
	// SQLite — so a lookup issued while this cursor is still open waits for a
	// connection the cursor itself is holding, and the process deadlocks. It
	// did, on the first real run.
	type row struct {
		id                int64
		path              string
		predicted, actual string
	}
	var candidates []row
	for rows.Next() {
		var id int64
		var path, predicted, actual sql.NullString
		if err := rows.Scan(&id, &path, &predicted, &actual); err != nil {
			die("scan: %v", err)
		}
		candidates = append(candidates, row{id, path.String, predicted.String, actual.String})
	}
	if err := rows.Err(); err != nil {
		die("rows: %v", err)
	}
	rows.Close()

	var plans []plan
	var (
		missing   int
		noCast    int
		total     int
		byUnknown int
	)
	for _, c := range candidates {
		id, predicted, actual := c.id, c.predicted, c.actual
		total++
		p := c.path
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			missing++ // moved or deleted since; leave it alone
			continue
		}
		names := castFor(dbh, predicted)
		if len(names) == 0 {
			names = castFor(dbh, actual)
		}
		name, local := pick(dbh, names)
		if name == "" {
			noCast++
			continue
		}
		if !local {
			byUnknown++
		}
		plans = append(plans, plan{grabID: id, from: p, performer: name, local: local})
		if *limit > 0 && len(plans) >= *limit {
			break
		}
	}

	fmt.Printf("grabs recorded under Unsorted: %d\n", total)
	fmt.Printf("  file no longer at that path: %d\n", missing)
	fmt.Printf("  no cast recorded for the scene: %d\n", noCast)
	fmt.Printf("  re-filable: %d  (%d under a performer the library already has, %d under a new folder)\n\n",
		len(plans), len(plans)-byUnknown, byUnknown)

	// Group by destination: a list of 3,000 individual moves tells you nothing,
	// whereas "412 files would go to Kenzie Reeves" is reviewable.
	byPerf := map[string]int{}
	for _, p := range plans {
		byPerf[p.performer]++
	}
	type kv struct {
		name string
		n    int
	}
	var top []kv
	for k, v := range byPerf {
		top = append(top, kv{k, v})
	}
	sort.Slice(top, func(i, j int) bool {
		if top[i].n != top[j].n {
			return top[i].n > top[j].n
		}
		return top[i].name < top[j].name
	})
	fmt.Printf("destinations (%d distinct performers):\n", len(top))
	for i, t := range top {
		if i >= 25 {
			fmt.Printf("  ... and %d more\n", len(top)-25)
			break
		}
		fmt.Printf("  %5d  %s\n", t.n, t.name)
	}

	if !*apply {
		fmt.Printf("\nDRY RUN. Nothing moved. Re-run with --apply to move %d files.\n", len(plans))
		return
	}

	moved, failed := 0, 0
	for _, p := range plans {
		dest := filepath.Join(cfg.LibraryRoot, sanitise(p.performer), filepath.Base(p.from))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "mkdir %s: %v\n", filepath.Dir(dest), err)
			failed++
			continue
		}
		if _, err := os.Stat(dest); err == nil {
			// Something is already there. Refuse rather than overwrite: the
			// whole point of this tool is not to lose files.
			fmt.Fprintf(os.Stderr, "skip %s: destination exists\n", dest)
			failed++
			continue
		}
		if err := os.Rename(p.from, dest); err != nil {
			fmt.Fprintf(os.Stderr, "move %s: %v\n", p.from, err)
			failed++
			continue
		}
		if _, err := dbh.Exec(
			`UPDATE grabs SET placed_path = ?, performer_name = ? WHERE id = ?`,
			dest, p.performer, p.grabID); err != nil {
			fmt.Fprintf(os.Stderr, "update grab %d: %v\n", p.grabID, err)
		}
		moved++
	}
	fmt.Printf("\nmoved %d, failed %d\n", moved, failed)
	fmt.Println("Stash still points at the old paths: run a library scan, then Stash's")
	fmt.Println("own cleanup, so the moved scenes re-attach at their new location.")
}

func castFor(dbh *sql.DB, sceneID string) []string {
	if sceneID == "" {
		return nil
	}
	var raw string
	if err := dbh.QueryRow(`SELECT performers FROM watches WHERE stashdb_id = ?`, sceneID).Scan(&raw); err != nil {
		return nil
	}
	var names []string
	if err := json.Unmarshal([]byte(raw), &names); err != nil {
		return nil
	}
	return names
}

// pick mirrors poller.placementPerformer: prefer a performer the library
// already has a folder for, else the billed lead.
func pick(dbh *sql.DB, names []string) (string, bool) {
	for _, n := range names {
		if n == "" {
			continue
		}
		var id string
		err := dbh.QueryRow(
			`SELECT stash_id FROM performer_cache WHERE name = ? COLLATE NOCASE AND stash_id != '' LIMIT 1`,
			n).Scan(&id)
		if err == nil && id != "" {
			return n, true
		}
	}
	for _, n := range names {
		if n != "" {
			return n, false
		}
	}
	return "", false
}

// sanitise mirrors the placer's own filename cleaning closely enough for a
// destination path; the placer owns the canonical version.
func sanitise(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', 0:
			return '_'
		}
		return r
	}, s)
	if s == "" {
		return "Unsorted"
	}
	return s
}

func die(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "refile-unsorted: "+f+"\n", a...)
	os.Exit(1)
}
