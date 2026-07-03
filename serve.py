#!/usr/bin/env python3
"""
Tiny local HTTP server for Synaptic.

Modern browsers block fetch() from file:// URLs for security. Open the dashboard
through this little server instead — it just runs python's stdlib http.server in
the project folder and opens the page in your default browser.

Usage:
    python serve.py            # serves on http://localhost:8765
    python serve.py 8080       # serves on http://localhost:8080
    python serve.py --no-open  # don't auto-open the browser

No external dependencies. Works on Windows, macOS, Linux with Python 3.7+.
"""
from __future__ import annotations
import argparse
import http.server
import os
import socketserver
import sys
import webbrowser
from pathlib import Path


class QuietHandler(http.server.SimpleHTTPRequestHandler):
    """Same as SimpleHTTPRequestHandler but with minimal logging and proper PLY mime type."""
    extensions_map = {
        **http.server.SimpleHTTPRequestHandler.extensions_map,
        ".ply": "application/octet-stream",
        ".json": "application/json",
        ".mjs": "application/javascript",
    }

    def log_message(self, fmt: str, *args):
        # Quieter than the default; still shows requests if you want them
        if "--verbose" in sys.argv:
            super().log_message(fmt, *args)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("port", nargs="?", type=int, default=8765, help="Port to listen on (default: 8765)")
    ap.add_argument("--no-open", action="store_true", help="Don't auto-open the dashboard in a browser")
    ap.add_argument("--verbose", action="store_true", help="Log each request")
    args = ap.parse_args()

    project_root = Path(__file__).resolve().parent
    os.chdir(project_root)

    url = f"http://localhost:{args.port}/index.html"
    print(f"Synaptic is serving from: {project_root}")
    print(f"Open: {url}")
    print("Press Ctrl-C to stop.\n")

    if not args.no_open:
        try:
            webbrowser.open(url)
        except Exception:
            pass

    with socketserver.ThreadingTCPServer(("127.0.0.1", args.port), QuietHandler) as httpd:
        try:
            httpd.serve_forever()
        except KeyboardInterrupt:
            print("\nShutting down.")


if __name__ == "__main__":
    main()
