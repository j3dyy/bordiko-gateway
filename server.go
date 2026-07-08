package main

import (
	"encoding/json"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// Gateway is the player-facing edge: a WebSocket endpoint for real-time play and
// a thin REST proxy to the game-host for lobby actions (so the browser only ever
// talks to one origin).
type Gateway struct {
	hub   *Hub
	ghURL string
}

func NewGateway(hub *Hub, ghURL string) *Gateway {
	return &Gateway{hub: hub, ghURL: ghURL}
}

func (gw *Gateway) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("GET /readyz", health)
	mux.HandleFunc("GET /ws", gw.hub.serveWS)

	// Lobby REST is proxied straight through to the game-host:
	//   POST /api/matches, GET /api/games, GET /api/matches/{id}, ...
	target, _ := url.Parse(gw.ghURL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	mux.Handle("/api/", http.StripPrefix("/api", proxy))

	return withCORS(mux)
}

func health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"service":"gateway","status":"ok"}`))
}

// withCORS allows the browser client (served from a different origin in dev) to
// call the REST proxy. WebSocket origin is governed separately by CheckOrigin.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{"error":"marshal_failed"}`)
	}
	return b
}
