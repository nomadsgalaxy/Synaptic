#!/usr/bin/env python3
"""
Install Synaptic into the per-user AppData location.

Run this from inside an unzipped Synaptic source tree (the directory
that contains `index.html`, `bridge/`, `tools/`, `docker-compose.yml`):

    python tools/install_to_appdata.py              # fresh install
    python tools/install_to_appdata.py --update     # update an existing install
                                                    #   (preserves user data)
    python tools/install_to_appdata.py --uninstall  # remove the install
    python tools/install_to_appdata.py --uninstall --purge
                                                    # also remove Docker volumes
    python tools/install_to_appdata.py --force      # overwrite existing install
                                                    #   without preserving user data
    python tools/install_to_appdata.py --root /custom/path
                                                    # override target location

Per-OS install location (current — Synaptic):

  Windows:  %LOCALAPPDATA%\\Synaptic
  macOS:    ~/Library/Application Support/Synaptic
  Linux:    $XDG_DATA_HOME/synaptic  (or ~/.local/share/synaptic)

Legacy install location (<2.1.0b1, when the product was branded
"Synaptic Disorder"):

  Windows:  %LOCALAPPDATA%\\SynapticDisorder
  macOS:    ~/Library/Application Support/SynapticDisorder
  Linux:    $XDG_DATA_HOME/synaptic-disorder

If a legacy install is detected, the installer prints a warning and points
at docs/dev/Legacy migration paths.md for the manual migration recipe (move
the install tree, then re-mount the Docker bank volume).

The installer copies the source tree into the install root, generates a
default `env` file with sensible defaults, and writes `_meta/` markers
(installed version, install date, last action) so subsequent --update and
--uninstall calls can find their target.

User data — `assets/memories.json`, `assets/synapses.json`,
`assets/thought_bubbles.json`, and the `.env` file — is preserved across
--update runs. Docker volumes (bank + Ollama models) are managed by Docker
Desktop independently and survive uninstall unless `--purge` is passed.

After install, run::

    cd <install root>
    docker compose up -d

Then open http://localhost:9911.
"""
from __future__ import annotations

import argparse
import datetime as dt
import os
import platform
import shutil
import subprocess
import sys
from pathlib import Path

# ----------------------------------------------------------------------------
# Configuration
# ----------------------------------------------------------------------------

# Files / directories to copy from the source tree into the install root.
# Anything not listed here doesn't ship to the install. Order doesn't matter.
INSTALL_INCLUDE: tuple[str, ...] = (
    "index.html",
    "VERSION",
    "README.md",
    "docker-compose.yml",
    "serve.py",
    "serve.bat",
    "serve.sh",
    "start.bat",
    ".gitignore",
    "assets",
    "bridge",
    "docs",
    "tools",
    "vendor",
    "config",
    ".claude-plugin",
)

# Paths (relative to install root, forward-slash form) that the --update
# command will NOT overwrite. These are user data and user-customized
# config — preserved across version upgrades.
#
# `.env` is the dot-file Docker Compose auto-reads for ${VAR:-} substitution;
# the legacy `env` (no dot) name is kept here too so updates from <2.0.2b2
# installs don't wipe whatever the user may have hand-edited there (though it
# was inert — compose never read it).
USER_DATA_PATHS: frozenset[str] = frozenset({
    "assets/memories.json",
    "assets/synapses.json",
    "assets/thought_bubbles.json",
    ".env",
    "env",
})

# Names that are skipped wherever they appear in the copy walk. Build
# detritus, dev-only directories, deprecated PLY meshes, and other things
# that shouldn't ship to an end-user install.
COPY_EXCLUDE_NAMES: frozenset[str] = frozenset({
    "node_modules",
    "__pycache__",
    "dist",
    ".pytest_cache",
    ".vscode",
    ".idea",
    ".DS_Store",
    "Thumbs.db",
    ".egg-info",
    ".venv",
    "venv",
    ".env",
    "release",
    "Prod",
    "Pages",
    "Releases",
    "Dev",
    "_meta",
    "sd_bank.db",
    "sd_bank.db-wal",
    "sd_bank.db-shm",
    # Legacy PLY brain meshes (~28 MB combined) — superseded by the Allen
    # voxel cloud in voxels.json.
    "brain_low.ply",
    "brain_med.ply",
    "brain_high.ply",
    "brain_ultra.ply",
    "allen_overlay.ply",
    "labeled_points.json",
    ".region_cache.json",
})

# Specific paths (relative to the source root, forward-slash form) skipped
# at copy time. These are user-data files, dev-only directories, and
# beta-only docs.
COPY_EXCLUDE_RELPATHS: frozenset[str] = frozenset({
    # Beta-only runbooks (setup docs, not for the runtime install)
    "Agent-Install.md",
    "Beta-Upgrade.md",
    # Dev-only docs — internal handoff / roadmap / personal Hindsight guide.
    # Matches build_release.py's exclusion of the same directory.
    "docs/dev",
    # User data — never ship the source tree's live data into a fresh install.
    # SD Core auto-creates empty stubs on first run; the .example.json
    # siblings ship as the synthetic seed for opt-in via tools/seed_with_example.py.
    "assets/memories.json",
    "assets/synapses.json",
    "assets/thought_bubbles.json",
})

DEFAULT_ENV_BODY = """# Synaptic runtime config -- referenced by docker-compose.yml.
#
# Leave SD_API_TOKEN empty for localhost-only mode (no auth on the API).
# For remote operation behind a Cloudflare Tunnel or Tailscale, generate a
# token with `openssl rand -hex 24` and use the same value on every adapter
# that talks to this SD Core.
#
# SD_OLLAMA_TIER1_MODEL controls which model the always-on Tier 1 Ollama
# container pulls on first launch. `llama3.2:3b` is the default; smaller
# alternatives for Pi-class hardware are listed in docker-compose.yml.
# SD_OLLAMA_TIER2_MODEL controls the on-demand Tier 2 burst container
# (only spun up when the `ollama-tier2` compose profile is enabled).

SD_API_TOKEN=
SD_OLLAMA_TIER1_MODEL=llama3.2:3b
SD_OLLAMA_TIER2_MODEL=llama3.1:8b
"""


# ----------------------------------------------------------------------------
# Path helpers
# ----------------------------------------------------------------------------

def install_root() -> Path:
    """Per-OS default install location."""
    if sys.platform == "win32":
        base = os.environ.get("LOCALAPPDATA")
        if not base:
            base = str(Path.home() / "AppData" / "Local")
        return Path(base) / "Synaptic"
    if sys.platform == "darwin":
        return Path.home() / "Library" / "Application Support" / "Synaptic"
    # Linux + everything else: XDG_DATA_HOME or ~/.local/share
    xdg = os.environ.get("XDG_DATA_HOME")
    base = Path(xdg) if xdg else Path.home() / ".local" / "share"
    return base / "synaptic"


def legacy_install_root() -> Path:
    """Per-OS install location used by <2.1.0b1 (when the product was still
    branded 'Synaptic Disorder'). Used only to detect stale installs and
    point the user at the migration recipe — never written to."""
    if sys.platform == "win32":
        base = os.environ.get("LOCALAPPDATA")
        if not base:
            base = str(Path.home() / "AppData" / "Local")
        return Path(base) / "SynapticDisorder"
    if sys.platform == "darwin":
        return Path.home() / "Library" / "Application Support" / "SynapticDisorder"
    xdg = os.environ.get("XDG_DATA_HOME")
    base = Path(xdg) if xdg else Path.home() / ".local" / "share"
    return base / "synaptic-disorder"


def source_root() -> Path:
    """Directory containing this script's parent (i.e. the unzipped tree)."""
    return Path(__file__).resolve().parent.parent


# ----------------------------------------------------------------------------
# Copy logic
# ----------------------------------------------------------------------------

def _copy_one(src: Path, dst: Path, *, preserve_user_data: bool, root_src: Path) -> int:
    """Copy `src` to `dst` recursively, applying excludes + user-data rules.
    Returns count of files copied."""
    if src.name in COPY_EXCLUDE_NAMES:
        return 0
    relpath = src.relative_to(root_src).as_posix()
    # Path-aware exclusion — applied to BOTH files and directories so
    # subtrees like `docs/dev/` get pruned at entry rather than walked into.
    if relpath in COPY_EXCLUDE_RELPATHS:
        return 0

    if src.is_dir():
        n = 0
        for entry in src.iterdir():
            n += _copy_one(entry, dst / entry.name,
                           preserve_user_data=preserve_user_data, root_src=root_src)
        return n

    if preserve_user_data and relpath in USER_DATA_PATHS and dst.exists():
        # Don't clobber existing user data on update.
        return 0
    dst.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(src, dst)
    return 1


# ----------------------------------------------------------------------------
# Meta
# ----------------------------------------------------------------------------

def detect_existing_install(root: Path) -> str | None:
    """Return the installed version string if a prior install exists, else None."""
    meta_v = root / "_meta" / "installed_version"
    if not meta_v.exists():
        return None
    try:
        return meta_v.read_text("utf-8").strip()
    except Exception:
        return None


def read_source_version(src: Path) -> str:
    v = src / "VERSION"
    if v.exists():
        return v.read_text("utf-8").strip()
    return "unknown"


def write_meta(root: Path, version: str, action: str) -> None:
    meta_dir = root / "_meta"
    meta_dir.mkdir(parents=True, exist_ok=True)
    (meta_dir / "installed_version").write_text(version + "\n", encoding="utf-8")
    (meta_dir / "install_date").write_text(
        dt.date.today().isoformat() + "\n", encoding="utf-8")
    (meta_dir / "last_action").write_text(
        f"{action} {dt.datetime.now().isoformat()}\n", encoding="utf-8")


def write_default_env_if_missing(root: Path) -> bool:
    # Compose auto-reads `.env` (dot) from the project dir for ${VAR:-}
    # substitution. Earlier installs wrote `env` (no dot) which compose
    # silently ignored, so SD_API_TOKEN / SD_OLLAMA_TIER1_MODEL etc. never
    # reached containers. Write the dotted name; if a legacy `env` exists,
    # leave it (might contain user edits, though they were inert).
    env = root / ".env"
    if env.exists():
        return False
    env.write_text(DEFAULT_ENV_BODY, encoding="utf-8")
    return True


# ----------------------------------------------------------------------------
# Commands
# ----------------------------------------------------------------------------

def cmd_install(src: Path, root: Path, *, force: bool) -> None:
    existing = detect_existing_install(root)
    if existing and not force:
        print(f"error: install already exists at {root} (v{existing}).", file=sys.stderr)
        print("Use --update to update in place (preserves user data),",
              file=sys.stderr)
        print("or --force to clobber (does not preserve user data).",
              file=sys.stderr)
        sys.exit(2)
    if existing and force:
        print(f"--force given; clobbering existing install at {root} (was v{existing})")
        # Wipe so the install is truly fresh.
        shutil.rmtree(root)

    root.mkdir(parents=True, exist_ok=True)
    n_files = 0
    for relpath in INSTALL_INCLUDE:
        s = src / relpath
        if not s.exists():
            continue
        d = root / relpath
        n_files += _copy_one(s, d, preserve_user_data=False, root_src=src)

    env_created = write_default_env_if_missing(root)
    new_version = read_source_version(src)
    write_meta(root, new_version, "install")

    print()
    print(f"Installed Synaptic v{new_version} to {root}")
    print(f"  ({n_files} files copied)")
    if env_created:
        print(f"  Wrote default .env file at {root / '.env'}")
        print(f"  Edit it to set SD_API_TOKEN if you plan to expose SD Core remotely.")
    print()
    print("Next steps:")
    if sys.platform == "win32":
        print(f'  cd "{root}"')
    else:
        print(f"  cd {root}")
    # `--build` forces a rebuild of the SD Core image from the freshly-installed
    # source. Without it, `docker compose up` would happily reuse a cached
    # `synaptic-disorder/core:latest` image from a prior install — which is the
    # most common cause of "the dashboard 404s on memories.json" reports
    # (cached image predates the auto-create-empty-stub logic).
    print("  docker compose up --build -d")
    print()
    print("Then open http://localhost:9911")


def cmd_update(src: Path, root: Path) -> None:
    existing = detect_existing_install(root)
    if not existing:
        print(f"error: no install at {root} to update.", file=sys.stderr)
        print("Run without --update for a fresh install.", file=sys.stderr)
        sys.exit(2)
    new_version = read_source_version(src)
    if existing == new_version:
        print(f"already at v{new_version} — nothing to update.")
        # Still write the meta in case install_date should refresh.
        write_meta(root, new_version, "update-noop")
        return

    print(f"updating {root}: v{existing} -> v{new_version}")
    print(f"(user data preserved: {', '.join(sorted(USER_DATA_PATHS))})")
    print()

    n_files = 0
    for relpath in INSTALL_INCLUDE:
        s = src / relpath
        if not s.exists():
            continue
        d = root / relpath
        n_files += _copy_one(s, d, preserve_user_data=True, root_src=src)

    write_meta(root, new_version, "update")
    print()
    print(f"Updated to v{new_version} ({n_files} files refreshed). User data preserved.")
    print()
    print("Restart the stack to pick up the new code:")
    if sys.platform == "win32":
        print(f'  cd "{root}"')
    else:
        print(f"  cd {root}")
    # `--build` forces a rebuild of the SD Core image from the freshly-updated
    # source. Without it, `docker compose up` would happily reuse the prior
    # cached image, defeating the point of the update.
    print("  docker compose down && docker compose up --build -d")


def cmd_uninstall(root: Path, *, purge: bool) -> None:
    if not root.exists():
        print(f"nothing to uninstall — {root} doesn't exist.")
        return
    existing = detect_existing_install(root)
    if existing is None:
        print(f"warning: {root} exists but has no _meta/installed_version marker.")
        print("Refusing to uninstall — this might not be a Synaptic install.")
        print("If you're sure, delete it manually.")
        sys.exit(2)

    print(f"uninstalling Synaptic v{existing} at {root}")
    if purge:
        print("(--purge: also removing Docker volumes)")
    else:
        print("(soft uninstall: Docker volumes preserved — pass --purge to remove them)")

    # Try to stop the running stack first.
    try:
        compose = root / "docker-compose.yml"
        if compose.exists():
            r = subprocess.run(
                ["docker", "compose", "-f", str(compose), "down"],
                capture_output=True, text=True, cwd=str(root),
            )
            if r.returncode == 0:
                print("stopped running stack")
    except Exception:
        pass

    shutil.rmtree(root)
    print(f"removed {root}")

    if purge:
        # Volumes are prefixed with the compose project name (`name: synaptic`
        # in docker-compose.yml). Compose-managed names — current names
        # (2.1.0b1+) drop the "disorder" stem. Legacy names from earlier
        # beta drops are listed too so --purge cleans up older installs
        # whose data lives under the older naming. Existing users wanting
        # to preserve data should run the migration in
        # docs/dev/Legacy migration paths.md before uninstalling.
        purge_volumes = (
            # Current (2.1.0b1+)
            "synaptic_synaptic-bank",
            "synaptic_ollama-tier1-data",
            "synaptic_ollama-tier2-data",
            "synaptic_airllm-tier2-cache",
            # Legacy renamed-bank (post-rename, pre-volume-rename)
            "synaptic_synaptic-disorder-bank",
            # Legacy original project name
            "synapticdisorder_synaptic-disorder-bank",
        )
        for v in purge_volumes:
            try:
                r = subprocess.run(
                    ["docker", "volume", "rm", v],
                    capture_output=True, text=True,
                )
                if r.returncode == 0:
                    print(f"removed Docker volume: {v}")
                elif "no such volume" in (r.stderr or "").lower():
                    print(f"  (volume {v} didn't exist)")
                else:
                    print(f"warn: couldn't remove {v}: {(r.stderr or '').strip()}")
            except FileNotFoundError:
                print("warn: docker CLI not found — skipping volume cleanup")
                break

    print()
    print("Uninstall complete.")


# ----------------------------------------------------------------------------
# main
# ----------------------------------------------------------------------------

def main() -> None:
    ap = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    grp = ap.add_mutually_exclusive_group()
    grp.add_argument("--update", action="store_true",
                    help="Update an existing install in place; preserves user data.")
    grp.add_argument("--uninstall", action="store_true",
                    help="Remove the install. Pass --purge to also remove Docker volumes.")
    grp.add_argument("--force", action="store_true",
                    help="Clobber existing install (does NOT preserve user data — use --update for that).")
    ap.add_argument("--purge", action="store_true",
                    help="With --uninstall: also remove the bank + Ollama Docker volumes.")
    ap.add_argument("--root", type=Path, default=None,
                    help="Override install root. Default is the per-OS AppData equivalent.")
    args = ap.parse_args()

    src = source_root()
    root = args.root.resolve() if args.root else install_root()

    print(f"source:   {src}")
    print(f"target:   {root}")
    print(f"platform: {platform.system()} ({sys.platform})")
    print()

    # Detect a legacy <2.1.0b1 install at the old SynapticDisorder/synaptic-disorder
    # path. The installer does not auto-migrate (the Docker bank volume rename
    # is a separate, manual step), but warns loudly so the user doesn't end up
    # with two parallel installs by accident.
    if args.root is None:
        legacy = legacy_install_root()
        if legacy.exists() and (legacy / "_meta" / "installed_version").exists():
            legacy_v = detect_existing_install(legacy)
            print("=" * 70)
            print(f"WARNING: legacy install detected at {legacy}")
            print(f"  (version {legacy_v}; product was branded 'Synaptic Disorder')")
            print()
            print(f"This installer targets the new path: {root}")
            print("Proceeding will leave the legacy install in place and run a")
            print("FRESH install at the new path. To migrate properly, follow")
            print("docs/dev/Legacy migration paths.md before continuing.")
            print()
            print("If you intended a fresh install at the new path, re-run with")
            print("the same arguments and answer 'y' below.")
            print("=" * 70)
            try:
                answer = input("Continue with fresh install at new path? [y/N] ").strip().lower()
            except EOFError:
                answer = ""
            if answer not in ("y", "yes"):
                print("aborted — no changes made.")
                sys.exit(0)

    # Sanity check: don't install on top of the source tree.
    try:
        src_resolved = src.resolve()
        root_resolved = root.resolve()
        if src_resolved == root_resolved:
            print("error: source and target are the same path — refusing to clobber.",
                  file=sys.stderr)
            sys.exit(2)
        # Also refuse to install into a parent or child of source — that gets messy.
        if str(root_resolved).startswith(str(src_resolved) + os.sep):
            print("error: target is inside the source tree — refusing.", file=sys.stderr)
            sys.exit(2)
    except Exception:
        pass

    if args.uninstall:
        cmd_uninstall(root, purge=args.purge)
    elif args.update:
        cmd_update(src, root)
    else:
        cmd_install(src, root, force=args.force)


if __name__ == "__main__":
    main()
