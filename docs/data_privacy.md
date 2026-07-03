# Data privacy

How Synaptic handles your data, where it lives, and what (if anything) leaves your machine. Designed to be a precise, complete reference rather than a marketing summary — read this once and you'll know exactly what you're trusting.

## TL;DR

- **Default mode is localhost-only.** SD Core binds to `127.0.0.1:9911`. Adapters POST to `127.0.0.1`. Ollama runs locally. Nothing leaves the machine you installed it on.
- **No telemetry, no analytics, no phone-home.** This project never sends your data to Anthropic, the project author, or any third-party telemetry service.
- **One unavoidable third-party network call:** the dashboard's HTML pulls Three.js (and a few JS helpers) from `cdnjs.cloudflare.com`. Cloudflare's edge can see that your browser fetched those static asset URLs. Self-hosting alternative documented below.
- **You opt in to remote operation explicitly.** Cloudflare Tunnel or Tailscale extends reachability to other machines you control; the project doesn't do this without your configuration.

---

## What data exists

The system handles five kinds of data. Some are sensitive (memories), some are derived (bubbles), some are operational (the bank, the event ring buffer).

### 1. Memory records (`assets/memories.json`)

The biggest sensitive surface — when populated. A JSON array of memory records emitted by whichever provider you use (Markdown folders, JSON files, imports from external memory tools, or `memory_added` events from a connected AI). Contains:

- `id` — stable identifier
- `text` — full memory text (sentences, full content)
- `tags` — array of strings
- `created_at` / `updated_at` — ISO timestamps
- `region_hint` — anatomical region key, when the classifier ran
- `adapter_id` / `session_id` — which adapter wrote it

**Public release builds (`Prod/`) ship with NO `memories.json` and NO `synapses.json`.** They are user data, not source code, and the project doesn't include anyone else's. On first start, SD Core writes empty stubs (`{"items": []}`, `{"edges": []}`) so the dashboard's `/memories` endpoint always succeeds and the file watcher has something to diff against. Content accretes from there only as you populate it via your adapter / memory provider.

A fully synthetic seed (`assets/memories.example.json` + `assets/synapses.example.json`) is bundled for reference, for the static Pages demo, and as opt-in starter content. Running `python tools/seed_with_example.py` copies it into place so the dashboard launches populated. The seed carries a top-level `"synthetic": true` flag; the moment your first real (non-`sd-core-*`) adapter fires a `memory_added` event, SD Core auto-wipes the seed and the brain transitions cleanly to your real activity.

**Where it lives:** `assets/memories.json` on the host filesystem (after population). Mounted into SD Core's container as `/data/assets/memories.json`. Read by the dashboard at boot (via SD Core's `/memories` endpoint) and watched for changes by SD Core's filesystem watcher.

**Dev tree caveat:** if you're working from a Dev fork that has accumulated real memory data, that file IS your bank. Don't commit it. `tools/build_release.py` strips `memories.json` from the Prod artifact regardless of what's in Dev, so a build-then-commit-from-Prod workflow is safe — but a `git add .` against Dev would catch the live file. Always commit from `Prod/`, never from `Dev/`.

### 2. Activity events (in-memory ring buffer + WS broadcasts)

The real-time event stream — what flows from adapters → SD Core → connected dashboards. Includes:

- Prompt text (often truncated by the adapter — Claude Code plugin caps at 200 chars; MCP adapter forwards whatever the AI passes verbatim)
- Tool names and (per the Claude Code plugin) tool argument **keys** without values; the MCP adapter and SDKs let the AI choose what to include
- Memory IDs, region hints, scores
- Error messages and where they happened
- Timestamps, adapter IDs, session IDs

**Where it lives:** SD Core's in-process ring buffer (1000 events by default, oldest dropped). Broadcast to connected WebSocket subscribers as they fire. **Not persisted to disk** unless an event is also a `memory_added` event with text — those go to the SQLite bank below.

When the dashboard reconnects, the last 50 events are replayed automatically so it can catch up.

### 3. SQLite fallback bank (`/var/lib/sd/sd_bank.db` — Docker named volume)

A durable store of `memory_added` events that carry text. Used so SD Core can be the AI's memory system of last resort when no upstream system is configured. Schema mirrors the memory record above.

**Where it lives:** Docker named volume `synaptic-bank`. Survives `docker compose down` (data preserved). Destroyed by `docker compose down --volumes` or `docker volume rm synaptic-bank`. Native installs put it at `<data-dir>/sd_bank.db` by default.

The bank is **not** populated by SD Core's filesystem watcher (those events have no text payload) — only by adapters reporting via `report_memory_save` or by direct writes to `POST /bank/memory`.

### 4. Thought-bubble pool (`assets/thought_bubbles.json`)

Phrases generated by SD Core's bubble generator. Each phrase is a short inner-monologue thought (4–12 words) generated by Ollama, conditioned on a sample of your memories. Could contain echoes of memory content (a project name, a worry, a domain term). The phrases are derived; the underlying memories themselves never leave the machine through this path.

**Where it lives:** `assets/thought_bubbles.json` on the host filesystem. Served by SD Core's static handler as `/assets/thought_bubbles.json`. Atomically rewritten on each generation tick.

### 5. Browser localStorage

Per-browser state used by the dashboard. Includes:

- Theme + cognitive modes settings
- Render presets (`sd:presetsAll`)
- Subject name (`SUBJECT: SUBJECT-01` until you change it)
- The thought-bubble pool from earlier dashboard versions (legacy, ignored when the server pool is reachable)
- The Bridge config: `sd_core_url` and `sd_api_token` if you pasted them via the Config panel

**Where it lives:** the browser's local storage for the origin you opened the dashboard from. Visible to JS running on that origin. Wiped when you clear site data.

The API token in localStorage is the same value SD Core was started with — handle it like a password. If you suspect it's been exposed, rotate by changing both ends (SD Core's `SD_API_TOKEN` env and the value in your localStorage / Config panel).

---

## Where data travels

### Default mode (localhost-only)

When you run `docker compose up` and open `http://localhost:9911`, every connection is a loopback. Nothing leaves the host machine. Specifically:

| Connection | What flows | Crosses network? |
|---|---|---|
| Dashboard browser → SD Core (HTTP/WS) | Static asset requests, event subscription, control plane calls | No (loopback) |
| Adapter → SD Core | Activity events | No (loopback) |
| SD Core → Ollama | Bubble generator prompts (text + sampled memories) | No (Docker bridge network) |
| `tools/classifier.py` → Ollama | Build-time classification queries | No (loopback) |

Ollama itself runs locally (the official `ollama/ollama` Docker image). It does not phone home. Model weights are pulled once at first launch from `ollama.com` (or your configured registry); after that, the LLM operates fully offline.

### Remote mode (explicit opt-in)

When you configure Cloudflare Tunnel or Tailscale per [`REMOTE.md`](REMOTE.md), the dashboard's API and WebSocket reach beyond your machine. Specifically:

#### Cloudflare Tunnel

- **What goes through Cloudflare:** every HTTP request and every WebSocket frame between adapters/dashboards on remote machines and your SD Core. The tunnel itself is a persistent outbound connection from your desktop to Cloudflare's edge — encrypted in TLS.
- **What Cloudflare can see:** request metadata (URL paths, hostnames, headers including the Bearer token, response sizes, timing). Cloudflare cannot see TLS-encrypted bodies in transit, but TLS terminates at their edge before traffic enters your tunnel — so they have access to the decrypted payload at the edge layer.
- **What Cloudflare logs:** depends on your account settings. The free tier of Cloudflare Tunnel logs metadata; payload contents are not retained by default. If you enable Cloudflare Access, your authentication identity (email / OAuth provider) is also visible to Cloudflare.
- **Practical implication:** treat events flowing through CF Tunnel as if Cloudflare were your reverse proxy. Don't put anything in event payloads you wouldn't be comfortable with a major cloud provider seeing in plaintext at their edge. The Bearer token gates *who* can connect, not what Cloudflare sees.

#### Tailscale

- **What goes through Tailscale:** every HTTP request and WebSocket frame between machines in your tailnet. The traffic is end-to-end WireGuard-encrypted between devices.
- **What Tailscale can see:** Tailscale operates the coordination server (which manages key exchange and ACLs) but does not handle the actual data plane — packets between your devices flow direct (or via DERP relays only when direct connection is impossible). When relayed via DERP, traffic is end-to-end encrypted; Tailscale sees only encrypted bytes and metadata (which devices are talking, when).
- **What Tailscale logs:** ACL-relevant decisions; not data-plane content. See [tailscale.com/security](https://tailscale.com/security) for their full posture.
- **Practical implication:** Tailscale is closer to a private VPN than a reverse proxy. The tailnet is your perimeter — bearer-token auth becomes optional (the tailnet ACL gates who's even on the network).

### Adapter-side caveats

Each adapter has its own posture toward what it forwards:

- **Claude Code plugin:** truncates prompt text to 200 characters, forwards tool input *keys* only (not values), drops tool outputs entirely. Nothing else from your Claude Code session reaches SD Core.
- **MCP adapter:** explicit by design — only emits events the AI calls `report_event` / `report_memory_save` for. The AI controls payload contents. If you don't want full prompt text in your event log, instruct the model to truncate before calling.
- **OTel adapter:** translates whatever attributes your OTel instrumentation emits. GenAI semantic-conventions instrumentations include token counts and timing by default; some include prompt/response text depending on the instrumentation's settings.
- **Python and TypeScript SDKs:** you write the payload. Whatever your code passes to `emit()` is what gets forwarded. The SDK adds nothing automatic.

None of the adapters phone home. They POST only to the SD Core URL you configure (default `127.0.0.1:9911`).

---

## The unavoidable third-party network call

The dashboard's `index.html` pulls Three.js and a few helper libraries from `https://cdnjs.cloudflare.com/...`. This means:

- Your browser makes an outbound HTTPS connection to Cloudflare every time you load the dashboard.
- Cloudflare can log: your IP address, the User-Agent, the asset URLs requested, the timing.
- This applies even in localhost-only mode — the browser still fetches those scripts from the public internet.

If this matters for your threat model, two mitigations:

1. **Self-host the JS.** Download Three.js + the helpers, drop them under `assets/vendor/`, and edit `index.html`'s `<script>` tags to point at the local copies. The dashboard will then load fully offline with zero third-party requests.
2. **Use a network-blocking layer.** Browser extensions like uBlock Origin can block `cdnjs.cloudflare.com` if you've cached the assets locally. Pi-hole / NextDNS can block at the network level. (You'd need to keep a local cache, since the browser would otherwise fail to load.)

Future versions will likely vendor these libraries in-tree to remove the dependency. Tracked as a roadmap item.

---

## What's NOT collected

For the avoidance of doubt:

- **No analytics or telemetry.** This project does not include any analytics SDK, error reporting service, or usage-tracking code. Searching the source for "analytics", "telemetry", "tracking" returns zero functional matches outside doc files.
- **No phone-home from SD Core.** Verify yourself: `bridge/core/main.go` makes outbound HTTP calls only to the configured Ollama URL (when the bubble generator is enabled). It never reaches anything outside that one URL.
- **No phone-home from adapters.** Each adapter POSTs only to the configured SD Core URL.
- **No usage stats sent to the project author.** I (Anthony, the author) cannot see whether or how you're using this. The only feedback channel is whatever you choose to share with me directly.
- **No identifying information collected from your AI clients.** Adapters report a free-form `adapter_id` and `model` label; both are local choices you (or the integration's defaults) make.

---

## Your controls

| Control | Effect |
|---|---|
| `SD_BUBBLE_GEN=0` env | Disables the bubble generator entirely. Dashboard falls back to the baked seed pool. No prompts sent to Ollama from SD Core. |
| Don't install Ollama | Both the build-time classifier and the bubble generator silently no-op. Dashboard still works. |
| Don't run SD Core | Use `python serve.py` only. Dashboard runs on the mock event engine. No data is stored anywhere except localStorage. |
| `SD_API_TOKEN=<token>` env on SD Core | Gates who can read/write the API. Without a matching token, requests get 401. |
| `docker volume rm synaptic-bank` | Wipes the SQLite bank entirely. (The container must be stopped first.) |
| Delete `assets/memories.json` | Removes the memory dataset. Watcher emits `memory_deleted` events for every record on next reload. |
| Delete `assets/thought_bubbles.json` | Wipes the bubble pool. Server regenerates over time. |
| Browser: clear site data for the dashboard origin | Wipes localStorage. Bridge config + theme + presets all reset. |
| Self-host CDN assets (see above) | Eliminates the third-party network call to `cdnjs.cloudflare.com`. |

---

## Default file locations summary

| File / volume | What | Sensitive? |
|---|---|---|
| `assets/memories.json` | Your memory records (Dev only — Prod ships without this file) | **Yes — never commit from `Dev/`; commit from `Prod/` which strips it automatically** |
| `assets/voxels.json`, `assets/anatomy.json`, `assets/synapses.json` | Allen Atlas voxel data + region definitions + computed synapse paths | No (CC BY 4.0 anatomical data + derived structure) |
| `assets/thought_bubbles.json` | Generated bubble pool | Mildly — derived from memory samples |
| `synaptic-bank` Docker volume (`/var/lib/sd/sd_bank.db`) | SQLite bank of `memory_added` events with text | Yes |
| `ollama-tier1-data` / `ollama-tier2-data` Docker volumes | Cached Ollama model weights (per tier) | No (just the model files) |
| `airllm-tier2-cache` Docker volume | Cached HuggingFace model weights for AirLLM (Tier 2 profile only) | No (just the model files) |
| `<repo>/.env` | Optional — your `SD_API_TOKEN` lives here in the recommended setup | Yes |
| Browser localStorage for dashboard origin | UI state + optional cached Bridge config (URL + token) | Yes if token is stored there |

---

## Public release artifact (`Prod/` and the beta zip)

The `Prod/` directory and the beta zip in `Releases/Beta/` produced by `tools/build_release.py` are **scrubbed by construction**:

- `assets/memories.json` and `assets/synapses.json` are **not shipped at all**. SD Core auto-creates empty stubs on first start.
- `assets/memories.example.json` and `assets/synapses.example.json` ARE shipped — fully synthetic, opt-in via `tools/seed_with_example.py`. Self-wipes on first real adapter event.
- `docs/dev/` is excluded wholesale (HANDOFF.md, PROJECT_PLAN.md, SHIPPING.md, `docs/dev/` (the dev-only Hindsight guide)).
- Any `sd_bank.db*` files are excluded.
- Any `hindsight-backup*.json` files are excluded.
- `node_modules/`, `__pycache__/`, `.venv/`, `dist/`, and similar build detritus are excluded.
- The dev-only `window.sd = ...` debug exposure is stripped from `index.html`.
- Personal-config substitutions applied: filesystem paths in examples, machine names, default subject identifier (`SUBJECT-01` → `SUBJECT-01`), and tunnel URL placeholders. Author attribution (HUD byline, package metadata, CREDITS.md) is preserved — see `tools/_sanitize.py` for the precise list.

So the Prod artifact is safe to share publicly. Re-run `python tools/build_release.py` after any change to regenerate. As of v2.3.0b1, the release pipeline also strips the `# Pending changelog` boilerplate header from `CHANGELOG-PENDING.md` before promoting it to the versioned `Releases/Beta/synaptic-<version>-NOTES.md` file — published release notes no longer leak the dev-tree scaffold.

---

## Reporting privacy issues

If you find a data path that flows somewhere unexpected, or believe this document misrepresents an actual behavior, please open an issue. I'd rather know.
