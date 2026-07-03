# Synaptic

An agent-driven indexed memory store with a 3D MRI/CAT-scan-style dashboard. Memories are neurons in anatomical brain regions, shared tags become synapses, and live agent activity fires the regions in real time. Your agent queries the store; Synaptic remembers, organizes, and surfaces what's relevant.

> If you want the short read on what Synaptic is and isn't, start with [`docs/WHAT_SYNAPTIC_IS.md`](docs/WHAT_SYNAPTIC_IS.md).

_By [NomadsGalaxy](https://github.com/nomadsgalaxy)_

## Introduction

Hello, I'm Nomad, and welcome to Synaptic. For a long time, I've been fascinated by how the brain works, how we think, store memories, conciousness as a whole. As I started to use AI more as a tool in my workflows, I wanted to better understand and visualize how it remembers things, I wanted to see what it would look like if I put my agent's brains in an MRI machine, and watch as the synapses fired, like lightning bolts in clouds.

So, I decided to try my hand at making it, admittedly heavily using Claude to generate it, and after several iterations, I want to publish it. What started as a visualizer ended up growing a full memory backend underneath — so it now does both: an indexed memory store your agent can write to and recall from, and the 3D brain dashboard that shows you what your agent is thinking. I hope you enjoy it as much as I do.

The best experience is to have the visualizer on a secondary monitor while using the plugin method like in Claude CLI, there are some configurations, so have fun with it.

## Requirements

The dashboard itself is dependency-free in the browser. Everything else is layered, so you only install the pieces you actually use. **Docker is the convenience path, not a hard dependency** — every component has a native install option, and the visualization runs end-to-end on the built-in mock engine with nothing more than Python + a browser.

| Component                                                                                     | Native requirement                                                                                                                        | Docker alternative                | Required?                                                                                     |
| --------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------- | --------------------------------------------------------------------------------------------- |
| **SD Core + dashboard** (single Go binary; SD Core serves `index.html` + assets at port 9911) | Go ≥ 1.25 (`go build` produces a static binary, no runtime deps) and a modern browser (Chrome / Edge / Firefox / Safari, recent versions) | `synaptic-core` container         | **Required** unless you only want the mock dashboard at :8765 (see below)                     |
| **Mock-only dashboard** (`python serve.py`, no SD Core)                                       | Python 3.7+ and a modern browser                                                                                                          | — (skip Docker entirely)          | Optional, lower-fidelity. Brain runs on simulated activity; can't accept real adapter events. |
| **Ollama** (region classifier Tier 3 + thought bubbles)                                       | Native install from <https://ollama.com>, then `ollama pull llama3.2:3b`                                                                  | `synaptic-ollama-tier1` container | Optional — falls back to keyword-only classification + pre-baked bubble pool                  |
| **MCP adapter**                                                                               | Node.js ≥ 18 (run `npm install` in `bridge/mcp-adapter/` once)                                                                            | — (always native)                 | Only if you wire an MCP-aware client                                                          |
| **OTel adapter**                                                                              | Node.js ≥ 18 (`bridge/otel-adapter/`)                                                                                                     | — (always native)                 | Only if your runtime exports OpenTelemetry traces                                             |
| **Claude Code plugin**                                                                        | Nothing — Claude Code manages it via `/plugin install`                                                                                    | — (always in-process)             | Only if you use Claude Code                                                                   |
| **Python SDK**                                                                                | Python ≥ 3.9 (no third-party deps)                                                                                                        | —                                 | Only for custom integrations                                                                  |
| **TypeScript SDK**                                                                            | Node.js ≥ 18 / browser                                                                                                                    | —                                 | Only for custom integrations                                                                  |

### Can I run this without Docker?

**Yes — fully.** The minimal "no Docker, no anything" install is:

1. Go 1.25+ → `cd bridge/core && go build -o sd-core . && ./sd-core --listen 127.0.0.1:9911 --data-dir ../..` — that single binary serves the dashboard at `/`, the API at `/event`/`/events`/`/bank/*`, and the WebSocket at `/ws`. Open `http://localhost:9911` in your browser.
2. (Optional) Native Ollama for nicer classification + bubbles. Skip this and the dashboard still runs.
3. (Optional) `npm install` in `bridge/mcp-adapter/` if you want to wire an MCP client; the adapter itself is always run natively as a stdio child of your client.

If you don't want to install Go, you can still run **just** the dashboard on the mock event engine via `python serve.py` — see Option B in Quick Start. Brain runs on simulated activity; real adapter events can't reach it without SD Core.

Disk footprint, native install: SD Core binary ≈ 15 MB; repo + dashboard ≈ 30 MB; Ollama runtime ≈ 1 GB plus model weights (`llama3.2:3b` ≈ 2 GB, smaller models down to ≈ 400 MB). Docker stack equivalent is ≈ 4 GB total once images and volumes are populated.

## Quick start

Three steps from a clean machine: bring up the dashboard, plug in your AI client, verify the bridge is live. Total wall-clock time is about five minutes if Docker is already installed.

> **Hot-swap, no restart.** As of v2.3.0b1, changing your Tier 2 / Tier 3 provider (Ollama ↔ AirLLM ↔ remote API) from the Config panel takes effect live — `PUT /settings/providers/{tier}` rebuilds the tier from the freshly-saved config and atomically swaps it in, so in-flight `Chat()` calls finish on the old provider and the next lookup sees the new one. The Save response carries `hot_swapped: true` + `active_provider: "..."` so the UI confirms which model is wired in. Only `.env` changes (e.g. rotating `SD_API_TOKEN`) still require `docker compose up -d core` to re-read the file.

### Step 1 — Bring up the dashboard

The canonical install path uses the bundled installer to put the runtime in the per-user OS-standard data directory (`%LOCALAPPDATA%\Synaptic` on Windows, `~/Library/Application Support/Synaptic` on macOS, `~/.local/share/synaptic` on Linux). Updates and uninstalls run from the same script, so the source you downloaded becomes throwaway carrier and the install lives in a stable location.

```bash
# From the unzipped source tree (or this repo's Dev/Prod tree):
python tools/install_to_appdata.py
# Prints the resolved install path. cd into it for the next step.
cd "<install path>"
docker compose up
# Then open http://localhost:9911
```

The installer copies only what the runtime needs (no `node_modules/`, no `.venv/`, no dev-only docs), generates a default `env` file, and writes `_meta/` markers so future `--update` and `--uninstall` calls find the right target. User data (`memories.json`, `synapses.json`, `thought_bubbles.json`, `env`) is preserved across `--update` runs.

If you'd rather skip the AppData install and run from wherever you extracted (no relocation, no installer), Options A/B/C below all work standalone — just `docker compose up` from inside the source tree. You give up the canonical install location and the `--update` workflow, but it's a valid quick-start.

#### Option A — Everything in Docker (zero host installs)

```bash
docker compose up
# Then open http://localhost:9911
```

Brings up the default stack:

- `synaptic-core` on port 9911 — Go binary that serves the dashboard, hosts the API + WebSocket, runs the bubble generator, and persists memories. (Triple duty replaces the previous separate dashboard container.)
- `synaptic-ollama-tier1` on port 11434 — always-on local LLM for the Tier 1 (Encoder) workload. The model pull (`llama3.2:3b`, ~2 GB) is baked into the container's entrypoint and runs once on first launch.
- `synaptic-ollama-tier2` (profile-gated, port 11435) and `synaptic-airllm-tier2` (profile-gated, port 11436) — Tier 2 (Nightly) services that only spin up when their compose profile is enabled in the Config UI.

No Python, no Node, no manual `ollama pull` — just Docker. Bank + per-tier Ollama models + AirLLM caches persist in named volumes across restarts. The dashboard works immediately even before the model finishes downloading; classification falls back to keyword-only and bubbles to the pre-baked pool until Ollama is ready.

If you already run Ollama on the host (port 11434 in use), Docker compose will fail to start `synaptic-ollama-tier1`. Either stop the host Ollama or comment out the `ollama-tier1` service in `docker-compose.yml` and set `SD_OLLAMA_URL=http://host.docker.internal:11434` (or your host LAN IP) on the `core` service.

To enable Bearer-token auth (recommended for any deployment exposed beyond localhost), drop a `.env` next to `docker-compose.yml` with `SD_API_TOKEN=<token>` (generate one with `openssl rand -hex 24`). See [`docs/REMOTE.md`](docs/REMOTE.md) for the full remote-operation guide.

For lower-spec hardware (Raspberry Pi 4/5), swap the Tier 1 model to a smaller one:

```bash
SD_OLLAMA_TIER1_MODEL=llama3.2:1b docker compose up
```

The `docker-compose.yml` lists Pi-friendly options including `qwen2.5:0.5b`, `tinyllama:1.1b`, `gemma2:2b`, and `phi3:mini`.

#### Option B — Mock-only dashboard (no SD Core, no Docker)

```bash
python serve.py
# (Windows: double-click serve.bat. macOS/Linux: ./serve.sh)
# Dashboard opens at http://localhost:8765/index.html
```

You need a modern browser (Chrome, Edge, Firefox, Safari recent versions) and Python 3.7+. No npm install, no build step. The dashboard is a single `index.html` plus a few JSON data files in `assets/`. The brain runs on the built-in mock event engine — every UI feature works (themes, cognitive modes, MRI slice, regions panel, timeline scrubber, thought bubbles from the pre-baked pool) but real adapter events can't reach the dashboard without SD Core.

To see your real AI client drive the brain, use Option A or C instead. Or open the dashboard via Option B and then point it at a remote SD Core via the Config panel's Bridge section.

> **Why a server and not just double-click?** Modern browsers block `fetch()` from `file://` URLs for security. The bundled `serve.py` runs Python's standard-library HTTP server in this folder — no external dependencies, ~20 lines of logic.

#### Option C — Fully native, no Docker

If you don't want Docker on this machine at all:

```bash
# SD Core (Go ≥ 1.25 required) serves both the API AND the dashboard at :9911.
cd bridge/core
go build -o sd-core .
./sd-core --listen 127.0.0.1:9911 --data-dir ../..
# Now open http://localhost:9911 in your browser.

# Optional second terminal — Ollama for classifier + bubbles
ollama serve &
ollama pull llama3.2:3b
```

The Go build produces a static binary (~15 MB, CGO disabled) — no runtime dependencies, runs anywhere. Cross-compile for distribution with `GOOS=linux/darwin/windows GOARCH=amd64/arm64 go build`.

If you skip Ollama, the dashboard falls back to keyword-only region classification and the pre-baked thought-bubble pool — everything still works, the bubbles just feel less personalized.

If you'd rather not install Go either, fall back to Option B above — just `python serve.py` runs the mock-only dashboard.

#### Where you are after Step 1

After any of A / B / C you have a working dashboard. The HUD shows `MODEL: MOCK` and the brain pulses with simulated activity. To see your real AI client drive the brain, do Step 2.

### Step 2 — Plug in your AI client

Pick whichever surface your tool exposes — they all land at SD Core on `localhost:9911` and any number of them can run side-by-side. The full per-client reference (Claude Desktop, Cursor, Cline, Continue, Gemini CLI, Antigravity, OpenCode, Goose, Aider, …) is in [`docs/INTEGRATIONS.md`](docs/INTEGRATIONS.md). For wiring devices on a different network than the dashboard host (laptop on the road talking to a desktop at home), see [`docs/REMOTE.md`](docs/REMOTE.md) — Cloudflare Tunnel + Bearer token (recommended) or Tailscale (private-mesh alternative).

> If the AI you want to wire up is _itself_ the one reading this README, point it at [`Agent-Install.md`](./Agent-Install.md) — it is the same content rewritten as a self-driving runbook so the agent can install Synaptic into its own client without a human in the loop.

#### Option 1 — Claude Code plugin (highest-fidelity)

Uses Claude Code's native hook surface, no MCP layer, no model-side prompting needed. From inside any Claude Code session:

```text
/plugin marketplace add /abs/path/to/synaptic-<version>
/plugin install synaptic-claude-code@synaptic
```

Or, after publishing to GitHub:

```text
/plugin marketplace add nomadsgalaxy/Synaptic-Disorder
/plugin install synaptic-claude-code@synaptic
```

> **Directory-marketplace path: point at the repo root, not the plugin subdirectory.**
> Claude Code's directory marketplace discovers plugins by reading `.claude-plugin/marketplace.json` at the path you give it, then scanning the `source` paths listed there. For Synaptic, that file lives at the repo root and points at `./bridge/claude-code-plugin`.
>
> - ✓ Correct: `/plugin marketplace add C:\path\to\synaptic-2.3.0b1`
> - ✗ Incorrect: `/plugin marketplace add C:\path\to\synaptic-2.3.0b1\bridge\claude-code-plugin`
>   If you accidentally point at the plugin subdirectory, the install will fail with "plugin not found in marketplace". The nested `.claude-plugin/plugin.json` is fine where it is — don't move it; Claude Code expects to read it from there.

Tool calls flash motor cortex, prompts hit Wernicke's, responses hit Broca's, errors fire the amygdala. The plugin never blocks the host session — every hook exits within ~1.2 s regardless of whether SD Core is reachable. Full reference: [`bridge/claude-code-plugin/README.md`](bridge/claude-code-plugin/README.md).

##### Pointing the plugin at a remote SD Core

The plugin defaults to `localhost:9911`. To point at a tunneled SD Core (e.g. `https://cognito.example.com`), set `SD_CORE_URL` and `SD_API_TOKEN` in the plugin's `mcpServers` env block — see [`bridge/claude-code-plugin/README.md`](bridge/claude-code-plugin/README.md). The plugin works without Docker installed on the client machine: it's pure forwarding to whatever SD Core URL you give it.

#### Option 2 — MCP server (Claude Desktop, Cursor, Cline, Continue, Gemini CLI, Goose, …)

Works with anything that speaks Model Context Protocol. Two MCP tools are exposed: `report_event` (any v1.0 event) and `report_memory_save` (memory_added wrapper).

First install the adapter once:

```bash
cd bridge/mcp-adapter && npm install
```

Then add to your client's `mcpServers` config:

```json
{
  "mcpServers": {
    "synaptic-disorder": {
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

Config file location per client:

| Client                   | Config path                                                       |
| ------------------------ | ----------------------------------------------------------------- |
| Claude Desktop (macOS)   | `~/Library/Application Support/Claude/claude_desktop_config.json` |
| Claude Desktop (Windows) | `%APPDATA%\Claude\claude_desktop_config.json`                     |
| Claude Desktop (Linux)   | `~/.config/Claude/claude_desktop_config.json`                     |
| Cursor                   | Settings → MCP → Add new server                                   |
| Cline                    | VS Code Cline panel → ⚙️ → MCP Servers → Edit JSON                |
| Continue                 | `~/.continue/config.json`                                         |
| Gemini CLI               | `~/.gemini/mcp_servers.json`                                      |
| Goose                    | `~/.config/goose/config.yaml`                                     |
| OpenCode                 | `~/.opencode/config.json`                                         |

Restart the client after editing. Set `SD_CLIENT` to a label that matches your client (e.g. `cursor`, `cline`) so the multi-session HUD distinguishes parallel agents. Adapter reference: [`bridge/mcp-adapter/README.md`](bridge/mcp-adapter/README.md).

#### Option 3 — OpenTelemetry adapter (broadest coverage)

Anything with an OpenInference or GenAI semantic-conventions instrumentation works — `opentelemetry-instrumentation-anthropic`, `opentelemetry-instrumentation-openai-v2`, OpenInference for LangChain / LlamaIndex / AutoGen / CrewAI, etc. It's also the only path that surfaces token-level streaming.

```bash
cd bridge/otel-adapter && npm install && node index.js
# Receiver listening on http://127.0.0.1:4318/v1/traces
```

Then point your app's OTLP exporter at `http://localhost:4318`. Span-kind to event-type mapping (LLM → broca, RETRIEVER → hippocampus, TOOL → motor cortex, AGENT → prefrontal cortex) is in [`bridge/otel-adapter/README.md`](bridge/otel-adapter/README.md).

#### Option 4 — Adapter SDK (anything custom)

For your own agent loop, an in-house framework, or a tool plugin emitting from inside its own process. Two-line wire-up:

```python
# pip install synaptic
import synaptic as sd
with sd.connect(adapter_id="my-tool", model="claude-opus-4-7") as c:
    c.emit("tool_call", {"tool_name": "bash"}, region_hint="motor_cortex")
```

```ts
// npm install @synaptic/sdk
import { connect } from "@synaptic/sdk";
const c = connect({ adapterId: "my-tool", model: "claude-opus-4-7" });
await c.emit(
  "tool_call",
  { tool_name: "bash" },
  { regionHint: "motor_cortex" },
);
```

Both SDKs validate against the v1.0 event schema, auto-emit `session_start` / `session_end`, and are fire-and-forget over a 1.5 s timeout. SDK references: [`bridge/sdk/python/README.md`](bridge/sdk/python/README.md) and [`bridge/sdk/typescript/README.md`](bridge/sdk/typescript/README.md).

#### A note on memory tagging

If your agent saves memories (via `report_memory_save`, the SDK, or any memory provider feeding `assets/memories.json`), the tags you attach decide which brain region the neuron lives in and which other neurons it forms synapses with. Read [`docs/MEMORY_AUTHORING_GUIDE.md`](docs/MEMORY_AUTHORING_GUIDE.md) before integrating — untagged memories produce orphan neurons floating disconnected from the rest of the brain.

### Step 3 — Verify

```bash
curl http://localhost:9911/healthz                                  # SD Core up?
curl 'http://localhost:9911/events?limit=5' | jq '.events[].type'   # Events arriving?
```

The dashboard's HUD model line flips from `MODEL: MOCK` to `MODEL: <your-model> · BRIDGE LIVE` once at least one adapter is firing. Multiple concurrent clients show up as `MODELS: claude-opus-4-7 (12) · gpt-5 (3)` — the SessionTracker tops by recent event count and times sessions out after ~60 s of silence.

If the dashboard never flips out of `MOCK`, see the Troubleshooting section of [`docs/INTEGRATIONS.md`](docs/INTEGRATIONS.md#troubleshooting).

### Step 4 — Populate the brain with memories

A fresh install ships with **no memories** — `assets/memories.json` and `assets/synapses.json` don't exist in the public release at all. SD Core auto-creates empty stubs on first start, so the dashboard launches with an empty brain (voxels visible, no neurons yet). This is normal and intentional: the project doesn't ship anyone else's memory data, and your brain should be your own.

Two paths to populate it:

- **Connect a memory provider** — `tools/memory_providers/` ships with three providers: Hindsight (backup file or live MCP), JSON file, and Markdown folder. Pick the one matching where your memories already live, run it, and it writes `assets/memories.json` with proper tags. The dashboard picks up the changes via the file watcher.

- **Have an AI agent populate via `report_memory_save`** — the MCP adapter exposes a `report_memory_save(text, tags, …)` tool. Any AI you've connected (Claude Desktop, Cursor, Cline, etc.) can call it, and a fresh neuron lights up in the right anatomical region with synapses to whatever shares its tags.

**Read [`docs/MEMORY_AUTHORING_GUIDE.md`](docs/MEMORY_AUTHORING_GUIDE.md) before populating either way.** Tags decide where a memory's neuron lives and which other neurons it connects to via synapses. Untagged memories produce orphan neurons floating disconnected from the rest of the brain. Five minutes with the guide saves you a hairball-shaped brain.

#### Want demo data on first launch?

Optional: run `python tools/seed_with_example.py` to populate the brain with the synthetic example dataset (168 memories across all 14 regions, ~857 synapses). Useful for screenshots, demos, or just seeing what a "loaded brain" looks like before wiring up your real data.

The seed is **self-disposing**: it carries a `synthetic: true` flag, and SD Core auto-wipes it the moment your first real adapter fires a `memory_added` event. The brain transitions cleanly from demo data to your own activity with no manual cleanup.

## What you'll see

A slowly-rotating 3D brain built from ~80,000 Allen Atlas voxels. Each glowing point is one of your memories, placed in the anatomical region whose function matches the memory's content (visual cortex for web/UI work, motor cortex for tool execution, hippocampus for memory consolidation, etc.). Synapses are real tubes routed through brain tissue and thicken with use over time (Hebbian learning). The HUD shows scan readouts, an EEG-style activity graph, the event log, live neurogenesis counters, brain age, and the strongest pathways your brain has built up.

When SD Core is reachable, real activity from your AI clients fires regions in real time — visual cortex flashes when Claude takes a screenshot, motor cortex flares on a tool call, the amygdala goes red on errors. When events fire, **synapse pulses** travel along the connecting tubes from the firing neuron toward its neighbors. **Thought bubbles** spawn from individual neurons (which activate when their thought surfaces) and Ollama generates new bubbles every ~30 s conditioned on your active cognitive modes, recent events, and a sample of your real memories — so the bubbles read like THIS brain thinking, not stock GPT.

A **timeline scrubber** at the bottom of the viewport lets you drag back to the earliest memory and watch the brain develop neuron-by-neuron — the marching-cubes brain surface visibly shrinks and grows with the visible set.

If memories in your brain are missing embeddings or deep encoding (common after importing a backlog or switching Tier 2 models), the Deep encoding card surfaces a single **Backfill memories · N embed + M encode** CTA. One click runs `/admin/embed-all` first to fill any missing embeddings so Phase 1 dedup can stop skipping, then chains into `/admin/deep-encode-all` for Phase 0b enrichment. Two stacked progress bars track each phase in real time via the `embed_backfill_progress` / `deep_encode_backfill_progress` WS events.

## Troubleshooting

| Symptom                                                                                        | Likely cause                                                                       | Fix                                                                                                                                                                                                                |
| ---------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `/plugin install synaptic-claude-code@synaptic` → "plugin not found in marketplace 'synaptic'" | Marketplace path points at `bridge/claude-code-plugin` instead of the repo root    | Re-run `/plugin marketplace add` with the path to the unzipped root (`synaptic-<version>/`) — the directory that contains `.claude-plugin/marketplace.json`                                                        |
| Plugin installed but hooks not firing                                                          | `forward.js` can't find its peer files                                             | Don't move files around inside `bridge/claude-code-plugin/`. The plugin expects its original layout and resolves `${CLAUDE_PLUGIN_ROOT}/../mcp-adapter/index.js` from the plugin dir                               |
| Dashboard 404s on `GET /` after a Profile Enable in the Services panel                         | Compose-helper bug (`.env` not read, or wrong project-dir bind) — fixed in 2.1.2b1 | Update to ≥ 2.1.2b1 and rerun. If still 404, `cd "<install>"; docker compose up -d --build` from a host shell to bypass the helper                                                                                 |
| Dashboard HUD says `MODEL: MOCK` after wiring an adapter                                       | Adapter isn't reaching SD Core                                                     | Check `curl -fsS http://localhost:9911/healthz` (Bearer-token form if auth enabled); adapter logs (`docker compose logs -f core`); confirm `SD_CORE_URL` + `SD_API_TOKEN` env vars are set in the adapter's config |
| Remote MCP adapter (Cloudflare Tunnel) returns 401                                             | Bearer token mismatch                                                              | Token in adapter's `SD_API_TOKEN` must equal `SD_API_TOKEN` in SD Core's `.env`. Tokens are case-sensitive and the full string (no trailing newlines)                                                              |
| `external volume "synaptic-disorder-bank" not found` on fresh install                          | Stale 2.0.1b1 ZIP (compose volume was external)                                    | Use 2.0.2b1 or newer. The volume is compose-managed in current releases                                                                                                                                            |

## Mobile

On viewports ≤ 900 px CSS pixels, the dashboard reflows automatically: brain canvas takes the top 50 vh, all HUD blocks + chip switchers stack into a scrollable bottom panel, the timeline pins to the seam between brain and panel, and the side panels (theme/modes/regions/etc.) open into the bottom half so you can see the brain react live as you toggle settings.

## Customization

- **Subject name** — click `SUBJECT: SUBJECT-01` in the HUD to rename your subject (persists in `localStorage`).
- **Themes** — 13 built-ins (Medical, Tron, Matrix, CRT Amber, Bioluminescent, Blueprint, Solar Flare, Clinical Light, Research Print, Dark Hex, Dark Hex Pro, Light Hex, Light Hex Pro) plus a full custom-theme editor with JSON import/export.
- **Cognitive modes** — 10 stackable opt-in modes (The Voices, The Tism, ADHD, Synesthesia, Pattern Seeker, Stim Mode, Time Blindness, Hyperempathy, Special Interest, Sensory-Soothing). Every one of the 1024 possible combinations gets a unique fun name (45 hand-curated pairs like `AuDHD`, `Cozy Mode`, `Internal Council`; the rest generated deterministically as evocative 2- or 3-word names like `Frantic Spiral` or `Tender Wrapped Hourglass`).
- **MRI slice** — Off / Horizontal / Sagittal / Coronal with auto-flip.
- **Render panel** — live tuning of voxel/sphere/synapse/brain mesh appearance.
- **Regions sidebar** — sortable list of all 14 anatomical regions with real-anatomy + dashboard-mapping descriptions and per-region memory drill-down.

## Remote operation

The dashboard runs fine cross-machine: your dashboard host hosts SD Core + the dashboard, your client machine runs an adapter that forwards events from any network. Two transport options — **Cloudflare Tunnel + Bearer token** (recommended; works from anywhere, optional SSO via Cloudflare Access) and **Tailscale** (private mesh; no public hostname). Step-by-step setup for both, plus the dashboard's runtime URL+token Config panel and one-token-fits-all-machines convention, is in [`docs/REMOTE.md`](docs/REMOTE.md).

The dashboard uses the same code path locally and remotely — point a browser at the URL, it figures out same-origin vs configured-elsewhere automatically. Once a real adapter event arrives over the bridge, the mock engine is locked out for the rest of the session so the dashboard never misleads you with simulated activity in front of real activity.

## Architecture

Two pluggable layers, both ports-and-adapters.

**Live activity** (real-time, push-based) — adapters emit events to **SD Core** (`bridge/core/`, Go single static binary on port 9911). SD Core fan-outs to dashboards via WebSocket and persists `memory_added` events to a SQLite fallback bank (`/bank/*` HTTP endpoints).

Shipped adapters:

- **`bridge/claude-code-plugin`** — Claude Code plugin via marketplace; HTTP hooks for `SessionStart/End`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `PostToolUseFailure`, `SubagentStop`, `Stop`. Zero-dep Node forwarder, never blocks the host session.
- **`bridge/mcp-adapter`** — universal MCP server with `report_event` and `report_memory_save` tools; works with Claude Desktop, Cursor, Cline, Continue, Gemini CLI, Claude Code.
- **`bridge/otel-adapter`** — OTLP/HTTP receiver on port 4318 (JSON + Protobuf) that translates OpenTelemetry traces from `opentelemetry-instrumentation-anthropic`, `opentelemetry-instrumentation-openai-v2`, and OpenInference (LangChain, LlamaIndex, AutoGen, CrewAI, etc.) into Synaptic events.
- **`bridge/sdk`** — dual-language adapter SDK (Python `synaptic-disorder` + TypeScript `@synaptic/sdk`) for anything custom.

**Memory state** (slow loop, persistent) — memory providers translate any backing store into a normalized memory schema. v1 ships with JSON file, Markdown folder, MCP-AI-reported (with the SQLite fallback bank), and a backup-file importer for migrating from other memory tools.

**Region classifier** is a layered cascade in `tools/classifier.py` (build-time, runs Ollama Tier 3) plus a runtime mirror in the dashboard for live `memory_added` events (Tier 1 tag patterns → Tier 2 keyword scan; falls back to `frontal_lobe`).

See `docs/event-schema.md` and `docs/memory-schema.md` for the full contracts.

## Local LLM (Ollama)

Synaptic uses a small local LLM via [Ollama](https://ollama.com) for two jobs:

1. **Region classifier — Tier 3** _(shipped)_. The build pipeline (`tools/classifier.py`) walks each memory through a tag-pattern → keyword-scan → LLM cascade. Cache is content-hashed.
2. **Thought-bubble generator** _(shipped, runtime)_. The dashboard asks Ollama for one new phrase every ~30 s, conditioned on (a) the user's active cognitive modes, (b) the recent event stream, (c) 3 random memories sampled from your bank — and only adds it to the persistent pool if a second Ollama call says it's not too similar to existing entries. Phrases are seeded with rotating "flavors" (sudden questions, weird memories, cravings, hyperfixations, intrusive worries, etc.) so the bubbles feel idiosyncratic rather than generic.

Both fail gracefully when Ollama is offline. The Docker stack pulls and serves Ollama for you; for native install:

```bash
ollama pull llama3.2:3b   # 2 GB
```

As of v2.3.0b1, SD Core auto-sizes Ollama's `num_ctx` per request — estimated prompt tokens + completion budget, rounded up to the next power-of-two (floor 2 KB, ceiling 16 KB). Heavy nightly prompts (dream-entry, Phase 0b enrichment) get the headroom they need; lightweight Tier 1 calls keep their small windows. Without this, Ollama's default 2048-token window silently truncated long prompts mid-word.

> Earlier in development I tried capturing live data with a man-in-the-middle proxy for stubborn agents like Gemini that lack a plugin surface. **I cannot recommend setting up an intentional MITM proxy just for a novelty dashboard** — it requires installing a system-wide root certificate that exposes all your traffic. The adapter approach above is dramatically less invasive.

## Folder layout

```
Synaptic Disorder/
├── docker-compose.yml              Release-mode stack (core + per-tier ollama + AirLLM Tier 2 sidecar)
├── .claude-plugin/marketplace.json Claude Code plugin marketplace manifest
├── index.html                      The dashboard. Single file.
├── serve.py / serve.bat / .sh      Local launcher.
├── assets/
│   ├── voxels.json                 ~80k Allen Atlas voxels (auto-generated)
│   ├── memories.json               Memories on voxels (auto-generated; PERSONAL DATA)
│   ├── synapses.json               Computed shared-tag paths through voxels
│   └── anatomy.json                14 region definitions (centers, radii, tag patterns)
├── docs/
│   ├── INTEGRATIONS.md             Per-client wiring (Claude Desktop, Cursor, Cline, …)
│   ├── REMOTE.md                   Cloudflare Tunnel + Tailscale paths for cross-machine setup
│   ├── MEMORY_AUTHORING_GUIDE.md   How to tag memories so they land in the right region
│   ├── data_privacy.md             What data exists, where it lives, where it travels
│   ├── event-schema.md             Live activity event contract (v1.0)
│   ├── event.schema.json
│   ├── memory-schema.md            Memory record contract (v1.0)
│   ├── memory.schema.json
│   ├── CREDITS.md                  Third-party attribution (Allen Atlas, Three.js, etc.)
│   └── dev/                        Dev-only docs — excluded from public release builds
│       ├── HANDOFF.md              Agent-handoff doc for next session
│       ├── PROJECT_PLAN.md         Living roadmap
│       └── SHIPPING.md             One-page architecture cheat sheet
├── tools/
│   ├── build_voxel_brain.py        Main asset rebuild pipeline
│   ├── classifier.py               Layered region classifier (tag/keyword/Ollama)
│   └── memory_providers/           Per-provider scripts that produce memories.json
└── bridge/
    ├── core/                       SD Core (Go binary; serves dashboard, API, WS, bubble gen)
    ├── claude-code-plugin/         Claude Code hooks adapter
    ├── mcp-adapter/                Universal MCP server adapter
    ├── otel-adapter/               OpenTelemetry OTLP/HTTP receiver
    └── sdk/
        ├── python/                 synaptic-disorder package
        └── typescript/             @synaptic/sdk package
```

## Roadmap

- **Phase 1** _(complete)_ — dashboard with mock events, themes, panels, Hebbian synapses, thought bubbles, voxel-based brain
- **Phase 2** _(complete)_ — SD Core, three primary adapters (Claude Code / MCP / OTel), Adapter SDK, three memory providers, SQLite fallback bank, Ollama bundle in Docker
- **Phase 3** _(complete)_ — timeline scrubber, synapse pulse animation, layered classifier (tag/keyword/Ollama), runtime classifier, mobile layout, multi-agent session tracker, customizable subject, every-combo-named cognitive modes, Ollama-driven quirky thought bubbles
- **Phase 4** _(in progress)_ — release-mode build (D1 ✅ — Prod ships without user data; SD Core auto-creates empty on first run), synthetic example dataset (D2 ✅ — fully randomized 168-memory seed for Pages + opt-in via `seed_with_example.py`), final docs/credits pass (D3 ✅), portable ZIP + start scripts (D4 partial ✅; native Windows `.exe` / macOS `.dmg` / Linux `.deb`/`.rpm` still pending — design at [`docs/dev/INSTALL_DESIGN.md`](docs/dev/INSTALL_DESIGN.md)), public demo (D5 — GitHub Pages workflow ready, one manual trigger away from live), one-page docs site (D6 ✅ — `docs/site/index.html`)
- **Phase 5 — Memory cognition** _(in progress)_ — purpose-built embed model (P ✅), hybrid BM25+cosine retrieval (J ✅), inline auto-dedup (K ✅), dormant-memory lifecycle (L ✅), cross-encoder rerank with dynamic scaling (✅), keyword-rewrite multi-probe retrieval (✅); `sd_link` lexicon-link MCP tool (M) and entity resolution (N) still in flight. See [`docs/WHAT_SYNAPTIC_IS.md`](docs/WHAT_SYNAPTIC_IS.md) for the design philosophy.
- **Polish** — A4 stencil-buffer cap-fill for sliced spheres remains as the only Track-A item not yet shipped (low priority)

## Privacy

Default mode is localhost-only — nothing leaves your machine. SD Core binds to `127.0.0.1`, Ollama runs locally, no analytics, no phone-home. The one unavoidable third-party network call is the dashboard fetching Three.js from `cdnjs.cloudflare.com`; self-hosting alternative documented in [`docs/data_privacy.md`](docs/data_privacy.md). When you opt into remote operation (Cloudflare Tunnel or Tailscale), the privacy doc walks through exactly what each transport can see and log.

[`docs/data_privacy.md`](docs/data_privacy.md) is the precise reference: what data exists, where it lives on disk and in volumes, where it travels, what's NOT collected, and your controls (env flags to disable bubble gen, wipe the bank, clear localStorage, self-host CDN assets, etc.).

## Credits

See [`docs/CREDITS.md`](docs/CREDITS.md) for the full attribution detail.

In short:

- **Anatomical atlas** — **Allen Human Reference Atlas – 3D, 2020**, © 2019 Allen Institute for Brain Science, RRID:SCR*017764, CC BY 4.0. Dataset citation: Ding et al. (2020); structure ontology citation: Ding et al. (2016), \_J. Comp. Neurol.* DOI [10.1002/cne.24080](https://doi.org/10.1002/cne.24080).
- **Three.js** — MIT.
- **Ollama** — `ollama/ollama` Docker image (MIT).
- **Inspiration** — [Neurotorium's Brain Atlas](https://neurotorium.org/tool/brain-atlas/) and the [BrainMinds 3D Atlas Viewer](https://dataportal.brainminds.jp/3d-atlas-viewer).

## License

Synaptic is released under the **Open Community License v1.1 + Software Attribution v1** (`OCL v1.1 + SWAtt v1`).
See [`LICENSE`](LICENSE) and [`LICENSE-SWAtt.md`](LICENSE-SWAtt.md) for the full terms.
Derivatives must retain visible attribution ("built on Synaptic") in their UI and source per SWAtt v1.
