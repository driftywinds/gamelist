# Game List Manager

A fully local CLI application (Windows `.exe`, zero CGO, no installer) that signs in to
your game storefronts, pulls every game you own, matches them against
[IGDB](https://igdb.com) for metadata, and shows when one game is owned on
multiple stores. All data lives in a single SQLite file on your machine.

---

## Store support status (honest version)

| Store            | Status      | Notes                                                                                   |
|------------------|-------------|-----------------------------------------------------------------------------------------|
| Steam            | **Working** | Needs a [Web API key](https://steamcommunity.com/dev/apikey) + your SteamID64. Profile's "Game details" privacy must be **public**. |
| Epic Games Store | Partial     | Credentials are validated live; listing a library requires Epic's **end-user** OAuth (device-code) flow — not implemented yet. |
| Battle.net       | Partial     | Credentials validated live; Blizzard exposes per-game account data, **no unified library API**. |
| GOG              | Placeholder | No documented library API; Galaxy's embedded endpoints are unofficial and change without notice. |
| Ubisoft Connect  | Placeholder | No public API; tokens must come from the closed desktop client.                         |
| Xbox             | Placeholder | Library data sits behind Xbox Live XSTS auth; no public user-library API.               |
| DLsite           | Placeholder | No public purchase/library API; would require authenticated scraping.                   |

Placeholder stores fail **loudly** with the reason (and it is logged in `sync_log`),
never silently with an empty list.

---

## Project structure

```
game-list-manager/
├── cmd/
│   └── main.go                    CLI entry point: signin, signout, sync, list, search, status
├── internal/
│   ├── api/
│   │   ├── igdb.go                IGDB client: search, external-ID match, details (rate-limited)
│   │   ├── igdb_auth.go           Twitch OAuth token source (auto-fetch + refresh + caching)
│   │   └── store_apis.go          StoreAPI interface + Steam / Epic / Battle.net / placeholders
│   ├── database/
│   │   └── sqlite.go              modernc.org/sqlite (pure Go), versioned migrations, CRUD
│   ├── models/
│   │   └── game.go                Game model + canonical store keys / display names
│   └── services/
│       ├── game_service.go        Concurrent fetch, cross-store merge, IGDB enrich, persist
│       └── auth_service.go        Sign-in validation + credential persistence
├── pkg/
│   └── api/
│       ├── client.go              Generic HTTP client (persistent headers, JSON)
│       ├── errors.go              APIError type
│       └── client_test.go
├── migrations/
│   └── 001_create_tables.sql      Reference schema (applied automatically at startup)
├── configs/
│   ├── config.go                  Typed YAML config
│   └── config.yaml                Your credentials go here
├── internal/*/..._test.go         Unit + integration tests (no network needed)
├── go.mod / go.sum
└── README.md
```

Key architectural rules:

- **Canonical store keys.** `steam`, `epic`, `gog`, ... are the only identifiers used in
  the database and JSON. Display names ("Steam", "Epic Games Store") are derived via
  `models.DisplayStoreName` — this prevents duplicate `steam`/`Steam` ownership rows.
- **API-first.** Every store implements `api.StoreAPI` (`Key/Name/SignIn/GetOwnedGames`).
  Services are plain Go functions over the database; `list` and `search` already emit
  JSON. A future frontend is an HTTP wrapper around the same services away.
- **IGDB matching prefers exact IDs.** A Steam appid is resolved through IGDB's
  `external_games` endpoint first; fuzzy title search is only the fallback.

---

## Setup

### 1. Prerequisites

- **Go** ≥ 1.22 (only for building from source; no C compiler needed — the SQLite
  driver is pure Go).

### 2. Configure IGDB (you have these already)

IGDB is **free for non-commercial use but not key-less**: it authenticates through
Twitch. You need a **Client ID** and **Client Secret**; the app mints and refreshes the
short-lived OAuth token itself, so you never paste a bearer token.

1. Register an app at <https://dev.twitch.tv/console/apps/create> (already done).
2. Fill in the credentials — either edit `configs/config.yaml` directly:

   ```yaml
   igdb:
     clientId: "your_twitch_client_id"
     clientSecret: "your_twitch_client_secret"
   ```

   or use the built-in config command:

   ```powershell
   gamelist config set igdb.clientId your_twitch_client_id
   gamelist config set igdb.clientSecret your_twitch_client_secret
   ```

3. Test immediately (no store needed):

   ```powershell
   go run .\cmd\main.go search "doom"
   ```

   You should get JSON results from IGDB. That proves the whole auth + query path works.

### 3. Configure a store (Steam is the working one)

The recommended way is the interactive flow — it prompts for values, validates
them with a live API call, and only saves when they actually work:

```powershell
gamelist config signin steam
```

Or edit `configs/config.yaml` (or use `gamelist config set`) and then run
`gamelist signin steam` to validate:

```yaml
stores:
  steam:
    enabled: true
    apiKey: "your_steam_web_api_key"     # https://steamcommunity.com/dev/apikey
    steamId: "76561198012345678"         # your SteamID64
```

- Get your SteamID64 from your profile URL or <https://steamid.io>.
- In Steam privacy settings, set **Game details** to **public**, or Steam will return
  an empty library.

### 4. Build and run

```powershell
go build -o gamelist.exe .\cmd\main.go
.\gamelist.exe status
```

The config file is discovered in this order: `--config <path>` flag → `configs\config.yaml`
next to the exe → `configs\config.yaml` in the working directory. The database file is
always created **next to the config file**, so the app behaves the same from any directory.

### What gets committed (and what never does)

`configs/config.yaml` (your personal API credentials) and the SQLite database are
**gitignored** — a fresh clone auto-generates a default config on first run, and you
fill it with `gamelist config`. Only source, `go.mod`/`go.sum`, migrations and docs are
committed; everything needed to build and run is in the repo.

---

## Usage

```
gamelist [--config <path>] <command> [args]

  signin <store>    Validate and store credentials for a store (reads config.yaml)
  signout <store>   Remove stored credentials
  sync [store]      Fetch owned games (all stores or one), match IGDB, store locally
                    [--refresh | --incomplete] skip the interactive completion prompt
  list              Print all stored games as JSON (sorted by title)
  search <title>    Search IGDB directly (tests your IGDB credentials)
  status            Show which stores are enabled and signed in
  config            Manage configuration without editing config.yaml
    config show [--reveal]     Print current values (secrets masked unless --reveal)
    config set <key> <value>   Set one value, e.g. config set igdb.clientId myid
    config signin <store>      Interactive sign-in: prompts, validates live, saves
    config help                List all editable keys
```

### Configuration without touching config.yaml

`gamelist config` is the user-facing way to set everything up. The interactive
sign-in flow prompts for credentials, **validates them with a live API call**,
and only then saves them to `config.yaml` and the local database:

```powershell
gamelist config signin steam
#   Steam Web API key [current: F974...1A62 - Enter to keep]:
#   SteamID64 [current: 76561... - Enter to keep]:
#   Signed in to Steam: credentials validated and saved to config + database.
```

`config set` edits individual values in place while preserving the comments in
your config file. Secrets are always masked in `config show` output.

### Typical session

```powershell
gamelist config signin steam      # interactive setup + validation
gamelist status                   # steam: enabled / signed in: yes
gamelist sync                     # or: gamelist sync steam
gamelist list                     # JSON with owned_stores per game
```

### Sync modes: resume vs. refresh

IGDB matching of a large library takes a while (rate-limited to 4 requests/second),
so `sync` asks before it starts. If it detects games that were already matched in
earlier syncs, it offers:

```
412 of 543 game(s) are already matched with IGDB from earlier syncs.
  1) Complete only the missing games (fast - recommended)
  2) Refresh all games against IGDB (re-checks everything)
Choose [1/2, default 1]:
```

- **Complete missing** skips every game that already carries an IGDB match — a
  second sync after adding one new game is nearly instant.
- **Refresh all** re-checks everything (use after improving matching or when IGDB
  data changed).

While enrichment runs, each game reports its result as it completes:

```
[  17/ 543] Doom                                            matched IGDB #620
[  18/ 543] Halo 3                                          already matched (IGDB #1020) - skipped
[  19/ 543] Some Obscure Game                               no IGDB match
```

Use `sync --refresh` or `sync --incomplete` to skip the prompt (useful for
scripts and future frontends). Per-store outcomes are recorded in the `sync_log`
table.

---

## How it works

1. **`config signin`** prompts for credentials, runs a live validation call
   (Steam: `GetPlayerSummaries`; Epic/Battle.net: their OAuth token endpoints),
   and saves them to `config.yaml` **and** the `store_credentials` table.
2. **`sync`** fans out `GetOwnedGames()` across all signed-in stores concurrently,
   logging each outcome to `sync_log`.
3. Games are normalized (lowercase, whitespace-collapsed) and **merged across stores**,
   so a game owned on Steam and Epic becomes one entry with
   `owned_stores: ["epic", "steam"]` and both store IDs.
4. **IGDB enrichment** runs in the mode you picked (complete-missing or refresh-all).
   An exact match is attempted via the Steam appid (`external_games`), falling back to
   title search; full details (summary, release date, developers/publishers from
   `involved_companies`, platforms) are then fetched. Requests are throttled to IGDB's
   4 req/s limit, the Twitch token is fetched and refreshed automatically, and each
   game reports its result as it completes.
5. Everything is **upserted into SQLite**: one row per game in `games`, ownership in
   `game_stores` (unique per game+store, so re-syncing never duplicates), and unmatched
   games are matched by title on later syncs instead of piling up duplicates. Empty
   enrichment fields never overwrite data stored by an earlier sync.
6. **`list`** reassembles full `Game` objects from both tables and prints JSON.

---

## Testing in debug mode

### Run without building

```powershell
# NOTE: no "--" needed; everything after the package is passed to the app
go run .\cmd\main.go status
go run .\cmd\main.go search "doom"
go run .\cmd\main.go signin steam
go run .\cmd\main.go sync steam
go run .\cmd\main.go list
```

### Unit + integration tests (no network, no credentials needed)

```powershell
go test ./...
go test -v ./internal/services/...    # cross-store merge, canonical links, dedup
go test -v ./internal/api/...         # IGDB queries, token refresh, header handling
go test -v ./internal/database/...    # migrations, upsert dedup, credential round-trip
```

The IGDB tests run against an in-process stub server, and the Twitch token source is
injected — the suite never touches the network.

### Step debugging with Delve

```powershell
go install github.com/go-delve/delve/cmd/dlv@latest
dlv debug .\cmd\main.go -- status        # args after -- reach the program
```

Inside the prompt: `break internal/services/game_service.go:120` → `continue` → `print games`, etc.
Useful breakpoints: `GameService.enrichViaIGDB`, `IGDBClient.query`, `DB.UpsertGame`.

### Debug build (unoptimized, debugger-friendly symbols)

```powershell
go build -gcflags="all=-N -l" -o gamelist-debug.exe .\cmd\main.go
.\gamelist-debug.exe status
```

### Inspect the database

The DB is a plain SQLite file next to `configs/config.yaml`. Open it with
[DB Browser for SQLite](https://sqlitebrowser.org/) or the `sqlite3` CLI:

```sql
SELECT title, igdb_id, release_date FROM games ORDER BY title;
SELECT g.title, gs.store_name, gs.store_id FROM game_stores gs JOIN games g ON g.id = gs.game_id;
SELECT * FROM sync_log ORDER BY id DESC LIMIT 20;
SELECT store_name, length(access_token), expires_at FROM store_credentials;
```

### Release build

```powershell
go build -trimpath -ldflags "-s -w" -o gamelist.exe .\cmd\main.go
```

No CGO, no runtime DLLs — the exe is fully portable across Windows x64 machines.
(If you build on a machine without Go, the produced exe still needs nothing installed.)

---

## Adding a new store

1. Add a config struct + YAML block in `configs/` if the store needs credentials.
2. Implement `api.StoreAPI` (`Key`, `Name`, `SignIn`, `GetOwnedGames`) in
   `internal/api/store_apis.go`. If the API is not viable yet, wrap `ErrNotImplemented`
   so `sync` reports the reason instead of an empty list.
3. Register it in `buildStoreAPIFromConfig` in `cmd/main.go` and add its key to
   `models.AllStoreKeys()` + the display-name map.
4. If its auth produces tokens, extend `AuthService.persist` so `signin` stores them.

## Known limitations / next steps

- Epic: implement the device-code user flow to actually list a library.
- GOG/Ubisoft/Xbox/DLsite: blocked on viable APIs (see status table); the interface is
  ready the day one exists.
- Cross-store matching is exact-ID first, title-fallback; a manual rematch command is a
  natural next step.

## License

MIT
