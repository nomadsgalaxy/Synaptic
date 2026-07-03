#!/usr/bin/env python3
"""
Generate a synthetic example dataset for Synaptic.

Produces ``assets/memories.example.json`` (and ``assets/synapses.example.json``
if missing) so the dashboard can be shown publicly without leaking the
project author's real Hindsight bank. The release-mode scrubber (D1) uses
this file to swap out ``memories.json`` before packaging a release.

Strategy:
  - Read ``assets/anatomy.json`` for region tag patterns.
  - Read the existing ``assets/memories.json`` if present, ONLY to reuse
    the per-memory voxel position + degree + id structure (so the brain
    keeps its actual shape). All TEXT and TAGS are replaced with
    region-appropriate synthetic content.
  - Reuse ``assets/synapses.json`` unchanged: the paths are voxel indices,
    not text, so they're already non-personal.

If ``assets/memories.json`` doesn't exist (clean fork), fall back to
generating ~150 synthetic memories with random voxel positions sampled
from ``assets/voxels.json`` and tagged by region.

Usage::

    python tools/generate_synthetic_memories.py

Outputs::

    assets/memories.example.json
    assets/synapses.example.json   (only created if missing)
"""
from __future__ import annotations

import hashlib
import json
import random
from datetime import datetime, timedelta, timezone
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
ASSETS = REPO / "assets"

# -----------------------------------------------------------------------------
# Per-region synthetic content. Each entry is a list of (text, extra_tags)
# pairs. The memory's tags will include the region's first tag_pattern + any
# extras + a couple of generic tags. Text is plausible enough that the
# dashboard reads as a real-looking brain, but obviously not personal data.
# -----------------------------------------------------------------------------
REGION_CONTENT: dict[str, list[tuple[str, list[str]]]] = {
    "prefrontal_cortex": [
        ("Decided to use a feature flag for the new payment flow rollout.", ["decision", "rollout"]),
        ("Subagent spawn pattern: planner → researcher → executor with bounded retries.", ["subagent", "agent"]),
        ("Workflow update: PR review now blocks merge until tests pass.", ["workflow", "process"]),
        ("Architectural decision: prefer event sourcing for audit trail traceability.", ["design-decision"]),
        ("Orchestration plan: kick off DB migration, then deploy worker, then app.", ["orchestration"]),
        ("Decided against splitting the monolith this quarter — too much in flight.", ["decision"]),
        ("Plan for next sprint: focus on perf, defer feature work to Q3.", ["planning"]),
        ("AI cascade: classifier first, then generator, then verifier.", ["ai-cascade", "agent"]),
        ("New agent role: 'auditor' — read-only, scans for inconsistencies.", ["agent"]),
        ("Subagent budget should cap at 3 levels deep to avoid runaway costs.", ["subagent"]),
        ("Decision log: rolled back the new caching layer; revisit in two weeks.", ["decision"]),
        ("Workflow improvement: schedule weekly architecture sync.", ["workflow"]),
    ],
    "frontal_lobe": [
        ("User profile: prefers terse, technical explanations over verbose ones.", ["user-profile", "preferences"]),
        ("Project overview: portfolio site rebuild, targeting February 2026.", ["project", "overview"]),
        ("Feedback: code reviews should focus on intent, not style nits.", ["feedback"]),
        ("Personality note: prefers async chat over synchronous meetings.", ["personality", "preferences"]),
        ("Directive: never auto-merge without human review on production branches.", ["directive"]),
        ("Feature roadmap: dark mode, then keyboard shortcuts, then mobile polish.", ["feature", "project"]),
        ("Project purpose: replace the legacy invoice system before Q4.", ["purpose", "project"]),
        ("Personal preference: tabs for code, spaces for prose markdown.", ["preferences"]),
        ("Feedback loop: monthly retro every first Friday.", ["feedback"]),
        ("Project: documentation site rebuild — starts after current sprint.", ["project"]),
        ("Directive: feature work pauses during incident windows.", ["directive"]),
        ("Personal note: morning hours are deep-work time, no meetings before 11.", ["personality"]),
    ],
    "broca_area": [
        ("Drafted release notes for v3.2 — ship Tuesday morning.", ["writing", "output"]),
        ("Response template: lead with the answer, supporting context after.", ["response", "writing"]),
        ("Documentation refresh: API reference now uses OpenAPI 3.1.", ["documentation", "docs"]),
        ("Summary: 12 PRs merged, 4 incidents resolved, 3 features shipped this week.", ["summary"]),
        ("Output format: prefer markdown tables for tabular data.", ["output", "writing"]),
        ("Wrote the onboarding doc — point new hires at it day one.", ["docs", "writing"]),
        ("Response style guide: avoid hedging, use concrete examples.", ["response"]),
        ("Documentation gap: no migration guide for v2 → v3 yet.", ["documentation"]),
        ("Summary email goes out Friday EOD; draft by Thursday.", ["summary", "writing"]),
        ("Output style: short paragraphs, ≤4 sentences each, no bullet stacks.", ["output"]),
        ("Drafted the Q2 wrap-up post; needs one more edit pass.", ["writing"]),
        ("Docs: added a worked example for the bulk-import endpoint.", ["docs"]),
    ],
    "wernicke_area": [
        ("Prompt parsing: split intent from constraints, handle each separately.", ["parsing", "prompt"]),
        ("Comprehension fail: user said 'fast' — meant low-latency, not throughput.", ["comprehension", "intent"]),
        ("Intent classifier needs an 'unsure' bucket — silent fail is worse than ask.", ["intent", "parsing"]),
        ("User intent: 'clean up the dashboard' usually means visual decluttering.", ["intent"]),
        ("Prompt patterns: 'make it work' → focus on correctness over polish.", ["prompt", "intent"]),
        ("Comprehension issue: ambiguous pronouns in long prompts.", ["comprehension"]),
        ("Parsing rule: dates without years default to current year, not next.", ["parsing"]),
        ("Prompt template for bug reports needs reproduction steps required.", ["prompt"]),
        ("Intent inference: 'simpler' usually means fewer dependencies, not less code.", ["intent"]),
        ("Comprehension: user's 'soon' typically means 'within the week'.", ["comprehension"]),
        ("Prompt drift: long sessions accumulate context that confuses parsing.", ["prompt", "parsing"]),
        ("Intent shift detected mid-session: switched from debug to feature work.", ["intent"]),
    ],
    "visual_cortex": [
        ("Dashboard chart needs better contrast — current palette fails WCAG AA.", ["ui", "ux"]),
        ("Screenshot from staging: header alignment is off by 2px on Safari.", ["screenshot", "ui"]),
        ("Browser-extension popup overflows on 1366×768 — fixed with max-width.", ["browser-extension", "css"]),
        ("React component refactor: split the modal into header / body / footer slots.", ["react", "ui"]),
        ("3D viewport: orbit controls feel sluggish under load — debounce camera updates.", ["3d", "visual"]),
        ("HTML structure for the new landing page — semantic, not div soup.", ["html", "web"]),
        ("CSS: replaced flex layout with grid for the pricing table.", ["css", "ui"]),
        ("Image optimization saved 60% on the homepage payload.", ["image", "web"]),
        ("Chrome-MCP screenshot: comparing before/after of the new color theme.", ["chrome-mcp", "screenshot"]),
        ("UX review: empty states need helpful CTAs, not just blank panels.", ["ux"]),
        ("CAD viewer: click-to-explode showed unexpected hidden geometry.", ["cad", "3d"]),
        ("Visual regression test caught a missing icon in the header.", ["visual", "ui"]),
    ],
    "temporal_lobe_left": [
        ("Reference: TLS handshake takes ~1 RTT on modern stacks.", ["reference", "knowledge"]),
        ("Fact: Postgres uses MVCC for transaction isolation by default.", ["memory_type:fact", "knowledge"]),
        ("Procedure: rotate API keys monthly, document old/new in changelog.", ["memory_type:procedure"]),
        ("Rule: never parse JSON with regex.", ["memory_type:rule"]),
        ("Music: classical piano works well for deep coding sessions.", ["music", "audio"]),
        ("Audio: noise-cancelling helps but pink noise helps more.", ["audio"]),
        ("Reference: HTTP/3 uses QUIC, not TCP underneath.", ["reference"]),
        ("Fact: Linux's epoll outperforms select for high-concurrency servers.", ["memory_type:fact"]),
        ("Procedure for postmortems: timeline first, root cause second, actions third.", ["memory_type:procedure"]),
        ("Knowledge: vector DBs trade storage for query latency.", ["knowledge"]),
        ("Rule: don't cache without an invalidation strategy.", ["memory_type:rule"]),
        ("Music recommendation: ambient generative loops for focus.", ["music"]),
    ],
    "temporal_lobe_right": [
        ("Knowledge base: PostgreSQL row-level security policies.", ["knowledge", "reference"]),
        ("Procedure: backup encryption keys to two separate offline locations.", ["memory_type:procedure"]),
        ("Rule: production database changes go through a migration tool, never psql.", ["memory_type:rule"]),
        ("Fact: rust's borrow checker prevents double-free at compile time.", ["memory_type:fact"]),
        ("Reference doc: AWS region/AZ topology and latency matrix.", ["reference"]),
        ("Audio cue: success chime on deploy, sad trombone on rollback.", ["audio"]),
        ("Music: lo-fi works for me; jazz fusion does NOT, oddly.", ["music"]),
        ("Knowledge: BGP convergence times in real-world networks.", ["knowledge"]),
        ("Rule: don't ship features behind feature flags older than 90 days.", ["memory_type:rule"]),
        ("Procedure: incident comms template — what / impact / status / next update.", ["memory_type:procedure"]),
        ("Reference: HTTP status codes the web actually uses (vs the spec).", ["reference"]),
        ("Fact: the human eye can distinguish about 10 million colors.", ["memory_type:fact"]),
    ],
    "hippocampus": [
        ("Consolidated memory: prefer Postgres for OLTP, ClickHouse for analytics.", ["consolidated"]),
        ("Mental model: incident severity is impact × duration, not impact alone.", ["mental-model"]),
        ("Bank refresh: rotated old session memories into the cold-storage tier.", ["bank", "memory"]),
        ("Memory store: stable document IDs reduce orphan facts in long-running banks.", ["memory", "store"]),
        ("Mental model: code review is teaching, not gatekeeping.", ["mental-model"]),
        ("Consolidated: 'simple' is a feature, not a missing feature.", ["consolidated"]),
        ("Memory bank tagged 'feedback' is the highest-signal source for retrospectives.", ["bank", "feedback"]),
        ("Memory cleanup: collapsed 30 single-fact docs into 4 multi-fact ones.", ["memory", "cleanup"]),
        ("Mental model: latency budgets > raw perf numbers for UX decisions.", ["mental-model"]),
        ("Consolidated rule: every save should set a timestamp + a stable id.", ["consolidated"]),
        ("Memory: a project's complexity is bounded by what one person can hold in head.", ["memory"]),
        ("Memory store: tag layout works best with 3–5 tags per memory.", ["memory", "store"]),
    ],
    "parietal_lobe": [
        ("Architecture: split read/write paths so each scales independently.", ["architecture"]),
        ("System design: circuit breakers between services, not just timeouts.", ["system-design"]),
        ("Tech stack pick: TypeScript on the backend was the right call.", ["tech-stack", "tech"]),
        ("Repo layout: monorepo with workspaces beats polyrepo for shared types.", ["repo-layout"]),
        ("Code review: extract the magic constant into a named export.", ["code"]),
        ("Math: fenwick trees are the right shape for rolling-window stats.", ["math"]),
        ("Logic: the auth check belongs at the edge, not deep in handlers.", ["logic"]),
        ("Data model: one source of truth for user state, derived views downstream.", ["data"]),
        ("Config schema: validate on boot; fail loud, fail fast.", ["config-schema", "schema"]),
        ("Tech stack note: prefer boring tech for the critical path.", ["stack"]),
        ("Architecture: pull-based queues scale operations better than push.", ["architecture"]),
        ("System design: rate limit by API key, not just by IP.", ["system-design"]),
    ],
    "motor_cortex": [
        ("Tool call: ran migration in staging; took 4 minutes, no errors.", ["tool", "exec"]),
        ("Bash: created the new deploy script — wraps docker compose + healthcheck.", ["bash", "exec"]),
        ("MCP server adapter accepts both inbound report_event and report_memory.", ["mcp", "tool"]),
        ("Action: tagged release v3.2 and pushed to GitHub.", ["action", "github"]),
        ("Deployment: rolled out to canary first, watched metrics 10 minutes.", ["deployment", "operations"]),
        ("Setup: provisioned the new dev VM with one Ansible playbook.", ["setup"]),
        ("GitHub: cleaned up stale branches; 23 merged feature branches purged.", ["github"]),
        ("Rate limiting: bumped the API budget after measuring real usage.", ["rate-limiting"]),
        ("CLI: wrote a small wrapper around the deploy command for convenience.", ["cli", "tool"]),
        ("Cloudflare config: added a worker for cache-key normalization.", ["cloudflare"]),
        ("Docker: rebuilt the base image to pull in the latest CVE patches.", ["docker"]),
        ("Operations: scheduled the cron job for nightly DB compaction.", ["operations"]),
    ],
    "cerebellum": [
        ("Testing: added a property-based test for the date parser.", ["testing", "test"]),
        ("CI: split the test job into 4 shards, cut runtime from 18 to 6 minutes.", ["ci", "testing"]),
        ("Auth: rotated the JWT signing key and verified zero downtime.", ["auth"]),
        ("Coordination: aligned with platform team on the migration window.", ["coordination"]),
        ("Test plan: covered the three new edge cases identified in code review.", ["test", "testing"]),
        ("CI failure: flaky test; pinned to a fixed timestamp to stabilize.", ["ci"]),
        ("Auth flow: simplified the OAuth callback to a single redirect.", ["auth"]),
        ("Coordination meeting: settled the API contract for cross-team work.", ["coordination"]),
        ("Testing strategy: integration tests over mocks for the payment path.", ["testing"]),
        ("CI/CD: pipeline now caches deps; saved 3 minutes per run.", ["ci"]),
        ("Auth: enforced 2FA on all admin accounts.", ["auth"]),
        ("Coordination: kicked off the cross-team weekly working group.", ["coordination"]),
    ],
    "amygdala": [
        ("Bug: race condition in the token refresh — patched with a mutex.", ["bug", "bugfix"]),
        ("Error: HTTP 500 from the recommendation service, brief outage at 3pm.", ["error"]),
        ("Gotcha: Postgres' default lock timeout is 0 — set it explicitly.", ["gotcha"]),
        ("Incident: misconfigured DNS caused 30-minute partial outage.", ["incident", "error"]),
        ("Warning: don't run vacuum during peak hours; it locks tables.", ["warning"]),
        ("Bugfix: off-by-one in the pagination cursor; added a regression test.", ["bugfix", "bug"]),
        ("Gotcha: timezone naive datetimes silently shift on serialization.", ["gotcha"]),
        ("Incident postmortem: missing alarms hid a slow rollout problem.", ["incident"]),
        ("Warning: the cleanup script doesn't handle symlinks correctly.", ["warning"]),
        ("Error path: handle the network error before the 'happy path' assertion.", ["error"]),
        ("Bug discovered: unbounded retry on transient errors blew up costs.", ["bug"]),
        ("Gotcha: Docker's --restart unless-stopped doesn't survive `compose down`.", ["gotcha"]),
    ],
    "corpus_callosum": [
        ("Slack bridge: forwards #releases to the ops channel automatically.", ["slack", "bridge"]),
        ("Inter-agent contract: events use schema_version + adapter_id at the envelope.", ["inter-agent"]),
        ("Discord-bridge bot now relays threads instead of flattening them.", ["discord-bridge", "bridge"]),
        ("Observability: Phoenix tracer wired into the OpenInference instrumentation.", ["phoenix-tracer", "observability"]),
        ("AI tooling: standardized on the MCP protocol for cross-tool interop.", ["ai-tooling"]),
        ("Coordination: replaced the polling bridge with a webhook subscription.", ["coordination", "bridge"]),
        ("Bridge: deduplicated events by hash to handle dual-publish scenarios.", ["bridge"]),
        ("Mitmproxy was tested for live capture but rejected as too invasive.", ["mitmproxy"]),
        ("Inter-agent: auth flows go through a shared identity provider, not per-tool.", ["inter-agent"]),
        ("Observability: trace IDs propagate across all internal HTTP calls.", ["observability"]),
        ("Slack: status pings now include a link to the live dashboard.", ["slack"]),
        ("Bridge between MCP and OTel: same event lands in both pipelines.", ["bridge"]),
    ],
    "brain_stem": [
        ("Creative riff: what if the dashboard played sound based on activity?", ["creative", "imagination"]),
        ("Generation: drafted three logo concepts for the project — picking one tomorrow.", ["generation", "creative"]),
        ("Imagination: a brain dashboard that visualizes ANY agent's activity.", ["imagination", "art"]),
        ("Dream: a CLI that genuinely understands intent, not just commands.", ["dream"]),
        ("Art project idea: synaesthetic timer — color shifts as time passes.", ["art", "creative"]),
        ("Generative palette: pick three colors, get a balanced 10-color theme.", ["generation"]),
        ("Imagination: anatomical brain skin for any chat agent.", ["imagination"]),
        ("Creative pattern: every release ships with one delight, however small.", ["creative"]),
        ("Dream: a static-site generator that's actually fast and actually pretty.", ["dream"]),
        ("Art: pixel-perfect 3D model of a familiar object, then animate it.", ["art"]),
        ("Generation prompt template: tone + audience + format + 1 surprising element.", ["generation"]),
        ("Creative warm-up: rewrite a paragraph in five different voices.", ["creative"]),
    ],
}


def _id_from(text: str, region: str, idx: int) -> str:
    return "synth-" + hashlib.sha1(f"{region}|{idx}|{text}".encode()).hexdigest()[:12]


def _iso(dt: datetime) -> str:
    return dt.replace(microsecond=0).isoformat() + "Z"


def _compute_synapses(items: list[dict], min_shared_tags: int = 2,
                       max_per_node: int = 8) -> list[dict]:
    """Compute synthetic synapses by tag overlap.

    Two memories form an edge when they share at least `min_shared_tags`
    tags. Each node is capped at `max_per_node` outgoing edges (sorted by
    overlap weight, then by random) to keep the graph from collapsing into
    a hairball. Path arrays are intentionally empty — the runtime renderer
    builds tube curves from memory positions directly (per the 26.5.6.x
    layout pipeline), so voxel paths are unused.
    """
    edges: list[dict] = []
    seen: set[tuple[str, str]] = set()
    by_node: dict[str, list[tuple[int, str]]] = {}
    for i, a in enumerate(items):
        a_tags = set(a.get("tags", []))
        for b in items[i + 1:]:
            b_tags = set(b.get("tags", []))
            shared = a_tags & b_tags
            if len(shared) < min_shared_tags:
                continue
            key = (a["id"], b["id"]) if a["id"] < b["id"] else (b["id"], a["id"])
            if key in seen:
                continue
            seen.add(key)
            by_node.setdefault(a["id"], []).append((len(shared), b["id"]))
            by_node.setdefault(b["id"], []).append((len(shared), a["id"]))
            edges.append({"a": a["id"], "b": b["id"], "w": len(shared), "path": []})
    # Per-node cap — drop excess edges to keep the visualization legible.
    keep: set[tuple[str, str]] = set()
    for node, neigh in by_node.items():
        neigh.sort(key=lambda x: (-x[0], random.random()))
        for _, other in neigh[:max_per_node]:
            key = (node, other) if node < other else (other, node)
            keep.add(key)
    filtered = []
    for e in edges:
        key = (e["a"], e["b"]) if e["a"] < e["b"] else (e["b"], e["a"])
        if key in keep:
            filtered.append(e)
    return filtered


def main() -> None:
    random.seed(42)  # deterministic — same dataset every run
    anatomy_path = ASSETS / "anatomy.json"
    voxels_path = ASSETS / "voxels.json"

    if not anatomy_path.exists() or not voxels_path.exists():
        raise SystemExit("Need both assets/anatomy.json and assets/voxels.json to generate.")

    anatomy = json.loads(anatomy_path.read_text("utf-8"))
    region_keys = list(anatomy["regions"].keys())
    voxels = json.loads(voxels_path.read_text("utf-8"))
    voxel_positions = voxels.get("positions") or []

    # Always-fresh strategy: synthesize ~12 memories per region from the
    # curated pool, with random voxel positions. Never reads real
    # memories.json — even if present — so the example file is guaranteed
    # to carry no statistical or structural fingerprint of the author's
    # actual memory data.
    items: list[dict] = []
    region_counts: dict[str, int] = {}
    for region in region_keys:
        pool = REGION_CONTENT.get(region) or REGION_CONTENT["frontal_lobe"]
        # Shuffle the pool per-region so multi-region runs don't pick the
        # same first-N entries every time.
        shuffled = list(pool)
        random.shuffle(shuffled)
        n_for_region = min(12, len(shuffled))
        for i in range(n_for_region):
            text, extra_tags = shuffled[i]
            tags = sorted(set(extra_tags + [region.split("_")[0], "synthetic"]))
            vi = random.randrange(max(1, len(voxel_positions) // 3))
            now = datetime.now(timezone.utc)
            ts = _iso(now - timedelta(days=random.randint(0, 28), hours=random.randint(0, 23)))
            mid = _id_from(text, region, i)
            items.append({
                "id": mid,
                "text": text,
                "tags": tags,
                "region": region,
                "pos": [voxel_positions[vi*3], voxel_positions[vi*3+1], voxel_positions[vi*3+2]],
                "degree": 0,
                "mentioned_at": ts,
                "created_at": ts,
            })
            region_counts[region] = region_counts.get(region, 0) + 1

    # Compute synapses from tag overlap (≥2 shared tags, capped at 8/node).
    edges = _compute_synapses(items, min_shared_tags=2, max_per_node=8)
    # Backfill degree (count of edges touching each node) so the dashboard
    # can size neurons by connectivity even without a real synapse build.
    degree: dict[str, int] = {}
    for e in edges:
        degree[e["a"]] = degree.get(e["a"], 0) + 1
        degree[e["b"]] = degree.get(e["b"], 0) + 1
    for m in items:
        m["degree"] = degree.get(m["id"], 0)

    out_mem = {
        "items": items,
        "schema_version": "1.0",
        "synthetic": True,
        "comment": "Fully synthetic dataset — no real personal data. Generated by tools/generate_synthetic_memories.py from a curated pool of generic developer/AI memory examples. Used by the Pages static demo and as a reference structure for new installs.",
    }
    (ASSETS / "memories.example.json").write_text(json.dumps(out_mem, indent=2), encoding="utf-8")
    print(f"wrote assets/memories.example.json with {len(items)} synthetic memories")
    for r, n in sorted(region_counts.items(), key=lambda x: -x[1]):
        print(f"  {r:24s} {n}")

    out_syn = {
        "schema_version": "1.0",
        "comment": "Tag-overlap synapses for the synthetic example dataset. Path arrays are empty (the runtime renderer derives tube curves from memory positions directly).",
        "edges": edges,
    }
    (ASSETS / "synapses.example.json").write_text(json.dumps(out_syn, indent=2), encoding="utf-8")
    print(f"wrote assets/synapses.example.json with {len(edges)} synthetic synapses")


if __name__ == "__main__":
    main()
