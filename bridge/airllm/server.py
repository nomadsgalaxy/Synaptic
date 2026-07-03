#!/usr/bin/env python3
"""AirLLM sidecar — local large-model Tier 3 provider for Synaptic Core.

Runs a small HTTP server (stdlib `http.server` — no FastAPI dependency) that
exposes AirLLM's layered-streaming inference as `POST /generate` and
`GET /healthz`. SD Core's `airllm_provider.go` is the only client.

Why stdlib over FastAPI: keeps the dependency surface narrow (airllm itself
already pulls in transformers + torch + accelerate; we don't need uvicorn
on top). The endpoint set is two routes, sync, no streaming — stdlib is
the right tool.

Usage:

    python server.py --listen 127.0.0.1:9912 \\
                     --model meta-llama/Llama-3.1-70B-Instruct \\
                     --compression 4bit

Environment variables:
    AIRLLM_AUTH_TOKEN   optional bearer token; required on /generate when set.
    HF_HOME             override HuggingFace cache dir (default ~/.cache/huggingface).

The model is loaded once at startup (slow — minutes on first run, seconds on
warm cache). To switch models, restart the sidecar with a new --model flag.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import threading
import time
import traceback
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse


# ---- Model loading ----------------------------------------------------------
#
# AirLLM dispatches based on model id prefix. We probe the id and pick the
# right class. Failure to import airllm at startup is a hard fatal — the
# user should `pip install -r requirements.txt` first.

def _resolve_airllm_class(model_id: str):
    """Pick the AirLLM class for this HF model id."""
    try:
        import airllm
    except ImportError as e:
        # ImportError text carries the actual failed module — surface it
        # so the user sees "No module named 'optimum.bettertransformer'"
        # instead of the misleading "airllm not installed" line. When the
        # missing name is literally 'airllm', the pip install hint still
        # applies; otherwise it's a transitive dep version mismatch and
        # the rebuild path differs.
        missing = getattr(e, "name", None) or ""
        if missing == "airllm" or not missing:
            sys.stderr.write(
                "ERROR: airllm not installed. Run `pip install -r requirements.txt` "
                "in this directory.\n"
            )
        else:
            sys.stderr.write(
                f"ERROR: airllm import failed because of a missing transitive dep: {e}.\n"
                f"This is usually a version mismatch in requirements.txt — try rebuilding "
                f"the container with `docker compose --profile airllm-tier2 build airllm-tier2` "
                f"after pulling a fresh requirements.txt.\n"
            )
        raise SystemExit(1) from e

    lid = model_id.lower()
    # Llama variants (3.x, 2.x, code-llama, etc.)
    if "llama" in lid:
        return airllm.AirLLMLlama2
    # Mistral / Mixtral
    if "mistral" in lid or "mixtral" in lid:
        return airllm.AirLLMMistral
    # Qwen (Alibaba)
    if "qwen" in lid:
        return airllm.AirLLMQWen
    # ChatGLM / GLM family (THUDM)
    if "chatglm" in lid or "thudm/" in lid:
        return airllm.AirLLMChatGLM
    # Default: try the auto class — covers most Hugging Face decoder-only models.
    if hasattr(airllm, "AutoModel"):
        return airllm.AutoModel
    # Last resort.
    return airllm.AirLLMLlama2


def _load_model(model_id: str, compression: str):
    """Load + return the AirLLM model. Compression: 4bit | 8bit | none."""
    cls = _resolve_airllm_class(model_id)
    kwargs: dict = {}
    if compression and compression != "none":
        # AirLLM uses bitsandbytes-style flags; the API has shifted between
        # versions. Try the modern signature first, fall back to the legacy.
        kwargs["compression"] = compression
    print(f"[airllm-sidecar] loading {model_id} (compression={compression}) ...", flush=True)
    t0 = time.time()
    try:
        try:
            model = cls(model_id, **kwargs)
        except TypeError:
            # Older airllm versions used `compression` as a positional arg or
            # didn't accept it at all.
            model = cls(model_id)
    except Exception as e:
        # Surface HF-side access/lookup errors with actionable guidance
        # instead of the raw traceback. The huggingface_hub package is
        # imported lazily to avoid a hard import-time dep on a specific
        # version — older HF clients raise from `huggingface_hub.utils`,
        # newer ones from `huggingface_hub.errors`. We detect by name.
        cls_name = type(e).__name__
        if cls_name == "GatedRepoError":
            sys.stderr.write(
                f"ERROR: HuggingFace says you don't have access to '{model_id}'.\n"
                f"This is a gated model. To use it:\n"
                f"  1. Visit https://huggingface.co/{model_id} and accept the license\n"
                f"  2. Generate a token at https://huggingface.co/settings/tokens\n"
                f"  3. Set HUGGING_FACE_HUB_TOKEN in your .env, then recreate this container\n"
            )
            raise SystemExit(2) from e
        if cls_name == "RepositoryNotFoundError":
            sys.stderr.write(
                f"ERROR: HuggingFace can't find '{model_id}'. Check the model id spelling.\n"
                f"Format is `<owner>/<repo>`, e.g. `meta-llama/Llama-3.1-8B-Instruct`.\n"
            )
            raise SystemExit(2) from e
        # Unknown failure — let the original traceback through. We've
        # done what we can to add color for the common cases.
        raise
    print(f"[airllm-sidecar] loaded in {time.time() - t0:.1f}s", flush=True)
    return model


# ---- Server state -----------------------------------------------------------

class State:
    """Module-level mutable state — kept on a class for clarity, not for OO."""
    model = None
    model_id: str = ""
    compression: str = "4bit"
    loaded: bool = False
    auth_token: str = ""
    lock = threading.Lock()  # AirLLM is not thread-safe; serialize generate calls.

    # Live-progress snapshot for GET /progress. Updated by ProgressStreamer
    # below as each token is sampled. SD Core polls this every ~5s during
    # Phase 0b enrichment to drive the Dream Journal progress UI and to
    # heartbeat the sidecar's lifecycle reaper. Fields:
    #   busy            — whether a generate call is in flight
    #   request_id      — opaque per-request marker (epoch-ms of start)
    #   prompt_chars    — len(prompt) on the inbound request
    #   prompt_tokens   — tokens in the prompt (input_ids.shape[-1])
    #   tokens_emitted  — output tokens produced so far
    #   max_tokens      — requested upper bound
    #   started_at      — epoch seconds of generate-call entry
    #   last_token_at   — epoch seconds of the most recent token
    #   elapsed_ms      — derived; total time inside generate
    #   eta_seconds     — derived; (max_tokens - tokens_emitted) × per-token rate
    progress = {
        "busy": False,
        "request_id": None,
        "prompt_chars": 0,
        "prompt_tokens": 0,
        "tokens_emitted": 0,
        "max_tokens": 0,
        "started_at": None,
        "last_token_at": None,
        "elapsed_ms": 0,
        "eta_seconds": None,
        "model": "",
    }
    progress_lock = threading.Lock()


class ProgressStreamer:
    """transformers `streamer`-compatible adapter that publishes per-token
    progress into State.progress. transformers calls `.put(value)` after
    each token is sampled (during `_sample`'s loop) and `.end()` when the
    generation finishes. We don't actually surface the streamed tokens —
    they're already collected in `output` by `model.generate()` — we just
    use the callbacks as a per-token tick to refresh ETA.

    Keeping the interface to the two methods transformers actually calls
    so we don't depend on the full TextStreamer surface (which expects a
    tokenizer + decoding path we don't need)."""

    def __init__(self, max_tokens: int, prompt_tokens: int, request_id: int, model_id: str):
        self.max_tokens = max(1, int(max_tokens))
        self.prompt_tokens = int(prompt_tokens)
        self.request_id = request_id
        self.model_id = model_id
        self.tokens_emitted = 0
        self.start_time = time.time()
        # Initialize the State snapshot at the start.
        with State.progress_lock:
            State.progress.update({
                "busy": True,
                "request_id": request_id,
                "prompt_chars": 0,  # filled by _handle_generate before this is constructed
                "prompt_tokens": prompt_tokens,
                "tokens_emitted": 0,
                "max_tokens": self.max_tokens,
                "started_at": self.start_time,
                "last_token_at": None,
                "elapsed_ms": 0,
                "eta_seconds": None,
                "model": model_id,
            })

    def put(self, value):
        # transformers may pass either a single token id (scalar) or a
        # tensor — we don't care, just count it. The first call with
        # the prompt's input_ids gets a tensor of length=prompt_tokens
        # and we want to skip that "echo" so our token count reflects
        # only new tokens. The simplest distinguisher: transformers
        # always calls `.put(input_ids)` first (one big chunk) and then
        # `.put(next_token)` once per sampled token. So if the count
        # we'd add matches prompt_tokens AND we haven't emitted any
        # yet, treat it as the echo and skip.
        try:
            shape = tuple(getattr(value, "shape", ()) or ())
            length = int(shape[-1]) if shape else 1
        except Exception:
            shape = ()
            length = 1
        # Diagnostic: one-line stderr per put() call. Surfaced under
        # docker logs synaptic-airllm-tier2. Pruned to first ~10 calls
        # per request (after that we trust the increment); SD Core's
        # /progress polling is the canonical surface for the dashboard,
        # this is just for verifying transformers actually invokes us.
        if self.tokens_emitted < 10:
            sys.stderr.write(
                f"[airllm-sidecar] streamer.put #{self.tokens_emitted + 1}: "
                f"shape={shape} length={length} prompt_tokens={self.prompt_tokens}\n"
            )
            sys.stderr.flush()
        if self.tokens_emitted == 0 and length == self.prompt_tokens and length > 1:
            return  # echo of the prompt — don't count
        self.tokens_emitted += length
        now = time.time()
        elapsed = now - self.start_time
        rate = self.tokens_emitted / elapsed if elapsed > 0 else 0
        remaining = max(0, self.max_tokens - self.tokens_emitted)
        eta = remaining / rate if rate > 0 else None
        with State.progress_lock:
            State.progress.update({
                "tokens_emitted": self.tokens_emitted,
                "last_token_at": now,
                "elapsed_ms": int(elapsed * 1000),
                "eta_seconds": eta,
            })

    def end(self):
        sys.stderr.write(
            f"[airllm-sidecar] streamer.end: total_tokens={self.tokens_emitted} "
            f"prompt_tokens={self.prompt_tokens}\n"
        )
        sys.stderr.flush()
        with State.progress_lock:
            State.progress["busy"] = False
            State.progress["eta_seconds"] = 0


# ---- HTTP handlers ----------------------------------------------------------

def _json_response(handler: BaseHTTPRequestHandler, status: int, payload: dict) -> None:
    body = json.dumps(payload).encode("utf-8")
    handler.send_response(status)
    handler.send_header("Content-Type", "application/json")
    handler.send_header("Content-Length", str(len(body)))
    handler.end_headers()
    handler.wfile.write(body)


def _check_auth(handler: BaseHTTPRequestHandler) -> bool:
    if not State.auth_token:
        return True
    auth = handler.headers.get("Authorization", "")
    if not auth.startswith("Bearer "):
        return False
    return auth[len("Bearer "):] == State.auth_token


def _handle_healthz(handler: BaseHTTPRequestHandler) -> None:
    payload = {
        "status": "ok",
        "model": State.model_id,
        "loaded": State.loaded,
        "compression": State.compression,
    }
    _json_response(handler, 200, payload)


def _handle_progress(handler: BaseHTTPRequestHandler) -> None:
    """GET /progress — live snapshot of in-flight inference.

    No auth required (same as /healthz) — the snapshot leaks at most
    timing + token counts, no prompt/output bytes. SD Core polls this
    every ~5s during Phase 0b enrichment to drive:
      - The Dream Journal progress UI (per-memory tokens / ETA / percent)
      - The sidecar's lifecycle heartbeat (a slow generate doesn't trip
        the idle reaper because /progress is moving — separate from
        SignalProviderUse but redundant with it, harmless)

    Returns the State.progress dict verbatim. `busy` is the load-bearing
    field: False between requests, True for the duration of generate().
    A poll that sees busy=False and last_token_at recently in the past
    means generation just finished — caller can use that as a terminal
    signal."""
    with State.progress_lock:
        snapshot = dict(State.progress)
    # Refresh elapsed_ms / eta_seconds in case the model is stuck on a
    # slow token (no .put() since the snapshot was last updated). This
    # is important: if generation is on token 1 of a 70B model, /progress
    # is the only thing telling SD Core "we're still alive" — but if we
    # only update fields inside ProgressStreamer.put, a hang would look
    # identical to a fast first-token finish. Recompute live.
    if snapshot.get("busy") and snapshot.get("started_at"):
        now = time.time()
        elapsed = now - float(snapshot["started_at"])
        snapshot["elapsed_ms"] = int(elapsed * 1000)
        # Don't recompute ETA here — without a token rate it'd be wrong.
        # Caller should look at last_token_at to detect a stall.
    _json_response(handler, 200, snapshot)


def _handle_generate(handler: BaseHTTPRequestHandler) -> None:
    if not _check_auth(handler):
        _json_response(handler, 401, {"error": "unauthorized"})
        return
    if not State.loaded or State.model is None:
        _json_response(handler, 503, {"error": "model not loaded"})
        return
    try:
        length = int(handler.headers.get("Content-Length", "0"))
    except ValueError:
        length = 0
    raw = handler.rfile.read(length) if length > 0 else b""
    try:
        body = json.loads(raw or b"{}")
    except json.JSONDecodeError as e:
        _json_response(handler, 400, {"error": f"bad json: {e}"})
        return

    prompt = body.get("prompt", "")
    if not isinstance(prompt, str) or not prompt:
        _json_response(handler, 400, {"error": "prompt is required"})
        return
    max_tokens = int(body.get("max_tokens", 512))
    temperature = float(body.get("temperature", 0.4))

    # AirLLM's generate signature varies between versions; the most common
    # shape is (input_ids, max_new_tokens=...).
    #
    # Device placement is load-bearing here. AirLLM 2.11.x's
    # layer-streaming forward runs each Llama block on `cuda:0` (when
    # GPU is available) while leaving auxiliary tensors (input_ids,
    # attention_mask) on CPU at tokenizer-output time. transformers
    # 4.43.x's `_sample` post-forward arithmetic then crashes:
    #     RuntimeError: Expected all tensors to be on the same device,
    #     but found at least two devices, cuda:0 and cpu!
    # — because `next_tokens` lands on cuda:0 but `pad_token_id` was
    # materialized as a CPU tensor. Two-part fix:
    #   1. Move tokenizer outputs onto the model's device before calling
    #      generate(). Best-effort: AirLLM doesn't always expose .device,
    #      so probe a few common attrs and skip the move if none stick.
    #   2. Don't pass pad_token_id explicitly — transformers will fall
    #      back to eos_token_id with a one-line stderr warning, which is
    #      preferable to the device-mismatch crash. The warning is
    #      cosmetic; the device crash is fatal.
    # We DO still pass attention_mask (now on the right device) — the
    # mask itself doesn't trigger the device crash, and it silences the
    # related transformers warning + improves left-padding behavior.
    started = time.time()
    with State.lock:
        try:
            tokenizer = getattr(State.model, "tokenizer", None)
            if tokenizer is None:
                _json_response(handler, 500, {"error": "model exposes no tokenizer"})
                return
            # Apply the model's Instruct chat template when available. Without
            # it, the model sees the user prompt as plain text to continue —
            # which breaks Phase-0b-style prompts that END with a complete
            # JSON example or a closing brace: Llama-3.1-70B reads that as
            # "the input is already finished" and emits EOS as the first
            # token (zero new tokens, empty completion, tokens_out=0). The
            # template wraps the prompt as a user turn and appends the
            # assistant-turn cue, giving the model the unambiguous "your
            # turn now" signal. Falls back to raw tokenization if the
            # tokenizer doesn't ship a chat_template — preserves prior
            # behavior for non-Instruct models.
            template = getattr(tokenizer, "chat_template", None)
            if template:
                try:
                    inputs = tokenizer.apply_chat_template(
                        [{"role": "user", "content": prompt}],
                        add_generation_prompt=True,
                        return_tensors="pt",
                        return_dict=True,
                    )
                    # Only wrap when we got a bare tensor — `BatchEncoding`
                    # inherits from `UserDict` (NOT `dict`), so a plain
                    # `isinstance(inputs, dict)` check returns False and
                    # would have wrapped the whole BatchEncoding under
                    # "input_ids", making `inputs["input_ids"]` resolve to
                    # the BatchEncoding itself (no `.shape`, crash on the
                    # tokens_in line). Probe for either an explicit key or
                    # mapping-ish behaviour instead.
                    if not (hasattr(inputs, "__getitem__") and "input_ids" in inputs):
                        inputs = {"input_ids": inputs}
                except Exception:
                    sys.stderr.write("[airllm-sidecar] chat_template apply failed; falling back to raw tokenize\n")
                    traceback.print_exc(file=sys.stderr)
                    sys.stderr.flush()
                    inputs = tokenizer(prompt, return_tensors="pt", truncation=True)
            else:
                inputs = tokenizer(prompt, return_tensors="pt", truncation=True)
            tokens_in = int(inputs["input_ids"].shape[-1])
            # AirLLM's wrapper inherits from PreTrainedModel but its
            # layer-streaming bypasses the normal device placement —
            # `State.model.device` is unreliable across versions. Detect
            # via the simpler fact: if CUDA is available, AirLLM runs on
            # cuda:0 (its layer-streaming hard-codes that target; see
            # airllm/airllm_base.py:running_device). Move inputs there
            # so the inputs match the sampler's expectations.
            try:
                import torch as _torch
                target_device = _torch.device("cuda:0") if _torch.cuda.is_available() else _torch.device("cpu")
            except Exception:
                target_device = None
            if target_device is not None:
                # Move tensors individually so non-tensor entries in a
                # BatchEncoding (e.g. `encodings`, returned by
                # tokenizer.apply_chat_template) don't blow up the whole
                # move via a dict-comprehension that has no .to() on a
                # list. Previously we did `{k: v.to(...) for k, v in ...}`
                # wrapped in a bare except — which silently swallowed the
                # non-tensor failure and left input_ids stranded on CPU,
                # producing a device-mismatch crash 6+ minutes later
                # inside `_sample` after a full forward pass had run.
                try:
                    if hasattr(inputs, "to"):
                        # BatchEncoding.to() handles tensor/non-tensor mix.
                        inputs = inputs.to(target_device)
                    else:
                        moved = {}
                        for k, v in inputs.items():
                            try:
                                moved[k] = v.to(target_device) if hasattr(v, "to") else v
                            except Exception:
                                moved[k] = v
                        inputs = moved
                except Exception:
                    sys.stderr.write("[airllm-sidecar] device move failed; staying on CPU\n")
                    traceback.print_exc(file=sys.stderr)
                    sys.stderr.flush()
            gen_kwargs = dict(
                max_new_tokens=max_tokens,
                temperature=temperature,
                do_sample=temperature > 0,
            )
            if "attention_mask" in inputs:
                gen_kwargs["attention_mask"] = inputs["attention_mask"]
            # Per-token progress streamer. Publishes into State.progress
            # so GET /progress reflects live token-count + ETA. Polled
            # by SD Core during Phase 0b enrichment for the Dream
            # Journal UI and as a lifecycle heartbeat (a slow generate
            # never trips the idle reaper because /progress is moving).
            request_id = int(started * 1000)
            streamer = ProgressStreamer(
                max_tokens=max_tokens,
                prompt_tokens=tokens_in,
                request_id=request_id,
                model_id=str(State.model_id),
            )
            with State.progress_lock:
                State.progress["prompt_chars"] = len(prompt)
            gen_kwargs["streamer"] = streamer
            # Force pad_token_id onto the same device as inputs. transformers
            # 4.43.x's generate() builds generation_config._pad_token_tensor
            # from whatever int pad_token_id it resolves (falling back to
            # eos_token_id), and that tensor lands on CPU by default. Later,
            # _sample tries `pad_token_id * (1 - unfinished_sequences)` where
            # the RHS is on cuda:0, blowing up with a device-mismatch
            # RuntimeError. Workaround: pass `pad_token_id` ourselves as a
            # tensor explicitly built on the target device — transformers'
            # `_prepare_generation_config` accepts tensors and uses them as-is.
            pad_id = getattr(tokenizer, "pad_token_id", None)
            if pad_id is None:
                pad_id = getattr(tokenizer, "eos_token_id", None)
            if pad_id is not None and target_device is not None:
                try:
                    import torch as _torch
                    gen_kwargs["pad_token_id"] = _torch.tensor(pad_id, device=target_device, dtype=_torch.long)
                except Exception:
                    gen_kwargs["pad_token_id"] = pad_id  # fall back to int
            output = State.model.generate(inputs["input_ids"], **gen_kwargs)
            # Slice off the input tokens to keep only newly-generated ones.
            # Replaces the previous text.startswith(prompt) trim, which only
            # worked when no chat template wrapped the input — template tokens
            # decode to text that doesn't match the raw prompt string, so a
            # startswith check would leave the entire wrapped prompt in the
            # response. Slicing by token count works for both paths and is
            # also more robust to whitespace normalization in tokenizer
            # round-trips.
            new_token_ids = output[0][tokens_in:]
            text = tokenizer.decode(new_token_ids, skip_special_tokens=True)
            tokens_out = int(new_token_ids.shape[-1])
        except Exception as e:  # noqa: BLE001 — surface ANY model failure to client
            # Print the full traceback to stderr so docker logs captures
            # WHERE the failure happened (the JSON response only carries
            # the exception type + message, which for tuple-unpack /
            # None-attribute errors is barely actionable).
            sys.stderr.write("[airllm-sidecar] /generate failure:\n")
            traceback.print_exc(file=sys.stderr)
            sys.stderr.flush()
            # Always clear `busy` on error — otherwise /progress reports
            # an in-flight generation forever after a crash, confusing
            # SD Core's polling loop and the Dream Journal UI.
            with State.progress_lock:
                State.progress["busy"] = False
                State.progress["eta_seconds"] = 0
            _json_response(handler, 500, {"error": f"{type(e).__name__}: {e}"})
            return
        finally:
            # Belt-and-suspenders: even on success the streamer.end() call
            # SHOULD have cleared busy, but if generate exited early (e.g.
            # stop sequence hit before any tokens), make sure progress
            # snapshot reflects the terminal state for the next /progress
            # poll.
            with State.progress_lock:
                if State.progress.get("busy"):
                    State.progress["busy"] = False
                    State.progress["eta_seconds"] = 0

    elapsed_ms = int((time.time() - started) * 1000)
    _json_response(handler, 200, {
        "text": text,
        "tokens_in": tokens_in,
        "tokens_out": max(tokens_out, 0),
        "elapsed_ms": elapsed_ms,
        "model": State.model_id,
        "compression": State.compression,
    })


class AirLLMHandler(BaseHTTPRequestHandler):
    """Routes /healthz GET + /generate POST. All other paths -> 404."""

    # Quiet default access logging — sidecar is noisy enough on its own.
    def log_message(self, format: str, *args) -> None:  # noqa: A002
        sys.stderr.write("[airllm-sidecar] " + (format % args) + "\n")

    def do_GET(self) -> None:  # noqa: N802 (stdlib API)
        path = urlparse(self.path).path
        if path == "/healthz":
            _handle_healthz(self)
            return
        if path == "/progress":
            _handle_progress(self)
            return
        _json_response(self, 404, {"error": "not found"})

    def do_POST(self) -> None:  # noqa: N802 (stdlib API)
        path = urlparse(self.path).path
        if path == "/generate":
            _handle_generate(self)
            return
        _json_response(self, 404, {"error": "not found"})


# ---- Main -------------------------------------------------------------------

def _parse_args() -> argparse.Namespace:
    # Env-var fallbacks for the docker-compose path. CLI flags still
    # override when both are supplied, so `python server.py --model X`
    # works the same as it always did. The compose services set
    # AIRLLM_LISTEN / AIRLLM_MODEL / AIRLLM_COMPRESSION at the env layer
    # rather than building a `command:` block with shell interpolation.
    listen_default = os.environ.get("AIRLLM_LISTEN", "127.0.0.1:9912")
    model_default = os.environ.get("AIRLLM_MODEL", "")
    compression_default = os.environ.get("AIRLLM_COMPRESSION", "4bit")

    p = argparse.ArgumentParser(description="AirLLM service for Synaptic Tier 2/3")
    p.add_argument("--listen", default=listen_default,
                   help=f"host:port to bind (default {listen_default}; "
                        "or AIRLLM_LISTEN env var)")
    p.add_argument("--model", default=model_default,
                   help="HuggingFace model id (or AIRLLM_MODEL env var). "
                        "Required — provide one or the other.")
    p.add_argument("--compression", default=compression_default,
                   choices=["4bit", "8bit", "none"],
                   help=f"layer compression scheme (default {compression_default}; "
                        "or AIRLLM_COMPRESSION env var)")
    args = p.parse_args()
    if not args.model:
        p.error("--model is required (either via --model flag or AIRLLM_MODEL env var)")
    return args


def main() -> None:
    args = _parse_args()
    State.model_id = args.model
    State.compression = args.compression
    State.auth_token = os.environ.get("AIRLLM_AUTH_TOKEN", "")

    State.model = _load_model(args.model, args.compression)
    State.loaded = True

    host, _, port = args.listen.rpartition(":")
    if not host:
        host = "127.0.0.1"
    server = ThreadingHTTPServer((host, int(port)), AirLLMHandler)
    print(f"[airllm-sidecar] listening on {host}:{port}", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("[airllm-sidecar] shutting down", flush=True)
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
