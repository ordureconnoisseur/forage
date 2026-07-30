package stashdb

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// linkBatch mirrors genderBatch: stash-box has no "find these ids" field, so
// a batch is aliased findPerformer selections in one document.
const linkBatch = 50

// PerformerStashDBLinks returns, for each performer id on THIS box, the
// StashDB performer id their profile links to, or "" if they link to none.
//
// Secondary boxes carry no cross-id field, but their editors routinely list a
// performer's StashDB page among their urls: on a 40-scene sample that was 78%
// of JavStash performers and 47% of FansDB's. That link is the only way to
// recognise someone the user already owns without their having identified the
// performer against the secondary box first, which almost nobody does (147
// FansDB and 2 JavStash performers on the reference library).
//
// An id the box does not know is recorded as "" rather than omitted, so a
// caller memoising these does not re-ask forever.
func (c *Client) PerformerStashDBLinks(ctx context.Context, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	seen := make(map[string]bool, len(ids))
	uniq := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		uniq = append(uniq, id)
	}

	for start := 0; start < len(uniq); start += linkBatch {
		end := start + linkBatch
		if end > len(uniq) {
			end = len(uniq)
		}
		chunk := uniq[start:end]

		var b strings.Builder
		b.WriteString("query ForagerPerformerLinks(")
		for i := range chunk {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "$p%d: ID!", i)
		}
		b.WriteString(") {\n")
		for i := range chunk {
			// Ids come from the box's own response rather than from user
			// input, but they are still bound as variables: a query built
			// by concatenation is a query someone else can shape.
			fmt.Fprintf(&b, "  p%d: findPerformer(id: $p%d) { id urls { url } }\n", i, i)
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
			out[id] = "" // settled answer unless a link turns up below
			raw, ok := resp[fmt.Sprintf("p%d", i)]
			if !ok || string(raw) == "null" {
				continue
			}
			var p struct {
				URLs []struct {
					URL string `json:"url"`
				} `json:"urls"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				continue
			}
			for _, u := range p.URLs {
				if sdb := StashDBPerformerID(u.URL); sdb != "" {
					out[id] = sdb
					break
				}
			}
		}
	}
	return out, nil
}

// StashDBPerformerID pulls the performer uuid out of a stashdb.org profile
// URL, or returns "" for anything else.
//
// Deliberately strict about the host. A link to a performer's page on some
// other site that happens to end in a uuid would otherwise be read as a
// StashDB id and mark the wrong person as owned.
func StashDBPerformerID(raw string) string {
	u := strings.TrimSpace(raw)
	for _, p := range []string{"https://", "http://"} {
		u = strings.TrimPrefix(u, p)
	}
	u = strings.TrimPrefix(u, "www.")
	const host = "stashdb.org/performers/"
	if !strings.HasPrefix(strings.ToLower(u), host) {
		return ""
	}
	id := u[len(host):]
	// Trim anything hanging off the id: a trailing slash, a sub-path like
	// /scenes, a query string or a fragment.
	if i := strings.IndexAny(id, "/?#"); i >= 0 {
		id = id[:i]
	}
	if !looksLikeUUID(id) {
		return ""
	}
	return strings.ToLower(id)
}

// looksLikeUUID checks the 8-4-4-4-12 hex shape. Cheap, and enough: the value
// is only ever used as a map key against ids the library already holds, so a
// malformed one matches nothing rather than matching the wrong thing.
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}
