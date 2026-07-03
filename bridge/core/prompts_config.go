// prompts_config.go — the canonical registry of every LLM prompt the
// system sends, plus the override-lookup primitive that future wire-up
// will use to make each prompt user-editable.
//
// Why this file exists:
//
//   1. Inventory. Until this file landed, the 17 distinct prompts were
//      scattered across nightly_*.go, handlers_*.go, bubble_generator.go,
//      classifier.go, and provider.go. Grepping for them required
//      knowing the exact build-function name. This file is the single
//      place to look.
//
//   2. Stable keys. Every prompt has a kebab-case key (e.g. "phase0b-deep-enrich",
//      "phase11-dream-narrative") that maps to its settings-table column
//      via `promptSettingKey(key) = "prompt." + key`. The sensitive
//      classifier already follows this shape — see SensitiveClassifierPromptKey
//      = "sensitive_classifier_prompt". We deliberately did NOT migrate
//      it into this registry to avoid disturbing a working, shipped feature
//      (the classifier's editable-prompt + hot-swap cache lives in
//      sensitive_classifier.go and stays there).
//
//   3. Future wire-up. When we add the Prompts config UI, each call site
//      (e.g. nightly_phase0b.go's `buildDeepEnrichPrompt`) will swap its
//      `const xPrompt = `...`` for `getPromptOverride(pc.bank, "phase0b-deep-enrich")`.
//      The default text returned matches today's hardcoded constant
//      verbatim — wire-up is one search-and-replace per call site, with
//      no semantic change until the user overrides via settings.
//
// What this file does NOT do (yet):
//
//   - It does NOT replace any existing constant or get called by any
//     existing prompt-building function. The intent here was "first,
//     create the prompts.config file." Wire-up is the follow-up.
//   - It does NOT load from an on-disk JSON/YAML file. The registry IS
//     the config — Go constants are the canonical source, and any
//     `prompts.config` file we publish elsewhere will be GENERATED from
//     this file (via `go run cmd/dump_prompts_config`, when we write it).
//   - It does NOT cache. The wire-up step will add a per-key cache with
//     PUT-handler invalidation, mirroring `invalidateSensitivePromptCache`
//     in sensitive_classifier.go.
//
// Naming conventions:
//
//   - Keys are kebab-case, prefixed by their phase (`phase0b-…`, `phase11-…`)
//     or call-site role (`map-propose`, `reflect`, `bubble-generate`).
//   - Settings-table column = `"prompt." + key`.
//   - Default text is verbatim what the current call site sends today —
//     so flipping a memory's prompt to its default behaves identically to
//     deleting the settings row.
package main

// PromptKey is a stable identifier for a prompt entry in the registry.
// New prompts MUST add their key here (so the registry doubles as the
// type-safe "what prompts exist" enum) and append their default to
// promptDefaults below.
type PromptKey string

const (
	// ── Tier 2 / Nightly pipeline ──────────────────────────────────────
	//
	// All Tier 2 prompts target the "Nightly" provider (Ollama llama3.1:8b
	// by default, or AirLLM/remote when so configured). Each runs once
	// per memory or per cluster, on the schedule.

	// PromptPhase0bDeepEnrich — Phase 0b deep-encoding. Refines summary,
	// tags, and brain-region anatomical placement for a single memory
	// using up to 5 neighbours as context. JSON output. Source:
	// nightly_phase0b.go buildDeepEnrichPrompt.
	PromptPhase0bDeepEnrich PromptKey = "phase0b-deep-enrich"

	// PromptPhase2Synthesis — Phase 2 cluster synthesis. Merges N related
	// observations into a 2-4 sentence summary. Source:
	// nightly_pipeline.go runPhase2 inline ~line 1188.
	PromptPhase2Synthesis PromptKey = "phase2-synthesis"

	// PromptPhase8Schema — Phase 8 schema synthesis. Identifies the
	// higher-order pattern across a set of recent Phase 2 outputs. 2-3
	// sentences. Source: nightly_pipeline.go runPhase8Schema inline ~2209.
	PromptPhase8Schema PromptKey = "phase8-schema"

	// PromptPhase8_5Reframe — Phase 8.5 schema reframing (R5). Finds the
	// higher-order pattern across narratives drawn from different time
	// periods within the same project. 2-3 sentences. Source:
	// nightly_pipeline.go runPhase8_5SchemaReframing inline ~2338.
	PromptPhase8_5Reframe PromptKey = "phase8.5-reframe"

	// PromptPhase8_7ContextMemory — Phase 8.7 context memory formation
	// (R13). Composite "what is this area about" framework for a region.
	// 4-8 sentences with self-reported confidence (low/medium/high).
	// JSON output. Source: nightly_phase8_7.go buildContextMemoryPrompt.
	PromptPhase8_7ContextMemory PromptKey = "phase8.7-context-memory"

	// PromptPhase10Replay — Phase 10 generative replay (R4). Weaves a
	// map's memories (recent + pre-existing) into a single coherent
	// narrative spanning time. 4-6 sentences. Source:
	// nightly_pipeline.go buildReplayPrompt.
	PromptPhase10Replay PromptKey = "phase10-replay"

	// PromptPhase11Narrative — Phase 11 dream narrative. Observational
	// 3-5 sentence "sleep researcher" voice describing what the brain
	// did overnight. Source: nightly_runner.go generateNarrative inline
	// ~line 778.
	PromptPhase11Narrative PromptKey = "phase11-narrative"

	// PromptPhase12DreamEntry — Phase 12 dream entry. Evocative prose,
	// 2-4 paragraphs, archetype-flavoured; uses thought-fragments
	// verbatim from the bubble pool. Source: nightly_pipeline.go
	// (or nightly_runner.go) generateDreamEntry inline ~3281.
	PromptPhase12DreamEntry PromptKey = "phase12-dream-entry"

	// ── Tier 3 / Oracle research surfaces ──────────────────────────────
	//
	// All Tier 3 prompts target the "Oracle" provider (typically a
	// remote API like OpenAI or Anthropic) and run on user-initiated
	// requests. Egress goes through PrepareOracleCall, which strips
	// sensitive memories before send.

	// PromptReflect — user-initiated reflection. Synthesises themes
	// across recalled memories to answer a question. Source:
	// handlers_reflect.go buildReflectPrompt.
	PromptReflect PromptKey = "reflect"

	// PromptResearch — general /research endpoint. Open-ended query,
	// memory-grounded. Source: handlers_research.go buildResearchPrompt.
	PromptResearch PromptKey = "research"

	// PromptMapResearch — map-scoped Tier 3 research that feeds back as
	// a manual_augment memory. Source: handlers_map_research.go
	// buildMapResearchPrompt.
	PromptMapResearch PromptKey = "map-research"

	// PromptMapPropose — proposes a new MemoryMap (name, anchor tags,
	// schema) from a memory set. JSON output. Source:
	// handlers_maps_propose.go buildProposePrompt.
	PromptMapPropose PromptKey = "map-propose"

	// PromptAugmentConcept / Technology / Topic — Phase 6 augmentation
	// (auto-Oracle for thin maps). Three category-specific templates
	// so spend is predictable and an extra LLM call isn't needed just
	// to generate the prompt. Source: handlers_augmentation.go
	// canonicalAugmentQuery switch.
	PromptAugmentConcept    PromptKey = "augment-concept"
	PromptAugmentTechnology PromptKey = "augment-technology"
	PromptAugmentTopic      PromptKey = "augment-topic"

	// ── Tier 1 / Realtime + ambient ────────────────────────────────────
	//
	// Tier 1 runs locally on the always-on Ollama container. These fire
	// on every memory ingest (region classifier) or on the bubble
	// generator's 30-second cadence.

	// PromptRegionClassifier — classify a new memory into one of 14
	// brain regions. Output is just the region key. Source:
	// classifier.go classifySystemPrompt.
	PromptRegionClassifier PromptKey = "region-classifier"

	// PromptBubbleGenerate — produce one short (4-12 word) inner-
	// monologue thought for the dashboard's thought-bubble ambient
	// engine. Source: bubble_generator.go defaultGeneratePrompt.
	PromptBubbleGenerate PromptKey = "bubble-generate"

	// PromptBubbleSimilarity — duplicate-check a candidate bubble
	// against the existing bubble pool before persisting. Source:
	// bubble_generator.go defaultSimilarityPrompt.
	PromptBubbleSimilarity PromptKey = "bubble-similarity"

	// PromptBubbleMutate — rewrite an existing bubble through a
	// cognitive-mode lens (Diagnosed mode etc.). Source:
	// bubble_generator.go defaultMutatePrompt.
	PromptBubbleMutate PromptKey = "bubble-mutate"

	// ── Cross-cutting ──────────────────────────────────────────────────

	// PromptRemoteSystem — system-message prepended to every Tier 3 /
	// remote-provider Chat call. Sets non-negotiable data-handling
	// rules ("don't echo verbatim, don't retain, refuse if sensitive
	// leaked through"). Source: provider.go DefaultRemoteSystemPrompt.
	PromptRemoteSystem PromptKey = "remote-system"

	// PromptBenchmark — one-shot text used by /admin/llm/benchmark for
	// tokens/sec measurement. Editing this changes nothing about the
	// model's behaviour; it's just the sample input. Source:
	// handlers_admin_llm.go benchmarkPromptDefault.
	PromptBenchmark PromptKey = "benchmark"
)

// promptDefaults is the canonical "prompts.config" — every key's default
// text, in one place, in source-of-truth form.
//
// IMPORTANT: each value here MUST stay byte-identical to the constant
// the corresponding call site sends today. Wire-up will replace each
// call-site constant with `getPromptOverride(bank, <key>)`; if the
// default here drifts from what the call site used to send, behaviour
// silently changes when users haven't set an override. Tests will be
// added at wire-up time to assert default == call-site-constant.
//
// To regenerate the user-facing `prompts.config` doc (when we add it):
// `go run ./cmd/dump_prompts_config > prompts.config`.
var promptDefaults = map[PromptKey]string{
	// The defaults are intentionally NOT inlined here — they live next
	// to their call sites (e.g., classifyPrompt in classifier.go,
	// defaultGeneratePrompt in bubble_generator.go,
	// DefaultRemoteSystemPrompt in provider.go, the inline-Builder
	// prompts in nightly_pipeline.go, etc.). Wire-up will move them
	// here in a single pass; until then this map is empty and
	// getPromptOverride() falls through to "" → callers keep using
	// their existing const literals.
	//
	// This file is the COMMITMENT — the keys are stable and the
	// inventory is complete; the strings move on the wire-up turn.
}

// promptSettingKey returns the settings-table column used to store a
// user's override for a given prompt. Matches the
// SensitiveClassifierPromptKey convention but namespaced under "prompt.".
//
// Example: PromptPhase0bDeepEnrich → "prompt.phase0b-deep-enrich"
func promptSettingKey(key PromptKey) string {
	return "prompt." + string(key)
}

// getPromptOverride looks up the user's override for a prompt key from
// the settings table. Returns the override when present (non-empty
// string), else "" — callers should treat "" as "use the default" and
// fall through to their existing const.
//
// Hot-swap is NOT wired here. Today this function is best-effort: a
// fresh read on every call. When we wire prompts up at the call sites,
// we'll add a per-key cache (TTL ~10s) + an invalidate-on-PUT hook,
// same pattern as invalidateSensitivePromptCache.
//
// Safety: returns "" on any error (bank nil, settings table missing,
// JSON parse error, etc.). The intent is that an unreachable bank
// NEVER causes a prompt to go missing — the call site's hardcoded
// default keeps working.
func getPromptOverride(b *Bank, key PromptKey) string {
	if b == nil {
		return ""
	}
	val, _, err := b.GetSetting(promptSettingKey(key))
	if err != nil || val == "" {
		return ""
	}
	return val
}

// allPromptKeys is the registered enum of every prompt the system
// sends. Used by the future Prompts config UI to render one row per
// prompt with a textarea for the override. Order matters for UI
// presentation — tier 2 first (most impactful), then tier 3, then
// tier 1, then cross-cutting.
func allPromptKeys() []PromptKey {
	return []PromptKey{
		// Tier 2 — nightly pipeline
		PromptPhase0bDeepEnrich,
		PromptPhase2Synthesis,
		PromptPhase8Schema,
		PromptPhase8_5Reframe,
		PromptPhase8_7ContextMemory,
		PromptPhase10Replay,
		PromptPhase11Narrative,
		PromptPhase12DreamEntry,
		// Tier 3 — Oracle research
		PromptReflect,
		PromptResearch,
		PromptMapResearch,
		PromptMapPropose,
		PromptAugmentConcept,
		PromptAugmentTechnology,
		PromptAugmentTopic,
		// Tier 1 — realtime + ambient
		PromptRegionClassifier,
		PromptBubbleGenerate,
		PromptBubbleSimilarity,
		PromptBubbleMutate,
		// Cross-cutting
		PromptRemoteSystem,
		PromptBenchmark,
	}
}
