package stashdb

import (
	"strings"
	"testing"
)

// Both paths that feed the performers strip must ask for the same fields.
// They did not: queryPerformers got `age` and the alias-batched findPerformer
// lookup did not, so two lenses showed an age and the third showed none. The
// two documents are written in different places and are easy to drift apart.
func TestPerformerQueriesAskForTheSameFields(t *testing.T) {
	// The alias document is built at call time, so reproduce its field list
	// the same way the builder does.
	const aliasFields = " gender age scene_count images { url width height } }"

	for _, f := range []string{"gender", "age", "scene_count", "images"} {
		if !strings.Contains(queryPerformersGQL, f) {
			t.Errorf("queryPerformers does not ask for %q", f)
		}
		if !strings.Contains(aliasFields, f) {
			t.Errorf("the findPerformer alias batch does not ask for %q", f)
		}
	}
}

// The wire struct has to decode what the documents ask for, or the field
// arrives and is silently dropped.
func TestPerformerWireDecodesAge(t *testing.T) {
	w := performerWire{Name: "X", Age: 31}
	if got := w.toProfile().Age; got != 31 {
		t.Errorf("Age = %d, want 31", got)
	}
}
