"""Reprocess region-less memories in the active Synaptic bank.

Pulls every memory from the running SD Core, filters to those with an
empty `region_hint`, and assigns a region via three cascading passes:

  Tier 1  tag rules        (no I/O)
  Tier 2  keyword keywords (no I/O)
  Tier 3  Ollama LLM       (Tier-1 model via SD_OLLAMA_TIER1_URL)

Then PATCHes /bank/memories/{id} with the inferred region_hint.

Rules ported 1-to-1 from `bridge/core/classifier.go` (tagPatterns,
kwPatterns, classifySystemPrompt) so the offline reprocess produces
the same regions the online classifier would.
"""

from __future__ import annotations

import json
import os
import re
import sys
import time
import urllib.request
import urllib.error
from typing import Iterable

CORE_URL = os.environ.get("SD_CORE_URL", "http://localhost:9911")
OLLAMA_URL = os.environ.get("SD_OLLAMA_TIER1_URL", "http://localhost:11434")
OLLAMA_MODEL = os.environ.get("SD_TIER1_MODEL", "llama3.2:3b")
TOKEN = os.environ["SD_API_TOKEN"]
HEADERS = {"Authorization": f"Bearer {TOKEN}", "Content-Type": "application/json"}

REGION_KEYS = {
    "prefrontal_cortex", "frontal_lobe", "broca_area", "wernicke_area",
    "visual_cortex", "temporal_lobe_left", "temporal_lobe_right",
    "hippocampus", "parietal_lobe", "motor_cortex",
    "cerebellum", "amygdala", "corpus_callosum", "brain_stem",
}

TAG_RULES: list[tuple[str, set[str]]] = [
    ("amygdala", {"error", "gotcha", "incident", "warning", "bug", "bugfix", "rate-limiting", "broken", "crash"}),
    ("hippocampus", {"consolidated", "mental-model", "bank", "retrospective", "memory", "memory_type:fact", "memory_type:rule", "memory_type:procedure"}),
    ("broca_area", {"documentation", "docs", "output", "response", "writing", "readme", "summary"}),
    ("wernicke_area", {"prompt", "intent", "parsing", "comprehension", "prompt-engineering"}),
    ("visual_cortex", {"ui", "ux", "visual", "screenshot", "chrome-mcp", "image", "obs-websocket",
                       "vdo-ninja", "react", "html", "css", "browser-extension", "browser",
                       "scene", "design", "radiation", "event-tracker"}),
    ("temporal_lobe_left", {"reference", "knowledge", "music", "audio", "language", "dictionary"}),
    ("motor_cortex", {"tool", "mcp", "bash", "exec", "execution", "deployment", "operations",
                      "setup", "github", "cloudflare", "docker", "cli", "command", "shell",
                      "powershell", "windows", "prusa-community-dashboard"}),
    ("cerebellum", {"testing", "ci", "test", "coordination", "auth", "automation", "fine-motor"}),
    ("corpus_callosum", {"slack", "discord-bridge", "discord", "bridge", "inter-agent", "mitmproxy",
                         "phoenix-tracer", "observability", "ai-tooling", "community-live-obs", "ipc", "mcp-bridge"}),
    ("parietal_lobe", {"architecture", "tech-stack", "repo-layout", "code", "system-design",
                       "config", "config-schema", "schema", "data", "database", "logic",
                       "math", "gravity", "opencascade", "positron3d"}),
    ("prefrontal_cortex", {"decision", "design-decision", "planning", "ai-cascade", "agent",
                           "subagent", "workflow", "orchestration", "synaptic", "synaptic-disorder"}),
    ("frontal_lobe", {"feedback", "directive", "user-profile", "user:anthony", "user:nomad",
                      "preferences", "personality", "feature", "purpose", "overview", "project"}),
    ("brain_stem", {"creative", "generation", "imagination", "art", "dream", "concept", "vision"}),
]

# Tag prefixes that pre-map straight to a region (cheap structural rule).
TAG_PREFIX = [
    ("hippocampus", "memory_type:"),
]

KW_RULES: list[tuple[str, list[str]]] = [
    ("amygdala", ["error ", "exception", "broke ", "broken", "crashed", " bug ", "warning", "incident", "rate limit"]),
    ("hippocampus", ["retrospective", "consolidat", "mental model", "recall", " memory ", " remember "]),
    ("broca_area", ["readme", "documentation", " docs ", "wrote a", "summary of"]),
    ("wernicke_area", [" prompt ", "user input", "comprehen", "parse"]),
    ("visual_cortex", [" ui ", " ux ", "visual", "screenshot", "browser", "obs ", "vdo", "react component", " css ", " html ", "design"]),
    ("temporal_lobe_left", ["fact:", "reference", "knowledge", " music ", " audio "]),
    ("motor_cortex", ["bash ", "deploy", " run ", "execut", "github", "cloudflare", "docker", " cli ", "command line", "powershell"]),
    ("cerebellum", [" test ", " tests ", " ci ", " auth ", "automation", "playwright"]),
    ("corpus_callosum", [" slack ", " discord ", "bridge", "tracing", " mcp ", "mitmproxy", "phoenix", "telemetry"]),
    ("parietal_lobe", ["architectur", "schema", "tech stack", "system design", " repo ", "config"]),
    ("prefrontal_cortex", ["plan ", "planning", "decision", "agent", "orchestrat", "subagent"]),
    ("frontal_lobe", ["preference", "personality", "feedback", "feature", "directive"]),
    ("brain_stem", ["creative", "imagin", "dream", " art ", "generat"]),
]

CLASSIFY_PROMPT = """You classify a memory into ONE of 14 brain regions based on what cognitive function the memory is about. Output ONLY the region key from the list below — no explanation, no punctuation, no markdown.

Regions:
  prefrontal_cortex   - agent orchestration, planning, architectural choices
  frontal_lobe        - identity, personality, work-style preferences, decisions
  broca_area          - response generation, documentation, READMEs, written output
  wernicke_area       - prompt parsing, user input comprehension
  visual_cortex       - UI/UX, web, screenshots, browser extensions, visual design
  temporal_lobe_left  - long-term factual knowledge, references, language, music
  temporal_lobe_right - long-term factual knowledge (right hemisphere variant)
  hippocampus         - memory systems themselves (consolidation, mental models)
  parietal_lobe       - code architecture, system design, schemas, data structures
  motor_cortex        - tool execution, MCP calls, bash, deployment, ops
  cerebellum          - testing, CI, coordination, auth flows
  amygdala            - errors, bugs, gotchas, warnings, incidents
  corpus_callosum     - inter-system bridges (Slack, MCP, telemetry, observability)
  brain_stem          - creative generation, imagination, art, dreams

If multiple regions could fit, pick the most specific. Output only the region key."""


def http_json(method: str, url: str, body: dict | None = None, timeout: float = 60.0) -> dict:
    data = None
    if body is not None:
        data = json.dumps(body).encode("utf-8")
    req = urllib.request.Request(url, data=data, headers=HEADERS, method=method)
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        raw = resp.read()
        if not raw:
            return {}
        return json.loads(raw)


def pull_all_memories() -> list[dict]:
    # The current `/bank/memories` endpoint honours `limit` but silently
    # ignores `offset` — paginating against it loops forever. Pull
    # everything in one shot with a generous limit.
    url = f"{CORE_URL}/bank/memories?limit=20000"
    data = http_json("GET", url, timeout=120)
    return data.get("memories", []) if isinstance(data, dict) else []


def classify_by_tags(tags: list[str]) -> str:
    tagset = {t.lower() for t in tags}
    # Prefix rules first.
    for region, prefix in TAG_PREFIX:
        if any(t.startswith(prefix) for t in tagset):
            return region
    # Exact-tag rules in declared order — first hit wins to mirror Go.
    for region, ruletags in TAG_RULES:
        if tagset & ruletags:
            return region
    return ""


def classify_by_keywords(text: str) -> str:
    lower = (" " + text.lower() + " ")
    for region, kws in KW_RULES:
        for kw in kws:
            if kw in lower:
                return region
    return ""


REGION_RE = re.compile(r"\b(" + "|".join(re.escape(k) for k in REGION_KEYS) + r")\b", re.I)


def classify_by_llm(text: str) -> str:
    body = {
        "model": OLLAMA_MODEL,
        "stream": False,
        "options": {"temperature": 0.0, "num_predict": 24},
        "messages": [
            {"role": "system", "content": "[think:off]\n\n" + CLASSIFY_PROMPT},
            {"role": "user", "content": text[:1500]},
        ],
    }
    try:
        req = urllib.request.Request(
            f"{OLLAMA_URL}/api/chat",
            data=json.dumps(body).encode("utf-8"),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(req, timeout=60) as resp:
            data = json.loads(resp.read())
    except Exception as exc:
        print(f"  LLM error: {exc}", file=sys.stderr)
        return ""
    content = (data.get("message") or {}).get("content", "")
    m = REGION_RE.search(content)
    if m:
        key = m.group(1).lower()
        if key in REGION_KEYS:
            return key
    return ""


def patch_region(mem_id: str, region: str, source: str) -> bool:
    url = f"{CORE_URL}/bank/memories/{mem_id}"
    try:
        http_json("PATCH", url, {"region_hint": region}, timeout=30)
        return True
    except urllib.error.HTTPError as exc:
        print(f"  PATCH {mem_id} failed: {exc.code} {exc.read()[:200]}", file=sys.stderr)
        return False
    except Exception as exc:
        print(f"  PATCH {mem_id} error: {exc}", file=sys.stderr)
        return False


def main() -> int:
    print(f"core={CORE_URL}  ollama={OLLAMA_URL}  model={OLLAMA_MODEL}")
    print("pulling memories…")
    memories = pull_all_memories()
    print(f"total returned: {len(memories)}")

    region_less = [m for m in memories if not (m.get("region_hint") or "").strip()]
    print(f"region-less:    {len(region_less)}")
    if not region_less:
        print("nothing to do")
        return 0

    counts = {"tag": 0, "keyword": 0, "llm": 0, "unmapped": 0, "errors": 0}
    t0 = time.time()
    for i, mem in enumerate(region_less, 1):
        mem_id = mem["id"]
        tags = mem.get("tags") or []
        text = mem.get("text") or ""

        region = classify_by_tags(tags)
        source = "tag"
        if not region:
            region = classify_by_keywords(text)
            source = "keyword"
        if not region:
            region = classify_by_llm(text)
            source = "llm"
        if not region:
            counts["unmapped"] += 1
            print(f"[{i:>4}/{len(region_less)}] {mem_id}  UNMAPPED  tags={tags[:3]}")
            continue

        ok = patch_region(mem_id, region, source)
        if not ok:
            counts["errors"] += 1
            continue
        counts[source] += 1
        if i % 50 == 0 or i == len(region_less):
            elapsed = time.time() - t0
            rate = i / max(elapsed, 0.001)
            eta = (len(region_less) - i) / max(rate, 0.001)
            print(f"[{i:>4}/{len(region_less)}] tag={counts['tag']} kw={counts['keyword']} "
                  f"llm={counts['llm']} unmapped={counts['unmapped']} err={counts['errors']} "
                  f"  {rate:.1f}/s  eta={eta/60:.1f}m")

    elapsed = time.time() - t0
    print()
    print(f"done in {elapsed/60:.1f} min")
    for k, v in counts.items():
        print(f"  {k:<10} {v}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
