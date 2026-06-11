// Package gqlclient is the one GraphQL transport the Stash and StashDB
// clients share: an ApiKey-authenticated POST to <base>/graphql with the
// standard data/errors envelope decoded. The two clients carried
// byte-for-byte copies of this (drifted only in the error prefix), so a
// transport fix had to be made twice; label keeps the error messages
// distinguishable. The REST clients (prowlarr/qbit/sab) are genuinely
// different and stay as they are.
package gqlclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	apiKey  string
	label   string // error-message prefix, e.g. "stash" / "stashdb"
	http    *http.Client
}

func New(baseURL, apiKey, label string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		label:   label,
		http:    &http.Client{Timeout: 60 * time.Second},
	}
}

type gqlError struct {
	Message string `json:"message"`
}

// Do posts the query and decodes the response's data field into out
// (skipped when out is nil). GraphQL-level errors and non-200 statuses
// surface as errors prefixed with the client's label.
func (c *Client) Do(ctx context.Context, query string, vars map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/graphql", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("ApiKey", c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s graphql %d: %s", c.label, resp.StatusCode, raw)
	}
	var wrap struct {
		Data   json.RawMessage `json:"data"`
		Errors []gqlError      `json:"errors,omitempty"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return fmt.Errorf("decode: %w (body=%s)", err, raw)
	}
	if len(wrap.Errors) > 0 {
		return fmt.Errorf("%s graphql errors: %+v", c.label, wrap.Errors)
	}
	if out != nil {
		if err := json.Unmarshal(wrap.Data, out); err != nil {
			return fmt.Errorf("decode data: %w", err)
		}
	}
	return nil
}
