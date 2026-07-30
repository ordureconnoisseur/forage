package stashdb

import "context"

// fingerprintSceneBatch caps how many scenes one lookup asks about. The
// request body carries every hash of every scene, and a popular release can
// hold ninety, so this is bounded by payload rather than by round trips.
const fingerprintSceneBatch = 40

// FindScenesByFingerprints answers, for each scene's set of file hashes,
// which scene on THIS box holds the same file. The returned slice is
// position-aligned with the input and holds "" where nothing matched.
//
// This is the one join that works between stash-boxes. Scene ids do not
// travel: a FansDB uuid means nothing to StashDB, and asking anyway returns
// an empty result that reads exactly like "StashDB has not indexed it". A
// fingerprint is computed from the file itself, so when two boxes have both
// seen a release they agree on its hash, and the answer is a fact rather than
// a guess at matching titles.
//
// Callers pass the hashes another box reported and run this against StashDB.
func (c *Client) FindScenesByFingerprints(ctx context.Context, scenes [][]Fingerprint) ([]string, error) {
	out := make([]string, len(scenes))
	// Scenes with no hashes are dropped from the request and left "": the
	// GraphQL input type rejects an empty inner list, and sending one to
	// preserve alignment would fail the whole batch over a scene that could
	// never have matched.
	idx := make([]int, 0, len(scenes))
	for i, fps := range scenes {
		if len(fps) > 0 {
			idx = append(idx, i)
		}
	}

	const q = `
query ForagerScenesByFingerprints($fingerprints: [[FingerprintQueryInput!]!]!) {
  findScenesBySceneFingerprints(fingerprints: $fingerprints) { id }
}`
	for start := 0; start < len(idx); start += fingerprintSceneBatch {
		end := start + fingerprintSceneBatch
		if end > len(idx) {
			end = len(idx)
		}
		chunk := idx[start:end]

		input := make([][]Fingerprint, 0, len(chunk))
		for _, i := range chunk {
			input = append(input, scenes[i])
		}
		var resp struct {
			Found [][]struct {
				ID string `json:"id"`
			} `json:"findScenesBySceneFingerprints"`
		}
		if err := c.do(ctx, q, map[string]any{"fingerprints": input}, &resp); err != nil {
			// Partial results are still useful, and the caller treats a
			// miss as "not known to be owned" rather than as an error.
			return out, err
		}
		// Trust position, but do not assume it: a short reply would
		// otherwise shift every id onto the wrong scene, which here means
		// hiding a scene the user does not own.
		for n, matches := range resp.Found {
			if n >= len(chunk) {
				break
			}
			if len(matches) > 0 {
				out[chunk[n]] = matches[0].ID
			}
		}
	}
	return out, nil
}
