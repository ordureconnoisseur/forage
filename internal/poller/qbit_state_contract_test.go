package poller

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestQbitStateContract walks the recorded version-matrix fixtures
// (internal/qbit/testdata/contract) and asserts classifyQbitState places
// every state a real qBittorrent reported into a known class — never
// "unknown". This is how the 4.x→5.x pausedDL→stoppedDL rename would have
// been caught before a user hit it: record the new version, and any state
// spelling the classifier doesn't know fails here.
func TestQbitStateContract(t *testing.T) {
	root := filepath.Join("..", "qbit", "testdata", "contract")
	dirs, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("no qbit contract fixtures: %v", err)
	}
	seen := 0
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, d.Name(), "info.json"))
		if err != nil {
			t.Fatal(err)
		}
		var torrents []struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(raw, &torrents); err != nil {
			t.Fatalf("%s: decode info.json: %v", d.Name(), err)
		}
		for _, tor := range torrents {
			seen++
			if cls := classifyQbitState(tor.State); cls == "unknown" || cls == "" {
				t.Errorf("%s: state %q classifies as %q — teach classifyQbitState the new spelling",
					d.Name(), tor.State, cls)
			}
		}
	}
	if seen == 0 {
		t.Fatal("fixtures contained no torrents — the contract recording is broken")
	}
}
