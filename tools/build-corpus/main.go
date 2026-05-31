// Build a matcher test corpus from real qBit + SAB history.
//
// Joins (release name, downloaded file) pairs from the download clients
// against the user's Stash library, emitting (release, expected
// StashDB scene id) entries. Output is the ground truth for matcher
// regression tests — same workflow the matcher faces in production
// (parse a Prowlarr release name) but with verified answers because
// the user already grabbed + scanned each scene.
//
// Designed to run inside the forager Docker container so it sees the
// same qBit/SAB/Stash endpoints (host.docker.internal:...). Output
// goes to /data/corpus.yaml by default — the bind-mounted data dir
// the user can scp back to their dev machine.
//
//	docker exec forager /build-corpus -out /data/corpus.yaml
//
// Output is YAML, hand-rolled (avoids the yaml.v3 dep just for one
// tool). Each entry holds the release name, file path, source
// (qbit | sab), and the verified StashDB scene id.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/qbit"
	"github.com/ordureconnoisseur/forager/internal/sabnzbd"
	"github.com/ordureconnoisseur/forager/internal/stash"
)

// Categories worth considering. qBit grabs from the *arr stack often
// use category names like "sonarr"/"radarr" — those aren't porn
// scenes. Empty category passes (SAB sometimes returns no category).
var relevantCats = map[string]bool{
	"manual":  true,
	"porn":    true,
	"forager": true,
	"":        true,
}

// Skip releases whose name screams "compilation/pack" — these don't
// map to a single StashDB scene so they aren't useful test data.
var skipReleaseNames = regexp.MustCompile(`(?i)(compil|\bpack\b|vol\.?\s*\d+|best\.?of|\bcollection\b|megapack|siterip)`)

// slug helper: lowercase + collapse non-alphanum to '-'.
var slugCleaner = regexp.MustCompile(`[^a-z0-9]+`)

type entry struct {
	ID            string
	Release       string
	ExpectedScene string
	Source        string
	FilePath      string
}

// candidate is the pre-Stash-lookup shape: a release-name claim from
// some source (qBit / SAB history / orphaned .torrent file / a confirmed
// grab). The main loop joins each candidate against Stash; matches become
// entries.
//
// ExpectedScene, when set, is a known-good StashDB id (from a confirmed
// grab's actual_stashdb_id) — the candidate then skips the path→Stash
// lookup entirely. This is what makes grabs the highest-fidelity source:
// the release string is exactly what the matcher saw at search time and
// the answer is the phash-verified result, no reconstruction.
type candidate struct {
	Release       string
	FilePath      string
	Source        string // "qbit" | "sab" | "torrent" | "grabs-search"
	Category      string
	ExpectedScene string // pre-known StashDB id; skips the Stash path lookup
}

type stats struct {
	candidates       int
	wrongCategory    int
	compilation      int
	emptyFilePath    int
	noStashMatch     int
	noStashDBID      int
	dupScene         int
	stashLookupError int
}

func main() {
	outPath := flag.String("out", "/data/corpus.yaml", "output YAML path")
	limit := flag.Int("limit", 0, "stop after N successful entries (0 = no limit)")
	verbose := flag.Bool("v", false, "log per-candidate decisions to stderr")
	torrentsDir := flag.String("torrents-dir", "", "optional directory of .torrent files to also harvest; each torrent's info.name is treated as a release name. Implies -include-downloads-style data (tagged 'torrent').")
	foragerURL := flag.String("forager-url", "http://localhost:7979", "forager daemon base URL; confirmed grabs are harvested from its /grabs API as the highest-fidelity corpus source (real search release_title + phash-verified scene id). Empty to skip.")
	includeDownloads := flag.Bool("include-downloads", false, "also harvest qBit/SAB download NAMES (tagged 'qbit'/'sab'). These are post-renamed FILENAMES the matcher never sees in production (it runs on Prowlarr release titles at search time), so they make the benchmark unrepresentative. Off by default: the corpus is built from confirmed search-grabs only.")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		die("config: %v", err)
	}

	stashC := stash.New(cfg.StashURL, cfg.StashAPIKey)

	var qbitC *qbit.Client
	if *includeDownloads && cfg.QbitURL != "" {
		qbitC = qbit.New(cfg.QbitURL, cfg.QbitUsername, cfg.QbitPassword)
	}
	var sabC *sabnzbd.Client
	if *includeDownloads && cfg.SabURL != "" && cfg.SabAPIKey != "" {
		sabC = sabnzbd.New(cfg.SabURL, cfg.SabAPIKey)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	var candidates []candidate

	if qbitC != nil {
		fmt.Fprintf(os.Stderr, "listing qBit torrents (download names — unrepresentative)…\n")
		ts, err := qbitC.ListTorrents(ctx, qbit.ListOpts{Filter: "all"})
		if err != nil {
			warn("qbit list: %v", err)
		} else {
			for _, t := range ts {
				candidates = append(candidates, candidate{
					Release:  t.Name,
					FilePath: t.ContentPath,
					Source:   "qbit",
					Category: t.Category,
				})
			}
			fmt.Fprintf(os.Stderr, "  %d torrents\n", len(ts))
		}
	}

	if sabC != nil {
		fmt.Fprintf(os.Stderr, "pulling SAB history (download names — unrepresentative)…\n")
		history, err := sabC.History(ctx, 1000)
		if err != nil {
			warn("sab history: %v", err)
		} else {
			for _, h := range history {
				// Only completed SAB items have a real on-disk path.
				if !strings.EqualFold(h.Status, "Completed") {
					continue
				}
				candidates = append(candidates, candidate{
					Release:  h.Name,
					FilePath: h.Path,
					Source:   "sab",
					Category: h.Category,
				})
			}
			fmt.Fprintf(os.Stderr, "  %d completed SAB items\n", len(history))
		}
	}

	if *torrentsDir != "" {
		fmt.Fprintf(os.Stderr, "scanning .torrent files in %s…\n", *torrentsDir)
		torrentCands, parseErrors := harvestTorrents(*torrentsDir)
		candidates = append(candidates, torrentCands...)
		fmt.Fprintf(os.Stderr, "  %d torrent files parsed (%d parse errors)\n", len(torrentCands), parseErrors)
	}

	// Confirmed grabs are the gold source: release_title is the exact
	// string the matcher saw during search, and actual_stashdb_id is the
	// phash-verified answer. Only grabs that came through search (have a
	// release_indexer) are tagged "grabs-search" — those exercise the
	// matcher's real production input. Pulled via the daemon's HTTP API so
	// we never touch the live WAL DB directly.
	if *foragerURL != "" {
		fmt.Fprintf(os.Stderr, "pulling confirmed grabs from %s…\n", *foragerURL)
		grabCands, err := harvestGrabs(ctx, *foragerURL)
		if err != nil {
			warn("grabs harvest: %v", err)
		} else {
			candidates = append(candidates, grabCands...)
			fmt.Fprintf(os.Stderr, "  %d confirmed search-grabs\n", len(grabCands))
		}
	}

	var s stats
	s.candidates = len(candidates)
	seen := map[string]bool{}
	seenScenes := map[string]bool{}
	var entries []entry

	for _, c := range candidates {
		if *limit > 0 && len(entries) >= *limit {
			break
		}
		if c.Category != "" && !relevantCats[strings.ToLower(c.Category)] {
			s.wrongCategory++
			continue
		}
		if skipReleaseNames.MatchString(c.Release) {
			s.compilation++
			if *verbose {
				fmt.Fprintf(os.Stderr, "  skip (compilation): %s\n", trim(c.Release, 80))
			}
			continue
		}

		// Resolve the expected scene id. Grab candidates already carry the
		// phash-verified answer; everything else is joined against Stash by
		// the downloaded file's basename.
		stashID := c.ExpectedScene
		if stashID == "" {
			needle := filepath.Base(c.FilePath)
			if needle == "" || needle == "." || needle == "/" {
				// Fall back to the release name — sometimes SAB history
				// entries don't surface a path even when status=Completed.
				needle = c.Release
			}
			if needle == "" {
				s.emptyFilePath++
				continue
			}
			// Dedup at the request level: many qBit/SAB entries share the
			// same content path after the user re-grabbed or moved things.
			if seen[needle] {
				continue
			}
			seen[needle] = true

			scene, err := stashC.FindSceneByPathContains(ctx, needle)
			if err != nil {
				s.stashLookupError++
				warn("stash lookup %q: %v", trim(needle, 60), err)
				continue
			}
			if scene == nil {
				s.noStashMatch++
				if *verbose {
					fmt.Fprintf(os.Stderr, "  skip (no stash match): %s\n", trim(c.Release, 80))
				}
				continue
			}
			if scene.StashDBID == "" {
				s.noStashDBID++
				continue
			}
			stashID = scene.StashDBID
		}

		if seenScenes[stashID] {
			s.dupScene++
			continue
		}
		seenScenes[stashID] = true

		entries = append(entries, entry{
			ID:            slugify(c.Release),
			Release:       c.Release,
			ExpectedScene: stashID,
			Source:        c.Source,
			FilePath:      c.FilePath,
		})
		if *verbose {
			fmt.Fprintf(os.Stderr, "  ✓ %s → %s\n", trim(c.Release, 70), shortID(stashID))
		}
	}

	// Sort entries by source then release for stable diffs.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Source != entries[j].Source {
			return entries[i].Source < entries[j].Source
		}
		return entries[i].Release < entries[j].Release
	})

	f, err := os.Create(*outPath)
	if err != nil {
		die("create %s: %v", *outPath, err)
	}
	defer f.Close()
	writeYAML(f, entries)

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "=== Build-corpus summary ===")
	fmt.Fprintf(os.Stderr, "candidates seen:             %d\n", s.candidates)
	fmt.Fprintf(os.Stderr, "skipped (wrong category):    %d\n", s.wrongCategory)
	fmt.Fprintf(os.Stderr, "skipped (compilation/pack):  %d\n", s.compilation)
	fmt.Fprintf(os.Stderr, "skipped (no file path):      %d\n", s.emptyFilePath)
	fmt.Fprintf(os.Stderr, "skipped (no stash match):    %d\n", s.noStashMatch)
	fmt.Fprintf(os.Stderr, "skipped (no stashdb id):     %d\n", s.noStashDBID)
	fmt.Fprintf(os.Stderr, "skipped (dup scene):         %d\n", s.dupScene)
	fmt.Fprintf(os.Stderr, "stash lookup errors:         %d\n", s.stashLookupError)
	fmt.Fprintf(os.Stderr, "entries written:             %d\n", len(entries))
	fmt.Fprintf(os.Stderr, "output:                      %s\n", *outPath)
}

func writeYAML(f *os.File, entries []entry) {
	fmt.Fprintln(f, "# forager matcher test corpus")
	fmt.Fprintln(f, "# Auto-generated by tools/build-corpus from confirmed search-grabs")
	fmt.Fprintln(f, "# (source: grabs-search) — the real Prowlarr release titles the")
	fmt.Fprintln(f, "# matcher scored at search time, with phash-verified scene ids. Pass")
	fmt.Fprintln(f, "# -include-downloads to also fold in qBit/SAB download NAMES (source:")
	fmt.Fprintln(f, "# qbit/sab) — those are post-renamed filenames the matcher never sees")
	fmt.Fprintln(f, "# in production, so they make recall look worse than it is.")
	fmt.Fprintln(f, "# Do NOT commit publicly — contains real scene IDs from your library.")
	fmt.Fprintf(f, "# Generated: %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintln(f, "#")
	fmt.Fprintln(f, "# Each entry is one verified (release name, expected StashDB scene id)")
	fmt.Fprintln(f, "# pair. The matcher should return expected_scene_id at rank 1 when")
	fmt.Fprintln(f, "# fed the `release` string.")
	fmt.Fprintln(f)
	for _, e := range entries {
		fmt.Fprintf(f, "- id: %s\n", e.ID)
		fmt.Fprintf(f, "  release: %s\n", yamlString(e.Release))
		fmt.Fprintf(f, "  expected_scene_id: %s\n", e.ExpectedScene)
		fmt.Fprintf(f, "  source: %s\n", e.Source)
		if e.FilePath != "" {
			fmt.Fprintf(f, "  file_path: %s\n", yamlString(e.FilePath))
		}
		fmt.Fprintln(f)
	}
}

// yamlString returns a safely-quoted YAML scalar. Uses single-quoted
// style (only escape: ” for a literal single quote). Handles the
// common release-name characters (brackets, dots, dashes, spaces) and
// avoids the YAML 1.2 implicit-type traps double-quoted strings have.
func yamlString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func slugify(s string) string {
	out := slugCleaner.ReplaceAllString(strings.ToLower(s), "-")
	out = strings.Trim(out, "-")
	if len(out) > 60 {
		out = out[:60]
	}
	if out == "" {
		out = "unnamed"
	}
	return out
}

func trim(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func warn(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "WARN: "+format+"\n", args...)
}
func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ERROR: "+format+"\n", args...)
	os.Exit(1)
}

// harvestGrabs pulls confirmed grabs from the daemon's /grabs API and
// emits a candidate for each one that came through search (has a
// release_indexer) and carries a phash-verified scene id. These are the
// highest-fidelity corpus rows: release is the literal Prowlarr title the
// matcher scored at search time, expected is the verified answer.
// Manual/adopted grabs (no indexer) are excluded — their release strings
// are download/file names, not the search input the matcher serves.
func harvestGrabs(ctx context.Context, baseURL string) ([]candidate, error) {
	url := strings.TrimRight(baseURL, "/") + "/grabs?limit=1000"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("grabs %d", resp.StatusCode)
	}
	var body struct {
		Grabs []struct {
			ReleaseTitle    string `json:"release_title"`
			ReleaseIndexer  string `json:"release_indexer"`
			ActualStashDBID string `json:"actual_stashdb_id"`
			Kind            string `json:"kind"`
		} `json:"grabs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	var out []candidate
	for _, g := range body.Grabs {
		// Need the search input + a verified answer; skip packs (no single
		// scene) and manual/adopted grabs (no indexer = not a search input).
		if g.ReleaseTitle == "" || g.ActualStashDBID == "" || g.ReleaseIndexer == "" || g.Kind == "pack" {
			continue
		}
		out = append(out, candidate{
			Release:       g.ReleaseTitle,
			Source:        "grabs-search",
			ExpectedScene: g.ActualStashDBID,
		})
	}
	return out, nil
}

// harvestTorrents walks dir (non-recursively) for .torrent files,
// parses each, and emits a candidate per file using the bencoded
// info.name as both the "release name" and the basename needle the
// Stash-side lookup will search by. Files that fail to parse are
// counted and skipped; the function never aborts on per-file errors
// because the directory might contain unrelated files.
func harvestTorrents(dir string) (cands []candidate, parseErrors int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		warn("read torrents dir %q: %v", dir, err)
		return nil, 0
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".torrent") {
			continue
		}
		path := filepath.Join(dir, name)
		torrentName, err := extractTorrentName(path)
		if err != nil {
			parseErrors++
			continue
		}
		if torrentName == "" {
			continue
		}
		cands = append(cands, candidate{
			Release: torrentName,
			// FilePath empty — there's no file on disk for orphaned
			// torrents. The Stash-lookup falls back to the release name
			// as its needle (see the main loop's `needle = c.Release`
			// branch when basename ends up empty).
			FilePath: "",
			Source:   "torrent",
			Category: "",
		})
	}
	return cands, parseErrors
}
