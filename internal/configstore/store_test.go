package configstore

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestAtomicWrite_NoPartialFiles verifies the write path never leaves
// config.json in a half-written state. After every successful Set the
// file on disk must parse as valid JSON and contain the patched value.
func TestAtomicWrite_NoPartialFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Hammer Set 500 times with alternating values. Inspect the file
	// between iterations: at no point should it be empty, missing, or
	// non-JSON. tmp+rename guarantees this; the test pins the contract.
	for i := 0; i < 500; i++ {
		val := "v-" + string(rune(i%26+'a'))
		if err := s.Set(Patch{QbitCategory: strPtr(val)}); err != nil {
			t.Fatalf("set #%d: %v", i, err)
		}
		b, err := os.ReadFile(filepath.Join(dir, fileName))
		if err != nil {
			t.Fatalf("read after #%d: %v", i, err)
		}
		if len(b) == 0 {
			t.Fatalf("empty config.json after save #%d", i)
		}
		var parsed StoredConfig
		if err := json.Unmarshal(b, &parsed); err != nil {
			t.Fatalf("parse after #%d: %v", i, err)
		}
		if parsed.QbitCategory == nil || *parsed.QbitCategory != val {
			t.Fatalf("save #%d: want QbitCategory=%q, got %v", i, val, parsed.QbitCategory)
		}
	}
}

// TestConcurrentSet_NoCorruption stress-tests parallel writers. The
// store's RWMutex serializes them; correctness means the final file
// equals one of the patches submitted, never a mix.
func TestConcurrentSet_NoCorruption(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	const N = 50
	candidates := make([]string, N)
	for i := range candidates {
		candidates[i] = "writer-" + string(rune(i%26+'a'))
	}
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Set(Patch{StashURL: strPtr(candidates[i])}); err != nil {
				t.Errorf("writer %d: %v", i, err)
			}
		}()
	}
	wg.Wait()
	b, err := os.ReadFile(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatal(err)
	}
	var got StoredConfig
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("final file not valid JSON: %v\nbody: %s", err, b)
	}
	if got.StashURL == nil {
		t.Fatal("StashURL missing from final file")
	}
	found := false
	for _, c := range candidates {
		if *got.StashURL == c {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("final StashURL %q not in candidate set — possible corruption", *got.StashURL)
	}
}

// TestBackupRotation verifies that .bak.1/.2/.3 hold the previous N
// saves and that .bak.4 never appears.
func TestBackupRotation(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Five saves with distinct values; .bak.1 should be save #4's
	// content, .bak.2 → #3, .bak.3 → #2, and #1 should be evicted.
	for i := 1; i <= 5; i++ {
		val := "save-" + string(rune('0'+i))
		if err := s.Set(Patch{QbitCategory: strPtr(val)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, bk := range []struct {
		name string
		want string
	}{
		{fileName, "save-5"},
		{fileName + ".bak.1", "save-4"},
		{fileName + ".bak.2", "save-3"},
		{fileName + ".bak.3", "save-2"},
	} {
		b, err := os.ReadFile(filepath.Join(dir, bk.name))
		if err != nil {
			t.Fatalf("read %s: %v", bk.name, err)
		}
		var c StoredConfig
		if err := json.Unmarshal(b, &c); err != nil {
			t.Fatalf("parse %s: %v", bk.name, err)
		}
		if c.QbitCategory == nil || *c.QbitCategory != bk.want {
			t.Fatalf("%s: want %q got %v", bk.name, bk.want, c.QbitCategory)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, fileName+".bak.4")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf(".bak.4 should never exist; got err=%v", err)
	}
}

// TestPatchSemantics confirms nil fields preserve existing values and
// non-nil fields (including empty strings) overwrite.
func TestPatchSemantics(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Set qbit + sab.
	if err := s.Set(Patch{QbitCategory: strPtr("qcat"), SabCategory: strPtr("scat")}); err != nil {
		t.Fatal(err)
	}
	// Update only sab — qbit must be preserved.
	if err := s.Set(Patch{SabCategory: strPtr("scat2")}); err != nil {
		t.Fatal(err)
	}
	got := s.Get()
	if got.QbitCategory == nil || *got.QbitCategory != "qcat" {
		t.Fatalf("nil patch field should preserve: want qcat, got %v", got.QbitCategory)
	}
	if got.SabCategory == nil || *got.SabCategory != "scat2" {
		t.Fatalf("non-nil patch should overwrite: want scat2, got %v", got.SabCategory)
	}
	// Empty-string patch clears the field (still non-nil pointer).
	if err := s.Set(Patch{SabCategory: strPtr("")}); err != nil {
		t.Fatal(err)
	}
	got = s.Get()
	if got.SabCategory == nil || *got.SabCategory != "" {
		t.Fatalf("empty-string patch should set empty: got %v", got.SabCategory)
	}
}

// TestPermissions ensures persisted files are 0600 — they hold API
// keys, so other-readable would be a serious leak.
func TestPermissions(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set(Patch{StashAPIKey: strPtr("sensitive-value")}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("config.json mode = %o, want 0600", mode)
	}
}

func strPtr(s string) *string { return &s }
