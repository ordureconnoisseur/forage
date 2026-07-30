package configstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// applyPatch is field-by-field, so a new StoredConfig field that nobody adds a
// clause for is accepted on the wire, reported as saved, and silently dropped.
// That is exactly what happened to hideMalePerformers: the daemon answered
// {"ok":true} and wrote {} to disk. This drives the real Set → disk → reload
// path rather than assembling a struct in memory.
func TestSetPersistsHideMalePerformers(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	if err := st.Set(Patch{HideMalePerformers: &yes}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]any
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk["hideMalePerformers"] != true {
		t.Fatalf("config.json = %s, want hideMalePerformers true", raw)
	}

	// And it survives a reopen — the daemon reads this at boot.
	st2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := st2.Get().HideMalePerformers
	if got == nil || !*got {
		t.Fatalf("after reopen = %v, want true", got)
	}

	// Switching it back off must persist too, not read as "unset".
	no := false
	if err := st2.Set(Patch{HideMalePerformers: &no}); err != nil {
		t.Fatal(err)
	}
	st3, _ := Open(dir)
	got = st3.Get().HideMalePerformers
	if got == nil || *got {
		t.Fatalf("after switching off = %v, want an explicit false", got)
	}
}
