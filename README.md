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

## ManRocket whiteboards

Public (no API key), CORS + rate-limited. Source: `manrocket_whiteboards.go`. PNGs live as BLOBs in SQLite.

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/manrocket/whiteboards` | `{png_base64, name, install_id}` → `{ok, id}`. PNG ≤ 300 KB decoded, ≤ 1600x800, must decode. Cap: 5/install + 20/IP-hash per 24 h (429). |
| GET | `/v1/manrocket/whiteboards/random?limit=N` | `{ok, boards:[{id, name, png_base64}]}`, N ≤ 10 (default 5), random order |
| GET | `/v1/manrocket/whiteboards/default[?have=V]` | Director's default board: `{ok, version, updated_at, png_base64}`; `version:0` = never set; `have=<current version>` → `{unchanged:true}` without the PNG |
| PUT/POST | `/v1/manrocket/whiteboards/default` | `X-Admin-Key: $MANROCKET_ADMIN_KEY` + `{png_base64}` → `{ok, version}` (bumps version). Env unset/short = read-only. |

Moderation: `UPDATE manrocket_whiteboards SET hidden=1 WHERE id=?` (or `DELETE`).

## Limits

- 4 MB max body (`/v1/events`, `/v1/crash`)
- 32 MB max body (`/v1/bugreport` — screenshot + save snapshot)
- 200 events per batch
- 2 req/sec/IP sustained, burst 20 (rate limiter)

## Deploy

The droplet dir `/srv/cynthion-telemetry` is NOT a git checkout — source files are copied over. **Never
build on the droplet** (1 vCPU; the Go compile starves DNS/other services). Build the image locally and ship it:

```bash
cd ~/Documents/GitHub/cynthion-telemetry
docker build -t cynthion-telemetry:latest .
docker save cynthion-telemetry:latest | gzip > /tmp/img.tar.gz
scp /tmp/img.tar.gz root@170.64.160.249:/tmp/
scp *.go go.mod go.sum docker-compose.yml Dockerfile README.md root@170.64.160.249:/srv/cynthion-telemetry/   # keep source in sync
ssh root@170.64.160.249 'docker load < /tmp/img.tar.gz && rm /tmp/img.tar.gz && cd /srv/cynthion-telemetry && docker compose up -d && docker logs --tail 20 cynthion-telemetry'
```

`.env` on the droplet holds `INGEST_API_KEY` and `MANROCKET_ADMIN_KEY` (never commit it). `data/` is the live DB — never overwrite.

## Querying the data

```bash
ssh root@170.64.160.249 'sqlite3 /srv/cynthion-telemetry/data/events.db "SELECT event_type, COUNT(*) FROM events GROUP BY event_type"'
ssh root@170.64.160.249 'sqlite3 /srv/cynthion-telemetry/data/events.db "SELECT received_at, install_id, error_summary FROM crashes ORDER BY received_at DESC LIMIT 10"'
ssh root@170.64.160.249 'sqlite3 /srv/cynthion-telemetry/data/events.db "SELECT received_at, install_id, category, severity, description, archive_name FROM bugreports ORDER BY received_at DESC LIMIT 10"'
# Pull a bug-report zip down to inspect locally:
scp root@170.64.160.249:/srv/cynthion-telemetry/data/bugreports/<archive_name> /tmp/
```

## Privacy notes

- We collect: `install_id` (random UUID per install), `app_version`, `os`, `gpu`, event names + small JSON payloads, crash logs.
- We DON'T collect: usernames, email, file paths (boot/player logs must be scrubbed client-side to strip `C:\Users\<name>\...` before upload).
- IPs appear in container stdout (request log) and in rate-limit memory. Not stored in the SQLite DB raw — ManRocket community tables keep only a truncated salted SHA-256 of the IP (`ip` column) for per-IP daily caps.
- See `cynthiongame.com/privacy` for the user-facing policy.

## GDPR delete (manual for now)

```bash
ssh root@170.64.160.249 'sqlite3 /srv/cynthion-telemetry/data/events.db "DELETE FROM events WHERE install_id=?; DELETE FROM crashes WHERE install_id=?" <UUID> <UUID>'
```
