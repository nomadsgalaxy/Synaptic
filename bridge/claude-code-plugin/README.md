# synaptic-claude-code

Claude Code plugin that forwards agent activity to the **Synaptic** live brain dashboard (formerly "Synaptic Disorder"). While the plugin is enabled and SD Core is running on `localhost:9911`, every prompt, tool call, tool result, and session boundary lights up the corresponding region in the dashboard in real time. The plugin also bundles an MCP server (`synaptic`) exposing 17 tools for memory ops, Tier 3 Oracle research, dream-pipeline control, and audit/budget observability.

## What it forwards

| Claude Code hook | → Synaptic event | Region hint |
|---|---|---|
| `SessionStart` | `session_start` (with `model`, `cwd`) | — |
| `SessionEnd` | `session_end` (with `reason`) | — |
| `UserPromptSubmit` | `prompt_received` (truncated to 200 chars) | `wernicke_area` |
| `PreToolUse` | `tool_call` (tool name + arg key list, **no values**) | per-tool (Bash → motor cortex, Read → visual cortex, etc.) |
| `PostToolUse` | `tool_result` (`ok: true`) | `cerebellum` |
| `PostToolUseFailure` | `error` | `amygdala` |
| `SubagentStop` | `subagent_complete` | `prefrontal_cortex` |
| `Stop` | `response_complete` | `broca_area` |

Tool **input values are never forwarded** — only the input's top-level keys, so the dashboard knows the structural shape without leaking prompt or file contents. Prompt text is truncated to 200 characters.

> **Note for memory persistence:** The Claude Code plugin only forwards **activity** to the dashboard. If your agent also writes to a separate memory backend, follow [`docs/MEMORY_AUTHORING_GUIDE.md`](../../docs/MEMORY_AUTHORING_GUIDE.md) for tagging conventions so each saved memory lands in the right region and forms synaptic connections with related ones.

## Install

The plugin lives in the Synaptic repo. Install it via the marketplace at the repo root.

```bash
# In Claude Code (any project)
/plugin marketplace add /path/to/AI\Synaptic Disorder
/plugin install synaptic-claude-code@synaptic
```

Or, after publishing to GitHub:

```bash
/plugin marketplace add nomadsgalaxy/Synaptic-Disorder
/plugin install synaptic-claude-code@synaptic
```

## Run SD Core

The plugin POSTs to `http://localhost:9911/event`. Start SD Core first:

```bash
cd "<install root>"
docker compose up -d
```

`<install root>` is the AppData install printed by `python tools/install_to_appdata.py` (e.g. `%LOCALAPPDATA%\Synaptic` on Windows). SD Core comes up from the install root, not from `bridge/core/` — the canonical `docker-compose.yml` lives at the install root.

If SD Core isn't reachable, the plugin silently drops events (it will not block your Claude Code session — every hook exits 0 within ~1.2 s regardless).

## Run the dashboard

```powershell
cd "<install root>"
python serve.py
# http://localhost:9911
```

When the bridge is up, the HUD's model line will flip to `MODEL: claude-code  · BRIDGE LIVE` (or to your `SD_MODEL` value — see below). With multiple Claude Code sessions running concurrently, the line becomes `MODELS: ...`.

## Configuration

Edit **`hooks/config.json`** — it ships with localhost defaults and is the easiest place to configure the plugin:

```json
{
  "SD_CORE_URL": "http://localhost:9911",
  "SD_API_TOKEN": ""
}
```

To point at a remote SD Core, set `SD_CORE_URL` to its full URL and `SD_API_TOKEN` to the bearer token it was started with. That's it.

All options (env vars override `config.json` if both are set):

| Var | Default | Notes |
|---|---|---|
| `SD_CORE_URL` | `http://localhost:9911` | Full URL of SD Core. Preferred for remote operation. When set, takes precedence over `SD_CORE_HOST` + `SD_CORE_PORT`. |
| `SD_CORE_HOST` | `127.0.0.1` | Legacy host for localhost mode. |
| `SD_CORE_PORT` | `9911` | Legacy port. |
| `SD_API_TOKEN` | _(empty)_ | Bearer token sent on every request when set. Match what SD Core was started with. Required for any deployment with auth on. |
| `SD_ADAPTER_ID` | `claude-code-hooks` | Used as `adapter_id` on every event. |
| `SD_MODEL` (or `CLAUDE_CODE_MODEL`) | `claude-code` | Reported as `payload.model` on `session_start`. Claude Code doesn't expose the active model to hooks today, so set this manually if you want a real model name in the HUD (e.g. `claude-opus-4-7`). |

For wiring this plugin to a remote SD Core (your dashboard runs on a different machine), see [`docs/REMOTE.md`](../../docs/REMOTE.md).

## Privacy

This plugin runs locally and only talks to `localhost`. It does not phone home. The forwarder does not include:

- Tool input values (only key names)
- Tool output text
- Full prompt content (truncated to 200 chars)
- Any transcript file contents

If you want zero forwarding for a session, disable the plugin via `/plugin disable synaptic-claude-code` or stop SD Core — the plugin's events will simply be dropped on the floor.

## Files

```
synaptic-claude-code/
├── .claude-plugin/
│   ├── plugin.json   # plugin manifest
│   └── hooks.json    # hook → forwarder bindings
├── hooks/
│   └── forward.js    # the Node forwarder (no third-party deps)
└── README.md
```

## Troubleshooting

- **Dashboard shows `MOCK` even though Claude Code is firing.** Check SD Core is up: `curl http://localhost:9911/healthz` should return 200.
- **Events arriving but the wrong region pulses.** Region hints are coarse; tools without an explicit mapping land in `motor_cortex`. Add a mapping in `hooks/forward.js → TOOL_REGION` and reload.
- **Hooks not firing at all.** Confirm install with `/plugin list`. Hooks live under `.claude-plugin/hooks.json`; if you forked the plugin, make sure `plugin.json`'s `hooks` field still points there.

## License

Pending — Open Community License. See the parent Synaptic repo.
