# Synaptic Event Schema v1.0

Real-time activity events that adapters emit and the dashboard consumes. Stable, semver'd contract — adapter authors and dashboard authors should both program against this.

## Envelope

Every event is a JSON object with these required top-level fields:

| Field | Type | Required | Notes |
|---|---|---|---|
| `schema_version` | string `"1.0"` | yes | Bump only on breaking changes |
| `type` | enum (see below) | yes | Event kind |
| `timestamp` | ISO 8601 string | yes | UTC; emitter sets at event time |
| `adapter_id` | string | yes | Stable identifier, e.g. `"claude-code-hooks"` |
| `session_id` | string | no | Groups events from the same agent run |
| `payload` | object | no | Event-type-specific |

## Event types

| Type | Triggers when… | Typical payload |
|---|---|---|
| `session_start` | Adapter starts a new session | `{"client": "Claude Code", "model": "claude-opus-4-7"}` |
| `session_end` | Session terminates | `{"reason": "user_exit"}` |
| `prompt_received` | User submits a prompt | `{"text": "...", "char_count": 42}` |
| `model_thinking` | Model begins generating (no token yet) | `{"model": "claude-opus-4-7"}` |
| `response_streaming` | A streaming token batch arrives | `{"delta": "...", "tokens": 12}` |
| `response_complete` | Response finishes | `{"total_tokens": 482, "stop_reason": "end_turn"}` |
| `tool_call` | Agent invokes a tool / MCP | `{"tool_name": "bash", "args": {...}, "tool_call_id": "..."}` |
| `tool_result` | Tool returns | `{"tool_call_id": "...", "ok": true, "result_preview": "..."}` |
| `memory_recall` | An existing memory was touched | `{"memory_id": "...", "score": 0.83, "region_hint": "frontal_lobe"}` |
| `memory_added` | A new memory was committed | `{"memory_id": "...", "text": "...", "tags": [...]}` |
| `memory_updated` | An existing memory changed | `{"memory_id": "...", "fields_changed": ["text"]}` |
| `memory_deleted` | A memory was deleted | `{"memory_id": "..."}` |
| `subagent_spawn` | A child agent starts | `{"subagent_id": "...", "purpose": "..."}` |
| `subagent_complete` | A child agent finishes | `{"subagent_id": "...", "ok": true}` |
| `error` | Anything went wrong | `{"error_message": "...", "where": "..."}` |
| `bubble_added` | SD Core's bubble generator produced a new thought-bubble phrase | `{"text": "...", "flavor": "...", "pool_len": 47}` |

### Server-side broadcast events

These events are emitted by **SD Core** (not by adapters) and reach the dashboard over WebSocket. Adapters never `POST /event` with these types; they appear in the live event stream only. Reserved server-side as of v2.3.0b1:

| Type | Triggers when… | Typical payload |
|---|---|---|
| `embed_backfill_progress` | Phase 1 embedding backfill processes one memory (every memory including cached skips, 250 ms throttle) | `{"processed": 412, "total": 3611, "percent": 11.4, "memory_id": "..."}` |
| `embed_backfill_done` | Phase 1 embedding backfill completes | `{"processed": 3611, "ok": 3589, "errors": 22}` |
| `deep_encode_backfill_progress` | Phase 0b deep-encoding backfill processes one memory | `{"processed": 87, "total": 3611, "percent": 2.4, "memory_id": "..."}` |
| `deep_encode_backfill_done` | Phase 0b deep-encoding backfill completes | `{"processed": 3611, "ok": 3580, "errors": 31}` |
| `phase0b_memory_progress` | A single memory within a Phase 0b nightly run finishes enrichment | `{"run_id": "...", "memory_id": "...", "percent": 47.2}` |

## Region hints

Adapters may attach a `region_hint` to any event payload to nudge the dashboard's region animation. Valid values match keys in `assets/anatomy.json`. Hints are advisory — the dashboard may also infer region from tool name, tag overlap, or text content.

## Transport

Adapters POST events to `http://localhost:9911/event` (one event per request) or push them over WebSocket at `ws://localhost:9911/ws`. The dashboard subscribes to the same WS endpoint.

For remote operation, point at the public/tailnet URL of SD Core (e.g. `https://cognito.example.com/event`, `wss://cognito.example.com/ws`) and pass the Bearer token. HTTP adapters use the `Authorization: Bearer <token>` header; WebSocket subscribers use a `?token=<tok>` query parameter (browsers can't set headers on WebSocket). See [`REMOTE.md`](REMOTE.md) for the full deployment guide.

## Auth

When SD Core is started with `SD_API_TOKEN` set, every request to `/event`, `/events`, `/memories/*`, `/bank/*`, `/admin/*`, and the `/ws` upgrade requires a matching token. `/healthz` and `OPTIONS` preflights are always exempt. The dashboard's static shell is also exempt (edge auth — Cloudflare Access, Tailscale ACL, reverse proxy — typically gates the page). Empty `SD_API_TOKEN` = open mode (suitable for localhost only).

Notable read-only / control paths protected by the same token:

- `GET /memories/coverage` — live embedding + deep-encoding coverage. Deliberately served under `/memories/*` (not `/admin/*`) so a Cloudflare Access policy that blocks `/admin/*` for SSO-only users still lets the dashboard read coverage with the Bearer token.
- `POST /admin/router/reload` — hot-swap reload: re-reads all tier provider configs from the SQLite DB and atomically rebuilds the `ModelRouter`. Useful when scripts bypass the settings API and write directly to `provider_configs`. Returns the resolved per-tier provider names.

## Versioning

This schema is `1.0`. Additive changes (new event types, optional payload fields) ship as `1.x`. Breaking changes (renamed fields, removed types) require `2.0` and a migration period where SD Core accepts both versions.
