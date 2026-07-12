package torrentmeta

import (
	"encoding/base32"
	"encoding/hex"
	"net/url"
	"strings"
)

// MagnetInfoHash extracts the v1 info_hash from a magnet URI, normalised
// to the lowercase 40-char hex that qBit keys torrents by. Handles both
// hex (btih:<40 hex>) and base32 (btih:<32 base32>) encodings. Returns ""
// for non-magnets, v2-only magnets (btmh), or anything malformed — callers
// then fall back to poller-side linking.
func MagnetInfoHash(downloadURL string) string {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(downloadURL)), "magnet:") {
		return ""
	}
	u, err := url.Parse(downloadURL)
	if err != nil {
		return ""
	}
	const prefix = "urn:btih:"
	for _, xt := range u.Query()["xt"] {
		if !strings.HasPrefix(xt, prefix) {
			continue
		}
		h := strings.TrimSpace(xt[len(prefix):])
		switch len(h) {
		case 40:
			if _, err := hex.DecodeString(h); err == nil {
				return strings.ToLower(h)
			}
		case 32:
			b, err := base32.StdEncoding.DecodeString(strings.ToUpper(h))
			if err == nil && len(b) == 20 {
				return hex.EncodeToString(b)
			}
		}
	}
	return ""
}
