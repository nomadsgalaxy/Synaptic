# Remote operation

Run the Synaptic dashboard on one machine while AI activity from any number of other machines flows into it. Two transport options below; pick whichever fits your situation. Both pair with SD Core's Bearer-token auth so untrusted networks can't push fake events into your brain.

If you only ever use one machine, you don't need any of this — local Docker (`docker compose up`) is enough.

## What changes vs. local-only mode

| | Local | Remote |
|---|---|---|
| Where the dashboard runs | `localhost:9911` | a public hostname (Cloudflare Tunnel) or a private mesh address (Tailscale) |
| What adapters point at | `127.0.0.1:9911` | the same public/private address |
| Who can reach SD Core | only this machine | anyone you give the address + token to (CF Tunnel) or anyone in your tailnet (Tailscale) |
| Auth | none (bound to localhost) | Bearer token (CF Tunnel) or tailnet ACL (Tailscale) |

The dashboard, adapter SDKs, and SD Core all natively support both modes — you just configure two env vars: `SD_CORE_URL` and `SD_API_TOKEN`.

---

## Path A — Cloudflare Tunnel + Bearer token (recommended)

**Best when:** you control DNS for a domain (or use a Cloudflare-provided hostname), want devices to reach the dashboard from any network without per-device VPN setup, and would like the dashboard hostname behind interactive SSO. No port forwarding, no static IP, no router config.

### Topology

- `synaptic.<your-domain>` — the dashboard (HTML page in a browser). Optional: behind Cloudflare Access for interactive SSO.
- `cognito.<your-domain>` — SD Core's API + WebSocket. Bearer-token auth on every request.

Both subdomains point to the same SD Core process via `cloudflared` running on the desktop. The dashboard JS auto-detects which subdomain it was loaded from and connects WS to the matching origin (or you can set the URL explicitly via the Config panel).

> **📘 Full setup guide: [`CLOUDFLARE_BRIDGE.md`](CLOUDFLARE_BRIDGE.md).** The steps below cover the high-level shape (generate token, install `cloudflared`, route DNS, run as service). The dedicated guide expands every step with the comprehensive path-family table (§3), the canonical ingress regex (§4), the `/admin/*` Access-policy gotcha (§5), WebSocket considerations (§6), an end-to-end worked example (§7), verification probes + common failure modes (§8), and the procedure for when SD Core adds a new path family (§9). Read it before going live; come back here for the quick-reference setup.

### Setup

1. **Generate a token.** One token shared across all your machines.
   ```bash
   openssl rand -hex 24
   # e.g.: a3f2c8d4e9b0...   ← keep this private, treat like a password
   ```

2. **Bring up SD Core with the token set.** Drop a `.env` next to `docker-compose.yml`:
   ```bash
   # .env
   SD_API_TOKEN=a3f2c8d4e9b0...
   ```
   Then `docker compose up -d`. The startup log should say `auth: SD_API_TOKEN set — Bearer token required`.

3. **Install `cloudflared`** on the dashboard host:
   ```bash
   # macOS:    brew install cloudflared
   # Windows:  winget install --id Cloudflare.cloudflared
   # Linux:    https://github.com/cloudflare/cloudflared/releases/latest
   cloudflared tunnel login                    # OAuth flow in your browser
   cloudflared tunnel create synaptic
   ```

4. **Configure the tunnel.** Drop a `~/.cloudflared/config.yml`:
   ```yaml
   tunnel: synaptic
   credentials-file: ~/.cloudflared/<tunnel-id>.json

   ingress:
     # Both subdomains point to the same SD Core. CF routes them to the same
     # backend; SD Core serves the dashboard at any path and the API at the
     # same paths. Two hostnames give you separate Access policies (e.g. SSO
     # on the dashboard but token-only on the API), not separate backends.
     - hostname: synaptic.your-domain.com
       service: http://localhost:9911
     - hostname: cognito.your-domain.com
       service: http://localhost:9911
     - service: http_status:404
   ```

5. **Route DNS** (creates the CNAMEs for you):
   ```bash
   cloudflared tunnel route dns synaptic synaptic.your-domain.com
   cloudflared tunnel route dns synaptic cognito.your-domain.com
   ```

6. **Run the tunnel** as a service:
   ```bash
   cloudflared service install                 # macOS/Linux
   sudo systemctl start cloudflared            # Linux only
   # On Windows: cloudflared installs as a Windows service via the same command.
   ```

7. **(Optional but recommended) put the dashboard behind Cloudflare Access** for interactive SSO. In the Cloudflare dashboard: Zero Trust → Access → Applications → Add → Self-hosted → enter `synaptic.your-domain.com` → policy "Allow if email matches `you@example.com`." Free tier covers up to 50 users. The API subdomain `cognito.` stays Access-free so adapters connect with just the Bearer token.

8. **Open the dashboard in a browser** at `https://synaptic.your-domain.com`. The dashboard auto-detects same-origin and uses the loaded host for both static + API + WS. To test the auth gate from a terminal: `curl -H "Authorization: Bearer $SD_API_TOKEN" https://cognito.your-domain.com/healthz`.

9. **Wire your adapter on each remote device** with the URL + token:
   ```bash
   export SD_CORE_URL=https://cognito.your-domain.com
   export SD_API_TOKEN=a3f2c8d4e9b0...
   ```
   Then install the adapter as usual (Claude Code plugin / MCP server / OTel / SDK — see [`INTEGRATIONS.md`](INTEGRATIONS.md)). Every event flows through the tunnel, signed by the Bearer header.

### Expected latency

About 40–120 ms end-to-end per event over a typical home → coffee-shop link, dominated by the TCP+TLS round-trip to the nearest Cloudflare edge. WebSocket means after the initial connect, events fire one-way down the open socket — no per-event handshake. The dashboard's animations (200–400 ms easing on neuron pops, synapse pulses) easily mask the transport delay.

### Troubleshooting

- **Dashboard shows OFFLINE:** open browser devtools → Network → WS, confirm the request to `wss://cognito.your-domain.com/ws` succeeds. 401 means the token is wrong; check the Config panel's Bridge section.
- **Adapter logs show 401:** `SD_API_TOKEN` doesn't match what SD Core was started with. They must be byte-equal.
- **`cloudflared` fails to start:** check `journalctl -u cloudflared` (Linux) or the macOS / Windows service logs. Most common cause is stale tunnel credentials.

---

## Path B — Tailscale (private mesh, no public exposure)

**Best when:** you only want your own devices reaching the dashboard, prefer no public hostname at all, and accept that every device needs the Tailscale daemon installed. Connecting from a borrowed machine doesn't work.

### Topology

- All your devices join the same tailnet.
- your dashboard host runs SD Core bound to its tailnet IP (or `0.0.0.0`).
- Other devices reach SD Core directly at `http://<desktop-tailnet-ip>:9911` or via a friendly Tailscale-served hostname.
- No Bearer token required — the tailnet is the perimeter. (You can still set `SD_API_TOKEN` if you want belt-and-suspenders.)

### Setup

1. **Install Tailscale** on every device that needs to reach SD Core:
   - <https://tailscale.com/download>
   - Sign in with the same account on every device.

2. **Find your dashboard host's tailnet IP** (typically `100.x.y.z`):
   ```bash
   tailscale ip -4
   # e.g.: 100.64.7.42
   ```

3. **Bring up SD Core bound to the tailnet interface** (or to `0.0.0.0`, which exposes on the LAN too — fine if the LAN is trusted):
   ```bash
   # In docker-compose.yml, the core service already uses SD_CORE_LISTEN=0.0.0.0:9911,
   # which means the host's :9911 is reachable on every interface, including Tailscale.
   docker compose up -d
   ```

   For tailscale-only binding, override:
   ```yaml
   environment:
     SD_CORE_LISTEN: "100.64.7.42:9911"   # your tailnet IP
   ```

4. **(Optional) `tailscale serve`** to give the dashboard a friendlier URL:
   ```bash
   tailscale serve --bg --https=443 --set-path=/ http://localhost:9911
   # → https://nomad-desktop.tailxxxx.ts.net
   ```

   This gives you HTTPS via Tailscale's automatic certs, so the dashboard's WSS works naturally.

5. **Wire your adapter on each remote device** to the tailnet address:
   ```bash
   export SD_CORE_URL=http://100.64.7.42:9911
   # Or, if you set up tailscale serve:
   export SD_CORE_URL=https://nomad-desktop.tailxxxx.ts.net
   ```
   No `SD_API_TOKEN` needed — the tailnet is your auth boundary.

### Expected latency

Direct device-to-device connection over Tailscale's WireGuard mesh is typically 5–40 ms — usually faster than CF Tunnel because there's no edge hop. Same dashboard responsiveness story applies.

### Troubleshooting

- **`100.x.y.z:9911` unreachable from another device:** confirm `tailscale ping <other-device>` works. If not, check Tailscale ACLs in the admin console.
- **`tailscale serve` says permission denied:** Tailscale needs MagicDNS enabled and HTTPS certs provisioned for the tailnet. Run `tailscale cert` once if needed.
- **Both Tailscale and CF Tunnel running:** they don't conflict — Tailscale is L3, CF Tunnel is L7. Pick one URL to point your adapters at and stick with it.

---

## Both paths share

### One token across all machines

A single `SD_API_TOKEN` is fine — there's no per-device key management. Generate once with `openssl rand -hex 24`, store in a password manager, drop into the SD Core `.env` and into every adapter's environment. Rotate by changing both ends at the same time.

### The dashboard's Bridge config UI

Open the dashboard, click **CONFIG** in the bottom-right chip switcher, scroll to the **Bridge (SD Core)** section. You can paste the URL + token at runtime; values persist in `localStorage` and survive page reloads. Useful for: testing, demoing from a borrowed browser, or pointing one dashboard at multiple SD Cores in turn. Status line shows `configured · <url> · connected · auth` when everything is green. Pasting a new URL re-binds the dashboard's bridge live — no page reload needed.

### Once a real agent is on the line, the mock engine stays dead

The dashboard ships with a mock event engine that drives the brain when no SD Core is reachable. As soon as a real adapter (anything not prefixed `sd-core-`) fires its first event over the bridge, the mock engine is locked out for the rest of the session — even if the bridge later drops. This way, the dashboard's silence is informative ("nothing is happening right now") rather than misleading ("here's some plausible-looking activity").

### Adapter env-var reference

Every adapter (Claude Code plugin, MCP server, OTel adapter, Python SDK, TypeScript SDK) honors:

| Env var | Purpose | Default |
|---|---|---|
| `SD_CORE_URL` | Full URL of SD Core (`https://cognito.example.com`). When set, takes precedence over host+port. | unset |
| `SD_CORE_HOST` | Legacy host for localhost mode. | `127.0.0.1` |
| `SD_CORE_PORT` | Legacy port for localhost mode. | `9911` |
| `SD_API_TOKEN` | Bearer token sent on every request when set. Match SD Core's. | unset |
| `SD_CLIENT` (MCP only) | Free-form label distinguishing adapters in the HUD. | `mcp-client` |
| `SD_MODEL` | Model name shown in the HUD's model line. | `unknown` |

The dashboard itself reads URL + token from (in priority order): `?sd=<url>&token=<tok>` query string → `localStorage` (set by the Config panel) → `<meta name="sd-core-url">` / `<meta name="sd-api-token">` → same-origin fallback (works automatically in the recommended Docker setup).

---

## Choosing between the two

|  | Cloudflare Tunnel | Tailscale |
|---|---|---|
| Public exposure | Yes (gated by token + optional CF Access) | None |
| Setup effort | Higher (DNS, tunnel, optional Access) | Lower |
| Devices need any agent installed? | No | Yes (Tailscale daemon) |
| Demoing from a borrowed laptop | Works (token-only access) | Doesn't (machine must be in tailnet) |
| Latency | 40–120 ms typical | 5–40 ms typical |
| Best for | Personal use across any network, optional sharing with a few people | Personal device mesh only, security-conscious |

If you can't decide: go Cloudflare Tunnel. It's more flexible and the Bearer token gives you most of Tailscale's safety properties.
