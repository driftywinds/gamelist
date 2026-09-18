# Server Use (Headless JSON API)

`gamelist --headless` runs the app as a local HTTP server exposing **every**
CLI action as a JSON API endpoint, so a frontend (web UI, TUI, script) can be
built on the same services the CLI uses.

```powershell
gamelist --headless                 # serve on 127.0.0.1:8080
gamelist --headless --addr 127.0.0.1:9090
gamelist --headless --config D:\path\to\config.yaml   # custom config + DB location
```

Stop with `Ctrl+C` (graceful shutdown, in-flight requests finish).

> **Security:** the API is **unauthenticated** and can read your config
> (including secrets with `?reveal=true`) and write your config file. It is
> meant to be bound to **localhost only** (the default). Do not port-forward,
> proxy, or expose it to a network.

## Conventions

- **Base URL:** `http://127.0.0.1:8080` (configurable via `--addr`).
- **All responses are JSON.** Errors are `{"error": "message"}` with an
  appropriate HTTP status. Successful collection responses are JSON arrays
  (`[]`, never `null`).
- **Store names:** every parameter that names a store accepts the canonical
  key (`steam`, `epic`, `gog`, `ubisoft`, `xbox`, `battlenet`, `dlsite`) or
  the display name ("Steam", "Epic Games Store", "Battle.net"), case-insensitive.
  `GET /api/stores` returns the authoritative list.
- **Game shape:** game payloads are the same `Game` objects the `list`
  command prints: `id` (IGDB ID, `0` when unmatched), `title`, `description`,
  `release_date`, `developers`, `publishers`, `platforms`, `owned_stores`
  (canonical keys, sorted), `store_ids` (map of store key -> store-specific ID).

| HTTP status | Meaning |
|-------------|---------|
| 200 | Success |
| 400 | Bad request (unknown store/config key, malformed body, missing query) |
| 404 | Unknown route / method on route |
| 405 | Method not allowed for that route |
| 409 | Conflict: sync already running, or the store is not enabled |
| 422 | Unprocessable: store needs the interactive CLI sign-in flow, or has no viable API |
| 500 | Local failure (database/config write) |
| 502 | Upstream failure (store API or IGDB call failed) |
| 503 | Feature not configured (e.g. IGDB credentials missing) |

---

## Endpoints

### Status

#### `GET /api/status`

Which stores are enabled and signed in - the `status` command.

```json
{
  "igdb_configured": true,
  "stores": [
    {
      "store": "steam",
      "display_name": "Steam",
      "enabled": true,
      "signed_in": true,
      "credentials": { "has_token": false, "has_refresh_token": false, "expires_at": 0 }
    },
    { "store": "epic", "display_name": "Epic Games Store", "enabled": false, "signed_in": false }
  ]
}
```

`expires_at` is a unix timestamp (0 = not applicable). Token values are never
returned.

#### `GET /api/stores`

The store vocabulary as an array of the same store objects (canonical keys +
display names + enabled/signed-in state). Use this to render store pickers.

---

### Sign in / sign out

#### `POST /api/stores/{store}/signin`

Validates the store's credentials from `configs/config.yaml` and stores the
sign-in record - the `signin <store>` command. The store must already be
enabled and configured (see `PUT /api/config/{key}`).

```powershell
curl -X POST http://127.0.0.1:8080/api/stores/steam/signin
```

```json
{ "store": "steam", "signed_in": true, "note": "credentials validated and stored" }
```

- `409` when the store is not enabled/configured yet (enable it via the
  config endpoint first).
- `422` for `epic` and `gog`: their OAuth flows need a browser + stdin
  (device-code login / paste-the-redirect-URL). Run
  `gamelist config signin epic|gog` in the CLI **once**; the stored session
  refreshes itself on every later sync, including API syncs. `422` is also
  returned by placeholder stores (`xbox`, `dlsite`) that have no viable API.
- `502` when credential validation fails (wrong key/ID, rejected login).

#### `DELETE /api/stores/{store}/session`

Clears stored credentials - the `signout <store>` command.

```json
{ "store": "steam", "signed_in": false, "note": "stored credentials cleared" }
```

---

### Sync

#### `POST /api/sync`

Runs one full sync: fetch owned games from every signed-in (or auto
sign-in-able) store, merge cross-store ownership, enrich via IGDB, persist -
the `sync` command with the interactive mode prompt resolved explicitly.

Request body (all fields optional):

```json
{ "store": "steam", "mode": "incomplete" }
```

| Field  | Values | Meaning |
|--------|--------|---------|
| `store` | any store key/name, or omit | Limit the sync to one store (like `sync steam`) |
| `mode`  | `incomplete` (default) / `refresh` | `incomplete` enriches only games not yet matched (fast); `refresh` re-checks everything against IGDB (like `sync --refresh`) |

```powershell
curl -X POST http://127.0.0.1:8080/api/sync -d "{}"
curl -X POST http://127.0.0.1:8080/api/sync -d "{\"mode\":\"refresh\"}"
curl -X POST http://127.0.0.1:8080/api/sync -d "{\"store\":\"gog\"}"
```

Success response (`200`):

```json
{
  "total_games": 543,
  "games": [ { "id": 620, "title": "Doom", "owned_stores": ["epic", "steam"], "store_ids": { "steam": "35140" } } ],
  "stores": [
    { "store": "epic", "success": true, "games": 214 },
    { "store": "steam", "success": true, "games": 412 }
  ],
  "skipped_games": 411
}
```

- `games` is the full merged list exactly as `GET /api/games` would return
  after the sync.
- `stores` reports **per-store outcomes; failures are never silent** - a
  failed fetch appears with `"success": false` and the `error` message. A
  store that could not even auto-sign-in appears the same way.
- `skipped_games` counts games `incomplete` mode did not re-enrich (already
  matched in earlier syncs).

Other statuses: `409` if a sync is already running (syncs are exclusive;
a second concurrent request waits up to 30s, then answers `409`),
`502` when every store failed (with the per-store errors in the body),
`404`-style semantics for bad bodies are `400`.

> **Long-running:** a full first sync fetches every store and rate-limits
> IGDB at 4 req/s, so it can take minutes for large libraries. Call it in the
> background from your frontend and poll `GET /api/sync/log` for progress,
> or restrict the first sync with `{"store": "..."}` per store.

#### `GET /api/sync/log?limit=50`

Recent per-store sync history (newest first) - the `sync_log` table.

```json
[
  {
    "id": 12,
    "store": "steam",
    "status": "success",
    "message": "412 games",
    "started_at": "2026-09-18 14:03:11",
    "finished_at": "2026-09-18 14:03:19"
  }
]
```

`status` is `pending` (started, not finished), `success`, or `error` (see
`message`). Useful for a "last sync" widget and for noticing failed stores.

---

### Library

#### `GET /api/games`

Every stored game, sorted by title, with cross-store ownership merged - the
`list` command.

Optional filter: `GET /api/games?store=steam` keeps only games owned on that
store (`400` for an unknown store).

#### `GET /api/multi?stores=steam,epic`

Games owned on **more than one store** - the `multi --json` command. The
optional comma-separated `stores` parameter restricts the result to games
owned on **all** of the listed stores (same AND semantics as the CLI:
`multi steam epic` = owned on both Steam and Epic). `400` for unknown stores.

#### `GET /api/search?q=doom`

Direct IGDB title search - the `search` command. Requires IGDB credentials
(`503` otherwise; upstream failures answer `502`). Returns up to 10 `Game`
objects with `id`/`title`/`description`/`release_date` filled.

---

### Configuration

#### `GET /api/config`

Every editable config key with its current value - the `config show`
command, one object per key:

```json
[
  { "key": "igdb.clientId", "kind": "string", "value": "abcd1234", "secret": false },
  { "key": "igdb.clientSecret", "kind": "string", "value": "abcd...wxyz", "secret": true }
]
```

- `secret` values are **masked** (`abcd...wxyz`) unless `?reveal=true`.
- Unset values read as `"(not set)"`.
- `kind` is `bool` or `string` (mirrors what `PUT` accepts).

#### `PUT /api/config/{key}`

Sets one value - the `config set <key> <value>` command. The write preserves
comments and key order in `config.yaml`, and the in-memory config is
reloaded immediately, so subsequent requests use the new value.

Body:

```json
{ "value": "true" }
```

```powershell
curl -X PUT http://127.0.0.1:8080/api/config/stores.steam.enabled -d "{\"value\":\"true\"}"
curl -X PUT http://127.0.0.1:8080/api/config/igdb.clientId -d "{\"value\":\"myclientid\"}"
```

- Boolean keys (`*.enabled`) accept `true/false`, `yes/no`, `1/0`, `on/off`
  and are normalized to `true`/`false`.
- `400` for unknown keys (the error lists all valid keys) or bad boolean
  values.
- `database.dataSourceName` is rejected while the server is running (the DB
  file is already open) - restart with a different `--config` instead.

**Typical frontend setup flow for a key-based store (Steam, Battle.net):**
`PUT` the credentials -> `PUT stores.<store>.enabled=true` ->
`POST /api/stores/<store>/signin` (validates live) -> `POST /api/sync`.
For `epic`/`gog`, direct the user to the CLI once, or ship a config file
with `enabled: true`, then sign in via the CLI and sync via the API.

---

## Endpoint summary

| Method | Path | CLI equivalent | Notes |
|--------|------|----------------|-------|
| GET  | `/api/status` | `status` | IGDB configured? per-store enabled/signed-in |
| GET  | `/api/stores` | (store list) | canonical keys + display names |
| POST | `/api/stores/{store}/signin` | `signin <store>` | validates config credentials; epic/gog -> 422 |
| DELETE | `/api/stores/{store}/session` | `signout <store>` | clears stored credentials |
| POST | `/api/sync` | `sync [store] [--refresh]` | body: `{"store","mode"}`; exclusive; inline result |
| GET  | `/api/sync/log?limit=` | (sync_log table) | per-store history, newest first |
| GET  | `/api/games?store=` | `list` | full library, optional store filter |
| GET  | `/api/multi?stores=a,b` | `multi --json [stores]` | cross-store ownership; AND filter |
| GET  | `/api/search?q=` | `search <title>` | IGDB search; needs IGDB config |
| GET  | `/api/config` | `config show [--reveal]` | editable keys; secrets masked |
| PUT  | `/api/config/{key}` | `config set <key> <value>` | writes config.yaml + reloads |

## What is intentionally not exposed

- **`config signin <store>` interactive flows** (Epic device-code, GOG
  redirect-paste): they require a local browser and stdin. Sign in once per
  machine via the CLI; sessions persist in the local database and refresh
  automatically afterwards.
- **Raw secrets**: only masked values via `GET /api/config` (`reveal=true`
  shows them, because the operator is local and trusted anyway).
- **Database path changes**: rejected while running (see above).

## Frontend notes

- **Progress:** `POST /api/sync` answers only when finished. For progress UI,
  poll `GET /api/sync/log` (each store flips `pending` -> `success`/`error`)
  or render the final `games` array when the POST returns.
- **First run:** a fresh clone has no `configs/config.yaml`; start the server
  once normally (or any API call after `--headless` start) and the default
  config is generated automatically, then use the config endpoints.
- **Concurrency:** reads (`GET /api/*`) are safe during a sync (SQLite WAL);
  syncs themselves are serialized with a 30s wait then `409`.
- **Everything is local:** config file, SQLite database (`gamelist.db` next
  to the config), credentials - same files the CLI uses, so CLI and server
  can be used interchangeably (just not simultaneously for writes).
