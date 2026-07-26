package config
import "testing"
func TestNormalizeLogOnly(t *testing.T) {
  if got := normalizePackKeep(" LOG-ONLY "); got != "log-only" { t.Fatalf("got %q", got) }
  if got := normalizePackKeep("junk"); got != "existing" { t.Fatalf("junk -> %q", got) }
}
