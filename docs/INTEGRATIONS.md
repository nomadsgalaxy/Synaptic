# Integration guide

How to wire the Synaptic dashboard to every major LLM client. The dashboard has two layers; this guide focuses on the **live activity** layer (events flowing into SD Core on `localhost:9911`). Each section shows: what to install, where the config file lives, exact JSON/YAML to paste, and how to verify it's working.

> **Before you start.** Make sure the dashboard stack is running:
>
> ```bash
> cd /path/to/synaptic-disorder
> docker compose up
> # Dashboard + API:  http://localhost:9911
> # Ollama:           http://localhost:11434
> ```
>
> Single port — SD Core serves the dashboard and the API together. Verify with `curl http://localhost:9911/healthz`; expect JSON with `"status":"ok"`.

> **For agents that save memories** — read [`MEMORY_AUTHORING_GUIDE.md`](./MEMORY_AUTHORING_GUIDE.md) before integrating. It covers how tags route memories to brain regions and form synaptic connections. Untagged memories produce orphan neurons floating disconnected from the rest of the brain.

> **Remote dashboard.** Every adapter on this page also accepts `SD_CORE_URL` (e.g. `https://cognito.example.com`) and `SD_API_TOKEN` env vars for wiring against an SD Core running on a different machine. The local-mode JSON examples below stay valid; just add those two vars to the `env` block. Full deployment guide for Cloudflare Tunnel + Tailscale: [`REMOTE.md`](./REMOTE.md).

---

## Claude Code

The highest-fidelity adapter — uses native Claude Code hooks, no MCP layer.

**Install:** in any Claude Code session,

```
/plugin marketplace add /abs/path/to/synaptic-disorder
/plugin install synaptic-claude-code@synaptic
```

Or, once published to GitHub:

```
/plugin marketplace add nomadsgalaxy/Synaptic-Disorder
/plugin install synaptic-claude-code@synaptic
```

**Verify:** start any Claude Code session in the same shell that started the dashboard. The HUD model line should flip to `MODEL: claude-code · BRIDGE LIVE`. Tool calls light up the motor cortex; prompts hit Wernicke's; responses hit Broca's.

**Configure:** the plugin's hook forwarder reads `SD_MODEL` to label the active model. Claude Code doesn't currently expose the active model name to hooks, so set it manually:

```bash
export SD_MODEL=claude-opus-4-7
```

---

## Claude Desktop

Uses the universal MCP adapter. Edit your `claude_desktop_config.json`:

- **macOS:** `~/Library/Application Support/Claude/claude_desktop_config.json`
- **Windows:** `%APPDATA%\Claude\claude_desktop_config.json`
- **Linux:** `~/.config/Claude/claude_desktop_config.json`

Add an `mcpServers` entry:

```json
{
  "mcpServers": {
    "synaptic": {
      "command": "node",
      "args": ["/abs/path/to/synaptic-disorder/bridge/mcp-adapter/index.js"],
      "env": {
        "SD_CLIENT": "claude-desktop",
        "SD_MODEL": "claude-opus-4-7"
      }
    }
  }
}
```

Restart Claude Desktop. Claude will see two new tools — `report_event` and `report_memory_save` — and pick them up automatically. Add a one-line system-prompt nudge if you want it to call them aggressively (see "How a model uses the tools" in `bridge/mcp-adapter/README.md`).

**Verify:** `curl 'http://localhost:9911/events?adapter_id=mcp-adapter'` should show events from your session.

---

## Cursor

Settings → Cursor Settings → MCP → Add new server:

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

Reload Cursor (Cmd/Ctrl+Shift+P → "Reload Window").

---

## Cline (VS Code)

Open the Cline panel in VS Code → ⚙️ Settings → MCP Servers → Edit JSON. Add:

```json
{
  "mcpServers": {
    "synaptic": {
      "command": "node",
      "args": ["/abs/path/to/synaptic-disorder/bridge/mcp-adapter/index.js"],
      "env": { "SD_CLIENT": "cline" }
    }
  }
}
```

---

## Continue (VS Code / JetBrains)

Edit `~/.continue/config.json` (or `%USERPROFILE%\.continue\config.json` on Windows). Add to `mcpServers`:

```json
{
  "mcpServers": [
    {
      "name": "synaptic",
      "command": "node",
      "args": ["/abs/path/to/synaptic-disorder/bridge/mcp-adapter/index.js"],
      "env": { "SD_CLIENT": "continue" }
    }
  ]
}
```

---

## Gemini CLI

Edit `~/.gemini/mcp_servers.json`:

```json
{
  "synaptic": {
    "command": "node",
    "args": ["/abs/path/to/synaptic-disorder/bridge/mcp-adapter/index.js"],
    "env": { "SD_CLIENT": "gemini-cli", "SD_MODEL": "gemini-2.5-pro" }
  }
}
```

---

## Gemini Antigravity

Antigravity's agent platform consumes MCP servers via its workspace settings. In Antigravity:

1. Open the workspace → **Tools** → **MCP servers** → **Add server**.
2. Set type: `stdio`. Command: `node`. Args: `/abs/path/to/synaptic-disorder/bridge/mcp-adapter/index.js`.
3. Add env: `SD_CLIENT=gemini-antigravity`, `SD_MODEL=gemini-2.5-pro`.

If Antigravity expects a JSON config file, the shape is the same as Gemini CLI's `mcp_servers.json` above.

---

## OpenCode

OpenCode reads MCP servers from `~/.opencode/config.json`:

```json
{
  "mcpServers": {
    "synaptic": {
      "command": "node",
      "args": ["/abs/path/to/synaptic-disorder/bridge/mcp-adapter/index.js"],
      "env": { "SD_CLIENT": "opencode" }
    }
  }
}
```

---

## Aider

Aider doesn't speak MCP natively, but it speaks OpenTelemetry through any of the OpenInference instrumentations. Set the OTLP exporter to point at the OTel adapter:

```bash
cd /path/to/synaptic-disorder/bridge/otel-adapter
npm install && node index.js &      # listens on :4318

# In your aider session:
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
export OTEL_SERVICE_NAME=aider
aider --4o ...
```

---

## Goose (Block)

Goose configures MCP servers in `~/.config/goose/config.yaml`:

```yaml
extensions:
  synaptic-disorder:
    type: stdio
    command: node
    args:
      - /abs/path/to/synaptic-disorder/bridge/mcp-adapter/index.js
    env:
      SD_CLIENT: goose
```

Reload Goose (`/exit` and restart).

---

## OpenAI / Anthropic / LangChain / LlamaIndex / AutoGen / CrewAI

Anything with an OpenInference or GenAI semantic-conventions instrumentation works through the OTel adapter:

```bash
cd /path/to/synaptic-disorder/bridge/otel-adapter
npm install && node index.js
```

Then in your app:

```python
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter

# Pick the right instrumentor for your stack:
from opentelemetry.instrumentation.anthropic import AnthropicInstrumentor
# from opentelemetry.instrumentation.openai_v2 import OpenAIInstrumentor
# from openinference.instrumentation.langchain import LangChainInstrumentor
# from openinference.instrumentation.llama_index import LlamaIndexInstrumentor

trace.set_tracer_provider(TracerProvider())
trace.get_tracer_provider().add_span_processor(
    BatchSpanProcessor(OTLPSpanExporter(endpoint="http://localhost:4318/v1/traces"))
)
AnthropicInstrumentor().instrument()
```

---

## Anything else (custom)

If your tool isn't in this list, the **Adapter SDK** is the universal path. Two-line wire-up:

**Python:**

```python
import synaptic as sd

with sd.connect(adapter_id="my-tool", model="claude-opus-4-7") as client:
    client.emit("tool_call", {"tool_name": "bash"}, region_hint="motor_cortex")
```

**TypeScript / JavaScript:**

```ts
import { connect } from '@synaptic/sdk';
const client = connect({ adapterId: 'my-tool', model: 'claude-opus-4-7' });
await client.emit('tool_call', { tool_name: 'bash' }, { regionHint: 'motor_cortex' });
```

See `bridge/sdk/python/examples/` and `bridge/sdk/typescript/examples/` for cookbook patterns (decorators that wrap any function with `tool_call`/`tool_result` events).

---

## Verification recipes

Regardless of which client you wired:

```bash
# 1. Is SD Core seeing events?
curl 'http://localhost:9911/events?limit=10' | jq '.events[].type'

# 2. Specifically from your adapter?
curl 'http://localhost:9911/events?adapter_id=synaptic-mcp' | jq

# 3. Does the dashboard's HUD show your client?
# Open http://localhost:9911 and watch the MODEL line.
```

The dashboard's bottom-right HUD shows live counts; the `MODEL:` line reflects the active session's model name; specific event types fire region animations:

| Event | Region |
|---|---|
| `prompt_received` | Wernicke's area |
| `response_streaming` / `response_complete` | Broca's area |
| `tool_call` / `tool_result` | Motor cortex / Cerebellum |
| `memory_recall` / `memory_added` | Hippocampus |
| `model_thinking` | Frontal lobe |
| `error` | Amygdala |
| `subagent_spawn` / `subagent_complete` | Prefrontal cortex |

## Troubleshooting

- **"BRIDGE LIVE" never shows up** — SD Core isn't reachable. `docker compose ps` to check, `curl http://localhost:9911/healthz` to ping.
- **Events arrive at SD Core but the dashboard doesn't react** — check the dashboard's WebSocket: open browser devtools → Network → WS, look for `ws://localhost:9911/ws`.
- **MCP client says "tool not available"** — restart the client after editing its config. Some clients (Continue, Cline) require a workspace reload.
- **Multiple clients connected at once** — that's the design. The HUD's `MODELS:` line shows top-N active sessions by event count.
