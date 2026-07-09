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

// GameHostClient is the gateway's HTTP client to the authoritative game-host.
// The gateway holds NO game state itself — it forwards intents and fans out the
// redacted views the game-host returns.
type GameHostClient struct {
	base string
	hc   *http.Client
}

func NewGameHostClient(base string) *GameHostClient {
	return &GameHostClient{base: base, hc: &http.Client{Timeout: 10 * time.Second}}
}

type MatchMeta struct {
	ID            string          `json:"id"`
	GameID        string          `json:"gameId"`
	Players       []string        `json:"players"`
	CurrentPlayer string          `json:"currentPlayer"`
	Turn          int             `json:"turn"`
	MoveCount     int             `json:"moveCount"`
	Ended         bool            `json:"ended"`
	Result        json.RawMessage `json:"result"`
}

type ApplyResp struct {
	OK            bool            `json:"ok"`
	Error         string          `json:"error"`
	Events        json.RawMessage `json:"events"`
	MoveCount     int             `json:"moveCount"`
	CurrentPlayer string          `json:"currentPlayer"`
	Ended         bool            `json:"ended"`
	Result        json.RawMessage `json:"result"`
}

func (g *GameHostClient) GetMatch(ctx context.Context, id string) (*MatchMeta, error) {
	body, _, err := g.do(ctx, http.MethodGet, "/matches/"+id, nil)
	if err != nil {
		return nil, err
	}
	var m MatchMeta
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("decode match: %w", err)
	}
	return &m, nil
}

// CreateMatch asks the game-host to set up a new match with the given players
// (user ids). Used by the lobby once a pending match fills up.
func (g *GameHostClient) CreateMatch(ctx context.Context, gameID string, players []string) (*MatchMeta, error) {
	reqBody, _ := json.Marshal(map[string]any{"gameId": gameID, "players": players})
	body, status, err := g.do(ctx, http.MethodPost, "/matches", reqBody)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("game-host create match: %d %s", status, truncate(body))
	}
	var m MatchMeta
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("decode match: %w", err)
	}
	return &m, nil
}

// RatingEntry is one row of a game's leaderboard as returned by the game-host
// (keyed by player = user id; the gateway resolves display names).
type RatingEntry struct {
	Player string  `json:"player"`
	Rating float64 `json:"rating"`
	Wins   int     `json:"wins"`
	Losses int     `json:"losses"`
	Draws  int     `json:"draws"`
	Games  int     `json:"games"`
}

func (g *GameHostClient) GetLeaderboard(ctx context.Context, gameID string) ([]RatingEntry, error) {
	path := "/leaderboard"
	if gameID != "" {
		path += "?gameId=" + gameID
	}
	body, status, err := g.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("game-host leaderboard: %d %s", status, truncate(body))
	}
	var wrap struct {
		Entries []RatingEntry `json:"entries"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return nil, err
	}
	return wrap.Entries, nil
}

// ApplyMove forwards a move and returns the parsed result plus the HTTP status
// (422 means the guest rejected the move — a normal client error).
func (g *GameHostClient) ApplyMove(ctx context.Context, id, playerID, mtype string, payload json.RawMessage) (*ApplyResp, int, error) {
	reqBody, _ := json.Marshal(map[string]any{"playerId": playerID, "type": mtype, "payload": payload})
	body, status, err := g.do(ctx, http.MethodPost, "/matches/"+id+"/moves", reqBody)
	if err != nil {
		return nil, status, err
	}
	var res ApplyResp
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, status, fmt.Errorf("decode apply: %w", err)
	}
	return &res, status, nil
}

func (g *GameHostClient) GetView(ctx context.Context, id, playerID string) (json.RawMessage, error) {
	body, _, err := g.do(ctx, http.MethodGet, "/matches/"+id+"/view?playerId="+playerID, nil)
	return body, err
}

// GetLegalMoves returns just the moves array for the current player.
func (g *GameHostClient) GetLegalMoves(ctx context.Context, id string) (json.RawMessage, error) {
	body, _, err := g.do(ctx, http.MethodGet, "/matches/"+id+"/legal", nil)
	if err != nil {
		return nil, err
	}
	var wrap struct {
		Moves json.RawMessage `json:"moves"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return nil, err
	}
	return wrap.Moves, nil
}

func (g *GameHostClient) do(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, reader)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode >= 500 {
		return data, resp.StatusCode, fmt.Errorf("game-host %s %s: %d %s", method, path, resp.StatusCode, truncate(data))
	}
	return data, resp.StatusCode, nil
}

func truncate(b []byte) string {
	const max = 200
	if len(b) > max {
		return string(b[:max]) + "…"
	}
	return string(b)
}
