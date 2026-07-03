"""Backfill map_traces for Phase 4 proposed maps that pre-date Iter 25.

Before Iter 25 (2026-05-14), Phase 4 created memory_maps but didn't link
member memories via map_traces — the association was implicit via the
anchor tag. After Iter 25 the link is created at propose time, but
pre-existing rows still have empty trace lists. Accepting one in the UI
then renders an empty map (the user-visible bug on
map-1778725082855342587).

This script:
  1. Lists every memory_map with non-empty anchor_tags but 0 map_traces
     (excluding dismissed).
  2. For each, finds all memories sharing the first anchor tag and links
     them via POST /maps/{id}/traces.
  3. Reports per-map counts.

LinkMapTrace is idempotent (ON CONFLICT(map_id, trace_id) DO NOTHING),
so the script is safe to re-run.
"""

from __future__ import annotations

import json
import os
import sys
import time
import urllib.request
import urllib.error

CORE_URL = os.environ.get("SD_CORE_URL", "http://localhost:9911")
TOKEN = os.environ["SD_API_TOKEN"]
HEADERS = {"Authorization": f"Bearer {TOKEN}", "Content-Type": "application/json"}


def http_json(method, url, body=None, timeout=30):
    data = json.dumps(body).encode("utf-8") if body is not None else None
    req = urllib.request.Request(url, data=data, headers=HEADERS, method=method)
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        raw = resp.read()
        return json.loads(raw) if raw else {}


def main() -> int:
    print(f"core={CORE_URL}")
    print("loading maps + memories…")
    maps = http_json("GET", f"{CORE_URL}/maps?limit=2000", timeout=120).get("maps", [])
    items = http_json("GET", f"{CORE_URL}/bank/memories?limit=20000", timeout=120).get("memories", [])
    print(f"  maps:     {len(maps)}")
    print(f"  memories: {len(items)}")

    # Pre-index memories by tag for O(1) lookup
    by_tag: dict[str, list[str]] = {}
    for m in items:
        for t in m.get("tags") or []:
            by_tag.setdefault(t, []).append(m["id"])

    # Filter maps that need backfill
    to_fix = []
    for m in maps:
        if m.get("status") == "dismissed":
            continue
        tags = m.get("anchor_tags") or []
        if not tags:
            continue
        # Check current trace count
        try:
            t = http_json("GET", f"{CORE_URL}/maps/{m['id']}/traces?limit=1", timeout=10)
        except Exception as e:
            print(f"  err getting traces for {m['id']}: {e}", file=sys.stderr)
            continue
        if t.get("count", 0) == 0:
            to_fix.append((m["id"], tags[0], m.get("name", "")))

    print(f"\nmaps to backfill: {len(to_fix)}")
    if not to_fix:
        print("nothing to do")
        return 0

    t0 = time.time()
    map_ok = map_skip = link_ok = link_err = 0
    for i, (mid, tag, name) in enumerate(to_fix, 1):
        member_ids = by_tag.get(tag, [])
        if not member_ids:
            map_skip += 1
            print(f"  [{i:>3}/{len(to_fix)}] {mid}  tag={tag!r:<40}  no members; skip")
            continue
        ok = err = 0
        for tid in member_ids:
            try:
                http_json("POST", f"{CORE_URL}/maps/{mid}/traces",
                          {"trace_id": tid}, timeout=10)
                ok += 1
            except Exception:
                err += 1
        link_ok += ok
        link_err += err
        map_ok += 1
        if i % 20 == 0 or i == len(to_fix):
            elapsed = time.time() - t0
            rate = i / max(elapsed, 0.001)
            eta = (len(to_fix) - i) / max(rate, 0.001)
            print(f"  [{i:>3}/{len(to_fix)}] {mid}  tag={tag!r:<32}  linked {ok}  ({elapsed:.0f}s, {rate:.1f} maps/s, eta {eta/60:.1f}m)")

    print()
    print(f"done in {(time.time()-t0)/60:.1f} min")
    print(f"  maps_backfilled: {map_ok}")
    print(f"  maps_skipped:    {map_skip}")
    print(f"  links_created:   {link_ok}")
    print(f"  link_errors:     {link_err}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
