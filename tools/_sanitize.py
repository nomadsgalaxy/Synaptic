"""
Build-time sanitization shared by build_release.py and build_pages.py.

Removes *personal-but-not-attributional* references from a copied tree
before the build artifact ships publicly. The Dev/ source keeps Anthony's
configuration (NOMAD-01 subject default, his actual filesystem paths in
examples, his machine names) for his own daily use; the build scripts run
this sanitization on the way to Prod/ and Pages/ so the public artifact
doesn't ship his private setup as the public default.

What we DO NOT sanitize — author attribution stays intact everywhere:

  - "Synaptic by NomadsGalaxy"            — HUD credit byline (rebrand from
                                            "Synaptic Disorder by NomadsGalaxy").
  - "(Anthony, the author)" / "I (Anthony, the author)"  — first-person
    author voice in docs/data_privacy.md and similar.
  - "Anthony's Hindsight Control Plane"   — references to Anthony's other
    projects (Hindsight). Those are project attribution, not personal data.
  - `docs/CREDITS.md`                     — author attribution lives here on purpose.
  - `bridge/*/package.json`               — author/maintainer metadata is standard OSS.
  - `bridge/sdk/python/pyproject.toml`    — same.
  - `.claude-plugin/marketplace.json`     — plugin owner is the publisher's identity.
  - `bridge/claude-code-plugin/.claude-plugin/plugin.json` — same.
  - GitHub URL references (`nomadsgalaxy/Synaptic-Disorder`) — that's the canonical
    project URL where the plugin marketplace looks. Must stay for installs to work.

What we DO sanitize — these are personal-config / setup details, not
attribution:

  - `NOMAD-01`                            — the dev-time default subject identifier
  - `nomadsgalaxy.com` in tunnel example URLs — to avoid users following the
                                            tutorial and pointing at the author's
                                            real domain by accident
  - `C:\\Users\\Anthony\\…` filesystem paths in examples — Anthony's local layout
  - `Nomad Desktop` / `RND-Laptop` machine-name examples
  - `user:anthony` / `user:nomad` lowercase tag-form examples
  - The `Anthony_Hindsight_Memory_Guide.md` filename in exclusion lists
    (the file itself is excluded from public builds; the bare filename ref
    becomes meaningless without it)
"""
from __future__ import annotations

import re
from pathlib import Path
from typing import Sequence

# Regex substitutions applied to every file in SANITIZE_FILES below.
# Order matters — more specific patterns first.
SUBSTITUTIONS: list[tuple[re.Pattern, str]] = [
    # Default subject identifier (HUD shows SUBJECT: <name>; user can rename)
    (re.compile(r"NOMAD-01"), "SUBJECT-01"),
    # Personal domain used in tunnel/remote tutorial examples. The author's
    # actual domain is left as-is in HUD bylines and other attribution
    # surfaces — it's only swapped where a user might literally copy the
    # value into their own deployment by mistake.
    (re.compile(r"\.nomadsgalaxy\.com\b"), ".example.com"),
    (re.compile(r"\bnomadsgalaxy\.com\b"), "example.com"),
    # Anthony's literal local Windows path → generic placeholder.
    # Cover both raw single-backslash and JSON-escaped double-backslash forms.
    (re.compile(r"C:\\\\Users\\\\Anthony\\\\AI\\\\Synaptic Disorder\\\\Dev\\\\"),
     "/path/to/synaptic-disorder/"),
    (re.compile(r"C:\\\\Users\\\\Anthony\\\\AI\\\\Synaptic Disorder\\\\"),
     "/path/to/synaptic-disorder/"),
    (re.compile(r"C:\\Users\\Anthony\\AI\\Synaptic Disorder\\Dev\\"),
     "/path/to/synaptic-disorder/"),
    (re.compile(r"C:\\Users\\Anthony\\AI\\Synaptic Disorder\\"),
     "/path/to/synaptic-disorder/"),
    (re.compile(r"C:\\Users\\Anthony\\"),
     "/path/to/"),
    # Forward-slash form (used in Python code with raw paths)
    (re.compile(r"C:/Users/Anthony/AI/"),
     "/path/to/"),
    (re.compile(r"C:/Users/Anthony/"),
     "/path/to/"),
    # Anthony's machine-name examples used in remote-operation docs
    (re.compile(r"Nomad Desktop"), "your dashboard host"),
    (re.compile(r"RND-Laptop"),     "your client machine"),
    # Lowercase tag-form examples in MEMORY_AUTHORING_GUIDE.md. These are
    # generic "this is what an identity tag looks like" examples, not
    # author attribution. Replace with a placeholder to keep the docs
    # generic for any reader.
    (re.compile(r"\buser:anthony\b"), "user:alex"),
    (re.compile(r"\buser:nomad\b"), "user:alex"),
    # The "Anthony prefers …" example in MEMORY_AUTHORING_GUIDE.md is a
    # demonstration of HOW to write a memory tag about a user's preference.
    # Generic-name it so the example reads as instructional rather than as
    # personal data.
    (re.compile(r"text: \"Anthony prefers"),
     'text: "the user prefers'),
    # The Hindsight memory guide filename appears in exclusion lists. The
    # file itself is excluded from the public build, so referring to it by
    # name is harmless but personal-coded; generalize.
    (re.compile(r"`Anthony_Hindsight_Memory_Guide\.md`"),
     "`docs/dev/` (the dev-only Hindsight guide)"),
]

# Files (relative to the dst root) to apply SUBSTITUTIONS to. Limiting the
# allowlist prevents accidentally rewriting CREDITS / package.json / etc.
# where the personal name is intentional metadata.
SANITIZE_FILES: tuple[str, ...] = (
    "index.html",
    "README.md",
    "Beta-Upgrade.md",
    "Agent-Install.md",
    "RELEASE_BUILD.md",
    "docs/INTEGRATIONS.md",
    "docs/REMOTE.md",
    "docs/event-schema.md",
    "docs/data_privacy.md",
    "docs/MEMORY_AUTHORING_GUIDE.md",
    "docs/site/index.html",
    "bridge/claude-code-plugin/README.md",
    "bridge/mcp-adapter/README.md",
    "bridge/otel-adapter/README.md",
    "bridge/sdk/README.md",
    "bridge/sdk/python/README.md",
    "bridge/sdk/typescript/README.md",
    "tools/memory_providers/README.md",
    "tools/memory_providers/hindsight_provider.py",
    # build_release.py carries a hardcoded MIRROR_PATH constant pointing at
    # the author's OneDrive→GitHub working copy. The substitutions strip
    # `C:\Users\Anthony\` etc. so the public copy gets a generic placeholder
    # path that fails the `.exists()` check and quietly skips the mirror.
    "tools/build_release.py",
    # The plugin-install agent guide hardcodes Anthony's local + AppData
    # paths in the install snippets; sanitize them to generic placeholders.
    "docs/AGENT_INSTALL_PLUGIN.md",
    # Go test fixtures that bake in the author's local path layout or
    # tunnel hostname. Not secrets, but they leak personal config when
    # the source tree ships publicly.
    "bridge/core/compose_runner_test.go",
    "bridge/core/handlers_admin_network.go",
    "bridge/core/handlers_admin_network_test.go",
    # The gemini-cli-hooks forwarder was committed with a hardcoded
    # SD_API_TOKEN; the sanitizer wipes it back to an env-var read.
    "bridge/gemini-cli-hooks/forward.js",
)

# Extra substitutions only applied to files where we expect a real secret
# (rather than mere personal config) to be present. Keeps the generic
# allow-list tight while still scrubbing committed tokens.
SECRET_SUBS: list[tuple[re.Pattern, str]] = [
    # Hardcoded SD_API_TOKEN in JS adapters: replace the literal string
    # with a process.env read so the file still parses but ships without
    # the live token.
    (re.compile(r"const\s+SD_API_TOKEN\s*=\s*'[^']*'\s*;"),
     "const SD_API_TOKEN = process.env.SD_API_TOKEN || '';"),
]
SECRET_SUB_FILES: tuple[str, ...] = (
    "bridge/gemini-cli-hooks/forward.js",
)

# Block to strip from Prod README — the "Privacy note" warning about real
# memories. After build_release.py swaps memories.json for the synthetic
# .example.json, the note no longer applies. Stripping the note keeps the
# Prod README accurate for someone landing on the public repo.
PRIVACY_NOTE_RE = re.compile(
    r"\n>\s*\*\*Privacy note:\*\*[^\n]+(?:\n>[^\n]*)*\n",
    re.MULTILINE,
)

# Block to strip from any .md — the BETA-ONLY callout. We leave this in for
# Pages/ (where it's still beta-relevant) but strip from Prod when
# `--strip-beta-callout` is passed.
BETA_CALLOUT_RE = re.compile(
    r"<!-- BETA-ONLY · REMOVE BEFORE v1\.0 RELEASE -->.*?<!-- END BETA-ONLY -->\s*",
    re.DOTALL,
)


def sanitize_tree(dst: Path, *, strip_privacy_note: bool = True,
                  strip_beta_callout: bool = False) -> dict[str, int]:
    """Apply SUBSTITUTIONS to each file in SANITIZE_FILES under dst.

    Returns a {filename: number_of_changes} report so callers can log a summary.
    Files not present are silently skipped (the Pages build doesn't ship every
    file the Prod build does).
    """
    report: dict[str, int] = {}
    for relpath in SANITIZE_FILES:
        src = dst / relpath
        if not src.is_file():
            continue
        original = src.read_text("utf-8")
        new = original
        n_subs = 0
        for pat, repl in SUBSTITUTIONS:
            new, count = pat.subn(repl, new)
            n_subs += count
        # Optional content-block strips (only for README.md)
        if relpath == "README.md":
            if strip_privacy_note:
                new, count = PRIVACY_NOTE_RE.subn("\n", new)
                n_subs += count
            if strip_beta_callout:
                new, count = BETA_CALLOUT_RE.subn("", new)
                n_subs += count
        if relpath in SECRET_SUB_FILES:
            for pat, repl in SECRET_SUBS:
                new, count = pat.subn(repl, new)
                n_subs += count
        if new != original:
            src.write_text(new, encoding="utf-8")
            report[relpath] = n_subs
    return report


def format_report(report: dict[str, int]) -> str:
    if not report:
        return "  (no personal references to scrub — already clean)"
    total = sum(report.values())
    lines = [f"  scrubbed {total} occurrence(s) across {len(report)} file(s):"]
    for k in sorted(report):
        lines.append(f"    - {k} ({report[k]})")
    return "\n".join(lines)
