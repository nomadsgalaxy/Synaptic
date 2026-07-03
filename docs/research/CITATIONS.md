# Synaptic Disorder — Research Citations

**Last updated:** 2026-05-12
**Purpose:** Catalogue the academic literature that informs Synaptic Disorder's architecture. Every cited paper has a corresponding implementation pattern in the codebase. This file enables three things:

1. A credible bibliography for academic work that describes or builds on Synaptic Disorder.
2. The engineering team's reference when adding new pipeline phases.
3. Traceability from any line of code to the empirical literature that justifies it.

If you're citing Synaptic Disorder in academic work, please also cite the underlying primary sources below — the system's mechanisms were built with generalized knowledge of how the mind and sleep works, but these papers allowed me to pinpoint how we can use actual science regarding memory management and sleep in humans to make a powerful Agent Memory Management system.

---

## How citations are tied to the system

Each citation below has three sections:

- **Mechanism:** what the paper claims
- **Mapped to:** the architectural intent — what part of Synaptic Disorder's design realizes this finding, in plain language. Where the corresponding code has shipped, file paths and line numbers point at the implementation.
- **Synaptic alignment:** how this citation advances the project's mission of building a memory-management system that mimics human brain function.

Code references in "Mapped to" are best-effort snapshots that may rot as the codebase evolves; a maintenance protocol keeps them in sync (see `Dev/docs/dev/CITATION_PROTOCOL.md`). For up-to-date per-architectural-decision tracking — including planned vs shipped status and the queue of pending implementation work — see `Dev/docs/dev/Pinned.md`.

### Citation strength rubric

Some implementations sit on firmer empirical ground than others. Where the link between cited paper and shipped code is not 1:1 — common when neuroscience suggests a mechanism that engineering then approximates with a heuristic — the "Mapped to" section carries an explicit **Status grade** so readers can calibrate their confidence:

- **F (firm)** — well-replicated mechanism, multiple independent studies, mainstream consensus. Default when no grade is shown.
- **P (provisional / interpretive)** — single primary study, contested hypothesis, or a defensible inferential bridge from the paper's claim to the system's behavior. The engineering may still be sound; the *biological warrant* is partial.
- **C (loose inspiration)** — biology supplies framing or motivation, but the implementation is principally an engineering choice. Honest acknowledgement that the citation is decorative as much as load-bearing.

Items without a Status grade are F by default. When you read a P/C grade, treat the architectural decision as engineering-defensible regardless: the system was designed to degrade gracefully if any single citation's mechanism turns out to be overstated.

---

## Foundational reviews (system + synaptic consolidation)

### 1. Stickgold & Walker — *Sleep-Dependent Memory Triage* (2013)

**Stickgold, R. & Walker, M.P. (2013).** Sleep-dependent memory triage: Evolving generalization through selective processing. *Nature Neuroscience*, **11**(2), 139–145.

**Mechanism:** Sleep transforms memories rather than just preserving them. Gist is enhanced *at the cost of* details; false memories grow because rules survive while exemplars fade. Sleep selectively decides what to consolidate vs. discard based on future-relevance signals set during waking.

**Mapped to:** The Phase 0b deep enrichment loop (`nightly_phase0b.go` `runPhase0bDeepEncoding`) applies Tier 2-driven gist drift to a memory's summary while preserving the raw `text` field as the immutable hippocampal trace. The refined summary lands in `enriched_text` (`bank_p5.go` `PersistDeepEncoding`); per-memory diffs are captured in the `deep_encoding_log` audit table.

**Synaptic alignment:** Mirrors how a real brain operates. The system maintains an immutable record of what was originally observed (the hippocampal episodic trace) while progressively transforming the actively-used representation toward gist (the neocortical semantic trace). Users gain a memory system that gets *smarter* about its own contents over time, not just more populated.

### 2. Stickgold & Walker — *Overnight Alchemy: Sleep-dependent Memory Evolution* (2010)

**Stickgold, R. & Walker, M.P. (2010).** Overnight alchemy: Sleep-dependent memory evolution. *Nature Reviews Neuroscience*, **11**(3), 218.

**Mechanism:** Distinguishes system consolidation (SWS-driven hippocampus→neocortex transfer) from synaptic consolidation (REM-driven local plasticity). Establishes the two-stage architecture for memory consolidation that maps directly onto our pipeline staging.

**Mapped to:** The two-stage pipeline structure — Phase 0b synaptic consolidation (`nightly_phase0b.go`) feeds Stage 2 system consolidation, with Stage 2 phases gated on memories having been deep-encoded (`nightly_pipeline.go` `loadEmbeddedMemoriesWithGate`). [R7] **shipped** — `MemoryRecord.ConsolidationStage` tracks `episodic → consolidating → semantic` migration. `bank_p5.go` `SetMemoryLifecycle` flips `episodic → consolidating` on `light_encoded=true`; `nightly_pipeline.go` `runPhase9Reinforcement` flips `→ semantic` when a memory is both deep-encoded AND has `recall_strength > 1.5`. Stages never regress.

**Synaptic alignment:** Synaptic Disorder is not a flat memory store — it's a graded system where memories migrate from "recently encoded specifics" (hippocampus-like) to "integrated general knowledge" (neocortex-like) as they're processed and re-accessed. This is what distinguishes a *memory* system from a *log*.

### 3. Paller, Creery & Schechtman — *Memory and Sleep* (2021)

**Paller, K.A., Creery, J.D. & Schechtman, E. (2021).** Memory and sleep: How sleep cognition can change the waking mind for the better. *Annual Review of Psychology*, **72**, 123–150.

**Mechanism:** Modern integrated review of nested oscillations (sharp-wave ripples + thalamo-cortical spindles + slow oscillations). Targeted Memory Reactivation (TMR): external cues during sleep can selectively re-prioritize specific memories.

**Mapped to:** [R11] **shipped** — `POST /bank/memories/{id}/dirty` accepts `{reason: "tmr_user"}` (handler: `handlers_p5.go` `handleMemoryDirty`; bank: `bank_p5.go` `MarkMemoryDirty`). The `PickNextDirtyMemory` priority CASE was updated so `tmr_user` outranks every other dirty reason — user-marked memories hop to the front of the next Phase 0b queue.

**Synaptic alignment:** Gives the user agency over the consolidation pipeline. A real brain doesn't let the user direct what consolidates — but a memory system *should*, because the user has goals the system can't infer. TMR provides the biological precedent for this affordance: external cues are a real mechanism, not just a UX convenience.

---

## Synaptic homeostasis hypothesis (SHY)

### 4. Tononi & Cirelli — *Sleep and the Price of Plasticity* (2014)

**Tononi, G. & Cirelli, C. (2014).** Sleep and the price of plasticity: From synaptic and cellular homeostasis to memory consolidation and integration. *Neuron*, **81**(1), 12–34.

**Mechanism:** Sleep performs competitive down-selection — weakly activated synapses depress while frequently potentiated, schema-consistent synapses are protected. Net effect: weak memories fade, strong ones survive proportionally.

**Mapped to:** [R2] **shipped** — `nightly_pipeline.go` `runPhase5Decay` is a continuous strength multiplier: `protection = 0.5*salience + 0.3*(recall_strength/5) + 0.2*(len(tags)/5)`; `decayed = recall_strength * (1 - DecayBaseRate*(1-protection))`. Memories whose post-decay strength drops below `DecayStrengthFloor` AND match the legacy eligibility predicates (age, recall window, tag whitelist) become pruning candidates. Pairs operationally with Phase 9 reinforcement (same `recall_strength` column, opposite directions). New settings: `decay_base_rate` (default 0.05), `decay_strength_floor` (default 0.05). Sensitive memories never participate.

**Synaptic alignment:** Without SHY-style competitive down-selection, the bank grows unboundedly and recall quality degrades over time. With it, the bank approaches a stable distribution where the most useful memories occupy the most prominent positions, and unused content fades gracefully. This is the difference between a memory system that's useful for one year vs. one decade.

### 5. Tononi & Cirelli — *Sleep and synaptic down-selection* (2020)

**Tononi, G. & Cirelli, C. (2020).** Sleep and synaptic down-selection. *European Journal of Neuroscience*, **51**(1), 413–421.

**Mechanism:** Updated SHY. Despite global SWS-driven down-selection, REM selectively *strengthens* certain synapses — the mechanism is weighted, not uniform.

**Mapped to:** [R9] **shipped** — `nightly_pipeline.go` `runPhase9Reinforcement` runs a weak-trace boost FIRST pass (selector: `salience > 0.6 AND recall_strength < 0.8`, +0.05 strength delta) before the recall-based reinforcement loop. Boosted IDs are tracked in `r9Boosted` so the secondary recall-decay branch can't overwrite the boost with stale arithmetic.

**Synaptic alignment:** Counterintuitive but biologically correct: strong memories don't need help, weak-but-important ones do. Synaptic Disorder treats this as a deliberate design choice rather than letting the most-recalled memories dominate. The system protects fragile knowledge before it's lost.

---

## Hippocampal replay and sharp-wave ripples

### 6. Buzsáki — *Hippocampal sharp wave-ripple* (2015)

**Buzsáki, G. (2015).** Hippocampal sharp wave-ripple: A cognitive biomarker for episodic memory and planning. *Hippocampus*, **25**(10), 1073–1188.

**Mechanism:** Sharp-wave ripples are discrete events during which compressed sequences replay from CA3 → CA1 → cortex. Crucially, SPW-Rs **combine recently acquired and pre-existing information** — replay is generative, not playback.

**Mapped to:** Phase 10 replay (`nightly_pipeline.go` `runPhase10Replay`, R4) generates new sequences that explicitly mix recent (`ReplayRecentDays` window, default 7d) and pre-existing (`ReplayPreexistingDays` window, default 30d) memories within each project map. Up to `ReplayMaxPerRun` replays per cycle (default 3, R12-scaled). Source memory IDs persist in `synthesis_source_ids` so the [R8] schema-from-replay path (`runPhase8SchemaFromReplay`) can cluster replays by shared sources and Phase 7 / Phase 8 see them in subsequent cycles. Replays land with `LightEncoded=false` for user review before standard recall.

**Synaptic alignment:** A memory system that just plays memories back is useful for retrieval. A system that *recombines* them produces new associations the user didn't explicitly form. This is where the "improvement" in "memory and improve memory and thought" actually happens — not in cleaner storage, but in the system catching connections the user missed.

### 7. Schapiro et al. — *Human hippocampal replay prioritizes weakly learned information* (2018)

**Schapiro, A.C., McDevitt, E.A., Rogers, T.T., Mednick, S.C. & Norman, K.A. (2018).** Human hippocampal replay during rest prioritizes weakly learned information and predicts memory performance. *Nature Communications*, **9**(1), 3920.

**Mechanism:** Hippocampal replay during rest preferentially reactivates *weak-but-tagged traces*, not strong ones — strong memories don't need the help.

**Mapped to:** Same secondary queue described in #5 (planned); weak ∧ salient outranks strong ∧ salient.

**Synaptic alignment:** The system spends compute where it matters. Replaying frequently-recalled memories wastes Tier 2 cycles; replaying weakly-encoded ones rescues knowledge that would otherwise drift away.

### 8. Lewis, Knoblich & Poe — *How Memory Replay in Sleep Boosts Creative Problem-Solving* (2018)

**Lewis, P.A., Knoblich, G. & Poe, G. (2018).** How memory replay in sleep boosts creative problem-solving. *Trends in Cognitive Sciences*, **22**(6), 491–503.

**Mechanism:** REM and non-REM sleep are iteratively interleaved in ~90-minute cycles with complementary functions. NREM stabilizes via replay; REM enables creative recombination via spontaneous reactivation of problem-related schemas alongside unrelated schemas. Connections "that make sense" are retained.

**Mapped to:** [R6] **shipped** — `nightly_pipeline.go` `runPhase7CrossRegion` writes two pools: pool A (cosine ≥ `cross_region_threshold` NN, existing) and pool B (cosine in [0.30, 0.50) AND ≥2 shared rare tags, where rare = tag in <5% of bank, computed by `computeRareTagSet`). Each pool capped at `cross_region_max_per_run/2`. Phase 8 schema-from-replay (R8) is still planned. The Stage 2 phase ordering (NREM-like → REM-like within a single run) follows the same alternation model and is in place today (`nightly_pipeline.go` Stage 2 step list).

**Synaptic alignment:** The phase ordering is not arbitrary scheduling — it's a deliberate model of how the brain alternates stabilization and recombination. Synaptic Disorder's nightly run is one cycle of this alternation; running multiple cycles per night would more faithfully mirror the biology, but one cycle is sufficient for the system's current scale.

---

## Synaptic tagging and capture (STC)

### 9. Ibrahim, Wang & Sajikumar — *Synapses tagged, memories kept* (2024)

**Ibrahim, M.Z.B., Wang, Z. & Sajikumar, S. (2024).** Synapses tagged, memories kept: Synaptic tagging and capture hypothesis in brain health and disease. *Philosophical Transactions of the Royal Society B*, **379**(1906), 20230237.

**Mechanism:** A weak experience leaves a transient tag; if a strong, plasticity-related event happens within a critical window, the tagged synapse captures the proteins and persists.

**Mapped to:** [R3 temporal_neighbor] **shipped** — `bank_p5.go` `MarkTemporalNeighborsDirty` runs async from `bank.SaveMemory` when computed `salience > 0.7`. Memories created within ±30 minutes of the anchor in the same region get re-marked dirty so the next nightly run revisits them as a cluster.

**Synaptic alignment:** A real brain doesn't process each memory in isolation — events that arrive close in time get tagged together because they're plausibly related. Synaptic Disorder respects this: a burst of related ingestion gets processed as a cluster, not as N separate items. This produces tighter synthesis output than processing each memory independently.

### 10. Moncada et al. — *Behavioral Tagging: A Translation of the STC Hypothesis* (2015)

**Moncada, D., Ballarini, F. & Viola, H. (2015).** Behavioral tagging: A translation of the synaptic tagging and capture hypothesis. *Neural Plasticity*, **2015**, 650780.

**Mechanism:** Behavioral version of STC — weak training becomes long-term memory if a salient event coincides nearby in time.

**Mapped to:** [R1] **shipped** — runtime computation lives in `salience.go` (`computeSalience` blends `computeEmotionalScore` × 0.4 + novelty × 0.4 + `computeRecencyScore` × 0.2). `bank.SaveMemory` calls it at insert (novelty defaults to 0.5 because the new row has no embedding yet); `bank_p5.go` `PersistDeepEncoding` re-derives it after every Phase 0b enrichment using `computeNoveltyForMemory` for true cosine-NN novelty. Column on `bank.go` `MemoryRecord.Salience`. Drives Phase 0b queue priority (R3 emotional/temporal_neighbor triggers), Phase 5 decay protection (planned, R2), and Phase 9 reinforcement candidate selection (planned, R9).

**Synaptic alignment:** The salience score is the system's single most-important signal — it drives queue ordering, decay protection, and reinforcement priority. Without it, every memory looks equally important and the pipeline can't be selective.

---

## Sleep-dependent transformation (gist + insight + creativity)

### 11. Lacaux et al. — *Sleep onset is a creative sweet spot* (2021)

**Lacaux, C., Andrillon, T., Bastoul, C., Idir, Y., Fonteix-Galet, A., Arnulf, I. & Oudiette, D. (2021).** Sleep onset is a creative sweet spot. *Science Advances*, **7**(50), eabj5866.

**Mechanism:** A brief period of N1 (sleep onset) fosters creative insight — the sudden discovery of a solution to a problem.

**Mapped to:** Stage 2 phase ordering — Phase 8 schema abstraction runs *after* Phase 10 replay, never before. Insight is a deferred product of accumulated overlapping-replay material, not a goal of any single phase. [R8] adds a direct path (`nightly_pipeline.go` `runPhase8SchemaFromReplay`) where Phase 8 consumes replays clustered by tag overlap (≥3 shared non-noise tags, ≥2 members) as a parallel input next to synthesis clusters; outputs carry `source='nightly_schema_from_replay'`. Together with R4's generative-mix replay rewrite, the conditions Lacaux describes (overlapping replay fragments converging into novel framings) are explicitly modelled. **Status grade: P (interpretive).** Lacaux 2021 demonstrates N1 sleep onset boosts creative insight on a single arithmetic task (n=103) — strong primary evidence for *insight-from-sleep-onset*. The bridge to "schema abstraction over clustered replays produces an analogous insight effect" is an architectural inference, not a direct empirical claim from Lacaux. The broader claim (sleep generates novel schema by overlapping prior content) is corroborated by Lewis et al. 2018 (#8) on REM-NREM iteration and the dual-process model in Diekelmann & Born 2010 (*Nat Rev Neurosci* 11:114–126), but the specific tag-overlap clustering rule is engineering, not biology.

**Synaptic alignment:** The system explicitly does not try to compute insight inline. It accumulates the conditions for insight — overlapping replay sequences — and then runs schema abstraction over the accumulated material. Insight is a side effect of the pipeline structure, not a goal of any single phase.

---

## Schema integration

### 12. Aghayan Golkashani et al. — *Advantage conferred by overnight sleep on schema-related memory may last only a day* (2023)

**Aghayan Golkashani, H., Ghorbani, S., Leong, R.L.F., Ong, J.L. & Chee, M.W.L. (2023).** Advantage conferred by overnight sleep on schema-related memory may last only a day. *SLEEP Advances*, **4**(1), zpad019.

**Mechanism:** Sleep's benefit to schema-related memory has a short window — hours-to-days — after which the advantage decays without continued reactivation.

**Mapped to:** Phase 8 schema refresh on a configurable cadence — schemas that haven't been reactivated within the refresh window get re-evaluated rather than persisting indefinitely.

**Synaptic alignment:** Schemas aren't permanent. Topics that were once dominant in the user's bank fade if the user stops engaging with them. Synaptic Disorder lets schemas naturally decay rather than treating them as immutable abstractions.

### 13. Ashton, Staresina & Cairney — *Sleep bolsters schematically incongruent memories* (2022)

**Ashton, J.E., Staresina, B.P. & Cairney, S.A. (2022).** Sleep bolsters schematically incongruent memories. *PLOS One*, **17**(7), e0269439.

**Mechanism:** Counter to the prediction that sleep mostly enhances schema-consistent memories: sleep also bolsters memories that violate the current schema. Surprise has its own consolidation pathway.

**Mapped to:** [R3 surprise] **shipped** — `bank_p5.go` `PersistDeepEncoding` checks the BEFORE state and re-marks the memory dirty with reason `surprise` when Tier 2 either changed the region OR produced a tag set with `tagDeltaFraction > 0.5` (Jaccard distance helper). The salience boost path through Phase 8 is still planned. **Status grade: P → F-leaning.** Ashton et al. 2022 is a single PLOS One study (n=99) on schema-incongruent word pairs; the broader "prediction-error / surprise gets its own consolidation pathway" claim is corroborated by the dopamine-driven novelty/surprise literature (Lisman & Grace 2005 on the hippocampal-VTA loop; Greve, Cooper & Henson 2019 on schema-incongruence and explicit memory). The Synaptic implementation is conservative — surprise just re-queues for re-enrichment rather than directly amplifying recall_strength — so it works under both strong and weak readings of the surprise pathway.

**Synaptic alignment:** Without surprise-handling, the bank tends to homogenize toward the schema and loses anomalies. Anomalies are often the most valuable signals — they're the user noticing something *new*. Bolstering them prevents the system from drowning out novelty.

---

## Emotional memory + selectivity at encoding

### 14. Payne & Kensinger — *Stress, sleep, and selective consolidation of emotional memories* (2018)

**Payne, J.D. & Kensinger, E.A. (2018).** Stress, sleep, and the selective consolidation of emotional memories. *Current Opinion in Behavioral Sciences*, **19**, 36–43.

**Mechanism:** Emotional salience and stress-related neuromodulators at *encoding time* tag memories for preferential reactivation. Selectivity is set during waking, not during sleep.

**Mapped to:** [R1 + R3 emotional] **shipped** — `salience.go` `computeEmotionalScore` is the runtime emotional-language scorer (exclamation marks, ALL-CAPS bursts, lexicon hits, tag-affect bonus); contributes 0.4× weight to the final salience. The `emotional` dirty-flag trigger fires from `bank_p5.go` `PersistDeepEncoding` when post-enrich salience > 0.8 — high-arousal memories automatically re-enter the queue.

**Synaptic alignment:** The system doesn't wait until consolidation to evaluate importance. Emotional weight is captured at ingestion and propagated through the entire pipeline. This matches how human brains operate: a stressful event gets re-thought-about for days, not just one night.

### 15. Hutchison & Rathore — *The role of REM sleep theta activity in emotional memory* (2015)

**Hutchison, I.C. & Rathore, S. (2015).** The role of REM sleep theta activity in emotional memory. *Frontiers in Psychology*, **6**, 1439.

**Mechanism:** REM theta is a prominent feature in hippocampus, amygdala, and neocortex during REM. Provides the neural signature for offline emotional memory consolidation.

**Mapped to:** [R5] **shipped** — `nightly_pipeline.go` `runPhase8_5SchemaReframing` runs after Phase 8 and before Phase 9. For each schema produced this run, it finds memories with cosine ≥ 0.7 to the schema's embedding AND ≥1 shared non-structural tag, then appends the schema's id to their `schema_source_ids` (text never rewritten — preserves R10 immutability). Skips sensitive + LocalConcept rows. Cap: `SchemaReframeMaxPerRun` (default 50). New stat `SchemaReframed` reports the count. **Status grade: P (interpretive).** Hutchison & Rathore 2015 establishes REM theta as the *neural signature* of emotional consolidation; the further claim that this signature corresponds to "schema reframing" (i.e., re-attaching prior memories to newly-discovered schemas) is an inference the citation itself does not make. The implementation never rewrites memory text — it only appends schema lineage IDs — so the engineering risk of being wrong is bounded. The phase-ordering claim (REM-equivalent phases after NREM-equivalent ones) is independently supported by Lewis et al. 2018 (#8) and the broader two-stage consolidation literature.

**Synaptic alignment:** Not just abstractly REM-flavored — the specific phases that handle emotional content are placed where the biology says they should be.

### 16. Nishida et al. — *REM Sleep, Prefrontal Theta, and the Consolidation of Human Emotional Memory* (2009)

**Nishida, M., Pearsall, J., Buckner, R.L. & Walker, M.P. (2009).** REM sleep, prefrontal theta, and the consolidation of human emotional memory. *Cerebral Cortex*, **19**(5), 1158–1166.

**Mechanism:** Prefrontal theta during REM correlates with overnight consolidation of emotional (but not neutral) memories — selective amplification.

**Mapped to:** Phase 9 reinforcement scales each strength delta by the memory's salience score, so emotional content receives disproportionate reinforcement compared to a flat per-memory bump.

**Synaptic alignment:** A flat reinforcement model bumps every recalled memory equally. The system's reinforcement is biased toward emotionally-weighted content because that's what biological reinforcement does — and it's what produces a memory system that surfaces what matters, not just what's recent.

---

## REM-specific mechanisms (and skeptical view)

### 17. Liu et al. — *Human REM sleep recalibrates neural activity in support of memory formation* (2023)

**Liu, S., Pikovsky, A., Cohen, M.X. et al. (2023).** Human REM sleep recalibrates neural activity in support of memory formation. *Science Advances*, **9**(34), eadj1895.

**Mechanism:** REM's effect on memory comes from non-oscillatory (aperiodic) brain activity rebalancing population dynamics — recalibration extent predicts consolidation success.

**Mapped to:** [R14] **shipped** — `nightly_pipeline.go` `applyGlobalStrengthRebalance(targetMean=1.5, targetStddev=0.7)` invoked at the end of `runPhase9Reinforcement`. Computes bank-wide `recall_strength` mean + stddev, then applies a soft squeeze: `blend = 0.9*current + 0.1*(targetMean + (current-actualMean)*(targetStddev/actualStddev))`. Caps preserved at [0.1, 5.0]. Early-exits when distribution is already near target. **Status grade: P (single primary study + interpretive bridge).** Liu et al. 2023 establishes that REM-driven aperiodic activity rebalances *neural population dynamics*; the implementation translates this to "rebalance the recall_strength distribution toward a target mean + stddev." This is a metaphorical mapping, not a 1:1 mechanism. The deeper claim — that consolidation actively preserves contrast in the strength distribution (not just push-up reinforcement) — is independently corroborated by Tononi & Cirelli 2014 (#4, SHY) which describes competitive down-selection. The 0.9/0.1 blend ratio + target values are engineering choices with no biological referent.

**Synaptic alignment:** Reinforcement isn't bookkeeping — it's about keeping the bank's recall-strength distribution well-shaped. A bank where everything is equally strong is functionally identical to one where everything is weak; Synaptic Disorder maintains useful contrast.

### 18. Johnson — *REM sleep and the development of context memory* (2005)

**Johnson, J.D. (2005).** REM sleep and the development of context memory. *Medical Hypotheses*, **64**(3), 499–504.

**Mechanism:** REM exists to build *context memory* — composite frameworks formed from many separate events in the same environment. During REM, noradrenergic + prefrontal activity drops while cholinergic activity stays high; events MERGE rather than retain individuality. The hippocampus integrates merged input into context memory, which projects back to neocortex during waking to provide a scaffold for new episodes. Bizarre dream content (bike→car merges, person-blends) is the visible signature of this mechanism, not noise.

**Mapped to:** Phase 8.7 Context Memory Formation (`nightly_phase8_7.go` `runPhase8_7ContextMemory`, R13). **Revised in v2.4.0b1 as a map-proposal pattern:** the AI never writes context-memory rows directly any more. For each region with ≥`ContextMemoryMinMemories` deep-encoded members and no fresh existing context within `ContextRefreshDays`, Tier 2 synthesizes a composite framework + self-reported `confidence` (low|medium|high) and the result is recorded as a `MemoryMap` row with `status='proposed'` and `generated_by='r13_redesign'`. The user reviews the proposal in the Maps → Proposed sub-tab and either Accepts (flips status to `accepted`), Edits (changes name/type/schema text before accepting), or Dismisses (status `dismissed`). This makes the user the source of authority on what becomes a context memory — the AI provides candidates, the user provides assent. [R15] (`augmentAndRetry`) still kicks in when confidence=low AND `ContextAugmentOnLowConfidence=true` AND augment_enabled: a Phase-6-shaped Tier 3 call enriches the region, the proposal regenerates once, and the proposed map carries `context_used_augment=true` + `oracle_augmented` tag. Default OFF (`context_memory_enabled=false`) because Johnson 2005 is hypothesis-grade. The user can also synthesize a context map on demand via `POST /maps/propose` (`source=tag|query|recall_id|auto`) — wired in the dashboard's "+ Propose map" affordance and the `sd_propose_map` MCP tool. Status grade: **P** (hypothesis-grade neuroscience; the engineering implementation is solid F-grade but the underlying claim that "REM exists to build context memory" is still contested).

**Synaptic alignment:** Context memories give the user an *operational understanding* of a topic distilled from many specific experiences — exactly what a human says when they tell you "I get how this works" without being able to cite specific instances. The v2.4.0b1 map-proposal redesign is a deliberate concession to user autonomy: AI-generated abstractions are surfaced as suggestions, never silently committed. Without this gate, the bank could end up populated with Tier-2-confabulated "this is what you think" rows the user never authorized. Without context memories at all, the bank stores facts; with them under user assent, the system has working knowledge that the user has explicitly endorsed.

### 19. Siegel — *The REM Sleep–Memory Consolidation Hypothesis* (2001)

**Siegel, J.M. (2001).** The REM sleep–memory consolidation hypothesis. *Science*, **294**(5544), 1058–1063.

**Mechanism:** Contrarian view. MAO inhibitors fully suppress REM with no memory deficit; intelligence doesn't correlate with REM duration; whales/dolphins have minimal REM despite high cognition. Argues REM's role in consolidation is overstated.

**Mapped to:** Architectural hedge — Phase 10 replay defaults off in backend settings (the contested REM-creativity claim is opt-in), while the NREM-equivalent phases (dedup, lexicon, maps, decay, cross-region) run unconditionally as the safe consolidation backbone.

**Synaptic alignment:** The architecture hedges against scientific debate. If the optimistic view of REM is wrong, Synaptic Disorder still works — the NREM-equivalent phases handle most of the load. If the optimistic view is right, the user can opt into the additional REM-equivalent phases. Default-off is the safe choice when the field hasn't settled.

### 20. Sarangi & Paital — *Association between REM sleep and strengthening memory* (2021)

**Sarangi, A. & Paital, B. (2021).** Association between REM sleep and strengthening memory: A mini review. *Journal of Clinical Images and Medical Case Reports*, **2**(6), 1451.

**Mechanism:** REM is most prevalent in infants (50% of sleep) and decreases with age. REM correlates with environmental novelty — periods of high learning demand have more REM.

**Mapped to:** Adaptive phase weighting (`nightly_pipeline.go` `computePhaseBudgetWeights` + `applyPhaseBudgetWeights`, R12). At the top of `runDreamPipeline`, after periodic-stale marking, the dirty queue's reason-distribution drives per-run budget allocation. ≥60% `created`/`never_encoded` → 1.4× `DeepEnrichMaxPerRun` + `DeepEnrichTokenBudget` (Phase 0b) and a 1.4× Phase 10 multiplier stored for R4 to consume. ≥60% `periodic` → 1.4× `CrossRegionMaxPerRun` + `SchemaMaxPerRun` (Phase 7 + Phase 8). Otherwise weights stay at 1.0 ("balanced"). The chosen weights surface in `stats.phase_budget_weights` so the dashboard can render "tonight: 1.4× Phase 0b". **Status grade: C (loose inspiration).** Sarangi & Paital 2021 is a mini review citing the long-known infant-REM observation; the link to a *per-run budget multiplier in a software pipeline* is biological *inspiration*, not implementation of a measured mechanism. The 60% threshold + 1.4× ratio are engineering choices selected to keep most nights "balanced" while still being responsive to ingestion bursts. The underlying behavior — adapt resource allocation to ingestion phase — is defensible on its own engineering merits and would be the right design even if the REM-novelty correlation didn't exist.

**Synaptic alignment:** A new bank (or a bank that just absorbed a major ingestion burst) has different needs than a mature one. The system adapts: more deep-enrichment when there's new material, more schema-building when the bank stabilizes. Mirrors the biological lifespan curve.

### 21. Liu et al. — *Slow-wave sleep and REM sleep differentially contribute to memory representational transformation* (2025)

**Liu, J., Chen, D., Xia, T., Zeng, S., Xue, G. & Hu, X. (2025).** Slow-wave sleep and REM sleep differentially contribute to memory representational transformation. *Communications Biology*, **8**, 1302.

**Mechanism:** Theta and beta power during REM positively associated with memory **representational transformations** — not just stabilization. SWS and REM contribute to different aspects of consolidation.

**Mapped to:** Phase 0b deep enrichment (`nightly_phase0b.go` `runPhase0bDeepEncoding`) is the transformation phase — explicitly re-summarizes, re-tags, and refines region in light of bank context, distinct from stabilization. The Tier 2 prompt asks for refined summaries that may differ from raw text (`buildDeepEnrichPrompt`), and the parser preserves the original when Tier 2 chooses no change (`parseDeepEnrichResponse`). Stage 2 ordering puts stabilization (Phase 1 dedup) before transformation (Phase 8 schema). Phase 8.7 context formation is planned.

**Synaptic alignment:** Distinguishes "the memory got saved" from "the memory got better understood." Synaptic Disorder cares about both; the architecture explicitly separates them.

---

## Forgetting and pruning

### 22. Poe — *Sleep is for Forgetting* (2017)

**Poe, G.R. (2017).** Sleep is for forgetting. *Journal of Neuroscience*, **37**(3), 464–473.

**Mechanism:** Active synaptic depression during NREM is a *purpose* of sleep, not a side effect. Pruning weak memories is computationally necessary to maintain learning capacity.

**Mapped to:** Phase 5 decay (`nightly_pipeline.go` `runPhase5Decay`) is treated as a primary phase that runs unconditionally each cycle, paired with Phase 9 reinforcement (`runPhase9Reinforcement`) which reads the same `recall_strength` column and applies the inverse direction. The current implementation is binary keep/drop; SHY-style weighted decay is planned.

**Synaptic alignment:** The system actively forgets. This is counterintuitive but essential — without forgetting, recall quality degrades as the bank scales. Synaptic Disorder commits to the controversial-but-correct view that sleep is for forgetting AS MUCH AS for remembering.

### 23. Genzel & Wixted — *Cellular and systems consolidation of declarative memory* (2019)

**Genzel, L. & Wixted, J.T. (2019).** Cellular and systems consolidation of declarative memory. *Frontiers in Cellular Neuroscience*, **13**, 71.

**Mechanism:** Sleep oscillations serve dual roles: protect important memories AND actively forget unimportant ones. Same nested-oscillation machinery does both simultaneously.

**Mapped to:** Phase 5 decay (`nightly_pipeline.go` `runPhase5Decay`) and Phase 9 reinforcement (`runPhase9Reinforcement`) pair operationally — both run every cycle and both manipulate the `recall_strength` column. The protection signal that integrates salience + schema fit + recall count is partial: the `salience` column is shipped (default 0.5), but the runtime computation that fills it with non-default values is planned.

**Synaptic alignment:** Decay and reinforcement aren't opposites — they're complements driven by the same underlying signal. The system's bookkeeping treats them as one mechanism with two directions, not two separate systems that might disagree.

---

## REM-specific recombination + bizarre dream content

### 24. Cai et al. — *REM, not incubation, improves creativity by priming associative networks* (2009)

**Cai, D.J., Mednick, S.A., Harrison, E.M., Kanady, J.C. & Mednick, S.C. (2009).** REM, not incubation, improves creativity by priming associative networks. *PNAS*, **106**(25), 10130–10134.

**Mechanism:** REM specifically improves Remote Associates Test performance — the benefit comes from forming links between previously *unassociated* items.

**Mapped to:**

- Phase 7 cross-region targets *low-similarity bridges* — moderate cosine distance + shared rare tags (planned)
- `nightly_pipeline.go` `findCrossRegionPairs()` uses two pools: high-similarity nearest-neighbors AND low-similarity surprising-bridge candidates

**Synaptic alignment:** A pure cosine-NN approach would only find the obvious connections — duplicates the user already knows are related. The surprising-bridge approach finds the connections the user hasn't noticed yet. This is where the system genuinely augments the user's thinking, not just organizes their notes.

### 25. Wamsley, Trost & Tucker — *Memory updating in dreams* (2024)

**Wamsley, E.J., Trost, T. & Tucker, M. (2024).** Memory updating in dreams. *SLEEP Advances*, **5**(1), zpae096.

**Mechanism:** Dreams update memory. Bizarre merging in dreams is interpreted as the visible signature of context memory formation — the system showing its work.

**Mapped to:** Phase 12 dream entry generation (`nightly_pipeline.go` `generateDreamEntry`) leans into the bizarre-merge framing. Stylistic archetypes (`dreamArchetypes` array — warehouse stocktake, library at night, etc., selected deterministically by run ID) shape each entry's voice. The Tier 2 prompt instructs "use the provided thought-fragments verbatim at least once; refer to topics obliquely or concretely; do NOT recite statistics" — explicitly tolerating surreal recombinations from the bank's actual content.

**Synaptic alignment:** The user's morning Dream Journal isn't a status report — it's a record of how the system *thought about* their bank overnight. The bizarre dream-style prose is biologically accurate AND useful: it gives the user a way to notice what the system found surprising or important without having to read raw stats.

---

## Goal alignment summary

Synaptic Disorder's mission: build a memory management system for AI that can manage and improve memory and thought like a real human brain does.

The studies above ground the architecture in empirical neuroscience, with each citation supporting a specific implementation pattern. The strength of that grounding varies — items marked **F (firm)** sit on well-replicated mechanisms; items marked **P (provisional)** or **C (loose inspiration)** are honest about the inferential leap between paper and code (see [§How citations are tied to the system](#how-citations-are-tied-to-the-system)). The key claim — that this system is biologically *informed* (not strictly faithful, and not merely metaphorical) — rests on three commitments traceable through the citations:

1. **Memory is graded, not flat.** The system maintains a continuous spectrum from episodic (specific, recent) to semantic (general, integrated) [Stickgold & Walker 2010, Tononi & Cirelli 2014]. Memories migrate along this axis as they're processed. The pipeline architecture supports this through Phase 0b deep enrichment and (planned) `consolidation_stage` transitions.

2. **Memory is processed, not stored.** Every memory gets revisited by Tier 2 in light of the bank's broader context [Lewis et al. 2018, Johnson 2005]. Re-processing is triggered by ingestion, edits, model upgrades, and time. The dirty-flag system and Phase 0b deep enrichment loop realise this.

3. **Memory improves through reorganization, not just accumulation.** The system actively forgets weak memories [Poe 2017], reinforces useful ones [Schapiro et al. 2018], synthesizes patterns [Lewis & Durrant 2011], builds context frameworks [Johnson 2005], and surfaces surprising connections [Cai et al. 2009]. The 12-phase nightly pipeline (Phases 1, 2, 5, 7, 8, 9, 10, 12 in particular) is the operational expression of this commitment.

The literature does not unanimously support every architectural decision. Where the field is contested (Siegel 2001 on REM), the system hedges via opt-in defaults. Where the field has converged (SHY, STC, replay), the system commits.

Together, the 25 papers above ground Synaptic Disorder's claim to be a memory management system that *mirrors* — to the extent the field permits — how human brains process memories during sleep. Where the literature has converged, the architecture commits firmly (R1 salience, R2 SHY decay, R4 generative replay, R7 consolidation stages, R9 weak-trace boost, R10 immutability, R11 TMR). Where it has not, the architecture hedges via opt-in defaults (R5 schema reframing, R8 schema-from-replay, R12 adaptive weighting, R13 context memory, R14 strength rebalance) and the Status grade annotations make the load-bearing vs decorative distinction legible to the reader. This is what makes the project an *engineering* system informed by neuroscience, not a neuroscience-themed UI sitting atop a generic vector store.

