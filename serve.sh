#!/usr/bin/env bash
# Synaptic - one-click launcher for macOS/Linux
cd "$(dirname "$0")"
exec python3 serve.py "$@"
