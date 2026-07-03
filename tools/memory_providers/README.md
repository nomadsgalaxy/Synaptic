# Memory providers

Each provider reads memories from a different source and normalizes them
into the canonical `assets/memories.json` shape that the dashboard consumes.
All conform to **Memory Source Schema v1.0** (see `docs/memory-schema.md`).

| Provider | Source | Best for |
|---|---|---|
| `hindsight_provider.py` | Hindsight backup JSON, or a running `hindsight-local-mcp` | Anthony's primary path; anyone using Hindsight |
| `json_provider.py` | A hand-authored JSON file | Universal "bring your own" — works for any system that can export JSON |
| `markdown_provider.py` | A folder of `.md` notes (Obsidian-compatible) | Obsidian users, plain-text note-takers, CLAUDE.md collections |

## Quick start

After running a provider, **also re-run the brain build** so the new memories
get classified, placed on voxels, and joined into the synapse graph:

```bash
# Use whichever provider matches your setup
python tools/memory_providers/json_provider.py path/to/your/memories.json
# or
python tools/memory_providers/markdown_provider.py path/to/notes-folder
# or
python tools/memory_providers/hindsight_provider.py        # latest backup
python tools/memory_providers/hindsight_provider.py --live # query live MCP

# Then rebuild the brain (voxels + paths + classification)
python tools/build_voxel_brain.py
```

Reload the dashboard tab and the new memories appear, classified by the
layered classifier (`tools/classifier.py`).

## Schema

Every provider outputs:

```json
{
  "schema_version": "1.0",
  "provider_id": "<provider>",
  "generated_at": "<ISO 8601>",
  "count": <int>,
  "memories": [ /* records, see docs/memory-schema.md */ ],
  "items":    [ /* alias of memories for the dashboard's reader */ ]
}
```

Per-record required: `id`, `text`. Optional: `tags`, `context`, `fact_type`,
`created_at`, `updated_at`, `linked_to`, `source_metadata`, `mentioned_at`.

## Watch mode

JSON and Markdown providers support `--watch`:

```bash
python tools/memory_providers/json_provider.py memories.json --watch
python tools/memory_providers/markdown_provider.py ~/notes --watch
```

The script polls every 2-3 seconds and re-runs when it detects changes.

## Custom providers

Drop a new script next to these. Three things to do:

1. Read whatever you read.
2. Coerce each record into a dict with at least `id` and `text`.
3. Hand the list to `write_canonical(memories, provider_id="your-name", out_path=...)`.

See `__init__.py` for the helpers.
