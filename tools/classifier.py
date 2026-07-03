"""
Synaptic layered region classifier.

Decides which of the 14 anatomical regions each memory belongs to, using
progressively richer evidence:

  Tier 0 — provider override map  (per-provider tag → region overrides)
  Tier 1 — tag pattern matching    (canonical patterns per region)
  Tier 2 — keyword scan on text    (substring match per region)
  Tier 3 — Ollama LLM (optional)   (semantic understanding)

Tiers run in order; the first one that produces a region wins. Tier 3 is
opt-in and only used when Ollama is reachable on localhost:11434. All
classifications are cached by content hash to a JSON file so subsequent
runs are instant.

Public API:
    classify_memories(memories, *, use_llm=True, cache_path=None,
                      ollama_model="llama3.2:3b") -> dict
        Returns a stats dict with per-region counts, per-tier counts,
        and the list of memories with `region` mutated in place.
    classify_one(memory, ...) -> str
        Single-memory classification.

The same classifier should later run in the bridge process for live memory
events so newly-arrived memories land in the right region without
manual tagging.
"""
from __future__ import annotations
import hashlib
import json
import os
import socket
import urllib.request
from collections import Counter, defaultdict
from pathlib import Path
from typing import Iterable

REGION_KEYS = [
    "prefrontal_cortex", "frontal_lobe", "broca_area", "wernicke_area",
    "visual_cortex", "temporal_lobe_left", "temporal_lobe_right",
    "hippocampus", "parietal_lobe", "motor_cortex",
    "cerebellum", "amygdala", "corpus_callosum", "brain_stem",
]

# ---------------------------------------------------------------------------
# Tier 1 — tag patterns (most specific first; first match wins)
# ---------------------------------------------------------------------------
TAG_PATTERNS = [
    ("amygdala", {
        "tags": {"error", "gotcha", "incident", "warning", "bug", "bugfix",
                 "rate-limiting", "broken", "crash"},
    }),
    ("hippocampus", {
        "tags": {"consolidated", "mental-model", "bank", "hindsight",
                 "memory", "memory_type:fact", "memory_type:rule",
                 "memory_type:procedure"},
        "tag_prefix": ["memory_type:"],
        "context_eq": {"user-profile"},
    }),
    ("broca_area", {
        "tags": {"documentation", "docs", "output", "response", "writing",
                 "readme", "summary"},
    }),
    ("wernicke_area", {
        "tags": {"prompt", "intent", "parsing", "comprehension",
                 "prompt-engineering"},
    }),
    ("visual_cortex", {
        "tags": {"ui", "ux", "visual", "screenshot", "chrome-mcp", "image",
                 "obs-websocket", "vdo-ninja", "react", "html", "css",
                 "browser-extension", "browser", "scene", "design",
                 "radiation", "event-tracker"},
    }),
    ("temporal_lobe_left", {
        "tags": {"reference", "knowledge", "music", "audio", "language",
                 "dictionary"},
    }),
    ("motor_cortex", {
        "tags": {"tool", "mcp", "bash", "exec", "execution", "deployment",
                 "operations", "setup", "github", "cloudflare", "docker",
                 "cli", "command", "shell", "powershell", "windows",
                 "prusa-community-dashboard"},
    }),
    ("cerebellum", {
        "tags": {"testing", "ci", "test", "coordination", "auth",
                 "automation", "fine-motor"},
    }),
    ("corpus_callosum", {
        "tags": {"slack", "discord-bridge", "discord", "bridge",
                 "inter-agent", "mitmproxy", "phoenix-tracer",
                 "observability", "ai-tooling", "community-live-obs",
                 "ipc", "mcp-bridge"},
    }),
    ("parietal_lobe", {
        "tags": {"architecture", "tech-stack", "repo-layout", "code",
                 "system-design", "config", "config-schema", "schema",
                 "data", "database", "logic", "math", "gravity",
                 "opencascade", "positron3d"},
    }),
    ("prefrontal_cortex", {
        "tags": {"decision", "design-decision", "planning", "ai-cascade",
                 "agent", "subagent", "workflow", "orchestration",
                 "synaptic-disorder"},
    }),
    ("frontal_lobe", {
        "tags": {"feedback", "directive", "user-profile", "user:anthony",
                 "user:nomad", "preferences", "personality", "feature",
                 "purpose", "overview", "project"},
    }),
    ("brain_stem", {
        "tags": {"creative", "generation", "imagination", "art", "dream",
                 "concept", "vision"},
    }),
]

# ---------------------------------------------------------------------------
# Tier 2 — keyword scan on memory text (lowercased substring match)
# ---------------------------------------------------------------------------
KEYWORD_PATTERNS = [
    ("amygdala",         ["error ", "exception", "broke ", "broken",
                           "crashed", " bug ", "warning", "incident",
                           "rate limit"]),
    ("hippocampus",      ["hindsight", "consolidat", "mental model",
                           "recall", " memory ", " remember "]),
    ("broca_area",       ["readme", "documentation", " docs ", "wrote a",
                           "summary of"]),
    ("wernicke_area",    [" prompt ", "user input", "comprehen", "parse"]),
    ("visual_cortex",    [" ui ", " ux ", "visual", "screenshot",
                           "browser", "obs ", "vdo", "react component",
                           " css ", " html ", "design"]),
    ("temporal_lobe_left",["fact:", "reference", "knowledge", " music ",
                            " audio "]),
    ("motor_cortex",     ["bash ", "deploy", " run ", "execut", "github",
                           "cloudflare", "docker", " cli ", "command line",
                           "powershell"]),
    ("cerebellum",       [" test ", " tests ", " ci ", " auth ",
                           "automation", "playwright"]),
    ("corpus_callosum",  [" slack ", " discord ", "bridge", "tracing",
                           " mcp ", "mitmproxy", "phoenix", "telemetry"]),
    ("parietal_lobe",    ["architectur", "schema", "tech stack",
                           "system design", " repo ", "config"]),
    ("prefrontal_cortex",["plan ", "planning", "decision", "agent",
                           "orchestrat", "subagent"]),
    ("frontal_lobe",     ["preference", "personality", "feedback",
                           "feature", "directive"]),
    ("brain_stem",       ["creative", "imagin", "dream", " art ",
                           "generat"]),
]

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
def _normalize_tags(memory: dict) -> set:
    raw = memory.get("tags") or []
    if isinstance(raw, str):
        raw = [t.strip() for t in raw.split(",") if t.strip()]
    return set(t.lower() for t in raw)

def _content_hash(memory: dict) -> str:
    text = (memory.get("text") or "")[:2000]
    tags = ",".join(sorted(_normalize_tags(memory)))
    ctx = memory.get("context") or ""
    h = hashlib.sha1((text + "|" + tags + "|" + ctx).encode("utf-8"))
    return h.hexdigest()[:16]

def _tier1_tags(memory: dict) -> str | None:
    tags = _normalize_tags(memory)
    ctx = (memory.get("context") or "").lower()
    for region, rule in TAG_PATTERNS:
        rule_tags = rule.get("tags", set())
        if tags & rule_tags:
            return region
        for prefix in rule.get("tag_prefix", []):
            if any(t.startswith(prefix) for t in tags):
                return region
        if ctx in rule.get("context_eq", set()):
            return region
    return None

def _tier2_keywords(memory: dict) -> str | None:
    text = (memory.get("text") or "").lower()
    if not text:
        return None
    for region, words in KEYWORD_PATTERNS:
        for w in words:
            if w in text:
                return region
    return None

def _ollama_alive(host: str = "127.0.0.1", port: int = 11434, timeout: float = 0.4) -> bool:
    try:
        with socket.create_connection((host, port), timeout=timeout):
            return True
    except (OSError, ConnectionRefusedError):
        return False

OLLAMA_SYSTEM_PROMPT = """You classify a memory into ONE of 14 brain regions based on what cognitive function the memory is about. Output ONLY the region key from the list below — no explanation, no punctuation, no markdown.

Regions:
  prefrontal_cortex   - agent orchestration, planning, architectural choices
  frontal_lobe        - identity, personality, work-style preferences, decisions
  broca_area          - response generation, documentation, READMEs, written output
  wernicke_area       - prompt parsing, user input comprehension
  visual_cortex       - UI/UX, web, screenshots, browser extensions, visual design
  temporal_lobe_left  - long-term factual knowledge, references, language, music
  temporal_lobe_right - long-term factual knowledge (right hemisphere variant)
  hippocampus         - memory systems themselves (Hindsight, mental models)
  parietal_lobe       - code architecture, system design, schemas, data structures
  motor_cortex        - tool execution, MCP calls, bash, deployment, ops
  cerebellum          - testing, CI, coordination, auth flows
  amygdala            - errors, bugs, gotchas, warnings, incidents
  corpus_callosum     - inter-system bridges (Slack, MCP, telemetry, observability)
  brain_stem          - creative generation, imagination, art, dreams

If multiple regions could fit, pick the most specific. Output only the region key."""

def _tier3_ollama(memory: dict, model: str) -> str | None:
    text = (memory.get("text") or "")[:1200]
    tags = ", ".join(sorted(_normalize_tags(memory))) or "(none)"
    ctx = memory.get("context") or ""
    user_prompt = f"Memory text: {text}\nTags: {tags}\nContext: {ctx}\n\nRegion key:"
    payload = {
        "model": model,
        "messages": [
            {"role": "system", "content": OLLAMA_SYSTEM_PROMPT},
            {"role": "user", "content": user_prompt},
        ],
        "stream": False,
        "options": {"temperature": 0.0, "num_predict": 12},
    }
    body = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(
        "http://127.0.0.1:11434/api/chat",
        data=body, headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            data = json.loads(resp.read().decode("utf-8"))
    except Exception:
        return None
    raw = (data.get("message", {}).get("content") or "").strip().lower()
    # The model can sometimes wrap the answer in quotes / backticks / etc
    raw = raw.strip("`'\"\n .,:;").splitlines()[0].strip() if raw else ""
    # Match against known region keys
    if raw in REGION_KEYS:
        return raw
    for k in REGION_KEYS:
        if k in raw:
            return k
    return None

# ---------------------------------------------------------------------------
# Public API
# ---------------------------------------------------------------------------
def classify_one(memory: dict, *, use_llm: bool = True, ollama_alive: bool | None = None,
                 ollama_model: str = "llama3.2:3b", cache: dict | None = None) -> tuple[str, str]:
    """Classify one memory. Returns (region_key, tier_used)."""
    h = _content_hash(memory)
    if cache is not None and h in cache:
        cached = cache[h]
        return cached["region"], cached["tier"]

    region = _tier1_tags(memory)
    tier = "tier1_tags" if region else None

    if region is None:
        region = _tier2_keywords(memory)
        if region:
            tier = "tier2_keywords"

    if region is None and use_llm:
        if ollama_alive is None:
            ollama_alive = _ollama_alive()
        if ollama_alive:
            region = _tier3_ollama(memory, ollama_model)
            if region:
                tier = "tier3_ollama"

    if region is None:
        region = "frontal_lobe"   # safe default — much better than brain stem
        tier = "fallback"

    if cache is not None:
        cache[h] = {"region": region, "tier": tier}
    return region, tier

def classify_memories(memories: Iterable[dict], *, use_llm: bool = True,
                      cache_path: str | Path | None = None,
                      ollama_model: str = "llama3.2:3b") -> dict:
    """Classify a batch of memories. Mutates each memory's `region` in place.

    Returns a stats dict: { "by_region": {...}, "by_tier": {...},
                            "ollama_used": bool, "n": int }.
    """
    cache = {}
    cache_path = Path(cache_path) if cache_path else None
    if cache_path and cache_path.exists():
        try:
            cache = json.loads(cache_path.read_text(encoding="utf-8"))
        except Exception:
            cache = {}

    ollama_alive_now = _ollama_alive() if use_llm else False
    by_region = Counter()
    by_tier = Counter()
    for m in memories:
        region, tier = classify_one(
            m, use_llm=use_llm, ollama_alive=ollama_alive_now,
            ollama_model=ollama_model, cache=cache,
        )
        m["region"] = region
        m["region_tier"] = tier
        by_region[region] += 1
        by_tier[tier] += 1

    if cache_path:
        cache_path.parent.mkdir(parents=True, exist_ok=True)
        cache_path.write_text(json.dumps(cache, indent=2), encoding="utf-8")

    return {
        "by_region": dict(by_region),
        "by_tier": dict(by_tier),
        "ollama_used": ollama_alive_now,
        "n": sum(by_region.values()),
        "regions": REGION_KEYS,
    }

if __name__ == "__main__":
    # Smoke-test
    import sys
    test_memories = [
        {"text": "We decided to use SSE over WebSocket because the connection lifecycle was too fragile",
         "tags": [], "context": ""},
        {"text": "Hindsight bank now has 1300 memories", "tags": ["hindsight"], "context": ""},
        {"text": "Bash one-liner to deploy", "tags": ["bash", "deployment"], "context": ""},
        {"text": "UI tweak for the dashboard", "tags": ["ui"], "context": ""},
        {"text": "Got a 429 rate-limit error during the test run", "tags": [], "context": ""},
    ]
    for m in test_memories:
        region, tier = classify_one(m, use_llm=False)
        print(f"  [{tier:16s}] {region:22s}  {m['text'][:60]}")
