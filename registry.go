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
}

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
