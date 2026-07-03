# Synaptic Adapter SDKs

Tiny, zero-runtime-dep client libraries for emitting Synaptic activity events from anywhere. Use these when none of the shipped adapters (Claude Code plugin, MCP server, OTel receiver) cover your stack.

| Language | Package | Folder |
|---|---|---|
| Python (≥3.9) | `synaptic` | [`./python`](./python) |
| TypeScript / JavaScript (Node ≥18, browser) | `@synaptic/sdk` | [`./typescript`](./typescript) |

Both SDKs expose the same shape:

| API | Python | TypeScript |
|---|---|---|
| Connect | `client = sd.connect(adapter_id, model=...)` | `const client = connect({ adapterId, model })` |
| Emit | `client.emit(type, payload, region_hint=...)` | `await client.emit(type, payload, { regionHint })` |
| Re-label | `client.register_adapter_id(...)` | `client.registerAdapterId(...)` |
| Close | `client.close()` (or `with sd.connect(...) as c:`) | `await client.close()` |

Both:

- Validate `type` against the v1.0 event schema enum and throw `SchemaError` on unknown types.
- Auto-emit `session_start` on connect (pass `auto_session_start=False` / `autoSessionStart: false` to skip).
- Auto-emit `session_end` on close.
- Are **fire-and-forget** — events are dropped silently if SD Core is offline. Calls never throw on network errors.
- Use a 2.5 s POST timeout (long enough for Cloudflare Tunnel / TLS handshake on a remote SD Core; localhost-only setups won't notice).
- Default to `http://127.0.0.1:9911/event`. Override with `url`/`host`/`port` args or the `SD_CORE_URL` / `SD_CORE_HOST` / `SD_CORE_PORT` env vars. `SD_CORE_URL` (e.g. `https://cognito.example.com`) is the preferred form for remote operation.
- Send an `Authorization: Bearer <token>` header on every request when `api_token`/`apiToken` is passed (or `SD_API_TOKEN` is set in env). Required when SD Core was started with `SD_API_TOKEN`.

## Remote operation

For wiring an SDK-instrumented agent to an SD Core running on another machine, see [`docs/REMOTE.md`](../../docs/REMOTE.md). The two env vars you need are `SD_CORE_URL` and `SD_API_TOKEN`. Cloudflare Tunnel + Bearer token is the recommended deployment; Tailscale is documented as the private-mesh alternative.

## When to reach for these vs. a packaged adapter

- Building a **Claude Code plugin** → use [`bridge/claude-code-plugin`](../claude-code-plugin) — it already wires every hook.
- Wiring **Claude Desktop, Cursor, Cline, Continue, Gemini CLI, etc.** → use [`bridge/mcp-adapter`](../mcp-adapter).
- Already running **OpenTelemetry instrumentation** → use [`bridge/otel-adapter`](../otel-adapter).
- Anything else (custom agent loop, in-house framework, embedded SDK, **a tool plugin emitting from inside its own process**) → use one of these SDKs.

## Cookbook examples

- Python — minimal: [`python/examples/hello_brain.py`](./python/examples/hello_brain.py)
- Python — wrap-a-function decorator: [`python/examples/wrap_function.py`](./python/examples/wrap_function.py)
- TypeScript — minimal: [`typescript/examples/hello-brain.ts`](./typescript/examples/hello-brain.ts)
- TypeScript — wrap-a-function helper: [`typescript/examples/wrap-function.ts`](./typescript/examples/wrap-function.ts)

## Schema

Both SDKs ship the v1.0 enum. Mirrors [`docs/event-schema.md`](../../docs/event-schema.md):

```
session_start         model_thinking         tool_call          memory_recall          subagent_spawn
session_end           response_streaming     tool_result        memory_added           subagent_complete
prompt_received       response_complete                         memory_updated         error
                                                                memory_deleted
```

## License

Pending — Open Community License. See the parent Synaptic repo.
