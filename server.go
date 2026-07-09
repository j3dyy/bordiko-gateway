package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
)

// Gateway is the player-facing edge: OAuth + sessions, a lobby for login-required
// matchmaking, a WebSocket endpoint for real-time play, an enriched leaderboard,
// and a thin REST proxy to the game-host for the remaining lobby/view actions (so
// the browser only ever talks to one origin).
type Gateway struct {
	hub            *Hub
	gh             *GameHostClient
	auth           *Auth
	lobby          *LobbyManager
	ghURL          string
	allowedOrigins []string
}

func NewGateway(hub *Hub, gh *GameHostClient, auth *Auth, lobby *LobbyManager, ghURL string, allowedOrigins []string) *Gateway {
	return &Gateway{hub: hub, gh: gh, auth: auth, lobby: lobby, ghURL: ghURL, allowedOrigins: allowedOrigins}
}

func (gw *Gateway) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("GET /readyz", health)
	mux.HandleFunc("GET /ws", gw.hub.serveWS)

	// Auth: /auth/providers, /auth/{provider}/login|callback, /auth/me, /auth/logout
	gw.auth.RegisterRoutes(mux)

	// Lobby (login required — you must be signed in to create or join a match).
	mux.HandleFunc("GET /api/lobby", gw.requireAuth(gw.listLobbies))
	mux.HandleFunc("POST /api/lobby", gw.requireAuth(gw.createLobby))
	mux.HandleFunc("GET /api/lobby/{id}", gw.requireAuth(gw.getLobby))
	mux.HandleFunc("POST /api/lobby/{id}/join", gw.requireAuth(gw.joinLobby))
	mux.HandleFunc("DELETE /api/lobby/{id}", gw.requireAuth(gw.cancelLobby))

	// Leaderboard (public read) — enriched with display names.
	mux.HandleFunc("GET /api/leaderboard", gw.handleLeaderboard)

	// Everything else under /api/ (games list, match summary, view, legal, moves)
	// is proxied straight through to the game-host.
	target, _ := url.Parse(gw.ghURL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	mux.Handle("/api/", http.StripPrefix("/api", proxy))

	return gw.withCORS(mux)
}

func health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"service":"gateway","status":"ok"}`))
}

/* --------------------------------- auth ----------------------------------- */

// requireAuth wraps a handler so it only runs for a signed-in user, passing the
// verified session claims through.
func (gw *Gateway) requireAuth(h func(http.ResponseWriter, *http.Request, *sessionClaims)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := gw.auth.sessionUser(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "login_required"})
			return
		}
		h(w, r, claims)
	}
}

/* --------------------------------- lobby ---------------------------------- */

func (gw *Gateway) listLobbies(w http.ResponseWriter, _ *http.Request, _ *sessionClaims) {
	writeJSON(w, http.StatusOK, map[string]any{"lobbies": gw.lobby.ListOpen()})
}

func (gw *Gateway) createLobby(w http.ResponseWriter, r *http.Request, u *sessionClaims) {
	var req struct {
		GameID string `json:"gameId"`
		Seats  int    `json:"seats"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.GameID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_request", "message": "gameId required"})
		return
	}
	l := gw.lobby.Create(LobbyPlayer{ID: u.Sub, Name: u.Name}, req.GameID, req.Seats)
	writeJSON(w, http.StatusCreated, l)
}

func (gw *Gateway) getLobby(w http.ResponseWriter, r *http.Request, _ *sessionClaims) {
	l, err := gw.lobby.Get(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not_found"})
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (gw *Gateway) joinLobby(w http.ResponseWriter, r *http.Request, u *sessionClaims) {
	l, err := gw.lobby.Join(r.Context(), r.PathValue("id"), LobbyPlayer{ID: u.Sub, Name: u.Name})
	if err != nil {
		switch {
		case errors.Is(err, ErrLobbyNotFound):
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "not_found"})
		case errors.Is(err, ErrLobbyFull):
			writeJSON(w, http.StatusConflict, map[string]any{"error": "lobby_full"})
		default:
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "start_failed", "message": err.Error()})
		}
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (gw *Gateway) cancelLobby(w http.ResponseWriter, r *http.Request, u *sessionClaims) {
	err := gw.lobby.Cancel(r.PathValue("id"), u.Sub)
	switch {
	case errors.Is(err, ErrLobbyNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not_found"})
	case errors.Is(err, ErrNotHost):
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "not_host"})
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

/* ------------------------------ leaderboard ------------------------------- */

// leaderboardRow is the enriched leaderboard entry the browser consumes.
type leaderboardRow struct {
	UserID      string  `json:"userId"`
	DisplayName string  `json:"displayName"`
	AvatarURL   string  `json:"avatarUrl"`
	Rating      int     `json:"rating"`
	Wins        int     `json:"wins"`
	Losses      int     `json:"losses"`
	Draws       int     `json:"draws"`
	Games       int     `json:"games"`
	WinRate     float64 `json:"winRate"`
}

func (gw *Gateway) handleLeaderboard(w http.ResponseWriter, r *http.Request) {
	gameID := r.URL.Query().Get("gameId")
	entries, err := gw.gh.GetLeaderboard(r.Context(), gameID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "leaderboard_unavailable"})
		return
	}
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.Player
	}
	users, _ := gw.auth.users.GetMany(r.Context(), ids)

	rows := make([]leaderboardRow, 0, len(entries))
	for _, e := range entries {
		name, avatar := e.Player, ""
		if u := users[e.Player]; u != nil {
			name, avatar = u.DisplayName, u.AvatarURL
		}
		winRate := 0.0
		if e.Games > 0 {
			winRate = float64(e.Wins) / float64(e.Games)
		}
		rows = append(rows, leaderboardRow{
			UserID: e.Player, DisplayName: name, AvatarURL: avatar,
			Rating: int(e.Rating + 0.5), Wins: e.Wins, Losses: e.Losses,
			Draws: e.Draws, Games: e.Games, WinRate: winRate,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Rating > rows[j].Rating })
	writeJSON(w, http.StatusOK, map[string]any{"gameId": gameID, "entries": rows})
}

/* ---------------------------------- CORS ---------------------------------- */

// withCORS allows the browser client (served from a different origin in dev) to
// call the API with credentials (cookies). With credentials the wildcard origin
// is not allowed, so we echo the request's origin when it is on the allow-list.
func (gw *Gateway) withCORS(next http.Handler) http.Handler {
	allowed := make(map[string]bool, len(gw.allowedOrigins))
	for _, o := range gw.allowedOrigins {
		allowed[strings.TrimRight(o, "/")] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}
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
