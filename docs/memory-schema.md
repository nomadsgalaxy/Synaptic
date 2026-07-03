# Synaptic Memory Source Schema v1.0

Normalized memory record format. Memory providers translate from their native store (JSON file, Markdown folder, SQLite, or imports from other tools) into this schema. Stable, semver'd contract.

## Envelope

```json
{
  "schema_version": "1.0",
  "provider_id": "hindsight",
  "generated_at": "2026-05-05T22:30:00Z",
  "memories": [ /* ... */ ]
}
```

| Field | Type | Required | Notes |
|---|---|---|---|
| `schema_version` | string `"1.0"` | yes | |
| `provider_id` | string | yes | Stable id, e.g. `"hindsight"`, `"json"`, `"markdown-folder"`, `"mcp-ai-reported"` |
| `generated_at` | ISO 8601 | recommended | Helps the dashboard show freshness |
| `memories` | array of memory records | yes | See below |

## Memory record

| Field | Type | Required | Notes |
|---|---|---|---|
| `id` | string | yes | Stable within the provider; treat as opaque |
| `text` | string | yes | The memory content |
| `tags` | array of strings | no | Used by the layered classifier for region assignment |
| `context` | string | no | Often a project name, e.g. `"project:event-tracker"` |
| `fact_type` | enum or null | no | `observation`, `world`, `experience`, `opinion`, or null |
| `created_at` | ISO 8601 | no | Used for the timeline scrubber and "memory age" stats |
| `updated_at` | ISO 8601 | no | |
| `linked_to` | array of strings | no | Explicit graph edges; supplements computed synapses |
| `source_metadata` | object | no | Provider-specific extras passed through |

Additional fields are allowed (the schema sets `additionalProperties: true` on memory records) so providers can pass through extras the dashboard may use later.

## Region assignment

The dashboard places each memory in an anatomical region using a layered classifier (see `assets/anatomy.json` for region definitions):

1. **Tag match** — overlap with each region's `tag_patterns`. Highest count wins.
2. **Keyword fallback** — if no tags match, scan the memory `text` for region keyword patterns.
3. **Embedding fallback** (optional) — local SBERT similarity to region function descriptions.

Providers can ship their own tag-to-region overrides at `tools/memory_providers/<provider>/region_overrides.json`.

## Transport

Two modes:

- **Bulk file** — the provider writes a snapshot to `assets/memories.json` matching this schema. The dashboard reads on load and on a slow polling cadence.
- **Live delta** — the provider posts `memory_added` / `memory_updated` / `memory_deleted` events to SD Core (see `event-schema.md`). The dashboard updates incrementally.

Most providers ship the bulk mode for v1; live delta is recommended where the source supports change notifications (mtime watch, websocket subscription, etc.).

## Versioning

Same discipline as the event schema. Additive changes ship as `1.x`; breaking changes require `2.0` with a migration period.
