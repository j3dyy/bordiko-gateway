package main

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"
)

// Hub tracks which WebSocket clients are watching which match ("rooms") and fans
// out per-player redacted state after every accepted move. It holds no game
// state — the game-host is authoritative; the hub only routes. It also runs a
// per-match TURN TIMER: when a turn opens, a timer is armed; if the acting
// player doesn't move before it fires, the hub auto-plays a safe (first legal)
// move on their behalf so a slow or absent player can't stall the table.
type Hub struct {
	mu    sync.RWMutex
	rooms map[string]map[*Client]struct{}
	gh    *GameHostClient
	auth  *Auth

	turnLimit time.Duration
	tmu       sync.Mutex
	turns     map[string]*turnState
}

// turnState is the live timer for one match's current turn.
type turnState struct {
	timer     *time.Timer
	moveCount int   // the turn this timer belongs to (guards against stale fires)
	deadline  int64 // unix ms, surfaced to clients for a countdown
}

func NewHub(gh *GameHostClient, auth *Auth, turnLimit time.Duration) *Hub {
	return &Hub{
		rooms:     make(map[string]map[*Client]struct{}),
		gh:        gh,
		auth:      auth,
		turnLimit: turnLimit,
		turns:     make(map[string]*turnState),
	}
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
	empty := false
	if room := h.rooms[c.matchID]; room != nil {
		delete(room, c)
		if len(room) == 0 {
			delete(h.rooms, c.matchID)
			empty = true
		}
	}
	h.mu.Unlock()
	// Nobody is watching → stop auto-playing. The timer re-arms when someone
	// (re)joins, so a match is never driven forward with an empty room.
	if empty {
		h.cancelTurn(c.matchID)
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

// handleChat relays a player's chat message to everyone in the match room
// (including the sender, so their own line echoes). Chat is ephemeral — the hub
// routes it and keeps no history. The name/id come from the authenticated
// session, so a client can't spoof another player.
func (h *Hub) handleChat(c *Client, cm clientMessage) {
	text := strings.TrimSpace(cm.Text)
	if text == "" {
		return
	}
	if len(text) > 500 {
		text = text[:500]
	}
	msg := mustJSON(map[string]any{
		"t": "chat", "matchId": c.matchID, "from": c.playerID, "name": c.playerName, "text": text, "ts": cm.TS,
	})
	for _, cl := range h.clientsIn(c.matchID) {
		cl.trySend(msg)
	}
}

// allowedEmotes is the fixed set of quick reactions a client may send (keeps the
// channel to a known, safe vocabulary — the client can't inject arbitrary text).
var allowedEmotes = map[string]bool{
	"ring": true, "love": true, "like": true, "dislike": true,
	"haha": true, "clap": true, "wow": true, "think": true,
}

// handleEmote relays a quick reaction (a "poke"/emoji) to everyone in the match
// room. Ephemeral like chat; the name comes from the session so it can't be
// spoofed.
func (h *Hub) handleEmote(c *Client, cm clientMessage) {
	if !allowedEmotes[cm.Emote] {
		return
	}
	msg := mustJSON(map[string]any{
		"t": "emote", "matchId": c.matchID, "from": c.playerID, "name": c.playerName, "emote": cm.Emote, "ts": cm.TS,
	})
	for _, cl := range h.clientsIn(c.matchID) {
		cl.trySend(msg)
	}
}

// broadcastState sends every client in the room its own redacted view (plus the
// current player's legal moves) and the events from the last move.
func (h *Hub) broadcastState(ctx context.Context, matchID string, events json.RawMessage) {
	meta, err := h.gh.GetMatch(ctx, matchID)
	if err != nil {
		log.Printf("broadcast %s: %v", matchID, err)
		return
	}
	deadline := h.armTurn(matchID, meta)
	names := h.resolveNames(ctx, matchID, meta.Players)
	hasEvents := len(events) > 0 && string(events) != "null"
	for _, c := range h.clientsIn(matchID) {
		if msg, err := h.stateMessage(ctx, matchID, c, meta, deadline, names); err == nil {
			c.trySend(msg)
		}
		if hasEvents {
			c.trySend(mustJSON(map[string]any{
				"t": "events", "matchId": matchID, "moveCount": meta.MoveCount, "events": events,
			}))
		}
	}
}

// resolveNames maps the match's player ids to display names: the durable store
// names first, then overlaid with the names of currently-connected clients (from
// their session), so a name still shows even if the account store is empty (e.g.
// in-memory + a recent redeploy).
func (h *Hub) resolveNames(ctx context.Context, matchID string, players []string) map[string]string {
	names := h.auth.DisplayNames(ctx, players)
	for _, c := range h.clientsIn(matchID) {
		if c.playerName != "" {
			names[c.playerID] = c.playerName
		}
	}
	return names
}

// stateMessage builds the redacted "state" message for one client: the
// game-host's per-player view, augmented with whose turn it is and — when it is
// this client's turn — the legal moves they may play.
func (h *Hub) stateMessage(ctx context.Context, matchID string, c *Client, meta *MatchMeta, deadline int64, names map[string]string) ([]byte, error) {
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
	if len(names) > 0 {
		obj["names"] = mustJSON(names) // player id → display name, for labelling the board
	}
	yourTurn := !meta.Ended && meta.CurrentPlayer == c.playerID
	obj["yourTurn"] = mustJSON(yourTurn)
	if yourTurn {
		if legal, err := h.gh.GetLegalMoves(ctx, matchID); err == nil {
			obj["legalMoves"] = legal
		}
	}
	if deadline > 0 && !meta.Ended {
		obj["turnDeadline"] = mustJSON(deadline)
	}
	return json.Marshal(obj)
}

/* ------------------------------- turn timer ------------------------------- */

// armTurn (re)starts the timer for a match's current turn and returns the turn's
// deadline (unix ms), or 0 when there's no timing (ended / disabled). Re-arming
// for the SAME turn keeps the existing deadline, so a mid-turn broadcast (e.g. a
// client reconnecting) doesn't reset the clock.
func (h *Hub) armTurn(matchID string, meta *MatchMeta) int64 {
	if h.turnLimit <= 0 {
		return 0
	}
	h.tmu.Lock()
	defer h.tmu.Unlock()
	ts := h.turns[matchID]
	if meta.Ended {
		if ts != nil {
			ts.timer.Stop()
			delete(h.turns, matchID)
		}
		return 0
	}
	if ts != nil && ts.moveCount == meta.MoveCount {
		return ts.deadline // same turn — keep the running clock
	}
	if ts != nil {
		ts.timer.Stop()
	}
	mc := meta.MoveCount
	deadline := time.Now().Add(h.turnLimit).UnixMilli()
	t := time.AfterFunc(h.turnLimit, func() { h.onTurnTimeout(matchID, mc) })
	h.turns[matchID] = &turnState{timer: t, moveCount: mc, deadline: deadline}
	return deadline
}

func (h *Hub) cancelTurn(matchID string) {
	h.tmu.Lock()
	defer h.tmu.Unlock()
	if ts := h.turns[matchID]; ts != nil {
		ts.timer.Stop()
		delete(h.turns, matchID)
	}
}

// onTurnTimeout fires when a turn ran out of time. If it's still the same turn,
// the match is live, and someone is watching, the hub auto-plays the first legal
// move for the current player — then broadcasts, which arms the next turn.
func (h *Hub) onTurnTimeout(matchID string, mc int) {
	if len(h.clientsIn(matchID)) == 0 {
		h.cancelTurn(matchID)
		return
	}
	ctx := context.Background()
	meta, err := h.gh.GetMatch(ctx, matchID)
	if err != nil || meta.Ended || meta.MoveCount != mc {
		return // stale or already moved/ended
	}
	legal, err := h.gh.GetLegalMoves(ctx, matchID)
	if err != nil {
		return
	}
	var moves []struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(legal, &moves); err != nil || len(moves) == 0 {
		return
	}
	pick := moves[0] // first legal move: Pass on a bid, No-trump on trump, a card on play
	res, status, err := h.gh.ApplyMove(ctx, matchID, meta.CurrentPlayer, pick.Type, pick.Payload)
	if err != nil || status == 422 || res == nil || !res.OK {
		log.Printf("turn timeout auto-move %s (%s): status=%d err=%v", matchID, meta.CurrentPlayer, status, err)
		return
	}
	// Tell the room whose turn was auto-played, then broadcast the new state.
	notice := mustJSON(map[string]any{"t": "turn_timeout", "matchId": matchID, "player": meta.CurrentPlayer, "auto": pick.Type})
	for _, c := range h.clientsIn(matchID) {
		c.trySend(notice)
	}
	h.broadcastState(ctx, matchID, res.Events)
}
