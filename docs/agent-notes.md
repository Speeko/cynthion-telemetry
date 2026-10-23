# Agent notes (cynthion-telemetry)

No secrets here. Keys live only in the droplet `.env` (root, 600) and on Brett's desktop.

## Deploy recipe (droplet root@170.64.160.249, /srv/cynthion-telemetry)
1. Merge to `main`, run tests (`docker run --rm -v "$PWD":/src -v gomodcache:/go/pkg/mod -w /src golang:1.22-alpine sh -c 'go vet ./... && go test ./... -skip TestLevelCommentsCRUDInbox'`), push.
2. Back up the DB: `sqlite3 data/events.db ".backup data/events.db.bak-<ts>-<why>"`.
3. Tag the running image for rollback: `docker tag cynthion-telemetry:latest cynthion-telemetry:pre-<ref>`.
4. Rsync code only: `*.go go.mod go.sum Dockerfile docker-compose.yml .env.example`. Never `.env`, never `data/`. The droplet README.md differs; leave it.
5. `docker compose up -d --build`; check logs for `manrocket ingest auth enabled` and `launchlaser1 ingest auth enabled`.
6. Curl: OPTIONS preflights (204), unkeyed POST 401, keyed smoke POST 200, `/health` 200 on api.cynthiongame.com and api.runedpoodle.com.

Rollback: restore code from the previous main commit + `.env.bak-<ts>` (or `docker tag` the pre-image back to latest), `docker compose up -d`.

Caution: the in-place Go build can starve the small droplet (2026-10-23 it hung during a rebuild). Prefer building off-box if that repeats.

## Facts
- `received_at` is unix milliseconds in every table.
- Pre-existing: `TestLevelCommentsCRUDInbox` fails on main (404); gofmt flags manrocket_levels.go.
- Game ingest: `/v1/manrocket/{events,crash,bugreport}` and `/v1/launchlaser1/{events,crash}` share `withIngestCORS` (itch origins), per-IP rate limit, comma-separated key lists, event_type `^[a-z0-9_.:-]{1,64}$`, 8 KB payload per event, daily caps per UTC day (in memory, reset on restart).
- LL1-TEL-2: launchlaser1_* rows older than 24 months are purged at startup and daily.

## Log
- 2026-10-23: deployed 4255883 (ManRocket ingest CORS + Launch Laser 1 ingest). Smoke install_ids: smoke-homelab-cors (ManRocket), smoke-homelab-ll1 (LL1).
- 2026-10-23: merged hardening (3b57a23) and LL1 retention (e65dbad). Deploy interrupted by droplet hang; see Homelab NOTES.md.
