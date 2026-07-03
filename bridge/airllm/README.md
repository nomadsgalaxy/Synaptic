# AirLLM service for Synaptic Tier 2 / Tier 3

> **Note:** AirLLM is a first-class peer LLM provider as of v2.0.x —
> alongside Ollama and remote APIs, never a "sidecar." The `sidecar`
> wording remains in some headings and inline examples below for
> back-compat with the original handoff (`CODE_HANDOFF — AirLLM as
> peer LLM provider not sidecar (2026-05-10).md`); the HTTP contract is
> identical regardless. The Docker-compose path is the recommended
> primary install; the Python venv path below is the alternative.

A small Python HTTP server that exposes [AirLLM](https://github.com/lyogavin/airllm)
as a Tier 2/3 (Oracle) provider for SD Core. SD Core's Go provider talks
to this service over HTTP; the service runs the actual model inference
on your machine.

## Why AirLLM

AirLLM streams model layers from disk per token, which lets you run a
70B / 405B parameter model on a 4 GB GPU or even on CPU. The trade is
slower inference (~1-3 tokens/sec on consumer hardware vs ~30-100 tok/s
for Ollama's RAM-resident inference). For Tier 3 — premium-quality on
demand, single completions, no egress — the trade is worth it: full
local operation, zero API cost, model quality that beats Ollama-class
hardware.

This is **not** a replacement for Ollama at Tier 1 (real-time ingest)
or Tier 2 (Phase 0b's 100 memories/run pace). The latency math doesn't
work at those tiers; the SD Core backend rejects `kind=airllm` for
Tier 1 and Tier 2 with a 400.

## Install

Native (preferred — gets your GPU without Docker GPU-passthrough):

```sh
cd bridge/airllm
python -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt
# First run downloads the model. 60-200 GB depending on size; the cache
# lives in ~/.cache/huggingface/. Set HF_HOME if you want it elsewhere.
python server.py --listen 127.0.0.1:9912 \
                 --model meta-llama/Llama-3.1-70B-Instruct \
                 --compression 4bit
```

Docker (optional — see `Dockerfile`):

```sh
docker build -t synaptic-airllm bridge/airllm
docker run --rm --gpus all -p 9912:9912 \
    -v "$HOME/.cache/huggingface:/root/.cache/huggingface" \
    synaptic-airllm \
    --model meta-llama/Llama-3.1-70B-Instruct --compression 4bit
```

To add the sidecar to your existing `docker-compose.yml`:

```yaml
airllm:
  build: ./bridge/airllm
  command: ["--model", "meta-llama/Llama-3.1-70B-Instruct", "--compression", "4bit"]
  ports: ["9912:9912"]
  volumes: ["~/.cache/huggingface:/root/.cache/huggingface"]
  restart: unless-stopped
  # If on a CUDA host with the NVIDIA Container Toolkit:
  # deploy:
  #   resources:
  #     reservations:
  #       devices:
  #         - driver: nvidia
  #           count: all
  #           capabilities: [gpu]
```

## Configure SD Core

In the dashboard:

- Settings → Connection → Tier 3 (Oracle)
- **Provider:** AirLLM
- **Endpoint:** `http://127.0.0.1:9912` (or wherever you bound the sidecar)
- **Model:** `meta-llama/Llama-3.1-70B-Instruct` (must match `--model` flag)
- **Compression:** `4bit` (must match `--compression` flag)
- **Auth token (optional):** name of an env var holding the bearer token
  if you set `AIRLLM_AUTH_TOKEN` on the sidecar

Verify in the HUD: "Tier 3: AirLLM | Llama-3.1-70B | 4bit". The settings
page's Ping button hits `GET /healthz` and shows whether the sidecar is
reachable + has its model loaded.

**Hot-swap on Save (v2.3.0b1+).** Clicking Save on the Tier 2/3 Config
panel — including the AirLLM-specific Verify+Save button — triggers a
live hot-swap of the running `ModelRouter`. No SD Core restart required.
In-flight `Chat()` calls finish on the old provider; the next lookup
sees the new one. The response payload carries `hot_swapped: true` and
the resolved `active_provider: "..."` so the UI can confirm which model
is wired in.

**Tier 2 only — `deep_enrich_reencode_on_model_change`.** A per-Tier-2
setting in the Phase 0b knobs section. Off by default. When enabled,
swapping the Tier 2 model bulk-flags every previously-encoded memory
with reason `model_upgrade`, so the next Phase 0b run picks them up and
re-encodes them on the new model. Rows already on the incoming model
are skipped. The Save response surfaces `reencode_flagged: N` so the UI
shows "N memories queued for re-encode" inline. Useful for "prime the
bank with a fast 8B model overnight, flip to 70B, let it re-encode
everything" workflows.

## Hardware notes

| Model               | RAM peak (4-bit) | First-token latency | Throughput   |
|---------------------|------------------|---------------------|--------------|
| Llama-3.1-8B        | ~5 GB            | ~1 s                | ~30 tok/s    |
| Llama-3.1-70B       | ~24 GB           | ~30 s               | ~2 tok/s     |
| Llama-3.1-405B      | ~80 GB           | ~2 min              | ~0.5 tok/s   |

For a single Tier 3 research call (typically 500–2000 output tokens),
expect anywhere from 30 seconds to 30 minutes on consumer hardware.
SD Core's frontend renders a progress chip while the call is in flight;
cancellation is via the existing Tier 3 budget cap.

## API

Two endpoints. SD Core's Go provider is the only documented client;
this is here for debugging.

### `GET /healthz`

```
{
  "status": "ok",
  "model": "meta-llama/Llama-3.1-70B-Instruct",
  "loaded": true,
  "compression": "4bit"
}
```

Returns 200 once the model is loaded. Useful for SD Core's `/ping?tier=3`.

### `POST /generate`

Request:

```json
{
  "prompt": "...",
  "max_tokens": 1024,
  "temperature": 0.4
}
```

Response (200):

```json
{
  "text": "...",
  "tokens_in": 312,
  "tokens_out": 480,
  "elapsed_ms": 47830,
  "model": "meta-llama/Llama-3.1-70B-Instruct",
  "compression": "4bit"
}
```

Errors return JSON `{"error": "<message>"}` with appropriate status (400
for bad request, 401 for unauthorised, 500 for inference failure, 503
for "model not loaded").

### Authentication

Set `AIRLLM_AUTH_TOKEN` in the sidecar's environment to require a bearer
token on `/generate`. The header form is `Authorization: Bearer <token>`.
SD Core's `airllm_provider.go` reads the token from the env var named
in your provider config's `api_key_env` field.

When `AIRLLM_AUTH_TOKEN` is empty, the sidecar accepts requests without
auth — fine when bound to `127.0.0.1` (default), wrong if you ever
expose it on `0.0.0.0` without a firewall.

## Limitations

- One model in memory at a time. To switch models, restart the sidecar
  with a new `--model` flag. Loading a 70B model takes ~minutes on cold
  cache, ~tens of seconds when cached.
- No streaming response. Tier 3 use cases (single completion per query)
  don't need it; the existing `tier3_progress` WS event from SD Core's
  nightly pipeline is the user's progress feedback.
- AirLLM's API surface has shifted between releases. The server is
  defensive about the `compression` flag's calling convention but if
  upstream changes the model class hierarchy, edit `_resolve_airllm_class`
  in `server.py`.
- Embeddings are intentionally NOT supported (see `airllm_provider.go`'s
  `Embed` returning `ErrAirLLMEmbedUnsupported`). AirLLM-class chat
  models produce poor embeddings; use Tier 1 (Ollama nomic-embed-text)
  instead.

## Troubleshooting

- **Sidecar starts but `/healthz` returns `loaded: false`.** Model is
  still downloading or warming up. Watch the sidecar's stderr — it
  prints loading progress every few minutes.
- **`POST /generate` returns 503 "model not loaded".** Same as above —
  the sidecar is still starting.
- **`bitsandbytes` import error.** 4-bit / 8-bit compression needs
  `bitsandbytes` which has CUDA-version sensitivity on Linux. Set
  `--compression none` to bypass; throughput drops but you'll have RAM
  for it on 8B-class models.
- **GPU not detected inside Docker.** Add `--gpus all` and ensure the
  NVIDIA Container Toolkit is installed on the host.

## What this sidecar does NOT do

- Multi-tenant scheduling. One sidecar = one model = one in-flight
  request at a time (the server serialises generate calls).
- Auto-update / model fetching from outside the HuggingFace cache. The
  `--model` flag's first-run download is your model-fetch path.
- GPU auto-detection / auto-tuning. The sidecar reports its inference
  rate via `/healthz`; you read it and adjust the
  `nightly_max_tier3_calls_per_run` setting in SD Core if 70B is too
  slow for your patience.
