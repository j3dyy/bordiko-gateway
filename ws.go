package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// Dev: accept any origin. Phase-4 hardening ties this to the configured
	// frontend origin and adds auth on the token.
	CheckOrigin: func(_ *http.Request) bool { return true },
}

const (
	writeWait  = 10 * time.Second
	pingPeriod = 30 * time.Second
	maxMessage = 1 << 20
)

// Client is one WebSocket connection: a single player watching one match.
type Client struct {
	hub       *Hub
	conn      *websocket.Conn
	send      chan []byte
	matchID   string
	playerID  string
	done      chan struct{}
	closeOnce sync.Once
}

// clientMessage is the inbound wire message (see packages/shared/protocol.ts).
type clientMessage struct {
	T            string `json:"t"`
	ClientMoveID string `json:"clientMoveId"`
	Move         struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	} `json:"move"`
	TS int64 `json:"ts"`
}

func (h *Hub) serveWS(w http.ResponseWriter, r *http.Request) {
	matchID := r.URL.Query().Get("matchId")
	playerID := r.URL.Query().Get("playerId")
	if matchID == "" || playerID == "" {
		http.Error(w, "matchId and playerId query params are required", http.StatusBadRequest)
		return
	}
	// Confirm the match exists (and is joinable) before upgrading.
	meta, err := h.gh.GetMatch(r.Context(), matchID)
	if err != nil {
		http.Error(w, "unknown match", http.StatusNotFound)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already wrote the error
	}
	c := &Client{
		hub:      h,
		conn:     conn,
		send:     make(chan []byte, 32),
		matchID:  matchID,
		playerID: playerID,
		done:     make(chan struct{}),
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
