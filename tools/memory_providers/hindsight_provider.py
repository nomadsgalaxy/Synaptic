"""
Hindsight memory provider.

Two operating modes:

  Mode A — Backup file       (default; works offline)
       Reads a Hindsight backup JSON from
       /path/to/hindsight-backups/hindsight-backup-YYYY-MM-DD.json
       (the format produced by Anthony's Hindsight pagination + bundle script).

  Mode B — Live MCP query    (--live flag)
       Talks to a running hindsight-local-mcp on localhost:8888 (or
       configured port) and paginates through list_memories.
       Requires Hindsight to be running locally.

Either mode normalizes results to the Memory Source Schema v1.0 and writes
to assets/memories.json.

Usage:
    python tools/memory_providers/hindsight_provider.py
    python tools/memory_providers/hindsight_provider.py --backup path/to/backup.json
    python tools/memory_providers/hindsight_provider.py --live --type observation
"""
from __future__ import annotations
import argparse
import json
import sys
import urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from memory_providers import normalize_many, write_canonical

DEFAULT_OUT = Path(__file__).resolve().parents[2] / "assets" / "memories.json"
DEFAULT_BACKUP_DIR = Path("/path/to/hindsight-backups")

# ----- Mode A: backup file ------------------------------------------------
def latest_backup(dir_: Path) -> Path | None:
    """Return the most recent hindsight-backup-YYYY-MM-DD.json under dir_."""
    if not dir_.exists():
        return None
    candidates = sorted(dir_.glob("hindsight-backup-*.json"), reverse=True)
    return candidates[0] if candidates else None

def load_from_backup(path: Path, *, fact_type: str | None) -> list[dict]:
    """Read a Hindsight backup JSON. The bundle structure is producer-specific
    so we accept either { 'memories': [...] } or { 'items': [...] } at the
    top level, or a list of memory records directly.
    """
    data = json.loads(path.read_text(encoding="utf-8"))
    if isinstance(data, list):
        items = data
    elif isinstance(data, dict):
        items = (data.get("memories") or data.get("items")
                 or data.get("memory_units") or [])
    else:
        items = []
    if fact_type:
        items = [m for m in items if m.get("fact_type") == fact_type]
    return items

# ----- Mode B: live MCP --------------------------------------------------
def query_live(host: str, port: int, *, fact_type: str | None,
               max_pages: int = 50) -> list[dict]:
    """Page through hindsight-local-mcp list_memories via JSON-RPC over HTTP."""
    base = f"http://{host}:{port}"
    out = []
    offset = 0
    page_size = 200
    for _ in range(max_pages):
        body = {
            "jsonrpc": "2.0", "id": offset,
            "method": "tools/call",
            "params": {
                "name": "list_memories",
                "arguments": {
                    "limit": page_size, "offset": offset,
                    **({"type": fact_type} if fact_type else {}),
                },
            },
        }
        req = urllib.request.Request(
            base + "/mcp",
            data=json.dumps(body).encode("utf-8"),
            headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=15) as resp:
                payload = json.loads(resp.read().decode("utf-8"))
        except Exception as e:
            print(f"  [error] {e}", flush=True)
            break
        result = payload.get("result", {})
        # MCP wraps tool results in a content array
        if "content" in result and isinstance(result["content"], list):
            for c in result["content"]:
                if c.get("type") == "text":
                    parsed = json.loads(c["text"])
                    items = parsed.get("items", [])
                    out.extend(items)
                    if len(items) < page_size:
                        return out
                    offset += page_size
                    break
            else:
                break
        else:
            break
    return out

# ----- Common normalization ----------------------------------------------
def hindsight_to_schema(raw: dict) -> dict:
    """Map a raw Hindsight memory record onto our schema. Hindsight uses
    `mentioned_at` for timestamp; we keep it AND mirror to created_at if
    not already set so downstream tooling has a consistent field.
    """
    rec = {"id": raw["id"], "text": raw.get("text", "")}
    for k in ("tags", "context", "fact_type", "mentioned_at",
              "created_at", "updated_at"):
        if k in raw and raw[k] is not None:
            rec[k] = raw[k]
    if "created_at" not in rec and "mentioned_at" in rec:
        rec["created_at"] = rec["mentioned_at"]
    extras = {k: v for k, v in raw.items()
              if k not in rec and k != "id" and k != "text"}
    if extras:
        rec["source_metadata"] = extras
    return rec

def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                  formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--backup", help="explicit backup file path "
                    f"(default: latest in {DEFAULT_BACKUP_DIR})")
    ap.add_argument("--live", action="store_true",
                    help="query the running hindsight-local-mcp instead of a backup")
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=8888)
    ap.add_argument("--type", default="all",
                    choices=["all", "observation", "world", "experience", "opinion"],
                    help="fact_type filter (default: all — imports every type)")
    ap.add_argument("--out", default=str(DEFAULT_OUT))
    args = ap.parse_args()

    fact_type = None if args.type == "all" else args.type
    if args.live:
        print(f"Querying hindsight-local-mcp at {args.host}:{args.port} "
              f"(type={args.type})...", flush=True)
        raws = query_live(args.host, args.port, fact_type=fact_type)
    else:
        backup_path = Path(args.backup) if args.backup else latest_backup(DEFAULT_BACKUP_DIR)
        if not backup_path or not backup_path.exists():
            sys.exit(f"No backup file found. Pass --backup PATH or place one in "
                     f"{DEFAULT_BACKUP_DIR}.")
        print(f"Reading backup {backup_path}...", flush=True)
        raws = load_from_backup(backup_path, fact_type=fact_type)

    print(f"  {len(raws)} raw Hindsight entries")
    intermediate = [hindsight_to_schema(r) for r in raws]
    memories = normalize_many(intermediate, provider_id="hindsight")
    stats = write_canonical(memories, provider_id="hindsight", out_path=args.out)
    print(f"Wrote {stats['path']}  ({stats['n']} memories, {stats['size_kb']} KB)")

if __name__ == "__main__":
    main()
