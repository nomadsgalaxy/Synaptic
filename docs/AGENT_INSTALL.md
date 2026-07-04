# Agent-Install — runbook for AI agents

> **You are an AI agent. Your operator has asked you to install Synaptic.** This document is written *for you*, not for them. Read it top to bottom, then execute the steps you can execute. Each section is self-contained and idempotent — re-running it should not break anything.
>
> If you are a human reading this, you probably want [`README.md`](./README.md) instead. This file exists for the beta period only and will be removed at the v1.0 public release.

---

## TL;DR

1. **Install to AppData (§2.0)**: from the unzipped source tree, run `python tools/install_to_appdata.py`. The install lives in the per-OS user data location (`%LOCALAPPDATA%\Synaptic` on Windows, `~/Library/Application Support/Synaptic` on macOS, `~/.local/share/synaptic` on Linux). The script prints the resolved path; use it as `${SD_ROOT}` for everything below. Updates later use `--update` from inside a fresh extract; uninstalls use `--uninstall`.
2. Bring the stack up: `cd "${SD_ROOT}" && docker compose up -d` (Docker — §2.1, preferred) or the native Go path (§2.2) if Docker isn't available.
3. Wait until `curl http://localhost:9911/healthz` returns `{"status":"ok"}`.
4. Identify which client *you* are running inside, then register the matching adapter from §3 of this document.
5. Make a single test call (any tool call) and confirm the dashboard's HUD flips from `MODEL: MOCK` to `MODEL: <your-model> · BRIDGE LIVE` at <http://localhost:9911>.

If any step fails, do not loop on it — write a clear failure report to the operator describing exactly which step failed and what error you observed, then stop.

---

## 1. Identify yourself

Before installing anything, figure out which client environment you are running in. The right adapter depends on this. Check the surface you have:

| If you have… | You are likely… | Use adapter |
|---|---|---|
| `mcp__*` tools whose names start with the host application | An MCP-aware client | §3.2 MCP server |
| `/plugin` slash command, hooks, native `Bash`/`Read`/`Write`/`Edit` | Claude Code | §3.1 Claude Code plugin (preferred) **or** §3.2 MCP server |
| Cursor's `Compose`/`Edit` UI, MCP support in Settings | Cursor | §3.2 MCP server |
| VS Code Cline / Continue panel | Cline / Continue | §3.2 MCP server |
| Gemini CLI / Antigravity | Gemini-family | §3.2 MCP server |
| OpenTelemetry tracer in your runtime | Any OTel-instrumented app | §3.3 OTel adapter |
| None of the above | Custom agent runtime | §3.4 SDK |

If you cannot tell, default to **§3.2 MCP server** — it is the broadest fit. If the host application has no extension surface at all, write a failure report to the operator: "I cannot install Synaptic into myself because my host (`<name>`) does not expose an extension surface I can register against."

---

## 2. Install to AppData and bring up the dashboard

The install lives in the per-user OS-standard data directory:

- **Windows:** `%LOCALAPPDATA%\Synaptic`
- **macOS:** `~/Library/Application Support/Synaptic`
- **Linux:** `$XDG_DATA_HOME/synaptic` (or `~/.local/share/synaptic`)

The unzipped source tree (referenced as `${UNZIP}` below — typically `~/Downloads/synaptic/` or `C:\Users\<name>\Downloads\synaptic\`) is just the carrier for the installer; you can delete it after step 2.0 succeeds. The install root (referenced as `${SD_ROOT}` from here on) is what the runtime, updates, and uninstalls operate against.

> Beta migration note: if an earlier install exists at the legacy
> `…/SynapticDisorder` path, the installer prompts before proceeding.
> Follow `docs/dev/Legacy migration paths.md` § 1 to move the tree
> before re-running.

### 2.0. Run the installer (always do this first)

```bash
cd "${UNZIP}"
python tools/install_to_appdata.py
```

The script prints the resolved `${SD_ROOT}` at the end ("Installed Synaptic v… to `<path>`") and a pair of "next steps" lines. Use the printed path as `${SD_ROOT}` for everything below.

If the script reports `error: install already exists at …`, the operator has a prior install. Two paths:

- **`python tools/install_to_appdata.py --update`** — refreshes the install in place. Preserves user data: `assets/memories.json`, `assets/synapses.json`, `assets/thought_bubbles.json`, `env`. This is the right answer 99% of the time.
- **`python tools/install_to_appdata.py --force`** — wipes the install and reinstalls fresh. **Destructive.** Use only if the operator explicitly says "yes, wipe my data."

### 2.0 fallback (only if Python isn't available)

If `python --version` fails, the operator's machine has no Python and the installer can't run. In that case, run the dashboard directly from the unzip directory — no AppData relocation. Tell the operator: "Python isn't available on this machine, so I skipped the AppData install. The install lives at `${UNZIP}` instead, and updates will require unzipping the new beta over it manually." Then continue with §2.1, treating `${SD_ROOT}` as `${UNZIP}` for the rest of this runbook.

### 2.1. Bring up the Docker stack

```bash
cd "${SD_ROOT}"
docker compose up -d
```

Wait up to 60 seconds, then probe:

```bash
curl -fsS http://localhost:9911/healthz
# Expect: {"status":"ok",...}

curl -fsS -o /dev/null -w "%{http_code}\n" http://localhost:9911/
# Expect: 200 (dashboard HTML)
```

If either probe fails, run `docker compose ps` and `docker compose logs --tail=80` and include the output in your failure report.

If you change provider config later (Tier 2 or Tier 3 — Ollama ↔ AirLLM ↔ remote API), no restart needed: Synaptic hot-swaps tiers live as of v2.3.0b1. Only environment-variable changes (e.g. rotating `SD_API_TOKEN` in `.env`) still require `docker compose up -d core`.

### 2.2. Fallback — Fully native (no Docker)

If Docker is unavailable, install everything natively. **This works fully — Docker is convenience, not a hard dependency.** You will need Python 3.7+ for the dashboard server and Go 1.25+ for SD Core. Both are widely available on Windows, macOS, Linux.

```bash
# Dashboard
cd "${SD_ROOT}"
python serve.py &
# Dashboard at http://localhost:8765/index.html

# SD Core — Go ≥ 1.25 required
cd "${SD_ROOT}/bridge/core"
go build -o sd-core .
./sd-core --listen 127.0.0.1:9911 --data-dir ../.. &

# Probe
curl -fsS http://localhost:9911/healthz
# Expect: {"status":"ok",...}
```

Optional: install Ollama natively from <https://ollama.com> if you want LLM-driven thought bubbles and Tier 3 region classification:

```bash
ollama serve &
ollama pull llama3.2:3b
```

If `go` is not available, ask the operator whether they want to install Go (point them at <https://go.dev/dl/>) or fall back to mock-only mode (§2.3).

### 2.3. Last resort — Dashboard only, no SD Core

If neither Docker nor Go is available, you can still bring up the dashboard on the **mock event engine**. This means the brain will visualize simulated activity instead of yours — every other feature (themes, cognitive modes, MRI slice, regions panel, timeline scrubber) still works, but no adapter from §3 will succeed because there is no SD Core to receive events.

```bash
cd "${SD_ROOT}"
python serve.py
```

Tell the operator: "I installed the dashboard in mock-only mode because SD Core requires either Docker or Go, and neither is available on this machine. To see your real session drive the brain, install one of those and re-run me." Then stop. Do not attempt §3 — adapter calls will all fail silently because nothing is listening on `:9911`.

---

## 3. Register an adapter into yourself

Pick exactly one of §3.1–§3.4. Multiple adapters can co-exist if you want, but for a clean install register one and verify it before adding more.

### 3.1. Claude Code plugin (when you are Claude Code)

```text
/plugin marketplace add ${SD_ROOT}
/plugin install synaptic-claude-code@synaptic
```

Run those slash-commands inside your own session. After the second command Claude Code registers the following hooks automatically — no manual `settings.json` edits needed:

| Hook | SD event forwarded | Dashboard region |
|---|---|---|
| `SessionStart` | `session_start` | — |
| `SessionEnd` | `session_end` | — |
| `UserPromptSubmit` | `prompt_received` (text truncated to 200 chars) | `wernicke_area` |
| `PreToolUse` | `tool_call` (tool name + arg key names, **no values**) | per-tool (Bash → motor cortex, Read → visual cortex, …) |
| `PostToolUse` | `tool_result` | `cerebellum` |
| `PostToolUseFailure` | `error` | `amygdala` |
| `SubagentStop` | `subagent_complete` | `prefrontal_cortex` |
| `Stop` | `response_complete` | `broca_area` |

Optionally, set the model label so the HUD shows the real model name instead of the generic `claude-code`:

```bash
export SD_MODEL=claude-opus-4-7   # or whatever model you actually are
```

Skip ahead to §4 to verify.

### 3.2. MCP server (when you are anything that speaks MCP)

The MCP adapter is a Node program. Install its dependencies once:

```bash
cd "${SD_ROOT}/bridge/mcp-adapter"
npm install
```

Then write the adapter into your client's MCP config. The exact file depends on the host:

#### Claude Desktop

| OS | Config path |
|---|---|
| macOS | `~/Library/Application Support/Claude/claude_desktop_config.json` |
| Windows | `%APPDATA%\Claude\claude_desktop_config.json` |
| Linux | `~/.config/Claude/claude_desktop_config.json` |

Merge this into the existing JSON (do not clobber other servers):

```json
{
  "mcpServers": {
    "synaptic": {
      "command": "node",
      "args": ["${SD_ROOT}/bridge/mcp-adapter/index.js"],
      "env": {
        "SD_CLIENT": "claude-desktop",
        "SD_MODEL": "claude-opus-4-7"
      }
    }
  }
}
```

Tell the operator they need to restart Claude Desktop for the config change to take effect — you cannot do that yourself.

#### Cursor

Cursor's MCP config is edited via UI: `Settings → Cursor Settings → MCP → Add new server`. Provide the same JSON shape as above, with `SD_CLIENT: "cursor"`. Tell the operator to reload the Cursor window.

#### Cline (VS Code)

Open the Cline panel → ⚙️ → MCP Servers → Edit JSON. Same shape, `SD_CLIENT: "cline"`.

#### Continue (VS Code / JetBrains)

Edit `~/.continue/config.json` (Windows: `%USERPROFILE%\.continue\config.json`). Continue uses an array shape:

```json
{
  "mcpServers": [
    {
      "name": "synaptic",
      "command": "node",
      "args": ["${SD_ROOT}/bridge/mcp-adapter/index.js"],
      "env": { "SD_CLIENT": "continue" }
    }
  ]
}
```

#### Gemini CLI

Edit `~/.gemini/mcp_servers.json`:

```json
{
  "synaptic": {
    "command": "node",
    "args": ["${SD_ROOT}/bridge/mcp-adapter/index.js"],
    "env": { "SD_CLIENT": "gemini-cli", "SD_MODEL": "gemini-2.5-pro" }
  }
}
```

#### Goose (Block)

Edit `~/.config/goose/config.yaml`:

```yaml
extensions:
  synaptic-disorder:
    type: stdio
    command: node
    args:
      - ${SD_ROOT}/bridge/mcp-adapter/index.js
    env:
      SD_CLIENT: goose
```

#### OpenCode

Edit `~/.opencode/config.json` with the standard `mcpServers` shape, `SD_CLIENT: "opencode"`.

#### Claude Code (alongside or instead of the plugin)

```bash
claude mcp add synaptic-disorder \
  --command node \
  --args "${SD_ROOT}/bridge/mcp-adapter/index.js" \
  --env SD_CLIENT=claude-code SD_MODEL=claude-opus-4-7
```

After registering, the client must be restarted before it picks up the new MCP server. **You cannot restart your host application from inside a session** — tell the operator what to do, then either wait for them to restart or end the install here pending their action.

### 3.3. OTel adapter (when you have OpenTelemetry tracing)

```bash
cd "${SD_ROOT}/bridge/otel-adapter"
npm install
node index.js &
# Listening on http://127.0.0.1:4318/v1/traces
```

Then, in the runtime that owns the traces, set:

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
export OTEL_SERVICE_NAME=<your-service>
```

OpenInference and GenAI semantic-convention instrumentations are auto-recognized.

### 3.4. SDK (when nothing else fits)

If you control the agent code path:

```python
# pip install synaptic
import synaptic as sd
client = sd.connect(adapter_id="my-agent", model="<your-model>")
client.emit("tool_call", {"tool_name": "bash"}, region_hint="motor_cortex")
```

```ts
// npm install @synaptic/sdk
import { connect } from '@synaptic/sdk';
const c = connect({ adapterId: 'my-agent', model: '<your-model>' });
await c.emit('tool_call', { tool_name: 'bash' }, { regionHint: 'motor_cortex' });
```

`session_start` and `session_end` are emitted automatically. Calls are fire-and-forget — they never throw on network errors.

---

## 4. Verify the install

Whichever path you took, verify the same way:

```bash
# 1. SD Core is up
curl -fsS http://localhost:9911/healthz | jq '.status'
# Expect: "ok"

# 2. Your adapter has registered a session
curl -fsS 'http://localhost:9911/events?limit=20' | jq '[.events[] | select(.type == "session_start")] | length'
# Expect: >= 1 (zero means your client did not actually load the adapter — likely needs a restart)

# 3. Real activity is flowing
# Make any tool call inside your session, then:
curl -fsS 'http://localhost:9911/events?limit=10' | jq '[.events[].type] | unique'
# Expect to see at least one of: tool_call, prompt_received, response_complete, model_thinking
```

Then, the canonical visual check: open <http://localhost:9911> in the operator's browser (or <http://localhost:8765> if they took the static-only Option B `python serve.py` fallback) and confirm the bottom-right HUD shows `MODEL: <something> · BRIDGE LIVE` rather than `MODEL: MOCK`. You can ask the operator to load this URL and read back what they see if you cannot drive a browser yourself.

---

## 5. Memory tagging (only if you save memories)

If your install includes `report_memory_save` or you write to `assets/memories.json` directly, **read [`docs/MEMORY_AUTHORING_GUIDE.md`](docs/MEMORY_AUTHORING_GUIDE.md) before saving anything**. The tags you attach decide which brain region the neuron lives in and which other neurons it forms synapses with. Untagged memories become orphan neurons floating disconnected from the rest of the brain.

Quick rule: 3–5 tags per memory drawn from the four buckets (region / topic / project / identity). Examples in the authoring guide.

---

## 6. Reporting back

After the install, send the operator a single status message in this shape:

```
Synaptic install: <DONE | PARTIAL | FAILED>

Dashboard:    http://localhost:9911      (Option A docker / Option C native — SD Core serves it)
              http://localhost:8765      (Option B static-only — python serve.py)
SD Core:      http://localhost:9911      (responding | not responding)
Adapter:      <claude-code-plugin | mcp-adapter | otel-adapter | sdk | none>
Client:       <claude-code | claude-desktop | cursor | cline | continue | gemini-cli | goose | opencode | other>
Restart needed: <yes — operator must restart {host application} | no>

Notes:
  - <one line per surprise / config edit / non-default port / etc.>
```

If `Restart needed: yes`, end your message with: "I'll be able to see events flow on my next session start — restart `<host application>` and run any tool call to verify."

---

## 7. Things you must not do

- **Do not modify `assets/memories.json` in this repo.** It contains the project author's real memory data. If your install path includes seeding example memories, write them to `assets/memories.example.json` or to the SQLite bank via `POST /bank/memory`.
- **Do not commit anything.** This is an install operation, not a source change. If you happen to be running in a worktree with version control, leave staging alone.
- **Do not open ports beyond `127.0.0.1`.** SD Core, the dashboard, Ollama, and the OTel receiver all default to localhost-only. If you change a host binding, get explicit confirmation from the operator first.
- **Do not exfiltrate the contents of any event payload.** This is a localhost-only telemetry dashboard by default; treat everything that flows through it as private. Read [`docs/data_privacy.md`](docs/data_privacy.md) for the full data-handling reference if the operator asks.
- **Do not loop on a failed step.** If `docker compose up -d` returns non-zero, do not retry it three times — capture the error and report.
- **Do not pass `--force` to `install_to_appdata.py` without explicit operator consent.** That flag wipes user data. Use `--update` for in-place upgrades; `--force` is only for "clobber whatever's there because the operator said so."
- **Do not call `--uninstall --purge` without explicit confirmation.** It removes the SQLite memory bank and the cached Ollama models. Soft uninstall (no `--purge`) preserves both Docker volumes and is reversible by re-installing.

---

## 8. Remote operation (when the agent host ≠ the SD Core host)

If you (the agent) are running on a different machine than SD Core — e.g., your operator's laptop is the dashboard host but you're a Claude Desktop install on their phone, or vice versa — there's a separate setup you need from the operator first. **Do not attempt to install SD Core locally if a remote one is intended.**

Ask the operator:

1. *Is SD Core running somewhere reachable from this machine?* If yes, get:
   - The URL (e.g. `https://cognito.your-domain.com` for Cloudflare Tunnel, or `http://100.64.7.42:9911` for Tailscale)
   - The Bearer token (whatever value SD Core was started with)
2. *Should I install a local SD Core too?* If no, skip §2 entirely and go to §3 with the env vars below.

Then in your adapter config (whichever path from §3.1–§3.4 applies), set:

```bash
export SD_CORE_URL=<the URL the operator gave you>
export SD_API_TOKEN=<the bearer token the operator gave you>
```

Both env vars are honored by every adapter (Claude Code plugin, MCP server, OTel adapter, Python SDK, TypeScript SDK). The full deployment guide is [`docs/REMOTE.md`](docs/REMOTE.md) — but if you're reading this as an installer, you don't need to set up the tunnel; the operator already did that.

## 9. Reference for what you just installed

| Component | What it does | Endpoint |
|---|---|---|
| SD Core (Go) | Event hub + WebSocket fan-out + SQLite memory bank | `http://localhost:9911` |
| Dashboard | The 3D brain visualization (served by SD Core when Docker / Native; standalone via `serve.py` for static-only) | `http://localhost:9911` (Docker / Native) or `http://localhost:8765` (Option B static-only) |
| Ollama | Local LLM for region classifier + thought bubbles (optional) | `http://localhost:11434` |
| OTel receiver | OTLP/HTTP collector translating spans into SD events (optional) | `http://localhost:4318` |
| Claude Code plugin | Forwards Claude Code hooks to SD Core | n/a (in-process) |
| MCP adapter | Stdio MCP server with `report_event` + `report_memory_save` tools | n/a (stdio) |

Schemas: [`docs/event-schema.md`](docs/event-schema.md), [`docs/memory-schema.md`](docs/memory-schema.md).

Deep references for each adapter:
- [`bridge/claude-code-plugin/README.md`](bridge/claude-code-plugin/README.md)
- [`bridge/mcp-adapter/README.md`](bridge/mcp-adapter/README.md)
- [`bridge/otel-adapter/README.md`](bridge/otel-adapter/README.md)
- [`bridge/sdk/README.md`](bridge/sdk/README.md)

Full integration matrix for every supported client: [`docs/INTEGRATIONS.md`](docs/INTEGRATIONS.md).

---

*End of runbook. If you got this far without errors, the operator's brain is now visualizing your activity in real time. Welcome to the dashboard.*
