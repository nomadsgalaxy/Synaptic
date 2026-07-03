# What Synaptic is

Synaptic is an **agent-driven memory store with a brain-anatomy
visualization**. It's the thing your agent talks to when it needs
to remember something — and the dashboard you watch when you want
to *see* your agent's mind light up.

The product is two things in one repo:

1. **A memory backend** (`bridge/core`) — SQLite-backed, embedding-
   indexed, multi-tier-provider, sensitive-classified. Agents call
   `/recall` to fetch ranked memories; they synthesize answers
   themselves. No pretense of being smarter than the agent talking
   to it.

2. **A 3D brain dashboard** (`index.html`) — every memory is a
   neuron, anchored in an anatomically-labelled region per its
   tags. Shared tags become synapses. Live agent activity flares
   the regions in real time. This is the part that doesn't exist
   anywhere else in the space.

## What it isn't

- **Not a question-answering engine.** The agent is. `/reflect` is
  a thin convenience wrapper for callers without an agent in the
  loop — it templates retrieved memories into a minimal prompt and
  calls Tier 3. No classifiers, no abstention/contradiction
  detection. Production callers should prefer `/recall`.

- **Not a leaderboard chaser.** Synthetic memory benchmarks score
  the coupled "retrieve + answer" system as one unit. Synaptic
  decouples those layers on purpose. We optimize the retrieval
  layer (cross-encoder rerank, RRF fusion, multi-probe query
  rewriting, temporal-aware ordering, dynamic scaling with bank
  size). The answering layer is the agent's job.

- **Not a single-purpose product.** Same memory backend serves the
  hands-free dashboard use case (just watch the brain), the
  agent-as-power-user case (full MCP tool surface), and the
  in-house framework case (REST + WebSocket).

## What makes it the one to use

Real, observed, in-the-tree differentiators — not aspirational:

### Privacy by construction
Three-layer sensitive classifier (regex → tag-trigger → Tier-2 AI),
sticky-on flag (once flagged, can't be silently unflagged), egress
chokepoint that excludes flagged memories from Tier 3 calls unless
the caller explicitly opts in. LocalConcept filtering is
non-overridable. Audit log records every classifier flip with raw
reason codes for review.

### Observability you can see
Audit log, event ring buffer, hook log of every adapter call, and
the 3D brain region-firing visualization. Every memory has a known
provenance and is visible in context — no black-box "the system
decided".

### Dream cycle
A scheduled offline pipeline that consolidates memories: light
encoding (Phase 0), Phase 0b enrichment (fact-dense compression),
deduplication, schema synthesis, lexicon rebuild, MemoryMap
creation, decay, augmentation, cross-region linking, schema
reframing, weak-trace reinforcement, replay. The dream is what
turns raw event logs into a structured, queryable, anatomically-
organized memory.

### Multi-tier provider model
Tier 1 (Encoder), Tier 2 (Nightly), Tier 3 (Oracle), Tier
Embedding — each independently configurable, hot-swappable at
runtime, mixable across vendors (Ollama, OpenAI, Anthropic,
AirLLM, custom OpenAI-compatible). No single LLM is a hard
dependency; the system runs end-to-end on local Ollama or on
fully remote.

### Allen-atlas region routing
Memories aren't a flat blob — they're routed to anatomical brain
regions based on their tags. Hippocampus for episodic recall,
prefrontal cortex for planning, amygdala for emotional, motor
cortex for procedural, etc. The routing is content-aware (tag
classifier with rules + AI fallback), persistent, and visible in
the dashboard.

### Patient isolation
Multiple independent memory banks per install — work
profile / personal / shared / test. Each Patient has its own
SQLite file and complete isolation. Hot-swap between Patients
without restart.

## Retrieval pipeline

Production-grade out of the box:

- **Embedding cosine retrieval** with model-tracked vectors so an
  embedding-model upgrade doesn't silently break lookups.
- **BM25 + RRF fusion** for keyword-strong queries.
- **Cross-encoder rerank** (LLM-based) with dynamic visibility
  scaling — 20 candidates for small banks, up to 100 for large
  banks. Output-token budget scales with candidate count so JSON
  doesn't truncate.
- **Keyword-rewrite multi-probe.** Long natural-language questions
  automatically get a stopword-stripped variant and a top-content-
  token variant; probes are RRF-fused. Short declarative memories
  still surface when the user asks a verbose question.
- **Multi-event sub-query expansion.** Questions naming two
  anchors ("how long between X and Y?") embed each anchor
  separately so neither falls below the rerank floor.
- **Temporal-aware ordering.** "What did I do most recently?" /
  "list in chronological order" re-sorts the candidate set by
  CreatedAt rather than relevance rank.
- **Session co-retrieval.** When the top-K hits cluster into a few
  sessions, sibling memories from those sessions are folded in.

Every one of these is independently tunable per Patient via the
`recall_*` settings. Sensible defaults that work on a 100-memory
test bank and a 4000-memory production bank.

## Integration paths

- **MCP plugin** (`bridge/claude-code-plugin`) — marketplace
  install, full MCP tool surface for save / recall / search /
  dream / audit.
- **HTTP hooks** (`bridge/mcp-adapter`) — drop-in for MCP-aware
  clients.
- **REST + WebSocket** — your own agent loop or in-house
  framework. Two-line wire-up via the `report_event` /
  `report_memory_save` primitives.
- **Adapter-less direct emit** — POST events from any process
  with a bearer token.

The plugin/hook is always the cheapest path. The MCP server keeps
your data local; only the tier-3 oracle calls (if configured for
a remote provider) leave the machine.

## What it's not trying to be

We're not building a hosted memory-as-a-service. We're not
chasing scores on synthetic benchmarks where the rules favor
in-system answer-synthesis. We're not trying to be the smartest
piece of the stack — your agent already is.

Synaptic is the memory layer underneath your agent, indexed and
visualizable. Use the agent you actually use, point it here for
its long-term recall.
