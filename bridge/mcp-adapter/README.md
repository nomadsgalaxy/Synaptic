# synaptic-mcp

A universal **Model Context Protocol** server that lets any MCP-aware AI client push live activity into the Synaptic brain dashboard. Compared to the dedicated `synaptic-claude-code` plugin (which uses Claude Code hooks), this adapter trades passive observation for explicit AI-driven reporting: the AI calls a tool when it wants the dashboard to know something happened.

It exposes 21 MCP tools — the two legacy event-push helpers plus 19 first-party `sd_*` tools shared with the Claude Code plugin. The legacy pair:

| Tool | Purpose |
|---|---|
| `report_event(type, payload?, region_hint?, session_id?)` | Emit any v1.0 schema event. Lights up the corresponding region. |
| `report_memory_save(text, tags?, region_hint?, memory_id?, session_id?)` | Convenience wrapper that emits a `memory_added` event when the AI saves to its memory backend. Triggers a fresh neuron + ripple. |

Plus 19 first-party tools under `mcp__synaptic__sd_*`:

- **Memory CRUD + lifecycle** — `sd_create_memory`, `sd_recall`, `sd_list_memories`, `sd_get_memory`, `sd_update_memory`, `sd_delete_memory`, `sd_restore_memory`, `sd_forget`, `sd_supersede`, `sd_set_dormant`, `sd_set_ttl`
- **Research / observability** — `sd_research`, `sd_get_audit`, `sd_get_budget`
- **Dream pipeline** — `sd_dream_run`, `sd_dream_status`, `sd_deep_encode_all`, `sd_embed_all`, `sd_lexicon_rebuild`, `sd_get_dream_settings`, `sd_set_dream_settings`

Full reference: [`docs/AGENT_INSTALL_PLUGIN.md`](../../docs/AGENT_INSTALL_PLUGIN.md).

The server itself emits `session_start` on boot and `session_end` on shutdown, so a connected client always shows up as a live agent in the HUD.

Tested with: Claude Desktop, Cursor, Cline, Continue, Gemini CLI, Claude Code.

## Install

```bash
cd bridge/mcp-adapter
npm install
```

That puts everything under `node_modules/`. The server is invoked as `node bridge/mcp-adapter/index.js`.

## Wire it into your MCP client

### Claude Desktop

Edit `claude_desktop_config.json` (macOS: `~/Library/Application Support/Claude/`, Windows: `%APPDATA%\Claude\`):

```json
{
  "mcpServers": {
    "synaptic": {
      "command": "node",
      "args": ["<INSTALL>\\bridge\\mcp-adapter\\index.js"],
      "env": {
        "SD_CLIENT": "claude-desktop",
        "SD_MODEL": "claude-opus-4-7"
      }
    }
  }
}
```

### Cursor

In Settings → MCP, add:

```json
{
  "mcpServers": {
    "synaptic": {
      "command": "node",
      "args": ["/abs/path/to/synaptic-disorder/bridge/mcp-adapter/index.js"],
      "env": { "SD_CLIENT": "cursor", "SD_MODEL": "gpt-5" }
    }
  }
}
```

### Cline / Continue (VS Code)

Both extensions accept an `mcpServers` block in their workspace or user settings. Use the same `command` + `args` shape as above; set `SD_CLIENT` to `cline` or `continue` so multi-session HUD distinguishes them.

### Claude Code

```bash
claude mcp add synaptic-disorder \
  --command node \
  --args "/abs/path/to/synaptic-disorder/bridge/mcp-adapter/index.js" \
  --env SD_CLIENT=claude-code SD_MODEL=claude-opus-4-7
```

(If you also use the `synaptic-claude-code` plugin, both can run side-by-side — the dashboard will show two parallel sessions per Claude Code launch.)

### Gemini CLI

`~/.gemini/mcp_servers.json`:

```json
{
  "synaptic": {
    "command": "node",
    "args": ["/abs/path/to/synaptic-disorder/bridge/mcp-adapter/index.js"],
    "env": { "SD_CLIENT": "gemini-cli", "SD_MODEL": "gemini-2.5-pro" }
  }
}
```

## Configuration

Edit **`config.json`** (same directory as `index.js`) — it ships with localhost defaults and is the easiest place to configure the adapter:

```json
{
  "SD_CORE_URL": "http://localhost:9911",
  "SD_API_TOKEN": ""
}
```

To point at a remote SD Core, set `SD_CORE_URL` to its full URL and `SD_API_TOKEN` to the bearer token it was started with.

All options (env vars set by the MCP client take precedence over `config.json`):

| Env var | Default | Notes |
|---|---|---|
| `SD_CORE_URL` | `http://localhost:9911` | Full URL of SD Core. Preferred for remote operation; takes precedence over host+port. |
| `SD_CORE_HOST` | `127.0.0.1` | Legacy host for localhost mode. |
| `SD_CORE_PORT` | `9911` | Legacy port. |
| `SD_API_TOKEN` | _(empty)_ | Bearer token sent on every request. Required when SD Core was started with auth on. |
| `SD_ADAPTER_ID` | `mcp-adapter` | Sent as `adapter_id` on every event. |
| `SD_CLIENT` | `mcp-client` | Free-form client label (e.g. `cursor`, `cline`). |
| `SD_MODEL` | `unknown` | Model name shown in the HUD's model line. |

For remote operation (this MCP server runs on one machine, SD Core on another), see [`docs/REMOTE.md`](../../docs/REMOTE.md).

## How a model uses the tools

You don't need to instruct the AI to call these tools manually — most clients pick them up automatically once the server is registered. But if you want to nudge the model, a system-prompt fragment like the following works well:

> You have access to two MCP tools, `report_event` and `report_memory_save`. Whenever you start working on a new request, call `report_event(type="prompt_received", payload={text, char_count})`. Before invoking any tool, call `report_event(type="tool_call", payload={tool_name})`. When you save anything to your memory backend, also call `report_memory_save(text, tags)` with **3–5 tags** drawn from region / topic / project / identity buckets — see `docs/MEMORY_AUTHORING_GUIDE.md` in the Synaptic repo. Don't narrate these calls — just make them.

### Tag your memories well

Synaptic visualizes each memory as a neuron in an anatomically-meaningful brain region, and connects related memories via synaptic pathways. Both depend on tags. **Read [`docs/MEMORY_AUTHORING_GUIDE.md`](../../docs/MEMORY_AUTHORING_GUIDE.md)** before integrating `report_memory_save` into your agent — it has the region map, tagging recipes for common memory types, and a quick-reference card. Untagged or poorly tagged memories produce orphan neurons that float disconnected from the rest of the brain.

## Privacy

This adapter sends event payloads **verbatim** to a localhost-only HTTP endpoint. There is no telemetry and no network egress beyond `127.0.0.1`. The AI controls what ends up in the payloads — if you don't want full prompt text in your event log, instruct the model to truncate before calling `report_event`.

## Verifying it works

```bash
# 1. SD Core up?
curl http://localhost:9911/healthz

# 2. Server starts without crashing?
node bridge/mcp-adapter/index.js < /dev/null      # macOS / Linux
node bridge/mcp-adapter/index.js < nul            # Windows (PowerShell / cmd)
# Expect: "[synaptic-mcp] connected — adapter=… session=… → 127.0.0.1:9911" on stderr

# 3. Did the session_start arrive?
curl 'http://localhost:9911/events?adapter_id=mcp-adapter' | jq '.count'
```

## Troubleshooting

- **Client says "tool not available."** Confirm the server starts standalone (step 2 above). Check the client's MCP log for connection errors. Some clients require a restart after `mcpServers` config changes.
- **Server boots but no events arrive.** Check `SD_CORE_PORT` matches your running SD Core instance. Default is `9911` (not `9999` — that port is reserved for the Hindsight control plane).
- **HUD shows `MODEL: unknown`.** Set the `SD_MODEL` env var per-client in your config above. (Most clients don't expose the active model name to MCP servers.)

## Roadmap

The original B5 / B6 line items (OTel collector for token-streaming visibility, SQLite fallback bank for the universal memory backend) both shipped in earlier waves — `bridge/otel-adapter/` and the SD Core `/bank/*` surface respectively. Current focus is the v2.7 retrieval bundles (P/J/K/L) wired through `sd_recall`, `sd_set_ttl`, `sd_set_dormant`, `sd_supersede`, and `sd_forget`. See `Dev/docs/COMPETITIVE_POSITION.md` for the live roadmap.

## License

Pending — Open Community License. See the parent Synaptic repo.
