# Writing memories for Synaptic

A practical guide for **AI agents** (Claude, GPT, Gemini, local LLMs, custom agents) writing memories to a memory backend that Synaptic visualizes. Following this guide makes the brain look anatomically meaningful and rich — every memory lands in a sensible region, related memories form synaptic connections, and the visualization tells a real story about what the agent has been thinking about.

This guide is intentionally short. Read it once and keep tagging accordingly.

---

## Why this matters

Synaptic turns each memory into a **neuron** placed inside a **brain region**, and turns shared tags between memories into **synaptic pathways** connecting them. Two things drive the visual:

1. **The classifier** parses each memory's tags (and falls back to text keywords) to assign it to one of 14 brain regions.
2. **The synapse builder** connects memories that share **2 or more tags**.

Bad tagging → memories land in the default region (frontal lobe), don't connect to anything, and the brain looks like a featureless cloud. Good tagging → memories land in the right anatomical place, form clusters with their relatives, and the brain reads like an MRI of the agent's actual cognition.

---

## Required: every memory needs `tags`

Every memory you save MUST include a `tags` array with **3–5 entries**, drawn from at least three of the four buckets below:

| Bucket | Why it matters | Example tags |
|---|---|---|
| **Region** (1+ required) | Routes the neuron to a brain region. | `feature`, `bugfix`, `architecture`, `tool` |
| **Topic** (1+ required) | Builds synaptic connections with other memories on the same topic. | `oauth`, `claude-mcp`, `voxel-layout` |
| **Project** (1+ required) | Lets dashboards filter or group by project. | `project:event-tracker`, `project:gravity` |
| **Identity** (optional) | Lets dashboards filter by who/where. | `user:alex`, `repo:synaptic-disorder` |

**A memory with fewer than 2 shared tags with any other memory ends up "orphaned"** — visible but disconnected. Aim for 3–5 well-chosen tags per memory.

---

## Region routing cheat sheet

These tags route memories to specific brain regions. Pick at least one when saving. The regions are anatomically meaningful — the agent's "speech" goes to Broca's area, "memory" goes to the temporal lobe, "errors" go to the amygdala (the brain's threat-response center), etc.

| Region | What it represents | Tag patterns |
|---|---|---|
| **Prefrontal Cortex** | Agent orchestration, planning, design decisions | `agent`, `subagent`, `workflow`, `orchestration`, `planning`, `design-decision`, `decision`, `ai-cascade` |
| **Frontal Lobe** | Personality, preferences, project overviews | `feedback`, `directive`, `user-profile`, `personality`, `preferences`, `feature`, `purpose`, `overview`, `project` |
| **Broca's Area** | Generated output (responses, docs, summaries) | `output`, `response`, `writing`, `summary`, `documentation`, `docs` |
| **Wernicke's Area** | Understanding user intent / parsing prompts | `prompt`, `intent`, `parsing`, `comprehension` |
| **Visual Cortex** | Web, screenshots, UI/UX, browsers, 3D, images | `screenshot`, `chrome-mcp`, `ui`, `ux`, `visual`, `image`, `web`, `browser`, `react`, `html`, `css`, `3d`, `cad` |
| **Temporal Lobe (L/R)** | Long-term knowledge, references, facts | `memory_type:fact`, `memory_type:rule`, `memory_type:procedure`, `reference`, `knowledge`, `music`, `audio` |
| **Hippocampus** | Memory consolidation itself | `consolidated`, `mental-model`, `bank`, `hindsight`, `memory` |
| **Parietal Lobe** | Code structure, architecture, schemas | `architecture`, `tech-stack`, `repo-layout`, `code`, `system-design`, `math`, `logic`, `data`, `config`, `schema` |
| **Motor Cortex** | Tool calls, MCP, deployments, ops | `tool`, `mcp`, `bash`, `exec`, `action`, `deployment`, `operations`, `setup`, `github`, `cli`, `cloudflare`, `docker` |
| **Cerebellum** | Testing, CI, auth flows (fine-motor coordination) | `testing`, `ci`, `test`, `coordination`, `auth` |
| **Amygdala** | Errors, gotchas, warnings, incidents | `error`, `gotcha`, `incident`, `warning`, `bug`, `bugfix` |
| **Corpus Callosum** | Inter-system bridges (Slack, MCP, observability) | `slack`, `discord-bridge`, `bridge`, `inter-agent`, `mitmproxy`, `observability`, `ai-tooling` |
| **Brain Stem** | Creative generation, imagination, dreams | `creative`, `generation`, `imagination`, `art`, `dream` |

Don't have a fitting region tag? Memories without one default to the **Frontal Lobe** (general-purpose). It's a graceful fallback — but the brain looks much better when memories actually live where they should.

---

## Memory types

Every memory carries a `memory_type` that tells Synaptic what kind of record it is. The value defaults to `episodic` if you don't set it.

| Value | What it is | Who writes it |
|---|---|---|
| `episodic` | Raw moments — the default. A single event, fact, observation, or directive captured as you go. | You (agents). |
| `synthesis` | Phase 2 cluster-summary memories. Carries `synthesis_source_ids` pointing back at the episodics it abstracts. | The nightly pipeline. Don't write these by hand. |
| `schema` | Phase 8 generalised pattern memories. Carries `schema_source_ids`. | The nightly pipeline. |
| `context` | Phase 8.7 region-level context frameworks. Feature-flagged OFF by default. | The nightly pipeline, when the flag is on. |
| `reflection` | Inline takeaways produced by `/reflect` when `save_as_memory=true`. Treat as a curated insight that lives alongside episodics. | You — via `/reflect`. |

You can leave `memory_type` unset for ordinary moment-capture. Set it to `reflection` only when the memory is the considered output of a reflection pass, not a raw observation.

---

## What good tagging looks like

```json
{
  "id": "evt-trkr-mobile-pwa",
  "text": "Event Tracker mobile PWA shipped Monday with offline caching via service worker; pre-cached 200kb of CSS/JS at install time.",
  "tags": ["feature", "react", "ui", "deployment", "project:event-tracker"]
}
```

Why this works:
- `feature` → Frontal Lobe (overview / preferences region)
- `react` + `ui` → Visual Cortex (browser/UI/UX)
- `deployment` → Motor Cortex (ops/tool execution)
- `project:event-tracker` → groups with other Event Tracker memories
- 5 tags → very likely to share 2+ with related memories → strong synaptic connections

```json
{
  "id": "voxel-layout-warp-bug",
  "text": "Voxel densification looped forever when motorcortex saturated past 25k memories; fixed by capping target at 2x baseline + 200ms wall-clock budget.",
  "tags": ["bugfix", "voxel-layout", "performance", "project:synaptic-disorder", "gotcha"]
}
```

Why this works:
- `bugfix` + `gotcha` → Amygdala (errors/warnings)
- `voxel-layout` → topic tag, connects with other voxel-layout memories
- `performance` → topic tag
- `project:synaptic-disorder` → project filter

---

## What bad tagging looks like

```json
{
  "id": "ev-001",
  "text": "We talked about the dashboard.",
  "tags": []
}
```

❌ Empty tags. Routes to the default region (frontal lobe). No connections. Orphaned neuron.

```json
{
  "id": "ev-002",
  "text": "I shipped a thing yesterday.",
  "tags": ["yesterday", "thing"]
}
```

❌ Vague tags that don't match any region pattern AND don't share with any other memory. Lands in the default region with no connections.

```json
{
  "id": "ev-003",
  "text": "Fixed bug in OAuth flow.",
  "tags": ["bug"]
}
```

⚠️ Only one tag. The classifier puts it in the amygdala correctly, but it can't form synaptic connections (the synapse builder needs 2+ shared tags). Add `oauth`, `auth`, `bugfix`, and `project:<name>` and now it's wired up.

---

## Tagging recipes for common memory types

### A new feature shipped
```
["feature", "<topic-tag>", "<ui|backend|tool>", "deployment", "project:<name>"]
```

### A bug fix
```
["bugfix", "<topic-tag>", "<error-cause-tag>", "project:<name>"]
```

### A user preference / directive
```
["directive", "user-profile", "<topic-tag>", "user:<name>"]
```

### A documentation note
```
["documentation", "memory_type:fact", "<topic-tag>", "project:<name>"]
```

### An architecture decision
```
["architecture", "design-decision", "<tech-stack-tag>", "project:<name>"]
```

### A test / CI artifact
```
["testing", "ci", "<framework-tag>", "project:<name>"]
```

### A tool call result you want to remember
```
["tool", "<tool-name>", "<topic-tag>", "project:<name>"]
```

### An error / gotcha to prevent future regressions
```
["error", "gotcha", "<topic-tag>", "<technology-tag>", "project:<name>"]
```

---

## Lifecycle: dormant memories, TTLs, and consolidation

A memory is not just "alive or deleted." Synaptic tracks a small lifecycle on every row:

- **`expires_at`** — an optional TTL trigger. When the time passes, the memory is put **dormant**, not deleted. Set it for facts you expect to go stale (a temporary credential rotation note, a "remind me to check this next month").
- **`dormant_at` / `dormant_reason`** — a dormant memory stays in the bank but is hidden from default `/recall` results. Recover it by passing `include_dormant=true`, or wake it manually with `sd_set_dormant`. This is the Dormant Memories framework: nothing is lost, just resting.
- **`consolidation_stage`** — tracks the hippocampus-to-neocortex migration (per Stickgold & Walker 2010): `episodic` → `consolidating` → `semantic`. The nightly pipeline advances this stage; you don't set it by hand, but you can read it to understand why a memory feels more like a fact than a moment.

Two rules worth knowing:

1. **TTL puts a memory dormant; it does not delete it.** Use `include_dormant=true` on `/recall` to bring expired memories back into view, or `sd_set_dormant` to wake one explicitly.
2. **Sensitive memories reject TTLs.** A sticky-on sensitive row cannot have an `expires_at` set — the system refuses the field rather than risk silently dormant-ing a secret you wanted to keep tracked.

---

## Sensitive memories

The `sensitive` flag blocks a memory from ever reaching Tier 3 (Oracle / external research / web fetch) egress. It's enforced in two layers:

1. **Layer 1 — auto-flag at write.** A regex scanner catches API keys, JWTs, SSNs, and similar patterns; tag triggers (e.g., a `sensitive` tag) also flip the flag.
2. **Layer 2 — AI classifier on update.** When a memory is edited, a classifier re-scans the text and can raise the flag.

Once `sensitive=true`, the flag is **sticky-on**: it stays set for the life of the row, and the row is permanently excluded from Tier 3 egress. As noted above, sensitive memories also cannot carry a TTL.

For author-side memories, the practical guidance is: if your memory text contains a secret, a personal identifier, or anything you wouldn't want in an external research prompt, let the auto-flag do its job and don't fight it.

---

## Entities and aliases

Synaptic has an entity registry (`entities` + `entity_aliases` tables) that lets you register canonical names and their aliases — for example, `AC` → `Anthropic`, or `evt-trkr` → `Event Tracker`. At query time, `/recall` automatically expands aliases, so a search for `AC` will surface memories written about `Anthropic` and vice versa.

This means you don't have to normalise nicknames in your memory text. Write naturally with whatever name you'd actually use; register the alias once and the retrieval layer handles the rest.

---

## Retrieval — how `/recall` ranks results

Knowing how retrieval works helps you write memories that surface when they should:

- **Hybrid BM25 + cosine via RRF.** `/recall` runs both a lexical (BM25) and a semantic (cosine over embeddings) search, then merges them with Reciprocal Rank Fusion (Cormack k=60). That means strong keyword signal AND strong semantic signal both pay off — there's no need to choose one.
- **Optional rerank pass.** A second pass weighted by salience, cosine, and `recall_strength` is available (Bundle Q). It's off by default; when on, recently-useful memories with high salience get a boost.

Practical implication: clear, specific text with the right vocabulary helps BM25, and concise memories with focused meaning help cosine. The advice in "Memory text quality" below covers both.

---

## Region hints and the entity-aware bank

The `region_hint` field still drives Phase 4 maps and Phase 7 cross-region edges — it's the strongest signal you can give the classifier when your tags are sparse. Combined with the entity registry above, this means the bank handles both *where a memory lives anatomically* and *what real-world thing it refers to* as separate concerns. You don't need to encode entity identity into tags; use `region_hint` for placement and let aliases handle naming.

---

## Memory text quality

Tags drive the visualization, but **the memory text drives Tier 2 keyword classification** (the fallback when tags don't match a region pattern). Two practical rules:

1. **Be specific.** "Fixed the OAuth callback to handle PKCE properly" beats "fixed auth thing." The keywords `oauth`, `pkce`, and `callback` are signal.
2. **Stay concise.** 1–3 sentences. Memories are extracted, not stored verbatim — too much prose dilutes the signal and bloats the record.

---

## Saving memories — the actual API

### Via MCP (any MCP-aware client: Claude Desktop, Cursor, Cline, Continue, Gemini CLI…)

```js
report_memory_save({
  text: "the user prefers descriptive-but-compact button labels (two-word over one-word).",
  tags: ["directive", "user-profile", "ui", "user:alex"],
  memory_id: "user-anthony-ui-prefs-2026-05-06",  // optional — stable ID for dedupe / cross-system linkage
  region_hint: "frontal_lobe"                      // optional — classifier handles routing
})
```

Parameters:

| Param | Required | Notes |
|---|---|---|
| `text` | yes | 1–3 sentence description of the memory |
| `tags` | strongly recommended | 3–5 tags from the region routing cheat sheet — drives both region placement and synapse connections |
| `memory_id` | no | Caller-supplied stable id. Useful for deduplication or to preserve identity when bridging from another memory backend. |
| `region_hint` | no | Override the classifier only when your tags are sparse. Valid keys listed below. |
| `session_id` | no | Omit — the server uses its own session id |

Valid `region_hint` values: `prefrontal_cortex`, `frontal_lobe`, `parietal_lobe`, `motor_cortex`, `visual_cortex`, `temporal_lobe_left`, `temporal_lobe_right`, `hippocampus`, `amygdala`, `cerebellum`, `brain_stem`, `broca_area`, `wernicke_area`, `corpus_callosum`.

The `region_hint` is optional. Include it only if you know better than the classifier (e.g., your tags are sparse but you know the memory belongs in a specific region).

### Via the SDK

```python
import synaptic as sd
with sd.connect(adapter_id="my-app", model="my-model") as client:
    client.report_memory_save(
        text="...",
        tags=["feature", "deployment", "project:my-app"]
    )
```

```ts
import { connect } from '@synaptic/sdk';
const client = connect({ adapterId: 'my-app', model: 'my-model' });
await client.reportMemorySave({
  text: '...',
  tags: ['feature', 'deployment', 'project:my-app'],
});
```

### Via direct event posting

`POST http://localhost:9911/event` with body:

```json
{
  "schema_version": "1.0",
  "type": "memory_added",
  "timestamp": "2026-05-06T15:30:00Z",
  "adapter_id": "my-tool",
  "session_id": "session-abc",
  "payload": {
    "memory_id": "uuid-here",
    "text": "...",
    "tags": ["feature", "deployment", "project:my-app"]
  }
}
```

---

## Mutating memories after the fact

Authoring isn't only "write once." Synaptic exposes four agent-callable tools for shaping the bank over time:

| Tool | What it does | When to use it |
|---|---|---|
| `sd_forget` | Soft-delete with an explicit reason. The row stays for audit but is hidden from `/recall`. | A memory turned out to be wrong, redundant, or shouldn't have been written. |
| `sd_supersede` | Replace fact A with fact B, recording a bi-temporal edge (R10) between them. | A fact has changed — a config value, a person's role, a deployment URL. Don't just write a contradicting memory; supersede the old one. |
| `sd_set_dormant` | Manually put a memory to sleep or wake it back up. | You want a memory out of the default `/recall` view without giving it a TTL, or you want to wake a dormant memory before its TTL would have fired. |
| `sd_set_ttl` | Set or clear `expires_at` on a memory. | You realise after writing that a memory has a known expiry, or you want to remove a TTL you set earlier. |

Use `sd_supersede` over writing-a-correction-as-a-new-memory whenever you can — the bi-temporal edge keeps the lineage intact and stops `/recall` from returning both the old and new fact as equally weighted.

---

## Bridging from an external memory backend

If your agent already writes memories to a separate backend (any
vector store, knowledge graph, or document index), keep doing that —
Synaptic does not try to be your only memory layer. Call
`report_memory_save` from your agent alongside the external write so
the dashboard reflects what's actually being stored. Use the **same
text and tags** for both calls so the visualization stays in sync.

```js
// 1. Write to your external memory backend (whatever you already use)
externalStore.save({
  content: "Event Tracker shipped PWA offline caching via service worker; pre-cached 200kb at install time.",
  tags: ["feature", "pwa", "deployment", "project:event-tracker"],
  id: "event-tracker-pwa-2026-05-06"
})

// 2. Notify Synaptic so the neuron lights up in the dashboard
mcp__synaptic__report_memory_save({
  text: "Event Tracker shipped PWA offline caching via service worker; pre-cached 200kb at install time.",
  tags: ["feature", "pwa", "deployment", "project:event-tracker"],
  memory_id: "event-tracker-pwa-2026-05-06"      // same stable id as the external write
})
```

**If `report_memory_save` fails or isn't available, continue silently** — the dashboard is non-critical.

### Write vs. read events

The brain distinguishes two types of memory activity:

| Operation | SD event type | What it looks like |
|---|---|---|
| Save a new memory | `memory_added` | Hippocampus lights up — a new memory was encoded |
| Recall, reflect, search, model | `memory_recall` | Hippocampus pulses — memory is being read |

The Claude Code hook forwarder (`forward.js`) handles this
automatically. If you are posting events directly, use
`type: "memory_added"` for writes and `type: "memory_recall"` for
all read operations.

### Stable ids and timestamps

When bridging from an external backend, pass that backend's stable
record id as `memory_id`. Without a stable id, every event creates a
new random UUID and the dashboard can't dedupe across event streams.
Use a deterministic shape like `<project>-<topic>-<date>` (e.g.,
`event-tracker-arch-2026-05-04`).

---

## Quick reference card

When in doubt, use this minimal recipe:

```
[<region-pattern-tag>, <topic-tag>, <topic-tag>, project:<name>]
```

That's 4 tags, hits all the buckets except identity, and gives the synapse builder enough signal to wire connections. Add a 5th identity tag (`user:<name>`, `repo:<name>`) when relevant.

**Goal:** every memory you save should land in a meaningful region AND share 2+ tags with at least one other memory in the bank. If you hit those two bars, the brain looks alive.
