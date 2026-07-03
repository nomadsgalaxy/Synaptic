"""
JSON memory provider — the universal "bring your own memories" path.

Reads a hand-authored JSON file containing memories in any of these shapes:

  Shape A (top-level array):       [ {id, text, tags, ...}, ... ]
  Shape B (Memory Source Schema):  { "memories": [ ... ] }
  Shape C (legacy `items` field):  { "items":   [ ... ] }

Normalizes each memory to the Memory Source Schema v1.0 and writes the
result to assets/memories.json.

Usage:
    python tools/memory_providers/json_provider.py path/to/source.json
    python tools/memory_providers/json_provider.py path/to/source.json --watch

The optional --watch flag re-runs the pipeline whenever the source file's
mtime changes (polled every 2s). Useful for live-editing memory files.
"""
from __future__ import annotations
import argparse
import json
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from memory_providers import normalize_many, write_canonical

DEFAULT_OUT = Path(__file__).resolve().parents[2] / "assets" / "memories.json"

def load_raws(src: Path) -> list[dict]:
    data = json.loads(src.read_text(encoding="utf-8"))
    if isinstance(data, list):
        return data
    if isinstance(data, dict):
        if isinstance(data.get("memories"), list):
            return data["memories"]
        if isinstance(data.get("items"), list):
            return data["items"]
    raise ValueError(
        f"Unsupported JSON shape in {src}; expected an array or "
        f"{{ memories: [...] }} / {{ items: [...] }}"
    )

def run_once(src: Path, out: Path) -> dict:
    print(f"Reading {src}...", flush=True)
    raws = load_raws(src)
    print(f"  {len(raws)} raw entries")
    memories = normalize_many(raws, provider_id="json")
    stats = write_canonical(memories, provider_id="json", out_path=out)
    print(f"Wrote {stats['path']}  ({stats['n']} memories, {stats['size_kb']} KB)")
    return stats

def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                  formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("source", help="path to a JSON file containing memories")
    ap.add_argument("--out", default=str(DEFAULT_OUT),
                    help=f"output path (default: {DEFAULT_OUT})")
    ap.add_argument("--watch", action="store_true",
                    help="re-run when the source file changes (polled every 2s)")
    args = ap.parse_args()

    src = Path(args.source).resolve()
    out = Path(args.out).resolve()
    if not src.exists():
        ap.error(f"source file not found: {src}")

    run_once(src, out)
    if args.watch:
        print("\nWatching for changes (Ctrl-C to stop)...")
        last_mtime = src.stat().st_mtime
        try:
            while True:
                time.sleep(2)
                mtime = src.stat().st_mtime
                if mtime != last_mtime:
                    print()
                    run_once(src, out)
                    last_mtime = mtime
        except KeyboardInterrupt:
            print("\nStopped.")

if __name__ == "__main__":
    main()
