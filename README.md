# gateway

The player-facing edge. Owns **accounts + sessions** (Google/GitHub/dev OAuth),
runs a **lobby** for login-required matchmaking, terminates WebSocket connections
for real-time play, routes move intents to the authoritative game-host and fans
out each player's **redacted** state, and serves an **enriched leaderboard**. The
rest of the lobby REST is proxied to the game-host so the browser talks to a
single origin. Holds no game state (that's the game-host); accounts live in
Postgres.

Standalone: its own `go.mod`/`go.sum` + `Dockerfile` (build context = this dir),
so it can live in its own repo and deploy as a usectl App.

## Config (env)

| Var | Default | Purpose |
| --- | --- | --- |
| `GATEWAY_ADDR` | `:8080` | Listen address |
| `GAME_HOST_URL` | `http://localhost:8081` | Authoritative game-host base (internal DNS in prod) |
| `REGISTRY_URL` | `http://localhost:8082` | Internal marketplace registry base — powers the catalog + publish proxy |
| `ADMIN_TOKEN` | _(unset)_ | Shared secret enabling `POST /api/publish`; unset → publishing disabled |
| `DATABASE_URL` | _(unset)_ | Postgres DSN for the `users` table; unset → in-memory accounts |
| `SESSION_SECRET` | _(ephemeral)_ | HMAC key for session cookies (`openssl rand -hex 32`) |
| `PUBLIC_URL` | `http://localhost:8080` | Gateway's public base (builds OAuth `redirect_uri`) |
| `FRONTEND_URL` | `http://localhost:5173` | Where the browser is sent after login |
| `ALLOWED_ORIGINS` | _(FRONTEND_URL)_ | Comma-separated CORS + WebSocket origin allow-list |
| `COOKIE_SECURE` | `false` | `true` → mark session cookies `Secure` (behind HTTPS) |
| `AUTH_DEV_ENABLED` | `true` | Passwordless dev login — **set `false` in production** |
| `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` | _(unset)_ | Enable Google login |
| `GITHUB_CLIENT_ID` / `GITHUB_CLIENT_SECRET` | _(unset)_ | Enable GitHub login |

See `docs/auth-setup.md` for creating the OAuth apps.

## Endpoints

- `GET /health`, `GET /readyz`
- Auth: `GET /auth/providers`, `GET /auth/{provider}/login`, `GET /auth/{provider}/callback`,
  `GET /auth/me`, `POST /auth/logout`
- Lobby (login required): `GET|POST /api/lobby`, `GET /api/lobby/{id}`,
  `POST /api/lobby/{id}/join`, `DELETE /api/lobby/{id}`
- `GET /api/leaderboard?gameId=` — per-game ELO, enriched with display names
- Marketplace:
  - `GET /api/games` — catalog game **ids** (registry ∪ game-host loaded), backward-compatible
  - `GET /api/catalog` — rich Discover catalog: per-game `{id, displayName, min/maxPlayers,
    board, rating, ratingCount, plays, live}` (registry metadata + ratings, game-host
    play counts, and open lobbies per game; every source best-effort)
  - `POST /api/games/{id}/rate` (login required) — `{stars}` 1–5; the gateway injects the
    **trusted user id** from the session and forwards to the registry (one rating per user)
  - `POST /api/publish` — admin-guarded (`X-Admin-Token` = `ADMIN_TOKEN`) proxy to the
    internal registry, so developers publish over HTTPS while the registry stays private
- `GET /ws?matchId=` — WebSocket play; identity comes from the **session cookie**
  (`move`/`move_ok`/`move_err`/`state`/`events`)
- `/api/*` — CORS-enabled REST proxy to the game-host for the remaining reads
  (match summary, view, legal, moves)

## Hardening

- CORS and the WebSocket `CheckOrigin` are pinned to `ALLOWED_ORIGINS` (defaults
  to `FRONTEND_URL`). Set it to your web app's public origin.
- Set `COOKIE_SECURE=true` and `AUTH_DEV_ENABLED=false` in production.
- Put the web client and gateway on the **same registrable domain** so the
  `SameSite=Lax` session cookie is sent (e.g. `app.` + `api.` of one domain).

See `docs/deploy-usectl.md` for deployment.
