package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// CheckOrigin is configured at startup (setWSAllowedOrigins) to the same
	// allow-list used for CORS, so only the configured frontend(s) may connect.
	CheckOrigin: func(_ *http.Request) bool { return true },
}

// setWSAllowedOrigins pins the WebSocket origin check to the given set. An empty
// set keeps the permissive dev default.
func setWSAllowedOrigins(origins []string) {
	if len(origins) == 0 {
		return
	}
	allowed := make(map[string]bool, len(origins))
	for _, o := range origins {
		allowed[strings.TrimRight(o, "/")] = true
	}
	upgrader.CheckOrigin = func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true // non-browser clients (tools, tests) send no Origin
		}
		return allowed[origin]
	}
}

const (
	writeWait  = 10 * time.Second
	pingPeriod = 30 * time.Second
	maxMessage = 1 << 20
)

// Client is one WebSocket connection: a single player watching one match.
type Client struct {
	hub        *Hub
	conn       *websocket.Conn
	send       chan []byte
	matchID    string
	playerID   string
	playerName string
	done       chan struct{}
	closeOnce  sync.Once
}

// clientMessage is the inbound wire message (see packages/shared/protocol.ts).
type clientMessage struct {
	T            string `json:"t"`
	ClientMoveID string `json:"clientMoveId"`
	Move         struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	} `json:"move"`
	Text string `json:"text"` // chat message body
	TS   int64  `json:"ts"`
}

func (h *Hub) serveWS(w http.ResponseWriter, r *http.Request) {
	// Login is required to play: the player identity comes from the session, not
	// a query param, so a client can only ever act as itself.
	claims, ok := h.auth.sessionUser(r)
	if !ok {
		http.Error(w, "login required", http.StatusUnauthorized)
		return
	}
	playerID := claims.Sub

	matchID := r.URL.Query().Get("matchId")
	if matchID == "" {
		http.Error(w, "matchId query param is required", http.StatusBadRequest)
		return
	}
	// Confirm the match exists before upgrading.
	meta, err := h.gh.GetMatch(r.Context(), matchID)
	if err != nil {
		http.Error(w, "unknown match", http.StatusNotFound)
		return
	}
	// The authenticated user must be one of the match's players.
	if !contains(meta.Players, playerID) {
		http.Error(w, "you are not a player in this match", http.StatusForbidden)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already wrote the error
	}
	c := &Client{
		hub:        h,
		conn:       conn,
		send:       make(chan []byte, 32),
		matchID:    matchID,
		playerID:   playerID,
		playerName: claims.Name,
		done:       make(chan struct{}),
	}
	h.add(c)
	go c.writePump()
	go c.readPump()

	// Push the current state to the freshly-joined client.
	if msg, err := h.stateMessage(context.Background(), matchID, c, meta); err == nil {
		c.trySend(msg)
	}
}

func (c *Client) close() {
	c.closeOnce.Do(func() {
		c.hub.remove(c)
		close(c.done)
		_ = c.conn.Close()
	})
}

func (c *Client) trySend(msg []byte) {
	select {
	case c.send <- msg:
	case <-c.done:
	default:
		// The client's buffer is full — it can't keep up. Drop it.
		c.close()
	}
}

func (c *Client) readPump() {
	defer c.close()
	c.conn.SetReadLimit(maxMessage)
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		var cm clientMessage
		if err := json.Unmarshal(data, &cm); err != nil {
			c.trySend(mustJSON(map[string]any{"t": "error", "code": "bad_message", "message": "invalid JSON"}))
			continue
		}
		ctx := context.Background()
		switch cm.T {
		case "move":
			c.hub.handleMove(ctx, c, cm)
		case "chat":
			c.hub.handleChat(c, cm)
		case "ping":
			c.trySend(mustJSON(map[string]any{"t": "pong", "ts": cm.TS}))
		case "leave":
			return
		default:
			c.trySend(mustJSON(map[string]any{"t": "error", "code": "unknown", "message": "unknown message type: " + cm.T}))
		}
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case msg := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				c.close()
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				c.close()
				return
			}
		case <-c.done:
			return
		}
	}
}
