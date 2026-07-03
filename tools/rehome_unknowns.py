"""Reclassify memories sitting on region_hint='unknown' into the expanded
50-region canon.

These memories rendered as "Intrusive Thoughts" — the orbital cloud
around the brain. Some are legitimately ambiguous; many are just leftovers
from before the v3.0 region work. We re-run them through:

  Tier 1 — tag rules (no I/O)         — original 14-region map
  Tier 2 — keyword rules (no I/O)     — original 14-region map
  Tier 3 — Tier-1 LLM classification — full expanded 50-region prompt

Tier-3 picks up specialized regions (insula, posterior_cingulate,
basal_ganglia, etc.) added in the 2026-05-17 overhaul; the prior
reprocess script only knew the 14 base regions, so this script is
needed to push unknowns into the new homes the science now supports.
"""

from __future__ import annotations

import json
import os
import re
import sys
import time
import urllib.request
import urllib.error

# Reuse helpers from reprocess_regions.py
sys.path.insert(0, os.path.dirname(__file__))
from reprocess_regions import (  # noqa: E402
    CORE_URL, OLLAMA_URL, OLLAMA_MODEL, HEADERS,
    REGION_KEYS, classify_by_tags, classify_by_keywords, patch_region,
)
BASE_REGION_KEYS = REGION_KEYS

# Expanded 50-region prompt — same shape as bridge/core/classifier.go
# classifySystemPrompt but listing every canonical region.
EXPANDED_REGIONS = sorted(BASE_REGION_KEYS | {
    "insula", "thalamus", "basal_ganglia",
    "posterior_cingulate", "precuneus",
    "orbitofrontal_cortex", "ventromedial_prefrontal_cortex",
    "fusiform_gyrus", "angular_gyrus",
    "salience_network", "dorsal_attention_network",
    "frontoparietal_control_network",
})

EXPANDED_PROMPT = """You classify a memory into ONE brain region based on what cognitive function the memory is about. Output ONLY the region key from the list below — no explanation, no punctuation, no markdown.

Base regions:
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

Specialized regions (use when clearly applicable):
  insula                          - gut-level signals, "something feels off" intuitions
  thalamus                        - attention routing, what got through the filter
  basal_ganglia                   - habits, procedural memories, standing directives
  posterior_cingulate             - autobiographical recall, looking back at own work
  precuneus                       - mental-image / scenario reconstruction
  orbitofrontal_cortex            - value judgments, regret, "that was a bad idea"
  ventromedial_prefrontal_cortex  - emotionally-weighted choices, team-trust calls
  fusiform_gyrus                  - pattern recognition of specific recurring things
  angular_gyrus                   - cross-domain integration (TPJ, theory of mind)
  salience_network                - interrupt-worthy signals, triage decisions
  dorsal_attention_network        - deliberate focus, goal-directed search
  frontoparietal_control_network  - multi-step workflow context, sub-task switching

If multiple regions could fit, prefer a base region unless a specialized region is clearly the better match. Output only the region key."""

REGION_RE = re.compile(r"\b(" + "|".join(re.escape(k) for k in EXPANDED_REGIONS) + r")\b", re.I)


def classify_by_llm_expanded(text: str) -> str:
    body = {
        "model": OLLAMA_MODEL,
        "stream": False,
        "options": {"temperature": 0.0, "num_predict": 24},
        "messages": [
            {"role": "system", "content": "[think:off]\n\n" + EXPANDED_PROMPT},
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
        if key in EXPANDED_REGIONS:
            return key
    return ""


def main() -> int:
    print(f"core={CORE_URL}  ollama={OLLAMA_URL}  model={OLLAMA_MODEL}")
    print(f"expanded canon: {len(EXPANDED_REGIONS)} regions")
    url = f"{CORE_URL}/bank/memories?limit=20000"
    req = urllib.request.Request(url, headers=HEADERS, method="GET")
    with urllib.request.urlopen(req, timeout=120) as resp:
        data = json.loads(resp.read())
    items = data.get("memories", [])
    unknowns = [m for m in items if m.get("region_hint") == "unknown"]
    print(f"unknown (intrusive): {len(unknowns)}")
    if not unknowns:
        print("nothing to do")
        return 0

    counts = {"tag": 0, "keyword": 0, "llm": 0, "stayed_unknown": 0, "errors": 0}
    t0 = time.time()
    for i, mem in enumerate(unknowns, 1):
        mem_id = mem["id"]
        tags = mem.get("tags") or []
        text = mem.get("text") or ""

        region = classify_by_tags(tags)
        source = "tag"
        if not region:
            region = classify_by_keywords(text)
            source = "keyword"
        if not region:
            region = classify_by_llm_expanded(text)
            source = "llm"
        if not region:
            counts["stayed_unknown"] += 1
            print(f"  [{i:>3}/{len(unknowns)}] {mem_id}  STAYED unknown  tags={tags[:3]}")
            continue

        ok = patch_region(mem_id, region, source)
        if not ok:
            counts["errors"] += 1
            continue
        counts[source] += 1
        print(f"  [{i:>3}/{len(unknowns)}] {mem_id}  {source:<7} -> {region}")

    elapsed = time.time() - t0
    print()
    print(f"done in {elapsed:.1f}s")
    for k, v in counts.items():
        print(f"  {k:<14} {v}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
