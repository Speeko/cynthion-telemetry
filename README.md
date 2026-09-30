# Runed Poodle telemetry

Runed Poodle telemetry is the shared ingest for multiple games. This process currently hosts **Cynthion** and **ManRocket** in one SQLite database, behind Caddy on `api.cynthiongame.com`. The public hostname is unchanged. The repo, Docker image, container, and droplet directory are still named `cynthion-telemetry`.

Cynthion stays on the existing unprefixed routes (`POST /v1/events`, `/v1/crash`, `/v1/bugreport`) and tables (`events`, `crashes`, `bugreports`). Renaming those paths is a later migration.

ManRocket is game-scoped on `/v1/manrocket/events`, `/v1/manrocket/crash`, and `/v1/manrocket/bugreport`. It uses `MANROCKET_INGEST_API_KEY` and its own tables (`manrocket_events`, `manrocket_crashes`, `manrocket_bugreports`). Request shapes match Cynthion ingest.

## Layout

- `main.go` — Cynthion ingest, store, routing
- `manrocket_ingest.go` — ManRocket ingest under Runed Poodle (own tables + `MANROCKET_INGEST_API_KEY`)
- `Dockerfile` — multi-stage build, runs non-root on Alpine
- `docker-compose.yml` — bound to `127.0.0.1:8090` on host (Caddy proxies via `host.docker.internal:8090`)
- `data/` — SQLite at `./data/events.db` (WAL mode); bug-report zips in `data/bugreports/` and `data/manrocket_bugreports/`
- `.env` — `INGEST_API_KEY`, `MANROCKET_INGEST_API_KEY`, `MANROCKET_ADMIN_KEY` (gitignored)

## Endpoints

Cynthion uses the unprefixed `/v1/events`, `/v1/crash`, and `/v1/bugreport` paths on `api.cynthiongame.com`. ManRocket uses the `/v1/manrocket/…` routes on that same host.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/health` | none | liveness probe |
| POST | `/v1/events` | `X-API-Key` (`INGEST_API_KEY`) | batched Cynthion gameplay events |
| POST | `/v1/crash` | `X-API-Key` (`INGEST_API_KEY`) | Cynthion crash + log upload |
| POST | `/v1/bugreport` | `X-API-Key` (`INGEST_API_KEY`) | Cynthion bug report (zip upload) |
| POST | `/v1/manrocket/events` | `X-API-Key` (`MANROCKET_INGEST_API_KEY`) | batched ManRocket gameplay events |
| POST | `/v1/manrocket/crash` | `X-API-Key` (`MANROCKET_INGEST_API_KEY`) | ManRocket crash + log upload |
| POST | `/v1/manrocket/bugreport` | `X-API-Key` (`MANROCKET_INGEST_API_KEY`) | ManRocket bug report (zip upload) |

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

## ManRocket ingest (Runed Poodle)

Source: `manrocket_ingest.go`. This is the ManRocket game under Runed Poodle telemetry, on the manrocket-prefixed routes. Same JSON / multipart shapes as the Cynthion routes above, including the optional batch fields `steam_id`, `persona_name`, `country`, and `language`. Auth header is `X-API-Key`, checked against **`MANROCKET_INGEST_API_KEY`**. A missing, wrong, or shorter-than-24-character key returns `401 unauthorized` and writes nothing. The process still boots if the var is unset, so Cynthion ingest keeps working; these three routes stay closed until Homelab sets the key and recreates the container.

Rows go to `manrocket_events`, `manrocket_crashes`, and `manrocket_bugreports`. Zips go to `data/manrocket_bugreports/<received_ms>_<install8>.zip`. Nothing is inserted into `events`, `crashes`, or `bugreports`, and nothing is written under `data/bugreports/`.

Public ManRocket community routes (`/v1/manrocket/levels`, level comments, `/v1/manrocket/notifications`, whiteboards, phrases, names, feedback, and `/v1/leaderboard`) do not take this key.

Clients may emit `manrocket_level_play_start` and `manrocket_level_play_complete` on `POST /v1/manrocket/events` for analytics. Maker popularity ranking does not read those rows. It uses `POST /v1/manrocket/levels/<id>/play` and the denormalized `play_count` (see ManRocket Maker levels).

```bash
curl -sS -X POST http://127.0.0.1:8090/v1/manrocket/events \
  -H 'Content-Type: application/json' \
  -H 'X-API-Key: YOUR_MANROCKET_INGEST_API_KEY' \
  -d '{"install_id":"00000000-0000-4000-8000-000000000000","session_id":"00000000-0000-4000-8000-000000000001","app_version":"0.1.0","os":"Windows 10","gpu":"NVIDIA RTX 3080","steam_id":"","persona_name":"","country":"US","language":"english","events":[{"client_ts":1779515374000,"event_type":"session_start","payload":{}}]}'
```

```bash
curl -sS -X POST http://127.0.0.1:8090/v1/manrocket/crash \
  -H 'Content-Type: application/json' \
  -H 'X-API-Key: YOUR_MANROCKET_INGEST_API_KEY' \
  -d '{"install_id":"00000000-0000-4000-8000-000000000000","session_id":"00000000-0000-4000-8000-000000000001","app_version":"0.1.0","os":"Linux","gpu":"NVIDIA RTX 3080 (Vulkan)","error_summary":"SIGSEGV","boot_log":"","player_log":"","payload":{}}'
```

```bash
curl -sS -X POST http://127.0.0.1:8090/v1/manrocket/bugreport \
  -H 'X-API-Key: YOUR_MANROCKET_INGEST_API_KEY' \
  -F 'install_id=00000000-0000-4000-8000-000000000000' \
  -F 'session_id=00000000-0000-4000-8000-000000000001' \
  -F 'app_version=0.1.0' \
  -F 'os=Windows 10' \
  -F 'gpu=NVIDIA RTX 3080' \
  -F 'category=gameplay' \
  -F 'severity=high' \
  -F 'description=rocket stuck on the pad' \
  -F 'expected_behavior=liftoff' \
  -F 'archive=@report.zip'
```

Responses match Cynthion: events `{"ok":true,"received":N}`, crash `{"ok":true,"id":<rowid>}`, bug report `{"ok":true,"id":<rowid>,"archive":"<filename>.zip"}`. Limits match too (4 MB events/crash, 32 MB bug report, 200 events per batch).

## Leaderboard

Source: `leaderboard.go`. Public (no API key), CORS + rate-limited. Table `leaderboard`.

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/leaderboard` | `{game, mode, level, course, time_ms, name, install_id, app_version}` → `{ok, id}`. `mode`: `full_run` \| `level` (1–99) \| `course` (ManRocket only: `course` = a `manrocket_levels` id, 404 `unknown course` otherwise). `time_ms` 500 ms–12 h. |
| GET | `/v1/leaderboard/<game>?mode=level&level=N[&limit=N]` | `{ok, game, mode, level, course, entries:[{rank, name, tag, time_ms, level, received_at, app_version}]}`, N ≤ 100 (default 20), fastest first. `tag` = 4-hex per-install tag (names aren't unique). |
| GET | `/v1/leaderboard/manrocket?mode=course&course=<id>[&limit=N]` | Same shape; ONE row per install (its best). Unknown course → 404. |

| POST | `/v1/leaderboard` (Launch Laser 2) | `{game:"launchlaser", mode:"score", score, run_ms, kills, name, install_id, app_version}` → `{ok, id}`. Plausibility: score 1–1,000,000, ≤ 8/s of run (+60), ≤ 40/kill, else 400 `bad score`. Source: `launchlaser_scores.go`. |
| GET | `/v1/leaderboard/launchlaser?mode=score&period=all\|week\|day[&limit=N][&install_id=ID]` | `{ok, game, mode, period, entries:[{rank, name, tag, score, received_at, app_version}], you:{rank, score}\|null}`. One row per install (its best), highest first. `day` = since 00:00 UTC, `week` = last 7 days. `tag` is salted `launchlaser-tag:`. |

The `course` and `score` columns were added to the live table by guarded `ALTER TABLE`s (`ensureLeaderboardCourseColumn`, `ensureLeaderboardScoreColumn`).
Delete a row: `sqlite3 data/events.db "DELETE FROM leaderboard WHERE id=?"`.

## ManRocket Maker levels

Source: `manrocket_levels.go`. Public (no API key), CORS + rate-limited — same as votes: `install_id` in the JSON body, not `X-API-Key` / `MANROCKET_INGEST_API_KEY`.

Likes are the existing vote route. There is no separate like resource. CORS `Access-Control-Allow-Methods` includes `PATCH` and `DELETE` so a browser can edit or remove a comment.

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/manrocket/levels` | Upload or replace a Maker level (`id` ≤ 80, plus `title`, `author`, `install_id`, and the level JSON). Replace only when `install_id` matches the row. Votes, loves, hates, play counts, and `starter` are kept. → `{ok, id}` |
| GET | `/v1/manrocket/levels?sort=top\|new\|plays\|love\|hate[&limit=N][&offset=N]` | `{ok, levels:[{id, title, author, votes, loves, hates, play_count, completion_count, starter, received_at, level}]}`. Every sort is `starter DESC`, then the key below. `top` (default; any other `sort` value too): `votes DESC, received_at DESC`. `new`: `received_at DESC`. `plays`: `play_count DESC, votes DESC, received_at DESC`. `love`: `loves DESC, received_at DESC`. `hate`: `hates DESC, received_at DESC`. `limit` 1–100 (default 40). `offset` 0–10000 (default 0); omitted or invalid offset is 0. |
| GET | `/v1/manrocket/levels/<id>` | `{ok, votes, loves, hates, play_count, completion_count, starter, level}` or 404 |
| POST | `/v1/manrocket/levels/<id>/vote` | `{install_id, value: 1\|-1\|2\|-2}` → `{ok, votes}`. `1` like, `-1` dislike, `2` love, `-2` hate. Anything else is 400 `bad value`. One row per install; last write wins. `votes` is the net of `+1`/`-1` only. Love and hate adjust `loves` and `hates` and do not fold into `votes`. A transition **to** `+1` or `2` writes one `vote_on_your_level` row for the level owner, once per voter (a later love after a like does not write another). No row when the owner votes their own level, or on `-1` or `-2`. |
| POST | `/v1/manrocket/levels/<id>/play` | `{phase:"start"\|"complete", install_id, session_id}` → `{ok, play_count, completion_count, counted}`. `start` increments `play_count`. `complete` increments `completion_count`. Idempotent on `(level_id, session_id, phase)`: a retry returns `counted: false` and does not inflate either counter. Unknown level → 404. `install_id` and `session_id` are required, each ≤ 64. |

`play_count` / `completion_count` are added on existing databases by `ensureManRocketLevelPlaySchema` (`ALTER TABLE`, duplicate column ignored). Dedup rows are `manrocket_level_plays`. `sort=plays` reads `play_count` on the level row. It does not aggregate `manrocket_events`.

`loves` / `hates` are added the same way by `ensureManRocketLevelLoveHateSchema`. The first time a column is added, counts are filled from `manrocket_level_votes` (`value = 2` and `value = -2`). Later votes maintain those columns in the vote transaction. `sort=love` and `sort=hate` read the level row. Endless ranking is `sort=love` on this list. `starter` is added by `ensureManRocketLevelStarterSchema` (`INTEGER NOT NULL DEFAULT 0`) and is returned as JSON `true` or `false`. Upload JSON does not set the column. A row stays non-starter until Homelab sets it:

```sql
UPDATE manrocket_levels SET starter=1 WHERE id='learn-to-fly' AND install_id='1790587088-377868843';
```

This process does not insert `learn-to-fly`. Ownership of an uploaded level is the `install_id` on the row. A later `POST /v1/manrocket/levels` with the same `id` and the same `install_id` updates `title`, `author`, `payload`, and `updated_at` only. A different `install_id` does not change the row: the conflict update's `WHERE` fails, and the response is still `{ok: true, id}` (not 403). That is the rule a later upsert of `learn-to-fly` by `1790587088-377868843` follows. It does not clear `starter`, `votes`, `loves`, `hates`, or play counts.

A counted `start` that moves `play_count` across 1, 10, 100, or 500 inserts one `manrocket_level_milestones` row (`level_id`, `threshold`, `reached_at`; primary key on level + threshold) and one `manrocket_level_notifications` row for the level owner (`kind=play_milestone`, `threshold` = that count). A retry, a later play that does not cross a new threshold, or a `complete` does not insert again. Those rows show up in the inbox below. Uniqueness for `kind=play_milestone` is still one row per level + threshold (`idx_mr_notif_play_once`). The old table-level `UNIQUE (level_id, kind, threshold)` is rebuilt into that partial index so comment and vote rows on the same level do not collide with it.

## ManRocket comments and notification inbox

Comments: `manrocket_comments.go`. Inbox: `manrocket_notifications.go`. Same public rules as levels (no API key, `install_id` ownership). Inbox rows are `manrocket_level_notifications` — the play stub table, extended with `actor_install_id`, `comment_id`, and `read_at`. `install_id` on that table is the recipient.

| Method | Path | Purpose |
|---|---|---|
| GET | `/v1/manrocket/levels/<id>/comments` | `{ok, sort, comments:[...]}`. `sort=new` (default): flat, newest first. `sort=thread` (alias `threaded`): top-level comments newest first, each with `replies` oldest-first (nested). `reply_to` or `parent_id` keeps only direct replies of that comment. `limit` ≤ 100 (default 40), `offset` ≤ 10000. Unknown level → 404. Thread view is assembled from the newest 500 live comments on the level. |
| POST | `/v1/manrocket/levels/<id>/comments` | `{install_id, author?, body, reply_to?}` → `{ok, id, comment}`. `body` required, ≤ 500 runes (400 `empty` / `too long`). `author` optional, cleaned like a level author (empty → `anon`). `reply_to` must be a live comment on this level (400 `bad reply_to`). Caps: 40/install + 120/IP-hash per 24 h, including soft-deleted rows (429 `daily comment limit reached`). Writes `comment_on_your_level` to the level owner and, when `reply_to` is set, `reply_to_you` to that comment's `install_id`. No notification to yourself. |
| PATCH | `/v1/manrocket/levels/<id>/comments/<comment_id>` | `{install_id, body?, author?}` → `{ok, comment}`. At least one of `body` / `author`. Only the comment's `install_id` (403 `forbidden`). 404 if missing or already soft-deleted. |
| DELETE | `/v1/manrocket/levels/<id>/comments/<comment_id>` | `install_id` in the JSON body or `?install_id=`. Soft-delete (`deleted_at` set; the row stays, list hides it). Same owner rule. |
| POST | `/v1/manrocket/levels/<id>/comments/<comment_id>/delete` | Same as DELETE, for clients that cannot send a DELETE body. |
| GET | `/v1/manrocket/notifications?install_id=` | `{ok, notifications:[...]}` newest first. `install_id` required. Includes stub `play_milestone` rows already in the table. `limit` ≤ 100 (default 40), `offset` ≤ 10000. `unread=1` (also `true` / `yes`) returns rows with `read_at` null. |
| POST | `/v1/manrocket/notifications/read` | `{install_id, ids:[...]}` or `{install_id, all:true}` → `{ok, updated}`. Only rows whose `install_id` is that recipient are marked, including play-milestone stubs. Already-read rows are not counted again. `ids` ≤ 200. `all:true` ignores `ids`. |

Comment object:

```json
{"id":1,"level_id":"pad","install_id":"uuid","author":"ada","body":"nice pad","reply_to":null,"created_at":1710000000000,"updated_at":1710000000000}
```

`sort=thread` adds `"replies":[ ...same shape... ]`. `reply_to` is the parent comment id, or null.

Notification object (`type` and `kind` are the same string; `kind` is the column):

```json
{"id":1,"type":"play_milestone","kind":"play_milestone","actor_install_id":null,"level_id":"pad","comment_id":null,"milestone":10,"created_at":1710000000000,"read_at":null,"read":false}
```

`kind` is `comment_on_your_level`, `reply_to_you`, `vote_on_your_level`, or `play_milestone`. `comment_id` is set for the comment kinds. `milestone` is the `threshold` column (1, 10, 100, or 500) for `play_milestone`, otherwise null. Play stubs written before this inbox have an empty actor. `read` is true once `read_at` is set.

Client errors are `{"ok":false,"reason":"..."}` with 400, 403, 404, or 429.

Soft-deleted comments stay in `manrocket_level_comments` with `deleted_at` set. Hard-delete: `sqlite3 data/events.db "DELETE FROM manrocket_level_comments WHERE id=?"`.

## ManRocket whiteboards

Public (no API key), CORS + rate-limited. Source: `manrocket_whiteboards.go`. PNGs live as BLOBs in SQLite.

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/manrocket/whiteboards` | `{png_base64, name, install_id}` → `{ok, id}`. PNG ≤ 300 KB decoded, ≤ 1600x800, must decode. Cap: 5/install + 20/IP-hash per 24 h (429). |
| GET | `/v1/manrocket/whiteboards/random?limit=N` | `{ok, boards:[{id, name, png_base64}]}`, N ≤ 10 (default 5), random order |
| GET | `/v1/manrocket/whiteboards/default[?have=V]` | Director's default board: `{ok, version, updated_at, png_base64}`; `version:0` = never set; `have=<current version>` → `{unchanged:true}` without the PNG |
| PUT/POST | `/v1/manrocket/whiteboards/default` | `X-Admin-Key: $MANROCKET_ADMIN_KEY` + `{png_base64}` → `{ok, version}` (bumps version). Env unset/short = read-only. |

Moderation: `UPDATE manrocket_whiteboards SET hidden=1 WHERE id=?` (or `DELETE`).

## ManRocket meeting phrases

Source: `manrocket_phrases.go`. Unlocked in-game by uploading a level. The server renders the line as
Microsoft Sam via `https://www.tetyys.com/SAPI4/SAPI4` (speed 165) at the game's four seat pitches
(70/100/130/165) — **sequentially** (tetyys 503s parallel requests), ~16 s per submit — and stores the wavs as BLOBs.

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/manrocket/phrases` | `{text, name, install_id}` → `{ok, id, text, wavs:{"70":"/v1/…/<id>/70.wav",…}}`. Text ≤ 40 chars, `[A-Za-z0-9 .,!?'-]`, blocklist (whole-word + substring, leet-folded). One per install; resubmit replaces it (new id). Caps: 6/install + 15/IP-hash per 24 h. Errors: `{ok:false, reason}` 400/429/502. |
| GET | `/v1/manrocket/phrases/random?limit=N` | `{ok, phrases:[{id, text, name, wavs}]}`, N ≤ 30 (default 20) |
| GET | `/v1/manrocket/phrases/<id>/<pitch>.wav` | `audio/wav`, immutable (own looser rate bucket: 20/s, burst 160) |

Moderation: `UPDATE manrocket_phrases SET hidden=1 WHERE text LIKE '%…%'`.

## ManRocket pilot names

Source: `manrocket_names.go`. Names are cleaned exactly like leaderboard names (`cleanLeaderboardName`) and
compared case-insensitively. Owner = the install that FIRST posted the name to the manrocket leaderboard
(backfill is implicit — every name already on the board belongs to its poster), else the install holding a
claim. `anon` is shared and never owned. `POST /v1/leaderboard` for manrocket now **409s** a name owned by another install.

| Method | Path | Purpose |
|---|---|---|
| GET | `/v1/manrocket/names/check?name=..&install_id=..` | `{ok, available, reason:"ok"\|"taken", name:<cleaned>}` |
| POST | `/v1/manrocket/names/claim` | `{name, install_id}` → `{ok:true, available:true, name}` or 409 `{ok:false, reason:"taken"}`. Releases the install's previous claim (rename). |

## ManRocket feedback

Source: `manrocket_feedback.go`. The in-game "New Message" window (fake Outlook Express 98) posts here.
**Nothing is emailed** (no SMTP) — rows sit in `manrocket_feedback`; the Director reads them with the admin
GET (game repo: `tools/read_feedback.sh`, key from `~/.config/cynthion-telemetry/manrocket_admin_key`).

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/manrocket/feedback` | `{from, subject, body, cc[], pilot, version, platform, install_id}` → `{ok, id}`. Subject ≤ 120 runes, body ≤ 4000 (400 `too long`), at least one non-empty (400 `empty`). `cc` is the joke address book — stored, never sent (≤ 20 × 64). Caps: 10/install + 30/IP-hash per 24 h (429). |
| GET | `/v1/manrocket/feedback?limit=N` | `X-Admin-Key: $MANROCKET_ADMIN_KEY` → `{ok, feedback:[{id, received_at, from, subject, body, cc, pilot, version, platform, install_id}]}` newest first, N ≤ 200 (default 50). |

Delete: `sqlite3 data/events.db "DELETE FROM manrocket_feedback WHERE id=?"`.

## Limits

- 4 MB max body (`/v1/events`, `/v1/crash`, `/v1/manrocket/events`, `/v1/manrocket/crash`)
- 32 MB max body (`/v1/bugreport`, `/v1/manrocket/bugreport` — screenshot + save snapshot)
- 200 events per batch (both event routes)
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

`.env` on the droplet holds `INGEST_API_KEY`, `MANROCKET_INGEST_API_KEY`, and `MANROCKET_ADMIN_KEY` (never commit it). `data/` is the live DB — never overwrite.

### Homelab: set `MANROCKET_INGEST_API_KEY` after merge

Homelab owns the deploy. Do not build on the droplet. Ship the new image with the commands above first (or set the key, then ship — `docker compose up -d` at the end of that flow starts the new binary with `.env`).

Do not commit the value. Generate a new key; do not copy `INGEST_API_KEY`.

The new image can boot before the var exists: Cynthion ingest keeps using `INGEST_API_KEY`, and `/v1/manrocket/events`, `/v1/manrocket/crash`, and `/v1/manrocket/bugreport` return 401 until the key is present in the running container. Community routes are unaffected. An already-running container will not see an `.env` edit until it is recreated (brief restart, image is not rebuilt, `data/` is not touched).

```bash
ssh root@170.64.160.249 'bash -s' <<'EOF'
set -euo pipefail
cd /srv/cynthion-telemetry
umask 077
# Keep INGEST_API_KEY. Append one new line if it is not already there.
# A missing trailing newline on .env would glue this onto the previous line.
if [ -s .env ] && [ -n "$(tail -c1 .env)" ]; then
  echo >> .env
fi
if ! grep -q '^MANROCKET_INGEST_API_KEY=' .env; then
  echo "MANROCKET_INGEST_API_KEY=$(openssl rand -hex 32)" >> .env
fi
docker compose up -d --force-recreate
docker logs --tail 30 cynthion-telemetry
EOF
```

A healthy boot logs `manrocket ingest auth enabled (MANROCKET_INGEST_API_KEY)`. `MANROCKET_INGEST_API_KEY unset or shorter than 24 characters` means the var did not reach the container. Then smoke-check on the droplet. Replace the placeholder with the value just written to `.env` (do not paste that value into git or chat). An empty `events` array returns 200 and inserts no row:

```bash
ssh root@170.64.160.249 "curl -sS -o /dev/null -w '%{http_code}\n' -X POST http://127.0.0.1:8090/v1/manrocket/events \
  -H 'Content-Type: application/json' \
  -H 'X-API-Key: YOUR_MANROCKET_INGEST_API_KEY' \
  -d '{\"install_id\":\"smoke\",\"events\":[]}'"
```

`200` means the key matched. `401` means the header does not match the env var in the running container.

## Querying the data

```bash
ssh root@170.64.160.249 'sqlite3 /srv/cynthion-telemetry/data/events.db "SELECT event_type, COUNT(*) FROM events GROUP BY event_type"'
ssh root@170.64.160.249 'sqlite3 /srv/cynthion-telemetry/data/events.db "SELECT received_at, install_id, error_summary FROM crashes ORDER BY received_at DESC LIMIT 10"'
ssh root@170.64.160.249 'sqlite3 /srv/cynthion-telemetry/data/events.db "SELECT received_at, install_id, category, severity, description, archive_name FROM bugreports ORDER BY received_at DESC LIMIT 10"'
ssh root@170.64.160.249 'sqlite3 /srv/cynthion-telemetry/data/events.db "SELECT event_type, COUNT(*) FROM manrocket_events GROUP BY event_type"'
ssh root@170.64.160.249 'sqlite3 /srv/cynthion-telemetry/data/events.db "SELECT received_at, install_id, error_summary FROM manrocket_crashes ORDER BY received_at DESC LIMIT 10"'
ssh root@170.64.160.249 'sqlite3 /srv/cynthion-telemetry/data/events.db "SELECT received_at, install_id, category, severity, description, archive_name FROM manrocket_bugreports ORDER BY received_at DESC LIMIT 10"'
# Pull a bug-report zip down to inspect locally:
scp root@170.64.160.249:/srv/cynthion-telemetry/data/bugreports/<archive_name> /tmp/
scp root@170.64.160.249:/srv/cynthion-telemetry/data/manrocket_bugreports/<archive_name> /tmp/
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

ManRocket rows live in their own tables (and zips under `data/manrocket_bugreports/`, named with the first 8 characters of `install_id`):

```bash
ssh root@170.64.160.249 "sqlite3 /srv/cynthion-telemetry/data/events.db \"DELETE FROM manrocket_events WHERE install_id='<UUID>'; DELETE FROM manrocket_crashes WHERE install_id='<UUID>'; DELETE FROM manrocket_bugreports WHERE install_id='<UUID>';\""
```
