# Optional Dockerfile for the AirLLM sidecar. Most users run the sidecar
# natively on the host so the GPU is reachable without Docker GPU-passthrough
# setup; this image is for the "I want everything in compose" crowd.
#
# Build:   docker build -t synaptic-airllm bridge/airllm
# Run:     docker run --rm -p 9912:9912 \
#              -v "$HOME/.cache/huggingface:/root/.cache/huggingface" \
#              synaptic-airllm \
#              --model meta-llama/Llama-3.1-70B-Instruct \
#              --compression 4bit
#
# For GPU support add `--gpus all` and ensure NVIDIA Container Toolkit is set up.

FROM python:3.11-slim

# System-level deps for torch wheels: libgomp for OpenMP, build tools for
# anything that falls back to source. Keeps the image lean (~1.5GB before
# the model itself, which streams from the mounted HF cache).
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        build-essential \
        git \
        libgomp1 \
        ca-certificates \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt

COPY server.py ./

# HuggingFace cache is intentionally NOT bundled — mount it as a volume
# at /root/.cache/huggingface so model files (60-200 GB) live on the host.
VOLUME ["/root/.cache/huggingface"]

EXPOSE 9912

# Compose-friendly entrypoint: server.py reads AIRLLM_LISTEN / AIRLLM_MODEL /
# AIRLLM_COMPRESSION from the env, so the compose service supplies those
# directly. Legacy `docker run synaptic-airllm --model X` still works because
# CLI flags override env defaults.
ENTRYPOINT ["python", "server.py"]
