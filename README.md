# gateway

The player-facing edge. Terminates WebSocket connections for real-time play,
routes move intents to the authoritative game-host, and fans out each player's
**redacted** state. Lobby REST is proxied to the game-host so the browser talks
to a single origin. Holds no game state.

Standalone: its own `go.mod`/`go.sum` + `Dockerfile` (build context = this dir),
so it can live in its own repo and deploy as a usectl App.

## Config (env)

| Var | Default | Purpose |
| --- | --- | --- |
| `GATEWAY_ADDR` | `:8080` | Listen address |
| `GAME_HOST_URL` | `http://localhost:8081` | Authoritative game-host base (internal DNS in prod) |

## Endpoints

- `GET /health`, `GET /readyz`
- `GET /ws?matchId=&playerId=` — WebSocket play (`move`/`move_ok`/`move_err`/`state`/`events`)
- `/api/*` — CORS-enabled REST proxy to the game-host (lobby: create match, list games)

## Hardening

`CheckOrigin` currently returns `true` (dev). Pin it to your web app's public
origin before going live (`ws.go`).

See `docs/deploy-usectl.md` for deployment.
