## Synaptic v2.5.0b1 — Discord release notes
## Each `---POST---` marker = one Discord Nitro message (≤4000 chars).
## Copy/paste each section between markers in order.

---POST---
# **Synaptic v2.5.0b1**
*the integration release · 2026-05-12*

Maps gain a proposal lifecycle and a feedback loop that lets research deepen them. Memories gain bi-temporal supersede semantics. The bank gains bulk import. Recall gains a map-level surface. The theme customizer becomes a real tool.

We skipped public 2.2 / 2.3 / 2.4 — this is the next public drop after 2.1.0b1.

**TL;DR**
• __Theme system overhaul__ — Material-style customizer + WCAG checker, 13 themes → 7 families with Dark/Light toggle, cross-device sync
• __R13 map proposals__ — Tier 2 proposes, you accept/edit/dismiss. No silent auto-commits
• __R10 supersede__ — memories are append-only; corrections are bi-temporal edges
• __Research-into-memory loop__ — Tier 3 research scoped to a map ingests back as a new memory, deep-encoded next nightly
• __Bulk import__ — Hindsight / mem0 / Letta / generic JSON
• __Map-level recall__ — *"recall the project X"* returns X as the top hit
• __Configurable sensitive classifier__ + bulk re-classify
• __Intrusive Thoughts__ — uncategorized memories orbit the brain in a faint cloud
• __Token-bucket rate limit__ on memory writes, hot-swappable

Details below ⬇️

---POST---
## 🎨 **Theme system overhaul**

**ThemeForge** — full-screen customizer inspired by Material Theme Builder + themer.dev, in our clinical aesthetic. Six anchor seeds (`primary`, `secondary`, `error`, `surface`, `ink`, `bg`) derive 18 CSS vars via HSL tone math; the other 12 fall out automatically.

• Live preview tray — 8 sample UI surfaces update as you drag pickers
• WCAG contrast checker — 10 load-bearing pairs per theme, tiered AAA / AA / AA-L / FAIL, decorative pairs marked separately
• JSON export for sharing
• Pencil icon on every theme — opens ThemeForge pre-loaded with that theme's seeds

**Theme families** — 13 separate themes collapsed to **7 family rows** (Manila / Hex / Hex Pro / Blueprint / Clinical / Paper / Custom) with a single global Dark/Light toggle. Click "Hex" then toggle LIGHT — family stays "Hex," variant flips. Same toggle inside ThemeForge swaps between siblings when editing.

**Four new variants** to complete every pair:
• **Light Manila** — warm parchment, cyan HUD + magenta pulse
• **Whiteprint** — Blueprint's white-paper inverse, navy ink + cyan accent
• **Clinical Dark** — deep-navy clinical with bright cyan HUD
• **Mocha Paper** — roasted-bean coffee with cream HUD

**All themes re-baked** as `{seeds, variant, overrides}`. Every theme passes WCAG contrast (0 FAILs across 90 contrast pairs). The Layer 2 derivation engine uses bg-lightness (not just variant flag) to decouple atlas chrome from 3D scene, so light themes can pair pale chrome with dark brain plinths.

**Cross-device theme sync** — customs + Dark/Light preference follow the user across devices via `/settings/sd:themes:*`.

**Custom has both slots** — `custom` (dark) + `custom-light`. ThemeForge variant toggle auto-saves the current side before swapping.

---POST---
## 🧠 **Memory system: maps as proposals**

**R13 map-proposal pattern.** `POST /maps/propose` accepts `source: tag|query|recall_id|auto`. Tier 2 synthesizes a schema and the result lands as a `MemoryMap` with `status='proposed'` — never auto-committed.

• New **Proposed sub-tab** in Maps with animated pulse when the queue is non-empty
• Inline Accept / Edit / Dismiss buttons per proposed map
• `POST /maps/{id}/accept | dismiss` flips status
• **Phase 8.7** (context memory formation) rewired to propose, not auto-commit — you're back in the loop
• MCP tool: `sd_propose_map`

**"+ Propose map" button** on every Maps sub-tab opens a themed dialog: source selector, type, limit, name override, `include_sensitive` opt-in, optional **"Seed with Tier 3 research"** checkbox that fires `/maps/{id}/research` after a successful propose — useful for cold-start topics.

**"Cleanup…" button** on Maps view with dry-run preview showing count + sample names + breakdown by type/source before commit. `POST /maps/cleanup` accepts `{delete_empty, delete_dismissed, only_type, only_generated_by, dry_run}`. Cascades through `map_traces` + `map_associations` + drops orphan `map_overrides` rows (the bug behind ghost concept maps).

**Fixed map-merge bug** where the source map would reappear in Concepts after merge. Root cause: backend rewrote tags in SQLite but the dashboard's local cache held the pre-merge state; the re-render then re-synthesized the source from stale data. New `_localApplyTagMerge` mirrors the rewrite locally before re-rendering.

**`/memories` is now bank-backed.** Was serving a frozen JSON snapshot from 2026-05-09 — backend mutations (merges, supersede, imports) were invisible after CTRL+F5. Now reads live bank state.

---POST---
## 🔄 **Research → memory feedback loop**

The marquee feature. `POST /maps/{id}/research` closes the loop between "I asked the Oracle a question" and "the agent knows the answer next time."

**What happens when you click Research… on a map:**
1. Backend resolves the map's anchor tags + pulls up to 60 member memories as context
2. Egress chokepoint (`PrepareOracleCall`) drops sensitive + LocalConcept rows
3. Map-aware prompt tells Tier 3: "you're helping refine this map; fill gaps, don't restate context"
4. Tier 3 returns synthesis
5. Result cached in `research_cache` with `map_id` set
6. **NEW memory minted**: text = the synthesis, tags = `[<anchors>, manual_augment, oracle_augmented]`, `source='manual_augment'`, `dirty_reason='created'`
7. Linked to the map via `map_traces`

**The next nightly run picks it up automatically:**
• Phase 0b deep-encodes the augment (high priority, `created` outranks everything but `tmr_user`)
• Phase 7 cross-region finds high-cosine neighbors → bridges form
• Phase 8 schema synthesis clusters it with the map's other traces
• Phase 8.7 considers it when proposing context memories for the region

**Future recall** surfaces it alongside original memories. The agent effectively *remembers* the research as if it had always been in the bank.

Same shape Phase 6's nightly auto-augment uses — manual research front-loads it for a map you picked.

---POST---
## 📥 **Bulk import + map-level recall**

**`POST /import`** — one-way bulk memory import from other memory managers.

Five format adapters:
• `synaptic` — passthrough (round-trip from another install)
• `hindsight` — Hindsight's `retain` payload shape
• `mem0` — mem0's memory shape
• `letta` — Letta archival memory shape
• `generic` — loose mapping (`text` / `content` / `body` / `summary` / `memory`)

Options: `tag_prefix`, `adapter_id`, `preserve_ids`, `mark_sensitive`. 8 MiB payload cap; chunk client-side for bigger banks. Rate-limited via the same chokepoint as `/event`. UI lives in **Config → Data** with format dropdown, file picker, sdConfirm preview, 429 graceful handling.

**`POST /recall/maps`** — map-level recall.

Where `/recall` returns individual memories ranked by cosine, this returns **MemoryMaps** — the higher-level abstractions a query touches. Three signals blend in `hybrid` mode (default):

• **Name match** — *"Can you recall the project Synaptic-Disorder"* returns that map at score 1.00 even if cosine is fuzzy. Clean name match always wins outright via `max(nameScore, blend)`
• **Schema cosine** — embedding of `name + schema_text` matched against query embedding
• **Member aggregate** — top-K member memory cosine averaged

Other modes: `name` (strict), `schema`, `members`. Includes synthesized tag-cluster maps (≥3 members) alongside persisted memory_maps rows. MCP tool: `sd_recall_maps`.

**`POST /reflect`** (was already shipping; finalized in this release) — Tier 3 synthesis over a recall result set. `tags_match` + `budget` + `include_sensitive`. Dashboard surface + MCP tool `sd_reflect`. Sensitive memories filtered by default; opt-in path surfaces `included_sensitive_count`.

---POST---
## 🕰️ **R10 bi-temporal supersede**

Memories are now genuinely append-only. When a memory is corrected or replaced, you record an **edge** in the new `memory_supersedes` table; raw text of both memories stays untouched (R10 immutability preserved).

**Endpoints:**
• `GET /bank/memories/{id}/supersede` — chain (returns `supersedes` + `superseded_by`)
• `POST /bank/memories/{id}/supersede` body `{superseded_id, valid_from, reason}`
• `DELETE /bank/memories/{id}/supersede/{other}` — undo edge

**`/recall` extends:**
• `include_superseded: true` — return superseded entries too
• `as_of: "2025-01-01T..."` — time-travel: return the truth as of that timestamp
• Response surfaces `superseded_excluded` count so the UI can offer to include them

**Trace detail UI** surfaces the chain with "Replaces" / "Replaced by" sections + a "Mark as superseding…" affordance + Undo buttons per edge.

**Config → Privacy** has a default-include toggle for clients that want time-travel by default.

This is the foundation for honest correction: "I learned the prod database is in us-east-2, not us-west-1" becomes a supersede edge, not a mutation. The audit trail is preserved; recall sees the corrected view.

---POST---
## 🔒 **Privacy + rate limiting**

**Memory write rate limit.** Token-bucket middleware on `POST /event` + `POST /bank/memory` + `POST /import`. Default `0` = unlimited. Returns `429 Too Many Requests` with `Retry-After` header when over-cap. Configurable via `/settings/memory_write_rate_limit_per_min` from **Config → Pipeline**; takes effect within ~2s (cache TTL). MCP clients see graceful backoff hints.

**Settable sensitive classifier prompt.** Config → Privacy → "Classifier prompt (Layer 2 AI)" textarea. Persists at `/settings/sensitive_classifier_prompt`. The classifier reads it with a 10s cache; `PUT` / `DELETE` invalidates the cache instantly so changes take effect within ~1s — same hot-swap pattern as `PUT /settings/providers/{tier}`. No restart, no rebuild. Validation rejects prompts without a `%s` placeholder.

**Bulk re-classify.** `POST /admin/sensitive/reclassify` re-runs the (possibly user-tuned) classifier over currently-flagged memories and clears flags on memories the updated classifier no longer thinks are sensitive. **Never raises flags** — only clears false positives. Audited per memory + a summary row. UI lives directly under the prompt editor; dry-run available.

**Default prompt tightened.** The prior prompt mis-flagged "I implemented OAuth" / "Bearer token flow" / "JWT validation" as sensitive because it listed "access tokens" + "credentials" as triggers under a "When uncertain, answer YES" rule. New default explicitly draws the line: discussion of an auth **technology or protocol** is NOT sensitive — only literal secret **values** are. Placeholder strings like `<your-token-here>` are explicitly NO.

**Security fix.** `/import` and `/reflect` were missing from the auth allowlist — anyone could call them without a bearer token. Now properly auth-gated. (`/settings/*` was always gated; the new theme-sync keys inherit that.)

---POST---
## 🌌 **Visualization: Intrusive Thoughts + region canon expansion**

**Intrusive Thoughts.** Memories with `region='unknown'` or non-canonical region strings now render in an **orbital cloud** around the brain — Fibonacci-sphere distribution at r≈1.10, outside the brain mesh's `HALF=0.92` bound so they don't warp the hull. Their synapses render at 12% alpha — present but ghost-faint, signaling "not structurally integrated yet."

Region drilldown explains the metaphor: *"unexpected, fleeting thoughts that haven't found a home yet."*

**Region canon expanded 14 → 38.** Added unlateralized base regions (`frontal_lobe`, `temporal_lobe`, `prefrontal_cortex`, `parietal_lobe`, `hippocampus`, `amygdala`) and real sub-regions previously emitted by the classifier but never enumerated:

`occipital_lobe`, `dorsolateral_prefrontal_cortex`, `inferior_frontal_gyrus`, `lateral_frontal_cortex`, `anterior_cingulate`, `premotor_cortex`, `inferior_parietal_lobe`, `hypothalamus`, `somatosensory_cortex`, `cerebral_cortex`, `limbic_system`, `default_mode_network`

Each has proper `realFn` / `dashFn` copy for the drilldown.

**Region alias table** — ~30 variant spellings resolve to canonical entries: `pre-frontal_cortex` → `prefrontal_cortex`, `default_mode_region` → `default_mode_network`, `visual Cortex` → `visual_cortex`, etc. `resolveRegionKey()` normalizes at boot; the original lands on `m._regionRaw` for debugging.

**Region distribution counter fixed.** Was showing absurd values like *31/20 active regions* because numerator counted raw bank strings (70 distinct after classifier drift) while denominator counted canonical slots. Now both come from the same canonical set; typical reading is `21/38`.

**Brain SVG swapped.** Old single-path stick-figure brain replaced with SVGRepo's "brain-illustration-4" via a shared `<symbol>` defined once at body open. Every brain icon button across the app references it via `<use href>`.

---POST---
## 🛠️ **Bug fixes**

• **Map merge: source map reverted to Concepts** after merge (frontend cache + override interaction). Backend rewrite was working all along; UI was looking at stale state.
• **Ghost concept maps** from orphan `map_overrides` rows pointing at deleted maps. Both `DeleteMemoryMap` and `CleanupMemoryMaps` now sweep override rows whenever they delete a map. Frontend `_buildConceptMaps` also filters out forced overrides keyed by `^map-\d+$` defensively.
• **Region count: `31/20`** → `21/38` via canonicalization.
• **`/memories` serving stale snapshot** — backend mutations invisible after CTRL+F5. Now bank-backed.
• **Neuron idle invisible on dark themes** — derivation flipped sign. Was *darkening* primary on dark scenes instead of lightening, so neurons were invisible against the void.
• **Toast clipping off-screen** — `_showAtlasToast` reused `.atlas-onboard-hint` which carries `position: fixed`, hijacking each stacked toast out of the flex column. New `.atlas-toast` class is positioning-free; one canonical `showSdToast()` helper, body-attached, top-center, immune to ancestor transforms. Every toast in the app now routes through it.
• **Sensitive over-classification** on OAuth-as-a-concept (prompt rewrite).
• **Nightly run booting all Tier 2 sidecars** instead of just the active one. `EnsureForWorkflow` now takes an `activeFilter` parameter; `nightly_runner.go` computes it from `router.Tier2()`.
• **Bank schema migration ordering** for `memory_maps.status` (now created after the column-add migration).
• **Theme picker pencil clicks** triggering theme-apply (event-delegation guard).
• **ThemeForge** is now single-instance (opening one closes any existing) and desktop-only (mobile / coarse-pointer shows a toast).

---POST---
## 📚 **Docs + new endpoint surfaces**

**`CITATIONS.md` re-graded.** Every R-series mapping now carries an explicit **Status grade** (F / P / C). Six items audited with strengthening citations or honest reframing:

• R3 surprise (P → F-leaning, Lisman & Grace 2005 + Greve et al. 2019)
• R5 schema reframing (P, interpretive)
• R8 schema-from-replay (P, Diekelmann & Born 2010 cross-reference)
• R12 adaptive weighting (C, loose inspiration)
• R13 context memory (P, rewritten to map-proposal pattern)
• R14 strength rebalance (P, SHY corroborator)

Goal-alignment closing softened from "every architectural choice in empirical neuroscience" to honest *"biologically informed, not strictly faithful."*

**`CLOUDFLARE_BRIDGE.md`** path-family table updated with every new sub-resource. No tunnel regex change needed — every new endpoint sits under an existing top-level prefix (`/maps`, `/recall`, `/admin`, `/bank/`, `/import`, `/reflect`, `/settings`).

**New endpoints (all auth-gated):**

```
POST /import
POST /maps/propose
POST /maps/cleanup
POST /maps/{id}/accept
POST /maps/{id}/dismiss
POST /maps/{id}/research
POST /recall/maps
POST /reflect
GET|POST|DELETE /bank/memories/{id}/supersede[/{other}]
POST /admin/sensitive/reclassify
GET  /admin/sensitive/default_prompt
```

**New settings keys:**
`sd:themes:custom`, `sd:themes:user`, `sd:themes:pref`,
`sensitive_classifier_prompt`, `memory_write_rate_limit_per_min`

**MCP tools added:** `sd_recall_maps`, `sd_propose_map`, `sd_reflect`.

---POST---
## 🪜 **Migration**

• **No DB migration required** for fresh installs. Existing installs get `memory_supersedes` + `memory_maps.status` / `generated_by` / `generation_phase` columns auto-added at first boot under v2.5.0b1.
• **Existing custom themes**: prior `sd:theme:custom:colors` localStorage blob is treated as overrides on top of the (newly seeded) Dark Manila base for back-compat. Re-save once via ThemeForge to migrate fully.
• **Existing sensitive-flagged memories** from the old over-aggressive classifier: open **Config → Privacy → Bulk re-classify** to sweep the bank under the new prompt and clear false positives. Sticky-on policy still applies — bulk op can only CLEAR flags, never raise them.

## Notes

• v2.2 / v2.3 / v2.4 were internal-only betas. v2.5.0b1 is the next public release after v2.1.0b1.
• Build artifact: `synaptic-2.5.0b1.zip` (~9.0 MB).
• Classifier prompt is intentionally domain-tunable per-install. If your bank's threat model differs from the default, edit it from Config → Privacy.

🧠 *Happy dreaming.*
