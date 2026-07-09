package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"
)

// A lobby is a pending match waiting for players. Because login is required to
// play, every seat is filled by an authenticated user id — so when the lobby
// fills, the gateway asks the game-host to create the real match with those user
// ids as the players, and both clients enter the live match over WebSocket.
//
// Lobbies are intentionally in-memory: they are ephemeral pre-match state. The
// authoritative, durable record (the match + move log) lives in the game-host.

var (
	ErrLobbyNotFound = errors.New("lobby not found")
	ErrLobbyFull     = errors.New("lobby is full")
	ErrNotHost       = errors.New("only the host may do that")
)

type LobbyPlayer struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Lobby struct {
	ID        string        `json:"id"`
	GameID    string        `json:"gameId"`
	Host      string        `json:"host"`
	Seats     int           `json:"seats"`
	Players   []LobbyPlayer `json:"players"`
	MatchID   string        `json:"matchId,omitempty"`
	Status    string        `json:"status"` // "open" | "started"
	CreatedAt time.Time     `json:"createdAt"`
}

func (l *Lobby) hasPlayer(id string) bool {
	for _, p := range l.Players {
		if p.ID == id {
			return true
		}
	}
	return false
}

func (l *Lobby) playerIDs() []string {
	ids := make([]string, len(l.Players))
	for i, p := range l.Players {
		ids[i] = p.ID
	}
	return ids
}

type LobbyManager struct {
	mu      sync.Mutex
	lobbies map[string]*Lobby
	gh      *GameHostClient
}

func NewLobbyManager(gh *GameHostClient) *LobbyManager {
	return &LobbyManager{lobbies: map[string]*Lobby{}, gh: gh}
}

// Create opens a new lobby seated with its host.
func (m *LobbyManager) Create(host LobbyPlayer, gameID string, seats int) *Lobby {
	if seats < 2 {
		seats = 2
	}
	if seats > 8 {
		seats = 8
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	l := &Lobby{
		ID:        hex.EncodeToString(randID(6)),
		GameID:    gameID,
		Host:      host.ID,
		Seats:     seats,
		Players:   []LobbyPlayer{host},
		Status:    "open",
		CreatedAt: time.Now(),
	}
	m.lobbies[l.ID] = l
	return cloneLobby(l)
}

// ListOpen returns lobbies still waiting for players, newest first.
func (m *LobbyManager) ListOpen() []Lobby {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Lobby{}
	for _, l := range m.lobbies {
		if l.Status == "open" {
			out = append(out, *cloneLobby(l))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (m *LobbyManager) Get(id string) (*Lobby, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.lobbies[id]
	if !ok {
		return nil, ErrLobbyNotFound
	}
	return cloneLobby(l), nil
}

// Join adds a player and, once the lobby is full, starts the real match on the
// game-host. Joining an already-started lobby (or re-joining) just returns it.
func (m *LobbyManager) Join(ctx context.Context, id string, player LobbyPlayer) (*Lobby, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.lobbies[id]
	if !ok {
		return nil, ErrLobbyNotFound
	}
	if l.Status == "started" || l.hasPlayer(player.ID) {
		if !l.hasPlayer(player.ID) && l.Status == "started" {
			return nil, ErrLobbyFull
		}
		return cloneLobby(l), nil
	}
	if len(l.Players) >= l.Seats {
		return nil, ErrLobbyFull
	}
	l.Players = append(l.Players, player)
	if len(l.Players) >= l.Seats {
		meta, err := m.gh.CreateMatch(ctx, l.GameID, l.playerIDs())
		if err != nil {
			// Roll back the seat so the lobby stays joinable.
			l.Players = l.Players[:len(l.Players)-1]
			return nil, err
		}
		l.MatchID = meta.ID
		l.Status = "started"
	}
	return cloneLobby(l), nil
}

// Cancel removes an open lobby; only the host may cancel.
func (m *LobbyManager) Cancel(id, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.lobbies[id]
	if !ok {
		return ErrLobbyNotFound
	}
	if l.Host != userID {
		return ErrNotHost
	}
	delete(m.lobbies, id)
	return nil
}

func cloneLobby(l *Lobby) *Lobby {
	cp := *l
	cp.Players = append([]LobbyPlayer(nil), l.Players...)
	return &cp
}

func randID(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}
