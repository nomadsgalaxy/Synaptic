"""
Markdown folder memory provider — Obsidian-style.

Walks a folder of `.md` files. Each file becomes one memory. Optional YAML
frontmatter provides metadata; the body becomes the memory text.

Example .md file:

    ---
    id: obsidian-2024-09-12-1421
    tags: [project:event-tracker, architecture, react]
    context: project:event-tracker
    created_at: 2024-09-12T14:21:00Z
    ---
    We landed on a single-file React build because the deploy pipeline
    couldn't handle multiple bundles. Tradeoff is build time, but it's
    worth it for the hosting story.

If no frontmatter is present, the script derives an id from the filename,
tags from `#hashtags` mentioned in the body, and a created_at from the
file's mtime.

Usage:
    python tools/memory_providers/markdown_provider.py path/to/notes-folder
    python tools/memory_providers/markdown_provider.py path/to/notes-folder --watch

No external dependencies (custom mini-YAML parser handles the simple
shapes — full YAML deps avoided to keep the provider lightweight).
"""
from __future__ import annotations
import argparse
import datetime as dt
import json
import re
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from memory_providers import normalize_many, write_canonical

DEFAULT_OUT = Path(__file__).resolve().parents[2] / "assets" / "memories.json"
HASHTAG_RE = re.compile(r"(?<![\w/])#([\w/_:-]{2,40})\b")

def parse_frontmatter(text: str) -> tuple[dict, str]:
    """Returns (metadata_dict, remaining_body). Supports a tiny subset of YAML:
    key: value (string), key: [a, b, c] (list of strings), key: <quoted string>.
    """
    if not text.startswith("---"):
        return {}, text
    end = text.find("\n---", 3)
    if end == -1:
        return {}, text
    fm_block = text[3:end].strip()
    body = text[end + 4:].lstrip("\n")
    meta: dict = {}
    for line in fm_block.splitlines():
        line = line.rstrip()
        if not line or line.startswith("#"):
            continue
        m = re.match(r"^\s*([\w_-]+)\s*:\s*(.*)$", line)
        if not m:
            continue
        key, val = m.group(1), m.group(2).strip()
        # List form: [a, b, c]
        if val.startswith("[") and val.endswith("]"):
            inner = val[1:-1].strip()
            if inner:
                meta[key] = [s.strip().strip("\"'") for s in inner.split(",")]
            else:
                meta[key] = []
        # Quoted string
        elif (val.startswith('"') and val.endswith('"')) or (val.startswith("'") and val.endswith("'")):
            meta[key] = val[1:-1]
        else:
            meta[key] = val
    return meta, body

def file_to_memory(path: Path) -> dict | None:
    try:
        text = path.read_text(encoding="utf-8")
    except UnicodeDecodeError:
        return None
    meta, body = parse_frontmatter(text)
    body = body.strip()
    if not body:
        return None

    # Derive defaults from filename + filesystem when frontmatter omits them.
    mid = meta.get("id") or path.stem
    tags = meta.get("tags") or []
    if isinstance(tags, str):
        tags = [t.strip() for t in tags.split(",") if t.strip()]
    # Add inline #hashtags from body if not already in tags
    inline = set(HASHTAG_RE.findall(body))
    for t in inline:
        if t.lower() not in (x.lower() for x in tags):
            tags.append(t)
    created_at = meta.get("created_at")
    if not created_at:
        ts = dt.datetime.fromtimestamp(path.stat().st_mtime, tz=dt.timezone.utc)
        created_at = ts.strftime("%Y-%m-%dT%H:%M:%SZ")
    record = {
        "id": str(mid),
        "text": body,
        "tags": tags,
        "created_at": created_at,
    }
    if "context" in meta: record["context"] = str(meta["context"])
    if "fact_type" in meta: record["fact_type"] = str(meta["fact_type"])
    if "updated_at" in meta: record["updated_at"] = str(meta["updated_at"])
    if "linked_to" in meta: record["linked_to"] = list(meta["linked_to"])
    record["source_metadata"] = {"file": str(path), "format": "markdown"}
    return record

def collect_raws(folder: Path) -> list[dict]:
    raws = []
    for p in sorted(folder.rglob("*.md")):
        # Skip hidden / dotfile dirs (.obsidian/, etc.)
        if any(part.startswith(".") for part in p.relative_to(folder).parts):
            continue
        rec = file_to_memory(p)
        if rec:
            raws.append(rec)
    return raws

def run_once(folder: Path, out: Path) -> dict:
    print(f"Walking {folder}...", flush=True)
    raws = collect_raws(folder)
    print(f"  {len(raws)} markdown notes")
    memories = normalize_many(raws, provider_id="markdown")
    stats = write_canonical(memories, provider_id="markdown", out_path=out)
    print(f"Wrote {stats['path']}  ({stats['n']} memories, {stats['size_kb']} KB)")
    return stats

def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                  formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("folder", help="path to a folder of Markdown notes")
    ap.add_argument("--out", default=str(DEFAULT_OUT),
                    help=f"output path (default: {DEFAULT_OUT})")
    ap.add_argument("--watch", action="store_true",
                    help="re-run on any folder change (polled every 3s)")
    args = ap.parse_args()

    folder = Path(args.folder).resolve()
    out = Path(args.out).resolve()
    if not folder.exists() or not folder.is_dir():
        ap.error(f"folder not found: {folder}")

    run_once(folder, out)
    if args.watch:
        print("\nWatching for changes (Ctrl-C to stop)...")
        last_signature = None
        try:
            while True:
                time.sleep(3)
                # Cheap signature: sum of (path, mtime) over .md files
                sig = []
                for p in folder.rglob("*.md"):
                    if any(part.startswith(".") for part in p.relative_to(folder).parts):
                        continue
                    sig.append((str(p), p.stat().st_mtime))
                sig.sort()
                if sig != last_signature:
                    print()
                    run_once(folder, out)
                    last_signature = sig
        except KeyboardInterrupt:
            print("\nStopped.")

if __name__ == "__main__":
    main()
