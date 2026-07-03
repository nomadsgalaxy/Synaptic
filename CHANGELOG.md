# Changelog

All notable changes to **Synaptic** (previously released as "Synaptic Disorder")
are documented here. Releases are listed newest-first.

Versioning scheme (since v2.0.0a, 2026-05-08): `vM.F.P<suffix>`
where suffix is `a<n>` alpha, `b<n>` beta, `rc<n>` release candidate, or
omitted for stable. The beta-wave number (`b1`, `b2`, …) advances when the
beta-tester pool expands, independently of patch increments.

Older releases used a date-coded scheme (`vYY.M.D.<n>`) and lived under
`Releases/Alpha/Old Beta/`. They predate the formal notes pipeline.

---

## [v3.0.0b1] — 2026-05-15

**Theme:** the re-anchoring release. Synaptic stops pretending to be
smarter than the agent talking to it; the in-system "smart synthesis"
layer that grew inside `/reflect` is stripped, and what's left is a
smaller, more honest indexed memory store that gets out of the way.
Consolidates all internal v2.6, v2.7, and v2.8 cycle work (Bundles A
through S, the Patients system, hybrid retrieval, sensitive classifier
overhaul, dormant memories, entity registry, procedural memories,
hook auto-synthesis, atlas search, pagination, region reprocess, and
the agent-driven architectural pivot) — see
`Releases/Beta/synaptic-3.0.0b1-NOTES.md` for the full breakdown.

### Highlights

- **Agent-driven pivot.** `/reflect`'s precheck classifier, dual-recall
  negation embedding, contradiction scanners, paired-memory hoisting,
  and timeline-spread injector are all gone. `/reflect` is now a thin
  convenience wrapper around the retrieval pipeline + a minimal "answer
  from these memories" prompt. Production callers should prefer
  `/recall` and reason over the results in their own agent.
- **Patients — multiple isolated memory banks per install.** New
  `default` Patient inherits existing data; additional Patients each
  get their own SQLite bank under `<data-dir>/patients/<id>/sd_bank.db`.
  Boot-time login overlay; header chip with switch / browse / rename /
  delete; live bank swap at runtime (no core restart); advisory pin to
  protect long-running sessions from accidental switches; read-only
  browse so an inactive Patient's contents can be inspected without
  swapping. 9 new REST endpoints under `/patients/*`.
- **Hybrid retrieval (Bundle J).** `/recall` runs BM25 alongside cosine
  and fuses via reciprocal rank fusion. Lexical surface forms survive
  embedding drift. Default ON; togglable via `recall_hybrid_enabled`.
- **Default embed model flipped (Bundle P)** from `llama3.2:3b` to
  `nomic-embed-text:v1.5`. Auto-backfill on first start; recall keeps
  working through the migration.
- **Dormant memories (Bundle L).** TTL triggers a dormant state, not a
  hard delete. `expires_at` + `dormant_at` + `dormant_reason` columns;
  `/recall?include_dormant=true` to peek; four new MCP tools
  (`sd_set_ttl`, `sd_set_dormant`, `sd_supersede`, `sd_forget`).
- **Auto-dedup at write time (Bundle K).** Near-duplicates merge into
  the existing row at `SaveMemory` time instead of waiting for the
  Phase 2 dream cycle.
- **Entity registry (Bundle N) + procedural memories (Bundle O).**
  Aliases fan into canonical names at recall time; user-saved
  procedures auto-inject as standing directives into Tier 1 prompts.
- **Hook auto-synthesis (Bundle S)** — error / pattern / decision /
  end-of-session triggers fire Tier 3 synthesis from the agent's hook
  event stream.
- **Per-tier+vendor budget caps + multi-key API support.** New
  `SD_TIER{N}_API_{VENDOR}_BUDGET=M20|W5|D2|T100` env format; new
  `GET /admin/budget/caps`; Token Budget popover gains a "Budget caps ·
  period spend" section. Env values can be comma-split or numbered
  (`_1`…`_32`); active rotation lands in v3.1.
- **Local OpenAI-compatible servers (Bundle E).** `kind=custom`
  auto-detects LM Studio / llama.cpp / vLLM / Ollama on the local
  network — sensitive-AI auto-disable lifted, sensitive-memory egress
  filter skipped, 120 s default timeout. `openai_local` env spelling
  accepted as alias.
- **Provider hot-swap (Bundle C).** `PUT /settings/providers/{tier}`
  now takes effect at the next classify / generate / embed call;
  no container restart required. SynapseBuilder / MemoryClassifier /
  BubbleGenerator / SensitiveClassifier all resolve their provider
  per-operation via a `func() LLMProvider` resolver.
- **Reasoning-model defensive (Bundle A).** `[think:off]` prepended to
  Tier 1 system prompts; `/recall` routes through `router.ForEmbedding()`
  instead of constructing a provider from env vars.
- **Transient-error retry layer (Bundle B).** `withRateLimitRetry`
  wraps every LLM call with exponential backoff (2/4/8/16/32 s) on
  rate-limit + 5xx-transient errors. Pipeline-wide retry budget via
  `nightly_retry_budget` setting (default 0 = unlimited).
- **Sensitive classifier overhaul.** 7-category taxonomy + 14 worked
  examples + explicit "default to NO" stance cuts the false-positive
  rate from ~80% (Tier 1) to ~7% (Tier 2). Tier 2 selection;
  120 s classify timeout; bulk reclassify rate limit + async
  background goroutine with WebSocket progress.
- **5-folder Privacy UI** with pagination, in-place row removal, and
  enriched-text row labels. Sensitive memories never auto-dormant.
- **Atlas Search** — global search bar in the header (`/` keybind)
  with `tag:` / `region:` / `from:` / `until:` prefix scoping;
  replaces the per-tab search inputs that had drifted out of sync.
- **Pagination across every infinite-list surface** (Maps, Hooks,
  Audit, Experiences, Privacy, Memory Browser) via a shared
  `sdPaginate` helper with per-surface persisted page size.
- **Diagnostic surfaces.** `POST /admin/recall-trace` returns the
  per-stage retrieval funnel state for any query; Config →
  Diagnostics · Recall Trace visualises it. Wave 7a Docker control
  plane (`/admin/docker/*`) backs the Rx prescription-card UI from
  Bundle H. `POST /admin/extract-facts` extracts declarative facts
  from a multi-turn session for one-click ingest.
- **Documentation refresh.** New `docs/WHAT_SYNAPTIC_IS.md`;
  `docs/COMPETITIVE_POSITION.md` removed; README, schemas, REMOTE.md,
  CLOUDFLARE_BRIDGE.md, MCP adapter tool descriptions, and the docs
  site all reframed around the agent-driven model.
- **Pi4-compatible local LLM runtime profiles (Bundle H).**
  Commented-out `llama-cpp` + `localai` compose profiles; opt-in via
  `--profile`. Validated on Pi4 8GB.
- **Hermes Agent passive-observer plugin (Bundle F).** New
  `bridge/hermes-plugin/` sibling; 7 Hermes events mapped to Synaptic
  events; not advertised in onboarding.

### Migration

- No SQLite schema migration **required** — bank schema is forward-
  compatible. New columns added by the v2.6/2.7 work
  (`expires_at`, `dormant_at`, `dormant_reason`, `merged_from`, etc.)
  populate themselves at first start.
- If you previously ran with `SD_SYNAPSE_MODEL` unset, the embed model
  flip to `nomic-embed-text:v1.5` will trigger a one-shot background
  backfill — recall keeps working through it. Coverage drops surface
  the "Backfill memories" CTA.
- If you had a pre-v2.7 bank with >100 region-less memories, the
  region reprocess CTA appears once on first dashboard load.
- New `data/patients/default/` directory structure — your existing bank
  is symlinked / wrapped as the `default` Patient. The previous flat
  `SD_BANK_PATH` keeps working.

---

## [v2.5.1b1] — 2026-05-12

**Theme:** the resilience patch. Deep-encoding backfill no longer ties its life to your browser tab, no longer stacks goroutines into an unhealthy container, no longer hangs on a misconfigured Tier 2, and no longer flashes the wrong numbers between WebSocket updates and section re-renders. Plus a few free wins from Ollama Tier 2 tuning.

### Fixed

- **Backfill survives browser disconnect.** `POST /admin/deep-encode-all` is now async: returns `202 Accepted` in ~20ms with `{ started: true, started_at, total_at_start, tier2, token_budget }`. The enrichment loop runs in a server-side goroutine using `context.Background()`, so closing the tab, refreshing, suspending the laptop, or losing network mid-flight no longer cancels the run. Live progress + completion still arrive via the existing `deep_encode_backfill_progress` / `_done` WebSocket events; the Phase 2 bar on the Dream Journal card paints from them. Previously the loop was tied to the request context — any browser hiccup aborted a 90-minute run after ~5 memories.
- **Single-flight guard on `/admin/deep-encode-all`.** A second concurrent kickoff returns `409 Conflict` with the in-flight start timestamp. Stops the goroutine accumulation that previously starved `/healthz` and pushed the container into the `unhealthy` state when a user clicked Backfill twice (or the browser auto-retried after a timeout).
- **Pre-flight reachability probe for Tier 2.** Before the loop starts, the handler hits the provider's lightweight health endpoint (`/api/tags` for Ollama, `/healthz` for AirLLM) with an 8-second deadline. A misconfigured Tier 2 (host not in compose, base_url typo, sidecar service missing) now `503`s in <8 seconds with a useful error instead of burning 10 minutes of held HTTP goroutine on five consecutive 120-second Chat() timeouts.
- **Tier 2 sidecar boot.** `handleAdminDeepEncodeAll` now calls `lifecycle.EnsureForWorkflow(ctx, "nightly_run", activeFilter)` with an active-tier filter (via `sidecarNameForTier2Provider`) to start the configured Tier 2 sidecar — Ollama or AirLLM — before the enrichment loop runs. Mirrors the `nightly_runner.go` pattern. Previously, the lifecycle's 5-min idle reaper would have stopped the sidecar by the time the user clicked Backfill, and the loop would have looped against the stopped service until `consecutiveFailureLimit` (5) tripped.
- **Deep-encoding card stops flashing stale numbers.** `renderDeepEncodingCard()` now prefers `this._lastLiveCoverage` (populated by `refreshLiveCoverage()` from `/memories/coverage`) over `stats.embedding_coverage` and `de.queue.by_reason` from the most recent nightly run. Phase 1 bar, Phase 2 bar, queue breakdown chips, "ever deep-encoded %", the embed-gap warning, and the Backfill button label all paint from live values on every re-render. Previously, WebSocket events would paint live progress (e.g. 99.9%) onto the bar, then any section re-render would re-inject the dream-run-frozen snapshot (e.g. 79.7%) on top, producing a flicker.
- **Embed-gap warning patches in place.** `refreshLiveCoverage()` now also rewrites the "⚠ N of M memories lack embeddings" text and the Backfill button label using the live counts, so a finished Phase 1 reflects in those strings immediately rather than waiting for the next section re-render.

### Changed

- **Tier 2 Ollama `OLLAMA_KEEP_ALIVE` bumped 10m → 1h.** Keeps the model resident in VRAM through a full backfill (~90 min for 2,800 memories on llama3.1:8b). The lifecycle manager's 5-min container-idle reaper is independent of this knob, so Tier 2 still sleeps when no workflow is using it.
- **Tier 2 Ollama: `OLLAMA_FLASH_ATTENTION=1`.** Enables Flash Attention 2 on supported GPUs (Ampere+ NVIDIA, RDNA3+ AMD) — ~15-30% faster inference. Silently degrades to the SDPA path on older GPUs.
- **Tier 2 Ollama: `OLLAMA_KV_CACHE_TYPE=q8_0`.** Quantizes the K/V attention cache to int8 — ~30% VRAM savings on the cache at <1% quality loss.
- **Tier 2 Ollama: `OLLAMA_NUM_PARALLEL=4`** exposed as the explicit default. Prep for the upcoming parallel-workers backfill refactor.
- **Backfill button: re-enables on kickoff.** Was previously disabled for the entire ~90-minute run because the fetch held the connection. Now released as soon as the 202 acknowledgement returns.

### Endpoint changes

- `POST /admin/deep-encode-all` — response shape changed:
  - Success: `202 Accepted` with `{ started, started_at, total_at_start, tier2, token_budget, message }` (was `200 OK` with synchronous `{ enriched, errors, tokens_total, duration_ms }`).
  - Concurrent kickoff: `409 Conflict` (new).
  - Tier 2 unreachable: `503 Service Unavailable` (new — was effectively a 10-min hang).
  - Tier 2 unconfigured: `424 Failed Dependency` (unchanged).

### Migration

- **No data migration.** SQLite schema unchanged. Drop in the new ZIP, `docker compose up -d --force-recreate core ollama-tier2`, and you're done.

---

## [v2.5.0b1] — 2026-05-12

**Theme:** the integration release. Maps gain a proposal lifecycle and a feedback loop that lets research deepen them; memories gain bi-temporal supersede semantics; the bank gains bulk import; recall gains a map-level surface; the customizer becomes a real tool. Skipped public 2.2 / 2.3 / 2.4 betas internally; this is the next public drop after 2.1.0b1.

### Added

**Theme system overhaul.**

- **ThemeForge** — new full-screen customizer inspired by Material Theme Builder + themer.dev, rendered in the clinical aesthetic. Six anchor seeds (`primary`, `secondary`, `error`, `surface`, `ink`, `bg`) derive 18 CSS vars via HSL tone math; the other 12 fall out automatically. Live preview tray shows 8 sample UI surfaces; WCAG contrast checker runs 10 load-bearing pairs per theme and tiers each (AAA / AA / AA-L / FAIL); JSON export for sharing. Each existing theme grew a pencil icon — click to open ThemeForge pre-loaded with that theme's seeds.
- **Theme families** — 13 separate theme entries collapsed to 7 family rows (Manila / Hex / Hex Pro / Blueprint / Clinical / Paper / Custom) with a global Dark/Light toggle. Selecting "Hex" then toggling LIGHT now means "Hex" stays selected and the variant flips to Light Hex. Same toggle inside ThemeForge swaps between sibling themes when editing.
- **Four new theme variants** to complete every family pair: **Light Manila** (warm parchment with cyan HUD + magenta pulse), **Whiteprint** (Blueprint's white-paper inverse with navy ink + cyan accent), **Clinical Dark** (deep-navy clinical with bright cyan HUD), **Mocha Paper** (roasted-bean coffee surface with cream HUD).
- **All themes re-baked** as `{seeds, variant, overrides}` storage. Every theme passes WCAG contrast checks (0 FAILs across 90 contrast pairs). The Layer 2 derivation engine now uses bg-lightness (not just variant flag) to decouple atlas-chrome rules from 3D-scene rules, so light themes can pair pale chrome with dark brain plinths without breaking either.
- **Cross-device theme sync** — `/settings/sd:themes:custom`, `/settings/sd:themes:user`, `/settings/sd:themes:pref`. Customs + the Dark/Light preference follow the user across devices.
- **Custom theme has both variants** — `custom` (dark) and `custom-light` (light) as independent editable slots. ThemeForge variant toggle auto-saves the current side before swapping.

**Memory system.**

- **R13 map-proposal pattern.** `POST /maps/propose` accepts `source: tag|query|recall_id|auto`; Tier 2 synthesizes a schema and the result lands as a `MemoryMap` with `status='proposed'`. New "Proposed" sub-tab in Maps with animated pulse when the queue is non-empty; inline Accept / Edit / Dismiss buttons; `POST /maps/{id}/accept|dismiss` flips status. Phase 8.7 (context memory) rewired to produce proposals instead of auto-committed rows, putting the user back in the loop. MCP tool: `sd_propose_map`.
- **R10 bi-temporal supersede.** New `memory_supersedes` edge table — append-only, raw text never mutated. Endpoints: `GET/POST /bank/memories/{id}/supersede`, `DELETE /bank/memories/{id}/supersede/{other}`. `/recall` accepts `include_superseded` + `as_of` for time-travel reads; default current-truth view excludes superseded ids. Trace detail UI surfaces the supersede chain with "Replaces" / "Replaced by" sections + a "Mark as superseding…" affordance.
- **`POST /maps/{id}/research`** — map-scoped Tier 3 research that closes the loop. Resolves the map's anchor tags, runs Oracle research with the map's memories as context, caches the result with `map_id` set, AND ingests a new memory tagged with the map's anchors + `source=manual_augment`. The augment memory's `dirty_reason=created` flag means Phase 0b deep-encodes it on the next nightly run; Phase 7/8/8.7 cluster it with the map's other traces. Research now actually builds the knowledgebase up.
- **"Seed with research" on propose** — checkbox in the propose-map dialog auto-fires `/maps/{id}/research` after a successful propose, seeding a brand-new map with a foundational Tier 3 synthesis. Useful for cold-start topics with no existing memories.
- **`POST /maps/cleanup`** — bulk-prune maps matching `{delete_empty, delete_dismissed, only_type, only_generated_by, dry_run}`. Dry-run preview reports counts + sample names + breakdown by type/source before commit. Audited as `memory_maps_cleanup`. Cascades through `map_traces` + `map_associations`; deletes orphan `map_overrides` rows too (the bug behind ghost concept maps).
- **`POST /recall/maps`** — map-level recall. Hybrid scoring: name match (clean name match wins outright — `Can you recall the project Synaptic-Disorder` returns that map at score 1.00), schema-text cosine, member aggregate. Modes: `hybrid` (default), `name`, `schema`, `members`. Includes synthesized tag-cluster maps with ≥3 members, not just persisted memory_maps rows. MCP tool: `sd_recall_maps`.
- **Bulk memory import** — `POST /import` with five format adapters: `synaptic` (passthrough), `hindsight`, `mem0`, `letta`, `generic`. 8 MiB payload cap. Options: `tag_prefix`, `adapter_id`, `preserve_ids`, `mark_sensitive`. UI lives in Config → Data with format dropdown, file picker, sdConfirm preview, 429 graceful handling. Rate-limited via the same chokepoint as `/event` + `/bank/memory`.
- **`POST /reflect`** — Tier 3 synthesis over a recall result set, with `tags_match` + `budget` + `include_sensitive` opt-in. Dashboard surface + MCP tool `sd_reflect`. Sensitive memories filtered by default; `include_sensitive=true` opts in per-call with the count surfaced in the response.

**Privacy & rate limiting.**

- **Memory write rate limit** — token-bucket middleware on `POST /event` + `POST /bank/memory` + `POST /import`. Default `0` (unlimited). Returns `429 Too Many Requests` with `Retry-After` header when over-cap. Configurable via `/settings/memory_write_rate_limit_per_min` from Config → Pipeline; takes effect within ~2s (cache TTL). MCP clients see graceful backoff hints.
- **Settable sensitive classifier prompt** — Config → Privacy → "Classifier prompt (Layer 2 AI)" textarea. Persists at `/settings/sensitive_classifier_prompt`. The classifier reads it with a 10s cache; `PUT` / `DELETE` on the setting invalidates the cache instantly, so changes take effect within ~1s (same hot-swap pattern as `PUT /settings/providers/{tier}`). No restart. Validation rejects prompts without a `%s` placeholder.
- **Bulk re-classify** — `POST /admin/sensitive/reclassify` with `{max, dry_run}`. Re-runs the (possibly user-tuned) classifier over currently-flagged memories and clears flags on memories the updated classifier no longer thinks are sensitive. Never raises flags — only clears false positives. Audited as `auto_unflag_sensitive` per memory + a summary `bulk_reclassify_sensitive` row. UI lives directly under the prompt editor.
- **Tightened classifier prompt default** — the prior prompt mis-flagged "I implemented OAuth" / "Bearer token flow" / "JWT validation" discussion as sensitive because it listed "access tokens" + "credentials" as triggers under a "When uncertain, answer YES" rule. New default explicitly draws the line: discussion of an auth technology / protocol is NOT sensitive — only literal secret VALUES are. Placeholder strings like `<your-token-here>` are explicitly NO.
- **Reflect handler privacy gate** — `include_sensitive` opt-in; response surfaces `included_sensitive_count` whenever sensitives were included.

**Visualization.**

- **Intrusive Thoughts** — memories with `region='unknown'` or non-canonical region strings now render in an orbital cloud around the brain (Fibonacci-sphere at r≈1.10, outside the brain mesh's HALF=0.92 bound so they don't warp the hull). Their synapses render with `INTRUSIVE_DIM=0.12` alpha — present but ghost-faint, signaling "not structurally integrated." Region drilldown explains the metaphor: "unexpected, fleeting thoughts that haven't found a home yet."
- **Region canon expanded 14 → 38.** Added unlateralized base regions (`frontal_lobe`, `temporal_lobe`, `prefrontal_cortex`, `parietal_lobe`, `hippocampus`, `amygdala`) and real sub-regions previously emitted by the classifier but not enumerated (`occipital_lobe`, `dorsolateral_prefrontal_cortex`, `inferior_frontal_gyrus`, `lateral_frontal_cortex`, `anterior_cingulate`, `premotor_cortex`, `inferior_parietal_lobe`, `hypothalamus`, `somatosensory_cortex`, `cerebral_cortex`, `limbic_system`, `default_mode_network`). Each has a proper `realFn` / `dashFn` for the drilldown.
- **Region alias table** — ~30 variant spellings (hyphen↔underscore, capitalization, common misnamings) resolve to canonical entries. `pre-frontal_cortex` → `prefrontal_cortex`, `default_mode_region` → `default_mode_network`, `visual Cortex` → `visual_cortex`, etc. `resolveRegionKey()` normalizes at boot; the original lands on `m._regionRaw` for debugging.
- **Region distribution counter fixed.** Was showing absurd values like `31/20 active regions` because numerator counted raw bank region strings (70 distinct after classifier drift) while denominator counted canonical slots. Now both come from the same canonical set; the typical reading is `21/38`.
- **Brain SVG swapped.** Old single-path stick-figure brain replaced with SVGRepo's "brain-illustration-4" via a shared `<symbol id="sd-brain-icon">` defined once at body open. Every brain icon button across the app references it via `<use href>`.

**Maps UI.**

- **Proposed Maps sub-tab** with PROPOSED pill, schema preview (220 char), source label, related-tag chips, inline Accept / Edit / Dismiss. Animated pulse on the sub-tab when the proposal queue is non-empty.
- **"+ Propose map" button** on every Maps sub-tab opens a themed dialog: source selector (tag/query/recall_id), type, limit, name override, `include_sensitive` opt-in, optional "seed with Tier 3 research" checkbox.
- **"Research…" button** on map detail header opens a Tier 3 research dialog pre-filled with the map's name + anchor tags + member excerpts. Editable up to 1000 chars. Result ingested as a new memory tagged with the map's anchors.
- **"Cleanup…" button** with dry-run preview showing count + sample names + breakdown before committing the delete.
- **Unified header pill geometry** — Back / Focus / Research / Merge / Edit on map detail all share `atlas-map-header-btn` styling. The Focus button's old `brain-icon-btn` standalone shape was visually mismatched; now lines up at 20px height across the cluster.
- **Map merge bug fixed.** Previously after merging Project A into Project B, A's source map would reappear in Concepts after the re-render. Root cause: backend rewrote tags in SQLite but the dashboard's local `_items` cache held the pre-merge state; the re-render then re-synthesized A from stale data. Fix: new `_localApplyTagMerge` mirrors the backend's tag rewrite locally before re-render.
- **`/memories` is now bank-backed.** Was serving a static JSON snapshot from `<dataDir>/assets/memories.json` (a frozen Hindsight import from 2026-05-09). Backend mutations (tag merge, supersede, bulk import) were invisible after CTRL+F5 because the snapshot never updated. Now serves live bank state when the bank is enabled; falls back to the static file only when bank is disabled.
- **Orphan map_overrides cleanup.** Three orphans (`map-1778395048525070281` + 2 siblings) pointing at long-deleted memory_maps rows were rendering as ghost concept maps in the Concepts tab. Both DeleteMemoryMap and CleanupMemoryMaps now sweep the override row whenever they delete a map. Frontend `_buildConceptMaps` also filters out forced overrides keyed by `^map-\d+$` defensively.

### Changed

- Toast pipeline unified. Every toast in the dashboard now routes through one module-level `showSdToast(msg, ms)` helper, body-attached and top-center, immune to ancestor transforms. The prior `_showAtlasToast` reused the `.atlas-onboard-hint` class which carries `position: fixed` — that hijacked stacked toasts out of their flex column and pinned them to stale viewport coordinates, clipping them off-screen. New `.atlas-toast` class is positioning-free; the stack's flex layout handles placement.
- `ThemeForge` is single-instance + desktop-only. Opening one closes any existing instance; opening from a mobile/coarse-pointer device shows a toast ("Sorry, ThemeForge is best used on Desktop.") rather than rendering the three-column layout in a phone-width viewport.
- `applyMemoryLayout` extended: any memory whose region isn't in the canonical anatomy map (`REGION_ANATOMICAL_CENTERS` ∪ `anatomy.regions` ∪ `REGION_INFO` minus `unknown`) goes into the orbital cloud, not just literal `'unknown'`. So legacy `dorsolateral_prelude` / `occ-quirks` etc. are visualized correctly.
- Auth allowlist updated: `/import` and `/reflect` were missing from the `authPathExempt` API prefix list, leaving them auth-EXEMPT (anyone could call without a token). Now properly auth-gated.
- `nightly_run` lifecycle ensures only the **active** Tier 2 sidecar, not all candidates. `EnsureForWorkflow` now takes an `activeFilter` parameter; nightly_runner.go computes it from `router.Tier2()`. Was a bug where both Ollama Tier 2 and AirLLM Tier 2 would boot during a nightly run regardless of which provider was actually configured.
- `CITATIONS.md` re-graded. Every R-series mapping now carries an explicit **Status grade** (F / P / C). Six items audited: R3 surprise (P→F-leaning with Lisman & Grace 2005 + Greve et al. 2019 corroborators), R5 schema reframing (P, interpretive), R8 schema-from-replay (P with Diekelmann & Born 2010 cross-reference), R12 adaptive weighting (C, loose inspiration), R13 context memory (P, rewritten to map-proposal pattern), R14 strength rebalance (P with SHY corroborator). Goal-alignment closing softened from "every architectural choice in empirical neuroscience" to honest "biologically *informed*, not strictly faithful."
- Map merge tag-rewrite now mirrors locally; backend cache invalidation via `_invalidateBackendMaps()` on the frontend side.

### Fixed

- Map merge: source map reverted to Concepts after merge (frontend cache + override interaction).
- Region count: `31/20 active regions` → `21/38` (canonicalization).
- Sensitive over-classification on OAuth-as-a-concept (prompt rewrite).
- `/memories` serving stale snapshot after backend mutations.
- Ghost concept maps from orphan `map_overrides` rows pointing at deleted maps.
- Theme picker pencil clicks triggering theme-apply (event-delegation guard).
- Neuron idle invisible on dark themes (derivation flipped sign — was darkening primary on dark scenes instead of lightening).
- Toast clipping off-screen via stale class reuse.
- Bank schema migration ordering for `memory_maps.status` (now created after `migrateMemoryMapsColumns` adds the column).

### Endpoint additions

Auth-gated under existing top-level path families (no Cloudflared regex change needed):

| Path | Purpose |
|---|---|
| `POST /import` | Bulk memory import (5 formats) |
| `POST /maps/propose` | R13 map proposal |
| `POST /maps/cleanup` | Bulk map cleanup with dry-run |
| `POST /maps/{id}/accept` | Accept a proposed map |
| `POST /maps/{id}/dismiss` | Dismiss a proposed map |
| `POST /maps/{id}/research` | Map-scoped Tier 3 research + memory ingest |
| `POST /recall/maps` | Hybrid map-level recall |
| `POST /reflect` | Tier 3 synthesis over recall results |
| `GET/POST/DELETE /bank/memories/{id}/supersede[/{other}]` | R10 supersede chain |
| `POST /admin/sensitive/reclassify` | Bulk re-classify under updated prompt |
| `GET /admin/sensitive/default_prompt` | Default prompt for UI reset |
| Settings keys: `sd:themes:custom`, `sd:themes:user`, `sd:themes:pref`, `sensitive_classifier_prompt`, `memory_write_rate_limit_per_min` | New settings table entries |

### Migration

- No DB migration required for fresh installs. Existing installs get the `memory_supersedes` table + `memory_maps.status` / `generated_by` / `generation_phase` columns auto-added at first boot under v2.5.0b1.
- Existing custom themes: prior `sd:theme:custom:colors` localStorage blob is treated as overrides on top of the (newly seeded) Dark Manila base for back-compat. Re-save once via ThemeForge to migrate fully.
- Existing sensitive-flagged memories from the old over-aggressive classifier prompt: open Config → Privacy → "Bulk re-classify" to sweep the bank under the new prompt and clear false positives. Sticky-on policy still applies — the bulk operation can only CLEAR flags, never raise them.

### Notes

- v2.2 / v2.3 / v2.4 were internal-only betas. v2.5.0b1 is the next public release after v2.1.0b1.
- Build artifact: `synaptic-2.5.0b1.zip` (~9.0 MB).
- The classifier prompt is intentionally domain-tunable per-install. If your bank's threat model differs from the default, edit it from Config → Privacy.

---

## [v2.1.0b1] — 2026-05-11

**Theme:** full "Disorder" → "" identifier rebrand (breaking).

The product had been renamed in prose since v2.0.0a, but a long tail of
identifiers — install dirs, volume names, plugin slugs, MCP slugs,
package names — still carried the legacy stem. v2.1.0b1 finishes the
work. This is a **breaking release** for existing beta installs;
migration paths are documented inline. Fresh installs see only the new
names.

### Renamed

| Surface              | Before                                              | After                                  |
| -------------------- | --------------------------------------------------- | -------------------------------------- |
| Windows install dir  | `%LOCALAPPDATA%\SynapticDisorder`                   | `%LOCALAPPDATA%\Synaptic`              |
| macOS install dir    | `~/Library/Application Support/SynapticDisorder`    | `~/Library/Application Support/Synaptic` |
| Linux install dir    | `~/.local/share/synaptic-disorder`                  | `~/.local/share/synaptic`              |
| Bank volume          | `synaptic-disorder-bank`                            | `synaptic-bank`                        |
| Image tag            | `synaptic-disorder/core:latest`                     | `synaptic/core:latest`                 |
| Marketplace          | `synaptic-disorder`                                 | `synaptic`                             |
| Plugin slug          | `synaptic-disorder-claude-code@synaptic-disorder`   | `synaptic-claude-code@synaptic`        |
| MCP server slug      | `synaptic-disorder`                                 | `synaptic`                             |
| MCP tool prefix      | `mcp__synaptic-disorder__sd_*`                      | `mcp__synaptic__sd_*`                  |
| Python pip pkg       | `synaptic-disorder` / `import synaptic_disorder`    | `synaptic` / `import synaptic`         |
| npm pkg              | `@synaptic-disorder/sdk`                            | `@synaptic/sdk`                        |
| MCP adapter pkg      | `synaptic-disorder-mcp`                             | `synaptic-mcp`                         |
| OTel adapter pkg     | `synaptic-disorder-otel`                            | `synaptic-otel`                        |
| OTLP receiver pkg    | `synaptic-disorder-otlp-receiver`                   | `synaptic-otlp-receiver`               |

47 files swept; ~65 prose replacements of "Synaptic Disorder" →
"Synaptic". Historical-quoted references (README "previously named …"),
the academic citation file (`docs/research/CITATIONS.md`), and dev-only
handoffs under `docs/dev/` left intact.

### Preserved (wire-level identifiers, intentional back-compat)

- Env vars: `SD_API_TOKEN`, `SD_CORE_URL`, `SD_CORE_HOST`, `SD_CORE_PORT`,
  `SD_MODEL`, `SD_CLIENT`, `SD_ADAPTER_ID`, `SD_OLLAMA_TIER*_MODEL`,
  `SD_AIRLLM_TIER2_MODEL`, …
- localStorage prefixes `sd_*` and `cfg2-*`.
- Container names `synaptic-core`, `synaptic-ollama-tier1`,
  `synaptic-ollama-tier2`, `synaptic-airllm-tier2` (already on the short
  stem since wave 8).
- Component name **SD Core** (canonical).
- Source-tree variable names (`__sdToggleMobilePanel`, `_sdHydrateSettings`,
  `SD_HUD_FADE_SEL`, …).
- Console log prefix `[SD]`.

### Compatibility shim

The plugin's `forward.js` regex matches **both** `mcp__synaptic-disorder__sd_*`
and `mcp__synaptic__sd_*` (and the same for `report_memory_save`), so
pre-2.1.0b1 plugin installs keep emitting events until users re-install
the renamed plugin.

### Migration

`docs/dev/Legacy migration paths.md` §§ 1–2 cover the AppData-dir move
and the bank-volume rename. Same `docker run --rm cp` recipe shape works
for both legacy generations of the volume.

### Notes

- No backend logic changed. SD Core binary, dashboard HTML, and the
  wave 8 work shipped in 2.0.x are byte-identical apart from the
  `WWW-Authenticate` realm string and the classifier's new `synaptic`
  keyword (added alongside the preserved `synaptic-disorder` keyword).
- Build artifact: `synaptic-2.1.0b1.zip` (8.5 MB).

---

## [v2.0.3b1] — 2026-05-11

**Theme:** fresh-install simulation pass.

Companion patch on top of v2.0.2b1, cut after dry-running the
"fresh user unzips the ZIP and follows the README" flow. The 2.0.2b1
ZIP shipped the volume fix but several install-time helpers and docs
still pointed users at the wrong place. None of this affected existing
installs that were already up.

### Fixed

- **Installer (`tools/install_to_appdata.py`):**
  - Default env file now written as **`.env`** (dotted), not `env` (no
    dot) — compose auto-reads `.env` for `${VAR:-}` substitution and
    silently ignored the legacy name, so every installer-generated knob
    (`SD_API_TOKEN`, `SD_OLLAMA_TIER1_MODEL`, `HUGGING_FACE_HUB_TOKEN`,
    …) was previously inert.
  - Default env template now references `SD_OLLAMA_TIER1_MODEL` /
    `SD_OLLAMA_TIER2_MODEL` (was the wave-7c1-deprecated `SD_OLLAMA_MODEL`).
  - `--uninstall --purge` now lists the correct compose-managed volume
    names (`synaptic_synaptic-disorder-bank`, `synaptic_ollama-tier1-data`,
    `synaptic_ollama-tier2-data`, `synaptic_airllm-tier2-cache`). Was
    silently skipping every legacy name and leaving multi-GB behind.

- **One-line starters** (`tools/install/start.sh` + `start.ps1`):
  - Default port corrected `8765` → `9911`. SD Core serves the dashboard
    on 9911; 8765 is the legacy static-only `python serve.py` fallback.
    The polling loop had been timing out after 30 s on a healthy stack
    and auto-opening a dead URL.

- **Docs:** three stale port refs in `Agent-Install.md`, the
  README's Option A service listing, and the Python SDK example's
  docstring all now correctly distinguish Docker / Native (9911) from
  static-only Option B fallback (8765).

### Notes

- No backend code changed in this drop.
- Build artifact: `synaptic-2.0.3b1.zip`.

---

## [v2.0.2b1] — 2026-05-11

**Theme:** fresh-install volume bug + Docker / AirLLM UX hardening.

Same-day patch release. Headline is the **fresh-install fix**; the rest
is a substantial round of Docker control-plane and AirLLM deployment-UX
work that landed across wave 8.

### Fixed (release-blocker)

- **`synaptic-disorder-bank` volume `external: true` removed.** Leftover
  from the prior `synapticdisorder` → `synaptic` project rename. New
  users hit `external volume "synaptic-disorder-bank" not found` before
  any service started; existing users were unaffected because their
  volume already existed. Flipped back to compose-managed so compose
  auto-creates on first `up`. Legacy data → new volume migration
  documented in `docs/dev/Legacy migration paths.md`.

### Fixed

- **Docker control plane — recurring dashboard 404.** Helper-container-
  driven recreates (`POST /admin/docker/services/core/recreate`, Profile
  Enable) on Docker Desktop / Windows had been silently breaking the
  dashboard: the helper's `./` resolved to its own `/workspace` mount
  instead of the host's project dir, leaving the new container with a
  broken bind. Fixed by passing `--project-directory <daemon-translated-host-path>`
  (read from SD Core's own Mounts metadata; Docker Desktop translates
  `C:\Users\...` → `/run/desktop/mnt/host/c/Users/...`). Plus a
  defensive boot self-check that loudly warns if `/data/index.html` is
  missing.

- **Dream Journal entry truncation.** `generateDreamEntry` prompt now
  requests "2–4 short paragraphs, total ~300–350 words. End with a
  complete sentence — never trail off mid-thought." `max_tokens` 600 →
  800, post-trim safety guillotine made sentence-aware (walks back to
  last `.`/`!`/`?` at 3000 chars).

- **Lexicon Pairs panel** rendered blank because frontend read
  `p.a` / `p.b` / `p.count` but backend canonical (since wave 6)
  returns `tag_a` / `tag_b` / `cooccurrence`. Now reads both shapes.

- **Maps → Concepts trace count.** Backend `/maps` returned
  `trace_count: 0` for some concept maps. Merge logic now adopts the
  synthesized count when synth > backend and pre-populates the `traces`
  array so detail-view opens instantly without a `/maps/{id}/traces`
  round-trip.

- **Bubbles toggle** on the Brain page was unclickable — rendered at
  `position: fixed; top-right` under the Atlas chip.

### Added — AirLLM end-to-end deployment UX

- **HuggingFace token input** on the AirLLM profile card. Validates
  against HF `whoami-v2` before persisting; auto-recreates the
  container after save; status pill shows `✓ saved in .env` via
  `/admin/env_check`.
- **NVIDIA GPU passthrough** added to the AirLLM compose stanza
  (`deploy.resources.reservations.devices`). Docker Desktop +
  nvidia-container-toolkit honor it automatically when the host has an
  NVIDIA driver.
- **GPU preflight check** at `/admin/airllm/diagnostics` queries
  `daemon /info → Runtimes` to detect NVIDIA presence. `configure_tier`
  returns `400 {error: "cuda_required"}` with an actionable hint when
  no NVIDIA is detected — preventing AMD / Apple / CPU-only users
  from wasting 100 GB of model download.
- **Profile-card disk + GPU rows** surface storage requirements
  (~140 GB for 70B at FP16 download size) and GPU requirements
  prominently so users decide before downloading.

### Added — Tiered Ollama + lifecycle

- **Per-tier Ollama containers consolidated.** Dedicated `ollama-tier3`
  and `airllm-tier3` retired (wave 8c) — a Tier 2 model serves both
  `nightly_run` and `research_request` workflows. Tier 3 provider
  config now either points at `ollama-tier2`/`airllm-tier2` for local
  use OR at a remote API for live-internet research. Profile name
  standardized `tier2-ollama` → `ollama-tier2`.
- **Sidecar lifecycle: `SignalProviderUse` + LLM-call timeouts.** Long
  inference calls on AirLLM (1–3 tok/s on consumer GPUs) were getting
  killed by hardcoded 90–180 s `context.WithTimeout` at 11 LLM-call
  sites + the idle reaper assuming the sidecar was idle while it was
  actively processing. Fixed: new `llm_call_timeout_sec` setting
  (default 120 — same as before for Ollama) with provider-aware
  auto-bump (AirLLM → 1800 s / 30 min); `SignalProviderUse(tier)` fires
  immediately before every Chat call, bumping `last_used_at` on the
  sidecar's lifecycle record so the idle reaper waits.

### Added — Benchmarking + nightly duration estimator

- `POST /admin/llm/benchmark/{tier}` — runs a small benchmark Chat
  call against the configured provider, measures tokens-per-second,
  persists to `provider_benchmark` table.
- `GET /admin/llm/benchmark` — most recent benchmark per (provider,
  model, compression).
- `GET /admin/llm/estimate_nightly` — combines recent token totals
  from past runs with the benchmark rate, returns "estimated total
  seconds" + per-phase breakdown.
- **Auto-benchmark on `configure_tier`** queues a background benchmark
  for the new (provider, model) combo so estimates are ready by the
  time the user navigates to the Dream Journal card.

### Added — Service-card UX

- **Tier 2 auto-register on Profile Enable.** Enabling `ollama-tier2`
  or `airllm-tier2` profiles auto-PUTs the Tier 2 provider config to
  point at that container. Confirm dialog flags any overwrite.
- **"Set Active · Tier X" buttons** on each model-serving service card.
  Lets users swap freely between AirLLM ↔ Ollama Tier 2 without
  disabling/re-enabling profiles. State-aware: green
  `✓ Active · Tier X` badge when current config already matches.
- **Profile cards** grew rich metadata: tier role, default model, GPU
  requirement, RAM estimate + hardware note, disk requirement +
  download note, lifecycle behavior, colored hint border. Sorted
  active-first.
- **Disk usage** collapsible below the cards reads `/admin/docker/disk`,
  shows volume sizes + image layer total.

### Brain rendering rewrites

A multi-pass fix for the recurring "synapse animations cut each other
off" complaint:

- **`FireAnimationQueue`** — concurrent cap (6) + intensity-scaled
  duration. Hooks enqueue instead of calling `activate()` directly;
  pending hooks release with 150 ms stagger so bursts play as a wave.
  Duration = `1200 ms × intensity` (clamped 600–2400).
- **Invisible pulse rendering with additive tube halo.** Pulses are
  now conceptual traveling light sources, not visible sprites. The
  synapse tube glows in a Gaussian halo around the pulse's current
  position via max-blend writes into vertex colors — two pulses on
  the same wire show as two glowing spots passing each other instead
  of one replacing another.
- **Neuron glow pool 3 → 8 lights with smoothed transitions.** Root
  cause of "region B firing visually killed region A": the 3-light
  pool teleported to brighter neurons whenever a new region fired.
  8 lights cover the queue's MAX_ACTIVE=6 with headroom; position
  transitions ease at 0.30/frame instead of snapping.
- **`MAX_PULSES` 15 → 320.** The underlying pulse-list cap was a
  revolving door at 15 while each fire pushed 32 — every new fire was
  evicting all prior pulses. Sized to MAX_ACTIVE × PULSE_CAP_PER_FIRE
  × 1.6.
- **`window._sdSimulateBurst(n, intervalMs, region?)`** debug helper
  exposed for verifying brain rendering from devtools.

### Added — Dream Journal stat drill-down

Each stat in the Dream Journal card (Encoded, Consolidated, Maps
Updated, Merged, Pruned, Lexicon Pairs, Cross-region, Schemas,
Reinforced) is now a clickable button that toggles a focused breakdown
panel. Trace click-throughs where backend ships per-item arrays;
context-only panels (queue-by-reason, added/removed deltas) where it
ships counts only.

### Misc

- `section_dirty` event type (wave 7c2) — backend WS hub now emits a
  single `{sections: [...], dirty_at: "…"}` event when list data
  changes; frontend conditionally refreshes the matching atlas tab.
  Stops the dashboard re-rendering every list on every WS event.
- `GET /session/dirty` (per-section last-dirty-at map),
  `POST /session/clear/{section}` ack, cursor pagination on `/audit`
  + `/nightly/runs` behind `SD_LAZY_FETCH_ENABLED`.
- `HUGGING_FACE_HUB_TOKEN` allowed in the `env_write` allowlist.

### Known issues / deferred

- **Host-free-bytes** on `/admin/docker/disk` (precise "you have N GB
  free" comparison on profile cards) — backend brief sent but not yet
  built. Profile cards show daemon usage as a proxy.
- **Window-bounded nightly run** (graceful Phase 0b truncation when the
  configured time window elapses) — briefed, deferred.
- **AirLLM `kernels>=0.11.1` pin** is in `requirements.txt` but the
  deployed image hasn't been rebuilt with it — `bitsandbytes` falls
  back to a Python path until the next rebuild. (Moot for NVIDIA GPU
  users — that path doesn't use these kernels.)

---

## [v2.0.1b1] — 2026-05-11 (early)

**Theme:** R1–R15 backend refinement chain.

A small patch release rolling up the work between the original Beta 1
cut and 2026-05-11.

### Added — R1–R15 refinements (all 15 shipped)

Every item from v2.0.0b1's "Known issues / deferred" landed:

- **R1** runtime salience scoring
- **R2** weighted SHY decay
- **R3** expanded dirty-flag triggers (`cascading`, `periodic`,
  `temporal_neighbor`, `surprise`, `emotional`) on top of the previously
  shipped `created` / `edited` / `model_upgrade`
- **R4** generative replay
- **R5** Phase 8.5 schema reframing
- **R6** Phase 7 low-similarity bridges
- **R7** `consolidation_stage` column
- **R8** schema-from-replay
- **R9** weak-trace boost
- **R11** TMR (targeted memory reactivation) affordance endpoint
- **R12** adaptive phase weighting (activated)
- **R13** Phase 8.7 context memory (feature flag, default OFF —
  Johnson 2005 is hypothesis-grade)
- **R14** global `recall_strength` rebalancing
- **R15** augmentation fallback

22 new tests landed alongside the refinement chain.

### Added — AirLLM at Tier 2 / Tier 3

The dream-pipeline can now route Tier 2 (deep encoding) and Tier 3
(Oracle research) through AirLLM as an alternative to Ollama / cloud
providers. Sidecar `/healthz` chip in the provider config UI shows
live status; Tier 1 (light encoding) stays on `nomic-embed-text`.

### Added — Crash recovery

Nightly runs now write a heartbeat row; on next start, SD Core
reconciles any run that was mid-flight when the container crashed,
marks the abandoned run `interrupted`, and resumes from the recorded
cursor on the next nightly tick. No more half-finished runs blocking
subsequent ones.

### Changed — Config IA reorganization

Config sub-tabs went from 5 to 10 (Models / Pipeline / Bridge / Brain /
Display / Bubbles / Identity / Privacy / Data / Diagnostics), with a
hamburger overflow control so narrow viewports still surface all
sections. The "deep-dirty" trace lifecycle badge renamed to
**"undreamt"**, and the contradictory `enriched` badge was dropped.

### Added — Hook log feature

A new dashboard pane streams the Claude Code plugin's hook-forwarder
output in real time, making it possible to debug "why didn't my event
fire?" without `tail -f`.

### Fixed — Sanitization (release build)

Two leaks the v2.0.0b1 build didn't catch are now scrubbed before
Prod:

- A hardcoded `SD_API_TOKEN` literal in `bridge/gemini-cli-hooks/forward.js`
  replaced with `process.env.SD_API_TOKEN || ''` during sanitization
  (and the literal removed from Dev source).
- Author's tunnel hostname and local path baked into
  `bridge/core/handlers_admin_network*.go` and
  `bridge/core/compose_runner_test.go` test fixtures now sanitized to
  `tunnel.example.com` / `C:\Users\example\…` placeholders.
- `docs/AGENT_INSTALL_PLUGIN.md` added to the sanitize allowlist so
  future path references get scrubbed automatically.

### Added — Install / quickstart guide

A new top-level `INSTALL.md` gives end users a single condensed install
path (Docker primary, native Go secondary, mock-only tertiary) without
the 33 KB of architecture rationale that lives in `README.md`. Full
README stays as the deeper reference.

### Known issues / deferred

- R13 Phase 8.7 context memory ships behind a feature flag (default
  OFF) — Johnson 2005 is hypothesis-grade.
- Trace lifecycle `failed` badge still deferred — needs a per-memory
  deep-encode log endpoint.

---

## [v2.0.0b1] — 2026-05-10

**Theme:** Phase 0b deep enrichment + product rebrand (prose-only).

Closes out the dream-pipeline architecture with **Phase 0b deep
enrichment**, ships full-fidelity plugin tooling for orchestrating the
pipeline from any MCP client, and rebrands the product from
"Synaptic Disorder" → **Synaptic** (short form **Syn**) in prose only.

### Added — Phase 0b (REM-style deep enrichment)

Stage 1 is now two phases instead of one:

- **Phase 0a** (existing) — Tier 1 light encoding. Embed via
  `nomic-embed-text`, classify region via `llama3.2:3b`, flip
  `light_encoded`.
- **Phase 0b** (new) — Tier 2 deep enrichment. Re-summarize, retag,
  refine region. One memory at a time. Resumable across nightly runs
  via cursor.

Stage 2 phases (1 dedup, 2 synthesis, 7 cross-region, 8 schemas) skip
un-deep-encoded memories. Phases 3 (lexicon), 4 (maps), 5 (decay), 6
(augment), 9 (reinforcement), 11 (narrative), 12 (dream entry) run
unconditionally.

### Added — Dirty-flag queue

Memories carry `marked_dirty_at` and `dirty_reason`. Trigger reasons
shipped in this release:

- `created` — `SaveMemory` dirties the new memory.
- `edited` — `UpdateMemory` dirties when text or tags change.
  Sensitive-flag flips do not dirty.
- `model_upgrade` — Tier 2 model setting change bulk-marks every
  deep-encoded memory.

Planned: `cascading`, `periodic`, `temporal_neighbor`, `surprise`,
`emotional` (shipped in v2.0.1b1).

### Added — Stats v3 schema

Run rows have `deep_encoded_this_run`, `deep_encoded_remaining`,
`deep_encoded_cursor`. Aggregate `stats.deep_encoding` block:
`enriched_this_run`, `remaining`, `coverage_percent`, `queue.{total_dirty,
by_reason}`, `estimated_runs_to_completion`, `tokens_in/out`.
`schema_version: 3` set when Phase 0b runs.

### Added — Backfill + settings

- `POST /admin/deep-encode-all` synchronous endpoint (~70 min for 3000
  memories at concurrency 1). Returns 424 when Tier 2 is unconfigured.
- New `/settings/dream_pipeline` keys: `deep_enrich_enabled` (default
  true), `deep_enrich_max_per_run` (100),
  `deep_enrich_token_budget_per_run` (50000), `deep_enrich_concurrency`
  (1).

### Added — Frontend wiring for Phase 0b

- Settings card in Config → Connection → Nightly with master toggle and
  advanced knobs.
- Dream Journal progress card showing coverage%, queue breakdown by
  reason, ETA, embedding-coverage warning.
- Trace detail badges: `deep-enriched` (green), `deep-dirty` (yellow).
- Trace editor inline hint "Saving will queue this memory for
  re-enrichment" when text or tags differ from original.
- Tier 2 model-change confirmation dialog with affected-count from
  latest run's stats.
- Phase 0b master toggle + Phase 10 Replay toggle UI-default ON.

### Added — MCP server v0.4.0 (Claude Code plugin)

The bundled MCP server gained 7 new tools (17 total):

- `sd_dream_run` — manual nightly kick (`POST /nightly/run`)
- `sd_dream_status` — query recent runs
- `sd_deep_encode_all` — Phase 0b backfill
- `sd_embed_all` — Tier 1 embedding backfill
- `sd_lexicon_rebuild` — rebuild lexicon pairs
- `sd_get_dream_settings`, `sd_set_dream_settings` — read/write the
  19 dream-pipeline settings keys

The hook forwarder (`forward.js`) translates each new tool name into a
typed dashboard pulse (`dream_run_started`, `deep_encode_started`, …).

### Added — Citations system

The 25-paper APA-7 bibliography (`docs/research/CITATIONS.md`) is now
strictly user-facing: no implementation status, no `[Rn]` cross-references,
no rotting line numbers. Engineering tracking lives in
`docs/dev/Pinned.md`. Citation process / supersession rules in
`docs/dev/CITATION_PROTOCOL.md`. License and academic credit
consolidated in `docs/CREDITS.md`.

### Added — Docs

- New `docs/AGENT_QUICKSTART.md` — one-page-per-agent recipe book
  (Claude Code, Claude Desktop, Cursor, Cline, Continue, Gemini CLI,
  Antigravity, OpenCode, Goose, Aider/OTel, custom SDK).
- `docs/AGENT_INSTALL_PLUGIN.md` updated for v0.4.0 plugin and 17 MCP
  tools.
- All `docs/*.md` and `docs/site/index.html` rebranded to "Synaptic"
  (prose only).

### Changed — Lexicon Pairs endpoint sync

Frontend resolves `/lexicon/pairs` then falls back to `/lexicon` when
running against an older SD Core. Backend handoff queued for the
canonical implementation.

### Backward compatibility

Everything that lands on disk or crosses a wire kept its name:

- Env vars: `SD_API_TOKEN`, `SD_CORE_URL`, `SD_CORE_HOST`, `SD_CORE_PORT`,
  `SD_MODEL`, `SD_CLIENT`, `SD_ADAPTER_ID`.
- localStorage: `sd_*` and `cfg2-*` keys.
- MCP server slug: `synaptic-disorder`. MCP tool names:
  `mcp__synaptic-disorder__sd_*`.
- Plugin slug: `synaptic-disorder-claude-code`.
- Docker compose service names: `synaptic-disorder`,
  `synaptic-disorder-ollama`.
- Component name: `SD Core`.
- Folder paths: `Synaptic Disorder/` repo root,
  `%LOCALAPPDATA%\SynapticDisorder\` install dir.

(These were all renamed later in v2.1.0b1.)

### Known issues / deferred

R1–R15 backend refinements (`CODE_HANDOFF`s in `docs/dev/`) — all
shipped in v2.0.1b1.

The trace lifecycle `failed` badge is deferred until a per-memory
deep-encode log endpoint lands.

---

## [v2.0.0a] — 2026-05-08

**Theme:** Alpha 1. ATLAS Connectome Explorer + Brain Focus Mode.

First alpha of the v2 line, retiring the legacy date-based scheme.
Supersedes v26.5.7.1.8. The previous codebase was a 3D Three.js brain
— neurons placed by anatomy, mock event engine firing synapses, a HUD
chrome. v2.0.0a is the same brain plus **the entire ATLAS Connectome
Explorer**, **Brain Focus Mode**, and a polished interaction shell on
top.

### Added — ATLAS Overlay (Connectome Explorer)

A new top-level UI mode toggled by the **MINIMIZE** chip. 8-tab
overlay with manila-paper aesthetic. Theme-aware (`--atlasPaper`,
`--atlasNav`, `--atlasInk`, `--atlasFaint`, `--atlasRule`).

**Header:**
- Wordmark **Synaptic** + subtitle "Connectome Explorer".
- **Global search bar** — indexes traces + concept maps + tags +
  regions; prefix scoping (`trace:`/`tr:`/`map:`/`tag:`/`region:`/`reg:`);
  scoring (exact title 100 / prefix-match 50 / substring 25 / tag-match
  12 / subtitle 8 / full-text 4; cap 30 results); `/` keybind from
  anywhere; ↑/↓ arrow nav, Enter to route, Esc to close.
- **Recents** — last 10 routed targets persisted to
  `sd:atlas:search:recents`. Empty input + focus shows them under a
  "Recent" section header. Re-resolves stale data at click time.

**Keyboard cheat-sheet:** `?` toggles a centered modal listing every
shortcut.

**First-open onboarding hint:** small pulsing tip below the search
input, auto-dismiss at 7 s or on click (persisted as
`sd:atlas:onboarding:seen`).

**Tab navigation:** Horizontal tab-bar with 8 tabs, scrollable on
narrow viewports. ←/→ navigate tabs.

**Universal HUD fade:** `SD_HUD_FADE_SEL` is the single source of
truth for HUD elements that fade during atlas↔brain enter/exit; a
`<style>` tag with `body.ct-expanding :is(<list>) { opacity: 0 …; }`
is dynamically injected at startup from the same array.

### Added — ATLAS tabs

- **Overview** — stats grid (Memories / Synapses / Regions / Unique
  tags); region distribution table; top tag cloud (top 30); most
  recent traces (top 5).
- **Regions** — 14 lateralized regions with memory + synapse counts +
  bar viz. Driven by `REGION_INFO` map.
- **Region detail (P5-J)** — region emoji + name + Brain-focus icon;
  "In the Human Brain" prose + "In the Synaptic Brain" interpretation;
  memory list; brain-focus action.
- **Maps (P5-C)** — 5 sub-tabs (Projects · Entities · Technology ·
  Concepts · Archives) driven by `ATLAS_MAP_TYPES` config; tag-cluster
  MemoryMaps built client-side from co-occurrence; master-detail flow;
  map editor (edit name/type, archive, reset; persists to
  `sd:atlas:map-overrides`); search-scoped prefixes; promote-to-Map
  flow.
- **Lexicon (P5-E)** — two views: **Frequency** (default, dense table
  with brain-focus icon per row) + **Cloud** (chips sized by
  `log(count)`). Click any tag → drill into `_renderMapDetail` flow.
  Force-directed Graph view was shipped and ultimately removed —
  readability of node labels against edge clutter never reached
  production-grade.
- **Tag detail** — reuses `_renderMapDetail` via `_buildTagMap`
  helper.
- **Research** — placeholder (backend-blocked on `sd_research`).
- **Audit** — Experiences sub-tab (full-text search across ALL traces,
  no 50-trace cap); Trace Editor with Save/Cancel/Reset; tag reverse
  lookup with green `↗` / muted `+` chips; lifecycle badge in trace
  header; Audit history collapsible shell (backend pending); Dream
  Journal sub-tab placeholder.
- **CT Scan** — CRT power-on/off animations; `BrainViewport` component
  (P5-I Phase B) — portable Three.js canvas host; embedded HUD (model
  line, EKG-style activity strip, region/memory stats, MRI axis
  controls, position slider); Focus Mode integration with focus
  stamps; tri-state power button.
- **Config** — 7 collapsible folders (Visual · Brain · Spatial ·
  Diagnosis · Motion · Connection · Export); theme picker (13 built-in
  + custom); text size modifier (Small/Medium/Large); render panel;
  brain rendering kill-switch.

### Added — Brain Focus Mode (P5-I)

A way to spotlight a subset of memories in the 3D brain.

- **Renderer core (Phase A)** —
  `BrainRenderer.setFocusFilter({ memIds, label })` dims out-of-focus
  neurons to 8% via color multiplier, scales in-focus ×1.15, culls
  synapses outside `memIds ∪ 1-hop neighbors`. `window.sdFocus` API:
  `enable(cfg)`, `disable()`, `current()`, `recent()`,
  `clearHistory()`. Recent focus history: max 12 entries × 2000 memIds
  each, persisted to `sd:focus:history`.
- **BrainViewport component (Phase B)** — refactor of CT Scan terminal
  into a portable component. Power-button bar + DOS-text header +
  canvas slot. Single-canvas migration on mount/unmount.
- **Interaction polish** — hover preview, smart click-drill with
  `_lastRegionKey` tracking, ESC two-step in brain view, medical-login
  boot screen with Doctor Who actor name pool (DR.HARTNELL …
  DR.GATWA + DR.DRAGONE).
- **Focus stamps** — rubber-stamp animation reusing diagnosis-stamp
  keyframes; type-prefix codes (TAG / TRACE / PRO / ENT / TEC / CON /
  ARC / REGION); erase animation (SVG-mask 8 stripes + final
  clip-path swipe).
- **Focus entry points (P5-J)** — MemoryMap detail brain icon, Atlas
  region detail brain icon, Lexicon tag rows brain icon, Trace detail
  brain icon, Recent Focus HUD in `.hud-bl`.
- **Tag focus (P5-K)** — Lexicon brain icons + Overview tag-chip
  clicks both trigger it.

### Added — Brain page polish

- **Show Timeline toggle** — timeline hidden by default. Show button
  at bottom-center; when shown, the timeline spans the full viewport
  and HUD columns shift up to 90 px. Persisted to `sd:timeline:shown`.
  Mobile keeps timeline suppressed.
- **Power button glyph → SVG** (was `&middot;`, didn't render reliably
  on mobile).
- **Recent Focus HUD** absorbed into `.hud-bl` (was top-right).

### Added — Boot / Loading

- **Two-stream progress bar** — Brain stream 0..70% (anatomy → voxels
  → memories → synapses → surface) + Auth stream 0..30%
  (typeUser → typePass → loginFlash → AUTHORIZED). Bar fills only when
  both streams complete. CSS transitions for smoothing (1.0 s
  cubic-bezier) — not `requestAnimationFrame` (some headless contexts
  throttle rAF to zero). Visibly fills to 100% before overlay hides
  (~950 ms post-completion).
- **Login screen** — form height stable during password typing.

### Changed — Display rename

"Synaptic Disorder" → "Synaptic" across all 12 user-facing strings:
browser title, footer credits (×2), Overview subtitle, region detail
labels, `REGION_INFO` fallback strings, side-panel region detail
label, console boot log, code comments. Filesystem paths
(`AppData\Local\SynapticDisorder\`), localStorage `sd:*` prefix,
install folder, and the Hindsight memory directory intentionally
**untouched** at this stage.

### Architecture

- **Single-file HTML.** Entire dashboard ships as one `index.html`
  (~833 KB at v2.0.0a, was ~631 KB). No build step, no `src/`
  directory, no transpile.
- **Class layout:** `BrainRenderer` (Three.js scene),
  `CameraController` (orbit + auto-rotation + settle home),
  `RegionInteractor` (raycast-based clicks), `RegionsPanel`,
  `BrainViewport` (portable canvas host), `AtlasPanel` (overlay
  open/close/tab logic), `MockEventEngine`, `BridgeClient`.
- **Theme manager** — 13 built-in themes; custom theme via Config
  picker (saved to `sd:custom-themes`); `applyTheme()` writes every
  slot to `:root` as CSS variable; all UI reads via `var(--…)`.
- **localStorage keys** — still using `sd:*` prefix
  (`sd:lexicon:view`, `sd:atlas:search:recents`,
  `sd:atlas:onboarding:seen`, `sd:atlas:map-overrides`,
  `sd:trace-overrides`, `sd:timeline:shown`, `sd:text-size`,
  `sd:focus:history`, `sd:ctscan-render`, `sd:custom-themes`,
  `sd:theme`, …).

### Backend roadmap (deferred at the time of release)

P5-1 (`sd_recall` semantic-search HTTP endpoint) handoff doc authored;
P5-1 → P5-19 Connectome backend (Database schema, Trace lifecycle,
three-tier LLM stack, Groundedness Gate, NightlyRunner, MCP tools, web
research) — all shipped in v2.0.0b1 and v2.0.1b1.

D4 native installers (Windows .exe, macOS .dmg, Linux .deb/.rpm)
remain Phase 4 leftover — needs platform-specific build environments
unavailable from a single Windows host.

---

## Pre-v2 era — date-coded "Synaptic Disorder" releases

Releases predating v2.0.0a used a `vYY.M.D.<n>` date-coded scheme and
shipped under the original "Synaptic Disorder" name. Archive ZIPs live
under `Releases/Alpha/Old Beta/`. No formal release notes exist for
this era; the `Beta-Upgrade.md` runbook documents the migration path
from those builds.

Notable preserved markers:

- `v25.5.6.9.0` — first archived snapshot.
- `v26.5.6.4.2` — canonical pre-consolidation snapshot (Beta-Upgrade.md
  references this as the "4 → 2 container" migration baseline:
  separate `synaptic-disorder-core`, `synaptic-disorder-dashboard`
  (nginx), `synaptic-disorder-ollama`, `synaptic-disorder-ollama-init`
  containers; dashboard on port 8765).
- `v26.5.6.6` — AppData install introduced; consolidated 2-container
  layout (`synaptic-core` + `synaptic-ollama-tier1`); single-port 9911.
- `v26.5.6.9.x` and `v26.5.7.1.x` — patch trains leading into the
  v2.0.0a cut.
- `v26.5.7.1.8` — last date-coded release, superseded by v2.0.0a.

### Architectural milestones in this era (reconstructed)

- 4-container → 2-container consolidation (separate
  `synaptic-disorder-core` + nginx dashboard + Ollama + Ollama-init →
  unified `synaptic-disorder` Go binary serving the dashboard, API, WS
  + bundled Ollama).
- Port 8765 → 9911 (nginx dashboard retired; SD Core serves
  `index.html` directly).
- In-browser Ollama call → server-side bubble generator goroutine
  inside SD Core.
- Optional Bearer-token auth (`SD_API_TOKEN`).
- Adapter env vars: `SD_CORE_URL` + `SD_API_TOKEN` added alongside the
  legacy `SD_CORE_HOST` + `SD_CORE_PORT`.
- AppData install pattern (`%LOCALAPPDATA%\SynapticDisorder\` /
  `~/Library/Application Support/SynapticDisorder/` /
  `~/.local/share/synaptic-disorder/`) introduced via
  `tools/install_to_appdata.py`.

---

## Credits

Brain atlas: Allen Institute, Ding et al. 2020, RRID:SCR_017764,
CC-BY 4.0. Full bibliography of the 25 papers underlying the
architecture: `docs/research/CITATIONS.md` and `docs/CREDITS.md`.

By [NomadsGalaxy](https://github.com/nomadsgalaxy).
