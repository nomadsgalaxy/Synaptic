# Synaptic — Plugin + MCP install (handoff doc)

**Audience:** another agent (or human) setting up the Synaptic dashboard integration in their Claude Code session. Synaptic was previously branded "Synaptic Disorder"; the product is now called **Synaptic** ("Syn" for short), but the plugin slug, MCP server name, env vars (`SD_CORE_URL`, `SD_API_TOKEN`), and localStorage keys all keep their `sd_*` / `synaptic-disorder-*` prefixes for backward compatibility.

**Prereqs:** Claude Code CLI v2.0+, Node 20+, a running SD Core (`docker compose up -d` from `Synaptic Disorder/Prod` or your AppData install).

---

## What this installs

A single Claude Code plugin (`synaptic-claude-code` v0.4.0) that bundles:

- **8 hooks** — `SessionStart`/`End`, `UserPromptSubmit`, `PreToolUse`/`PostToolUse`/`PostToolUseFailure`, `SubagentStop`, `Stop` — forwarding events to the dashboard so the brain pulses in real time as Claude works. The forwarder now also recognizes the new dream-pipeline MCP tools and emits matching `dream_run_started`, `deep_encode_started`, `embed_backfill_started`, and `lexicon_rebuild_started` pulses.
- **21 MCP tools** under `mcp__synaptic__*`:
  - **Memory CRUD** — `sd_create_memory`, `sd_recall`, `sd_list_memories`, `sd_get_memory`, `sd_update_memory`, `sd_delete_memory`, `sd_restore_memory`
  - **Memory lifecycle (new in v2.7 — Bundle L/M)** — `sd_forget` (hard delete with audit), `sd_supersede(old_id, new_id, reason)` (mark old row superseded), `sd_set_dormant(memory_id, dormant)` (flip dormancy state directly), `sd_set_ttl(memory_id, expires_at)` (schedule auto-dormancy via nightly Phase 5b)
  - **Research / observability** — `sd_research`, `sd_get_audit`, `sd_get_budget`
  - **Dream pipeline** — `sd_dream_run` (manual nightly kick), `sd_dream_status` (recent runs), `sd_deep_encode_all` (Phase 0b backfill, ~70 min for 3000 memories), `sd_embed_all` (Tier 1 embedding backfill), `sd_lexicon_rebuild`, `sd_get_dream_settings`, `sd_set_dream_settings` (Phase 0b knobs include `deep_enrich_enabled`, `deep_enrich_max_per_run`, `deep_enrich_token_budget_per_run`, `deep_enrich_concurrency`, plus `deep_enrich_reencode_on_model_change` — off by default; when on, swapping the Tier 2 model bulk-flags every previously-encoded memory with reason `model_upgrade` so the next Phase 0b run re-encodes them on the new model). On the dashboard, `sd_embed_all` and `sd_deep_encode_all` are exposed as a single **Backfill memories · N embed + M encode** button that chains both phases.
  - **Legacy event push** — `report_event`, `report_memory_save` (kept for compatibility; new code should prefer `sd_create_memory`)
- **4 slash commands** — `/sd-recall <q>`, `/sd-research <q>`, `/sd-status`, `/sd-flag <q-or-id>`
- **MCP server config** that defaults to `http://localhost:9911` (override via `SD_CORE_URL` env or `bridge/mcp-adapter/config.json`).

---

## Install (3 commands)

The marketplace lives at the repo root in `.claude-plugin/marketplace.json`. Replace `<PATH>` with whichever you have:

- Local clone: `/path/to/ai\Synaptic Disorder\Prod` (or `Dev`)
- AppData install: `/path/to/AppData\Local\SynapticDisorder`
- GitHub: `nomadsgalaxy/Synaptic-Disorder`

```bash
# 1. Register the marketplace
claude plugin marketplace add "<PATH>"

# 2. Install the plugin (user-scope by default)
claude plugin install synaptic-claude-code@synaptic

# 3. Restart Claude Code so hooks + MCP server start fresh
#    (the install step itself ends with "Restart to apply changes")
```

---

## Configuration

The MCP server reads, in order: `process.env` (highest), `bridge/mcp-adapter/config.json`, built-in defaults.

| Variable | Default | What it controls |
|---|---|---|
| `SD_CORE_URL` | `http://localhost:9911` | SD Core HTTP base. Override for remote (`https://cognito.example.com`). |
| `SD_API_TOKEN` | `""` | Bearer token. Required when SD Core has `SD_API_TOKEN` set in its env. |
| `SD_ADAPTER_ID` | `claude-code-mcp` | Shows up in dashboard audit log + `adapter_id` field. |
| `SD_CLIENT` | `claude-code` | Display string in `session_start` events. |

The plugin's own `mcpServers.env` block hardcodes `SD_CLIENT` and `SD_ADAPTER_ID` and **forces** `SD_CORE_URL=http://localhost:9911` for local development. To point at a remote SD Core for one session: set `SD_CORE_URL` in the plugin's env override OR remove the override and let the user-env value pass through.

The token is best set ONCE in `bridge/mcp-adapter/config.json` — it's outside git (`.gitignore`'d) and survives plugin updates without leaking into shell history.

---

## Verify the install

```bash
# Plugin shows v0.4.0 user-scope
claude plugin list | grep synaptic
#   ❯ synaptic-claude-code@synaptic

# Single MCP entry, plugin-managed (NOT a separate "synaptic-disorder" line)
claude mcp list | grep synaptic
#   plugin:synaptic-claude-code:synaptic-disorder: ... ✓ Connected

# Hooks fire — open a fresh session and watch the dashboard:
#   The "BRIDGE LIVE" chip flips on within 2s of session start
#   PreToolUse/PostToolUse pulses appear on every tool call
```

In Claude Code itself:

```
/sd-status
```

Should return a bank-size + budget summary block within 2 seconds.

---

## Common gotchas

1. **"Two MCP servers showing"** — if `claude mcp list` shows BOTH `plugin:synaptic-claude-code:...` AND a standalone `synaptic-disorder`, you have a duplicate from a pre-v0.2 install. Remove the standalone:
   `claude mcp remove synaptic-disorder`
2. **MCP tools work but slash commands don't** — restart Claude Code. The plugin's `commands/` directory is only scanned at session start.
3. **`401 unauthorized` on every tool call** — `SD_API_TOKEN` mismatch between MCP config and SD Core's `.env`. Check both have the same value, then `docker compose up -d core` (NOT `restart` — that doesn't re-read `.env`). Note: provider/model config changes do **NOT** require a restart since v2.3.0b1 — `PUT /settings/providers/{tier}` hot-swaps the tier live. Only environment-variable changes (token rotation, listen address) still need `up -d core`.
4. **Plugin updates aren't visible** — `claude plugin update` only diffs against the marketplace. If you edited the plugin source directly, run `claude plugin marketplace update <name>` first to refresh, then `update` to install.
5. **MCP path resolves wrong on disk** — the plugin uses `${CLAUDE_PLUGIN_ROOT}/../mcp-adapter/index.js`. The adapter MUST be at `bridge/mcp-adapter/` SIBLING to `bridge/claude-code-plugin/`, not inside it. If you've moved files, update `mcpServers.args` in `plugin.json`.
6. **Token leaks in transcripts** — never `xxd` or `cat` the `.env` file or any byte-level dump of credential-containing files; use `awk -F= '/^KEY=/ {print length($2)}'` patterns to inspect lengths only.

---

## What "working" looks like end-to-end

A new Claude Code session with the plugin installed should:

1. **At session start**: log line in SD Core container — `ws subscriber connected (total=N)` plus a `session_start` event with `adapter_id: "claude-code-mcp"`. Dashboard's BRIDGE chip turns green.
2. **On any tool use**: motor cortex region pulses with the `tool_call` event color. `report_event` and `report_memory_save` fire automatically via hooks; no agent code needed.
3. **On `/sd-recall <query>`**: returns top-8 ranked memories within ~200ms (hybrid BM25 + cosine RRF retrieval over Ollama embeddings; `recall_hybrid_enabled` setting controls fusion, default ON).
4. **On `/sd-status`**: returns bank size, 30-day budget, last 10 audit operations in a single screen.
5. **On `mcp__synaptic__sd_create_memory`**: brain pulses a fresh neuron in the auto-routed region, classifier picks up the new memory within 30s, sensitive-AI scans it within 60s, synapse builder embeds it on the next file-watch tick.

---

## Update flow (existing install → new version)

```bash
# 1. Sync the source files (only needed if you edited Dev/ directly):
cp <DEV>/bridge/mcp-adapter/index.js      <APPDATA>/bridge/mcp-adapter/index.js
cp <DEV>/bridge/claude-code-plugin/.claude-plugin/plugin.json  <APPDATA>/bridge/claude-code-plugin/.claude-plugin/plugin.json
cp -r <DEV>/bridge/claude-code-plugin/commands/  <APPDATA>/bridge/claude-code-plugin/commands/
cp <DEV>/.claude-plugin/marketplace.json  <APPDATA>/.claude-plugin/marketplace.json

# 2. Bump version in plugin.json (else the updater is a no-op)
#    Edit "version" field

# 3. Validate + refresh + update
claude plugin validate "<APPDATA>/bridge/claude-code-plugin"
claude plugin marketplace update synaptic-disorder
claude plugin update synaptic-claude-code@synaptic

# 4. Restart Claude Code
```

---

## File locations (Windows reference)

| Path | What's there |
|---|---|
| `/path/to/AppData\Local\SynapticDisorder\` | Live install — Docker mount, marketplace source |
| `<install>\bridge\mcp-adapter\index.js` | The MCP server itself (Node, 17 tools) |
| `<install>\bridge\mcp-adapter\config.json` | URL + token override (gitignored) |
| `<install>\bridge\claude-code-plugin\.claude-plugin\plugin.json` | Plugin manifest |
| `<install>\bridge\claude-code-plugin\commands\*.md` | Slash command definitions |
| `<install>\bridge\claude-code-plugin\hooks\forward.js` | Hook event forwarder |
| `<install>\.claude-plugin\marketplace.json` | Marketplace catalog |
| `~\.claude\plugins\cache\synaptic-disorder\synaptic-claude-code\<version>\` | Installed cache (read-only — edits get overwritten on next update) |
| `~\.claude.json` | User MCP registry (don't edit by hand — use `claude mcp add/remove`) |

---

That's it. If something's off, `claude plugin list -v` and `claude mcp list` are the two most useful diagnostic commands. The repo's `Dev/docs/dev/CODE_HANDOFF — *.md` files are the canonical specs for any backend endpoint your code hits — read those before assuming an endpoint shape.
