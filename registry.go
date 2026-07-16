package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

// Asset fetches one of a game's uploaded images (Option 1c) from the internal
// registry, so the browser can load it from the gateway origin (img-src) without
// the registry being public. Returns the bytes, content type, and HTTP status.
func (r *RegistryClient) Asset(ctx context.Context, gameID, assetID string) ([]byte, string, int, error) {
	if !r.configured() {
		return nil, "", http.StatusNotFound, nil
	}
	u := r.base + "/games/" + url.PathEscape(gameID) + "/assets/" + url.PathEscape(assetID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", 0, err
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, "", 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	return body, resp.Header.Get("Content-Type"), resp.StatusCode, nil
}

// UI fetches a game's self-contained sandboxed UI bundle (Option 2) from the
// registry. The gateway serves it to the browser under a strict CSP.
func (r *RegistryClient) UI(ctx context.Context, gameID string) ([]byte, int, error) {
	if !r.configured() {
		return nil, http.StatusNotFound, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+"/games/"+url.PathEscape(gameID)+"/ui", nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return body, resp.StatusCode, nil
}

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

// RegGame is one game as the registry describes it (manifest metadata + rating
// aggregates). The gateway enriches this with play/live counts for the catalog.
type RegGame struct {
	GameID      string  `json:"gameId"`
	DisplayName string  `json:"displayName"`
	Board       string  `json:"board"`
	MinPlayers  int     `json:"minPlayers"`
	MaxPlayers  int     `json:"maxPlayers"`
	Rating      float64 `json:"rating"`
	RatingCount int     `json:"ratingCount"`
	// Enabled is a pointer so "field absent" (an older registry that predates the
	// admin flag) reads as enabled, not disabled — otherwise a gateway deployed
	// ahead of the registry would hide every game. nil ⇒ enabled.
	Enabled *bool `json:"enabled"`
	// HasUI is true when the game ships a custom sandboxed UI bundle (Option 2), so
	// the frontend auto-picks the sandbox renderer. Absent (old registry) ⇒ false.
	HasUI bool `json:"hasUI"`
}

// EnabledOrDefault treats a missing enabled flag (old registry) as enabled.
func (g RegGame) EnabledOrDefault() bool { return g.Enabled == nil || *g.Enabled }

// CatalogFull returns the registry's published games with metadata + ratings.
func (r *RegistryClient) CatalogFull(ctx context.Context) ([]RegGame, error) {
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
		Games []RegGame `json:"games"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrap); err != nil {
		return nil, err
	}
	return wrap.Games, nil
}

// SetGameEnabled enables/disables a whole game in the registry (admin action).
// Returns the registry's status + body verbatim.
func (r *RegistryClient) SetGameEnabled(ctx context.Context, gameID string, enabled bool) (int, []byte, error) {
	if !r.configured() {
		return 0, nil, fmt.Errorf("registry not configured")
	}
	payload, _ := json.Marshal(map[string]any{"enabled": enabled})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.base+"/games/"+url.PathEscape(gameID)+"/enabled", bytes.NewReader(payload))
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

// Rate submits a user's star rating for a game to the registry and returns its
// status + body verbatim.
func (r *RegistryClient) Rate(ctx context.Context, gameID, userID string, stars int) (int, []byte, error) {
	if !r.configured() {
		return 0, nil, fmt.Errorf("registry not configured")
	}
	payload, _ := json.Marshal(map[string]any{"userId": userID, "stars": stars})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.base+"/games/"+gameID+"/rate", bytes.NewReader(payload))
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

// MyRating returns the user's own stars for a game (0 if unrated / unavailable).
func (r *RegistryClient) MyRating(ctx context.Context, gameID, userID string) (int, error) {
	if !r.configured() {
		return 0, nil
	}
	u := r.base + "/games/" + url.PathEscape(gameID) + "/rating?userId=" + url.QueryEscape(userID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("registry rating: %d", resp.StatusCode)
	}
	var wrap struct {
		Stars int `json:"stars"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrap); err != nil {
		return 0, err
	}
	return wrap.Stars, nil
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
