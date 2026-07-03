"""
Synaptic memory providers.

A memory provider reads memories from some backing store (Hindsight, a JSON
file, a folder of Markdown notes, etc.) and translates them into the
normalized Memory Source Schema v1.0 (see docs/memory-schema.md).

Each provider is a self-contained CLI script. They all output the same
JSON shape — what the dashboard's `assets/memories.json` expects:

  {
    "schema_version": "1.0",
    "provider_id": "<provider name>",
    "generated_at": "<ISO 8601>",
    "memories": [ {id, text, tags, context, fact_type, created_at, ...}, ... ],
    "count": <int>,
    "items": [ ... ]   # alias of memories for backwards compat with the dashboard
  }

This module exposes shared helpers for validation + writing the canonical
output file. Per-provider scripts live next to it.
"""
from __future__ import annotations
import json
import datetime as _dt
from pathlib import Path
from typing import Iterable

SCHEMA_VERSION = "1.0"

# Required fields per memory record
REQUIRED = ("id", "text")

# Optional fields preserved as-is when present
OPTIONAL = (
    "tags", "context", "fact_type", "created_at", "updated_at",
    "linked_to", "source_metadata", "mentioned_at",
)

def now_iso() -> str:
    return _dt.datetime.now(_dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

def normalize_one(raw: dict, *, provider_id: str) -> dict:
    """Coerce a raw memory dict into a schema-conformant record.

    - Unknown keys are dropped (kept under `source_metadata` if non-empty).
    - Required fields raise on missing.
    - Tags are coerced to a list of strings.
    """
    out = {}
    for key in REQUIRED:
        if key not in raw or raw[key] is None:
            raise ValueError(f"memory missing required field {key!r}: {raw}")
        out[key] = raw[key]
    for key in OPTIONAL:
        if key in raw and raw[key] is not None:
            out[key] = raw[key]
    # Tag coercion
    if isinstance(out.get("tags"), str):
        out["tags"] = [t.strip() for t in out["tags"].split(",") if t.strip()]
    if "tags" in out and not isinstance(out["tags"], list):
        out["tags"] = list(out["tags"])
    # Anything else lands under source_metadata so providers can pass through extras
    extras = {k: v for k, v in raw.items()
              if k not in REQUIRED and k not in OPTIONAL}
    if extras:
        existing = out.get("source_metadata") or {}
        if isinstance(existing, dict):
            existing.update(extras)
            out["source_metadata"] = existing
        else:
            out["source_metadata"] = extras
    return out

def write_canonical(memories: list[dict], *, provider_id: str, out_path: Path | str) -> dict:
    """Write the canonical memories.json that the dashboard consumes.

    Returns a stats dict: { "n": int, "path": str, "size_kb": int }.
    """
    out_path = Path(out_path)
    payload = {
        "schema_version": SCHEMA_VERSION,
        "provider_id": provider_id,
        "generated_at": now_iso(),
        "count": len(memories),
        "memories": memories,
        # backward-compat alias for the dashboard's existing reader
        "items": memories,
    }
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(json.dumps(payload, ensure_ascii=False), encoding="utf-8")
    return {"n": len(memories), "path": str(out_path),
            "size_kb": round(out_path.stat().st_size / 1024)}

def normalize_many(raws: Iterable[dict], *, provider_id: str) -> list[dict]:
    out = []
    skipped = 0
    for raw in raws:
        try:
            out.append(normalize_one(raw, provider_id=provider_id))
        except ValueError as e:
            skipped += 1
            print(f"  [skip] {e}", flush=True)
    if skipped:
        print(f"  ({skipped} memories skipped due to validation errors)")
    return out
