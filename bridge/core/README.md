# Synaptic Core

The single localhost daemon that backs the dashboard. Serves the HTML, fans events to subscribers, generates thought bubbles, and persists memories. Single static Go binary; runs natively or in Docker.

## What it does

- **Serves the dashboard.** `index.html` plus the whitelisted static set (`/`, `/index.html`, `/favicon.ico`, `/assets/*`, `/bubbles/*`, `/bubbles.json`). Source code, build tools, and personal docs are 404'd even when the project root is mounted as the data dir.
- **Receives events** from adapters via `POST /event` (or batched `POST /events`).
- **Validates** them against the v1.0 schema.
- **Fans them out** to all dashboard subscribers via WebSocket `/ws`.
- **Ring-buffers** the last 1,000 events so a freshly-connected dashboard immediately gets recent context.
- **Watches** `assets/memories.json` and emits `memory_added` / `memory_updated` / `memory_deleted` automatically.
- **Generates thought bubbles** in a goroutine that calls Ollama every ~30 s, persists the pool to `assets/thought_bubbles.json` atomically, and broadcasts `bubble_added` events for live-mode dashboards.
- **SQLite fallback bank** durably stores `memory_added` events with text and accepts direct writes via `POST /bank/memory`.
- **Optional Bearer-token auth** on every API endpoint when `SD_API_TOKEN` is set. `/healthz` and `OPTIONS` preflights are always exempt; the static dashboard shell is exempt by design (edge auth — Cloudflare Access, Tailscale ACL, or a reverse proxy — typically gates the page itself).

## Run via Docker (recommended)

The repo's top-level `docker-compose.yml` is the canonical way to run SD Core — it pairs SD Core with a sibling Ollama container (used by the bubble generator and the build-time region classifier) in a 2-service stack.

```bash
cd /path/to/synaptic-disorder
docker compose up
# Dashboard + API:  http://localhost:9911
# Ollama:           http://localhost:11434  (model pull runs on first launch)
```

For SD-Core-only dev iteration:

```bash
cd bridge/core
docker compose up --build
```

That builds the image, starts the daemon, and bind-mounts the project tree at `/data` so the watcher sees `assets/memories.json` and the bubble generator can write `assets/thought_bubbles.json`.

```bash
# Sanity checks while it's running:
curl http://127.0.0.1:9911/healthz                                   # liveness
curl http://127.0.0.1:9911/                                          # dashboard HTML
curl http://127.0.0.1:9911/events?limit=10                           # recent events (auth required if SD_API_TOKEN set)
curl -X POST http://127.0.0.1:9911/event \
     -H 'Content-Type: application/json' \
     -d '{"schema_version":"1.0","type":"prompt_received",
          "timestamp":"2026-05-06T04:00:00Z","adapter_id":"manual",
          "payload":{"text":"test from curl"}}'
```

If `SD_API_TOKEN` is set, every API request needs `Authorization: Bearer <token>`; the dashboard shell at `/` does not.

## Run natively (Go ≥ 1.25)

```bash
go build -o sd-core .
./sd-core --listen 127.0.0.1:9911 --data-dir ../..
```

Cross-compile for distribution:

```bash
GOOS=linux   GOARCH=amd64  go build -o dist/sd-core-linux-amd64
GOOS=darwin  GOARCH=arm64  go build -o dist/sd-core-darwin-arm64
GOOS=windows GOARCH=amd64  go build -o dist/sd-core-windows-amd64.exe
```

`CGO_ENABLED=0` (the default since we don't import any C code) makes the binary fully static — runs anywhere with no glibc / OS deps.

## Environment variables

| Var | Default | Meaning |
|---|---|---|
| `SD_CORE_LISTEN` | `127.0.0.1:9911` | Listen address. Bind `0.0.0.0:9911` in Docker. |
| `SD_DATA_DIR` | `.` | Path containing `assets/memories.json` and the dashboard's `index.html`. |
| `SD_RING_SIZE` | `1000` | Events kept in memory for replay. |
| `SD_BANK_PATH` | `<data-dir>/sd_bank.db` | SQLite fallback-bank file. Set empty or `SD_BANK_DISABLE=1` to disable. |
| `SD_API_TOKEN` | `""` | Bearer token required on every API request when set. Comma-separate to accept multiple. Empty = open mode (suitable for localhost only). |
| `SD_OLLAMA_URL` | `http://localhost:11434` | Where the bubble generator finds Ollama. In the Docker compose, this is `http://ollama:11434`. |
| `SD_BUBBLE_GEN` | `1` | Set to `0` to disable the bubble generator goroutine entirely. |
| `SD_BUBBLE_CONFIG` | `<data-dir>/config/bubble_prompts.json` | Optional override of the bubble prompt template + flavors. Bake-in defaults are used when missing. |
| `SD_BUBBLE_INTERVAL_SEC` | `30` | Override the generator's tick interval. |
| `SD_SYNAPSE_MODEL` | `llama3.2:3b` | Ollama model used for embeddings (synapse builder + `/recall`). |
| `SD_REALTIME_MODEL` | `llama3.2:3b` | Tier 1 model (real-time encode). Falls back to `SD_CLASSIFY_MODEL`. |
| `SD_NIGHTLY_MODEL` | _(unset)_ | Tier 2 model (nightly consolidation). Unset = degrade to Tier 1. |
| `SD_LOCAL_CONCEPTS` | _(unset)_ | Comma-separated terms NEVER routed to Tier 3 / Oracle. Memories whose tags or text contain a listed concept are also blocked from outbound calls. |
| `SD_DISABLE_ORACLE` | _(unset)_ | `1` = global kill switch — `PrepareOracleCall` returns `ErrOracleDisabled` regardless of Tier 3 wiring. |
| `SD_SENSITIVE_TAGS` | `private,secret,personal,confidential,sensitive,nsfw,password,credential,credentials,api_key,apikey,token,auth_token,access_token,private_key,ssn,medical,health,finance,financial,tax,diary,journal_private` | Comma-separated tag names that auto-promote a memory to `sensitive=true` at write time. |
| `SD_SENSITIVE_AI` | `1` | `0` = disable the Layer 2 AI sensitivity classifier goroutine. Layer 1 regex still runs. |
| `SD_SENSITIVE_AI_INTERVAL` | `30` | Tier 1 classifier tick interval in seconds. |
| `SD_SENSITIVE_AI_BATCH` | `25` | Max records the classifier processes per tick. |
| `SD_SENSITIVE_AI_ALLOW_REMOTE` | _(unset)_ | Set to `1` to allow the AI sensitivity classifier to run when Tier 1 is a remote API. Default behaviour auto-disables it (Layer 1 regex still runs). Only set this for trusted self-hosted endpoints. |
| `SD_TIER1_API_KEY` / `SD_TIER2_API_KEY` / `SD_TIER3_API_KEY` | _(unset)_ | Suggested env var names for API keys when the corresponding tier is configured for a remote provider. The settings table stores only the env var NAME (`api_key_env`); the value is read from the environment at call time. Names are conventions — users can pick any. |

## Endpoints

| Method | Path | Purpose | Auth |
|---|---|---|---|
| GET  | `/healthz` | Liveness + stats (subscriber count, ring usage, bank stats) | exempt |
| GET  | `/` | Dashboard HTML | exempt (edge auth gates the shell) |
| GET  | `/index.html`, `/favicon.ico`, `/assets/*`, `/bubbles/*`, `/bubbles.json` | Dashboard static files | exempt |
| POST | `/event` | Adapter pushes one event | required when `SD_API_TOKEN` set |
| POST | `/events` | Adapter pushes a batch | required |
| GET  | `/events?limit=N&since=&adapter_id=&session_id=` | Most-recent N events | required |
| GET  | `/memories` | Proxies `assets/memories.json` to the dashboard | required |
| GET  | `/bank/memories?limit&since&adapter_id&include_deleted&only_deleted&exclude_sensitive&only_sensitive` | List durable bank records (newest first). Sensitive filters are key for any UI that feeds outbound research. | required |
| GET  | `/bank/memories/{id}` | Single bank record by id | required |
| POST | `/bank/memory` | Direct write into the bank (skips the event path) | required |
| POST | `/recall` | Semantic search: embed query, cosine rank all bank memories, return top-N with scores | required |
| PATCH | `/bank/memories/{id}` | Partial-patch a Trace (text, enriched_text, tags, region_hint, source, merged_from) | required |
| DELETE | `/bank/memories/{id}` | Soft-delete (`?hard=1` to hard-delete). Audit-logged. | required |
| POST | `/bank/memories/{id}/restore` | Clear `deleted_at`, restoring the trace | required |
| POST | `/bank/memories/{id}/lifecycle` | Body `{flag, value}` — set `light_encoded` / `nightly_consolidated` / `oracle_augmented` | required |
| GET / POST | `/maps` | List or create MemoryMaps (`?type=` filter) | required |
| GET / PATCH / DELETE | `/maps/{id}` | Full CRUD on one MemoryMap (delete cascades) | required |
| GET / POST | `/maps/{id}/associations` | List or add typed edges (`?direction=from\|to\|both`) | required |
| DELETE | `/maps/{id}/associations/{to}/{type}` | Remove an edge | required |
| GET / POST | `/maps/{id}/traces` | List or link traces to a map | required |
| DELETE | `/maps/{id}/traces/{trace_id}` | Unlink a trace from a map | required |
| GET / POST / DELETE | `/lexicon` | Tag-co-occurrence rows. POST upserts; DELETE body `{tag_a, tag_b}` | required |
| GET | `/audit` | Audit log (`?entity_type=&entity_id=&operation=&since=&limit=`) | required |
| GET / POST | `/research` | Research-cache list / upsert | required |
| GET / DELETE | `/research/{topic}` | Single research cache entry | required |
| GET / POST | `/budget` | Daily Oracle token spend. POST body `{date?, tokens}` adds; GET `?date=` or `?days=` | required |
| GET | `/settings` | List every (key, value, updated_at) row in the settings table | required |
| GET / PUT / DELETE | `/settings/{key}` | Generic key/value get/upsert/delete. Body for PUT: `{"value": "..."}` | required |
| GET / PUT | `/settings/nightly_schedule` | Typed accessor: returns `NightlySchedule` with safe defaults if absent. PUT auto-sets `configured:true`. | required |
| GET | `/settings/providers` | All four tier configs (`tier_embedding`, `tier1_provider`, `tier2_provider`, `tier3_provider`) plus an `api_key_env_set` map showing which env vars are populated. API keys never returned. | required |
| GET / PUT | `/settings/providers/{tier}` | Single tier (`embedding\|tier1\|tier2\|tier3` or canonical key). Backend validates kind, base_url, model, api_key_env. | required |
| WS   | `/ws` | Dashboard subscribes; receives events as they fire. Token via `?token=<tok>` query param (browsers can't set headers on WebSocket). | required |

All endpoints set permissive CORS so adapters in other processes / Docker networks can talk to it. `Authorization` is in the allowed-headers list so a remote dashboard's preflight passes when it sends Bearer headers.

## Static-file whitelist

`SD_DATA_DIR` typically points at the project root, which contains source code under `tools/`, `bridge/`, and `docs/` — none of which we want web-accessible. The static handler enforces a whitelist:

- Exact paths: `/`, `/index.html`, `/favicon.ico`, `/bubbles.json`
- Prefix paths: `/assets/`, `/bubbles/`
- Path traversal (`..`) and dotfile requests are rejected outright.

To extend, edit `staticDashboard()` in `main.go` and add to `allowedExact` / `allowedPrefixes`. Don't loosen the prefix check.

## Bubble generator

Inline goroutine in `bubble_generator.go`. On each tick:

1. Sample `MemorySampleN` random memories from `assets/memories.json` (when readable).
2. Pick a random flavor from `cfg.Flavors`.
3. Render the `generate` template (`text/template`) with `{Memories, Flavor, RecentEvents}` and POST to Ollama.
4. Optionally run a similarity check (second Ollama call) against a sample of the existing pool.
5. Append novel phrases to the pool, persist atomically to `assets/thought_bubbles.json`, broadcast a `bubble_added` event.

The prompt template + flavor list live in `<data-dir>/config/bubble_prompts.json` (baked-in defaults if missing). Editing prompts doesn't require a rebuild.

## Event schema

See `docs/event.schema.json` and `docs/event-schema.md` at the project root. The `bubble_added` type is the newest addition; all v1.0 types are accepted.

## Remote operation

For wiring a dashboard host to adapters running on other machines (Cloudflare Tunnel + Bearer token, or Tailscale private mesh), see [`docs/REMOTE.md`](../../docs/REMOTE.md). SD Core's auth and the dashboard's runtime URL+token Config panel are designed for that flow.

## License

TBD pending OCL.
