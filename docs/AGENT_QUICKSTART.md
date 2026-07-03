# Synaptic - Per-agent quickstart

A single-page recipe book: pick your agent, copy the block, restart, done. For deeper detail (memory tagging, OTel adapter, custom SDK), see [`INTEGRATIONS.md`](./INTEGRATIONS.md). For the Claude Code plugin specifically, see [`AGENT_INSTALL_PLUGIN.md`](./AGENT_INSTALL_PLUGIN.md).

> **Before you start.** SD Core must be running on `http://localhost:9911`:
>
> ```
> cd /path/to/synaptic-disorder
> docker compose up -d
> ```
>
> Verify with `curl http://localhost:9911/healthz`.

> **Naming note.** Synaptic was previously branded "Synaptic Disorder". The product is now called **Synaptic** ("Syn" for short). The MCP server slug, env vars (`SD_CORE_URL`, `SD_API_TOKEN`), and localStorage keys all keep their existing `sd_*` / `synaptic-disorder-*` prefixes for backward compatibility, so no existing config breaks.

The MCP server lives at `bridge/mcp-adapter/index.js`. Replace `<ABS>` below with the absolute path on your machine, e.g.:

- Windows: `C:\\Users\\you\\AppData\\Local\\SynapticDisorder\\bridge\\mcp-adapter\\index.js` (note doubled backslashes inside JSON strings)
- macOS/Linux: `/home/you/synaptic-disorder/bridge/mcp-adapter/index.js`

---

## Claude Code (highest fidelity - hooks + MCP)

```bash
# 1. Register the marketplace at the repo root
claude plugin marketplace add /abs/path/to/synaptic-disorder

# 2. Install
claude plugin install synaptic-claude-code@synaptic

# 3. Restart Claude Code
```

This installs hooks + 21 MCP tools + 4 slash commands in one shot. Full detail in [`AGENT_INSTALL_PLUGIN.md`](./AGENT_INSTALL_PLUGIN.md).

---

## Claude Desktop

Edit `claude_desktop_config.json`:

- macOS: `~/Library/Application Support/Claude/claude_desktop_config.json`
- Windows: `%APPDATA%\Claude\claude_desktop_config.json`
- Linux: `~/.config/Claude/claude_desktop_config.json`

```json
{
  "mcpServers": {
    "synaptic": {
      "command": "node",
      "args": ["<ABS>/bridge/mcp-adapter/index.js"],
      "env": {
        "SD_CLIENT": "claude-desktop",
        "SD_ADAPTER_ID": "claude-desktop-mcp",
        "SD_CORE_URL": "http://localhost:9911",
        "SD_MODEL": "claude-opus-4-7"
      }
    }
  }
}
```

Restart Claude Desktop. You will see 21 new tools under `mcp__synaptic__*`.

---

## Cursor

Settings -> Cursor Settings -> MCP -> Add new server.

```json
{
  "mcpServers": {
    "synaptic": {
      "command": "node",
      "args": ["<ABS>/bridge/mcp-adapter/index.js"],
      "env": { "SD_CLIENT": "cursor", "SD_MODEL": "gpt-5" }
    }
  }
}
```

Reload window after saving.

---

## Cline (VS Code)

Cline panel -> Settings -> MCP Servers -> Edit JSON.

```json
{
  "mcpServers": {
    "synaptic": {
      "command": "node",
      "args": ["<ABS>/bridge/mcp-adapter/index.js"],
      "env": { "SD_CLIENT": "cline" }
    }
  }
}
```

Reload VS Code window.

---

## Continue (VS Code / JetBrains)

Edit `~/.continue/config.json` (Windows: `%USERPROFILE%\.continue\config.json`).

```json
{
  "mcpServers": [
    {
      "name": "synaptic",
      "command": "node",
      "args": ["<ABS>/bridge/mcp-adapter/index.js"],
      "env": { "SD_CLIENT": "continue" }
    }
  ]
}
```

Reload window after saving.

---

## Gemini CLI

Edit `~/.gemini/mcp_servers.json`:

```json
{
  "synaptic": {
    "command": "node",
    "args": ["<ABS>/bridge/mcp-adapter/index.js"],
    "env": { "SD_CLIENT": "gemini-cli", "SD_MODEL": "gemini-2.5-pro" }
  }
}
```

---

## Gemini Antigravity

Workspace -> Tools -> MCP servers -> Add server. Type: stdio. Command: `node`. Args: `<ABS>/bridge/mcp-adapter/index.js`. Env: `SD_CLIENT=gemini-antigravity`, `SD_MODEL=gemini-2.5-pro`.

---

## OpenCode

Edit `~/.opencode/config.json`:

```json
{
  "mcpServers": {
    "synaptic": {
      "command": "node",
      "args": ["<ABS>/bridge/mcp-adapter/index.js"],
      "env": { "SD_CLIENT": "opencode" }
    }
  }
}
```

---

## Goose (Block)

Edit `~/.config/goose/config.yaml`:

```yaml
extensions:
  synaptic-disorder:
    type: stdio
    command: node
    args:
      - <ABS>/bridge/mcp-adapter/index.js
    env:
      SD_CLIENT: goose
```

---

## Aider / OpenAI / Anthropic / LangChain / LlamaIndex / AutoGen / CrewAI

These don't speak MCP natively but do speak OpenTelemetry. Use the OTel adapter:

```bash
cd <ABS>/bridge/otel-adapter
npm install && node index.js   # listens on :4318

# In your app:
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
export OTEL_SERVICE_NAME=my-app
```

Then add an OpenInference / GenAI instrumentation in your app code; the OTel adapter translates spans into Synaptic events. See `INTEGRATIONS.md` for instrumentation snippets.

---

## Custom (anything else)

If your client speaks neither MCP nor OTel, use the Adapter SDK:

```python
import synaptic as sd
with sd.connect(adapter_id="my-tool", model="claude-opus-4-7") as client:
    client.emit("tool_call", {"tool_name": "bash"}, region_hint="motor_cortex")
```

```ts
import { connect } from '@synaptic/sdk';
const client = connect({ adapterId: 'my-tool', model: 'claude-opus-4-7' });
await client.emit('tool_call', { tool_name: 'bash' }, { regionHint: 'motor_cortex' });
```

Examples in `bridge/sdk/python/examples/` and `bridge/sdk/typescript/examples/`.

---

## Verify the install (every agent)

1. Open the dashboard at `http://localhost:9911`. The HUD's MODEL line should flip to your client name within ~2 seconds of session start.
2. Run a tool call in your agent. Watch the matching brain region pulse.
3. Test MCP tool surface (where applicable): ask the agent to call `mcp__synaptic__sd_recall` with a query string. It should return a top-N memory list.

```bash
# Tail recent events from CLI
curl 'http://localhost:9911/events?limit=10' | jq '.events[].type'
```

---

## Common gotchas

- **No "BRIDGE LIVE" chip** - SD Core not reachable. `docker compose ps` and `curl http://localhost:9911/healthz`.
- **MCP tools missing after edit** - restart the client. Some clients (Continue, Cline) need a workspace reload, not just a process restart.
- **Two MCP servers showing** - if both `plugin:synaptic-claude-code:...` and a standalone `synaptic-disorder` show up in `claude mcp list`, drop the standalone with `claude mcp remove synaptic-disorder`.
- **Token mismatch (401)** - `SD_API_TOKEN` in your client's MCP config must equal the one SD Core was started with. After editing `.env`, run `docker compose up -d core` (NOT `restart`) so it re-reads the file. Note: this only applies to environment-variable changes. Provider/model config changes do **NOT** require a restart since v2.3.0b1 — Tier 2/3 saves hot-swap live.
- **Node missing on PATH** - the MCP server is a Node 20+ script. If your client launches it from a UI process that doesn't inherit your shell PATH, use the absolute path to `node` in the `command` field.
