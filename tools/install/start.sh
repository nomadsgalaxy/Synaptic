#!/usr/bin/env bash
# Synaptic — one-line starter for macOS / Linux.
#
# Brings up the Docker stack (synaptic-disorder-core + dashboard + ollama),
# waits for the dashboard to respond, and opens it in the default browser.
#
# Usage from the repo root:
#     ./tools/install/start.sh
set -euo pipefail

COMPOSE_FILE="${COMPOSE_FILE:-docker-compose.yml}"
# SD Core serves the dashboard on 9911 (matches docker-compose.yml's ports
# binding).  The legacy static-only fallback `serve.py` uses 8765; this
# Docker-based launcher must use 9911.
PORT="${PORT:-9911}"
NO_OPEN="${NO_OPEN:-0}"

# 1. Sanity-check Docker.
if ! command -v docker >/dev/null 2>&1; then
    cat >&2 <<EOF
Docker isn't on PATH. Install Docker Desktop / Engine:
    https://www.docker.com/products/docker-desktop/
    https://docs.docker.com/engine/install/
EOF
    exit 1
fi

# Resolve repo root: this script lives at tools/install/, so two-up.
SCRIPT_DIR="$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )"
REPO_ROOT="$( cd -- "$SCRIPT_DIR/../.." &> /dev/null && pwd )"
cd "$REPO_ROOT"

# 2. Bring up the stack.
echo "starting Synaptic Docker stack..."
docker compose -f "$COMPOSE_FILE" up -d

# 3. Poll the dashboard until it responds (up to 30s).
URL="http://localhost:${PORT}/index.html"
echo "waiting for dashboard at $URL ..."
deadline=$(( $(date +%s) + 30 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
    if curl -sf -o /dev/null -m 2 "$URL"; then
        break
    fi
    sleep 0.5
done

cat <<EOF

Synaptic is up.
  Dashboard:  $URL
  SD Core:    http://localhost:9911/healthz
  Ollama:     http://localhost:11434

Stop with: docker compose down
EOF

if [ "$NO_OPEN" != "1" ]; then
    if command -v open >/dev/null 2>&1; then
        open "$URL"
    elif command -v xdg-open >/dev/null 2>&1; then
        xdg-open "$URL" >/dev/null 2>&1 &
    fi
fi
