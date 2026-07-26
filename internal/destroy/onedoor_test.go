package destroy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestOneDoor enforces the package's reason to exist: every scene destroy
// and every recursive file removal in the daemon goes through an audited
// call site. This is what turns "I guarded the ones I found" into "there
// are no others" — on 2026-07-26 the codebase had five SceneDestroy call
// sites and only three carried the multi-file guard, because each had been
// guarded by hand.
//
// Adding a new destructive call site is allowed — by adding it to the
// allowlist here, which forces the author through this file and its
// comments before the build goes green.
func TestOneDoor(t *testing.T) {
	root := moduleRoot(t)

	checks := []struct {
		name    string
		pattern *regexp.Regexp
		allowed map[string]bool // repo-relative, forward slashes
	}{
		{
			name: "SceneDestroy calls",
			// Matches invocations (x.SceneDestroy(...)), not the method
			// definition or interface declarations.
			pattern: regexp.MustCompile(`\.\s*SceneDestroy\(`),
			allowed: map[string]bool{
				// The executor — the one door.
				"internal/destroy/destroy.go": true,
			},
		},
		{
			name:    "recursive disk removal",
			pattern: regexp.MustCompile(`os\.RemoveAll\(`),
			allowed: map[string]bool{
				// The grab purge's disk sweep: removes only the grab's own
				// placed path, previewed by the delete-preview endpoint.
				"internal/api/grab_detail.go": true,
				// Performer re-file: removes the OLD path after the placer
				// has re-placed the files under the new performer folder.
				"internal/api/grab_performer.go": true,
				// Premature-placement heal: removes a PARTIAL file forage
				// itself placed from a download that later turned out
				// unfinished — never a library copy. (Found by this very
				// test on its first run: it was the sixth destructive call
				// site in a codebase believed to have five. Residual risk
				// tracked in docs/error-handling.md: a failed RemoveAll
				// here still clears PlacedPath.)
				"internal/poller/grabs.go": true,
			},
		},
	}

	for _, c := range checks {
		violations := map[string]int{}
		err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return rerr
			}
			rel = filepath.ToSlash(rel)
			// The stash client defines SceneDestroy; the definition is not a
			// call and the pattern won't match it, but keep the file readable
			// here for the day someone adds an internal helper call.
			body, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			if n := len(c.pattern.FindAll(body, -1)); n > 0 && !c.allowed[rel] {
				violations[rel] = n
			}
			return nil
		})
		if err != nil {
			t.Fatalf("%s: walk: %v", c.name, err)
		}
		for file, n := range violations {
			t.Errorf("%s: %s has %d unaudited call(s).\n"+
				"Destructive operations go through internal/destroy (build a Plan, Vet it, Execute it)\n"+
				"so the invariants and the deletion preview apply. If this site genuinely cannot,\n"+
				"add it to the allowlist in onedoor_test.go with a comment saying why.",
				c.name, file, n)
		}
	}
}

// moduleRoot walks up from the package directory to the go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}
