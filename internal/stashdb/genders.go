package stashdb

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// genderBatch is how many performers one aliased query asks for. StashDB has
// no "find these ids" field, so the batch is built from aliased findPerformer
// selections in a single document — 378 un-owned performers on the reference
// instance became 8 requests instead of 378.
//
// Kept modest on purpose: a bigger document is one bigger failure, and the
// pacer charges per request, not per byte, so there is little left to win.
const genderBatch = 50

// PerformerGenders returns the StashDB gender for each id, keyed by id.
//
// An id that StashDB does not know (deleted or merged performer) is returned
// with an empty string rather than omitted: to the caller "StashDB has no
// gender for this performer" and "this performer no longer exists" are the
// same answer — stop asking — and the distinction has no effect on filtering.
// Callers must NOT read an empty gender as MALE or as FEMALE.
func (c *Client) PerformerGenders(ctx context.Context, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	// De-duplicate: the same performer appears on many scenes, and paying
	// for a repeat lookup is the whole cost of this pass.
	seen := make(map[string]bool, len(ids))
	uniq := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		uniq = append(uniq, id)
	}
	for start := 0; start < len(uniq); start += genderBatch {
		end := start + genderBatch
		if end > len(uniq) {
			end = len(uniq)
		}
		chunk := uniq[start:end]

		var b strings.Builder
		b.WriteString("query ForagerPerformerGenders(")
		for i := range chunk {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "$p%d: ID!", i)
		}
		b.WriteString(") {\n")
		for i := range chunk {
			// Aliased so every result is addressable, and the id is a
			// VARIABLE rather than interpolated: these come from the
			// database, and a query built by string concatenation is a
			// query someone else can shape.
			fmt.Fprintf(&b, "  p%d: findPerformer(id: $p%d) { id gender }\n", i, i)
		}
		b.WriteString("}")

		vars := make(map[string]any, len(chunk))
		for i, id := range chunk {
			vars[fmt.Sprintf("p%d", i)] = id
		}

		var resp map[string]json.RawMessage
		if err := c.do(ctx, b.String(), vars, &resp); err != nil {
			return out, err
		}
		for i, id := range chunk {
			raw, ok := resp[fmt.Sprintf("p%d", i)]
			if !ok || string(raw) == "null" {
				out[id] = "" // unknown to StashDB: a settled answer
				continue
			}
			var p struct {
				ID     string `json:"id"`
				Gender string `json:"gender"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				out[id] = ""
				continue
			}
			out[id] = strings.ToUpper(strings.TrimSpace(p.Gender))
		}
	}
	return out, nil
}
