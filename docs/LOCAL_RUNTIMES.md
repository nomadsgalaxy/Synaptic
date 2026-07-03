# Local LLM runtimes

This page covers the alternative local LLM runtimes Synaptic ships
profile-gated alongside the default Ollama tier. All of them speak
OpenAI wire format natively, so they slot into the `custom + local`
auto-detect path (`isLocalURL` classifies the BaseURL → `Local=true`
→ accepted at `TierEmbedding`, sensitive-AI doesn't auto-disable,
egress filter skipped). No proxy layer needed.

> **v2.6 ships the backend** (compose profiles, env vars). The
> "Rx · prescription card" UI for activating and monitoring these
> runtimes lands in v2.7 alongside Bundle G's UI follow-up. Until
> then, configure via `docker compose --profile <name> up` plus
> environment variables; status surfaces in the existing
> `Config → AI/API → Docker Services` panel.

## Shipped profiles

### `llama-cpp` — llama.cpp server

```bash
docker compose --profile llama-cpp up -d llama-cpp-server
```

- **Image:** `ghcr.io/ggerganov/llama.cpp:server` (default) or
  `:server-arm64` for Pi4 / Apple Silicon — set
  `SD_LLAMA_CPP_TAG=server-arm64` in `.env`.
- **Ports:** `8089:8080` (host:container)
- **Default model:** `qwen2.5-0.5b-instruct-q4_k_m.gguf` (~400 MB).
  Drop your own gguf into the `llama-cpp-models` volume and override
  with `SD_LLAMA_CPP_MODEL=/models/<your-file>.gguf`.
- **RAM footprint** (Pi4 budget): ~0.6 GB for the default 0.5b
  model; ~2 GB for a 1B-Q4; ~4 GB for a 3B-Q4. Leave ≥2 GB headroom
  for SD Core + Ollama Tier 1 + OS.
- **Pi4 fitness:** ✅ Good. Use the `:server-arm64` tag and a Q4 1B
  model for the best speed/quality balance. Expect 5-15 tok/s on a
  Pi4 8GB depending on model size.
- **Wire it as a tier:** uncomment the "llama.cpp server" stanza
  in the "Provider mode B" block at the top of the `core:` service
  in `docker-compose.yml`. Inside the compose network the URL is
  `http://llama-cpp-server:8080/v1`.

### `localai` — LocalAI aggregator

```bash
docker compose --profile localai up -d localai
```

- **Image:** `localai/localai:latest-cpu` (default). GPU build:
  `localai/localai:latest-aio-gpu-nvidia-cuda-12` — override via
  `SD_LOCALAI_IMAGE` in `.env`.
- **Ports:** `8090:8080`
- **Default preload:** `phi-3-mini-4k`. Comma-separate for multiple:
  `SD_LOCALAI_MODELS=phi-3-mini-4k,qwen2.5-0.5b`.
- **RAM footprint** (Pi4 budget): ~2.5 GB for `phi-3-mini-4k`
  Q4_K_M. Heavier than llama.cpp:server because of the
  aggregator layer.
- **Pi4 fitness:** ⚠️ Borderline. Works for a single small
  preload; do not load `phi-3-medium` or larger on a Pi4 8GB
  alongside the default SD Core stack.
- **Strengths:** runtime model swaps via LocalAI's `/models/apply`
  API; supports both gguf and transformers backends; integrated
  TTS/STT/image endpoints (not currently used by Synaptic but
  available).
- **Wire it as a tier:** uncomment the "LocalAI" stanza in the
  "Provider mode B" block. URL: `http://localai:8080/v1` inside
  the compose network.

## Not shipped (and why)

### vLLM

**Not included.** vLLM is excellent on x86 + NVIDIA hosts but has
two issues for Synaptic's default audience:

1. **ARM builds are unreliable.** As of late 2025 the official vLLM
   ARM images either drop CUDA paths (no GPU support on ARM
   anyway) or fail to import on linux/arm64 due to upstream
   torch wheels mismatches. Pi4 users would hit a build error
   on first start.
2. **RAM headroom is thin on 8GB.** vLLM's PagedAttention kernel
   reserves a contiguous block at startup; non-tiny models leave
   no room for SD Core + Ollama Tier 1.

If you're on x86 with a dedicated GPU, vLLM is faster than
llama.cpp for batched inference. Run it outside the Synaptic
compose project on a separate host and point a tier at it via
the "Provider mode B" env vars with the host's LAN IP.

### Text Generation WebUI (oobabooga) OpenAI extension

**Not included.** TGWI is a desktop-class application with no
clean headless / compose-friendly default. The "OpenAI
extension" mode does work for SD Core, but the install dance
(volume mounts for the extension, model directory layout,
manual `--api` flag toggling) doesn't translate cleanly to a
shipped compose profile.

If you already run TGWI, point a tier at it via "Provider mode B":

```yaml
SD_TIER2_KIND: "custom"
SD_TIER2_URL:  "http://<tgwi-host>:5000/v1"
SD_TIER2_MODEL: "<whichever model TGWI has loaded>"
```

Synaptic's `custom + local` classifier will treat it as local
(LAN address) and route accordingly.

## Rx prescription card preview (v2.7)

The v2.7 Bundle H UI follow-up presents each registered runtime
as a medical-style **prescription card**:

```
┌────────────────────────────────────────────────────┐
│ Rx · llama-cpp-server                    [Active] │
│ ─────────────────────────────────────────────────── │
│ ghcr.io/ggerganov/llama.cpp:server-arm64           │
│                                                     │
│ Sig:    bind :8089 → :8080                          │
│         lifecycle: lazy (5 min idle)                │
│         attached to Tier 2                          │
│                                                     │
│ Indic.: Local 0.5b–3b GGUF on Pi4 8GB              │
│ Contra: <2GB free RAM                               │
│                                                     │
│ Refills: ∞      Last filled: 2026-05-13 22:14:08   │
│                                                     │
│ [Renew]  [Discontinue]  [Edit dosage]               │
└────────────────────────────────────────────────────┘
```

Fields:

- **Rx label** — container name (compose service or user-registered)
- **Active / Inactive** — `lifecycle_enabled` + running state
- **Sig** (Latin shorthand) — port mapping + lifecycle policy + tier
- **Indications / Contraindications** — use case + RAM/CPU caveats
- **Refills** — restart counter; ∞ = always-on policy
- **Last filled** — last container start timestamp
- **Renew / Discontinue / Edit dosage** — start / stop / reconfigure

Same template covers shipped profile-gated runtimes (this
document's main subject) AND user-registered containers attached
via per-tier `ProviderConfig.Managed` (Bundle G).

## See also

- `CHANGELOG-PENDING.md` § Bundle H — the v2.6 backend ship notes
- `docker-compose.yml` § Provider mode B — env-var template
- Bundle E: `kind=custom + local` auto-detect makes this all work
  without per-runtime adapter code.
- Bundle G: per-tier `ProviderConfig.Managed` lets you attach an
  arbitrary user-run container (not just the shipped profiles) and
  have SD Core's lifecycle manager treat it as a peer of
  `synaptic-ollama-tier2`.
