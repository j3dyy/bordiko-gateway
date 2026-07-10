package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"
)

// A lobby is a pending match: a table with a fixed set of seats that players
// take before the game begins. Because login is required to play, every seat is
// filled by an authenticated user id — so when the host starts the table, the
// gateway asks the game-host to create the real match with those user ids as the
// players (in seat order) plus the table config (mode + teams), and every client
// enters the live match over WebSocket.
//
// Seats, not a fill-order list: a player chooses a specific seat, which — in
// "teams" mode — is how they choose their partnership (partners sit across from
// each other, seat-index parity). The host presses Start once every seat is
// filled; the table stays open until then so players can rearrange.
//
// Lobbies are intentionally in-memory: they are ephemeral pre-match state. The
// authoritative, durable record (the match + move log) lives in the game-host.

var (
	ErrLobbyNotFound = errors.New("lobby not found")
	ErrLobbyFull     = errors.New("lobby is full")
	ErrNotHost       = errors.New("only the host may do that")
	ErrLobbyStarted  = errors.New("the match has already started")
	ErrBadSeat       = errors.New("no such seat")
	ErrSeatTaken     = errors.New("that seat is taken")
	ErrNotSeated     = errors.New("you are not seated at this table")
	ErrNotReady      = errors.New("every seat must be filled to start")
)

type LobbyPlayer struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Seat is one place at the table. Team encodes the partnership: in "teams" mode
// partners sit across from each other, so team = index % 2 (seats 0,2 are one
// team; 1,3 the other); in "solo" mode every seat is its own team.
type Seat struct {
	Index  int          `json:"index"`
	Team   int          `json:"team"`
	Player *LobbyPlayer `json:"player,omitempty"`
}

type Lobby struct {
	ID        string    `json:"id"`
	GameID    string    `json:"gameId"`
	Host      string    `json:"host"`
	Mode      string    `json:"mode"` // "solo" | "teams"
	Seats     []Seat    `json:"seats"`
	MatchID   string    `json:"matchId,omitempty"`
	Status    string    `json:"status"` // "open" | "started"
	CreatedAt time.Time `json:"createdAt"`
}

// teamOf returns the team a seat belongs to. In "teams" mode partners sit
// opposite each other (parity of the seat index); otherwise each seat stands
// alone.
func teamOf(index int, mode string) int {
	if mode == "teams" {
		return index % 2
	}
	return index
}

func (l *Lobby) seatOf(id string) int {
	for i := range l.Seats {
		if l.Seats[i].Player != nil && l.Seats[i].Player.ID == id {
			return i
		}
	}
	return -1
}

func (l *Lobby) hasPlayer(id string) bool { return l.seatOf(id) >= 0 }

func (l *Lobby) filled() bool {
	for i := range l.Seats {
		if l.Seats[i].Player == nil {
			return false
		}
	}
	return true
}

// orderedPlayerIDs returns the seated players in seat order — this becomes the
// match's play order (and, in teams mode, keeps partners at alternating seats).
func (l *Lobby) orderedPlayerIDs() []string {
	ids := make([]string, 0, len(l.Seats))
	for i := range l.Seats {
		if l.Seats[i].Player != nil {
			ids = append(ids, l.Seats[i].Player.ID)
		}
	}
	return ids
}

// matchConfig is the table configuration handed to the game at setup: the mode,
// seat count, and — grouped by team — the partnerships (player ids). Games that
// don't model teams simply ignore it.
func (l *Lobby) matchConfig() json.RawMessage {
	byTeam := map[int][]string{}
	teamIDs := []int{}
	for i := range l.Seats {
		s := l.Seats[i]
		if s.Player == nil {
			continue
		}
		if _, ok := byTeam[s.Team]; !ok {
			teamIDs = append(teamIDs, s.Team)
		}
		byTeam[s.Team] = append(byTeam[s.Team], s.Player.ID)
	}
	sort.Ints(teamIDs)
	teams := make([][]string, 0, len(teamIDs))
	for _, t := range teamIDs {
		teams = append(teams, byTeam[t])
	}
	b, _ := json.Marshal(map[string]any{
		"mode":      l.Mode,
		"seatCount": len(l.Seats),
		"teams":     teams,
	})
	return b
}

type LobbyManager struct {
	mu      sync.Mutex
	lobbies map[string]*Lobby
	gh      *GameHostClient
}

func NewLobbyManager(gh *GameHostClient) *LobbyManager {
	return &LobbyManager{lobbies: map[string]*Lobby{}, gh: gh}
}

// Create opens a new table with the host already seated at seat 0. Teams mode
// requires an even seat count of at least 4; anything else falls back to solo.
func (m *LobbyManager) Create(host LobbyPlayer, gameID string, seatCount int, mode string) *Lobby {
	if seatCount < 2 {
		seatCount = 2
	}
	if seatCount > 8 {
		seatCount = 8
	}
	if mode != "teams" {
		mode = "solo"
	}
	if mode == "teams" && (seatCount < 4 || seatCount%2 != 0) {
		mode = "solo"
	}
	seats := make([]Seat, seatCount)
	for i := range seats {
		seats[i] = Seat{Index: i, Team: teamOf(i, mode)}
	}
	seated := host
	seats[0].Player = &seated

	m.mu.Lock()
	defer m.mu.Unlock()
	l := &Lobby{
		ID:        hex.EncodeToString(randID(6)),
		GameID:    gameID,
		Host:      host.ID,
		Mode:      mode,
		Seats:     seats,
		Status:    "open",
		CreatedAt: time.Now(),
	}
	m.lobbies[l.ID] = l
	return cloneLobby(l)
}

// ListOpen returns tables still waiting to start, newest first.
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

// Sit seats a player in a specific empty seat (moving them from any seat they
// already hold). Choosing a seat is how a player joins the table — and, in teams
// mode, how they choose their partnership.
func (m *LobbyManager) Sit(id string, player LobbyPlayer, seatIndex int) (*Lobby, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.lobbies[id]
	if !ok {
		return nil, ErrLobbyNotFound
	}
	if l.Status != "open" {
		return nil, ErrLobbyStarted
	}
	if seatIndex < 0 || seatIndex >= len(l.Seats) {
		return nil, ErrBadSeat
	}
	if p := l.Seats[seatIndex].Player; p != nil {
		if p.ID == player.ID {
			return cloneLobby(l), nil // already there — no-op
		}
		return nil, ErrSeatTaken
	}
	if cur := l.seatOf(player.ID); cur >= 0 {
		l.Seats[cur].Player = nil // vacate the old seat (moving seats)
	}
	seated := player
	l.Seats[seatIndex].Player = &seated
	return cloneLobby(l), nil
}

// Join seats a player in the first open seat (the quick-join path used from the
// "Live now" list). Re-joining an already-seated player is a no-op.
func (m *LobbyManager) Join(_ context.Context, id string, player LobbyPlayer) (*Lobby, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.lobbies[id]
	if !ok {
		return nil, ErrLobbyNotFound
	}
	if l.hasPlayer(player.ID) {
		return cloneLobby(l), nil
	}
	if l.Status != "open" {
		return nil, ErrLobbyStarted
	}
	idx := -1
	for i := range l.Seats {
		if l.Seats[i].Player == nil {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, ErrLobbyFull
	}
	seated := player
	l.Seats[idx].Player = &seated
	return cloneLobby(l), nil
}

// Stand vacates the player's seat (they remain free to sit elsewhere).
func (m *LobbyManager) Stand(id, playerID string) (*Lobby, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.lobbies[id]
	if !ok {
		return nil, ErrLobbyNotFound
	}
	if l.Status != "open" {
		return nil, ErrLobbyStarted
	}
	cur := l.seatOf(playerID)
	if cur < 0 {
		return nil, ErrNotSeated
	}
	l.Seats[cur].Player = nil
	return cloneLobby(l), nil
}

// Start creates the real match on the game-host once every seat is filled. Only
// the host may start. Players are handed over in seat order, with the table
// config (mode + teams) so the game can set up partnerships.
func (m *LobbyManager) Start(ctx context.Context, id, hostID string) (*Lobby, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.lobbies[id]
	if !ok {
		return nil, ErrLobbyNotFound
	}
	if l.Host != hostID {
		return nil, ErrNotHost
	}
	if l.Status == "started" {
		return cloneLobby(l), nil
	}
	if !l.filled() {
		return nil, ErrNotReady
	}
	meta, err := m.gh.CreateMatch(ctx, l.GameID, l.orderedPlayerIDs(), l.matchConfig())
	if err != nil {
		return nil, err
	}
	l.MatchID = meta.ID
	l.Status = "started"
	return cloneLobby(l), nil
}

// Cancel removes an open table; only the host may cancel.
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
	cp.Seats = make([]Seat, len(l.Seats))
	for i, s := range l.Seats {
		cp.Seats[i] = s
		if s.Player != nil {
			p := *s.Player
			cp.Seats[i].Player = &p
		}
	}
	return &cp
}

func randID(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}
