# Beta-Upgrade — runbook for AI agents

> **You are an AI agent. Your operator already has a Synaptic beta installed and wants to upgrade it to a newer beta.** This document is written *for you*, not for them. Read it top to bottom, then execute the steps you can execute. Each section is idempotent — re-running won't break anything.
>
> If the operator does **not** have an existing install, follow [`AGENT_INSTALL.md`](./AGENT_INSTALL.md) instead.
>
> If you are a human reading this, you probably want [`README.md`](./README.md). This document exists for the beta period only and will be removed at the v1.0 public release.

---

## TL;DR

1. Identify the operator's current version. Read `VERSION` in their existing install. If the file is missing, treat it as a pre-v2.0.0a archived snapshot — those used date-coded `vYY.M.D.<n>` versioning (the `26.5.6.4.2` zip in `Releases/Alpha/Old Beta/` is the canonical pre-consolidation snapshot). v2.0.0a (2026-05-08) retired the date scheme; everything from v2.0.0a forward uses semver `vM.F.P<suffix>` (current beta: `2.3.0b1`).
2. Stop the running stack (`docker compose down` from the operator's current install location).
3. Extract the new beta zip somewhere (anywhere — `~/Downloads/sd-update/` is fine; it's just a carrier).
4. Run the installer in update mode:
   - `python tools/install_to_appdata.py --update` if the operator already has an AppData install (anyone on 26.5.6.6+).
   - `python tools/install_to_appdata.py` (fresh install) if they're on a pre-26.5.6.6 layout, then manually copy their old `assets/memories.json` / `assets/synapses.json` / `assets/thought_bubbles.json` into the new install. Docker named volumes (`synaptic-bank`, `synaptic-ollama-tier1`) carry over automatically.
5. `cd "<install path printed by the installer>" && docker compose up -d`.
6. Verify with the §4 checks (`/healthz`, dashboard at `:9911`, auth gate, bridge live after a real adapter event).

If any step fails, do not loop. Report the failure to the operator with the exact step + error.

---

## 1. Detect the current version

```bash
cd "${SD_ROOT}"
test -f VERSION && cat VERSION || echo "no VERSION file — treat as 26.5.6.4.2 or earlier"
```

| Current version | Apply migration | What changed since |
|---|---|---|
| `2.3.0b1` | None — already current | (you're done) |
| `2.0.0a` … `2.2.x` (any post-v2 beta) | §3.1 (`--update` in place) | Cumulative beta deltas — schema migrations are forward-only and idempotent; `--update` preserves user data and Docker volumes |
| `26.5.6.6` (last date-coded with AppData install) | §3.1 | Adapter env-var additions, brand pivot to Synaptic, Phase 0b deep encoding, ATLAS overlay |
| `26.5.6.5.x` | §3.1 (small adapter env-var additions) | Bearer token + remote URL plumbing |
| `26.5.6.4.x` or earlier (incl. no VERSION file) | §3.0 (full consolidation) | 4 → 2 containers, server-side bubbles, auth, Bearer-token, single port 9911 |

If the operator is on v2.0.0a or newer (any AppData install), the answer is always §3.1 — `python tools/install_to_appdata.py --update` from a fresh extract of `synaptic-2.3.0b1.zip`. The `26.5.6.4.2` zip in `Releases/Alpha/Old Beta/` is the canonical pre-consolidation snapshot — start with §3.0 if that's what the operator has.

---

## 2. Stop the running stack and back up

Before replacing files:

```bash
cd "${SD_ROOT}"

# Stop whatever's running (handles both old 4-container and new 2-container layouts)
docker compose down

# OPTIONAL backup. If the operator has accumulated real memories or a live
# bank, capture it before files move.
cp -r assets/ /tmp/sd-backup-assets-$(date +%Y%m%d-%H%M%S)/ 2>/dev/null || true
docker volume inspect synaptic-bank >/dev/null 2>&1 && \
  docker run --rm -v synaptic-bank:/data -v /tmp:/backup alpine \
    tar czf /backup/sd-bank-$(date +%Y%m%d-%H%M%S).tgz -C /data . 2>/dev/null || true
```

Tell the operator where the backup landed. The Docker volumes (`synaptic-bank`, `synaptic-ollama-tier1`) are not destroyed by `docker compose down` and survive the upgrade — only the containers and images change.

---

## 3. Apply the version-specific migration

### 3.0. From `26.5.6.4.2` (or earlier) → current

This is the largest migration. The architecture changed. Key deltas:

- **Container count: 4 → 2 (default).** The previous stack had separate `synaptic-disorder-core`, `synaptic-disorder-dashboard` (nginx), `synaptic-disorder-ollama`, and `synaptic-disorder-ollama-init` containers. Current stack collapses to `synaptic-core` (Go binary that serves the dashboard, API, WebSocket, and bubble generator on a single port) and `synaptic-ollama-tier1` (with model-pull baked into entrypoint). Profile-gated `synaptic-ollama-tier2` and `synaptic-airllm-tier2` services may also start when their compose profiles are enabled in the dashboard's Services panel.
- **Port: 8765 → 9911.** The dashboard nginx is gone; SD Core serves index.html directly at `:9911` along with the API. Single port for everything except Ollama (still `:11434`).
- **In-browser Ollama → server-side bubble generator.** The dashboard used to call `localhost:11434` from the browser to generate thought bubbles, which broke any deployment where the browser couldn't see Ollama (anything remote). The bubble generator is now a Go goroutine inside SD Core, persisting to `assets/thought_bubbles.json` and broadcasting `bubble_added` events. Dashboard's `ServerBubblePool` fetches the snapshot at page load.
- **Optional Bearer-token auth.** SD Core honors `SD_API_TOKEN`. Empty = open mode (suitable for localhost only). Set this for any deployment exposed beyond localhost.
- **Adapter env vars: new options.** Every adapter now also honors `SD_CORE_URL` (full URL, preferred for remote — e.g. `https://cognito.example.com`) and `SD_API_TOKEN` (Bearer header on every outbound request). The legacy `SD_CORE_HOST` + `SD_CORE_PORT` still work when SD_CORE_URL is unset.

#### 3.0.1. Migrate to the AppData install (one-time)

The pre-26.5.6.6 layout ran the dashboard out of the operator's source folder (commonly `C:\Users\<name>\AI\Synaptic\` or `~/synaptic/`). The current layout puts the install in the per-user OS-standard data directory:

- **Windows:** `%LOCALAPPDATA%\Synaptic` (legacy: `%LOCALAPPDATA%\SynapticDisorder`)
- **macOS:** `~/Library/Application Support/Synaptic` (legacy: `…/SynapticDisorder`)
- **Linux:** `$XDG_DATA_HOME/synaptic` or `~/.local/share/synaptic` (legacy: `…/synaptic-disorder`)

If the operator's prior install is at the legacy path, the installer prompts before proceeding and points at `docs/dev/Legacy migration paths.md` § 1 for the manual move.

**Step 1: extract the new beta zip somewhere** (any temp location — `~/Downloads/synaptic-2.x/` is fine; current beta: `synaptic-2.3.0b1.zip`):

```bash
unzip /path/to/synaptic-2.x.zip -d ~/Downloads/synaptic-2.x/
cd ~/Downloads/synaptic-2.x/Prod   # the zip wraps Prod/
```

**Step 2: run the installer.** Fresh-install case (no prior AppData install):

```bash
python tools/install_to_appdata.py
```

Prints the resolved install path. Use that as the new `${SD_ROOT}` for the rest of this runbook.

**Step 3 (optional): import the operator's old user data.** Their existing `assets/memories.json`, `assets/synapses.json`, and `assets/thought_bubbles.json` live inside their pre-26.5.6.6 install directory. Copy them into the new install's `assets/`:

```bash
cp /path/to/old-install/assets/memories.json       "${SD_ROOT}/assets/"
cp /path/to/old-install/assets/synapses.json       "${SD_ROOT}/assets/"
cp /path/to/old-install/assets/thought_bubbles.json "${SD_ROOT}/assets/" 2>/dev/null || true
```

If the operator was using the SQLite bank and they had it host-mounted (rare in pre-26.5.6.6 setups), copy `sd_bank.db*` similarly. If it was in a Docker named volume (the default), it's already preserved automatically — Docker volumes survive `docker compose down`.

**Step 4: discard the old install.** Once the new AppData install is verified working in §4, the operator can delete the old source-tree install. The old Docker named volumes (`synaptic-bank`, `synaptic-ollama-tier1`) are still attached to the new install via the new compose file — same volume names — so user data carries over automatically.

The new install is **scrubbed by construction**: it ships no live `memories.json` or `synapses.json` (SD Core auto-creates empty stubs on first run). A synthetic example dataset (`memories.example.json` + `synapses.example.json`, fully randomized 168 memories / 857 synapses) ships alongside for opt-in demo seeding. The operator's real data only ends up in the new install if step 3 above runs.

#### 3.0.2. Update adapter envs on every device that talks to SD Core

For each device (Nomad Desktop's local adapters, RND-Laptop's adapter, etc.), update the adapter's environment:

**If everything is on one machine (no remote operation):** no env-var change needed. SD_CORE_HOST + SD_CORE_PORT defaults still work; auth defaults to open. The adapter will reach `127.0.0.1:9911` as before.

**If SD Core is now on a remote host (Cloudflare Tunnel or Tailscale):**

```bash
# In whatever shell or service file launches the adapter:
export SD_CORE_URL=https://cognito.example.com   # or your tailnet URL
export SD_API_TOKEN=<the same token SD Core was started with>
```

Per-client config-file edits (Claude Desktop's `claude_desktop_config.json`, Cursor's MCP settings, Continue's `~/.continue/config.json`, Gemini CLI's `~/.gemini/mcp_servers.json`, Goose's `~/.config/goose/config.yaml`, OpenCode's `~/.opencode/config.json`) need the new env block:

```json
{
  "mcpServers": {
    "synaptic": {
      "command": "node",
      "args": ["${SD_ROOT}/bridge/mcp-adapter/index.js"],
      "env": {
        "SD_CLIENT": "claude-desktop",
        "SD_MODEL": "claude-opus-4-7",
        "SD_CORE_URL": "https://cognito.example.com",
        "SD_API_TOKEN": "<token>"
      }
    }
  }
}
```

The host application must restart for the new MCP env to take effect. Tell the operator. **You cannot restart their host application from inside a session** — wait or end the runbook here pending their action.

#### 3.0.3. Generate / capture an API token

If the operator wants Bearer-token auth on:

```bash
openssl rand -hex 24
# Output: e.g. a3f2c8d4e9b0...
# Save in the operator's password manager.
```

Then write it into a `.env` next to `docker-compose.yml`:

```bash
echo "SD_API_TOKEN=<the token>" > "${SD_ROOT}/.env"
chmod 600 "${SD_ROOT}/.env"
```

Same value goes into every adapter's environment (see §3.0.2).

If the operator doesn't want auth (localhost-only deployment), skip this step. SD Core defaults to open mode when `SD_API_TOKEN` is unset.

#### 3.0.4. Bring up the new stack

```bash
cd "${SD_ROOT}"
docker compose up -d
```

Watch startup logs:

```bash
docker compose logs -f --tail 30 core
# Look for these lines:
#   bank: opened SQLite store at /var/lib/sd/sd_bank.db
#   memory watcher: watching /data/assets/memories.json
#   auth: SD_API_TOKEN set — Bearer token required on API endpoints   (or "unset — open mode")
#   bubble: starting generator (model=llama3.2:3b, every 30s, ollama=http://ollama:11434, ...)
#   Synaptic Core listening on 0.0.0.0:9911
```

Ollama's model pull on first run takes several minutes (~2 GB for `llama3.2:3b`). The dashboard works immediately though — region classification falls back to keyword-only and bubble generation is suspended until the model lands.

### 3.1. From v2.0.0a or newer (any prior AppData install) → current

If the operator already has an AppData install from a previous beta, updating is a single command:

```bash
unzip /path/to/SynapticDisorder_Beta.zip -d ~/Downloads/sd-update/
cd ~/Downloads/sd-update/Prod
python tools/install_to_appdata.py --update
```

The `--update` flag refreshes everything in the install (binaries, dashboard, adapters, tools, docs) but **preserves user data**: `assets/memories.json`, `assets/synapses.json`, `assets/thought_bubbles.json`, and `env`. Docker named volumes are untouched.

After the script finishes:

```bash
cd "${SD_ROOT}"   # the install root the script printed
docker compose down && docker compose up -d
```

That picks up the new SD Core image / refreshed compose config / any updated adapters. Since v2.0.0a, the compose-file shape only adds profile-gated services (`synaptic-ollama-tier2`, `synaptic-airllm-tier2`) when their Config toggles flip on — `--update` carries those through automatically without touching always-on services or user data.

Adapter env-var changes (§3.0.2) only apply if the operator is moving to remote operation. Local-only setups keep working without env changes.

The temp unzip directory (`~/Downloads/sd-update/`) can be deleted after a successful update — it's just the carrier for the installer.

---

## 4. Verify the upgrade

Whichever migration you ran, verify the same way:

```bash
# 1. Single-port dashboard responds
curl -fsS http://localhost:9911/healthz | jq '.status'
# Expect: "ok"

# 2. Dashboard HTML serves
curl -fsS -o /dev/null -w "HTTP %{http_code}\n" http://localhost:9911/
# Expect: HTTP 200 (and the response body should contain SD_VERSION = '<current>')

# 3. Auth gate (only if you set SD_API_TOKEN):
curl -fsS -o /dev/null -w "no-token: HTTP %{http_code}\n" http://localhost:9911/events
# Expect: HTTP 401

curl -fsS -o /dev/null -w "with-token: HTTP %{http_code}\n" \
  -H "Authorization: Bearer $SD_API_TOKEN" http://localhost:9911/events
# Expect: HTTP 200

# 4. Container count matches the new architecture
docker compose ps
# Expect at least 2 services running (synaptic-core + synaptic-ollama-tier1)
# plus profile-gated synaptic-ollama-tier2 / synaptic-airllm-tier2 if enabled.
# If you see 4 (core, dashboard, ollama, ollama-init), the compose file
# wasn't replaced — re-run §3.0.1.

# 5. Open the dashboard
# In the operator's browser: http://localhost:9911 (or the remote URL).
# - HUD bottom-right shows "v<current version>"
# - MODEL line says "MOCK" until a real adapter fires; then the adapter's model
# - Once a real-agent event arrives, mock locks out for the rest of the session
```

---

## 5. Rollback (if the upgrade went sideways)

```bash
cd "${SD_ROOT}"
docker compose down

# Restore the file backup you made in §2 (if any):
# (operator's choice — they know whether they kept the previous tree)

# Restore the bank backup:
docker run --rm -v synaptic-bank:/data -v /tmp:/backup alpine \
  tar xzf /backup/sd-bank-<timestamp>.tgz -C /data

# Bring up the old stack:
docker compose up -d
```

Volumes survive `docker compose down`, so if the issue is purely with the new container image (e.g., the new compose file references a missing service), `docker compose down` + `git checkout <previous-version>` + `docker compose up -d` is enough — no data restore needed.

---

## 6. Reporting back

Send the operator a single status message in this shape:

```
Synaptic upgrade: <DONE | PARTIAL | FAILED>

Previous version:  <e.g. 26.5.6.4.2>
Current version:   <e.g. 26.5.6.6>
Migration applied: <§3.0 / §3.1 / etc.>
Auth enabled:      <yes (token in .env) | no (open mode)>
Adapter restart needed: <yes — operator must restart {host application(s)} | no>
Backup location:   </tmp/sd-backup-... and /tmp/sd-bank-...>

Notes:
  - <one line per surprise / config edit / non-default port / etc.>
```

If `Restart needed: yes`, end your message with: "I'll be able to confirm event flow on my next session start — restart `<host application>` and run any tool call to verify."

---

## 7. Things you must not do during an upgrade

- **Do not delete `synaptic-bank` or `synaptic-ollama-tier1` volumes.** Both contain user data. The `docker compose down` already preserves them; a `down --volumes` would nuke them. Never use that flag.
- **Do not commit any changes to source files.** This is an upgrade, not a code change. Even if `git diff` shows untracked files (their `.env` for example), do not stage them.
- **Do not rotate the operator's API token without asking.** If they had auth enabled, keep using their existing token.
- **Do not change the operator's dashboard URL configuration in localStorage.** That belongs to their browser. If migrations require a URL change (e.g. they were on `:8765` and need `:9911`), tell them to update via the Config panel's Bridge section, don't try to do it for them.
- **Do not loop on a failing step.** `docker compose up` failing once is information; failing three times is wasted compute. Capture and report.

---

## 8. Reference for what's in the new beta

| Component | What it does | Endpoint |
|---|---|---|
| SD Core (Go) | Event hub + WebSocket fan-out + SQLite memory bank + dashboard static-file server + bubble generator | `http://localhost:9911` |
| Ollama | Local LLM for region classifier + thought bubbles (optional) | `http://localhost:11434` |
| OTel receiver | OTLP/HTTP collector translating spans into SD events (optional, run separately) | `http://localhost:4318` |
| Claude Code plugin | Forwards Claude Code hooks to SD Core | n/a (in-process) |
| MCP adapter | Stdio MCP server with `report_event` + `report_memory_save` tools | n/a (stdio) |

Schemas: [`docs/event-schema.md`](docs/event-schema.md), [`docs/memory-schema.md`](docs/memory-schema.md).

Architecture deep-dive references:
- [`bridge/core/README.md`](bridge/core/README.md)
- [`bridge/claude-code-plugin/README.md`](bridge/claude-code-plugin/README.md)
- [`bridge/mcp-adapter/README.md`](bridge/mcp-adapter/README.md)
- [`bridge/otel-adapter/README.md`](bridge/otel-adapter/README.md)
- [`bridge/sdk/README.md`](bridge/sdk/README.md)

Remote operation: [`docs/REMOTE.md`](docs/REMOTE.md). Per-client integration matrix: [`docs/INTEGRATIONS.md`](docs/INTEGRATIONS.md). Data handling reference: [`docs/data_privacy.md`](docs/data_privacy.md).

---

*End of upgrade runbook. If the operator's brain is firing on the new container set, you're done — welcome them back to the dashboard.*
