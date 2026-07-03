# GitHub Actions

## `deploy-demo.yml` — Phase 4 D5

Builds and deploys a static demo of Synaptic to GitHub Pages. The deployed dashboard runs in mock mode (no SD Core, no Ollama) so visitors see the brain animate from synthetic events.

### One-time setup

1. **Repository → Settings → Pages**: set Source to **GitHub Actions**.
2. **Repository → Actions tab**: run the **Deploy demo to GitHub Pages** workflow once manually to confirm it works. The published URL appears at the bottom of the deploy job.
3. *(Optional)* Open `.github/workflows/deploy-demo.yml` and uncomment the `push:` block to auto-deploy on every push to `main`.

### What the workflow does

- Checks out the repo
- Regenerates the synthetic example dataset via `tools/generate_synthetic_memories.py` (168 memories / 857 synapses, fully randomized, no personal data)
- Runs `tools/build_pages.py` to produce a clean `Pages/` directory: dashboard shell + synthetic data renamed to live (Pages has no backend to populate at runtime), legacy PLY meshes excluded, debug hooks stripped, "static demo" banner injected, personal-config substitutions applied
- Uploads `Pages/` as a GitHub Pages artifact and publishes it

### What ends up on the public URL

- `index.html` — the dashboard with debug hooks stripped and a "static demo" banner injected at the top
- `assets/{voxels,memories,synapses,anatomy}.json` — voxels + anatomy from the project assets, plus the synthetic example dataset renamed to `memories.json` / `synapses.json` so the dashboard's fetch path finds them. The synthetic dataset carries `"synthetic": true` for transparency.
- `vendor/gif.js` + `gif.worker.js` — bundled for the in-dashboard GIF exporter
- `docs/CREDITS.md` + `docs/data_privacy.md` — attribution + data handling reference
- `_pages_README.md` — what this build is for

Note: source code (`bridge/`, `tools/`), Docker configs, and `docs/dev/` are intentionally **not** included. Pages-class hosts can't run them; they'd just be download bloat.

### Limits

- **No SD Core** — GitHub Pages serves static files only. The dashboard's bridge connection times out and falls back to the mock event engine.
- **No Ollama** — same. Thoughts come from the pre-baked seed pool baked into `index.html`.
- **No bridge plugin install** — visitors can't connect their own AI clients to the deployed demo. This is intentional; the demo is "watch the brain animate," and self-hosting (`docker compose up` from a Prod download) is the path to live data.
- **Visitors can paste a remote SD Core URL** — the dashboard's Config panel "Bridge" section accepts a URL + Bearer token at runtime. So a visitor with their own Cloudflare-Tunnel'd SD Core could point the demo at it. See [`docs/REMOTE.md`](../../docs/REMOTE.md).
