# Cloudflare Bridge — setup guide

This page walks through bridging a remote Synaptic dashboard through Cloudflare Tunnel so any device on the internet can reach the SD Core API + WebSocket living on your home machine. If you only need the high-level "Cloudflare Tunnel or Tailscale?" framing, read [`REMOTE.md`](REMOTE.md) first; come back here when you're ready to wire the bridge.

The setup is two hostnames pointing at the same `cloudflared` process: one for the dashboard (`synaptic.<your-domain>`) and one for the API + WebSocket (`cognito.<your-domain>`). Cloudflare Access can sit in front of either one independently. You'll be looking at the same brain on your phone from the coffee shop as your desktop is rendering at home.

> **Looking for the engineer-side pre-flight checklist?** Skip to [§9 — When SD Core adds a new path family](#9-when-sd-core-adds-a-new-path-family). The full backend procedure lives in [`docs/dev/Pinned.md`](dev/Pinned.md).

---

## 1. Why bridge through Cloudflare

A few reasons people pick Cloudflare Tunnel for this:

- **No port forwarding.** The tunnel is an outbound TLS connection from your home machine to Cloudflare's edge. Nothing on your router changes, no public IP needed, and devices on hostile networks (hotel Wi-Fi, school networks, corporate guest LANs) reach the bridge just fine because everything is `https://`.
- **One token works everywhere.** SD Core's `SD_API_TOKEN` is the same byte string on every adapter you wire up — laptop, phone, the agent running in CI. No per-device key management.
- **Cloudflare Access for human SSO, Bearer token for machines.** You can put `synaptic.<your-domain>` (the dashboard) behind interactive SSO so only your email lights it up, while leaving `cognito.<your-domain>` (the API) Bearer-token-only so adapters don't need an OAuth dance. Free tier covers up to 50 users.
- **You stay in control of the data.** Cloudflare sees encrypted TLS to its edge, decrypts at the edge, re-encrypts to the tunnel. They have transient access to plaintext at the edge layer the same way any reverse proxy does. If you want zero edge visibility, use Tailscale instead — [`REMOTE.md`](REMOTE.md) covers that path.

---

## 2. Prerequisites

- A Cloudflare account with a domain you control (or use a Cloudflare-provided `*.cfargotunnel.com` hostname).
- SD Core running locally on `127.0.0.1:9911` with `SD_API_TOKEN` set in its `.env`. You generated the token with `openssl rand -hex 24` and stored it in your password manager.
- `cloudflared` installed on the same machine that runs SD Core:
  - macOS: `brew install cloudflared`
  - Windows: `winget install --id Cloudflare.cloudflared` (installs as a system service under `C:\Program Files (x86)\cloudflared\`)
  - Linux: <https://github.com/cloudflare/cloudflared/releases/latest>
- About 10 minutes.

---

## 3. SD Core path families — the comprehensive table

This is the load-bearing reference. Every top-level path family SD Core serves is listed below, with what it does, whether it needs the Bearer token, whether the WebSocket upgrade is involved, who uses it, and any setup notes that affect ingress.

| Path prefix | What it serves | Auth required? | WebSocket? | Used by | Notes |
|---|---|---|---|---|---|
| `/healthz` | Liveness probe. Returns `{"status":"ok",...}`. | No (intentional exempt) | No | Smoketests, CF Tunnel health checks, the dashboard's own bridge-state probe | Always exempt from auth so monitoring can run unauthenticated. Used by `cognito.<your-domain>` for "is the backend reachable?" |
| `/event` | Single-event ingest. POST a v1.0 event envelope. | Yes | No | Adapters (Claude Code plugin, MCP server, OTel adapter, SDKs) | The high-volume endpoint when an agent is firing |
| `/events` | Event ring-buffer reads + replay. GET recent events. | Yes | No | Dashboard cold-load + reconnects, smoketests | The dashboard fetches the last 50 events on reconnect |
| `/memories` | Memory CRUD, list, region/lifecycle filters. Includes `/memories/coverage` for live embedding + deep-encoding coverage. | Yes | No | Dashboard, MCP `sd_*` tools | Coverage subpath deliberately lives under `/memories/*` (not `/admin/*`) so Access policies that gate `/admin/*` with SSO don't break the dashboard's coverage chip |
| `/bank/` | SQLite fallback bank — `/bank/memory`, `/bank/memories`, `/bank/memories/{id}/...` | Yes | No | Adapters writing memories, Claude Code hooks | The `/{id}/...` subtree is where Phase 5 CRUD lives (PATCH / DELETE / restore / lifecycle / dirty / supersede). `POST /bank/memory` is rate-limited per `memory_write_rate_limit_per_min` (default unlimited; returns `429 + Retry-After` when over-cap — v2.5.0b1+). The `{id}/supersede` sub-resource is the R10 bi-temporal entry point — v2.4.0b1+. |
| `/recall` | Semantic search — `/recall` (memories), `/recall/maps` (MemoryMaps via hybrid name + schema-cosine + member-aggregate ranking). | Yes | No | Adapters (MCP `sd_recall`, `sd_recall_maps`, dashboard search) | `/recall/maps` is the canonical "Can you recall the project X" path — clean name-match always wins over fuzzy semantic. Added v2.5.0b1+. |
| `/reflect` | Tier 3 synthesis over a recall result set. POST returns a single composed answer. New in v2.4.0b1. | Yes | No | Dashboard reflect surface, MCP `sd_reflect` (planned) | Adding `/reflect` is the canonical example of "new top-level path requires an ingress regex edit" — see [§9](#9-when-sd-core-adds-a-new-path-family) |
| `/maps` | MemoryMap CRUD — `/maps`, `/maps/{id}`, `/maps/{id}/associations`, `/maps/{id}/traces`, `/maps/propose`, `/maps/cleanup`, `/maps/{id}/accept`, `/maps/{id}/dismiss`, `/maps/{id}/research` | Yes | No | Dashboard Maps tab, MCP `sd_propose_map` | Sub-paths all match because the regex uses `(/.*)?$`. The `propose`/`accept`/`dismiss` triplet is the R13 map-proposal pattern. `cleanup` is the bulk-prune endpoint with dry-run preview. `{id}/research` is the map-scoped Tier 3 path — runs Oracle research using the map's anchor tags + members as context, caches the result, AND ingests a new memory tagged with the map's anchors so the dream pipeline picks it up. Added v2.4.0b1–v2.5.0b1+. |
| `/lexicon` | Tag-pair lexicon — `/lexicon`, `/lexicon/pairs`, `/lexicon/rebuild` | Yes | No | Dashboard Lexicon tab | Frequency table + pairs view |
| `/audit` | Audit log reads. Filters by entity, operation, since. | Yes | No | Dashboard Audit tab, smoketests | Sensitive-flagged ops are redacted server-side |
| `/research` | Tier 3 Oracle research — list / fetch / store / delete. | Yes | No | Dashboard Research tab, MCP `sd_research` | Sensitive memories are filtered out of every Tier 3 call by default |
| `/budget` | Per-tier token budget reads + writes. | Yes | No | Dashboard token-chip popover, MCP `sd_get_budget` | Per-model breakdown shipped in v2.3.0b1 |
| `/nightly/` | Nightly dream cycle — `/nightly/runs`, `/nightly/runs/{id}`, `/nightly/trigger`, `/nightly/run` | Yes | No | Dashboard Dream Journal, manual trigger button | Trailing-slash variant covers `/{id}` item routes incl. DELETE |
| `/settings` | Settings CRUD — `/settings`, `/settings/{key}`, `/settings/providers/{tier}`. Also serves cross-device theme sync via `/settings/sd:themes:custom` (Custom-slot seeds + overrides + variant) and `/settings/sd:themes:user` (named custom themes). | Yes | No | Dashboard Config tab, settings hydration on boot, ThemeForge cross-device sync | `PUT /settings/providers/{tier}` hot-swaps the tier live (v2.3.0b1+). `PUT /settings/sensitive_classifier_prompt` hot-swaps the Layer 2 sensitivity classifier prompt within ~1s — no restart (v2.5.0b1+). Theme sync uses reserved `sd:themes:*` keys so the user's ThemeForge customizations follow them across devices. |
| `/admin/` | Admin-grade operations — env_write, env_check, network, embed-all, deep-encode-all, router/reload, docker, services, airllm, llm, sensitive/reclassify, sensitive/default_prompt | Yes | No | Dashboard Config / Diagnostics surfaces | This is the prefix that you may want behind Cloudflare Access SSO for an extra gate — see [§5](#5-cloudflare-access-policies-the-admin-oauth-gotcha). `sensitive/reclassify` (v2.5.0b1+) bulk-re-runs the Layer 2 AI sensitivity classifier with the (possibly user-customized) prompt; `sensitive/default_prompt` is a read-only GET that returns the hardcoded default so the Privacy UI's "Reset to default" can re-populate without shipping a duplicate string in the frontend. |
| `/ws` | WebSocket upgrade. Hub fans out events to connected dashboards. | Yes (via `?token=` query param — browsers can't set headers on WS) | **Yes** | Dashboard live feed, smoketest WS connect | The single WS endpoint; needs `noTLSVerify: false` on the tunnel ingress and explicit upgrade support on Cloudflare Access if you've gated it |
| `/bubbles` | Thought-bubble pool — `/bubbles/generate`, `/bubbles/mutate` | Yes | No | Dashboard bubble pool, server-side bubble generator's regen calls | Pool snapshot is also served as a static file under `/assets/thought_bubbles.json` on the dashboard hostname |
| `/patients` | Patient registry — `/patients` (list/create), `/patients/{id}` (DELETE), `/patients/{id}/activate` (Bank.Reopen). New in v2.8. | Yes | No | Dashboard Patient picker (header chip + login overlay) | Multiple isolated memory banks under one running core. `activate` swaps the live bank; the patient registry persists across container restarts under `/data/patients/`. Adding `/patients` is the most recent example of "new top-level path requires an ingress regex edit" — added to canonical regex 2026-05-15. |
| `/import` | Bulk memory import (CSV / JSON). | Yes | No | Dashboard Import dialog | Streams the upload through to bulk-insert + auto-embed. Added v2.4.0b1. |
| `/procedures` | Procedural-memory CRUD. | Yes | No | Dashboard Procedures tab | Procedure templates that can be invoked from MCP. Added v2.5.0b1+. |
| `/entities` | Named-entity index — `/entities`, `/entities/{name}`. | Yes | No | Dashboard Entity chips, MCP `sd_get_entity` | Entity aggregates pull from Phase 0b extraction. Added v2.5.0b1+. |

Two more families exist on the backend but **do not** appear in the canonical `cognito.<your-domain>` regex (intentional or noted):

| Path prefix | Status | Why it's not in the regex |
|---|---|---|
| `/VERSION`, `/assets/`, `/`, `/index.html`, `/favicon.ico` | **Intentionally on the dashboard hostname only.** | These are static dashboard paths. They're served by SD Core but the `synaptic.<your-domain>` ingress entry catches them with no path filter; the API hostname `cognito.<your-domain>` excludes them so they 404 cleanly at the edge instead of burning SD Core CPU on rejection. |
| `/session/` (`/session/dirty`, `/session/clear/`) | **Currently NOT covered by the cognito regex.** | This appears to be dashboard-side only (the dashboard talks to its own loopback for session ops). If a remote adapter ever needs `/session/*`, the regex must be updated. Documented here so the gap is visible. |

---

## 4. The canonical cloudflared ingress regex

This is the live regex as of v2.8 (after `/patients`, `/procedures`, `/entities`, `/import` were added 2026-05-15):

```regex
^/(event|events|memories|bank|recall|reflect|import|maps|lexicon|audit|research|budget|nightly|settings|admin|ws|bubbles|healthz|patients|procedures|entities)(/.*)?$
```

Walking through the parts:

- `^/` — match from the start of the path
- `(event|events|memories|…|entities)` — one of the 20 allowed top-level prefixes
- `(/.*)?$` — optional sub-path, anchored to end. Catches `/memories/coverage`, `/maps/{id}/associations`, `/admin/router/reload`, etc., without listing each sub-path explicitly

If you're starting a fresh tunnel today, this is what to paste. If a future SD Core minor adds a new top-level family, you'll edit the `(event|events|...)` alternation list to include it. [§9](#9-when-sd-core-adds-a-new-path-family) walks the engineer's side of that change.

---

## 5. Cloudflare Access policies — the `/admin/*` OAuth gotcha

Cloudflare Access is the SSO layer that sits in front of a hostname and gates it by identity provider (email-OTP, Google, GitHub, Okta, etc.). The standard setup for Synaptic is:

- `synaptic.<your-domain>` → behind Access. Your email (and anyone you grant) sees the dashboard; randoms hitting the URL get bounced to the SSO page.
- `cognito.<your-domain>` → Access-free. Adapters connect with just the Bearer token. Putting Access in front of the API hostname would force every machine-to-machine call to handle an OAuth redirect — adapters can't do that.

**The `/admin/*` gotcha.** Some users want to gate the `/admin/*` paths behind Access while keeping the rest of the API bearer-only. The cleanest way is **a third hostname** (e.g. `cortex.<your-domain>`) that only matches `/admin/`, gated by Access:

```yaml
- hostname: cortex.your-domain.com
  path: ^/admin/.*$
  service: http://localhost:9911
- hostname: cortex.your-domain.com
  service: http_status:404
```

…then point the dashboard's Config → Bridge URL at `cortex` only when you want admin ops. The reason this matters: if you try to gate `/admin/*` on `cognito.<your-domain>` directly, every admin endpoint call from the dashboard becomes a redirect-to-SSO call (because Access doesn't know the dashboard already has a valid Bearer token in its `Authorization` header). The bearer-token machine path and the SSO human path don't compose on the same hostname.

`/memories/coverage` was deliberately moved out from `/admin/memory/coverage` in v2.3.0b1 for exactly this reason: putting it under `/memories/*` means it survives a `synaptic.<your-domain>`-with-Access setup even when `/admin/*` is gated behind Access.

---

## 6. WebSocket considerations

The `/ws` endpoint is the one place the bridge has to handle a protocol upgrade. Three things to know:

1. **Tunnel ingress passes WebSocket through by default.** No flag needed on the ingress entry; `cloudflared` handles `Upgrade: websocket` natively.
2. **Cloudflare Access on the WS hostname requires service-token auth, not interactive SSO.** Browsers can't follow an SSO redirect for a `wss://` request — the WS handshake is a single round trip. If you want auth on `/ws`, leave Cloudflare Access off `cognito.<your-domain>` and rely on the Bearer token via query string (`wss://cognito.<your-domain>/ws?token=<token>`). SD Core honors the `?token=` query param exactly because browsers can't set headers on WS.
3. **Cloudflare's WebSocket timeout is 100 seconds of idle by default.** Synaptic's WS hub sends a ping every 30s so this never triggers, but if you build a custom adapter that goes quiet for >100s, expect to reconnect.

---

## 7. Worked example — wiring `synaptic.your-domain.com` end-to-end

This is the full walkthrough. Substitute `your-domain.com` for your actual domain everywhere.

### 7a. Generate the token and write the `.env`

```bash
# In whatever shell you've got, on the machine that runs SD Core:
openssl rand -hex 24
# Output: a3f2c8d4e9b0...   ← keep this private, like a password

# Write it into the .env next to docker-compose.yml:
cd "<install root>"   # e.g. %LOCALAPPDATA%\Synaptic on Windows
echo "SD_API_TOKEN=a3f2c8d4e9b0..." >> .env
```

Bring SD Core up (or restart it):

```bash
docker compose up -d
# Startup log should include: "auth: SD_API_TOKEN set — Bearer token required"
```

Probe locally to verify the token is wired:

```bash
curl -fsS http://localhost:9911/healthz
# Expect: {"status":"ok",...}

curl -fsS -o /dev/null -w "no-token: HTTP %{http_code}\n" http://localhost:9911/events
# Expect: HTTP 401

curl -fsS -o /dev/null -w "with-token: HTTP %{http_code}\n" \
  -H "Authorization: Bearer a3f2c8d4e9b0..." http://localhost:9911/events
# Expect: HTTP 200
```

### 7b. Authenticate cloudflared and create the tunnel

```bash
cloudflared tunnel login          # browser opens; pick your domain
cloudflared tunnel create synaptic
# Output: Created tunnel synaptic with id c5d4f03c-36b1-4f30-be47-00f061d8c745
# Credentials file written to: C:\Users\<you>\.cloudflared\<tunnel-id>.json
```

The credentials file is sensitive (it authenticates the tunnel to Cloudflare). Don't commit it; back it up to your password manager.

### 7c. Write the `config.yml`

The file lives at `~/.cloudflared/config.yml` (or `%USERPROFILE%\.cloudflared\config.yml` on Windows). On Windows, you'll **also** need to mirror it into the system-profile path before the service can read it — see §7e below.

```yaml
tunnel: c5d4f03c-36b1-4f30-be47-00f061d8c745
credentials-file: C:\Users\<you>\.cloudflared\c5d4f03c-36b1-4f30-be47-00f061d8c745.json

ingress:
  # Dashboard — no path filter; serves the static shell + assets + /VERSION
  - hostname: synaptic.your-domain.com
    service: http://localhost:9911

  # API + WebSocket — restricted to the 16 known top-level path families
  - hostname: cognito.your-domain.com
    path: ^/(event|events|memories|bank|recall|reflect|import|maps|lexicon|audit|research|budget|nightly|settings|admin|ws|bubbles|healthz|patients|procedures|entities)(/.*)?$
    service: http://localhost:9911

  - hostname: cognito.your-domain.com
    service: http_status:404

  # Catch-all
  - service: http_status:404
```

If you're using a Cloudflare-provided `*.cfargotunnel.com` hostname instead of your own domain, replace the hostnames accordingly — the rest of the config is identical.

### 7d. Route DNS

```bash
cloudflared tunnel route dns synaptic synaptic.your-domain.com
cloudflared tunnel route dns synaptic cognito.your-domain.com
```

This writes CNAME records pointing at `<tunnel-id>.cfargotunnel.com`. Propagation is usually instant within Cloudflare's edge.

### 7e. Install as a system service (Windows-specific gotcha)

On Linux/macOS, `cloudflared service install` reads the `~/.cloudflared/config.yml` you just wrote and you're done.

On Windows, the system service reads from `C:\Windows\System32\config\systemprofile\.cloudflared\` — **not** your user profile. The user-profile config you wrote in §7c is the source of truth, but it has to be mirrored to the system-profile path before the service can read it. Two ways:

```powershell
# Open an elevated PowerShell (Run as Administrator), then:
mkdir "C:\Windows\System32\config\systemprofile\.cloudflared" -Force
Copy-Item "$env:USERPROFILE\.cloudflared\config.yml" `
  "C:\Windows\System32\config\systemprofile\.cloudflared\config.yml" -Force
Copy-Item "$env:USERPROFILE\.cloudflared\<tunnel-id>.json" `
  "C:\Windows\System32\config\systemprofile\.cloudflared\<tunnel-id>.json" -Force
cloudflared service install
Restart-Service Cloudflared
```

Or install once and edit the system-profile config in place going forward. **Every time you change the ingress, repeat the mirror step (or edit the system-profile copy directly) and restart the service.** This is the #1 cause of "I edited the regex but it's still 404ing" — the service is reading the old system-profile config that doesn't reflect your edit.

### 7f. Wire your adapter

Pick whichever adapter your tool uses (see [`INTEGRATIONS.md`](INTEGRATIONS.md) for the per-client matrix). For an MCP server, set the env block to:

```json
{
  "env": {
    "SD_CORE_URL": "https://cognito.your-domain.com",
    "SD_API_TOKEN": "a3f2c8d4e9b0..."
  }
}
```

The adapter now POSTs to `https://cognito.your-domain.com/event`, `…/recall`, etc. and the dashboard at `https://synaptic.your-domain.com` will light up.

### 7g. (Optional) Put the dashboard behind Cloudflare Access

In the Cloudflare dashboard:

1. Zero Trust → Access → Applications → Add an Application → Self-hosted
2. Application domain: `synaptic.your-domain.com`
3. Identity provider: One-time PIN (email) is the no-extra-config option. Google/GitHub/Okta are one-click.
4. Policy: "Allow if email matches `you@example.com`"

Open `https://synaptic.your-domain.com` in a browser — the Access OTP page appears once per session. Adapter calls to `cognito.<your-domain>` still go through with just the Bearer token because that hostname stays Access-free.

---

## 8. Verifying the bridge

After the steps above, run these probes from any machine **not** on your home LAN to confirm the bridge works end-to-end. A coffee-shop laptop, your phone on cellular, a cloud VM — anything off-network.

```bash
# 1. Liveness — always-exempt path, no token needed
curl -fsS https://cognito.your-domain.com/healthz
# Expect: {"status":"ok",...}

# 2. Auth gate — no token should 401
curl -fsS -o /dev/null -w "no-token: HTTP %{http_code}\n" \
  https://cognito.your-domain.com/events
# Expect: HTTP 401

# 3. Auth gate — correct token should 200
curl -fsS -o /dev/null -w "with-token: HTTP %{http_code}\n" \
  -H "Authorization: Bearer <your-token>" \
  https://cognito.your-domain.com/events
# Expect: HTTP 200

# 4. Recall round-trip
curl -fsS -X POST \
  -H "Authorization: Bearer <your-token>" \
  -H "Content-Type: application/json" \
  -d '{"query":"test","limit":1}' \
  https://cognito.your-domain.com/recall
# Expect: JSON with a "results" array (possibly empty if bank is empty)

# 5. WebSocket — handshake should upgrade
curl -fsS -i -N \
  -H "Connection: Upgrade" -H "Upgrade: websocket" \
  -H "Sec-WebSocket-Key: dGVzdGluZyBzZWMtd2Vic29ja2V0LWtleQ==" \
  -H "Sec-WebSocket-Version: 13" \
  "https://cognito.your-domain.com/ws?token=<your-token>" 2>&1 | head -5
# Expect: HTTP/1.1 101 Switching Protocols
```

Then open `https://synaptic.your-domain.com` in a browser. The dashboard loads. The HUD's BRIDGE chip flips green within ~2 seconds of any adapter firing an event.

### Common failure modes

| Symptom | Likely cause | Fix |
|---|---|---|
| Dashboard 404 at `https://synaptic.your-domain.com` | DNS not propagated, or tunnel not running | `dig synaptic.your-domain.com` should resolve to `<tunnel-id>.cfargotunnel.com`. `Get-Service Cloudflared` on the host should show Running. |
| `cognito.<your-domain>/<path>` returns 404 but `http://localhost:9911/<path>` returns 200 | The ingress regex doesn't include `<path>` | Check §3 — if the path family isn't in the regex (and isn't a static dashboard asset), edit the system-profile config and `Restart-Service Cloudflared`. See [§9](#9-when-sd-core-adds-a-new-path-family). |
| Edited the regex, still 404 | The user-profile config was edited, not the system-profile one | On Windows, mirror to `C:\Windows\System32\config\systemprofile\.cloudflared\config.yml` and restart the service. The service does not read the user-profile file. |
| `wss://cognito.../ws` returns 401 | Token is wrong, or you forgot the `?token=` query param | Browsers can't set the `Authorization` header on WebSocket. SD Core accepts `?token=<value>` as a fallback. Double-check the token matches what's in `.env`. |
| Dashboard 401s instantly on load | `synaptic.<your-domain>` is behind Cloudflare Access, but you haven't completed the SSO login this session | Visit the URL in a fresh tab and complete the email/OAuth prompt. |
| Some `/admin/*` calls succeed, others 503 | The admin endpoint hits an external dependency (Docker socket, AirLLM sidecar) that isn't reachable | Check `docker compose ps` on the host. Endpoint-specific; not a tunnel issue. |
| `https://cognito.../healthz` 200 OK but the dashboard's BRIDGE chip is red | The dashboard's localStorage is pointing at the wrong URL or has a stale token | Open Config → Bridge in the dashboard and re-paste the URL + token. The dashboard re-binds live without reload. |

---

## 9. When SD Core adds a new path family

A new top-level path family (something that doesn't fit under any of the 16 prefixes in §3) requires editing the ingress regex on the host before remote access works. This is a host-side operation, not a code change. The most recent example is `/reflect` for v2.4.0b1 — backend shipped the route handler, then the regex needed `reflect` added to the alternation list before the path responded over the tunnel.

The rule:

> Any new top-level path family must either (a) fit under an existing prefix in §3, OR (b) require a cloudflared ingress regex update on the host before remote access works.

The procedure when (b) applies:

1. **Open an elevated shell** on the machine running `cloudflared`.
2. **Edit the system-profile `config.yml`** (Windows: `C:\Windows\System32\config\systemprofile\.cloudflared\config.yml`; Linux/macOS: `/etc/cloudflared/config.yml` or wherever `cloudflared service install` placed it).
3. **Add the new prefix to the alternation list** in the `cognito.<your-domain>` regex. Example: adding `imports` would make the list `(event|events|...|bubbles|healthz|imports)`.
4. **Restart the service:** `Restart-Service Cloudflared` on Windows, `sudo systemctl restart cloudflared` on Linux, `sudo launchctl kickstart -k system/com.cloudflare.cloudflared` on macOS.
5. **Probe:** `curl -H "Authorization: Bearer <token>" https://cognito.<your-domain>/<new-path>` should return 200, not 404.
6. **Mirror the change back to the user-profile config** if you edit the system-profile one in place, so the source of truth stays consistent across both copies.

### For engineers shipping a new path family

The pre-flight checklist for the engineer side of this change lives in [`docs/dev/Pinned.md`](dev/Pinned.md) — section **"Before shipping a new top-level path family."** That checklist is what backend agents run through before declaring a feature ready for remote smoketest. The summary is: register the route in `bridge/core/main.go`, then either (a) confirm the new path fits an existing prefix or (b) tell Anthony the regex needs a host-side edit before remote smoketest will pass.

---

## What's next

- Integration with external memory backends is documented in [`MEMORY_AUTHORING_GUIDE.md`](MEMORY_AUTHORING_GUIDE.md) — relevant if you're bridging Synaptic with a separate memory store.
- For Tailscale as an alternative bridge (no public hostname, no Cloudflare account needed), see [`REMOTE.md`](REMOTE.md) Path B.
- If something in this guide produced a different result on your setup, please open an issue or note it in the smoketest review — the failure modes table in §8 is where corrections land.
