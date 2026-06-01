package api

import (
	"net/http"
	"strings"
)

// download-client setup check — a READ-ONLY diagnostic for the wizard. It
// inspects each configured download client and reports whether forage's
// category exists, where it saves, and (the load-bearing part) whether a
// file in that save dir can actually be HARDLINKED into the library. It
// never mutates the client; it tells the user exactly what to fix.

type clientCheck struct {
	Client   string `json:"client"`   // "qbit" | "sab"
	Category string `json:"category"` // the category forage is configured to use
	// CategoryExists: the configured category is present in the client.
	CategoryExists bool   `json:"category_exists"`
	SavePath       string `json:"save_path,omitempty"` // the category's save dir, as the client reports it
	// Hardlink probe result (run against the category's save path):
	HardlinkChecked bool   `json:"hardlink_checked"`
	HardlinkOK      bool   `json:"hardlink_ok"` // true = same filesystem, will hardlink
	Detail          string `json:"detail"`      // human summary / what to fix
	Status          string `json:"status"`      // "ok" | "warn" | "error"
}

type downloadSetupResponse struct {
	LibraryRoot string        `json:"library_root"`
	Checks      []clientCheck `json:"checks"`
}

// getDownloadSetup powers the wizard's "check my download clients" panel.
func (s *Server) getDownloadSetup(w http.ResponseWriter, r *http.Request) {
	cfg := s.composedConfig()
	resp := downloadSetupResponse{LibraryRoot: s.pool.Placer().LibraryRoot()}

	if qb := s.pool.Qbit(); qb != nil {
		c := clientCheck{Client: "qbit", Category: cfg.QbitCategory}
		cats, err := qb.Categories(r.Context())
		if err != nil {
			c.Status = "error"
			c.Detail = "Couldn't read qBittorrent categories: " + err.Error()
		} else if cat, ok := cats[cfg.QbitCategory]; ok {
			c.CategoryExists = true
			c.SavePath = cat.SavePath
			s.fillHardlink(&c, cat.SavePath)
		} else {
			c.Status = "warn"
			c.Detail = "Create a category named \"" + cfg.QbitCategory +
				"\" in qBittorrent, with its save path on the same filesystem as your library."
		}
		resp.Checks = append(resp.Checks, c)
	}

	if sb := s.pool.Sab(); sb != nil {
		c := clientCheck{Client: "sab", Category: cfg.SabCategory}
		cats, err := sb.Categories(r.Context())
		if err != nil {
			c.Status = "error"
			c.Detail = "Couldn't read SABnzbd categories: " + err.Error()
		} else {
			var dir string
			for _, cat := range cats {
				if strings.EqualFold(cat.Name, cfg.SabCategory) {
					c.CategoryExists = true
					dir = cat.Dir
					break
				}
			}
			if c.CategoryExists {
				c.SavePath = dir
				// SAB's category dir can be relative to its global complete
				// dir; only probe when it looks like an absolute path forage
				// can resolve. Otherwise just confirm the category exists.
				if strings.HasPrefix(dir, "/") {
					s.fillHardlink(&c, dir)
				} else {
					c.Status = "ok"
					c.Detail = "Category exists. Its folder \"" + dir +
						"\" is relative to SAB's complete dir — make sure that resolves onto the same filesystem as your library."
				}
			} else {
				c.Status = "warn"
				c.Detail = "Create a category named \"" + cfg.SabCategory +
					"\" in SABnzbd, with its folder on the same filesystem as your library."
			}
		}
		resp.Checks = append(resp.Checks, c)
	}

	writeJSON(w, http.StatusOK, resp)
}

// fillHardlink runs the hardlink probe for a category's save path and sets
// the check's hardlink fields + status/detail accordingly.
func (s *Server) fillHardlink(c *clientCheck, savePath string) {
	pl := s.pool.Placer()
	if !pl.Configured() {
		c.Status = "warn"
		c.Detail = "Category exists, but no library root is set yet — can't verify hardlinking."
		return
	}
	res := pl.HardlinkProbe(savePath)
	c.HardlinkChecked = true
	c.Detail = res.Reason
	switch {
	case res.OK && res.Hardlink:
		c.HardlinkOK = true
		c.Status = "ok"
	case res.OK && !res.Hardlink:
		// Copy fallback works but wastes space — a warning, not an error.
		c.Status = "warn"
	default:
		c.Status = "error"
	}
}
