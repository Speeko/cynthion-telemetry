# cynthion-telemetry

Tiny Go HTTP ingest endpoint for Cynthion game telemetry (events + crash reports). SQLite storage, single binary, behind Caddy on `api.cynthiongame.com`.

## Layout

- `main.go` — everything (~300 LOC)
- `Dockerfile` — multi-stage build, runs non-root on Alpine
- `docker-compose.yml` — bound to `127.0.0.1:8090` on host (Caddy proxies via `host.docker.internal:8090`)
- `data/` — SQLite at `./data/events.db` (WAL mode)
- `.env` — `INGEST_API_KEY` (gitignored)

## Endpoints

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/health` | none | liveness probe |
| POST | `/v1/events` | `X-API-Key` | batched gameplay events |
| POST | `/v1/crash` | `X-API-Key` | crash + log upload |
| POST | `/v1/bugreport` | `X-API-Key` | user-submitted bug report (zip upload) |

### POST /v1/events

```json
{
  "install_id": "uuid",
  "session_id": "uuid",
  "app_version": "0.1.1",
  "os": "Windows 10",
  "gpu": "NVIDIA RTX 3080",
  "events": [
    {"client_ts": 1779515374000, "event_type": "session_start", "payload": {}},
    {"client_ts": 1779515380000, "event_type": "new_game", "payload": {"origin":"researcher"}}
  ]
}
```

Response: `{"ok":true,"received":N}`

### POST /v1/crash

```json
{
  "install_id": "uuid",
  "session_id": "uuid",
  "app_version": "0.1.1",
  "os": "Linux",
  "gpu": "NVIDIA RTX 3080 (Vulkan)",
  "error_summary": "SIGSEGV at InitializeOrResetSwapChain",
  "boot_log": "<boot_diagnostics.log contents>",
  "player_log": "<Player.log contents>",
  "payload": {}
}
```

Response: `{"ok":true,"id":<rowid>}`

### POST /v1/bugreport

`multipart/form-data` (NOT JSON — carries a binary zip). Text fields plus one file field:

| Field | Type | Notes |
|---|---|---|
| `install_id` | text | required |
| `session_id` | text | |
| `app_version` | text | |
| `os` | text | |
| `gpu` | text | |
| `category` | text | bug category |
| `severity` | text | |
| `description` | text | what happened |
| `expected_behavior` | text | |
| `archive` | file | the zipped report (`report.json` + `logs/` + `screenshot.png` + `save_snapshot.json`) |

The zip is written to `data/bugreports/<received_ms>_<install8>.zip`; a metadata row goes into the `bugreports` table (`archive_name` points at the file). Client sends this consent-independently — a manual bug report is an explicit user action.

Response: `{"ok":true,"id":<rowid>,"archive":"<filename>.zip"}`

## Limits

- 4 MB max body (`/v1/events`, `/v1/crash`)
- 32 MB max body (`/v1/bugreport` — screenshot + save snapshot)
- 200 events per batch
- 2 req/sec/IP sustained, burst 20 (rate limiter)

## Deploy

```bash
# On the droplet
cd /srv/cynthion-telemetry
docker compose up -d --build
docker logs cynthion-telemetry
```

## Querying the data

```bash
ssh root@cynthion-au 'sqlite3 /srv/cynthion-telemetry/data/events.db "SELECT event_type, COUNT(*) FROM events GROUP BY event_type"'
ssh root@cynthion-au 'sqlite3 /srv/cynthion-telemetry/data/events.db "SELECT received_at, install_id, error_summary FROM crashes ORDER BY received_at DESC LIMIT 10"'
ssh root@cynthion-au 'sqlite3 /srv/cynthion-telemetry/data/events.db "SELECT received_at, install_id, category, severity, description, archive_name FROM bugreports ORDER BY received_at DESC LIMIT 10"'
# Pull a bug-report zip down to inspect locally:
scp root@cynthion-au:/srv/cynthion-telemetry/data/bugreports/<archive_name> /tmp/
```

## Privacy notes

- We collect: `install_id` (random UUID per install), `app_version`, `os`, `gpu`, event names + small JSON payloads, crash logs.
- We DON'T collect: usernames, email, file paths (boot/player logs must be scrubbed client-side to strip `C:\Users\<name>\...` before upload).
- IPs appear in container stdout (request log) and in rate-limit memory. Not stored in the SQLite DB.
- See `cynthiongame.com/privacy` for the user-facing policy.

## GDPR delete (manual for now)

```bash
ssh root@cynthion-au 'sqlite3 /srv/cynthion-telemetry/data/events.db "DELETE FROM events WHERE install_id=?; DELETE FROM crashes WHERE install_id=?" <UUID> <UUID>'
```
