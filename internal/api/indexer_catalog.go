package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ordureconnoisseur/forager/internal/clienterr"
)

// Indexer catalog: Prowlarr's own definition list, filtered to
// adult-capable entries so indexers can be added from forage's wizard
// without opening Prowlarr. Filtering is by CAPABILITY (the definition
// declares XXX categories), never by opinion — the list is Prowlarr's,
// alphabetical, no recommendations.

// catalogEntry is the slim projection the UI renders.
type catalogEntry struct {
	// ID is the definition's stable identity (definitionName, falling
	// back to name) — what the add call sends back.
	ID          string `json:"id"`
	Name        string `json:"name"`
	Protocol    string `json:"protocol"`
	Privacy     string `json:"privacy"`
	Description string `json:"description,omitempty"`
	// Fields are the simple credential inputs a user must fill (private/
	// semi-private trackers): non-advanced textbox/password fields with no
	// preset value. Public indexers typically have none — one-click add.
	Fields []catalogField `json:"fields"`
	// Complex marks definitions needing configuration forage doesn't
	// render (exotic field types) — the UI shows an "open Prowlarr" link.
	Complex bool `json:"complex"`
	// Added: an indexer with this definition is already configured.
	Added bool `json:"added"`
}

type catalogField struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Password bool   `json:"password"`
}

// schemaCache: the definition catalog is ~500 large objects and changes
// only when Prowlarr updates; cache briefly.

func (s *Server) prowlarrSchemas(ctx context.Context) ([]map[string]any, error) {
	pc := s.pool.Prowlarr()
	if pc == nil {
		return nil, errors.New("prowlarr is not configured")
	}
	s.schemaCacheMu.Lock()
	defer s.schemaCacheMu.Unlock()
	if time.Since(s.schemaFetched) < 10*time.Minute && s.schemas != nil {
		return s.schemas, nil
	}
	schemas, err := pc.IndexerSchemas(ctx)
	if err != nil {
		return nil, err
	}
	s.schemas = schemas
	s.schemaFetched = time.Now()
	return schemas, nil
}

// xxxCapable reports whether a definition declares any category in the
// adult range (6000–6999), top-level or nested.
func xxxCapable(def map[string]any) bool {
	caps, _ := def["capabilities"].(map[string]any)
	cats, _ := caps["categories"].([]any)
	var walk func([]any) bool
	walk = func(list []any) bool {
		for _, c := range list {
			m, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if id, ok := m["id"].(float64); ok && id >= 6000 && id < 7000 {
				return true
			}
			if sub, ok := m["subCategories"].([]any); ok && walk(sub) {
				return true
			}
		}
		return false
	}
	return walk(cats)
}

func defID(def map[string]any) string {
	if v, _ := def["definitionName"].(string); v != "" {
		return v
	}
	v, _ := def["name"].(string)
	return v
}

// projectEntry classifies a definition's inputs: renderable credential
// fields vs anything forage shouldn't pretend to understand.
func projectEntry(def map[string]any, added map[string]bool) catalogEntry {
	name, _ := def["name"].(string)
	e := catalogEntry{
		ID:     defID(def),
		Name:   name,
		Fields: []catalogField{},
		Added:  added[strings.ToLower(name)],
	}
	e.Protocol, _ = def["protocol"].(string)
	e.Privacy, _ = def["privacy"].(string)
	e.Description, _ = def["description"].(string)
	fields, _ := def["fields"].([]any)
	for _, f := range fields {
		m, ok := f.(map[string]any)
		if !ok {
			continue
		}
		if adv, _ := m["advanced"].(bool); adv {
			continue
		}
		ftype, _ := m["type"].(string)
		fname, _ := m["name"].(string)
		label, _ := m["label"].(string)
		switch ftype {
		case "textbox", "password":
			// Only ask for what has no usable preset (credentials); a
			// pre-filled field (baseUrl etc.) rides its default.
			if m["value"] == nil || m["value"] == "" {
				e.Fields = append(e.Fields, catalogField{
					Name:     fname,
					Label:    label,
					Password: ftype == "password" || strings.Contains(strings.ToLower(fname), "pass"),
				})
			}
		case "checkbox", "select", "info", "tag", "number":
			// Fine with defaults; nothing to ask.
		default:
			e.Complex = true
		}
	}
	return e
}

// getIndexerCatalog: GET /indexer-catalog — adult-capable definitions,
// alphabetical, with configured ones flagged.
func (s *Server) getIndexerCatalog(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	schemas, err := s.prowlarrSchemas(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "could not load Prowlarr's indexer catalog: "+err.Error())
		return
	}
	added := map[string]bool{}
	if pc := s.pool.Prowlarr(); pc != nil {
		if configured, err := pc.Indexers(ctx); err == nil {
			for _, ix := range configured {
				added[strings.ToLower(ix.Name)] = true
			}
		}
	}
	out := []catalogEntry{}
	for _, def := range schemas {
		if !xxxCapable(def) {
			continue
		}
		out = append(out, projectEntry(def, added))
	}
	writeJSON(w, http.StatusOK, map[string]any{"catalog": out})
}

// postIndexerCatalogAdd: POST /indexer-catalog/add {id, fields:{name:value}}.
// Finds the definition fresh, applies the credential values, TESTS it
// against the real site, then creates it — so a green result means the
// indexer actually answered, not just that Prowlarr saved a row.
func (s *Server) postIndexerCatalogAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID     string            `json:"id"`
		Fields map[string]string `json:"fields"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		writeErr(w, http.StatusBadRequest, "bad request body")
		return
	}
	pc := s.pool.Prowlarr()
	if pc == nil {
		writeErr(w, http.StatusServiceUnavailable, "prowlarr is not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	schemas, err := s.prowlarrSchemas(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	var def map[string]any
	for _, d := range schemas {
		if defID(d) == body.ID {
			def = d
			break
		}
	}
	if def == nil {
		writeErr(w, http.StatusNotFound, "unknown indexer definition")
		return
	}
	// Work on a deep copy via JSON round-trip — the cached schema must
	// stay pristine for the next caller.
	raw, _ := json.Marshal(def)
	var payload map[string]any
	_ = json.Unmarshal(raw, &payload)
	payload["enable"] = true
	// Schemas ship appProfileId 0, which Prowlarr's validator rejects;
	// 1 is the built-in Standard profile every install has.
	if v, ok := payload["appProfileId"].(float64); !ok || v <= 0 {
		payload["appProfileId"] = 1
	}
	if fields, ok := payload["fields"].([]any); ok {
		for _, f := range fields {
			m, ok := f.(map[string]any)
			if !ok {
				continue
			}
			fname, _ := m["name"].(string)
			if v, ok := body.Fields[fname]; ok && v != "" {
				m["value"] = v
			}
		}
	}
	if err := pc.TestIndexer(ctx, payload); err != nil {
		status := http.StatusUnprocessableEntity
		if !errors.Is(err, clienterr.ErrRejected) {
			status = http.StatusBadGateway
		}
		writeErr(w, status, "indexer test failed: "+err.Error())
		return
	}
	if err := pc.CreateIndexer(ctx, payload); err != nil {
		writeErr(w, http.StatusBadGateway, "indexer create failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
