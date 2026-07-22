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

	botMu    sync.Mutex
	botTurns map[string]int // matchID → moveCount already scheduled for a bot

	// Real-time clock. realtimeOf reports a game's fixed tick timestep (ms), or
	// ok=false for a turn-based game (set by the gateway). ticks holds the running
	// per-match tick loops. A real-time match is driven forward by this clock (not
	// by the turn timer / bots) while at least one client is watching.
	realtimeOf func(gameID string) (dtMs int, ok bool)
	tickMu     sync.Mutex
	ticks      map[string]*tickState
}

// tickState is one match's running real-time clock; closing stop ends it.
type tickState struct {
	stop chan struct{}
}

// botDelay is the pause before a bot plays its turn, so the table can watch each
// move land rather than the bots resolving a whole trick instantly. Tunable via
// BOT_DELAY_MS (see main.go).
var botDelay = 900 * time.Millisecond

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
		botTurns:  make(map[string]int),
		ticks:     make(map[string]*tickState),
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
	// Nobody is watching → stop driving the match forward. The turn timer and the
	// real-time clock both re-arm when someone (re)joins, so a match is never
	// advanced with an empty room.
	if empty {
		h.cancelTurn(c.matchID)
		h.cancelTick(c.matchID)
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
	h.maybeDriveBot(matchID, meta)
	h.maybeStartTick(matchID, meta)
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
	// Bots never connect over WS and aren't in the user store — label them here.
	for _, p := range players {
		if isBot(p) {
			names[p] = botDisplayName(p)
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
	// yourTurn is true for the current player OR — in simultaneous mode — any seat
	// in the active set (a shared vote / night acknowledge / quest). Legal moves are
	// fetched for THIS seat, not the global current player.
	yourTurn := !meta.Ended && meta.IsActive(c.playerID)
	obj["yourTurn"] = mustJSON(yourTurn)
	if yourTurn {
		if legal, err := h.gh.GetLegalMoves(ctx, matchID, c.playerID); err == nil {
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
	if h.turnLimit <= 0 || h.isRealtime(meta.GameID) {
		// Real-time matches are advanced by the tick clock, not the per-turn
		// auto-play timer — so no turn deadline and no timeout auto-move.
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
	if ts := h.turns[matchID]; ts != nil {
		ts.timer.Stop()
		delete(h.turns, matchID)
	}
	h.tmu.Unlock()
	// Forget any scheduled bot turn so a rejoin re-drives it from scratch.
	h.botMu.Lock()
	delete(h.botTurns, matchID)
	h.botMu.Unlock()
}

/* ------------------------------ real-time clock --------------------------- */

// isRealtime reports whether a game is real-time (has a tick clock). Safe when no
// resolver is wired (tests / turn-based-only deployments) — returns false.
func (h *Hub) isRealtime(gameID string) bool {
	if h.realtimeOf == nil {
		return false
	}
	_, ok := h.realtimeOf(gameID)
	return ok
}

// maybeStartTick arms a match's real-time clock: while at least one client is
// watching a live real-time match, the host drives its world forward at the
// game's tick rate. Idempotent — a second watcher (or a rebroadcast) won't start
// a second loop.
func (h *Hub) maybeStartTick(matchID string, meta *MatchMeta) {
	if meta.Ended || h.realtimeOf == nil {
		return
	}
	dtMs, ok := h.realtimeOf(meta.GameID)
	if !ok || dtMs <= 0 {
		return
	}
	if len(h.clientsIn(matchID)) == 0 {
		return
	}
	h.tickMu.Lock()
	if _, running := h.ticks[matchID]; running {
		h.tickMu.Unlock()
		return
	}
	ts := &tickState{stop: make(chan struct{})}
	h.ticks[matchID] = ts
	h.tickMu.Unlock()
	go h.runTicks(matchID, dtMs, ts.stop)
}

// runTicks is one match's clock goroutine: every dt it advances the world by one
// fixed step (game-host tick) and broadcasts the new state. It stops when the
// room empties, the match ends, or the match is gone. A single slow/failed tick
// is skipped, not fatal — the next tick tries again.
func (h *Hub) runTicks(matchID string, dtMs int, stop chan struct{}) {
	interval := time.Duration(dtMs) * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if len(h.clientsIn(matchID)) == 0 {
				h.cancelTick(matchID)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), interval+2*time.Second)
			res, status, err := h.gh.Tick(ctx, matchID, dtMs)
			cancel()
			if status == 404 || status == 409 {
				// Match gone or already ended — nothing more to drive.
				h.cancelTick(matchID)
				return
			}
			if err != nil || res == nil || !res.OK {
				continue // transient guest error — skip this frame
			}
			h.broadcastState(context.Background(), matchID, res.Events)
			if res.Ended {
				h.cancelTick(matchID)
				return
			}
		}
	}
}

func (h *Hub) cancelTick(matchID string) {
	h.tickMu.Lock()
	if ts := h.ticks[matchID]; ts != nil {
		close(ts.stop)
		delete(h.ticks, matchID)
	}
	h.tickMu.Unlock()
}

// actionableSeats is the set of seats that may act right now: the simultaneous
// active set, or the single current player for an ordinary turn.
func actionableSeats(meta *MatchMeta) []string {
	if len(meta.Active) > 0 {
		return meta.Active
	}
	if meta.CurrentPlayer != "" {
		return []string{meta.CurrentPlayer}
	}
	return nil
}

// onTurnTimeout fires when a turn ran out of time. If it's still the same turn,
// the match is live, and someone is watching, the hub auto-plays the first legal
// move for EVERY seat still to act (one seat in an ordinary turn; the whole
// pending set in a simultaneous stage) — then broadcasts, which arms the next turn.
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
	played := false
	for _, seat := range actionableSeats(meta) {
		if mv, ok := h.autoPlaySeat(ctx, matchID, seat); ok {
			played = true
			notice := mustJSON(map[string]any{"t": "turn_timeout", "matchId": matchID, "player": seat, "auto": mv})
			for _, c := range h.clientsIn(matchID) {
				c.trySend(notice)
			}
		}
	}
	if played {
		h.broadcastState(ctx, matchID, nil)
	}
}

// autoPlaySeat plays the first legal move for one seat (used on timeout). Returns
// the move type and whether it applied — a seat with no legal moves (already
// acted, or the phase moved on) is skipped.
func (h *Hub) autoPlaySeat(ctx context.Context, matchID, seat string) (string, bool) {
	legal, err := h.gh.GetLegalMoves(ctx, matchID, seat)
	if err != nil {
		return "", false
	}
	var moves []struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(legal, &moves); err != nil || len(moves) == 0 {
		return "", false
	}
	pick := moves[0] // first legal move: Pass on a bid, No-trump on trump, ready/approve, a card
	res, status, err := h.gh.ApplyMove(ctx, matchID, seat, pick.Type, pick.Payload)
	if err != nil || status == 422 || res == nil || !res.OK {
		log.Printf("turn timeout auto-move %s (%s): status=%d err=%v", matchID, seat, status, err)
		return "", false
	}
	return pick.Type, true
}

/* --------------------------------- bots ----------------------------------- */

func botDisplayName(id string) string {
	if n := strings.TrimPrefix(id, botPrefix); n != id {
		return "Bot " + n
	}
	return "Bot"
}

// maybeDriveBot schedules a bot's move when it's a bot's turn and at least one
// human is watching (bots never open a WebSocket, so any client in the room is a
// human). It's idempotent per turn: a repeat broadcast for the same move count
// (e.g. a reconnect) won't double-schedule. When the bot plays it broadcasts,
// which calls this again for the next actor — so a run of consecutive bot seats
// resolves one visible move at a time until a human is on turn or the hand ends.
// pendingBotSeat returns a bot seat that may act right now (the current player, or
// any bot in the simultaneous active set), or "" if no bot is on to act.
func pendingBotSeat(meta *MatchMeta) string {
	for _, s := range actionableSeats(meta) {
		if isBot(s) {
			return s
		}
	}
	return ""
}

func (h *Hub) maybeDriveBot(matchID string, meta *MatchMeta) {
	if meta.Ended || h.isRealtime(meta.GameID) || pendingBotSeat(meta) == "" {
		return
	}
	if len(h.clientsIn(matchID)) == 0 {
		return // nobody watching — don't drive the table forward
	}
	h.botMu.Lock()
	if h.botTurns[matchID] == meta.MoveCount {
		h.botMu.Unlock()
		return
	}
	h.botTurns[matchID] = meta.MoveCount
	h.botMu.Unlock()

	mc := meta.MoveCount
	time.AfterFunc(botDelay, func() { h.driveBot(matchID, mc) })
}

// driveBot plays one bot move for the match's current (bot) player: fetch its
// view + legal moves, choose one, apply it, and broadcast.
func (h *Hub) driveBot(matchID string, mc int) {
	if len(h.clientsIn(matchID)) == 0 {
		return
	}
	ctx := context.Background()
	meta, err := h.gh.GetMatch(ctx, matchID)
	if err != nil || meta.Ended || meta.MoveCount != mc {
		return // stale, ended, or already moved
	}
	bot := pendingBotSeat(meta)
	if bot == "" {
		return // no longer a bot's turn
	}
	view, err := h.gh.GetView(ctx, matchID, bot)
	if err != nil {
		return
	}
	legalRaw, err := h.gh.GetLegalMoves(ctx, matchID, bot)
	if err != nil {
		return
	}
	var legal []moveDesc
	if err := json.Unmarshal(legalRaw, &legal); err != nil || len(legal) == 0 {
		return
	}
	pick := chooseBotMove(meta.GameID, view, legal)
	if pick == nil {
		return
	}
	res, status, err := h.gh.ApplyMove(ctx, matchID, bot, pick.Type, pick.Payload)
	if err != nil || status == 422 || res == nil || !res.OK {
		log.Printf("bot move %s (%s): status=%d err=%v", matchID, bot, status, err)
		return
	}
	h.broadcastState(ctx, matchID, res.Events)
}
