#!/usr/bin/env python3
"""
Build a release-mode artifact of Synaptic that contains zero
personal data — safe to publish, share, demo at conferences, drop into a
public repo, etc.

What it does:

  1. Copies the Dev/ source tree to ``../Prod/`` by default (sibling of Dev).
     ``--out`` overrides the destination.
  2. Swaps ``assets/memories.json`` for ``assets/memories.example.json``
     (run ``tools/generate_synthetic_memories.py`` first if missing).
     Same for ``synapses.json`` if a ``.example`` version exists.
  3. Strips the dev-only ``window.sd = { ... }`` debug exposure from
     ``index.html`` so the dashboard doesn't leak internal state to the
     browser console.
  4. Drops:
        - ``docs/dev/`` (HANDOFF.md, PROJECT_PLAN.md, SHIPPING.md,
                       Anthony_Hindsight_Memory_Guide.md, etc.)
        - ``sd_bank.db*`` (SQLite bank files)
        - any ``hindsight-backup*.json``
        - ``__pycache__`` / ``*.pyc`` / ``node_modules`` / ``dist`` / ``.venv``
        - prior ``synaptic-disorder*.zip`` build artifacts
        - ``release/`` (legacy; current builds go to ``../Prod/``)
  5. Adds a top-level ``RELEASE_BUILD.md`` noting the build mode + date,
     so anyone reading the artifact knows it's a scrubbed snapshot.

Run from inside the Dev/ tree::

    cd "Synaptic Disorder/Dev"
    python tools/build_release.py                          # → ../Prod/
    python tools/build_release.py --out ../Prod            # explicit
    python tools/build_release.py --zip                    # also produces ../synaptic-disorder-<version>.zip
    python tools/build_release.py --zip --github-mirror "D:/repos/synaptic-disorder"
                                                           # also mirrors Prod/ into a git-tracked dir
                                                           # (preserves .git/.gitignore at destination)
    python tools/build_release.py --zip --no-mirror        # skip the mirror step regardless of MIRROR_PATH

Idempotent: re-running blows away the previous output and rebuilds.
"""
from __future__ import annotations

import argparse
import datetime as dt
import os
import re
import shutil
import sys
from pathlib import Path

# Make sibling tools importable without installing the package.
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _sanitize  # noqa: E402

REPO = Path(__file__).resolve().parent.parent

# Release zip target. Channel routing is purely by semver suffix:
#   - vM.F.Pa<n>   -> Releases/Alpha/
#   - vM.F.Pb<n>   -> Releases/Beta/
#   - vM.F.Prc<n>  -> Releases/RC/
#   - Anything else (no suffix) -> Releases/   (stable)
SEMVER_CHANNEL_RE = re.compile(r"^\d+\.\d+\.\d+(?P<sfx>(a|b|rc)?)\d*$")


def _zip_target_dir(version: str) -> Path:
    """Where the release zip should land. Caller is responsible for mkdir."""
    base = REPO.parent / "Releases"
    m = SEMVER_CHANNEL_RE.match(version)
    if m:
        sfx = m.group("sfx")
        if sfx == "a":
            return base / "Alpha"
        if sfx == "b":
            return base / "Beta"
        if sfx == "rc":
            return base / "RC"
        return base
    return base

# Paths (relative to REPO) that should never appear in a release artifact.
EXCLUDE_NAMES = {
    "release",                                # don't copy our own output
    "node_modules",
    "__pycache__",
    "dist",
    ".pytest_cache",
    ".vscode",
    ".idea",
    ".DS_Store",
    "Thumbs.db",
    ".egg-info",
    ".venv",                                  # local Python venv — devtools only
    "venv",                                   # alternate venv name
    ".env",
}

# Path-aware exclusions (relative to REPO, forward-slash form). Use this when
# matching only-by-name would be too broad (e.g. "dev" appears as a directory
# name in places we DO want to ship).
EXCLUDE_RELPATHS = {
    "docs/dev",                               # dev-only docs (HANDOFF, PROJECT_PLAN,
                                              # SHIPPING, Anthony_Hindsight_Memory_Guide)
}
EXCLUDE_PATTERNS = [
    re.compile(r"^sd_bank\.db.*$"),           # SQLite bank + WAL/journal sidecars
    re.compile(r"^hindsight-backup.*\.json$"),
    re.compile(r".*\.pyc$"),
    re.compile(r".*\.egg-info$"),
    # Don't sweep prior release artifacts into the new build — that
    # would double the bundle size every time we re-run. Both the
    # versioned synaptic-disorder-<v>.zip and the unversioned alias
    # get filtered.
    re.compile(r"^synaptic-disorder.*\.zip$"),
    # Deprecated PLY brain meshes (project history; superseded by Allen
    # voxels in assets/voxels.json). ~28 MB combined — drop from releases.
    re.compile(r"^brain_(low|med|high|ultra)\.ply$"),
    re.compile(r"^allen_overlay\.ply$"),
    re.compile(r"^labeled_points\.json$"),
    re.compile(r"^\.region_cache\.json$"),
]

# --- GitHub mirror configuration ---
#
# After Prod/ is built and sanitized, optionally mirror it onto a
# git-tracked directory so the public artifact can be `git push`'d as a
# single commit per build. Configurable in three ways, in priority order:
#
#   1. --github-mirror PATH            CLI flag           (highest priority)
#   2. SD_GITHUB_MIRROR env variable
#   3. MIRROR_PATH constant below      (this file's default)
#
# Pass --no-mirror to skip the step regardless of any configuration.
#
# The mirror preserves anything in MIRROR_PRESERVE at the destination
# root — `.git`, `.github` workflows, `.gitignore`, `.gitattributes` —
# so the destination repo's history and CI rules survive every rebuild.
# Files that exist in the destination but no longer exist in Prod get
# deleted (so the mirror tracks the build exactly).
#
# The MIRROR_PATH constant below is environment-specific. The Prod copy
# of this file gets scrubbed by `_sanitize.py` (which is why
# `tools/build_release.py` is in SANITIZE_FILES), so the constant
# defaults to a /path/to/... placeholder for anyone running the public
# build script — they should set SD_GITHUB_MIRROR or pass the flag.
MIRROR_PATH: Path | None = Path(
    r"/path/to/OneDrive\Computers\your dashboard host 2025"
    r"\Documents\GitHub\Synaptic-Disorder"
)
MIRROR_PRESERVE: set[str] = {".git", ".github", ".gitignore", ".gitattributes"}


def _excluded(name: str, relpath: str = "") -> bool:
    """Return True if this file/dir should be excluded from the release.

    `name` is the leaf name (e.g. "dev"); `relpath` is the path relative to
    REPO with forward-slash separators (e.g. "docs/dev"). The relpath check
    lets us exclude `docs/dev/` without also excluding any other directory
    that happens to be named `dev`.
    """
    if name in EXCLUDE_NAMES:
        return True
    if relpath and relpath in EXCLUDE_RELPATHS:
        return True
    for pat in EXCLUDE_PATTERNS:
        if pat.match(name):
            return True
    return False


def _copy_tree(src: Path, dst: Path, root: Path = None) -> int:
    """Copy src -> dst, skipping excluded names + relpaths. Returns count of files copied."""
    if root is None:
        root = src
    n = 0
    for entry in src.iterdir():
        relpath = entry.relative_to(root).as_posix()
        if _excluded(entry.name, relpath):
            continue
        target = dst / entry.name
        if entry.is_dir():
            target.mkdir(parents=True, exist_ok=True)
            n += _copy_tree(entry, target, root)
        else:
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(entry, target)
            n += 1
    return n


def _strip_user_data_files(dst: Path) -> list[str]:
    """Remove `memories.json` + `synapses.json` from the public artifact.

    Production releases ship without these files. SD Core auto-creates empty
    stubs at runtime (`ensureEmptyAssetFile` in `bridge/core/main.go`) on
    first start, so the dashboard always has something to load. The
    `.example.json` siblings stay in the artifact as a reference / starter
    dataset and as the source the Pages static demo renames-and-uses.
    """
    removed: list[str] = []
    for stem in ("memories", "synapses"):
        live = dst / "assets" / f"{stem}.json"
        if live.exists():
            live.unlink()
            removed.append(f"{stem}.json")
    return removed


# Block of debug-exposure JS that the dev build leaves on window.sd.
# We match it by anchor strings to be resilient to small edits.
DEBUG_EXPOSURE_RE = re.compile(
    r"\n\s*//[^\n]*Dev-only debug handle[^\n]*\n"
    r"(?:\s*//[^\n]*\n)*"                        # subsequent comment lines
    r"\s*window\.sd\s*=\s*\{[^}]*\};\s*\n",
    re.DOTALL,
)


def _strip_debug_exposure(dst: Path) -> bool:
    """Remove the `window.sd = { ... }` block from index.html. Returns True if changed."""
    idx = dst / "index.html"
    if not idx.exists():
        return False
    src = idx.read_text("utf-8")
    new = DEBUG_EXPOSURE_RE.sub("\n", src)
    if new == src:
        # Fallback: simpler line-based strip in case the comment block changed
        new_lines = []
        skip = False
        for line in src.splitlines(keepends=True):
            if "window.sd = " in line and "Dev-only" not in line and skip is False:
                # one-liner pattern
                continue
            new_lines.append(line)
        new = "".join(new_lines)
    if new == src:
        return False
    idx.write_text(new, encoding="utf-8")
    return True


def _write_release_marker(dst: Path, version: str = "") -> None:
    today = dt.date.today().isoformat()
    version_line = f"**Version: `{version}`** | " if version else ""
    (dst / "RELEASE_BUILD.md").write_text(
        f"""# Synaptic - Release Build

{version_line}Generated by `tools/build_release.py` on **{today}**.

This is a **scrubbed snapshot**: no personal data, no debug hooks. Differences from the dev tree:

- `assets/memories.json` and `assets/synapses.json` are absent. SD Core auto-creates empty stubs at first start; the `.example.json` siblings ship as a starter dataset.
- `index.html` has the dev-only `window.sd = ...` debug exposure removed.
- `docs/dev/` (HANDOFFs, planning notes, the Hindsight memory guide) is excluded.
- Any `sd_bank.db*`, `hindsight-backup*.json`, `node_modules/`, `__pycache__/`, `dist/` and similar build detritus are excluded.

To rebuild from a fresh dev tree:

    python tools/generate_synthetic_memories.py   # regenerates synthetic data
    python tools/build_release.py                 # produces ../Prod/

Safe to publish, demo, drop into a public repo, or hand to a friend.
""",
        encoding="utf-8",
    )


def _bundle_zip(out: Path, zip_path: Path, top_level: str) -> int:
    """Create a portable ZIP of the release dir, with all entries rooted
    at `<top_level>/` instead of the on-disk dir name (`Prod`).

    Previously this used shutil.make_archive with root_dir/base_dir, which
    wrapped every entry as `Prod/…`. End-user feedback (2026-05-11):
    `Prod/` is a Dev/Prod-mirror artifact that surprises users who unzip
    expecting `synaptic-<version>/bridge/…` per open-source convention.
    Now we write the ZIP entry-by-entry with explicit arcnames so the
    on-disk tree stays `Prod/` (build pipeline / mirror still want that)
    but the published ZIP is `synaptic-<version>/`.

    Returns total ZIP size in bytes.
    """
    import zipfile
    if zip_path.exists():
        zip_path.unlink()
    with zipfile.ZipFile(zip_path, "w", zipfile.ZIP_DEFLATED, compresslevel=6) as zf:
        for entry in sorted(out.rglob("*")):
            rel = entry.relative_to(out).as_posix()
            arcname = f"{top_level}/{rel}"
            if entry.is_dir():
                # Write empty dir markers so tools that care (Windows Explorer)
                # show the structure even if it has no files. ZipInfo with
                # trailing slash is the conventional shape for a dir entry.
                info = zipfile.ZipInfo(arcname + "/")
                info.external_attr = (0o40755 << 16) | 0x10
                zf.writestr(info, b"")
            else:
                zf.write(entry, arcname)
    return zip_path.stat().st_size


def _sync_to_mirror(src: Path, dst: Path) -> tuple[int, int, int]:
    """Mirror ``src`` onto ``dst``, preserving git metadata at ``dst`` root.

    "Mirror" means: every file in ``src`` ends up at the same relative path
    under ``dst``, and any file under ``dst`` that's NOT in ``src`` gets
    deleted — except files/dirs at the root whose first segment is in
    ``MIRROR_PRESERVE`` (``.git`` etc.), which are left alone so the
    destination's git repo isn't disturbed.

    Returns ``(added_or_updated, removed, unchanged)``.
    """
    dst.mkdir(parents=True, exist_ok=True)

    # Collect every file in src, indexed by its dst-relative path.
    src_paths: set[str] = set()
    for p in src.rglob("*"):
        if p.is_file():
            src_paths.add(p.relative_to(src).as_posix())

    # Copy / overwrite. Skip files whose mtime+size already match (cheap
    # short-circuit; most files in the destination are unchanged build-to-
    # build and shutil.copy2 is the slow part).
    n_copy = 0
    n_unchanged = 0
    for rel in src_paths:
        s = src / rel
        d = dst / rel
        d.parent.mkdir(parents=True, exist_ok=True)
        if d.exists():
            sst = s.stat()
            dst_stat = d.stat()
            if (sst.st_size == dst_stat.st_size
                    and abs(sst.st_mtime - dst_stat.st_mtime) < 1.0):
                n_unchanged += 1
                continue
        shutil.copy2(s, d)
        n_copy += 1

    # Remove any file under dst that isn't in src and isn't preserved.
    n_rm = 0
    for p in list(dst.rglob("*")):
        if not p.is_file():
            continue
        rel = p.relative_to(dst).as_posix()
        first_seg = rel.split("/", 1)[0]
        if first_seg in MIRROR_PRESERVE:
            continue
        if rel not in src_paths:
            try:
                p.unlink()
                n_rm += 1
            except OSError:
                pass  # Locked / sync-in-progress; leave for next run.

    # Sweep up empty directories left behind by the deletions, deepest
    # first. Skip anything under a preserved root.
    for d in sorted(
        (x for x in dst.rglob("*") if x.is_dir()),
        key=lambda x: -len(x.as_posix()),
    ):
        first_seg = d.relative_to(dst).as_posix().split("/", 1)[0]
        if first_seg in MIRROR_PRESERVE:
            continue
        try:
            d.rmdir()  # only succeeds when truly empty
        except OSError:
            pass

    return n_copy, n_rm, n_unchanged


def _resolve_mirror_target(
    cli_flag: Path | None, no_mirror: bool
) -> Path | None:
    """Return the resolved mirror destination, or None if mirroring is off.

    Priority: --github-mirror > SD_GITHUB_MIRROR env > MIRROR_PATH constant.
    --no-mirror short-circuits to None regardless.
    """
    if no_mirror:
        return None
    if cli_flag:
        return cli_flag.expanduser()
    env_val = os.environ.get("SD_GITHUB_MIRROR", "").strip()
    if env_val:
        return Path(env_val).expanduser()
    if MIRROR_PATH:
        return MIRROR_PATH.expanduser()
    return None


# --- Versioning -------------------------------------------------------
#
# Source of truth: the `VERSION` file at the repo root. Whatever it
# contains is the build's version — manual semver like `2.3.0b1`,
# `2.3.0`, `3.0.0rc1`, etc. The build reads it as-is, never mutates
# it (use `--version <new>` to bump explicitly), and stamps the value
# into:
#   - the release copy of index.html (replaces the SD_VERSION constant)
#   - RELEASE_BUILD.md
#   - the zip filename: synaptic-disorder-<version>.zip
#
# Channel routing (Alpha/Beta/RC/stable) is decided by the suffix on
# the semver (see `SEMVER_CHANNEL_RE` above), not by the version itself.
# ---------------------------------------------------------------------

VERSION_FILE = REPO / "VERSION"


def _read_version() -> str:
    if VERSION_FILE.exists():
        v = VERSION_FILE.read_text("utf-8").strip()
        if v:
            return v
    return "0.0.0"


def _write_version(version: str) -> None:
    VERSION_FILE.write_text(version + "\n", encoding="utf-8")


SD_VERSION_RE = re.compile(r"const\s+SD_VERSION\s*=\s*'([^']+)'\s*;")


def _stamp_version_into_index(idx: Path, version: str) -> bool:
    """Rewrite the SD_VERSION constant in an index.html. Works on both
    the dev tree's source and the release copy. Returns True on change.
    """
    if not idx.exists():
        return False
    src = idx.read_text("utf-8")
    new = SD_VERSION_RE.sub(f"const SD_VERSION = '{version}';", src, count=1)
    if new == src:
        return False
    idx.write_text(new, encoding="utf-8")
    return True


# Discord-Nitro single-message ceiling. Conservative — Nitro actually
# accepts up to 4000, but tables-converted-to-bullets and emoji boilerplate
# can run a little long, so we leave ~100 char headroom before splitting.
DISCORD_MSG_CEILING = 3900


def _md_to_discord(notes_md: str) -> str:
    """Mechanical conversion of a canonical release-notes markdown blob
    into Discord-renderable text:
      - Strip `---` horizontal rules (Discord ignores them).
      - Convert markdown tables → bold-prefixed bullets.
      - Strip relative `[label](path.md)` link syntax — keep just the
        label text. Absolute http(s) links are left alone.
      - Collapse runs of 3+ blank lines to 2.
    Does NOT split into messages — that's the caller's job after this
    yields the body text.
    """
    out_lines: list[str] = []
    in_table = False
    table_header: list[str] = []
    for raw in notes_md.splitlines():
        line = raw.rstrip()
        # Horizontal rule → drop.
        if line.strip() in ("---", "***", "___"):
            continue
        # Table detection: a line of `| col | col | …` shape.
        if "|" in line and line.lstrip().startswith("|"):
            cells = [c.strip() for c in line.strip().strip("|").split("|")]
            # Separator row (e.g. `| --- | --- |`) — start table mode.
            if all(set(c) <= set("-: ") and c for c in cells):
                in_table = True
                continue
            if in_table:
                # Data row.
                if table_header and len(cells) == len(table_header):
                    bullet_parts = []
                    for h, v in zip(table_header, cells):
                        if h and v:
                            bullet_parts.append(f"**{h}:** {v}")
                        elif v:
                            bullet_parts.append(v)
                    out_lines.append("- " + " — ".join(bullet_parts))
                else:
                    # Misshapen — fall back to raw text.
                    out_lines.append("- " + " · ".join(cells))
            else:
                # Header row — buffer until we see the separator.
                table_header = cells
            continue
        # We left the table.
        if in_table:
            in_table = False
            table_header = []
        # Strip relative link syntax: [label](something_without_://) → label.
        line = re.sub(r"\[([^\]]+)\]\((?!https?://)[^)]+\)", r"\1", line)
        out_lines.append(line)
    # Squash 3+ blank lines.
    body = "\n".join(out_lines)
    body = re.sub(r"\n{3,}", "\n\n", body).strip() + "\n"
    return body


def _split_for_discord(body: str, ceiling: int = DISCORD_MSG_CEILING) -> list[str]:
    """Split the Discord-converted body into ≤ceiling-char chunks. Split
    at H2 (`## `) boundaries when possible; if a single H2 section is
    larger than the ceiling, split at sub-headers; if still too big, split
    at paragraph boundaries.
    """
    if len(body) <= ceiling:
        return [body]
    # Slice at H2 boundaries (preserves the heading with its content).
    sections = re.split(r"(?m)(?=^## )", body)
    chunks: list[str] = []
    buf = ""
    for sec in sections:
        if not sec.strip():
            continue
        if len(sec) > ceiling:
            # Recursively split this section at H3 / paragraph boundaries.
            subs = re.split(r"(?m)(?=^### )", sec)
            for sub in subs:
                if len(sub) > ceiling:
                    # Last resort: paragraph split.
                    paras = sub.split("\n\n")
                    paragraph_buf = ""
                    for p in paras:
                        if len(paragraph_buf) + len(p) + 2 > ceiling:
                            if paragraph_buf:
                                chunks.append(paragraph_buf.rstrip() + "\n")
                            paragraph_buf = p + "\n\n"
                        else:
                            paragraph_buf += p + "\n\n"
                    if paragraph_buf.strip():
                        chunks.append(paragraph_buf.rstrip() + "\n")
                elif len(buf) + len(sub) > ceiling:
                    if buf:
                        chunks.append(buf.rstrip() + "\n")
                    buf = sub
                else:
                    buf += sub
        elif len(buf) + len(sec) > ceiling:
            if buf:
                chunks.append(buf.rstrip() + "\n")
            buf = sec
        else:
            buf += sec
    if buf.strip():
        chunks.append(buf.rstrip() + "\n")
    return chunks


def _build_discord_notes(notes_path: Path, version: str) -> Path | None:
    """Generate `<notes_path stem>.discord.md` next to the canonical
    notes. Reads the canonical notes, converts tables / links / hrules,
    splits into Discord-Nitro-sized chunks, wraps in copy-paste blocks.
    Returns the output path, or None if notes_path is empty / unreadable.
    """
    if not notes_path.exists():
        return None
    try:
        raw = notes_path.read_text("utf-8").strip()
    except Exception:
        return None
    if not raw:
        return None
    body = _md_to_discord(raw)
    chunks = _split_for_discord(body)
    out_path = notes_path.with_name(notes_path.stem + ".discord.md")
    parts: list[str] = []
    parts.append(f"# Synaptic v{version} — Discord release notes\n")
    if len(chunks) == 1:
        parts.append(
            "Discord Nitro allows up to 4000 chars per message. The full "
            "release notes fit in one. Copy everything between the rule "
            "lines below into a single Discord message.\n"
        )
        parts.append("═" * 71 + "\n─── MESSAGE (single) ───\n" + "═" * 71 + "\n")
        parts.append(chunks[0])
        parts.append("═" * 71 + "\n─── END ───\n" + "═" * 71 + "\n")
    else:
        parts.append(
            f"Discord Nitro caps a message at 4000 chars; this release "
            f"is too long for one message, so it's split into {len(chunks)}. "
            f"Copy each `─── MESSAGE n ───` chunk into a separate Discord "
            f"message.\n"
        )
        for i, chunk in enumerate(chunks, 1):
            parts.append("═" * 71 + f"\n─── MESSAGE {i} ───\n" + "═" * 71 + "\n")
            parts.append(chunk)
        parts.append("═" * 71 + "\n─── END ───\n" + "═" * 71 + "\n")
    out_path.write_text("\n".join(parts), encoding="utf-8")
    return out_path


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--out", type=Path, default=REPO.parent / "Prod",
                    help="Output directory (default: ../Prod, sibling of the Dev/ source tree)")
    ap.add_argument("--zip", action="store_true",
                    help="Also produce a portable synaptic-disorder-<version>.zip alongside the release dir.")
    ap.add_argument("--no-bump", action="store_true",
                    help="Kept for compatibility — version is now ALWAYS sourced from the VERSION file as-is (no auto-mutation). This flag is a no-op.")
    ap.add_argument("--version", default=None,
                    help="Force a specific version string. Overrides whatever is currently in the VERSION file (and rewrites it).")
    ap.add_argument("--github-mirror", type=Path, default=None,
                    help="After building, mirror Prod/ to this git-tracked directory "
                         "(preserves .git/.github/.gitignore at destination). "
                         "Falls back to SD_GITHUB_MIRROR env var, then to the MIRROR_PATH "
                         "constant in this script.")
    ap.add_argument("--no-mirror", action="store_true",
                    help="Skip the GitHub mirror step regardless of any configured path.")
    args = ap.parse_args()

    # ---- Read + (optionally) persist the build version --------------
    #
    # The VERSION file is now the single source of truth. The build reads
    # it as-is, never auto-bumps. Pass `--version <new>` to overwrite the
    # file before building. `--no-bump` is a no-op kept for compatibility.
    current = _read_version()
    version = args.version if args.version else current
    if version != current:
        _write_version(version)
        print(f"version: {current} -> {version}  (VERSION file updated via --version)")
    else:
        print(f"version: {version} (read from VERSION file)")
    # Always re-stamp the dev tree's index.html. Previously this only ran
    # on a version change, which left Dev/index.html drifting when the
    # user pre-bumped VERSION (or invoked --version with the same string
    # already on disk). Drift mattered because the hardcoded constant is
    # the fallback when SD Core's `/VERSION` endpoint isn't reachable
    # (mock-only deployments, Pages), so a stale Dev constant could show
    # the wrong version to users on those paths AND make bug reports
    # ambiguous ("does it say 2.0.2 because that's actually running, or
    # because the constant lagged?"). Now: read the constant, restamp
    # to target if different. Cheap, idempotent, eliminates the trap.
    if _stamp_version_into_index(REPO / "index.html", version):
        print(f"  stamped dev/index.html -> SD_VERSION = '{version}'")
    else:
        print(f"  dev/index.html already at SD_VERSION = '{version}'")

    out: Path = args.out.resolve()
    if out.exists():
        print(f"clearing existing {out}")
        shutil.rmtree(out)
    out.mkdir(parents=True)

    print(f"copying {REPO} -> {out}")
    n = _copy_tree(REPO, out)
    print(f"  {n} files copied")

    removed = _strip_user_data_files(out)
    if removed:
        print(f"stripped user-data files (SD Core auto-creates on first run): {', '.join(removed)}")
    else:
        print("(no user-data files to strip — already absent)")

    if _strip_debug_exposure(out):
        print("stripped window.sd debug exposure from index.html")
    else:
        print("note: window.sd debug exposure not found (already clean?)")

    if _stamp_version_into_index(out / "index.html", version):
        print(f"stamped SD_VERSION = '{version}' into release index.html")
    else:
        # Most common cause: the dev source was already stamped earlier
        # this run, so the regex finds nothing to change. Confirm the
        # constant matches the expected version before deciding it's a
        # real miss.
        try:
            txt = (out / "index.html").read_text("utf-8", errors="replace")
            m = SD_VERSION_RE.search(txt)
            if m and m.group(1) == version:
                print(f"release index.html already has SD_VERSION = '{version}'")
            else:
                print("warning: SD_VERSION constant not found / mismatched in release index.html")
        except Exception:
            print("warning: could not verify SD_VERSION in release index.html")

    # Personal-data sanitization — strips Anthony-specific defaults,
    # branding, and example paths from the public artifact. Authorship
    # metadata in package.json/marketplace.json/CREDITS.md stays. See
    # tools/_sanitize.py for the substitution list.
    print("sanitizing personal references...")
    report = _sanitize.sanitize_tree(out, strip_privacy_note=True)
    print(_sanitize.format_report(report))

    _write_release_marker(out, version)
    print(f"\nrelease build ready at: {out}  (v{version})")

    if args.zip:
        # All current builds use the "synaptic-" zip prefix. The legacy
        # date-coded YY.M.D scheme (which used "synaptic-disorder-") is
        # retired; this path is preserved as a single name so existing
        # download tooling keeps working.
        prefix = "synaptic"
        zip_path = _zip_target_dir(version) / f"{prefix}-{version}.zip"
        zip_path.parent.mkdir(parents=True, exist_ok=True)
        print(f"bundling -> {zip_path}")
        # Top-level dir inside the ZIP mirrors the archive stem so users
        # who `unzip synaptic-2.1.2b1.zip` land in a `synaptic-2.1.2b1/`
        # directory — the open-source convention. Old behavior wrapped
        # everything under `Prod/` which leaked our Dev/Prod-mirror
        # internal naming into end-user paths.
        sz = _bundle_zip(out, zip_path, top_level=f"{prefix}-{version}")
        print(f"  {round(sz / 1024 / 1024, 1)} MB")
        # Also keep an unversioned alias for download links that
        # always point at the latest build in this channel.
        alias = zip_path.parent / f"{prefix}.zip"
        if alias.exists():
            alias.unlink()
        try:
            shutil.copy2(zip_path, alias)
            print(f"  alias: {alias.name} -> {zip_path.name}")
        except Exception as e:
            print(f"  alias copy failed: {e}")
        # Release notes companion. Two paths:
        #   1. Author wrote notes ahead of time at the target path → leave alone.
        #   2. No notes file present → write a stub the author fills in post-build.
        # Optionally: if Dev/CHANGELOG-PENDING.md exists, promote it into place
        # (rename in-tree + copy to the release dir), so the next release starts
        # with a fresh empty pending changelog.
        notes_path = zip_path.parent / f"{prefix}-{version}-NOTES.md"
        pending_path = REPO / "CHANGELOG-PENDING.md"
        if notes_path.exists():
            print(f"  notes: {notes_path.name} (already authored — left alone)")
        elif pending_path.exists() and pending_path.read_text("utf-8").strip():
            try:
                raw = pending_path.read_text("utf-8")
                # Strip the pending-file scaffold so the published notes
                # don't carry "# Pending changelog" or the editor-facing
                # "Edit as you ship work…" boilerplate. The convention is:
                # everything up to and including the first `---` separator
                # is metadata for the dev tree; the body that follows is
                # the actual content. We also drop the redundant
                # "## What changed since vXYZ" heading immediately after
                # the separator (the file title supplies that context).
                body = raw
                if "---" in body:
                    body = body.split("---", 1)[1]
                body = re.sub(r"^\s*##\s*What changed since v?[\w.\-]+\s*\n+", "", body, count=1, flags=re.IGNORECASE)
                body = body.lstrip("\n")
                notes_text = f"# Synaptic v{version}\n\n{body.rstrip()}\n"
                notes_path.write_text(notes_text, encoding="utf-8")
                # Reset the pending changelog for the next release.
                pending_path.write_text(
                    f"# Pending changelog\n\n"
                    f"Edit as you ship work; on next `tools/build_release.py --zip`,\n"
                    f"this file gets promoted to `synaptic-<version>-NOTES.md` next to\n"
                    f"the ZIP and reset to this stub.\n\n"
                    f"---\n\n## What changed since v{version}\n\n- TODO\n",
                    encoding="utf-8",
                )
                print(f"  notes: {notes_path.name} (promoted from CHANGELOG-PENDING.md; pending file reset)")
            except Exception as e:
                print(f"  notes promotion failed: {e}")
        else:
            try:
                today = dt.date.today().isoformat()
                notes_path.write_text(
                    f"# Synaptic v{version}\n\n"
                    f"**Released:** {today}\n\n"
                    f"---\n\n## What changed since the previous release\n\n"
                    f"- TODO: fill in.  Either edit this file directly or, for next time,\n"
                    f"  maintain `Dev/CHANGELOG-PENDING.md` and `tools/build_release.py`\n"
                    f"  will promote it into place at build time.\n",
                    encoding="utf-8",
                )
                print(f"  notes: {notes_path.name} (stub written — fill in before publish)")
            except Exception as e:
                print(f"  notes stub write failed: {e}")

        # Discord-Nitro companion file. Generated from whichever notes
        # path won above (already-authored / promoted / stubbed) so
        # any final hand-edits to the canonical notes flow through.
        # See memory: feedback_discord_notes — user posts releases to a
        # Discord channel with Nitro (4000-char message limit), so this
        # produces a ready-to-copy companion.
        try:
            discord_path = _build_discord_notes(notes_path, version)
            if discord_path:
                print(f"  discord: {discord_path.name}")
        except Exception as e:
            print(f"  discord notes generation failed: {e}")
    else:
        print("Inspect with: ls -la", out)

    # --- GitHub mirror (optional) ---
    # Mirror the (already-sanitized, already-zipped) Prod tree to a
    # git-tracked working copy, preserving .git/.github at the destination.
    # Leaves running `git status` / `git commit` / `git push` to the user
    # so they can review the diff before publishing.
    mirror = _resolve_mirror_target(args.github_mirror, args.no_mirror)
    if args.no_mirror:
        print("\nmirror skipped (--no-mirror)")
    elif mirror is None:
        # No mirror configured at any level — quiet skip.
        pass
    elif not mirror.exists():
        print(f"\nwarning: mirror target does not exist — skipping")
        print(f"  configured target: {mirror}")
        print( "  set SD_GITHUB_MIRROR or pass --github-mirror to override, or"
               " create the directory and run again.")
    else:
        mirror = mirror.resolve()
        print(f"\nmirroring {out} -> {mirror}")
        n_add, n_rm, n_keep = _sync_to_mirror(out, mirror)
        print(f"  +{n_add} added/updated  -{n_rm} removed  ={n_keep} unchanged")
        print(f"  preserved at destination: {', '.join(sorted(MIRROR_PRESERVE))}")
        print(f"  next: cd \"{mirror}\" && git status")


if __name__ == "__main__":
    main()
