"""Normalize non-canonical region_hint values across the active bank.

Two passes:
  1. Aliased — region_hint is in REGION_KEY_ALIASES (index.html). The UI
     already resolves it, but the stored value is non-canonical. Rewrite
     to the canonical form so backend/UI agree.
  2. Hallucinated — region_hint is neither canonical nor aliased. Use an
     anatomy-informed static table to remap each one to the closest
     valid canonical region (or "unknown" for genuinely-not-anatomy
     strings like "benchmark", "lateral_fissure", "session_summary_area").

Source of truth for canonical keys + alias mappings: dashboard
`REGION_INFO` and `REGION_KEY_ALIASES` in `Dev/index.html` (extracted by
`tools/_region_canon.json`). 38 canonical regions ported from the
Allen Human Reference Atlas + REM-style network labels.

This is idempotent: re-running after success patches 0 memories.
"""

from __future__ import annotations

import json
import os
import sys
import time
import urllib.request
import urllib.error
from collections import Counter

CORE_URL = os.environ.get("SD_CORE_URL", "http://localhost:9911")
TOKEN = os.environ["SD_API_TOKEN"]
HEADERS = {"Authorization": f"Bearer {TOKEN}", "Content-Type": "application/json"}

# Anatomy-informed remap for region_hint strings the LLM hallucinated
# during prior classifier runs. Each entry is justified by:
#   - real-anatomy correspondence (e.g. dorsal stream → parietal_lobe)
#   - misspelling / variant of a canonical key
#   - "unknown" when the string is non-anatomical garbage
HALLUCINATION_REMAP: dict[str, str] = {
    # Misspellings / variants of canonical keys
    "pre-frontal_lobe":                                 "prefrontal_cortex",
    "prefrontal_lobe":                                  "prefrontal_cortex",
    "prefrontal cortex":                                "prefrontal_cortex",
    "prefrontal_area":                                  "prefrontal_cortex",
    "lateral_pre-frontal_cortex":                       "prefrontal_cortex",
    "prefrontal cortex/decision_making":                "prefrontal_cortex",
    "dorsal_prelimbic_cortex":                          "prefrontal_cortex",   # medial PFC homologue
    "frontal lobe":                                     "frontal_lobe",
    "anterior-cingulate-cortex":                        "anterior_cingulate",
    "anterior_cingulate_gyrus":                         "anterior_cingulate",
    "dorsolateral_pFC":                                 "dorsolateral_prefrontal_cortex",
    "dorsal_lateral_frontal_cortex":                    "dorsolateral_prefrontal_cortex",

    # Lateralized variants
    "left_temporal_lobe":                               "temporal_lobe_left",
    "left-temporal_lobe":                               "temporal_lobe_left",
    "lateral-temporal-lobe":                            "temporal_lobe_left",  # most usage = LH temporal
    "lateral_temporal_cortex":                          "temporal_lobe_left",
    "temporal_lobe/right_hemisphere":                   "temporal_lobe_right",
    "left_inferior_frontal_gyrus":                      "inferior_frontal_gyrus",

    # Visual processing variants
    "visual_processing_area":                           "visual_cortex",
    "visual-association-area":                          "visual_cortex",
    "lateral_occipital":                                "occipital_lobe",
    "lateral-occipital-complex":                        "occipital_lobe",
    "dorsal_stream":                                    "parietal_lobe",       # occipital→parietal pathway

    # Parietal-region variants + TPJ family (TPJ sits in inferior parietal)
    "dorsal_parietal_area":                             "parietal_lobe",
    "dorsal_parietal":                                  "parietal_lobe",
    "temporal_parietal_lobe":                           "parietal_lobe",
    "parietal_lobe/temporal_assoc_area":                "parietal_lobe",
    "intraparietal sulcus":                             "parietal_lobe",
    "temporo-parietal":                                 "inferior_parietal_lobe",
    "temporal_parietal_junction":                       "inferior_parietal_lobe",
    "temporo-parietal junction":                        "inferior_parietal_lobe",
    "temporal-parietal":                                "inferior_parietal_lobe",

    # Network / function labels mapped to closest atlas region
    "default_lobe":                                     "default_mode_network",
    "default_area":                                     "default_mode_network",
    "pre-frontal_lobe/occipital-temporal_network":      "default_mode_network",
    # 2026-05-17: dorsal_attention_network is now a canonical region — drop
    # the prior parietal_lobe fallback so re-runs don't downgrade it.
    "social_cognition":                                 "angular_gyrus",       # RH TPJ (in angular gyrus) is the modern canonical substrate
    "temporal_associative_network":                    "temporal_lobe",

    # Generic "cerebral cortex" variants — neocortex ≈ cerebral_cortex
    "associative_cortex":                               "cerebral_cortex",
    "neocortex_computational_models":                   "cerebral_cortex",
    "neocortex_research_backend":                       "cerebral_cortex",

    # Non-anatomy / system tags / junk → Intrusive Thoughts
    "benchmark":                                        "unknown",
    "lateral_fascia":                                   "unknown",             # not brain anatomy
    "lateral_fissure":                                  "unknown",             # a sulcus, not a region
    "lateral_ventricle":                                "unknown",             # CSF space
    "lateral_lobe":                                     "unknown",             # not a region
    "session_summary_area":                             "unknown",
    "default-security-protocols":                       "unknown",
    "frontend-architecture":                            "unknown",
}


def http_json(method: str, url: str, body: dict | None = None, timeout: float = 30.0):
    data = json.dumps(body).encode("utf-8") if body is not None else None
    req = urllib.request.Request(url, data=data, headers=HEADERS, method=method)
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        raw = resp.read()
        return json.loads(raw) if raw else {}


def load_canon() -> tuple[set[str], dict[str, str]]:
    path = os.path.join(os.path.dirname(__file__), "_region_canon.json")
    with open(path, encoding="utf-8") as f:
        d = json.load(f)
    return set(d["canon"]), d["aliases"]


def pull_all_memories() -> list[dict]:
    data = http_json("GET", f"{CORE_URL}/bank/memories?limit=20000", timeout=120)
    return data.get("memories", []) if isinstance(data, dict) else []


def patch_region(mem_id: str, region: str) -> bool:
    try:
        http_json("PATCH", f"{CORE_URL}/bank/memories/{mem_id}",
                  {"region_hint": region}, timeout=30)
        return True
    except urllib.error.HTTPError as exc:
        body = exc.read()[:200]
        print(f"  PATCH {mem_id} -> {region!r} failed: {exc.code} {body}",
              file=sys.stderr)
        return False
    except Exception as exc:
        print(f"  PATCH {mem_id} -> {region!r} error: {exc}", file=sys.stderr)
        return False


def main() -> int:
    canon, aliases = load_canon()
    print(f"canonical keys: {len(canon)}")
    print(f"alias mappings: {len(aliases)}")
    print(f"hallucination remaps: {len(HALLUCINATION_REMAP)}")
    print(f"pulling memories from {CORE_URL}…")
    memories = pull_all_memories()
    print(f"total memories: {len(memories)}")

    plan: list[tuple[str, str, str, str]] = []   # (id, old, new, source)
    unhandled: Counter = Counter()
    for m in memories:
        old = (m.get("region_hint") or "").strip()
        if not old or old in canon:
            continue
        new = aliases.get(old) or HALLUCINATION_REMAP.get(old)
        if not new:
            unhandled[old] += 1
            continue
        if new == old:
            continue
        plan.append((m["id"], old, new,
                     "alias" if old in aliases else "remap"))

    print(f"planned patches: {len(plan)}")
    print(f"unhandled keys:  {len(unhandled)}")
    if unhandled:
        print("  (these will remain non-canonical — extend HALLUCINATION_REMAP):")
        for k, n in sorted(unhandled.items(), key=lambda x: -x[1]):
            print(f"    {n:>3}  {k!r}")
    if not plan:
        print("nothing to do")
        return 0

    counts = Counter()
    t0 = time.time()
    for i, (mid, old, new, source) in enumerate(plan, 1):
        if patch_region(mid, new):
            counts[(source, new)] += 1
        else:
            counts["__error__"] += 1
        if i % 50 == 0 or i == len(plan):
            print(f"  [{i:>4}/{len(plan)}] {time.time()-t0:.1f}s")

    print(f"\ndone in {(time.time()-t0)/60:.1f} min")
    print(f"  errors: {counts.pop('__error__', 0)}")
    print(f"  by destination region:")
    by_dest: Counter = Counter()
    for (source, dest), n in counts.items():
        by_dest[(source, dest)] += n
    for (src, dest), n in sorted(by_dest.items()):
        print(f"    {src:<6} -> {dest:<32} {n}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
