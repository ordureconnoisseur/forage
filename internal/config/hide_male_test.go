package config

import (
	"testing"

	"github.com/ordureconnoisseur/forager/internal/configstore"
)

// A stored *bool must round-trip in BOTH directions. If an explicit false were
// dropped as "unset", the toggle would be one-way: switching the filter on
// would persist, switching it back off would silently reload as on.
func TestHideMalePerformersRoundTrip(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name   string
		stored *bool
		env    bool
		want   bool
		source FieldSource
	}{
		{"unset defaults off", nil, false, false, SourceDefault},
		{"stored true", &yes, false, true, SourceJSON},
		{"stored false beats env true", &no, true, false, SourceJSON},
		{"env true with nothing stored", nil, true, true, SourceEnv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := BootstrapConfig{set: map[string]bool{}}
			b.HideMalePerformers = tc.env
			if tc.env {
				b.set["hideMalePerformers"] = true
			}
			cfg, src := Compose(b, configstore.StoredConfig{HideMalePerformers: tc.stored})
			if cfg.HideMalePerformers != tc.want {
				t.Errorf("HideMalePerformers = %v, want %v", cfg.HideMalePerformers, tc.want)
			}
			if src["hideMalePerformers"] != tc.source {
				t.Errorf("source = %v, want %v", src["hideMalePerformers"], tc.source)
			}
		})
	}
}
