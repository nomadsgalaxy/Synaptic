#!/usr/bin/env python3
"""
Build a GitHub Pages / Cloudflare Pages deployable from the Dev/ source tree.

What it does:

  1. Copies the dashboard SHELL only — `index.html`, `assets/`, `VERSION`,
     and the public `docs/CREDITS.md` (for attribution per CC BY 4.0). Skips
     every server-side asset (`bridge/`, `tools/`, `docs/dev/`, `serve.py`,
     `docker-compose.yml`, etc.) — Pages-class hosts can't run them.
  2. Swaps `assets/memories.json` and `assets/synapses.json` for their
     `.example.json` synthetic siblings. No real personal data ever ships.
  3. Strips the dev-only `window.sd = ...` debug exposure from `index.html`,
     same as the prod build.
  4. Injects a `<meta name="sd-core-url" content="">` tag and a small
     "static demo" banner div via a stamp pass on `index.html`. The dashboard
     resolves SD Core URL from the meta tag → falls back to same-origin →
     fails to connect (no `:9911` here) → mock engine drives the brain.
     Viewers see real-feeling activity even though no agent is connected.
  5. Writes `Pages/_pages_README.md` documenting what this build is for.

Run from inside the Dev/ tree::

    cd "Synaptic Disorder/Dev"
    python tools/build_pages.py                                # → ../Pages/
    python tools/build_pages.py --out /path/to/dist/sd-pages   # custom

Idempotent: re-running blows away the previous output and rebuilds.

Why not share build_release.py? The selection criteria are different. Release
keeps the full repo (so users can `docker compose up`); Pages keeps only what
a static host can serve, plus a banner explaining what it is.
"""
from __future__ import annotations

import argparse
import datetime as dt
import re
import shutil
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import _sanitize  # noqa: E402

REPO = Path(__file__).resolve().parent.parent

# Whitelist of relative paths to copy into the Pages output. Anything not
# listed is dropped. Order doesn't matter; we walk and copy.
PAGES_INCLUDE = [
    "index.html",
    "VERSION",
    "assets",                    # full dir; we'll swap .example data below
    "vendor",                    # gif.js + worker for the GIF Exporter feature
    "docs/CREDITS.md",           # CC BY 4.0 mandate (Allen Atlas)
    "docs/data_privacy.md",      # so visitors can see how data is handled
]

# Files / directories never to copy even if a parent is whitelisted.
PAGES_EXCLUDE_NAMES = {
    ".region_cache.json",
    "labeled_points.json",       # legacy, not used by the dashboard
    # Legacy PLY meshes — superseded by Allen voxels in voxels.json.
    # ~28 MB combined; no reason to ship in a static demo.
    "brain_low.ply",
    "brain_med.ply",
    "brain_high.ply",
    "brain_ultra.ply",
    "allen_overlay.ply",
}

# Files in assets/ that ship with synthetic data substituted.
ASSETS_DATA_SWAPS = {
    "memories.json": "memories.example.json",
    "synapses.json": "synapses.example.json",
}

# Stamp markers — strings injected into index.html so the dashboard knows
# it's running in Pages mode.
PAGES_META_TAG = (
    '<meta name="sd-core-url" content="">\n'
    '<meta name="sd-pages-demo" content="1">\n'
)

PAGES_BANNER_HTML = """
<!-- Static-demo banner — injected by build_pages.py. Visible at the top of the page,
     with a close button. Targets stay readable across the dashboard's themes. -->
<div id="sd-pages-banner" style="position:fixed;top:0;left:0;right:0;z-index:9999;
     padding:8px 14px;background:#0a1216;border-bottom:1px solid #1a2630;
     color:#7af2ff;font-family:ui-sans-serif,system-ui,sans-serif;font-size:12px;
     letter-spacing:0.04em;display:flex;justify-content:space-between;align-items:center;">
  <span>STATIC DEMO · brain runs on simulated activity. To wire your own AI agents,
    visit <a href="https://github.com/nomadsgalaxy/Synaptic-Disorder"
            style="color:#66ff99;text-decoration:underline;">github.com/nomadsgalaxy/Synaptic-Disorder</a>.</span>
  <button onclick="this.parentElement.remove()"
          style="background:transparent;border:1px solid #1a2630;color:#7af2ff;
                 padding:2px 8px;cursor:pointer;font:inherit;">close</button>
</div>
"""


def _copy_path(src: Path, dst_root: Path) -> int:
    """Copy a single relpath from REPO into dst_root, preserving structure.
    Returns count of files copied."""
    rel = Path(src.name) if src.parent == REPO else src.relative_to(REPO)
    target = dst_root / rel
    if src.is_file():
        if src.name in PAGES_EXCLUDE_NAMES:
            return 0
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(src, target)
        return 1
    elif src.is_dir():
        n = 0
        for entry in src.iterdir():
            if entry.name in PAGES_EXCLUDE_NAMES:
                continue
            sub_target = target / entry.relative_to(src)
            if entry.is_dir():
                sub_target.mkdir(parents=True, exist_ok=True)
                for inner in entry.rglob("*"):
                    if inner.is_file() and inner.name not in PAGES_EXCLUDE_NAMES:
                        rel_inner = inner.relative_to(src)
                        out_inner = target / rel_inner
                        out_inner.parent.mkdir(parents=True, exist_ok=True)
                        shutil.copy2(inner, out_inner)
                        n += 1
            else:
                target.mkdir(parents=True, exist_ok=True)
                shutil.copy2(entry, target / entry.name)
                n += 1
        return n
    return 0


def _swap_synthetic_assets(dst: Path) -> list[str]:
    """Replace `live` files with their `.example` siblings, then DELETE the
    `.example` originals so we don't ship the same data twice in the Pages
    deployable. The dev tree keeps both; Pages keeps only the one the
    dashboard actually fetches."""
    swapped = []
    for live, ex in ASSETS_DATA_SWAPS.items():
        ex_path = dst / "assets" / ex
        live_path = dst / "assets" / live
        if ex_path.exists():
            shutil.copy2(ex_path, live_path)
            ex_path.unlink()  # remove the .example duplicate
            swapped.append(live)
    return swapped


SD_VERSION_RE = re.compile(r"const\s+SD_VERSION\s*=\s*'([^']+)'\s*;")
DEBUG_EXPOSURE_RE = re.compile(
    r"\n\s*//[^\n]*Dev-only debug handle[^\n]*\n"
    r"(?:\s*//[^\n]*\n)*"
    r"\s*window\.sd\s*=\s*\{[^}]*\};\s*\n",
    re.DOTALL,
)


def _stamp_index(dst: Path) -> None:
    """Strip debug exposure + inject pages meta tag + banner into index.html."""
    idx = dst / "index.html"
    if not idx.exists():
        return
    src = idx.read_text("utf-8")

    # 1. Strip debug exposure (same as release build)
    src = DEBUG_EXPOSURE_RE.sub("\n", src)

    # 2. Inject meta tag inside <head>. Place it right after the charset meta
    # so it's near the top.
    src = re.sub(
        r"(<meta\s+charset[^>]*>\s*)",
        r"\1\n    " + PAGES_META_TAG.strip() + "\n    ",
        src, count=1,
    )

    # 3. Inject banner immediately after <body>.
    src = re.sub(
        r"(<body[^>]*>)",
        r"\1\n" + PAGES_BANNER_HTML,
        src, count=1,
    )

    idx.write_text(src, encoding="utf-8")


def _write_pages_readme(dst: Path, version: str) -> None:
    today = dt.date.today().isoformat()
    (dst / "_pages_README.md").write_text(
        f"""# Synaptic — static Pages build (v{version})

Generated by `tools/build_pages.py` on **{today}**.

This directory is the GitHub Pages / Cloudflare Pages deployable. It contains
only what a static host needs:

- `index.html` — the dashboard (with debug hooks stripped, demo banner injected)
- `assets/` — synthetic memory + voxel data (zero real personal data)
- `docs/CREDITS.md` — Allen Atlas + Three.js attribution
- `docs/data_privacy.md` — data-handling reference
- `VERSION`

Everything server-side (`bridge/`, `tools/`, `docs/dev/`, `docker-compose.yml`)
is intentionally absent. The dashboard's bridge connection fails to find a
local SD Core, falls back to the mock event engine after a few seconds, and
the brain runs on simulated activity.

## Deploy

GitHub Pages: push the contents of this directory to a `gh-pages` branch
(or to `main` and configure Pages to serve from `/Pages`). Cloudflare Pages:
connect the repo and point the build output to this directory.

No build step. No backend. No env vars.

## Wiring real activity

Viewers can paste a remote SD Core URL + Bearer token into the dashboard's
Config panel (Bridge section) at runtime. That points the in-page
`BridgeClient` at any SD Core they can reach, and the brain switches from
mock to live without redeploy. See `docs/REMOTE.md` in the repo for the
full deployment guide.

This build is regenerated each time `tools/build_pages.py` runs from the Dev
tree — never edit files here directly; they'll be overwritten on next build.
""",
        encoding="utf-8",
    )


def _read_version() -> str:
    vf = REPO / "VERSION"
    if vf.exists():
        v = vf.read_text("utf-8").strip()
        if v:
            return v
    return "unknown"


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--out", type=Path, default=REPO.parent / "Pages",
                    help="Output directory (default: ../Pages, sibling of Dev/)")
    args = ap.parse_args()

    out: Path = args.out.resolve()
    if out.exists():
        print(f"clearing existing {out}")
        shutil.rmtree(out)
    out.mkdir(parents=True)

    version = _read_version()

    # Copy whitelisted paths
    print(f"copying Pages-relevant subset of {REPO} -> {out}")
    n_total = 0
    for relpath in PAGES_INCLUDE:
        src = REPO / relpath
        if not src.exists():
            print(f"  skip (missing): {relpath}")
            continue
        n = _copy_path(src, out)
        n_total += n
        print(f"  + {relpath} ({n} files)")

    swapped = _swap_synthetic_assets(out)
    if swapped:
        print(f"swapped synthetic data -> {', '.join(swapped)}")
    else:
        print("WARNING: no .example.json files found in assets/ — Pages would ship real data!")
        print("         run tools/generate_synthetic_memories.py first.")

    _stamp_index(out)
    print("stamped index.html: stripped debug exposure, injected pages meta + banner")

    # Personal-data sanitization (same logic as Prod build).
    print("sanitizing personal references...")
    report = _sanitize.sanitize_tree(out, strip_privacy_note=True)
    print(_sanitize.format_report(report))

    _write_pages_readme(out, version)
    print(f"wrote {out / '_pages_README.md'}")

    print(f"\nPages build ready at: {out}  (v{version})  [{n_total} files copied]")


if __name__ == "__main__":
    main()
