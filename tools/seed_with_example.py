#!/usr/bin/env python3
"""
Seed a fresh install with the synthetic example dataset.

Production builds ship with no `assets/memories.json` or `assets/synapses.json`
— SD Core auto-creates empty stubs on first start, so the dashboard launches
with an empty brain and the user populates it via their adapter / memory
provider (see `docs/MEMORY_AUTHORING_GUIDE.md`).

Some users prefer the dashboard to look populated on first launch — to demo
it, to show what a "loaded brain" looks like, or just to have something to
look at while their real memory backend is wired up. This script copies the
synthetic example data over the empty stub:

    assets/memories.example.json  →  assets/memories.json
    assets/synapses.example.json  →  assets/synapses.json

The synthetic data carries `"synthetic": true` at the top of memories.json,
which SD Core uses as a flag — when the first real (non-`sd-core-*`) adapter
fires a `memory_added` event, SD Core wipes the seed data automatically and
the brain transitions to your real activity. So seeding is safe; the seed
is self-disposing.

Usage::

    cd /path/to/synaptic-disorder
    python tools/seed_with_example.py

Or, after installing into AppData::

    cd $LOCALAPPDATA/SynapticDisorder/bin
    python tools/seed_with_example.py --data-dir ../data

The script is idempotent — running it again just refreshes the seed.

Reverse: just delete `assets/memories.json` and restart SD Core. The empty
stub will be recreated automatically.
"""
from __future__ import annotations

import argparse
import shutil
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--data-dir", type=Path, default=REPO,
                    help="Root containing assets/ (default: project root above tools/)")
    ap.add_argument("--force", action="store_true",
                    help="Overwrite existing assets/memories.json even if it doesn't carry the synthetic flag.")
    args = ap.parse_args()

    assets = args.data_dir / "assets"
    if not assets.is_dir():
        print(f"error: {assets} not found — pass --data-dir explicitly.", file=sys.stderr)
        sys.exit(2)

    pairs = [
        ("memories.example.json", "memories.json"),
        ("synapses.example.json", "synapses.json"),
    ]

    seeded = 0
    for ex_name, live_name in pairs:
        ex = assets / ex_name
        live = assets / live_name
        if not ex.exists():
            print(f"  skip: {ex_name} not present (run tools/generate_synthetic_memories.py first?)")
            continue

        if live.exists() and not args.force:
            # Refuse to overwrite real data. Detection: read the live file
            # and check for the synthetic flag.
            try:
                head = live.read_text("utf-8")[:512]
            except Exception:
                head = ""
            if '"synthetic": true' not in head and '"synthetic":true' not in head:
                print(f"  skip: {live_name} appears to contain real data (no synthetic flag).")
                print(f"        Pass --force to overwrite anyway.")
                continue

        shutil.copy2(ex, live)
        print(f"  seeded: {ex_name} -> {live_name}")
        seeded += 1

    if seeded == 0:
        print("\nNothing was seeded.")
        sys.exit(1)
    print(f"\nDone. {seeded} file(s) seeded with synthetic data.")
    print("The seed will be wiped automatically when your first real")
    print("adapter fires a memory_added event — see SD Core's logs for")
    print("'synthetic-wipe: cleared seed data...' when that happens.")


if __name__ == "__main__":
    main()
