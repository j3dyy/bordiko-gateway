package main

import (
	"context"
	"encoding/json"
	"log"
	"sync"
)

// Hub tracks which WebSocket clients are watching which match ("rooms") and fans
// out per-player redacted state after every accepted move. It holds no game
// state — the game-host is authoritative; the hub only routes.
type Hub struct {
	mu    sync.RWMutex
	rooms map[string]map[*Client]struct{}
	gh    *GameHostClient
	auth  *Auth
}

func NewHub(gh *GameHostClient, auth *Auth) *Hub {
	return &Hub{rooms: make(map[string]map[*Client]struct{}), gh: gh, auth: auth}
}

func (h *Hub) add(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	room := h.rooms[c.matchID]
	if room == nil {
		room = make(map[*Client]struct{})
		h.rooms[c.matchID] = room
	}
	room[c] = struct{}{}
}

func (h *Hub) remove(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if room := h.rooms[c.matchID]; room != nil {
		delete(room, c)
		if len(room) == 0 {
			delete(h.rooms, c.matchID)
		}
	}
}

func (h *Hub) clientsIn(matchID string) []*Client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	room := h.rooms[matchID]
	out := make([]*Client, 0, len(room))
	for c := range room {
		out = append(out, c)
	}
	return out
}

// handleMove forwards an intent to the game-host and, on success, broadcasts the
// new redacted state to everyone in the room. A guest rejection (422) comes back
// to the sender as a move_err — never mutating anything.
func (h *Hub) handleMove(ctx context.Context, c *Client, cm clientMessage) {
	res, status, err := h.gh.ApplyMove(ctx, c.matchID, c.playerID, cm.Move.Type, cm.Move.Payload)
	if err != nil {
		c.trySend(mustJSON(map[string]any{"t": "error", "code": "host_error", "message": err.Error()}))
		return
	}
	if status == 422 || res == nil || !res.OK {
		reason := "illegal move"
		if res != nil {
			reason = res.Error
		}
		c.trySend(mustJSON(map[string]any{
			"t": "move_err", "matchId": c.matchID, "clientMoveId": cm.ClientMoveID, "reason": reason,
		}))
		return
	}
	c.trySend(mustJSON(map[string]any{
		"t": "move_ok", "matchId": c.matchID, "clientMoveId": cm.ClientMoveID, "moveCount": res.MoveCount,
	}))
	h.broadcastState(ctx, c.matchID, res.Events)
}

// broadcastState sends every client in the room its own redacted view (plus the
// current player's legal moves) and the events from the last move.
func (h *Hub) broadcastState(ctx context.Context, matchID string, events json.RawMessage) {
	meta, err := h.gh.GetMatch(ctx, matchID)
	if err != nil {
		log.Printf("broadcast %s: %v", matchID, err)
		return
	}
	hasEvents := len(events) > 0 && string(events) != "null"
	for _, c := range h.clientsIn(matchID) {
		if msg, err := h.stateMessage(ctx, matchID, c, meta); err == nil {
			c.trySend(msg)
		}
		if hasEvents {
			c.trySend(mustJSON(map[string]any{
				"t": "events", "matchId": matchID, "moveCount": meta.MoveCount, "events": events,
			}))
		}
	}
}

// stateMessage builds the redacted "state" message for one client: the
// game-host's per-player view, augmented with whose turn it is and — when it is
// this client's turn — the legal moves they may play.
func (h *Hub) stateMessage(ctx context.Context, matchID string, c *Client, meta *MatchMeta) ([]byte, error) {
	view, err := h.gh.GetView(ctx, matchID, c.playerID)
	if err != nil {
		return nil, err
	}
	obj := map[string]json.RawMessage{}
	if err := json.Unmarshal(view, &obj); err != nil {
		return nil, err
	}
	obj["t"] = mustJSON("state")
	obj["matchId"] = mustJSON(matchID)
	yourTurn := !meta.Ended && meta.CurrentPlayer == c.playerID
	obj["yourTurn"] = mustJSON(yourTurn)
	if yourTurn {
		if legal, err := h.gh.GetLegalMoves(ctx, matchID); err == nil {
			obj["legalMoves"] = legal
		}
	}
	return json.Marshal(obj)
}
