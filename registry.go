package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// RegistryClient is the gateway's HTTP client to the internal marketplace
// registry. The registry is not publicly reachable; the gateway exposes the
// catalog (public read) and an admin-guarded publish endpoint on its behalf, so
// the browser and the publish CLI only ever talk to the gateway origin.
type RegistryClient struct {
	base string
	hc   *http.Client
}

func NewRegistryClient(base string) *RegistryClient {
	return &RegistryClient{base: base, hc: &http.Client{Timeout: 30 * time.Second}}
}

func (r *RegistryClient) configured() bool { return r.base != "" }

// Catalog returns the game ids published to the registry. Best-effort: callers
// treat any error as "no registry games" and fall back to other sources.
func (r *RegistryClient) Catalog(ctx context.Context) ([]string, error) {
	if !r.configured() {
		return nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+"/games", nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry catalog: %d", resp.StatusCode)
	}
	var wrap struct {
		Games []struct {
			GameID string `json:"gameId"`
		} `json:"games"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrap); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(wrap.Games))
	for _, g := range wrap.Games {
		if g.GameID != "" {
			ids = append(ids, g.GameID)
		}
	}
	return ids, nil
}

// Publish forwards a raw publish package (the JSON body from tools/publish.mjs)
// to the registry and returns the registry's status code and response body
// verbatim, so the CLI sees the registry's own validation errors.
func (r *RegistryClient) Publish(ctx context.Context, body []byte) (int, []byte, error) {
	if !r.configured() {
		return 0, nil, fmt.Errorf("registry not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.base+"/publish", bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}
