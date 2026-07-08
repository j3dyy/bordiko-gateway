// Command gateway is the player-facing edge of Bordiko.
//
// It terminates WebSocket connections for real-time play, routes intents to the
// authoritative game-host, and fans out each player's redacted state. Lobby REST
// calls are proxied to the game-host so the browser talks to a single origin.
//
// Config (env):
//   GATEWAY_ADDR    listen address                 (default ":8080")
//   GAME_HOST_URL   authoritative game-host base   (default "http://localhost:8081")
package main

import (
	"log"
	"net/http"
	"os"
)

func main() {
	ghURL := env("GAME_HOST_URL", "http://localhost:8081")
	gw := NewGateway(NewHub(NewGameHostClient(ghURL)), ghURL)

	addr := env("GATEWAY_ADDR", ":8080")
	log.Printf("bordiko gateway listening on %s → game-host %s", addr, ghURL)
	if err := http.ListenAndServe(addr, gw.Routes()); err != nil {
		log.Fatalf("gateway failed: %v", err)
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
