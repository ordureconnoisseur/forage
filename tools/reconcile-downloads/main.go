// reconcile-downloads answers a question nothing in forage answers today:
// which files in the download tree still need to be there.
//
//	docker exec qbittorrent-porn /reconcile-downloads \
//	    -downloads /data/porn/downloads -library /data/porn/Media \
//	    -qbit http://localhost:8080 -out /tmp/reconcile.tsv
//
// It REPORTS. It never deletes, and there is deliberately no --apply: the
// decision of what to remove from a 5 TB folder is not one a tool should make
// on its first run.
//
// Why it exists. The download folder on the reference library had grown to
// 5.1 TB across 12,459 files dating back to 2017, and no part of the system
// knew which of them mattered. Three separate causes were tangled together:
//
//   - Re-encodes. The transcode pipeline writes a new file at the library
//     path, which breaks the hardlink and strands the full-size original in
//     the download folder. 1,671 files, 1.26 TB, superseded by their own
//     replacements.
//   - Never imported. Downloads that completed and were never filed at all:
//     900 files, 0.46 TB, the only copy of content that was paid for and
//     forgotten. These must never be deleted; they need FILING.
//   - Still seeding. Independent of both, and the reason deletion needs its
//     own gate rather than a bucket.
//
// Two hard-won rules are enforced in classify.go rather than left to the
// caller. Redundancy and seeding are separate axes, because a redundant file
// a torrent is serving must not be touched. And a file is only redundant when
// something proves the content exists elsewhere: inode matching alone missed
// 253 real duplicates (copies, not hardlinks), and CIFS link counts disagreed
// with themselves by 4,042 files across two runs of the same query.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func main() {
	var (
		downloads  = flag.String("downloads", "", "root of the download tree (required)")
		library    = flag.String("library", "", "root of the Stash library (required)")
		qbitURL    = flag.String("qbit", "", "qBittorrent base URL, for seeding status")
		extraIndex = flag.String("extra-index", "", "comma-separated name<TAB>size files listing content that legitimately lives elsewhere (another drive)")
		out        = flag.String("out", "", "write the full per-file report here as TSV")
		jsonOut    = flag.String("json", "", "write the full per-file report here as JSON")
		minDepth   = flag.Int("seed-min-depth", 4, "reject torrent content paths shallower than this many separators")
		noSeeding  = flag.Bool("ignore-seeding", false, "proceed without qBittorrent, treating nothing as seeded (UNSAFE for cleanup)")
	)
	flag.Parse()
	if *downloads == "" || *library == "" {
		fmt.Fprintln(os.Stderr, "both -downloads and -library are required")
		flag.Usage()
		os.Exit(2)
	}

	// Seeding status is a safety input, not a nicety. Without it every
	// actively-served file looks disposable, so the tool refuses to guess.
	var seeds []string
	switch {
	case *qbitURL != "":
		paths, err := fetchContentPaths(*qbitURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "qBittorrent: %v\n", err)
			fmt.Fprintln(os.Stderr, "refusing to report without seeding status; pass -ignore-seeding to override")
			os.Exit(1)
		}
		seeds = SeedingPaths(paths, *minDepth)
		fmt.Printf("torrents: %d content paths, %d usable (%d rejected as empty or too broad)\n",
			len(paths), len(seeds), len(paths)-len(seeds))
	case *noSeeding:
		fmt.Println("WARNING: -ignore-seeding: nothing is treated as seeded, so the")
		fmt.Println("         reclaimable figure below is NOT safe to act on.")
	default:
		fmt.Fprintln(os.Stderr, "pass -qbit URL, or -ignore-seeding to accept an unsafe report")
		os.Exit(2)
	}

	fmt.Printf("indexing library %s ...\n", *library)
	lib, n, err := indexTree(*library)
	if err != nil {
		fmt.Fprintf(os.Stderr, "index library: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  %d files\n", n)

	var extra []*Index
	for _, p := range splitList(*extraIndex) {
		ix, count, err := loadIndexFile(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "extra index %s: %v\n", p, err)
			os.Exit(1)
		}
		fmt.Printf("  extra index %s: %d files\n", p, count)
		extra = append(extra, ix)
	}

	fmt.Printf("walking downloads %s ...\n", *downloads)
	var entries []Entry
	err = filepath.WalkDir(*downloads, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil // unreadable corners must not abort a 12,000 file walk
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		id, ok := fileID(info)
		entries = append(entries, Classify(path, info.Size(), id, ok, lib, extra,
			IsSeeded(path, seeds)))
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "walk downloads: %v\n", err)
		os.Exit(1)
	}

	report(entries)
	if *out != "" {
		if err := writeTSV(*out, entries); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", *out, err)
			os.Exit(1)
		}
		fmt.Printf("\nfull report: %s\n", *out)
	}
	if *jsonOut != "" {
		if err := writeJSON(*jsonOut, entries); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", *jsonOut, err)
			os.Exit(1)
		}
		fmt.Printf("full report: %s\n", *jsonOut)
	}
}

const tb = 1099511627776.0

func report(entries []Entry) {
	type agg struct {
		files, seeding  int
		bytes, freeable int64
	}
	by := map[string]*agg{}
	var total, reclaimable int64
	var reclaimFiles int
	for _, e := range entries {
		a := by[e.Bucket]
		if a == nil {
			a = &agg{}
			by[e.Bucket] = a
		}
		a.files++
		a.bytes += e.Size
		total += e.Size
		if e.Seeding {
			a.seeding++
		}
		if e.Reclaimable() {
			a.freeable += e.Size
			reclaimable += e.Size
			reclaimFiles++
		}
	}
	order := []string{BucketSuperseded, BucketDuplicate, BucketVariant,
		BucketElsewhere, BucketOrphan, BucketInProgress, BucketNonMedia}
	fmt.Printf("\n%-14s %8s %10s %8s %12s\n", "bucket", "files", "size", "seeding", "reclaimable")
	for _, b := range order {
		a := by[b]
		if a == nil {
			continue
		}
		fmt.Printf("%-14s %8d %9.2fT %8d %11.2fT\n",
			b, a.files, float64(a.bytes)/tb, a.seeding, float64(a.freeable)/tb)
	}
	fmt.Printf("\ntotal %d files, %.2f TB\n", len(entries), float64(total)/tb)
	fmt.Printf("reclaimable: %d files, %.2f TB (redundant AND not seeding)\n",
		reclaimFiles, float64(reclaimable)/tb)
	if a := by[BucketOrphan]; a != nil && a.files > 0 {
		fmt.Printf("\n%d files (%.2f TB) are the ONLY copy and were never filed.\n",
			a.files, float64(a.bytes)/tb)
		fmt.Println("Those need placing, not deleting.")
	}
}

func writeTSV(path string, entries []Entry) error {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Bucket != entries[j].Bucket {
			return entries[i].Bucket < entries[j].Bucket
		}
		return entries[i].Size > entries[j].Size
	})
	var b strings.Builder
	b.WriteString("bucket\tseeding\treclaimable\tsize\tlibrary_size\tpath\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "%s\t%t\t%t\t%d\t%d\t%s\n",
			e.Bucket, e.Seeding, e.Reclaimable(), e.Size, e.LibrarySize, e.Path)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func writeJSON(path string, entries []Entry) error {
	b, err := json.MarshalIndent(entries, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func indexTree(root string) (*Index, int, error) {
	ix := NewIndex()
	n := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		id, ok := fileID(info)
		ix.Add(path, info.Size(), id, ok)
		n++
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	if n == 0 {
		// An empty library index would mark every download an orphan, which
		// reads as "nothing is redundant" and is the safe direction, but it
		// almost certainly means the path is wrong.
		return nil, 0, fmt.Errorf("no files under %s: wrong path or unmounted?", root)
	}
	return ix, n, nil
}

// loadIndexFile reads "name<TAB>size" lines, which is what a find on another
// machine produces for content that legitimately lives off this filesystem.
func loadIndexFile(path string) (*Index, int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	ix := NewIndex()
	n := 0
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		i := strings.LastIndex(line, "\t")
		if i < 0 {
			continue
		}
		size, err := strconv.ParseInt(strings.TrimSpace(line[i+1:]), 10, 64)
		if err != nil {
			continue
		}
		ix.Add(line[:i], size, 0, false)
		n++
	}
	if n == 0 {
		return nil, 0, errors.New("no usable name<TAB>size lines")
	}
	return ix, n, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// fetchContentPaths reads every torrent's content_path. No auth: this runs
// against a local qBittorrent whose WebUI is already reachable without one.
func fetchContentPaths(base string) ([]string, error) {
	c := &http.Client{Timeout: 60 * time.Second}
	resp, err := c.Get(strings.TrimRight(base, "/") + "/api/v2/torrents/info")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("torrents/info: %s", resp.Status)
	}
	var torrents []struct {
		ContentPath string `json:"content_path"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&torrents); err != nil {
		return nil, err
	}
	if len(torrents) == 0 {
		// Zero torrents makes everything look unseeded. That is a plausible
		// truth and a plausible misconfiguration, and the difference decides
		// whether files get deleted, so it does not pass silently.
		return nil, errors.New("qBittorrent reported no torrents at all")
	}
	out := make([]string, 0, len(torrents))
	for _, t := range torrents {
		out = append(out, t.ContentPath)
	}
	return out, nil
}
