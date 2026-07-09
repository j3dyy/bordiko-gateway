// Command gateway is the player-facing edge of Bordiko.
//
// It handles auth (Google/GitHub OAuth + sessions), runs a lobby for
// login-required matchmaking, terminates WebSocket connections for real-time
// play, routes intents to the authoritative game-host, serves an enriched
// leaderboard, and proxies the remaining lobby REST to the game-host so the
// browser talks to a single origin.
//
// Config (env):
//
//	GATEWAY_ADDR      listen address                         (default ":8080")
//	GAME_HOST_URL     authoritative game-host base           (default "http://localhost:8081")
//	DATABASE_URL      Postgres DSN for accounts (else in-memory)
//	SESSION_SECRET    HMAC key for session cookies (else ephemeral)
//	PUBLIC_URL        gateway's public base for OAuth redirect_uri (default "http://localhost:8080")
//	FRONTEND_URL      where to send the browser after login  (default "http://localhost:5173")
//	ALLOWED_ORIGINS   comma-separated CORS/WS origin allow-list (default: FRONTEND_URL + localhost)
//	GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET   enable Google login
//	GITHUB_CLIENT_ID / GITHUB_CLIENT_SECRET   enable GitHub login
//	AUTH_DEV_ENABLED  "true" (default) enables passwordless dev login
//	COOKIE_SECURE     "true" to mark session cookies Secure (behind TLS)
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"
)

func main() {
	ctx := context.Background()
	ghURL := env("GAME_HOST_URL", "http://localhost:8081")
	gh := NewGameHostClient(ghURL)

	users, err := openUserStore(ctx)
	if err != nil {
		log.Fatalf("user store: %v", err)
	}
	defer users.Close()

	auth := NewAuth(users)
	origins := allowedOrigins(auth.frontendURL)
	setWSAllowedOrigins(origins)

	hub := NewHub(gh, auth)
	lobby := NewLobbyManager(gh)
	gw := NewGateway(hub, gh, auth, lobby, ghURL, origins)

	addr := env("GATEWAY_ADDR", ":8080")
	log.Printf("bordiko gateway listening on %s → game-host %s (origins %v)", addr, ghURL, origins)
	if err := http.ListenAndServe(addr, gw.Routes()); err != nil {
		log.Fatalf("gateway failed: %v", err)
	}
}

func openUserStore(ctx context.Context) (UserStore, error) {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		log.Printf("gateway using Postgres user store")
		return NewPostgresUserStore(ctx, url)
	}
	log.Printf("gateway using in-memory user store (set DATABASE_URL to persist accounts)")
	return NewMemoryUserStore(), nil
}

// allowedOrigins is the CORS + WebSocket origin allow-list: ALLOWED_ORIGINS if
// set, otherwise the frontend URL plus the usual localhost dev origins.
func allowedOrigins(frontendURL string) []string {
	if raw := os.Getenv("ALLOWED_ORIGINS"); raw != "" {
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, strings.TrimRight(p, "/"))
			}
		}
		return out
	}
	seen := map[string]bool{}
	out := []string{}
	for _, o := range []string{frontendURL, "http://localhost:5173", "http://localhost:4173"} {
		o = strings.TrimRight(o, "/")
		if o != "" && !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	return out
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
