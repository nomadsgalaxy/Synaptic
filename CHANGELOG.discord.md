# Synaptic — Discord-formatted changelog (oldest → newest)

Copy each `─── MESSAGE n ───` chunk into a separate Discord message
(each is ≤ 2000 chars, the non-Nitro limit). Discord-flavored markdown:
`#`/`##`/`###` headers, **bold**, *italic*, `inline code`, code fences,
bullet lists with `-`. No tables — converted to bold-prefixed bullets.

═══════════════════════════════════════════════════════════════════════
─── MESSAGE 1 ───
═══════════════════════════════════════════════════════════════════════

# Synaptic — Changelog

All notable changes to **Synaptic** (previously released as *Synaptic Disorder*), oldest first.

Versioning since v2.0.0a (2026-05-08): `vM.F.P<suffix>` where suffix is `a<n>` alpha · `b<n>` beta · `rc<n>` RC · *(none)* = stable. Beta-wave number advances when the tester pool expands.

Pre-v2 builds used `vYY.M.D.<n>` date codes — archived under `Releases/Alpha/Old Beta/`.

═══════════════════════════════════════════════════════════════════════
─── MESSAGE 2 ───
═══════════════════════════════════════════════════════════════════════

## 📦 Pre-v2 era — date-coded *Synaptic Disorder* releases

Builds predating v2.0.0a used `vYY.M.D.<n>` date codes and shipped under the original name. Archive ZIPs under `Releases/Alpha/Old Beta/`. No formal release notes; `Beta-Upgrade.md` documents the migration path.

### Notable markers
- **v25.5.6.9.0** — first archived snapshot.
- **v26.5.6.4.2** — canonical pre-consolidation baseline. 4-container layout: `synaptic-disorder-core` + `synaptic-disorder-dashboard` (nginx) + `synaptic-disorder-ollama` + `synaptic-disorder-ollama-init`. Dashboard on `:8765`.
- **v26.5.6.6** — AppData install introduced. Consolidated to 2 containers (`synaptic-core` + `synaptic-ollama-tier1`). Single port `:9911`.
- **v26.5.6.9.x** + **v26.5.7.1.x** — patch trains leading into v2.0.0a.
- **v26.5.7.1.8** — last date-coded release.

### Architectural milestones in this era *(reconstructed)*
- 4-container → 2-container consolidation (nginx dashboard retired; SD Core serves `index.html` directly).
- Port `8765` → `9911`.
- In-browser Ollama → server-side bubble generator goroutine inside SD Core.
- Optional Bearer-token auth (`SD_API_TOKEN`).
- `SD_CORE_URL` + `SD_API_TOKEN` adapter env vars added alongside legacy `SD_CORE_HOST`/`SD_CORE_PORT`.
- AppData install pattern (`%LOCALAPPDATA%\SynapticDisorder\` etc.) via `tools/install_to_appdata.py`.

═══════════════════════════════════════════════════════════════════════
─── MESSAGE 3 ───
═══════════════════════════════════════════════════════════════════════

## 🎨 v2.0.0a — 2026-05-08

**Theme:** Alpha 1 — ATLAS Connectome Explorer + Brain Focus Mode.

First alpha of the v2 line, retiring the date-based scheme. Supersedes v26.5.7.1.8. Same brain plus the entire ATLAS overlay, Focus Mode, and a polished interaction shell on top.

### Added — ATLAS Connectome Explorer
8-tab overlay (Overview / Regions / Maps / Lexicon / Tag detail / Research / Audit / CT Scan / Config) toggled by the MINIMIZE chip. Manila-paper aesthetic, theme-aware.

- **Global search bar** — indexes traces + maps + tags + regions; prefix scoping (`trace:`/`map:`/`tag:`/`region:`); `/` keybind from anywhere.
- **Maps tab** — 5 sub-tabs (Projects · Entities · Technology · Concepts · Archives); tag-cluster MemoryMaps built from co-occurrence; map editor with archive/reset; promote-to-Map flow from any tag chip.
- **Lexicon tab** — Frequency table + Cloud view. Click any tag → drill into map-detail flow via `_buildTagMap`.
- **Audit tab** — full-text Experiences search (no 50-trace cap); Trace Editor with Save/Cancel/Reset; tag reverse lookup with `↗` / `+` chips; lifecycle badges.
- **CT Scan tab** — CRT power-on/off animations; portable `BrainViewport` component; embedded HUD (model line, EKG strip, region/memory stats, MRI controls).
- **Config tab** — 7 folders; 13 built-in themes + custom theme picker; text-size modifier; render panel; brain rendering kill-switch.

═══════════════════════════════════════════════════════════════════════
─── MESSAGE 4 ───
═══════════════════════════════════════════════════════════════════════

## 🎨 v2.0.0a *(continued)*

### Added — Brain Focus Mode (P5-I)
Spotlight a subset of memories: dim out-of-focus neurons to 8%, scale in-focus ×1.15, cull synapses outside `memIds ∪ 1-hop neighbors`.

- `window.sdFocus` API: `enable(cfg)`, `disable()`, `current()`, `recent()`, `clearHistory()`. History persisted to `sd:focus:history` (12 entries × 2000 memIds).
- **Focus stamps** — rubber-stamp animation; type-prefix codes (TAG / TRACE / PRO / ENT / TEC / CON / ARC / REGION); erase animation (SVG-mask 8 stripes + clip-path swipe).
- **Entry points** — MemoryMap detail / region detail / Lexicon row / Trace detail brain icons; Recent Focus HUD in `.hud-bl`.
- **Medical-login boot screen** with Doctor Who actor name pool.

### Added — Brain page polish
- Show Timeline toggle (hidden by default).
- Two-stream progress bar (Brain 0..70% + Auth 0..30%) — CSS transitions (not rAF, which gets throttled in headless contexts).

### Architecture
Single-file HTML (~833 KB). No build step. Classes: `BrainRenderer`, `CameraController`, `RegionInteractor`, `RegionsPanel`, `BrainViewport`, `AtlasPanel`, `MockEventEngine`, `BridgeClient`. Theme manager with CSS-variable slots — instant theme switch.

═══════════════════════════════════════════════════════════════════════
─── MESSAGE 5 ───
═══════════════════════════════════════════════════════════════════════

## 💭 v2.0.0b1 — 2026-05-10

**Theme:** Phase 0b deep enrichment + prose-only product rebrand.

### Added — Phase 0b (REM-style deep enrichment)
Stage 1 is now two phases:
- **Phase 0a** *(existing)* — Tier 1 light encoding. Embed via `nomic-embed-text`, classify region via `llama3.2:3b`, flip `light_encoded`.
- **Phase 0b** *(new)* — Tier 2 deep enrichment. Re-summarize, retag, refine region. One memory at a time. Resumable across nightly runs via cursor.

Stage 2 phases (dedup, synthesis, cross-region, schemas) skip un-deep-encoded memories. Phases 3 (lexicon), 4 (maps), 5 (decay), 6 (augment), 9 (reinforcement), 11 (narrative), 12 (dream entry) run unconditionally.

### Added — Dirty-flag queue
Memories carry `marked_dirty_at` + `dirty_reason`. Initial triggers: `created`, `edited`, `model_upgrade`. (`cascading`/`periodic`/`temporal_neighbor`/`surprise`/`emotional` shipped in v2.0.1b1.)

### Added — Stats v3 + backfill
- Run rows: `deep_encoded_this_run`, `deep_encoded_remaining`, `deep_encoded_cursor`.
- Aggregate `stats.deep_encoding` block: coverage%, queue breakdown by reason, ETA, token usage.
- `POST /admin/deep-encode-all` synchronous (~70 min for 3000 memories at concurrency 1). Returns 424 when Tier 2 unconfigured.
- New `/settings/dream_pipeline` keys: `deep_enrich_enabled`, `deep_enrich_max_per_run` (100), `deep_enrich_token_budget_per_run` (50000), `deep_enrich_concurrency` (1).

═══════════════════════════════════════════════════════════════════════
─── MESSAGE 5b ───
═══════════════════════════════════════════════════════════════════════

## 💭 v2.0.0b1 *(continued)*

### Added — MCP server v0.4.0 (17 tools)
7 new tools: `sd_dream_run`, `sd_dream_status`, `sd_deep_encode_all`, `sd_embed_all`, `sd_lexicon_rebuild`, `sd_get_dream_settings`, `sd_set_dream_settings`. Hook forwarder translates each into a typed dashboard pulse.

### Added — Citations system
25-paper APA-7 bibliography (`docs/research/CITATIONS.md`) strictly user-facing. Engineering tracking → `docs/dev/Pinned.md`. Process → `docs/dev/CITATION_PROTOCOL.md`. Credits → `docs/CREDITS.md`.

### Changed — Display rebrand (prose only)
"Synaptic Disorder" → "Synaptic" everywhere except wire-level identifiers. Env vars, localStorage, MCP slugs, plugin slugs, Docker service names, AppData install path all kept their legacy names *(renamed later in v2.1.0b1)*.

═══════════════════════════════════════════════════════════════════════
─── MESSAGE 6 ───
═══════════════════════════════════════════════════════════════════════

## 🧠 v2.0.1b1 — 2026-05-11 *(early)*

**Theme:** R1-R15 backend refinement chain.

### Added — R1-R15 refinements (all 15 shipped)
Every "Known issues / deferred" item from v2.0.0b1 landed:
- **R1** runtime salience scoring
- **R2** weighted SHY decay
- **R3** expanded dirty-flag triggers: `cascading`, `periodic`, `temporal_neighbor`, `surprise`, `emotional`
- **R4** generative replay
- **R5** Phase 8.5 schema reframing
- **R6** Phase 7 low-similarity bridges
- **R7** `consolidation_stage` column
- **R8** schema-from-replay
- **R9** weak-trace boost
- **R11** TMR (targeted memory reactivation) endpoint
- **R12** adaptive phase weighting (activated)
- **R13** Phase 8.7 context memory *(feature flag, default OFF — Johnson 2005 is hypothesis-grade)*
- **R14** global `recall_strength` rebalancing
- **R15** augmentation fallback

22 new tests landed alongside.

### Added — AirLLM at Tier 2/3
Dream-pipeline can now route Tier 2 (deep encoding) and Tier 3 (Oracle research) through AirLLM. Sidecar `/healthz` chip in provider UI. Tier 1 stays on `nomic-embed-text`.

### Added — Crash recovery
Nightly runs write a heartbeat row; on next start, SD Core reconciles any run mid-flight when the container crashed, marks it `interrupted`, and resumes from the recorded cursor.

### Changed — Config IA reorganization
5 → 10 sub-tabs (Models / Pipeline / Bridge / Brain / Display / Bubbles / Identity / Privacy / Data / Diagnostics) with hamburger overflow. "deep-dirty" badge renamed **"undreamt"**; contradictory `enriched` badge dropped.

### Added — Hook log feature
Dashboard pane streams the Claude Code plugin's hook-forwarder output in real time. Debug "why didn't my event fire?" without `tail -f`.

### Fixed — Sanitization
Hardcoded `SD_API_TOKEN` literal in `bridge/gemini-cli-hooks/forward.js` and author's tunnel hostname + Windows path in test fixtures now scrubbed before Prod.

═══════════════════════════════════════════════════════════════════════
─── MESSAGE 7 ───
═══════════════════════════════════════════════════════════════════════

## 🚀 v2.0.2b1 — 2026-05-11

**Theme:** fresh-install volume bug + Docker/AirLLM UX hardening.

### Fixed (release-blocker)
- **`synaptic-disorder-bank` volume `external: true` removed.** New users hit `external volume not found` before any service started. Flipped back to compose-managed; legacy → new volume migration documented.

### Fixed
- **Docker control plane recurring 404.** Helper-container recreates on Docker Desktop/Windows silently broke the dashboard — `./` resolved to the helper's `/workspace` instead of the host project dir. Fixed by passing `--project-directory <daemon-translated-host-path>`.
- **Dream Journal entry truncation.** Prompt now requests 2-4 paragraphs ending on complete sentence; `max_tokens` 600 → 800; sentence-aware post-trim guillotine at 3000 chars.
- **Lexicon Pairs panel** rendered blank because frontend read `p.a`/`p.b`/`p.count` but backend canonical (wave 6) returns `tag_a`/`tag_b`/`cooccurrence`. Now reads both.
- **Maps → Concepts trace count** stuck at 0; merge logic now adopts synthesized count + pre-populates `traces` array.
- **Bubbles toggle** on Brain page was unclickable (rendered under Atlas chip).

### Added — AirLLM end-to-end UX
- HF token input on AirLLM profile card; validates via HF `whoami-v2`; auto-recreates container on save.
- NVIDIA GPU passthrough in compose stanza.
- GPU preflight at `/admin/airllm/diagnostics` — `configure_tier` returns `400 cuda_required` for AMD/Apple/CPU-only hosts, preventing 100 GB of wasted download.
- Profile cards surface disk (~140 GB for 70B FP16 download) + GPU requirements prominently.

═══════════════════════════════════════════════════════════════════════
─── MESSAGE 8 ───
═══════════════════════════════════════════════════════════════════════

## 🚀 v2.0.2b1 *(continued)*

### Added — Per-tier Ollama + lifecycle
- Dedicated `ollama-tier3`/`airllm-tier3` retired (wave 8c). Tier 2 serves both `nightly_run` and `research_request`. Profile name standardized `tier2-ollama` → `ollama-tier2`.
- New `llm_call_timeout_sec` setting (default 120) with provider-aware auto-bump: AirLLM → 1800s. `SignalProviderUse(tier)` fires before every Chat call so the idle reaper doesn't kill slow inference.

### Added — Benchmarking + nightly duration estimator
- `POST /admin/llm/benchmark/{tier}` — measures tokens/sec, persists to `provider_benchmark`.
- `GET /admin/llm/estimate_nightly` — "your next nightly: ~4h 12min, ~70% in Phase 0b".
- Auto-benchmark on `configure_tier` queues a background job so estimates are ready when user navigates to Dream Journal.

### Added — Service-card UX
- Tier 2 auto-register on Profile Enable (auto-PUTs provider config to point at the container).
- "Set Active · Tier X" buttons on each model-serving service card.
- Profile cards grew rich metadata (tier role, GPU req, RAM, disk, lifecycle); sorted active-first.

═══════════════════════════════════════════════════════════════════════
─── MESSAGE 8b ───
═══════════════════════════════════════════════════════════════════════

## 🚀 v2.0.2b1 *(continued)*

### Changed — Brain rendering rewrites
Fix for the recurring "synapse animations cut each other off":
- **FireAnimationQueue** — concurrent cap 6 + intensity-scaled duration; pending hooks release with 150ms stagger.
- **Invisible pulses with additive tube halo** — pulses are now light sources, not sprites. Two pulses on the same wire show as two glowing spots passing each other.
- **Neuron glow pool 3 → 8 lights**, smoothed transitions (root cause of "region B firing visually killed region A").
- **`MAX_PULSES` 15 → 320** — the 15-cap was a revolving door evicting every prior pulse on each fire.

### Added — Dream Journal stat drill-down
Every stat (Encoded, Consolidated, Maps Updated, Merged, Pruned, Lexicon Pairs, Cross-region, Schemas, Reinforced) is now a clickable button toggling a focused breakdown panel.

### Known issues / deferred
Host-free-bytes on `/admin/docker/disk`; window-bounded nightly run; AirLLM `kernels>=0.11.1` not yet rebuilt into the image.

═══════════════════════════════════════════════════════════════════════
─── MESSAGE 9 ───
═══════════════════════════════════════════════════════════════════════

## 🔧 v2.0.3b1 — 2026-05-11

**Theme:** fresh-install simulation pass.

Companion patch on top of v2.0.2b1 after dry-running the "fresh user unzips and follows the README" flow. None of this affects existing installs.

### Fixed
- **Installer env file:** writes `.env` (dotted), not `env`. Compose only auto-reads the dotted form, so every previously generated env knob (`SD_API_TOKEN`, `SD_OLLAMA_TIER1_MODEL`, `HUGGING_FACE_HUB_TOKEN`) was inert.
- **Env template:** now sets `SD_OLLAMA_TIER1_MODEL` / `SD_OLLAMA_TIER2_MODEL` (was the wave-7c1-deprecated `SD_OLLAMA_MODEL`).
- **`--uninstall --purge`:** correct compose-managed volume names (was silently skipping every legacy name).
- **Start scripts** (`start.sh`/`start.ps1`): default port `8765` → `9911`. Polling loop had been timing out on a healthy stack and auto-opening a dead URL.
- **Docs:** stale port refs in `Agent-Install.md`, `README.md`, and the Python SDK example now distinguish Docker/Native (9911) from static-only fallback (8765).

No backend code changed.

═══════════════════════════════════════════════════════════════════════
─── MESSAGE 10 ───
═══════════════════════════════════════════════════════════════════════

## 🔖 v2.1.0b1 — 2026-05-11

**Theme:** full *"Disorder"* → *""* identifier rebrand. **Breaking** for existing beta installs.

The product had been renamed in prose since v2.0.0a, but install dirs, volume names, plugin slugs, MCP slugs, and package names still carried the legacy stem. v2.1.0b1 finishes the work.

### Renamed
- **Windows install dir:** `%LOCALAPPDATA%\SynapticDisorder` → `%LOCALAPPDATA%\Synaptic`
- **macOS install dir:** `~/Library/Application Support/SynapticDisorder` → `…/Synaptic`
- **Linux install dir:** `~/.local/share/synaptic-disorder` → `~/.local/share/synaptic`
- **Bank volume:** `synaptic-disorder-bank` → `synaptic-bank`
- **Image tag:** `synaptic-disorder/core:latest` → `synaptic/core:latest`
- **Plugin slug:** `synaptic-disorder-claude-code@synaptic-disorder` → `synaptic-claude-code@synaptic`
- **MCP slug:** `synaptic-disorder` → `synaptic`
- **MCP tool prefix:** `mcp__synaptic-disorder__sd_*` → `mcp__synaptic__sd_*`
- **Python pip:** `synaptic-disorder` / `import synaptic_disorder` → `synaptic` / `import synaptic`
- **npm:** `@synaptic-disorder/sdk` → `@synaptic/sdk`
- **Adapter pkgs:** `synaptic-disorder-{mcp,otel,otlp-receiver}` → drop `-disorder-` stem

47 files swept, ~65 prose replacements.

### Preserved (wire-level back-compat)
Env vars (`SD_*`), localStorage prefixes (`sd_*`, `cfg2-*`), container names (`synaptic-core`, `synaptic-ollama-tier1`, etc.), component name **SD Core**, source-tree variable names, `[SD]` log prefix.

### Compat shim
The plugin's `forward.js` regex matches **both** old and new MCP slugs, so pre-2.1.0b1 plugin installs keep emitting events until users re-install.

### Migration
`docs/dev/Legacy migration paths.md` §§ 1–2 cover the AppData-dir move and the bank-volume rename.

═══════════════════════════════════════════════════════════════════════
─── MESSAGE 11 ───
═══════════════════════════════════════════════════════════════════════

## 🙏 Credits

Brain atlas: **Allen Institute**, Ding et al. 2020, RRID:SCR_017764, CC-BY 4.0.

Full bibliography of the 25 papers underlying the architecture: `docs/research/CITATIONS.md` and `docs/CREDITS.md`.

By **NomadsGalaxy** — <https://github.com/nomadsgalaxy>

═══════════════════════════════════════════════════════════════════════
END
═══════════════════════════════════════════════════════════════════════
