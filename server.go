package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
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
	reg            *RegistryClient
	auth           *Auth
	lobby          *LobbyManager
	ghURL          string
	adminToken     string
	allowedOrigins []string
}

func NewGateway(hub *Hub, gh *GameHostClient, reg *RegistryClient, auth *Auth, lobby *LobbyManager, ghURL, adminToken string, allowedOrigins []string) *Gateway {
	return &Gateway{hub: hub, gh: gh, reg: reg, auth: auth, lobby: lobby, ghURL: ghURL, adminToken: adminToken, allowedOrigins: allowedOrigins}
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
	mux.HandleFunc("POST /api/lobby/{id}/sit", gw.requireAuth(gw.sitLobby))
	mux.HandleFunc("POST /api/lobby/{id}/stand", gw.requireAuth(gw.standLobby))
	mux.HandleFunc("POST /api/lobby/{id}/start", gw.requireAuth(gw.startLobby))
	mux.HandleFunc("DELETE /api/lobby/{id}", gw.requireAuth(gw.cancelLobby))

	// Leaderboard (public read) — enriched with display names.
	mux.HandleFunc("GET /api/leaderboard", gw.handleLeaderboard)

	// Catalog (public read) — union of the registry's published games and the
	// game-host's loaded games, so the browse list reflects the marketplace.
	mux.HandleFunc("GET /api/games", gw.handleGames)
	// Rich catalog (public read) — per-game metadata + real rating/plays/live,
	// consumed by the Discover screen.
	mux.HandleFunc("GET /api/catalog", gw.handleCatalog)
	// Rate a game (login required) — the gateway injects the trusted user id.
	mux.HandleFunc("POST /api/games/{id}/rate", gw.requireAuth(gw.handleRate))
	// The signed-in user's own rating for a game, to pre-fill the rater.
	mux.HandleFunc("GET /api/games/{id}/my-rating", gw.requireAuth(gw.handleMyRating))

	// Publish (admin-guarded) — proxies a game package to the internal registry
	// so developers can publish over HTTPS while the registry stays private.
	mux.HandleFunc("POST /api/publish", gw.handlePublish)

	// Leave an in-progress match (login required) — the leaver's team forfeits so
	// the others aren't stuck, and everyone is freed to start a new game.
	mux.HandleFunc("POST /api/matches/{id}/leave", gw.requireAuth(gw.leaveMatch))

	// The signed-in user's active (unfinished) match, if any — lets the client
	// offer "resume your game" on load and after a reconnect.
	mux.HandleFunc("GET /api/active", gw.requireAuth(gw.handleActive))

	// Everything else under /api/ (match summary, view, legal, moves) is proxied
	// straight through to the game-host.
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
		GameID     string `json:"gameId"`
		Seats      int    `json:"seats"`
		Mode       string `json:"mode"`
		Visibility string `json:"visibility"`
		Password   string `json:"password"`
		Khisht     string `json:"khisht"` // jokeri only: "spec" | "-200" | "-500" | "" (default)
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.GameID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_request", "message": "gameId required"})
		return
	}
	if gw.blockIfInGame(w, r, u) {
		return
	}
	l := gw.lobby.Create(LobbyPlayer{ID: u.Sub, Name: u.Name}, req.GameID, req.Seats, req.Mode, req.Visibility, req.Password, req.Khisht)
	writeJSON(w, http.StatusCreated, l)
}

// blockIfInGame rejects a create/join with 409 when the user is already in an
// unfinished match, returning the active match so the client can offer "resume".
func (gw *Gateway) blockIfInGame(w http.ResponseWriter, r *http.Request, u *sessionClaims) bool {
	mid, gid, active, err := gw.gh.ActiveMatch(r.Context(), u.Sub)
	if err == nil && active {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "active_match", "matchId": mid, "gameId": gid})
		return true
	}
	return false
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
	if gw.blockIfInGame(w, r, u) {
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // body is optional (public tables)
	l, err := gw.lobby.Join(r.Context(), r.PathValue("id"), LobbyPlayer{ID: u.Sub, Name: u.Name}, req.Password)
	gw.writeLobby(w, l, err)
}

// sitLobby seats the player in a specific seat (choosing their partnership in
// teams mode). Blocked if the player is already in a live match.
func (gw *Gateway) sitLobby(w http.ResponseWriter, r *http.Request, u *sessionClaims) {
	if gw.blockIfInGame(w, r, u) {
		return
	}
	var req struct {
		Seat     int    `json:"seat"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_request", "message": "seat required"})
		return
	}
	l, err := gw.lobby.Sit(r.PathValue("id"), LobbyPlayer{ID: u.Sub, Name: u.Name}, req.Seat, req.Password)
	gw.writeLobby(w, l, err)
}

// standLobby vacates the player's seat.
func (gw *Gateway) standLobby(w http.ResponseWriter, r *http.Request, u *sessionClaims) {
	l, err := gw.lobby.Stand(r.PathValue("id"), u.Sub)
	gw.writeLobby(w, l, err)
}

// startLobby begins the match — host only, once every seat is filled.
func (gw *Gateway) startLobby(w http.ResponseWriter, r *http.Request, u *sessionClaims) {
	l, err := gw.lobby.Start(r.Context(), r.PathValue("id"), u.Sub)
	gw.writeLobby(w, l, err)
}

// writeLobby maps the lobby domain errors onto HTTP responses (or returns the
// updated lobby on success).
func (gw *Gateway) writeLobby(w http.ResponseWriter, l *Lobby, err error) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, l)
	case errors.Is(err, ErrLobbyNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not_found"})
	case errors.Is(err, ErrNotHost):
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "not_host"})
	case errors.Is(err, ErrBadSeat):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_seat"})
	case errors.Is(err, ErrSeatTaken):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "seat_taken"})
	case errors.Is(err, ErrNotSeated):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "not_seated"})
	case errors.Is(err, ErrWrongPassword):
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "wrong_password"})
	case errors.Is(err, ErrNotReady):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "not_ready"})
	case errors.Is(err, ErrLobbyFull), errors.Is(err, ErrLobbyStarted):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "lobby_unavailable"})
	default:
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "start_failed", "message": err.Error()})
	}
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

// handleActive reports the signed-in user's current unfinished match (if any),
// so the client can offer to resume it on load / after a reconnect.
func (gw *Gateway) handleActive(w http.ResponseWriter, r *http.Request, u *sessionClaims) {
	mid, gid, active, err := gw.gh.ActiveMatch(r.Context(), u.Sub)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"active": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"active": active, "matchId": mid, "gameId": gid})
}

// leaveMatch force-ends an in-progress match: the leaver's team forfeits (they
// lose, everyone else wins) so no one is left stuck at an unfinishable table.
func (gw *Gateway) leaveMatch(w http.ResponseWriter, r *http.Request, u *sessionClaims) {
	id := r.PathValue("id")
	ctx := r.Context()
	meta, err := gw.gh.GetMatch(ctx, id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not_found"})
		return
	}
	if !contains(meta.Players, u.Sub) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "not_in_match"})
		return
	}
	if meta.Ended {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ended": true})
		return
	}

	losers := gw.leaverTeam(ctx, id, u.Sub, meta.Players)
	lose := make(map[string]bool, len(losers))
	for _, p := range losers {
		lose[p] = true
	}
	winners := make([]string, 0, len(meta.Players))
	for _, p := range meta.Players {
		if !lose[p] {
			winners = append(winners, p)
		}
	}
	name := u.Name
	if name == "" {
		name = "A player"
	}
	result, _ := json.Marshal(map[string]any{
		"winners": winners, "losers": losers, "reason": name + " left the game",
	})
	if err := gw.gh.EndMatch(ctx, id, result, u.Sub); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "end_failed", "message": err.Error()})
		return
	}
	// Push the now-ended state to everyone still in the room (game-over) and let
	// the turn timer cancel itself.
	gw.hub.broadcastState(ctx, id, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ended": true})
}

// leaverTeam returns the players who share the leaver's partnership (so they
// forfeit together). It reads the leaver's redacted view — games that model
// teams (Jokeri) expose a `teams` array; others fall back to just the leaver.
func (gw *Gateway) leaverTeam(ctx context.Context, id, leaver string, players []string) []string {
	view, err := gw.gh.GetView(ctx, id, leaver)
	if err == nil {
		// The game state is nested under "G" in the redacted view (games that
		// model teams — Jokeri — expose G.teams).
		var v struct {
			G struct {
				Teams [][]string `json:"teams"`
			} `json:"G"`
		}
		if json.Unmarshal(view, &v) == nil {
			for _, team := range v.G.Teams {
				if contains(team, leaver) {
					return team
				}
			}
		}
	}
	return []string{leaver}
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

/* --------------------------------- catalog -------------------------------- */

// handleGames returns the browse catalog: the union of games published to the
// registry and games the game-host already has loaded. Either source failing is
// non-fatal — we return whatever we can reach.
func (gw *Gateway) handleGames(w http.ResponseWriter, r *http.Request) {
	seen := map[string]bool{}
	ids := []string{}
	add := func(list []string) {
		for _, id := range list {
			if id != "" && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	if reg, err := gw.reg.Catalog(r.Context()); err == nil {
		add(reg)
	}
	if gh, err := gw.gh.ListGames(r.Context()); err == nil {
		add(gh)
	}
	sort.Strings(ids)
	writeJSON(w, http.StatusOK, map[string]any{"games": ids})
}

// catalogGame is one row of the Discover catalog: registry metadata + rating,
// enriched with real play counts (game-host) and live tables (lobby).
type catalogGame struct {
	ID          string  `json:"id"`
	DisplayName string  `json:"displayName"`
	MinPlayers  int     `json:"minPlayers"`
	MaxPlayers  int     `json:"maxPlayers"`
	Board       string  `json:"board"`
	Rating      float64 `json:"rating"`
	RatingCount int     `json:"ratingCount"`
	Plays       int     `json:"plays"`
	Live        int     `json:"live"`
}

// handleCatalog builds the marketplace catalog from real data: the registry's
// published games + ratings, game-host match counts (plays), and open lobbies
// per game (live). Every source is best-effort so the list degrades gracefully.
func (gw *Gateway) handleCatalog(w http.ResponseWriter, r *http.Request) {
	reg, _ := gw.reg.CatalogFull(r.Context())
	plays, _ := gw.gh.Stats(r.Context())
	live := map[string]int{}
	for _, l := range gw.lobby.ListOpen() {
		live[l.GameID]++
	}
	seen := map[string]bool{}
	out := []catalogGame{}
	for _, g := range reg {
		seen[g.GameID] = true
		out = append(out, catalogGame{
			ID: g.GameID, DisplayName: g.DisplayName, MinPlayers: g.MinPlayers, MaxPlayers: g.MaxPlayers,
			Board: g.Board, Rating: g.Rating, RatingCount: g.RatingCount,
			Plays: plays[g.GameID], Live: live[g.GameID],
		})
	}
	// Games the game-host has loaded but the registry doesn't list (e.g. local
	// dist in dev) still appear.
	if gh, err := gw.gh.ListGames(r.Context()); err == nil {
		for _, id := range gh {
			if !seen[id] {
				seen[id] = true
				out = append(out, catalogGame{ID: id, Plays: plays[id], Live: live[id]})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"games": out})
}

// handleRate forwards a signed-in user's star rating to the registry, injecting
// the trusted user id from the session (the client only sends the star count).
func (gw *Gateway) handleRate(w http.ResponseWriter, r *http.Request, u *sessionClaims) {
	var body struct {
		Stars int `json:"stars"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Stars < 1 || body.Stars > 5 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_request", "message": "stars must be 1-5"})
		return
	}
	status, resp, err := gw.reg.Rate(r.Context(), r.PathValue("id"), u.Sub, body.Stars)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "registry_unavailable", "message": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(resp)
}

// handleMyRating returns the signed-in user's own stars for a game (0 if unrated).
func (gw *Gateway) handleMyRating(w http.ResponseWriter, r *http.Request, u *sessionClaims) {
	stars, err := gw.reg.MyRating(r.Context(), r.PathValue("id"), u.Sub)
	if err != nil {
		stars = 0
	}
	writeJSON(w, http.StatusOK, map[string]any{"stars": stars})
}

// handlePublish proxies a game package to the internal registry. Guarded by an
// admin token (X-Admin-Token header) so the registry stays private while
// developers publish over HTTPS via the gateway. Publishing is disabled until
// ADMIN_TOKEN is set on the gateway.
func (gw *Gateway) handlePublish(w http.ResponseWriter, r *http.Request) {
	if gw.adminToken == "" {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "publish_disabled", "message": "ADMIN_TOKEN not set on gateway"})
		return
	}
	got := r.Header.Get("X-Admin-Token")
	if subtle.ConstantTimeCompare([]byte(got), []byte(gw.adminToken)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_request", "message": "read body"})
		return
	}
	status, respBody, err := gw.reg.Publish(r.Context(), body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "registry_unavailable", "message": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(respBody)
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
