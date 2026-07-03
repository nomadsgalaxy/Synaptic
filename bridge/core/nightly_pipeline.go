// nightly_pipeline.go — Dream Cycle consolidation pipeline (12 phases).
//
// Replaces NightlyRunner.runStepsFn's default no-op with a real REM-style
// reorganisation pass over the bank's memories. See
// `docs/dev/CODE_HANDOFF — dream cycle consolidation pipeline (2026-05-09).md`
// for the conceptual model; this file is the implementation.
//
// Phase ordering:
//
//   0  Light Encoding         (foundation; only fatal phase)
//   1  Deduplication           cosine > 0.95 → merge
//   2  Synthesis (Tier 2)      cluster summarisation
//   3  Lexicon Rebuild
//   4  Map Updates             emergence + decay
//   5  Decay / Pruning
//   6  Research Augmentation   (defers to handlers_augmentation.go)
//   7  Cross-region Association
//   8  Schema Abstraction (Tier 2)
//   9  Recall-Weighted Reinforcement
//  10  Replay (Tier 2, gated)
//  11  Narrative Generation    (Tier 2; observational voice)
//  12  Dream Entry             (Tier 2; literary prose)
//
// Each phase function takes `*pipelineContext` and returns an error. The
// orchestrator catches per-phase errors and appends them to
// stats.PhaseFailures rather than aborting the run; the only fatal phase
// is Phase 0 (no encoded inputs → nothing to do downstream).
//
// Privacy invariants (load-bearing):
//   - Sensitive memories are never sent to Tier 2/3 (they only appear in
//     local-only phases: dedup, decay, cross-region linking).
//   - LocalConcept-tagged memories are excluded from synthesis (Phase 2),
//     schema-building (Phase 8), and replay (Phase 10).
//   - Augmentation honours the existing strict category gate
//     (project / entity / personal HARD-skipped — see handlers_augmentation.go).
package main

import (
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// osReadFileNilSafe wraps os.ReadFile to return (nil, err) on errors so the
// nightly pipeline's bubble-quote collector can short-circuit cleanly when
// the pool file isn't present yet (fresh installs, tests, etc.).
func osReadFileNilSafe(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// settingsDreamPipeline is the typed GET/PUT handler for the 15
// dream-pipeline knobs. Defined in this file so the type and its keys
// stay co-located with the pipeline that consumes them.
func (s *Server) settingsDreamPipeline(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		st, err := s.bank.GetDreamPipelineSettings()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, st)
	case http.MethodPut:
		var st DreamPipelineSettings
		if err := readJSON(r, &st); err != nil {
			http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.bank.SaveDreamPipelineSettings(st); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.auditWrite(AuditEntry{
			Operation: "upsert", EntityType: "setting", EntityID: "dream_pipeline",
			AfterJSON: mustJSON(st), AdapterID: adapterIDFromRequest(r),
		})
		writeJSON(w, http.StatusOK, st)
	default:
		methodNotAllowed(w, "GET", "PUT", "OPTIONS")
	}
}

// pipelineContext is the per-run state threaded through each phase.
type pipelineContext struct {
	ctx      context.Context
	runID    string
	bank     *Bank
	router   *ModelRouter
	hub      *Hub
	now      time.Time
	settings DreamPipelineSettings

	// Mutated as phases run; merged into the run's stats blob at the end.
	stats         *NightlyStats
	lastRunFinish time.Time // baseline for "since last cycle" (Phase 9 reinforcement)

	// Optional bubble-generator hook so Phase 1/2/6/8 can enqueue refresh.
	bubbleEnqueue func(memID string)

	// Wave 8e — lifecycle manager handle. Used to bump last_used_at
	// before each LLM Chat call so the idle reaper doesn't stop an
	// on-demand-lazy sidecar (esp. AirLLM tier2) mid-inference. Nil-safe
	// — when unset, the SignalProviderUse helper is a no-op.
	lifecycle *LifecycleManager

	// 2026-05-23 (Feature #6) — back-reference to the owning NightlyRunner
	// so emitProgress can persist the current phase name + start time on
	// the runner's atomic fields. Read by /nightly/runs to populate the
	// in_progress row's current_phase + phase_started_at + phase_elapsed_ms
	// fields. Nil-safe (handlers_admin_deep_encode construction sets nil
	// because that path runs outside the normal nightly runner).
	runner *NightlyRunner

	// LocalConcepts is the set of terms that must NEVER reach Tier 3.
	localConcepts map[string]bool

	// Snapshot of lexicon pairs taken before Phase 3 — used to compute
	// added/removed deltas.
	lexiconBefore map[string]bool

	// Snapshot of map_keys that had >= MapMinMemories before Phase 4 —
	// used to detect emergence vs. pre-existing maps.
	mapKeysBefore map[string]bool

	// Phase 9 needs to know which memories were recalled since the last
	// run; capture once at run start so concurrent recalls during the
	// run don't get triple-counted.
	recalledThisCycle map[string]int
}

// DreamPipelineSettings is the typed view over the dream-pipeline knobs in
// the generic settings table. Defaults match the handoff exactly.
type DreamPipelineSettings struct {
	DedupThreshold       float64 `json:"dedup_threshold"`
	// DedupTagOverlapPct is the minimum tag-overlap (Jaccard, 0..100)
	// required IN ADDITION to DedupThreshold before Phase 1 merges a pair.
	// Mirrors the inline-dedup guard (auto_dedup_tag_overlap_pct) so a
	// degenerate/low-spread embedding model can't merge topically-unrelated
	// memories that happen to land in the same functional region group.
	// Default 50 — permissive enough to keep paraphrased near-duplicates
	// (which may carry slightly different tag sets) while structurally
	// blocking cross-topic merges (which have ~0 tag overlap).
	DedupTagOverlapPct   int     `json:"dedup_tag_overlap_pct"`
	SynthesisMaxPerRun   int     `json:"synthesis_max_per_run"`
	SynthesisClusterMin  int     `json:"synthesis_cluster_min"`
	SynthesisClusterSim  float64 `json:"synthesis_cluster_sim"`
	DecayAgeDays         int     `json:"decay_age_days"`
	DecayRecallDays      int     `json:"decay_recall_days"`
	DecayMaxPerRun       int     `json:"decay_max_per_run"`
	MapMinMemories       int     `json:"map_min_memories"`
	CrossRegionThreshold float64 `json:"cross_region_threshold"`
	CrossRegionMaxPerRun int     `json:"cross_region_max_per_run"`
	SchemaMaxPerRun      int     `json:"schema_max_per_run"`
	SchemaLookbackDays   int     `json:"schema_lookback_days"`
	SchemaClusterSim     float64 `json:"schema_cluster_sim"`
	ReinforceDecayFactor float64 `json:"reinforcement_decay_factor"`
	ReplayEnabled        bool    `json:"replay_enabled"`
	// [R4] Per-run cap on Phase 10 replays. Default 3. R12's Phase10 weight
	// multiplies this at the top of runDreamPipeline when the queue is
	// created-dominant. CITATIONS.md #6 (Buzsáki 2015 SPW-Rs are generative).
	ReplayMaxPerRun int `json:"replay_max_per_run"`
	// [R4] Recency horizon for the "recent" pool: memories created within
	// this many days. Default 7.
	ReplayRecentDays int `json:"replay_recent_days"`
	// [R4] Pre-existing horizon for the consolidation pool: memories
	// created earlier than this many days ago. Default 30.
	ReplayPreexistingDays int `json:"replay_preexisting_days"`
	// Phase 0b deep-encoding (handoff "deep per-memory enrichment 2026-05-10"):
	DeepEnrichEnabled     bool `json:"deep_enrich_enabled"`
	DeepEnrichMaxPerRun   int  `json:"deep_enrich_max_per_run"`
	DeepEnrichTokenBudget int  `json:"deep_enrich_token_budget_per_run"`
	DeepEnrichConcurrency int  `json:"deep_enrich_concurrency"`
	// [R3 periodic] CITATIONS.md #2 — bank context drifts over weeks/months;
	// re-enrich rows whose deep_encoded_at is older than this threshold so
	// yesterday's summary stays close to today's understanding. 0 disables.
	DeepEnrichRefreshDays int `json:"deep_enrich_refresh_days"`
	// [R2] CITATIONS.md #4 SHY weighted decay knobs.
	// DecayBaseRate: max per-cycle bleed of an unprotected memory's strength
	//                (default 0.05 = 5%; a fully-unprotected row decays to
	//                ~50% in ~14 cycles, ~10% in ~45 cycles).
	// DecayStrengthFloor: prune-eligibility threshold; memories whose
	//                     post-decay strength drops below this are
	//                     candidates for soft-delete (subject to existing
	//                     age + recall + tag predicates).
	DecayBaseRate      float64 `json:"decay_base_rate"`
	DecayStrengthFloor float64 `json:"decay_strength_floor"`
	// [R5] Phase 8.5 schema reframing cap (default 50). 0 disables.
	SchemaReframeMaxPerRun int `json:"schema_reframe_max_per_run"`
	// [R13] Phase 8.7 Context Memory Formation. CITATIONS.md #18 (Johnson
	// 2005 REM context memory). Default OFF — this is a hypothesis-grade
	// phase that synthesises a "what this region is about" composite
	// framework. ContextAugmentOnLowConfidence enables [R15]: when Tier 2
	// reports low confidence on an eligible region, trigger Phase-6-style
	// augmentation for that region and re-run the Tier 2 call once.
	ContextMemoryEnabled          bool `json:"context_memory_enabled"`
	ContextMemoryMinMemories      int  `json:"context_memory_min_memories"`
	ContextMemoryMaxPerRun        int  `json:"context_memory_max_per_run"`
	ContextRefreshDays            int  `json:"context_refresh_days"`
	ContextAugmentOnLowConfidence bool `json:"context_augment_on_low_confidence"`
	// Wave 8e — per-LLM-call timeout (in seconds), governing every
	// `context.WithTimeout` that wraps a Chat call in the nightly
	// pipeline (Phase 0b deep encode, Phase 2 synthesis, Phase 6
	// augment, Phase 8 schema, Phase 8.7 context memory, Phase 10
	// replay, dream-entry narrative, etc.). Embedding calls retain
	// their own 30s timeout — this knob is for inference only.
	//
	// Default 120s suits Ollama's sub-second-to-few-second throughput.
	// When Tier 2's provider kind is `airllm` AND this setting is at
	// its zero value (user hasn't customised), GetDreamPipelineSettings
	// auto-bumps the returned value to 1800 (30 min) so AirLLM's
	// 1-3 tok/s rate doesn't guillotine inference mid-call. User-set
	// values (anything non-zero in the settings table) are respected
	// verbatim.
	LLMCallTimeoutSec int `json:"llm_call_timeout_sec"`

	// ConceptMapsAutoCreate gates Phase 4's auto-creation of `type=concept`
	// MemoryMap rows. Previously every tag with count >= MapMinMemories
	// was auto-upserted as a concept map — produced thousands of low-signal
	// rows the user had to manually prune. With this set to `false`
	// (default), Phase 4 instead emits PROPOSED maps for tags meeting
	// MapProposalMinTraces (see below); the user accepts/edits/dismisses
	// via the R13 proposal flow.
	//
	// `true` restores the legacy Phase-4-auto-creates-concept-maps
	// behaviour. Existing concept maps are NOT touched by this flag —
	// to clear them, call POST /maps/cleanup with
	// {delete_all_matching: true, only_type: "concept"}.
	ConceptMapsAutoCreate bool `json:"concept_maps_auto_create"`

	// MapProposalMinTraces is the minimum number of memories sharing a tag
	// before Phase 4 will propose a MemoryMap for it. Default 10 — chosen
	// to filter out incidental tag pairs while still surfacing emerging
	// themes. The user can tune up (fewer, stronger proposals) or down
	// (more proposals to review). 0 disables the proposal path entirely.
	//
	// Independent of MapMinMemories, which still governs Phase 4's
	// metadata-refresh path for existing non-concept maps (project,
	// person, technology, organization) — those keep their lower-bound
	// threshold so a person/project that drops below 2 memories still
	// gets archived.
	MapProposalMinTraces int `json:"map_proposal_min_traces"`

	// DreamsPerNightMax caps how many dream cycles the auto-loop will
	// fire during a single nightly window. Default 0 means "as many as
	// fit in the window" — the auto-loop calibrates from each completed
	// cycle's duration and only fires the next one when the remaining
	// window time is wider than the running average (with a 10% safety
	// margin), so the second-to-last cycle never half-finishes.
	//
	// Use case: a user with a 4-hour window and an 8-minute typical
	// cycle can drain ~26 cycles per night unattended. The first cycle
	// is the "calibration" — auto-fired even when the window time-budget
	// is tight, so a new install or schedule change starts learning its
	// cadence immediately. Subsequent cycles obey the average + cap.
	//
	// Set to N to cap at N cycles even when more would fit. 1 restores
	// the legacy "one cycle per night" behaviour.
	DreamsPerNightMax int `json:"dreams_per_night_max"`
}

// DefaultDreamPipelineSettings returns the safe-default knobs (handoff §Settings).
func DefaultDreamPipelineSettings() DreamPipelineSettings {
	return DreamPipelineSettings{
		DedupThreshold:       0.92, // lowered from 0.95 (handoff "dream pipeline tuning 2026-05-10") — paraphrased near-duplicates were being missed
		DedupTagOverlapPct:   50,   // tag-overlap guard mirroring inline dedup — blocks cross-topic merges from a degenerate embedding model
		SynthesisMaxPerRun:   5,
		SynthesisClusterMin:  3,
		SynthesisClusterSim:  0.7,
		DecayAgeDays:         180, // lowered from 365 (handoff "dream pipeline tuning 2026-05-10") — gives a young bank a meaningful prune horizon
		DecayRecallDays:      90,
		DecayMaxPerRun:       50,
		MapMinMemories:       2,
		CrossRegionThreshold: 0.78,
		CrossRegionMaxPerRun: 20,
		SchemaMaxPerRun:      2,
		SchemaLookbackDays:   30,
		SchemaClusterSim:     0.65,
		ReinforceDecayFactor: 0.99,
		ReplayEnabled:        false,
		// [R4] defaults; ignored when ReplayEnabled=false.
		ReplayMaxPerRun:       3,
		ReplayRecentDays:      7,
		ReplayPreexistingDays: 30,
		// Phase 0b defaults (deep per-memory enrichment 2026-05-10):
		// On by default — that's the user's stated goal ("dream sequence
		// should parse every memory through the Tier 2 model"). Caps are
		// sized so a typical desktop completes the per-run work in <1h.
		DeepEnrichEnabled:     true,
		DeepEnrichMaxPerRun:   100,
		DeepEnrichTokenBudget: 50000,
		DeepEnrichConcurrency: 1,
		DeepEnrichRefreshDays: 90,    // quarterly refresh per handoff §Periodic; 0 disables
		DecayBaseRate:          0.05, // [R2] 5% per-cycle bleed for unprotected memories
		DecayStrengthFloor:     0.05, // [R2] prune candidates fall below this strength
		SchemaReframeMaxPerRun: 50,   // [R5] cap the per-schema lineage propagation
		// [R13] Phase 8.7 defaults — feature-flagged OFF; Johnson 2005 is
		// hypothesis-grade.
		ContextMemoryEnabled:          false,
		ContextMemoryMinMemories:      20,
		ContextMemoryMaxPerRun:        3,
		ContextRefreshDays:            60,
		// Wave 8e — 120s covers Ollama's typical inference. Provider-
		// aware bump to 1800 happens in GetDreamPipelineSettings when
		// Tier 2 is AirLLM and this stayed at the unset default.
		LLMCallTimeoutSec: 120,
		ContextAugmentOnLowConfidence: true,
		// Concept maps: auto-creation OFF by default — Phase 4 now
		// emits proposals for the user to review. Existing concept maps
		// must be cleared via POST /maps/cleanup {delete_all_matching,
		// only_type: "concept"} on upgrade.
		ConceptMapsAutoCreate: false,
		MapProposalMinTraces:  10,
		// 0 = unbounded within the window; auto-loop runs as many cycles
		// as fit. User can cap explicitly via Config → Pipeline.
		DreamsPerNightMax: 0,
	}
}

// computePhaseBudgetWeights derives R12's per-phase multipliers from the
// dirty-queue distribution. CITATIONS.md #20 (Sarangi 2021 — REM
// proportional to environmental novelty):
//
//   - "created" (or never_encoded) ≥60% of the queue → 1.4× Phase 0b token
//     budget + max-per-run, AND 1.4× Phase 10 replay budget. New material
//     to integrate; bias the run toward intake + speculative recombination.
//   - "periodic" ≥60% of the queue → 1.4× Phase 7 cross-region + 1.4×
//     Phase 8 schema. Stable bank, time to look for patterns.
//   - else → all weights 1.0 ("balanced").
//
// Pure function, no I/O — covered by phase_budget_weights_test.go.
func computePhaseBudgetWeights(total int, byReason map[string]int) PhaseBudgetWeights {
	w := PhaseBudgetWeights{Phase0b: 1.0, Phase7: 1.0, Phase8: 1.0, Phase10: 1.0, Reason: "balanced"}
	if total <= 0 {
		return w
	}
	// "never_encoded" is the synthetic bucket CountDirtyMemories emits for
	// rows that have no prior deep_encoded_at AND no dirty_reason (cold-
	// start before R3 'created' triggers shipped). Treat it as created for
	// weighting purposes — both signal "fresh material".
	created := byReason["created"] + byReason["never_encoded"]
	periodic := byReason["periodic"]
	createdFrac := float64(created) / float64(total)
	periodicFrac := float64(periodic) / float64(total)
	const boost = 1.4
	const dominantThreshold = 0.6
	switch {
	case createdFrac >= dominantThreshold && createdFrac >= periodicFrac:
		w.Phase0b = boost
		w.Phase10 = boost
		w.Reason = "created_dominant"
	case periodicFrac >= dominantThreshold && periodicFrac > createdFrac:
		w.Phase7 = boost
		w.Phase8 = boost
		w.Reason = "periodic_dominant"
	}
	return w
}

// applyPhaseBudgetWeights multiplies the relevant per-phase caps + token
// budgets in pc.settings by the R12 weights.
func (pc *pipelineContext) applyPhaseBudgetWeights(w PhaseBudgetWeights) {
	if pc == nil {
		return
	}
	if w.Phase0b != 1.0 {
		pc.settings.DeepEnrichMaxPerRun = int(float64(pc.settings.DeepEnrichMaxPerRun) * w.Phase0b)
		pc.settings.DeepEnrichTokenBudget = int(float64(pc.settings.DeepEnrichTokenBudget) * w.Phase0b)
	}
	if w.Phase7 != 1.0 {
		pc.settings.CrossRegionMaxPerRun = int(float64(pc.settings.CrossRegionMaxPerRun) * w.Phase7)
	}
	if w.Phase8 != 1.0 {
		pc.settings.SchemaMaxPerRun = int(float64(pc.settings.SchemaMaxPerRun) * w.Phase8)
	}
	// [R4] Phase 10 generative replay cap. R12 boosts this when the queue
	// is created-dominant ("new material to integrate").
	if w.Phase10 != 1.0 {
		pc.settings.ReplayMaxPerRun = int(float64(pc.settings.ReplayMaxPerRun) * w.Phase10)
	}
}

// settings keys for the pipeline (closed enum so a typo doesn't silently
// drop a knob).
const (
	keyDedupThreshold       = "dedup_threshold"
	keyDedupTagOverlapPct   = "dedup_tag_overlap_pct"
	keySynthesisMaxPerRun   = "synthesis_max_per_run"
	keySynthesisClusterMin  = "synthesis_cluster_min"
	keySynthesisClusterSim  = "synthesis_cluster_sim"
	keyDecayAgeDays         = "decay_age_days"
	keyDecayRecallDays      = "decay_recall_days"
	keyDecayMaxPerRun       = "decay_max_per_run"
	keyMapMinMemories       = "map_min_memories"
	keyCrossRegionThreshold = "cross_region_threshold"
	keyCrossRegionMaxPerRun = "cross_region_max_per_run"
	keySchemaMaxPerRun      = "schema_max_per_run"
	keySchemaLookbackDays   = "schema_lookback_days"
	keySchemaClusterSim     = "schema_cluster_sim"
	keyReinforceDecayFactor = "reinforcement_decay_factor"
	keyReplayEnabled        = "replay_enabled"
	keyReplayMaxPerRun       = "replay_max_per_run"
	keyReplayRecentDays      = "replay_recent_days"
	keyReplayPreexistingDays = "replay_preexisting_days"
	// Phase 0b knobs (handoff "deep per-memory enrichment 2026-05-10"):
	keyDeepEnrichEnabled    = "deep_enrich_enabled"
	keyDeepEnrichMaxPerRun  = "deep_enrich_max_per_run"
	keyDeepEnrichTokenBudg  = "deep_enrich_token_budget_per_run"
	keyDeepEnrichConcur     = "deep_enrich_concurrency"
	keyDeepEnrichRefreshDays = "deep_enrich_refresh_days"
	keyDecayBaseRate           = "decay_base_rate"
	keyDecayStrengthFloor      = "decay_strength_floor"
	keySchemaReframeMaxPerRun  = "schema_reframe_max_per_run"
	keyContextMemoryEnabled          = "context_memory_enabled"
	keyContextMemoryMinMemories      = "context_memory_min_memories"
	keyContextMemoryMaxPerRun        = "context_memory_max_per_run"
	keyContextRefreshDays            = "context_refresh_days"
	keyContextAugmentOnLowConfidence = "context_augment_on_low_confidence"
	keyLLMCallTimeoutSec             = "llm_call_timeout_sec"
	// Concept-maps takeover (the user requested switching map creation
	// from Phase-4-auto-concept-maps to the R13 proposal flow).
	keyConceptMapsAutoCreate = "concept_maps_auto_create"
	keyMapProposalMinTraces  = "map_proposal_min_traces"
	keyDreamsPerNightMax     = "dreams_per_night_max"
)

// GetDreamPipelineSettings reads the 15 dream-pipeline keys from the
// generic settings table, falling back to defaults for any missing field.
func (b *Bank) GetDreamPipelineSettings() (DreamPipelineSettings, error) {
	s := DefaultDreamPipelineSettings()
	if b == nil {
		return s, nil
	}
	getF := func(k string, dst *float64) {
		if v, ok, _ := b.GetSetting(k); ok {
			var f float64
			if _, err := fmt.Sscanf(v, "%f", &f); err == nil {
				*dst = f
			}
		}
	}
	getI := func(k string, dst *int) {
		if v, ok, _ := b.GetSetting(k); ok {
			var n int
			if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
				*dst = n
			}
		}
	}
	getB := func(k string, dst *bool) {
		if v, ok, _ := b.GetSetting(k); ok {
			*dst = v == "true" || v == "1"
		}
	}
	getF(keyDedupThreshold, &s.DedupThreshold)
	getI(keyDedupTagOverlapPct, &s.DedupTagOverlapPct)
	getI(keySynthesisMaxPerRun, &s.SynthesisMaxPerRun)
	getI(keySynthesisClusterMin, &s.SynthesisClusterMin)
	getF(keySynthesisClusterSim, &s.SynthesisClusterSim)
	getI(keyDecayAgeDays, &s.DecayAgeDays)
	getI(keyDecayRecallDays, &s.DecayRecallDays)
	getI(keyDecayMaxPerRun, &s.DecayMaxPerRun)
	getI(keyMapMinMemories, &s.MapMinMemories)
	getF(keyCrossRegionThreshold, &s.CrossRegionThreshold)
	getI(keyCrossRegionMaxPerRun, &s.CrossRegionMaxPerRun)
	getI(keySchemaMaxPerRun, &s.SchemaMaxPerRun)
	getI(keySchemaLookbackDays, &s.SchemaLookbackDays)
	getF(keySchemaClusterSim, &s.SchemaClusterSim)
	getF(keyReinforceDecayFactor, &s.ReinforceDecayFactor)
	getB(keyReplayEnabled, &s.ReplayEnabled)
	getI(keyReplayMaxPerRun, &s.ReplayMaxPerRun)
	getI(keyReplayRecentDays, &s.ReplayRecentDays)
	getI(keyReplayPreexistingDays, &s.ReplayPreexistingDays)
	getB(keyDeepEnrichEnabled, &s.DeepEnrichEnabled)
	getI(keyDeepEnrichMaxPerRun, &s.DeepEnrichMaxPerRun)
	getI(keyDeepEnrichTokenBudg, &s.DeepEnrichTokenBudget)
	getI(keyDeepEnrichConcur, &s.DeepEnrichConcurrency)
	getI(keyDeepEnrichRefreshDays, &s.DeepEnrichRefreshDays)
	getF(keyDecayBaseRate, &s.DecayBaseRate)
	getF(keyDecayStrengthFloor, &s.DecayStrengthFloor)
	getI(keySchemaReframeMaxPerRun, &s.SchemaReframeMaxPerRun)
	getB(keyContextMemoryEnabled, &s.ContextMemoryEnabled)
	getI(keyContextMemoryMinMemories, &s.ContextMemoryMinMemories)
	getI(keyContextMemoryMaxPerRun, &s.ContextMemoryMaxPerRun)
	getI(keyContextRefreshDays, &s.ContextRefreshDays)
	getB(keyContextAugmentOnLowConfidence, &s.ContextAugmentOnLowConfidence)
	getB(keyConceptMapsAutoCreate, &s.ConceptMapsAutoCreate)
	getI(keyMapProposalMinTraces, &s.MapProposalMinTraces)
	getI(keyDreamsPerNightMax, &s.DreamsPerNightMax)
	// Wave 8e — LLM call timeout. Distinguish "user-set" from "default"
	// by checking whether the settings key actually exists; if absent,
	// apply the provider-aware default (1800s when Tier 2 is AirLLM,
	// otherwise the 120s baked into DefaultDreamPipelineSettings).
	if v, ok, _ := b.GetSetting(keyLLMCallTimeoutSec); ok && v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			s.LLMCallTimeoutSec = n
		}
	} else if isAirLLMTier2(b) {
		// Unset + Tier 2 is AirLLM → bump to a 30-min ceiling so AirLLM's
		// 1-3 tok/s rate doesn't guillotine inference. User can override
		// by saving any explicit value to the settings table.
		s.LLMCallTimeoutSec = 1800
	}
	return s, nil
}

// isAirLLMTier2 reports whether the persisted Tier 2 provider config
// names AirLLM as its kind. Used by GetDreamPipelineSettings to apply
// a longer-default LLM-call timeout when the user hasn't explicitly
// configured one. Bank-nil returns false (test fixtures / native runs
// don't trigger the bump).
func isAirLLMTier2(b *Bank) bool {
	if b == nil {
		return false
	}
	cfg, err := b.GetProviderConfig(Tier2)
	if err != nil {
		return false
	}
	return cfg.Kind == ProviderAirLLM
}

// SaveDreamPipelineSettings persists the 15 dream-pipeline keys.
func (b *Bank) SaveDreamPipelineSettings(s DreamPipelineSettings) error {
	pairs := map[string]string{
		keyDedupThreshold:       fmt.Sprintf("%g", s.DedupThreshold),
		keyDedupTagOverlapPct:   fmt.Sprintf("%d", s.DedupTagOverlapPct),
		keySynthesisMaxPerRun:   fmt.Sprintf("%d", s.SynthesisMaxPerRun),
		keySynthesisClusterMin:  fmt.Sprintf("%d", s.SynthesisClusterMin),
		keySynthesisClusterSim:  fmt.Sprintf("%g", s.SynthesisClusterSim),
		keyDecayAgeDays:         fmt.Sprintf("%d", s.DecayAgeDays),
		keyDecayRecallDays:      fmt.Sprintf("%d", s.DecayRecallDays),
		keyDecayMaxPerRun:       fmt.Sprintf("%d", s.DecayMaxPerRun),
		keyMapMinMemories:       fmt.Sprintf("%d", s.MapMinMemories),
		keyCrossRegionThreshold: fmt.Sprintf("%g", s.CrossRegionThreshold),
		keyCrossRegionMaxPerRun: fmt.Sprintf("%d", s.CrossRegionMaxPerRun),
		keySchemaMaxPerRun:      fmt.Sprintf("%d", s.SchemaMaxPerRun),
		keySchemaLookbackDays:   fmt.Sprintf("%d", s.SchemaLookbackDays),
		keySchemaClusterSim:     fmt.Sprintf("%g", s.SchemaClusterSim),
		keyReinforceDecayFactor: fmt.Sprintf("%g", s.ReinforceDecayFactor),
		keyReplayEnabled:        fmt.Sprintf("%t", s.ReplayEnabled),
		keyReplayMaxPerRun:       fmt.Sprintf("%d", s.ReplayMaxPerRun),
		keyReplayRecentDays:      fmt.Sprintf("%d", s.ReplayRecentDays),
		keyReplayPreexistingDays: fmt.Sprintf("%d", s.ReplayPreexistingDays),
		keyDeepEnrichEnabled:     fmt.Sprintf("%t", s.DeepEnrichEnabled),
		keyDeepEnrichMaxPerRun:   fmt.Sprintf("%d", s.DeepEnrichMaxPerRun),
		keyDeepEnrichTokenBudg:   fmt.Sprintf("%d", s.DeepEnrichTokenBudget),
		keyDeepEnrichConcur:      fmt.Sprintf("%d", s.DeepEnrichConcurrency),
		keyDeepEnrichRefreshDays: fmt.Sprintf("%d", s.DeepEnrichRefreshDays),
		keyDecayBaseRate:          fmt.Sprintf("%g", s.DecayBaseRate),
		keyDecayStrengthFloor:     fmt.Sprintf("%g", s.DecayStrengthFloor),
		keySchemaReframeMaxPerRun: fmt.Sprintf("%d", s.SchemaReframeMaxPerRun),
		keyContextMemoryEnabled:          fmt.Sprintf("%t", s.ContextMemoryEnabled),
		keyContextMemoryMinMemories:      fmt.Sprintf("%d", s.ContextMemoryMinMemories),
		keyContextMemoryMaxPerRun:        fmt.Sprintf("%d", s.ContextMemoryMaxPerRun),
		keyContextRefreshDays:            fmt.Sprintf("%d", s.ContextRefreshDays),
		keyContextAugmentOnLowConfidence: fmt.Sprintf("%t", s.ContextAugmentOnLowConfidence),
		keyLLMCallTimeoutSec:             fmt.Sprintf("%d", s.LLMCallTimeoutSec),
		keyConceptMapsAutoCreate:         fmt.Sprintf("%t", s.ConceptMapsAutoCreate),
		keyMapProposalMinTraces:          fmt.Sprintf("%d", s.MapProposalMinTraces),
		keyDreamsPerNightMax:             fmt.Sprintf("%d", s.DreamsPerNightMax),
	}
	for k, v := range pairs {
		if err := b.SetSetting(k, v); err != nil {
			return err
		}
	}
	return nil
}

// ──────────────────────────────────────────────────────────────────────────
// Pipeline orchestrator — wired into NightlyRunner.runStepsFn
// ──────────────────────────────────────────────────────────────────────────

// runDreamPipeline is the runStepsFn replacement. Returns nil on success
// (status=completed). Returns an error ONLY when Phase 0 fatally fails.
// Per-phase non-fatal errors land in stats.PhaseFailures so the run is
// marked status=partial without losing the work that did succeed.
func (n *NightlyRunner) runDreamPipeline(ctx context.Context, runID string, stats *NightlyStats) error {
	if n == nil || n.bank == nil {
		return errors.New("nightly runner not configured")
	}
	settings, _ := n.bank.GetDreamPipelineSettings()
	now := n.nowFn()

	// Capture the previous run's finished_at so Phase 9 can scope
	// "recalled since last cycle" correctly.
	var prevFinish time.Time
	if recent, _ := n.bank.ListNightlyRuns(NightlyRunListOpts{Limit: 5}); len(recent) > 0 {
		for _, r := range recent {
			if r.ID == runID || r.Status == "in_progress" {
				continue
			}
			if r.FinishedAt == "" {
				continue
			}
			t, err := time.Parse(time.RFC3339Nano, r.FinishedAt)
			if err != nil {
				t, err = time.Parse(time.RFC3339, r.FinishedAt)
				if err != nil {
					continue
				}
			}
			prevFinish = t
			break
		}
	}

	pc := &pipelineContext{
		ctx:           ctx,
		runID:         runID,
		bank:          n.bank,
		router:        n.router,
		hub:           n.hub,
		now:           now,
		settings:      settings,
		stats:         stats,
		lastRunFinish: prevFinish,
		bubbleEnqueue: n.bubbleEnqueue,
		lifecycle:     n.lifecycle,
		runner:        n,
	}
	if n.router != nil {
		pc.localConcepts = n.router.LocalConcepts
	}

	// Snapshot embedding coverage once before any phase runs. Lets the user
	// see "X/Y embedded" in the run's stats blob and explain a Phase 1
	// silent-skip without guessing. (Bug 4 in handoff 2026-05-10.)
	pc.captureEmbeddingCoverage()

	// [R3 periodic] CITATIONS.md #2. Bank context drifts over weeks; rows
	// whose deep_encoded_at is older than DeepEnrichRefreshDays get
	// re-marked dirty so successive nightlies refresh them. Quarterly by
	// default (90d); 0 disables. Runs once per cycle, before any phase.
	if days := pc.settings.DeepEnrichRefreshDays; days > 0 {
		cutoff := pc.now.AddDate(0, 0, -days).Format(time.RFC3339Nano)
		if n, err := pc.bank.MarkPeriodicallyStale(cutoff); err == nil && n > 0 {
			log.Printf("nightly: marked %d memories periodic-stale (cutoff=%s, refresh_days=%d)", n, cutoff, days)
		}
	}

	// [R12] CITATIONS.md #20 — adaptive phase weighting. Read the
	// dirty-queue distribution (post periodic-stale marking) and apply
	// per-phase budget multipliers so a young bank with mostly-new memories
	// gets Phase 0b + Phase 10 boosts, and a stable bank with mostly-
	// periodic markers gets Phase 7 + Phase 8 boosts. Weights are surfaced
	// in stats.phase_budget_weights for the dashboard to render.
	if total, byReason, err := pc.bank.CountDirtyMemories(); err == nil {
		weights := computePhaseBudgetWeights(total, byReason)
		pc.applyPhaseBudgetWeights(weights)
		pc.stats.PhaseBudgetWeights = &weights
		if weights.Reason != "balanced" {
			log.Printf("nightly: R12 adaptive weighting → %s (phase0b=%.2g phase7=%.2g phase8=%.2g phase10=%.2g)",
				weights.Reason, weights.Phase0b, weights.Phase7, weights.Phase8, weights.Phase10)
		}
	}

	// Phase 0 is fatal — without encoded inputs, downstream is meaningless.
	pc.emitProgress("encoding", 0, 0)
	if err := pc.runPhase0Encoding(); err != nil {
		return fmt.Errorf("phase 0 (encoding): %w", err)
	}

	// All later phases are best-effort. Each appends to PhaseFailures on
	// error; the orchestrator continues so a Phase 2 hiccup doesn't block
	// Phase 3-12.
	type phase struct {
		name string
		fn   func() error
	}
	steps := []phase{
		// Phase 0b — REM-style per-memory Tier 2 enrichment. Slow,
		// sequential, resumable across runs. Stage 2 phases (1, 2, 7, 8)
		// gate on deep_encoded_at != '' so they only operate on
		// already-enriched memories.
		{"deep_encode", pc.runPhase0bDeepEncoding},
		{"dedup", pc.runPhase1Dedup},
		{"synthesis", pc.runPhase2Synthesis},
		{"lexicon", pc.runPhase3Lexicon},
		{"maps", pc.runPhase4Maps},
		{"decay", pc.runPhase5Decay},
		// v2.7 Bundle L — dormancy pass. Memories whose explicit TTL fired
		// move to dormant_at != '' (still in the bank, hidden from default
		// /recall, resurrectable on demand). Runs right after decay so the
		// rest of the pipeline (augment, schema, replay) never picks up
		// stale rows that the user has explicitly asked to age out.
		{"dormancy", pc.runPhase5bDormancy},
		// 2026-05-21 — Phase 5c: purge expired research_cache rows. The
		// cache lookup path already treats expired rows as misses, but
		// they were piling up in the table and surfacing in GET /research
		// indefinitely. Cheap (one DELETE statement) so colocated with
		// the other hygiene passes.
		{"research_purge", pc.runPhase5cResearchPurge},
		{"augment", pc.runPhase6Augment},
		{"cross_region", pc.runPhase7CrossRegion},
		{"schema", pc.runPhase8Schema},
		// [R5] Phase 8.5 schema reframing — propagates schema lineage onto
		// memories whose summaries drift toward the schema's abstraction.
		{"schema_reframe", pc.runPhase8_5SchemaReframing},
		// [R13] Phase 8.7 Context Memory Formation — builds a composite
		// "what this region is about" framework for regions with enough
		// deep-encoded coverage. Feature-flagged OFF by default.
		{"context_memory", pc.runPhase8_7ContextMemory},
		{"reinforcement", pc.runPhase9Reinforcement},
		{"replay", pc.runPhase10Replay},
	}
	for _, p := range steps {
		pc.emitProgress(p.name, 0, 0)
		phaseStart := time.Now()
		log.Printf("nightly: phase %s start", p.name)
		err := safeCall(p.fn)
		dur := time.Since(phaseStart)
		if err != nil {
			log.Printf("nightly: phase %s failed after %s: %v", p.name, dur.Round(time.Millisecond), err)
			stats.PhaseFailures = append(stats.PhaseFailures, p.name+": "+err.Error())
		} else {
			log.Printf("nightly: phase %s done in %s", p.name, dur.Round(time.Millisecond))
		}
	}
	return nil
}

// emitProgress publishes a `nightly_progress` event with the current phase
// name. done/total are optional; pass 0/0 when the phase doesn't have a
// natural per-item counter.
func (pc *pipelineContext) emitProgress(phase string, done, total int) {
	// Persist the active phase on the runner so /nightly/runs can report
	// it on the in_progress row — polling clients (the dashboard's
	// Dream Journal card without WS, healthchecks, etc.) see live phase
	// progression without subscribing to WS. Cleared in DoRun's defer.
	if pc.runner != nil && done == 0 {
		pc.runner.SetCurrentPhase(phase)
	}
	if pc.hub == nil {
		return
	}
	payload := map[string]interface{}{
		"run_id": pc.runID,
		"phase":  phase,
	}
	if total > 0 {
		payload["done"] = done
		payload["total"] = total
	}
	pc.hub.Fanout(Event{
		SchemaVersion: SchemaVersion,
		Type:          "nightly_progress",
		Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
		AdapterID:     "sd-core-nightly",
		Payload:       payload,
	})
}

// llmCallTimeout returns the per-LLM-call deadline duration for any
// Chat-style inference call in the nightly pipeline. Driven by the
// `llm_call_timeout_sec` setting (DreamPipelineSettings.LLMCallTimeoutSec).
// Falls back to 120s if the setting is missing or non-positive — never
// returns zero, which would cause context.WithTimeout to deadline
// instantly.
//
// Wave 8e — replaces the previously-hardcoded 60/90/120-second ceilings
// scattered across phases. AirLLM-class providers (1-3 tok/s) get a
// 1800-second auto-default via the provider-aware path in
// GetDreamPipelineSettings; users can override via the dashboard's
// dream-pipeline settings panel.
func (pc *pipelineContext) llmCallTimeout() time.Duration {
	if pc == nil || pc.settings.LLMCallTimeoutSec <= 0 {
		return 120 * time.Second
	}
	return time.Duration(pc.settings.LLMCallTimeoutSec) * time.Second
}

// signalProviderUse bumps the lifecycle manager's last_used_at for every
// non-always_on managed service. Called immediately BEFORE each LLM Chat
// call in the nightly pipeline so a long AirLLM inference (5-15 min)
// doesn't trip the 10-min idle reaper mid-call. Nil-safe.
//
// `workflow` should be "nightly_run" for any phase running under the
// nightly cycle (Phase 0b, 2, 6, 7, 8, 8.7 augment + 8.7 main, 10) —
// "research_request" is reserved for the (currently future) recall-time
// Tier 3 augment path.
func (pc *pipelineContext) signalProviderUse(workflow string) {
	if pc == nil || pc.lifecycle == nil {
		return
	}
	pc.lifecycle.SignalProviderUse(workflow)
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 0 — Light Encoding
// ──────────────────────────────────────────────────────────────────────────

// runPhase0Encoding ensures every new memory has a cached embedding AND
// the light_encoded flag — but the flag flip is GATED on the embed
// succeeding. Previously the flag flipped first regardless of whether the
// embed went through; the user ended up with 3611 memories all marked
// light_encoded=true and zero embeddings on disk, which made every
// embedding-dependent phase silent-zero. (Per handoff "Phase 0 silent
// embedding failure 2026-05-10".)
//
// Per-memory failures are appended to phase_failures rather than aborting
// the whole run — a single bad row shouldn't sabotage the whole pass. But
// systemic failure (Ollama down, model not pulled, Tier 1 misconfigured)
// trips the consecutive-failure circuit breaker after 3 in a row to avoid
// hammering a known-broken upstream for thousands of memories.
//
// If 0 of the unencoded memories successfully embed AND there were
// failures, this phase returns an error → the run lands in status=failed.
// (Phase 0 is foundational; everything downstream depends on it.)
func (pc *pipelineContext) runPhase0Encoding() error {
	mems, err := pc.bank.ListMemoriesWith(MemoryListOpts{Limit: 50000})
	if err != nil {
		return fmt.Errorf("list memories: %w", err)
	}
	// Embedding provider — local Tier 1. Nil when unconfigured (test
	// fixtures, headless runs); we degrade to flag-only flipping in that
	// case so the pipeline still progresses and the no-Tier-1 path stays
	// testable. Production runs where this is nil should already be
	// caught at boot by the schema/provider check.
	embedder := (LLMProvider)(nil)
	if pc.router != nil {
		embedder = pc.router.ForEmbedding()
	}
	model := envOr("SD_SYNAPSE_MODEL", envOr("SD_CLASSIFY_MODEL", "llama3.2:3b"))

	const consecutiveFailureLimit = 3
	flipped := 0
	failures := 0
	consecutiveFailures := 0
	considered := 0

	for _, m := range mems {
		if m.LightEncoded {
			continue
		}
		considered++

		// Build the same hash key the synapse builder uses, so embeddings
		// written here are visible to AllEmbeddingsForMemories without
		// translation.
		tagsAny := make([]interface{}, len(m.Tags))
		for i, t := range m.Tags {
			tagsAny[i] = t
		}
		embedText := synapseEmbedText(map[string]interface{}{
			"text": m.Text,
			"tags": tagsAny,
		})
		hash := synapseTextHash(embedText)

		// Skip the actual embed call when one already exists for this
		// content. Cheaper than re-embedding 3000+ rows on every nightly
		// run when the synapse builder did its job between cycles.
		existing, gerr := pc.bank.GetEmbedding(hash)
		if gerr == nil && existing != nil {
			// Already embedded — just flip the flag.
		} else if embedder == nil {
			// Tier 1 unavailable — we can't generate the missing embedding.
			// Don't flip the flag, but also don't keep retrying memory after
			// memory; record one failure and break out so the run produces a
			// clean phase_failures entry.
			pc.stats.PhaseFailures = append(pc.stats.PhaseFailures,
				"encode:embed_failed (Tier 1 embedding provider unconfigured)")
			log.Printf("nightly: phase0 — Tier 1 embedding provider unconfigured; %d memories left unencoded", len(mems)-considered+1)
			break
		} else {
			ectx, cancel := context.WithTimeout(pc.ctx, 30*time.Second)
			vec, embedErr := embedder.Embed(ectx, embedText)
			cancel()
			if embedErr != nil {
				failures++
				consecutiveFailures++
				log.Printf("nightly: phase0 embed failed for %s: %v", m.ID, embedErr)
				pc.stats.PhaseFailures = append(pc.stats.PhaseFailures,
					fmt.Sprintf("encode:embed_failed (%s: %v)", m.ID, embedErr))
				if consecutiveFailures >= consecutiveFailureLimit {
					log.Printf("nightly: phase0 aborting — %d consecutive embed failures (Ollama down? model not pulled?)",
						consecutiveFailures)
					break
				}
				continue
			}
			if saveErr := pc.bank.SaveEmbedding(hash, vec, model); saveErr != nil {
				failures++
				consecutiveFailures++
				log.Printf("nightly: phase0 SaveEmbedding %s: %v", m.ID, saveErr)
				pc.stats.PhaseFailures = append(pc.stats.PhaseFailures,
					fmt.Sprintf("encode:save_failed (%s: %v)", m.ID, saveErr))
				if consecutiveFailures >= consecutiveFailureLimit {
					break
				}
				continue
			}
			consecutiveFailures = 0 // reset run of failures on a real success
		}

		// Embedding is present (cache hit or freshly written). NOW flip the
		// lifecycle flag.
		if err := pc.bank.SetMemoryLifecycle(m.ID, "light_encoded", true); err != nil {
			failures++
			pc.stats.PhaseFailures = append(pc.stats.PhaseFailures,
				fmt.Sprintf("encode:flip_failed (%s: %v)", m.ID, err))
			continue
		}
		flipped++
		_ = pc.bank.AppendAuditSilent(AuditEntry{
			Operation: "memory_light_encode", EntityType: "memory", EntityID: m.ID,
			AdapterID: "sd-core-nightly", Reason: "phase0",
		})
	}
	pc.stats.Encoded = flipped

	// Foundational failure: nothing got encoded but we tried (and failed).
	// Returning an error here means the run lands in status=failed and
	// downstream phases skip — which is the right call when no embeddings
	// exist for them to operate on.
	if considered > 0 && flipped == 0 && failures > 0 {
		return fmt.Errorf("phase0 encoded 0/%d memories — every embed call failed (Tier 1 unreachable or misconfigured)", considered)
	}
	return nil
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 1 — Deduplication
// ──────────────────────────────────────────────────────────────────────────

// memWithVec couples a memory record with its cached embedding for
// pairwise similarity work in Phases 1, 2, 7, 8.
type memWithVec struct {
	rec MemoryRecord
	vec []float32
}

// DedupRegionGroups maps anatomical regions to functional clusters used
// by Phase 1 dedup. Same-region pairing was too narrow — a memory about
// "added a Run-now button" might land in motor_cortex (UI action) while
// a near-duplicate lands in prefrontal_cortex (recent decision); raw
// region equality misses the pair. Pairing within a functional GROUP
// covers the cross-region duplicates the user actually has without
// blowing up to all-vs-all O(N²). Memories whose region isn't listed
// here fall back to a singleton group keyed by their raw region (so
// they still pair with same-region siblings, just not cross-region).
//
// Per handoff "dream pipeline tuning 2026-05-10" Bug 4 #3.
var DedupRegionGroups = map[string][]string{
	"memory":      {"hippocampus", "entorhinal_cortex", "parahippocampal_cortex"},
	"executive":   {"prefrontal_cortex", "frontal_lobe", "anterior_cingulate"},
	"language":    {"broca_area", "wernicke_area", "temporal_lobe", "temporal_lobe_left", "temporal_lobe_right"},
	"motor":       {"motor_cortex", "premotor_cortex", "cerebellum"},
	"sensory":     {"somatosensory_cortex", "occipital_lobe", "parietal_lobe", "visual_cortex"},
	"emotional":   {"amygdala", "insula"},
	"associative": {"associative_cortex", "default_mode_network", "corpus_callosum"},
	"subcortical": {"thalamus", "basal_ganglia", "brain_stem"},
}

// regionToGroup is the inverse lookup built once at package init: each
// region name → the functional group it belongs to.
var regionToGroup = func() map[string]string {
	out := map[string]string{}
	for group, regions := range DedupRegionGroups {
		for _, r := range regions {
			out[r] = group
		}
	}
	return out
}()

// dedupGroupOf returns the functional group for a region, or the region
// itself (singleton group) when unknown. Empty region collapses to "_unknown"
// so all unclassified memories pair against each other rather than not at all.
func dedupGroupOf(region string) string {
	if region == "" {
		return "_unknown"
	}
	if g, ok := regionToGroup[region]; ok {
		return g
	}
	return region // singleton group
}

// captureEmbeddingCoverage takes one pre-run snapshot of how many live
// memories have a cached embedding. Logged at INFO too so a user reading
// the container logs sees the gap without having to crack open the stats
// blob. Best-effort — a query failure leaves stats.EmbeddingCoverage nil.
func (pc *pipelineContext) captureEmbeddingCoverage() {
	if pc.bank == nil {
		return
	}
	var live int
	if err := pc.bank.db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE deleted_at = ''`,
	).Scan(&live); err != nil {
		log.Printf("nightly: embedding coverage live-count: %v", err)
		return
	}
	// Embeddings table has no FK to memories — count distinct rows whose
	// text_hash corresponds to a live memory. Cheap approximation: count
	// rows in embeddings where the text hash exists at all. The exact gap
	// only matters for the "is dedup likely to find anything?" decision,
	// not for forensic precision.
	model := envOr("SD_SYNAPSE_MODEL", envOr("SD_CLASSIFY_MODEL", "llama3.2:3b"))
	vecs, err := pc.bank.AllEmbeddingsForMemories(model)
	if err != nil {
		log.Printf("nightly: embedding coverage map: %v", err)
		return
	}
	embedded := len(vecs)
	unembedded := live - embedded
	if unembedded < 0 {
		unembedded = 0
	}
	pc.stats.EmbeddingCoverage = &EmbeddingCoverageStats{
		Live: live, Embedded: embedded, Unembedded: unembedded,
	}
	if live > 0 {
		gapPct := float64(unembedded) / float64(live) * 100.0
		log.Printf("nightly: embedding coverage live=%d embedded=%d unembedded=%d (gap %.1f%%)",
			live, embedded, unembedded, gapPct)
	}
}

// loadEmbeddedMemories returns all live (non-deleted) memories that have a
// cached embedding, paired with the embedding vector. Used by Phases 1,
// 2, 7, and 8 — they all walk the same set so we share the load.
//
// requireDeepEncoded gates on `deep_encoded_at != ''` per handoff
// "deep per-memory enrichment phase 2026-05-10" §Stage 2 gating —
// when true, Stage 2 phases only run against Tier 2-enriched content,
// not raw ingestion-time text. Pass false to keep the legacy "everything
// with an embedding" behaviour (used by phases that don't depend on
// content quality, like cross-region linking against a brand-new bank
// where deep-encoding is still ramping).
func (pc *pipelineContext) loadEmbeddedMemories() ([]memWithVec, error) {
	return pc.loadEmbeddedMemoriesWithGate(true)
}

// loadEmbeddedMemoriesAll returns the legacy unfiltered set — used by
// any caller that explicitly wants to operate on every embedded memory,
// not just deep-encoded ones.
func (pc *pipelineContext) loadEmbeddedMemoriesAll() ([]memWithVec, error) {
	return pc.loadEmbeddedMemoriesWithGate(false)
}

func (pc *pipelineContext) loadEmbeddedMemoriesWithGate(requireDeepEncoded bool) ([]memWithVec, error) {
	model := envOr("SD_SYNAPSE_MODEL", envOr("SD_CLASSIFY_MODEL", "llama3.2:3b"))
	vecs, err := pc.bank.AllEmbeddingsForMemories(model)
	if err != nil {
		return nil, err
	}
	mems, err := pc.bank.ListMemoriesWith(MemoryListOpts{Limit: 50000})
	if err != nil {
		return nil, err
	}
	out := make([]memWithVec, 0, len(mems))
	for _, m := range mems {
		v, ok := vecs[m.ID]
		if !ok {
			continue
		}
		if requireDeepEncoded && m.DeepEncodedAt == "" {
			continue
		}
		out = append(out, memWithVec{rec: m, vec: v})
	}
	return out, nil
}

// memoryHasTag returns true when `want` is in the tag slice (case-sensitive).
// Tags are short canonical strings (per the MCP tools' tag guidance) so we
// don't normalize here — caller passes the exact string they're looking for.
func memoryHasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

// runPhase1Dedup finds memory pairs in the same functional region group
// with cosine similarity above SD_DEDUP_THRESHOLD. The older / longer /
// more-tagged member wins canonical; the loser is soft-deleted with a
// 'merged into' reason and its id appended to canonical.merged_from.
//
// "Same functional region group" replaces the prior "same raw region"
// rule (handoff Bug 4 #3) so motor_cortex/prefrontal_cortex pair under
// the executive group umbrella, etc. See DedupRegionGroups.
//
// Refuses to run when the embedding-coverage gap is large (> 5% of live
// memories unembedded): with the synapse builder behind on writes,
// pairwise comparison runs on a stale subset and silently writes
// Merged: 0. Better to skip and surface the cause via PhaseFailures
// than to lie about the result. (Bug 4 #2.)
func (pc *pipelineContext) runPhase1Dedup() error {
	if cov := pc.stats.EmbeddingCoverage; cov != nil && cov.Live > 0 {
		gap := float64(cov.Unembedded) / float64(cov.Live)
		if gap > 0.05 {
			pc.stats.PhaseFailures = append(pc.stats.PhaseFailures,
				fmt.Sprintf("dedup:embedding_gap_too_large (%d/%d unembedded — %.1f%%)",
					cov.Unembedded, cov.Live, gap*100))
			log.Printf("nightly: phase1 skipped — embedding gap %.1f%% (%d/%d unembedded). Run synapse builder to backfill.",
				gap*100, cov.Unembedded, cov.Live)
			return nil
		}
	}
	mwvs, err := pc.loadEmbeddedMemories()
	if err != nil {
		return err
	}
	// Bucket by functional region GROUP rather than raw region. Same group
	// → same pairwise pool. See DedupRegionGroups for the mapping; unknown
	// regions fall back to a singleton group of their own name.
	byGroup := map[string][]memWithVec{}
	for _, x := range mwvs {
		g := dedupGroupOf(x.rec.RegionHint)
		byGroup[g] = append(byGroup[g], x)
	}
	merged := map[string]bool{} // id → merged-away-already
	pruned := 0
	mergeRefs := []MergePairRef{}

	for _, group := range byGroup {
		for i := 0; i < len(group); i++ {
			for j := i + 1; j < len(group); j++ {
				a, b := group[i], group[j]
				if merged[a.rec.ID] || merged[b.rec.ID] {
					continue
				}
				// Pinned memories (sd_pin_memory) are user-curated ground
				// truth — never merge a pinned memory into another, and
				// never merge anything into a pinned memory. Either side
				// being pinned skips the pair entirely.
				if memoryHasTag(a.rec.Tags, "pinned") || memoryHasTag(b.rec.Tags, "pinned") {
					continue
				}
				sim := float64(vecDot(a.vec, b.vec))
				if sim < pc.settings.DedupThreshold {
					continue
				}
				// Tag-overlap guard (mirrors inline findNearDuplicate). A
				// degenerate/low-spread embedding model can push cosine over
				// the threshold for topically-unrelated memories that merely
				// share a functional region group; requiring a minimum Jaccard
				// tag overlap blocks those cross-topic merges. jaccardPct is
				// symmetric, so candidate order doesn't matter. tagSet here is
				// the package-level helper — declared before the local map of
				// the same name below shadows it.
				if jaccardPct(tagSet(a.rec.Tags), tagSet(b.rec.Tags)) < pc.settings.DedupTagOverlapPct {
					continue
				}
				canonical, loser := pickCanonicalAndLoser(a.rec, b.rec)
				tagSet := map[string]bool{}
				for _, t := range canonical.Tags {
					tagSet[t] = true
				}
				for _, t := range loser.Tags {
					tagSet[t] = true
				}
				unionTags := make([]string, 0, len(tagSet))
				for t := range tagSet {
					unionTags = append(unionTags, t)
				}
				sort.Strings(unionTags)
				mergedFrom := append([]string{}, canonical.MergedFrom...)
				mergedFrom = append(mergedFrom, loser.ID)
				patch := MemoryUpdate{
					Tags:       &unionTags,
					MergedFrom: &mergedFrom,
				}
				if _, err := pc.bank.UpdateMemory(canonical.ID, patch); err != nil {
					log.Printf("nightly: phase1 update canonical %s: %v", canonical.ID, err)
					continue
				}
				_ = pc.bank.SetMemoryLifecycle(canonical.ID, "nightly_consolidated", true)
				pc.bank.wmu.Lock()
				_, derr := pc.bank.db.Exec(
					`UPDATE memories SET deleted_at = ?, deleted_reason = ?, updated_at = ?
					 WHERE id = ? AND deleted_at = ''`,
					pc.now.Format(time.RFC3339Nano),
					"merged into "+canonical.ID,
					pc.now.Format(time.RFC3339Nano),
					loser.ID,
				)
				pc.bank.wmu.Unlock()
				if derr != nil {
					log.Printf("nightly: phase1 soft-delete loser %s: %v", loser.ID, derr)
					continue
				}
				merged[loser.ID] = true
				pruned++
				if len(mergeRefs) < 20 {
					mergeRefs = append(mergeRefs, MergePairRef{
						CanonicalID:      canonical.ID,
						MergedID:         loser.ID,
						Similarity:       sim,
						CanonicalExcerpt: shortExcerpt(canonical, 60),
						MergedExcerpt:    shortExcerpt(loser, 60),
					})
				}
				// Silent: Phase 1 can write many merge rows; the nightly.completed
				// summary carries the user-visible aggregate via stats.merge_pairs.
				_ = pc.bank.AppendAuditSilent(AuditEntry{
					Operation:  "memory_merge",
					EntityType: "memory",
					EntityID:   canonical.ID,
					AfterJSON:  fmt.Sprintf(`{"canonical_id":"%s","merged_id":"%s","similarity":%g}`, canonical.ID, loser.ID, sim),
					Reason:     fmt.Sprintf("dedup similarity=%.3f", sim),
					AdapterID:  "sd-core-nightly",
				})
				if pc.bubbleEnqueue != nil {
					pc.bubbleEnqueue(canonical.ID)
				}
			}
		}
	}
	pc.stats.Merged = len(mergeRefs)
	pc.stats.Pruned += pruned
	pc.stats.MergePairs = mergeRefs
	return nil
}

// pickCanonicalAndLoser implements the "older / more tags / longer / higher
// recall_strength" tie-breaker. Stable: same inputs → same canonical.
func pickCanonicalAndLoser(a, b MemoryRecord) (canonical, loser MemoryRecord) {
	if a.RecallStrength > b.RecallStrength {
		return a, b
	}
	if b.RecallStrength > a.RecallStrength {
		return b, a
	}
	if len(a.Tags) > len(b.Tags) {
		return a, b
	}
	if len(b.Tags) > len(a.Tags) {
		return b, a
	}
	if len(a.Text) > len(b.Text) {
		return a, b
	}
	if len(b.Text) > len(a.Text) {
		return b, a
	}
	if a.CreatedAt < b.CreatedAt && a.CreatedAt != "" {
		return a, b
	}
	return b, a
}

// shortExcerpt returns the first n runes of the memory's text/enriched
// content, trimmed of newlines. Used for *Ref structs the narrative consumes.
func shortExcerpt(m MemoryRecord, n int) string {
	text := m.EnrichedText
	if text == "" {
		text = m.Text
	}
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= n {
		return text
	}
	return text[:n] + "…"
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 2 — Synthesis (Tier 2)
// ──────────────────────────────────────────────────────────────────────────

// runPhase2Synthesis clusters related-but-distinct memories (3+ shared
// tags, inter-similarity > 0.7, no sensitive / LocalConcept members,
// not already synthesised) and asks Tier 2 to write a single 2-4 sentence
// summary. The synthesis is saved as a new memory tagged with the union
// of cluster tags + ["synthesis", "nightly_consolidated"].
func (pc *pipelineContext) runPhase2Synthesis() error {
	if pc.router == nil {
		return errors.New("router unconfigured")
	}
	provider := pc.router.ForNightly()
	if provider == nil {
		return errors.New("Tier 2 unconfigured")
	}
	mwvs, err := pc.loadEmbeddedMemories()
	if err != nil {
		return err
	}
	// Eligible filter.
	eligible := mwvs[:0]
	for _, x := range mwvs {
		if x.rec.Sensitive {
			continue
		}
		if pc.tagsContainLocalConcept(x.rec.Tags) {
			continue
		}
		eligible = append(eligible, x)
	}
	clusters := pc.findClusters(eligible, pc.settings.SynthesisClusterMin, pc.settings.SynthesisClusterSim, true)
	if len(clusters) == 0 {
		return nil
	}
	maxSyntheses := pc.settings.SynthesisMaxPerRun
	created := 0
	syntheses := []SynthesisRef{}
	for _, cluster := range clusters {
		if created >= maxSyntheses {
			break
		}
		// Skip if any cluster member has already been part of a synthesis
		// (lineage check via synthesis_source_ids on their connected syntheses).
		if pc.clusterAlreadySynthesised(cluster) {
			continue
		}
		// Diversity: skip if Jaccard token overlap is too high (Phase 1's job).
		if jaccardOverlap(cluster) > 0.70 {
			continue
		}
		// Build synthesis prompt.
		var b strings.Builder
		b.WriteString("Synthesise these ")
		fmt.Fprintf(&b, "%d", len(cluster))
		b.WriteString(" related observations into a single 2-4 sentence summary that captures what they collectively reveal. Be concrete and factual. No preamble, no bullet points.\n\nOBSERVATIONS:\n")
		for _, m := range cluster {
			b.WriteString("- ")
			b.WriteString(memoryDisplayText(m.rec))
			b.WriteString("\n")
		}
		pc.signalProviderUse("nightly_run")
		nctx, cancel := context.WithTimeout(pc.ctx, pc.llmCallTimeout())
		out, err := provider.Chat(nctx, []Message{{Role: "user", Content: b.String()}}, 400)
		cancel()
		if err != nil {
			log.Printf("nightly: phase2 synthesis tier2 error: %v", err)
			continue
		}
		out = strings.TrimSpace(out)
		if len(out) < 20 {
			continue
		}
		if len(out) > 1000 {
			out = out[:1000]
		}
		// Build the synthesis memory.
		tagSet := map[string]bool{"synthesis": true, "nightly_consolidated": true}
		regionVote := map[string]int{}
		sourceIDs := make([]string, 0, len(cluster))
		for _, m := range cluster {
			sourceIDs = append(sourceIDs, m.rec.ID)
			for _, t := range m.rec.Tags {
				tagSet[t] = true
			}
			if m.rec.RegionHint != "" {
				regionVote[m.rec.RegionHint]++
			}
		}
		tags := make([]string, 0, len(tagSet))
		for t := range tagSet {
			tags = append(tags, t)
		}
		sort.Strings(tags)
		region := ""
		bestRegionCount := 0
		for r, c := range regionVote {
			if c > bestRegionCount {
				bestRegionCount = c
				region = r
			}
		}
		newMem := MemoryRecord{
			Text:                out,
			Tags:                tags,
			RegionHint:          region,
			Source:              "nightly_synthesis",
			NightlyConsolidated: true,
			LightEncoded:        true,
			SynthesisSourceIDs:  sourceIDs,
		}
		saved, err := pc.bank.SaveMemory(newMem)
		if err != nil {
			log.Printf("nightly: phase2 save synthesis: %v", err)
			continue
		}
		tIn := estimateTokens(b.String())
		tOut := estimateTokens(out)
		pc.stats.TokensIn += tIn
		pc.stats.TokensOut += tOut
		pc.chargeBudget(provider, tIn, tOut, Tier2)
		_ = pc.bank.AppendAudit(AuditEntry{
			Operation:  "memory_synthesis",
			EntityType: "memory",
			EntityID:   saved.ID,
			AfterJSON:  fmt.Sprintf(`{"id":"%s","source_ids":%s,"tokens_in":%d,"tokens_out":%d}`, saved.ID, mustJSON(sourceIDs), tIn, tOut),
			Reason:     fmt.Sprintf("cluster of %d memories", len(cluster)),
			AdapterID:  "sd-core-nightly",
		})
		if pc.bubbleEnqueue != nil {
			pc.bubbleEnqueue(saved.ID)
		}
		created++
		syntheses = append(syntheses, SynthesisRef{
			ID: saved.ID, SourceCount: len(cluster),
			SummaryExcerpt: shortExcerpt(saved, 80),
		})
	}
	pc.stats.Consolidated = created
	pc.stats.Syntheses = syntheses
	return nil
}

// memoryDisplayText returns the text the model should see — enriched if
// available, raw otherwise. Capped at 400 chars to keep prompts compact.
func memoryDisplayText(m MemoryRecord) string {
	t := m.EnrichedText
	if t == "" {
		t = m.Text
	}
	t = strings.TrimSpace(t)
	if len(t) > 400 {
		t = t[:400] + "…"
	}
	return t
}

// findClusters groups memories by shared tags + cosine similarity.
// Greedy: each memory joins the first cluster that satisfies the
// shared-tag count and inter-similarity floor. Suitable for hundreds of
// memories — we don't need a full k-means here.
func (pc *pipelineContext) findClusters(items []memWithVec, minSize int, simFloor float64, requireSharedTags bool) [][]memWithVec {
	clusters := [][]memWithVec{}
	used := make([]bool, len(items))
	for i := range items {
		if used[i] {
			continue
		}
		group := []memWithVec{items[i]}
		used[i] = true
		for j := i + 1; j < len(items); j++ {
			if used[j] {
				continue
			}
			// Inter-similarity to ALL current cluster members must clear floor.
			ok := true
			for _, m := range group {
				if float64(vecDot(m.vec, items[j].vec)) < simFloor {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
			if requireSharedTags && sharedTagCount(group, items[j]) < 2 {
				continue
			}
			group = append(group, items[j])
			used[j] = true
		}
		if len(group) >= minSize {
			clusters = append(clusters, group)
		}
	}
	return clusters
}

func sharedTagCount(group []memWithVec, candidate memWithVec) int {
	if len(group) == 0 {
		return 0
	}
	common := map[string]int{}
	for _, t := range candidate.rec.Tags {
		common[t] = 1
	}
	for _, m := range group {
		for _, t := range m.rec.Tags {
			if common[t] >= 1 {
				common[t]++
			}
		}
	}
	count := 0
	for _, c := range common {
		if c == len(group)+1 {
			count++
		}
	}
	return count
}

func jaccardOverlap(group []memWithVec) float64 {
	// Average pairwise Jaccard on token-set of text. Coarse but cheap.
	if len(group) < 2 {
		return 0
	}
	tokenize := func(s string) map[string]bool {
		out := map[string]bool{}
		for _, w := range strings.Fields(strings.ToLower(s)) {
			if len(w) > 2 {
				out[w] = true
			}
		}
		return out
	}
	sets := make([]map[string]bool, len(group))
	for i, g := range group {
		sets[i] = tokenize(g.rec.Text)
	}
	var sum float64
	count := 0
	for i := 0; i < len(sets); i++ {
		for j := i + 1; j < len(sets); j++ {
			inter, union := 0, 0
			seen := map[string]bool{}
			for w := range sets[i] {
				seen[w] = true
				if sets[j][w] {
					inter++
				}
			}
			for w := range sets[j] {
				seen[w] = true
			}
			union = len(seen)
			if union > 0 {
				sum += float64(inter) / float64(union)
				count++
			}
		}
	}
	if count == 0 {
		return 0
	}
	return sum / float64(count)
}

// clusterAlreadySynthesised checks whether any existing synthesis memory
// already lists every member of `cluster` in its synthesis_source_ids.
// Cheap: walks the bank's synthesis-tagged memories.
func (pc *pipelineContext) clusterAlreadySynthesised(cluster []memWithVec) bool {
	rows, err := pc.bank.db.Query(
		`SELECT synthesis_source_ids FROM memories
		 WHERE source = 'nightly_synthesis' AND deleted_at = ''`)
	if err != nil {
		return false
	}
	defer rows.Close()
	memberSet := map[string]bool{}
	for _, m := range cluster {
		memberSet[m.rec.ID] = true
	}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			continue
		}
		var ids []string
		if err := json.Unmarshal([]byte(raw), &ids); err != nil {
			continue
		}
		// If every member of `cluster` is in this synthesis's source_ids,
		// we've already done this cluster.
		all := true
		seen := map[string]bool{}
		for _, id := range ids {
			seen[id] = true
		}
		for id := range memberSet {
			if !seen[id] {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 3 — Lexicon Rebuild
// ──────────────────────────────────────────────────────────────────────────

// runPhase3Lexicon rebuilds the lexicon table from current memory state.
// Full rebuild (DELETE then INSERT) so merges/syntheses/decays land
// correctly without an incremental delta dance.
func (pc *pipelineContext) runPhase3Lexicon() error {
	// Snapshot existing pairs to compute added/removed delta.
	before := map[string]bool{}
	rows, err := pc.bank.db.Query(`SELECT tag_a, tag_b FROM lexicon`)
	if err == nil {
		for rows.Next() {
			var a, b string
			if err := rows.Scan(&a, &b); err == nil {
				before[a+"|"+b] = true
			}
		}
		rows.Close()
	}
	pc.lexiconBefore = before

	// Wipe and rebuild.
	pc.bank.wmu.Lock()
	if _, err := pc.bank.db.Exec(`DELETE FROM lexicon`); err != nil {
		pc.bank.wmu.Unlock()
		return fmt.Errorf("clear lexicon: %w", err)
	}
	pc.bank.wmu.Unlock()

	// CRITICAL: Phase 3 is destructive on a partial scan — the DELETE above
	// wipes the table, so a subset rebuild loses 1 - (cap/total) of the
	// user's lexicon graph. The 50k cap below is the bank's hard ceiling
	// (see bank.go ListMemoriesWith); above 50k memories this phase MUST
	// switch to paginated cursors before the cap silently truncates.
	// (See "dream pipeline tuning 2026-05-10" handoff Bug 1 — Phase 3 caveat.)
	mems, err := pc.bank.ListMemoriesWith(MemoryListOpts{Limit: 50000})
	if err != nil {
		return err
	}
	// Iter 33 — batched bulk insert. Previously this loop called
	// IncrementLexicon per-pair, which acquired the bank write mutex
	// and committed a separate WAL transaction for every tag pair.
	// On a 150-memory / 4500-pair rebuild this dominated the nightly
	// runtime (~3-6 min). Batching into a single transaction with a
	// prepared statement brings it to single-digit seconds.
	pairs := make([][2]string, 0, len(mems)*4)
	for _, m := range mems {
		for i := 0; i < len(m.Tags); i++ {
			for j := i + 1; j < len(m.Tags); j++ {
				pairs = append(pairs, [2]string{m.Tags[i], m.Tags[j]})
			}
		}
	}
	if err := pc.bank.BulkIncrementLexicon(pairs); err != nil {
		log.Printf("nightly: phase3 bulk lexicon insert: %v", err)
	}

	// Total + delta.
	after := map[string]bool{}
	rows2, _ := pc.bank.db.Query(`SELECT tag_a, tag_b FROM lexicon`)
	if rows2 != nil {
		for rows2.Next() {
			var a, b string
			if err := rows2.Scan(&a, &b); err == nil {
				after[a+"|"+b] = true
			}
		}
		rows2.Close()
	}
	added, removed := 0, 0
	for k := range after {
		if !before[k] {
			added++
		}
	}
	for k := range before {
		if !after[k] {
			removed++
		}
	}
	pc.stats.LexiconPairs = len(after)
	pc.stats.LexiconPairsAdded = added
	pc.stats.LexiconPairsRemoved = removed

	_ = pc.bank.AppendAudit(AuditEntry{
		Operation: "lexicon_rebuild", EntityType: "lexicon", EntityID: pc.runID,
		AfterJSON: fmt.Sprintf(`{"pairs":%d,"added":%d,"removed":%d}`, len(after), added, removed),
		AdapterID: "sd-core-nightly",
	})
	return nil
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 4 — Map Updates
// ──────────────────────────────────────────────────────────────────────────

// runPhase4Maps recomputes per-tag map metadata. For every distinct tag
// with count >= MapMinMemories we ensure a memory_maps row exists, refresh
// top_region + last_touched_at, and detect emergence (newly-eligible) /
// decay (count fell to 0).
func (pc *pipelineContext) runPhase4Maps() error {
	mems, err := pc.bank.ListMemoriesWith(MemoryListOpts{Limit: 50000})
	if err != nil {
		return err
	}
	// Tally per-tag.
	type tagInfo struct {
		count       int
		regionVote  map[string]int
		lastTouched string
	}
	tally := map[string]*tagInfo{}
	for _, m := range mems {
		for _, t := range m.Tags {
			info, ok := tally[t]
			if !ok {
				info = &tagInfo{regionVote: map[string]int{}}
				tally[t] = info
			}
			info.count++
			if m.RegionHint != "" {
				info.regionVote[m.RegionHint]++
			}
			if m.UpdatedAt > info.lastTouched {
				info.lastTouched = m.UpdatedAt
			}
		}
	}
	// Pre-existing maps snapshot for emergence detection.
	preExisting := map[string]bool{}
	if all, err := pc.bank.ListMemoryMaps("", 1000); err == nil {
		for _, mp := range all {
			preExisting[strings.ToLower(mp.Name)] = true
		}
	}

	emerged := []MapEmergenceRef{}
	archived := []string{}
	updated := 0
	// Track which keys we just emerged/refreshed this run so the archive
	// loop never archives a map we just promoted. Bug 5 in handoff
	// "dream pipeline tuning 2026-05-10" — `positron3d` previously appeared
	// in BOTH MapsEmerged and MapsArchived because tally is keyed by the
	// raw tag ("Positron3D") while the archive loop reads `mp.Name` which
	// is lowercase, so `tally[mp.Name]` returned nil → archive fired on a
	// just-emerged row.
	touchedKeys := map[string]bool{}
	proposed := 0     // count of newly-proposed maps emitted this Phase 4 (R13 takeover path)
	dedupSkipped := 0 // proposals avoided because a live map of another type already covers the name
	for tag, info := range tally {
		if info.count < pc.settings.MapMinMemories {
			continue
		}
		// Pick top region.
		topRegion := ""
		bestVote := 0
		for r, v := range info.regionVote {
			if v > bestVote {
				bestVote = v
				topRegion = r
			}
		}
		key := strings.ToLower(tag)
		// Concept-maps takeover: when ConceptMapsAutoCreate is false (the
		// default), Phase 4 no longer auto-upserts type=concept rows.
		// Instead:
		//   - If a map with this name already exists (any type), refresh
		//     its metadata only — preserves user-curated project/person/
		//     etc. maps that share a tag with this cluster.
		//   - Else if the tag cluster meets MapProposalMinTraces, emit a
		//     status='proposed' row that the user can accept/edit/dismiss
		//     via the R13 proposal UI.
		//   - Else: do nothing. The tag exists but isn't strong enough yet
		//     to warrant surfacing.
		if !pc.settings.ConceptMapsAutoCreate {
			// Look for ANY live (archived=0) map by this name across all
			// types. GetMemoryMapByName(key, "") was fixed 2026-05-26 to
			// actually do this cross-type lookup — previously it only
			// matched type='' which never exists, so Phase 4 happily
			// re-proposed concepts that already lived as accepted
			// Project/Entity/Technology rows, producing perpetual
			// Proposed regrowth + UNIQUE collisions on user acceptance.
			existing, _ := pc.bank.GetMemoryMapByName(key, "")
			isNew := existing.ID == ""
			if !isNew {
				// Refresh derived fields only — preserve the user's
				// curated type/name/schema_text. If the matched row is
				// already accepted in a non-concept type, this is the
				// dedup-by-name win: we DO NOT propose a duplicate
				// type=concept twin.
				existing.TopRegion = topRegion
				existing.LastTouchedAt = info.lastTouched
				if _, err := pc.bank.SaveMemoryMap(existing); err != nil {
					log.Printf("nightly: phase4 refresh map %s: %v", key, err)
					continue
				}
				if existing.Type != "concept" {
					dedupSkipped++
				}
				updated++
				touchedKeys[key] = true
				continue
			}
			// Brand-new tag cluster — propose if it crosses the threshold.
			threshold := pc.settings.MapProposalMinTraces
			if threshold <= 0 || info.count < threshold {
				continue // not strong enough; let it ride
			}
			mm := MemoryMap{
				Name:            key,
				Type:            "concept",
				AnchorTags:      []string{tag},
				Source:          "inferred",
				Status:          "proposed",
				GeneratedBy:     "phase_4_proposal",
				GenerationPhase: "phase_4",
				TopRegion:       topRegion,
				LastTouchedAt:   info.lastTouched,
				SchemaText:      fmt.Sprintf("Auto-proposed by Phase 4 from %d memories sharing tag %q. Review and accept, edit, or dismiss.", info.count, tag),
			}
			saved, err := pc.bank.SaveMemoryMap(mm)
			if err != nil {
				log.Printf("nightly: phase4 propose map %s: %v", key, err)
				continue
			}
			// Iter 25 (2026-05-14) — link every memory that carries this tag
			// to the new map via map_traces so hierarchical recall
			// (rankCandidates → mapAugmentCandidates) can expand from a
			// matched anchor_tag back to its member memories. Previously
			// Phase 4 created maps but left map_traces empty, so map-level
			// retrieval found nothing to expand.
			for _, m := range mems {
				for _, t := range m.Tags {
					if t == tag {
						_ = pc.bank.LinkMapTrace(saved.ID, m.ID)
						break
					}
				}
			}
			proposed++
			updated++
			touchedKeys[key] = true
			emerged = append(emerged, MapEmergenceRef{MapKey: key, MemoryCount: info.count})
			continue
		}
		// Legacy auto-create-concept-maps path (ConceptMapsAutoCreate=true).
		// Upsert by (name, type) — type defaults to "concept" for
		// auto-derived rows. Maps the user explicitly created with another
		// type are preserved; we update the metadata only.
		existing, err := pc.bank.GetMemoryMapByName(key, "concept")
		isNew := false
		if err != nil {
			isNew = true
		}
		mm := MemoryMap{
			ID:            existing.ID,
			Name:          key,
			Type:          "concept",
			AnchorTags:    []string{tag},
			Source:        "inferred",
			TopRegion:     topRegion,
			LastTouchedAt: info.lastTouched,
		}
		if existing.ID != "" {
			// Preserve existing schema_text + anchor_tags shape; only refresh
			// the derived fields. Cheaper to re-Save with the existing values
			// merged in.
			mm.SchemaText = existing.SchemaText
			if len(existing.AnchorTags) > 0 {
				mm.AnchorTags = existing.AnchorTags
			}
			mm.Source = existing.Source
		}
		saved, err := pc.bank.SaveMemoryMap(mm)
		if err != nil {
			log.Printf("nightly: phase4 save map %s: %v", key, err)
			continue
		}
		// Iter 25 — link every memory carrying this tag to the auto-created
		// map. Same hierarchical-recall plumbing as the proposal branch above.
		for _, m := range mems {
			for _, t := range m.Tags {
				if t == tag {
					_ = pc.bank.LinkMapTrace(saved.ID, m.ID)
					break
				}
			}
		}
		updated++
		touchedKeys[key] = true
		if isNew && !preExisting[key] {
			emerged = append(emerged, MapEmergenceRef{MapKey: key, MemoryCount: info.count})
		}
		// Silent: Phase 4 touches every distinct tag — would otherwise blast
		// dozens of audit.appended events per nightly run. The
		// stats.maps_updated count and stats.maps_emerged array carry the
		// user-visible signal.
		_ = pc.bank.AppendAuditSilent(AuditEntry{
			Operation: "map_update", EntityType: "memory_map", EntityID: key,
			AfterJSON: fmt.Sprintf(`{"map_key":"%s","memory_count":%d,"top_region":"%s"}`, key, info.count, topRegion),
			AdapterID: "sd-core-nightly",
		})
	}
	// Decay: maps that have zero live members (all merged or deleted away).
	// Two safeguards against the Bug 5 overlap:
	//   1. Skip if we just touched this key in the emerge loop (touchedKeys).
	//   2. Look up the tally by lowercase key — tally is keyed by raw tag,
	//      mp.Name is lowercase, so a case-folded probe is required.
	if all, err := pc.bank.ListMemoryMaps("", 1000); err == nil {
		// Build a lowercase-keyed view of the tally for case-insensitive lookup.
		tallyLower := make(map[string]*tagInfo, len(tally))
		for tag, info := range tally {
			tallyLower[strings.ToLower(tag)] = info
		}
		for _, mp := range all {
			lc := strings.ToLower(mp.Name)
			if touchedKeys[lc] {
				// Just emerged or refreshed — leave it alone.
				continue
			}
			if info := tallyLower[lc]; info != nil && info.count > 0 {
				continue
			}
			if mp.Archived {
				continue
			}
			pc.bank.wmu.Lock()
			_, _ = pc.bank.db.Exec(
				`UPDATE memory_maps SET archived = 1, updated_at = ? WHERE id = ?`,
				pc.now.Format(time.RFC3339Nano), mp.ID,
			)
			pc.bank.wmu.Unlock()
			archived = append(archived, mp.Name)
		}
	}
	pc.stats.MapsUpdated = updated
	pc.stats.MapsEmerged = emerged
	pc.stats.MapsArchived = archived
	if proposed > 0 {
		log.Printf("nightly: phase4 proposed %d new map(s) (threshold=%d, ConceptMapsAutoCreate=false)",
			proposed, pc.settings.MapProposalMinTraces)
	}
	if dedupSkipped > 0 {
		log.Printf("nightly: phase4 dedup-by-name skipped %d candidate(s) already-live in another type",
			dedupSkipped)
	}

	// Iter 25 — compute map embeddings for hierarchical recall.
	// Embed `name + " " + join(anchor_tags) + " " + schema_text` so the
	// query-to-map cosine match captures both literal tag overlap AND
	// semantic similarity to the cluster theme.
	//
	// Best-effort: an embedding provider miss doesn't block the dream.
	// Only re-embeds maps the run actually touched (touchedKeys) plus
	// any non-archived map whose embedding column is empty (catches
	// legacy maps from before this column existed).
	if pc.router != nil {
		if emb := pc.router.ForEmbedding(); emb != nil {
			embeddedCount := pc.embedConceptMapsPhase4(emb, touchedKeys)
			if embeddedCount > 0 {
				log.Printf("nightly: phase4 embedded %d concept map(s)", embeddedCount)
			}
		}
	}
	return nil
}

// embedConceptMapsPhase4 walks all non-archived maps, embeds each one's
// composite text (name + anchor_tags + schema_text), and persists the
// vector via SaveMapEmbedding. Skips maps whose embedding is already
// fresh under the current model unless the map was touched this run.
// Returns count of embeddings written.
func (pc *pipelineContext) embedConceptMapsPhase4(emb LLMProvider, touchedKeys map[string]bool) int {
	if pc.bank == nil || emb == nil {
		return 0
	}
	maps, err := pc.bank.ListMemoryMaps("", 1000)
	if err != nil {
		log.Printf("nightly: phase4 embed list maps: %v", err)
		return 0
	}
	modelLabel := providerModelLabel(emb)
	// Snapshot existing (id → model) so we can skip already-embedded
	// maps on the SAME model when they weren't touched this run.
	// MemoryMap struct doesn't carry the embedding columns; query
	// directly.
	existingByMap := map[string]string{}
	if pc.bank != nil {
		if rows, qerr := pc.bank.db.Query(
			`SELECT id, embedding_model FROM memory_maps WHERE archived = 0`,
		); qerr == nil {
			for rows.Next() {
				var id, mdl string
				if err := rows.Scan(&id, &mdl); err == nil {
					existingByMap[id] = mdl
				}
			}
			rows.Close()
		}
	}
	count := 0
	for _, mp := range maps {
		if mp.Archived {
			continue
		}
		key := strings.ToLower(mp.Name)
		if !touchedKeys[key] && existingByMap[mp.ID] == modelLabel {
			continue
		}
		composite := mp.Name
		if len(mp.AnchorTags) > 0 {
			composite += " " + strings.Join(mp.AnchorTags, " ")
		}
		if mp.SchemaText != "" {
			composite += " " + mp.SchemaText
		}
		// Reuse the standard embed path. ForEmbedding's provider has its
		// own context handling; phase context is fine for the call.
		ctx, cancel := context.WithTimeout(pc.ctx, 30*time.Second)
		vec, eerr := emb.Embed(ctx, composite)
		cancel()
		if eerr != nil {
			log.Printf("nightly: phase4 embed map %s: %v", mp.Name, eerr)
			continue
		}
		if err := pc.bank.SaveMapEmbedding(mp.ID, vec, modelLabel); err != nil {
			log.Printf("nightly: phase4 save map embedding %s: %v", mp.Name, err)
			continue
		}
		count++
	}
	return count
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 5 — Decay / Pruning
// ──────────────────────────────────────────────────────────────────────────

// runPhase5Decay applies Synaptic Homeostasis Hypothesis (SHY) — a
// continuous strength multiplier with salience-weighted protection,
// pruning at a low strength_floor.
//
// [R2] CITATIONS.md #4 (Tononi & Cirelli 2014 SHY) + #5 (2020 update).
// Sleep performs competitive down-selection: weakly activated synapses
// depress while frequently potentiated, schema-consistent ones are
// protected. We model this as:
//
//   protection = clamp01(0.5*salience + 0.3*recall_factor + 0.2*schema_fit)
//   decayed    = recall_strength * (1 - base_rate*(1-protection))
//
// where:
//   - recall_factor = clamp01(recall_strength / 5)   — highly-recalled rows protect themselves
//   - schema_fit    = clamp01(len(tags) / 5)         — well-tagged rows are structurally connected
//
// A memory is pruning-eligible (soft-delete) only when:
//   - existing eligibility predicates match (age, recall window, tag whitelist)
//   - decayed strength < settings.DecayStrengthFloor
//   - cap not yet reached
//
// Otherwise the new strength is just written back. Sensitive memories
// are never pruned regardless of strength (CITATIONS.md #22 Poe — sleep
// is for forgetting, but the user controls what gets forgotten).
func (pc *pipelineContext) runPhase5Decay() error {
	mems, err := pc.bank.ListMemoriesWith(MemoryListOpts{Limit: 50000})
	if err != nil {
		return err
	}
	ageCutoff := pc.now.AddDate(0, 0, -pc.settings.DecayAgeDays).Format(time.RFC3339Nano)
	recallCutoff := pc.now.AddDate(0, 0, -pc.settings.DecayRecallDays).Format(time.RFC3339Nano)
	baseRate := pc.settings.DecayBaseRate
	if baseRate <= 0 {
		baseRate = 0.05 // 5% strength bleed per cycle for unprotected memories
	}
	floor := pc.settings.DecayStrengthFloor
	if floor <= 0 {
		floor = 0.05
	}
	pruned := 0
	decayedCount := 0
	for _, m := range mems {
		if m.Sensitive {
			continue
		}
		if !m.LightEncoded {
			continue
		}
		// Compute SHY-style protection. Even nightly_consolidated memories
		// participate in the strength curve — the weighted approach lets a
		// high-salience consolidated memory stay strong without our needing
		// a hard "skip" carve-out.
		recallFactor := m.RecallStrength / 5.0
		if recallFactor > 1.0 {
			recallFactor = 1.0
		}
		schemaFit := float64(len(m.Tags)) / 5.0
		if schemaFit > 1.0 {
			schemaFit = 1.0
		}
		protection := 0.5*m.Salience + 0.3*recallFactor + 0.2*schemaFit
		if protection > 1.0 {
			protection = 1.0
		}
		if protection < 0 {
			protection = 0
		}
		decayed := m.RecallStrength * (1.0 - baseRate*(1.0-protection))
		if decayed < 0 {
			decayed = 0
		}
		// Persist the new strength when it actually changed (>= ~0.1% delta).
		if !approxEqual(decayed, m.RecallStrength, 0.001) {
			pc.bank.wmu.Lock()
			_, _ = pc.bank.db.Exec(
				`UPDATE memories SET recall_strength = ?, updated_at = ? WHERE id = ?`,
				decayed, pc.now.Format(time.RFC3339Nano), m.ID,
			)
			pc.bank.wmu.Unlock()
			decayedCount++
		}

		// Pruning gate — same eligibility predicates as the legacy binary
		// path PLUS the new strength_floor check. Cap-bounded.
		if pruned >= pc.settings.DecayMaxPerRun {
			continue
		}
		if m.NightlyConsolidated {
			continue
		}
		if m.CreatedAt > ageCutoff {
			continue
		}
		lastRecall := m.LastRecalledAt
		if lastRecall == "" {
			lastRecall = m.CreatedAt
		}
		if lastRecall > recallCutoff {
			continue
		}
		if !decayEligibleByTags(m.Tags) {
			continue
		}
		if decayed > floor {
			continue
		}
		// Soft-delete with reason.
		pc.bank.wmu.Lock()
		_, _ = pc.bank.db.Exec(
			`UPDATE memories SET deleted_at = ?, deleted_reason = ?, updated_at = ?
			 WHERE id = ? AND deleted_at = ''`,
			pc.now.Format(time.RFC3339Nano),
			"nightly_decay",
			pc.now.Format(time.RFC3339Nano),
			m.ID,
		)
		pc.bank.wmu.Unlock()
		pruned++
		_ = pc.bank.AppendAuditSilent(AuditEntry{
			Operation: "memory_decay", EntityType: "memory", EntityID: m.ID,
			Reason: fmt.Sprintf("nightly_decay (SHY): strength %.3f < floor %.3f, age > %dd, protection %.2f",
				decayed, floor, pc.settings.DecayAgeDays, protection),
			AdapterID: "sd-core-nightly",
		})
	}
	pc.stats.Pruned += pruned
	if decayedCount > 0 {
		log.Printf("nightly: phase5 SHY decay applied to %d memories (%d pruned at strength_floor=%.2f)",
			decayedCount, pruned, floor)
	}
	return nil
}

// approxEqual reports whether two floats are within tolerance.
func approxEqual(a, b, tol float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < tol
}

// decayEligibleByTags returns true when the memory's tags signal "ok to
// prune": empty (no anchor) or contains low_priority/transient/noise.
// Anchor-tagged memories are kept regardless of age — those are the user's
// curated entries.
func decayEligibleByTags(tags []string) bool {
	if len(tags) == 0 {
		return true
	}
	for _, t := range tags {
		switch strings.ToLower(t) {
		case "low_priority", "transient", "noise":
			return true
		}
	}
	return false
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 5b — Dormancy (v2.7 Bundle L)
// ──────────────────────────────────────────────────────────────────────────

// runPhase5bDormancy moves expired memories into dormancy. Calls the
// bank's RunDormancyPass — keeps the bank-level invariants (sensitive
// skip, sticky-on, audit row) in one place instead of duplicating here.
// The pipeline-stats row carries the count under DormantNew so the
// dashboard's nightly summary can show "X memories went dormant".
func (pc *pipelineContext) runPhase5bDormancy() error {
	if pc.bank == nil {
		return nil
	}
	res, err := pc.bank.RunDormancyPass()
	if err != nil {
		return err
	}
	if pc.stats != nil {
		pc.stats.DormantNew += res.Slept
	}
	return nil
}

// runPhase5cResearchPurge deletes research_cache rows whose expires_at
// is in the past. Cheap one-shot DELETE. The cache lookup path already
// treats expired rows as misses, but ListResearchCache used to surface
// them in the dashboard's research list indefinitely (the user saw
// research from 2026-05-12 still listed two days after its 2026-05-19
// expiry). Wiring the purge into the nightly pipeline keeps the table
// bounded and the list view truthful.
func (pc *pipelineContext) runPhase5cResearchPurge() error {
	if pc.bank == nil {
		return nil
	}
	n, err := pc.bank.PurgeExpiredResearchCache()
	if err != nil {
		return err
	}
	if n > 0 {
		log.Printf("nightly: phase 5c purged %d expired research_cache rows", n)
	}
	return nil
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 6 — Research Augmentation (delegates to handlers_augmentation.go)
// ──────────────────────────────────────────────────────────────────────────

// runPhase6Augment wraps the augmentation engine for the nightly path.
// Honors `augment_enabled` master switch + per-run caps. Memories created
// here get nightly_consolidated=true so phases 0-5 in the SAME run skip them.
func (pc *pipelineContext) runPhase6Augment() error {
	if pc.router == nil || pc.router.Tier3() == nil {
		return nil // not configured — silent skip
	}
	settings, _ := pc.bank.GetAugmentSettings()
	if !settings.Enabled {
		return nil
	}
	cands, err := findAugmentCandidates(pc.ctx, pc.bank, settings, pc.localConcepts)
	if err != nil {
		return err
	}
	tokensSpent := 0
	created := 0
	refs := []AugmentRef{}
	for _, c := range cands {
		if created >= settings.MaxPerRun {
			break
		}
		if tokensSpent >= settings.MaxTokensPerRun {
			break
		}
		safe, err := pc.router.PrepareOracleCall(OracleRequest{
			Topic:  c.CanonicalQuery,
			Prompt: c.CanonicalQuery,
			Reason: "nightly augmentation: " + c.MapKey,
		})
		if err != nil {
			continue
		}
		pc.signalProviderUse("nightly_run")
		nctx, cancel := context.WithTimeout(pc.ctx, pc.llmCallTimeout())
		content, err := safe.Provider.Chat(nctx,
			[]Message{{Role: "user", Content: safe.Prompt}}, 1024)
		cancel()
		if err != nil {
			continue
		}
		tIn := estimateTokens(safe.Prompt)
		tOut := estimateTokens(content)
		tokensSpent += tIn + tOut
		researchID := researchID(c.CanonicalQuery)
		richEntry := researchEntry{
			ID:          researchID,
			Query:       c.CanonicalQuery,
			Content:     content,
			CachedAt:    time.Now().UTC().Format(time.RFC3339Nano),
			ExpiresAt:   time.Now().UTC().AddDate(0, 0, 30).Format(time.RFC3339Nano),
			TokensIn:    tIn,
			TokensOut:   tOut,
			TokensTotal: tIn + tOut,
			Model:       safe.Provider.Name(),
		}
		richJSON, _ := json.Marshal(richEntry)
		_ = pc.bank.SaveResearchCache(ResearchCacheEntry{
			Topic:     researchID,
			Payload:   string(richJSON),
			FetchedAt: richEntry.CachedAt,
			ExpiresAt: richEntry.ExpiresAt,
			FetchedBy: "auto",
		})
		pc.chargeBudget(safe.Provider, tIn, tOut, Tier3)

		text := content
		if len(text) > 4000 {
			text = text[:4000]
		}
		mem, err := pc.bank.SaveMemory(MemoryRecord{
			Text:                text,
			Tags:                []string{c.MapKey, "augment", "oracle_augmented"},
			Source:              "nightly_augment",
			OracleAugmented:     true,
			LightEncoded:        true,
			NightlyConsolidated: true,
		})
		if err != nil {
			continue
		}
		_ = pc.bank.AppendAudit(AuditEntry{
			Operation:  "nightly_augment",
			EntityType: "memory",
			EntityID:   mem.ID,
			AfterJSON: fmt.Sprintf(
				`{"map_key":"%s","memory_id":"%s","tokens":%d,"research_id":"%s","run_id":"%s"}`,
				c.MapKey, mem.ID, tIn+tOut, researchID, pc.runID,
			),
			AdapterID: "sd-core-nightly",
			Reason:    fmt.Sprintf("nightly: thin %s map (%d memories, %d avg chars)", c.Category, c.MemoryCount, c.AvgChars),
		})
		if pc.bubbleEnqueue != nil {
			pc.bubbleEnqueue(mem.ID)
		}
		created++
		refs = append(refs, AugmentRef{
			MapKey: c.MapKey, MemoryID: mem.ID,
			Tokens: tIn + tOut, ResearchID: researchID,
		})
	}
	pc.stats.AugmentedCount = created
	pc.stats.AugmentedTokens = tokensSpent
	pc.stats.Augmented = refs
	return nil
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 7 — Cross-region Association
// ──────────────────────────────────────────────────────────────────────────

// runPhase7CrossRegion finds two kinds of cross-domain associations:
//
//   Pool A — high-cosine pairs (the "obvious neighbours" path). Cosine ≥
//   CrossRegionThreshold (default 0.78), score = cosine * (1 - jaccard)
//   so shared-tag pairs are penalised in favour of genuinely surprising
//   ones.
//
//   Pool B — [R6] low-similarity bridges (Cai et al. 2009 PNAS, REM
//   creativity from moderate-distance bridges). Cosine in [0.30, 0.50)
//   AND ≥ 2 shared rare tags (rare = appears in <5% of bank memories).
//   These are the pairs no NN search would ever surface; sharing rare
//   tags is the structural signal that makes them non-random.
//
// Each pool caps at CrossRegionMaxPerRun / 2 so neither dominates. The
// final dedup happens via the cross_region_links primary-key (collision
// = the higher-cosine pool A entry wins because it's inserted first).
//
// CITATIONS.md #24 (Cai 2009).
func (pc *pipelineContext) runPhase7CrossRegion() error {
	mwvs, err := pc.loadEmbeddedMemories()
	if err != nil {
		return err
	}
	rareTags := computeRareTagSet(mwvs, 0.05) // tags in <5% of bank
	type cand struct {
		a, b    memWithVec
		sim     float64
		jaccard float64
		score   float64
		pool    string // "nn" | "bridge"
	}
	var poolNN, poolBridge []cand
	for i := 0; i < len(mwvs); i++ {
		for j := i + 1; j < len(mwvs); j++ {
			a, b := mwvs[i], mwvs[j]
			if a.rec.RegionHint == b.rec.RegionHint {
				continue
			}
			sim := float64(vecDot(a.vec, b.vec))
			jc := tagJaccard(a.rec.Tags, b.rec.Tags)
			// Pool A — high-cosine NN.
			if sim >= pc.settings.CrossRegionThreshold {
				poolNN = append(poolNN, cand{
					a: a, b: b, sim: sim, jaccard: jc,
					score: sim * (1 - jc),
					pool:  "nn",
				})
				continue
			}
			// Pool B — low-sim bridges with shared rare tags.
			if sim >= 0.30 && sim < 0.50 {
				rareShared := countSharedRareTags(a.rec.Tags, b.rec.Tags, rareTags)
				if rareShared >= 2 {
					// Score: prefer the lowest-similarity bridges that
					// nonetheless share the most rare tags.
					poolBridge = append(poolBridge, cand{
						a: a, b: b, sim: sim, jaccard: jc,
						score: float64(rareShared) * (1 - sim),
						pool:  "bridge",
					})
				}
			}
		}
	}
	sort.Slice(poolNN, func(i, j int) bool { return poolNN[i].score > poolNN[j].score })
	sort.Slice(poolBridge, func(i, j int) bool { return poolBridge[i].score > poolBridge[j].score })
	half := pc.settings.CrossRegionMaxPerRun / 2
	if half < 1 {
		half = 1
	}
	if len(poolNN) > half {
		poolNN = poolNN[:half]
	}
	if len(poolBridge) > half {
		poolBridge = poolBridge[:half]
	}
	// Insert NN first so the ON CONFLICT prefers the high-cosine entry
	// when a pair somehow qualifies for both pools.
	combined := append(poolNN, poolBridge...)

	links := []CrossRegionRef{}
	bridgesWritten := 0
	for _, c := range combined {
		// Canonical ordering so (a,b) and (b,a) collapse.
		aID, bID := c.a.rec.ID, c.b.rec.ID
		if aID > bID {
			aID, bID = bID, aID
			c.a, c.b = c.b, c.a
		}
		pc.bank.wmu.Lock()
		_, _ = pc.bank.db.Exec(`
INSERT INTO cross_region_links (memory_a_id, memory_b_id, similarity, tag_overlap, discovered_at, run_id)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(memory_a_id, memory_b_id) DO UPDATE SET
    similarity    = excluded.similarity,
    tag_overlap   = excluded.tag_overlap,
    discovered_at = excluded.discovered_at,
    run_id        = excluded.run_id`,
			aID, bID, c.sim, c.jaccard, pc.now.Format(time.RFC3339Nano), pc.runID,
		)
		pc.bank.wmu.Unlock()
		links = append(links, CrossRegionRef{
			AID: aID, BID: bID, Similarity: c.sim, TagOverlap: c.jaccard,
			AExcerpt: shortExcerpt(c.a.rec, 60), BExcerpt: shortExcerpt(c.b.rec, 60),
		})
		if c.pool == "bridge" {
			bridgesWritten++
		}
		// Silent: Phase 7 caps at 20/run but each link still hits the wire
		// otherwise. stats.cross_region_links carries the user-visible payload.
		_ = pc.bank.AppendAuditSilent(AuditEntry{
			Operation: "cross_region_link", EntityType: "memory", EntityID: aID,
			AfterJSON: fmt.Sprintf(`{"partner":"%s","similarity":%g,"tag_overlap":%g,"pool":%q}`, bID, c.sim, c.jaccard, c.pool),
			AdapterID: "sd-core-nightly",
		})
	}
	pc.stats.CrossRegionLinks = links
	if bridgesWritten > 0 {
		log.Printf("nightly: phase7 wrote %d high-cosine + %d low-sim-bridge links", len(combined)-bridgesWritten, bridgesWritten)
	}
	return nil
}

// computeRareTagSet returns tags that appear in fewer than `frac` of the
// memories in `mwvs`. Used by [R6] to identify "structural signals" — a
// pair sharing a rare tag is more meaningful than a pair sharing a tag
// half the bank carries. (CITATIONS.md #24.)
func computeRareTagSet(mwvs []memWithVec, frac float64) map[string]bool {
	if len(mwvs) == 0 {
		return map[string]bool{}
	}
	counts := map[string]int{}
	for _, x := range mwvs {
		seen := map[string]bool{}
		for _, t := range x.rec.Tags {
			lc := strings.ToLower(strings.TrimSpace(t))
			if lc == "" || seen[lc] {
				continue
			}
			seen[lc] = true
			counts[lc]++
		}
	}
	threshold := int(float64(len(mwvs)) * frac)
	if threshold < 1 {
		threshold = 1
	}
	rare := map[string]bool{}
	for tag, n := range counts {
		if n < threshold {
			rare[tag] = true
		}
	}
	return rare
}

// countSharedRareTags counts how many of `rare` tags appear in BOTH a and b.
// Case-insensitive. Used by Phase 7 pool B.
func countSharedRareTags(a, b []string, rare map[string]bool) int {
	aSet := map[string]bool{}
	for _, t := range a {
		lc := strings.ToLower(strings.TrimSpace(t))
		if rare[lc] {
			aSet[lc] = true
		}
	}
	count := 0
	for _, t := range b {
		lc := strings.ToLower(strings.TrimSpace(t))
		if aSet[lc] {
			count++
		}
	}
	return count
}

func tagJaccard(a, b []string) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	setA := map[string]bool{}
	for _, t := range a {
		setA[t] = true
	}
	inter, union := 0, len(setA)
	for _, t := range b {
		if setA[t] {
			inter++
		} else {
			union++
		}
	}
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 8 — Schema Abstraction (Tier 2)
// ──────────────────────────────────────────────────────────────────────────

// runPhase8Schema clusters recently-created syntheses (Phase 2 outputs)
// and asks Tier 2 to identify the higher-order pattern.
func (pc *pipelineContext) runPhase8Schema() error {
	if pc.router == nil {
		return nil
	}
	provider := pc.router.ForNightly()
	if provider == nil {
		return nil
	}
	lookback := pc.now.AddDate(0, 0, -pc.settings.SchemaLookbackDays).Format(time.RFC3339Nano)
	mems, err := pc.bank.ListMemoriesWith(MemoryListOpts{Limit: 500, Since: lookback})
	if err != nil {
		return err
	}
	// Filter to syntheses + non-sensitive + non-LocalConcept.
	model := envOr("SD_SYNAPSE_MODEL", envOr("SD_CLASSIFY_MODEL", "llama3.2:3b"))
	vecs, _ := pc.bank.AllEmbeddingsForMemories(model)
	candidates := []memWithVec{}
	for _, m := range mems {
		if m.Source != "nightly_synthesis" {
			continue
		}
		if m.Sensitive {
			continue
		}
		if pc.tagsContainLocalConcept(m.Tags) {
			continue
		}
		v, ok := vecs[m.ID]
		if !ok {
			continue
		}
		candidates = append(candidates, memWithVec{rec: m, vec: v})
	}
	clusters := pc.findClusters(candidates, 3, pc.settings.SchemaClusterSim, true)
	created := 0
	refs := []SchemaRef{}
	for _, cluster := range clusters {
		if created >= pc.settings.SchemaMaxPerRun {
			break
		}
		if pc.clusterAlreadySchematized(cluster) {
			continue
		}
		var b strings.Builder
		b.WriteString("You are merging several summaries the system generated about one project. Write a single 2-3 sentence schema memory that captures the SPECIFIC, CONCRETE through-line they share. Requirements: name the actual files, components, commands, decisions, or outcomes that recur (e.g. 'forward.js hook routing', 'the SchemaMaxPerRun cap'); do NOT describe a generic process (forbidden phrasings: 'systematic application of', 'iterative improvement', 'the higher-order pattern is'). If the summaries share no concrete artifact, decision, or outcome in common — only that they happened in the same project — reply with exactly SKIP and nothing else. No preamble.\n\nSUMMARIES:\n")
		for _, c := range cluster {
			b.WriteString("- ")
			b.WriteString(memoryDisplayText(c.rec))
			b.WriteString("\n")
		}
		pc.signalProviderUse("nightly_run")
		nctx, cancel := context.WithTimeout(pc.ctx, pc.llmCallTimeout())
		out, err := provider.Chat(nctx, []Message{{Role: "user", Content: b.String()}}, 400)
		cancel()
		if err != nil {
			continue
		}
		out = strings.TrimSpace(out)
		if reason := classifySchema(out); reason != schemaKeep {
			pc.recordSchemaDrop(reason, "synthesis", out)
			continue // SKIP sentinel, filler, or no concrete anchor — don't pollute the bank
		}
		if len(out) < 20 {
			continue
		}
		if len(out) > 1000 {
			out = out[:1000]
		}
		tagSet := map[string]bool{"schema": true, "nightly_consolidated": true}
		sourceIDs := make([]string, 0, len(cluster))
		for _, c := range cluster {
			sourceIDs = append(sourceIDs, c.rec.ID)
			for _, t := range c.rec.Tags {
				if t == "synthesis" || t == "nightly_consolidated" {
					continue
				}
				tagSet[t] = true
			}
		}
		tags := make([]string, 0, len(tagSet))
		for t := range tagSet {
			tags = append(tags, t)
		}
		sort.Strings(tags)
		newMem := MemoryRecord{
			Text:                out,
			Tags:                tags,
			RegionHint:          "associative_cortex",
			Source:              "nightly_schema",
			NightlyConsolidated: true,
			LightEncoded:        true,
			SchemaSourceIDs:     sourceIDs,
		}
		saved, err := pc.bank.SaveMemory(newMem)
		if err != nil {
			continue
		}
		tIn := estimateTokens(b.String())
		tOut := estimateTokens(out)
		pc.stats.TokensIn += tIn
		pc.stats.TokensOut += tOut
		pc.chargeBudget(provider, tIn, tOut, Tier2)
		_ = pc.bank.AppendAudit(AuditEntry{
			Operation: "memory_schema", EntityType: "memory", EntityID: saved.ID,
			AfterJSON: fmt.Sprintf(`{"id":"%s","source_ids":%s,"tokens_in":%d,"tokens_out":%d}`, saved.ID, mustJSON(sourceIDs), tIn, tOut),
			AdapterID: "sd-core-nightly",
		})
		if pc.bubbleEnqueue != nil {
			pc.bubbleEnqueue(saved.ID)
		}
		created++
		refs = append(refs, SchemaRef{
			ID: saved.ID, SourceCount: len(cluster),
			AbstractionExcerpt: shortExcerpt(saved, 80),
		})
	}
	// [R8] Parallel input: cluster replay memories with overlapping
	// concepts. Replays carry temporal-mix signal that pure synthesis
	// clusters don't — schemas-from-replay are a distinct class. Bounded
	// by the same SchemaMaxPerRun cap (counts shared with the synthesis
	// path above so a flood of replay clusters can't bypass the budget).
	if replayRefs := pc.runPhase8SchemaFromReplay(provider, lookback, len(refs)); len(replayRefs) > 0 {
		refs = append(refs, replayRefs...)
	}
	pc.stats.Schemas = refs
	return nil
}

// schemaDropReason classifies *why* isVacuousSchema rejected a candidate, so
// the Phase-8 paths can count drops by cause and emit a debug line. This is the
// observability half of the gate: with SchemaMaxPerRun=2, a couple of silent
// drops produce zero schemas with no trace, and the gate is strict enough that
// it can plausibly drop abstract-but-useful schemas (a consolidation schema is
// abstract by nature). We deploy, measure the no-anchor drop-rate, sample the
// dropped text over a few nights, and relax/pivot the gate if it's nuking good
// ones — see the per-drop log lines in runPhase8Schema / runPhase8SchemaFromReplay.
type schemaDropReason string

const (
	schemaKeep         schemaDropReason = ""          // not vacuous — save it
	schemaDropSkip     schemaDropReason = "skip"      // model emitted the SKIP sentinel
	schemaDropFiller   schemaDropReason = "filler"    // matched a generic-process phrase
	schemaDropNoAnchor schemaDropReason = "no_anchor" // carried no concrete, lookup-able anchor
)

// Compiled once: the strong concrete-anchor patterns used by classifySchema.
// A schema clears the positive-anchor gate only if at least one matches — i.e.
// it references something a reader could actually look up: a real file, a path,
// a code identifier, an issue ref, a sha, or a known product/tool name. Generic
// process prose ("enhance functionality", "modular design") matches none and is
// dropped.
//
// Note RE2 has no lookahead, so the sha digit-requirement and the brand-noun
// exclusion are enforced in code below, not in-pattern.
var schemaAnchorPatterns = []*regexp.Regexp{
	// Filename with a real source/config extension: forward.js, nightly_pipeline.go.
	regexp.MustCompile(`(?i)\b[\w.-]+\.(go|js|ts|tsx|jsx|py|html?|json|ya?ml|sh|md|css|scss|sql|toml|rs|java|cpp|hpp|rb|php)\b`),
	// Path segment: where/error_message, /bank/memories. Two word-runs joined by "/".
	regexp.MustCompile(`\b\w+/\w[\w./-]*`),
	// Issue / PR reference: #52.
	regexp.MustCompile(`#\d+`),
	// Backtick-quoted token — the prompt invites quoting identifiers this way.
	regexp.MustCompile("`[^`]+`"),
	// snake_case identifier: error_message, brain_pulse (≥2 segments, has a letter).
	regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9]*_[A-Za-z0-9_]+\b`),
}

// schemaCamelHump matches a CamelCase / dromedaryCase token with an internal
// lower→upper hump: SchemaMaxPerRun, errorSignature. A single-hump capitalized
// English word (Read, Grep, Bash) has NO lower→upper transition, so it does NOT
// match — the key trap-avoider from the v1 handoff. BUT multi-hump *brand
// nouns* (TypeScript, JavaScript, PostgreSQL, WebSockets, GraphQL, GitHub,
// OAuth, macOS) DO have the hump and would falsely qualify vacuous "<brand>
// project" filler — so a camelHump token whose lowercased form is in
// schemaBrandNouns does not count as an anchor (checked in code, RE2 has no
// lookahead).
var schemaCamelHump = regexp.MustCompile(`\b\w*[a-z][A-Z]\w*\b`)

// schemaBrandNouns are multi-hump technology brand words that trip the
// camelHump pattern but carry no project-specific reference. Lowercased keys.
var schemaBrandNouns = map[string]bool{
	"typescript": true, "javascript": true, "postgresql": true,
	"websockets": true, "websocket": true, "graphql": true,
	"github": true, "gitlab": true, "oauth": true, "oauth2": true,
	"macos": true, "ios": true, "nodejs": true, "openai": true,
	"openapi": true, "mongodb": true, "dynamodb": true, "grpc": true,
	"restapi": true, "mysql": true, "redux": true, "nextjs": true,
}

// schemaShaPattern matches a hex run that *might* be a commit sha; the digit
// requirement (so "deface"/"feedback" don't qualify) is checked in code.
var schemaShaPattern = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
var schemaHexHasDigit = regexp.MustCompile(`[0-9]`)

// schemaWordTok extracts lowercase word tokens for the product-noun allowlist
// scan.
var schemaWordTok = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9]*`)

// schemaProductNouns is a precise allowlist of the real tools/products in this
// project. It REPLACES v1's loose generic lowercase-hyphen-compound anchor
// (which leaked stuffable filler like "through-line", "co-located",
// "cross-functional"): a legit operational schema that names cloudflared or
// ollama in plain prose still survives, without admitting the loose vector.
// Lowercased keys; matched on whole word tokens.
var schemaProductNouns = map[string]bool{
	"cloudflared": true, "cloudflare": true, "ollama": true,
	"docker": true, "sqlite": true, "sigma": true, "prusa": true,
	"unifi": true, "kubernetes": true, "postgres": true,
}

// schemaHasConcreteAnchor reports whether s names at least one concrete,
// lookup-able artifact (file, path, identifier, issue ref, sha, code-identifier
// CamelCase that isn't a bare brand noun, or a known product/tool name). It is
// the positive half of the isVacuousSchema gate.
func schemaHasConcreteAnchor(s string) bool {
	for _, re := range schemaAnchorPatterns {
		if re.MatchString(s) {
			return true
		}
	}
	// CamelCase code identifier — but a bare brand noun ("TypeScript project")
	// does not count.
	for _, m := range schemaCamelHump.FindAllString(s, -1) {
		if !schemaBrandNouns[strings.ToLower(m)] {
			return true
		}
	}
	// Commit-sha-like hex run (must contain a digit).
	for _, m := range schemaShaPattern.FindAllString(s, -1) {
		if schemaHexHasDigit.MatchString(m) {
			return true
		}
	}
	// Known product/tool name.
	for _, m := range schemaWordTok.FindAllString(s, -1) {
		if schemaProductNouns[strings.ToLower(m)] {
			return true
		}
	}
	return false
}

// schemaFillerPhrases is the generic-process denylist — a cheap first pass that
// catches the worst rephrasings before the anchor scan and lets us count
// "filler" drops separately from "no anchor" drops.
var schemaFillerPhrases = []string{
	"higher-order pattern",
	"higher order pattern",
	"systematic application of",
	"systematic execution of",
	"systematic use of",
	"structured approach",
	"modular design",
	"iterative improvement",
	"iterative refinement",
	"enhance functionality",
	"enhancing functionality",
	"enhanced functionality",
	"improve functionality",
	"improving functionality",
	"managing system settings",
	"overall functionality",
	"software development characterized by",
	"reflects a commitment to",
	"demonstrates a commitment to",
	"emphasis on best practices",
	"focus on best practices",
	"continuous improvement",
	"streamline the development",
	"streamlining the development",
	"improve the overall",
	"enhancing the overall",
}

// classifySchema returns the reason a Phase-8 schema reply should be discarded,
// or schemaKeep if it should be saved. The pipeline uses the reason to count
// drops by cause and log a sample; isVacuousSchema is the boolean wrapper.
//
// The prompt asks the model to reply with the SKIP sentinel when a cluster
// shares no concrete through-line, but the weak Tier-2 model routinely ignores
// that and just rephrases the same emptiness ("the project consistently
// utilized Bash commands … to enhance document structure and functionality").
// A blocklist alone can't keep up with the rephrasings, so the gate is
// positive-anchor-first:
//
//  1. SKIP sentinel            → schemaDropSkip.
//  2. Generic-process phrase   → schemaDropFiller (cheap, short-circuits).
//  3. No concrete anchor       → schemaDropNoAnchor. Process prose carries no
//     filename/path/identifier/sha/product-name and is dropped even when it
//     sprinkles in capitalized tool names ("Read"/"Grep" — single-hump words,
//     no anchor) or bare brand nouns ("TypeScript project" — excluded).
//
// A legitimately concrete schema ("These summaries revolve around the
// forward.js hook bridge … one fixes errorSignature's where/error_message
// read.") trips at least one anchor and is kept.
func classifySchema(s string) schemaDropReason {
	trimmed := strings.TrimSpace(s)
	up := strings.ToUpper(trimmed)
	if up == "" || up == "SKIP" ||
		strings.HasPrefix(up, "SKIP ") || strings.HasPrefix(up, "SKIP.") ||
		strings.HasPrefix(up, "SKIP:") || strings.HasPrefix(up, "SKIP-") ||
		strings.HasPrefix(up, "SKIP,") || strings.HasPrefix(up, "SKIP\n") {
		return schemaDropSkip
	}
	low := strings.ToLower(trimmed)
	for _, bad := range schemaFillerPhrases {
		if strings.Contains(low, bad) {
			return schemaDropFiller
		}
	}
	if !schemaHasConcreteAnchor(trimmed) {
		return schemaDropNoAnchor
	}
	return schemaKeep
}

// isVacuousSchema reports whether a Phase-8 schema reply should be discarded.
// Thin boolean wrapper over classifySchema for callers that don't need the
// reason.
func isVacuousSchema(s string) bool {
	return classifySchema(s) != schemaKeep
}

// truncateForLog clamps s to n runes for a single-line debug sample, appending
// an ellipsis when it was cut. Newlines are flattened so a multi-line schema
// stays on one log line.
func truncateForLog(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// recordSchemaDrop tallies a dropped Phase-8 schema by reason into the run
// stats and logs a one-line sample (reason + path + ~160 chars of the dropped
// text). The counters surface the drop-rate on the dashboard; the log line lets
// us eyeball *what* got dropped, so we can tell a healthy "killed the vacuous
// filler" night from a "the gate is nuking good abstract schemas" night. path
// is "synthesis" or "replay".
func (pc *pipelineContext) recordSchemaDrop(reason schemaDropReason, path, text string) {
	switch reason {
	case schemaDropSkip:
		pc.stats.SchemasSkipped++
	case schemaDropFiller:
		pc.stats.SchemasDroppedFiller++
	case schemaDropNoAnchor:
		pc.stats.SchemasDroppedNoAnchor++
	default:
		return
	}
	log.Printf("nightly: phase8 schema dropped (%s, %s): %s",
		reason, path, truncateForLog(text, 160))
}

// runPhase8SchemaFromReplay is the [R8] companion path for runPhase8Schema.
// It clusters Phase-10 replays produced in the lookback window by shared
// tags (≥3 shared, ≥2 replays per cluster) and asks Tier 2 to extract the
// higher-order schema. Outputs are stored with source='nightly_schema_from_replay'
// so the dashboard can distinguish them from synthesis-derived schemas.
//
// CITATIONS.md #11 (Lacaux 2021 N1 sleep-onset insight). The biological
// hook: insight forms during the transitional state when replay
// fragments overlap. We model "overlap" with the simplest signal we have
// — tag intersection — and let Tier 2 do the conceptual work.
//
// alreadyCreated is the number of synthesis-path schemas already produced
// in this run; we count toward the same SchemaMaxPerRun cap.
func (pc *pipelineContext) runPhase8SchemaFromReplay(provider LLMProvider, lookback string, alreadyCreated int) []SchemaRef {
	remaining := pc.settings.SchemaMaxPerRun - alreadyCreated
	if remaining <= 0 {
		return nil
	}
	mems, err := pc.bank.ListMemoriesWith(MemoryListOpts{Limit: 500, Since: lookback})
	if err != nil {
		return nil
	}
	// Filter to replays + non-sensitive.
	var replays []MemoryRecord
	for _, m := range mems {
		if m.Source != "nightly_replay" {
			continue
		}
		if m.Sensitive {
			continue
		}
		replays = append(replays, m)
	}
	if len(replays) < 2 {
		return nil
	}
	clusters := clusterReplaysByTagOverlap(replays, 3)
	if len(clusters) == 0 {
		return nil
	}
	refs := []SchemaRef{}
	for _, cluster := range clusters {
		if len(refs) >= remaining {
			break
		}
		if pc.clusterAlreadySchematizedByIDs(cluster) {
			continue
		}
		var b strings.Builder
		b.WriteString("These narratives mix memories from different time periods within one project and share overlapping concepts. Write a single 2-3 sentence schema capturing the SPECIFIC, CONCRETE recurring principle, decision, or tension across them. Requirements: name the actual files, components, commands, decisions, or outcomes that recur; do NOT describe a generic process (forbidden phrasings: 'systematic application of', 'iterative improvement', 'the higher-order pattern is'). If they share no concrete artifact, decision, or outcome — only that they happened in the same project — reply with exactly SKIP and nothing else. No preamble.\n\nNARRATIVES:\n")
		for _, c := range cluster {
			b.WriteString("- ")
			b.WriteString(memoryDisplayText(c))
			b.WriteString("\n")
		}
		pc.signalProviderUse("nightly_run")
		nctx, cancel := context.WithTimeout(pc.ctx, pc.llmCallTimeout())
		out, err := provider.Chat(nctx, []Message{{Role: "user", Content: b.String()}}, 400)
		cancel()
		if err != nil {
			continue
		}
		out = strings.TrimSpace(out)
		if reason := classifySchema(out); reason != schemaKeep {
			pc.recordSchemaDrop(reason, "replay", out)
			continue // SKIP sentinel, filler, or no concrete anchor — don't pollute the bank
		}
		if len(out) < 20 {
			continue
		}
		if len(out) > 1000 {
			out = out[:1000]
		}
		tagSet := map[string]bool{
			"schema":               true,
			"nightly_consolidated": true,
			"from_replay":          true,
		}
		sourceIDs := make([]string, 0, len(cluster))
		for _, c := range cluster {
			sourceIDs = append(sourceIDs, c.ID)
			for _, t := range c.Tags {
				if t == "synthesis" || t == "nightly_consolidated" || t == "replay" || t == "schema" {
					continue
				}
				tagSet[t] = true
			}
		}
		tags := make([]string, 0, len(tagSet))
		for t := range tagSet {
			tags = append(tags, t)
		}
		sort.Strings(tags)
		newMem := MemoryRecord{
			Text:                out,
			Tags:                tags,
			RegionHint:          "associative_cortex",
			Source:              "nightly_schema_from_replay",
			NightlyConsolidated: true,
			LightEncoded:        true,
			SchemaSourceIDs:     sourceIDs,
		}
		saved, err := pc.bank.SaveMemory(newMem)
		if err != nil {
			continue
		}
		tIn := estimateTokens(b.String())
		tOut := estimateTokens(out)
		pc.stats.TokensIn += tIn
		pc.stats.TokensOut += tOut
		pc.chargeBudget(provider, tIn, tOut, Tier2)
		_ = pc.bank.AppendAudit(AuditEntry{
			Operation: "memory_schema_from_replay", EntityType: "memory", EntityID: saved.ID,
			AfterJSON: fmt.Sprintf(`{"id":"%s","source_ids":%s,"tokens_in":%d,"tokens_out":%d}`, saved.ID, mustJSON(sourceIDs), tIn, tOut),
			AdapterID: "sd-core-nightly",
		})
		if pc.bubbleEnqueue != nil {
			pc.bubbleEnqueue(saved.ID)
		}
		refs = append(refs, SchemaRef{
			ID: saved.ID, SourceCount: len(cluster),
			AbstractionExcerpt: shortExcerpt(saved, 80),
		})
	}
	return refs
}

// clusterReplaysByTagOverlap groups replays so each cluster has ≥2
// members sharing ≥minSharedTags tags. Tags considered noise (replay
// provenance, map name itself) are dropped before counting. Greedy seed-
// based clustering: each unassigned replay starts a new cluster, and the
// remaining replays join if they share ≥minSharedTags with the seed.
// Pure function — testable without Tier 2.
func clusterReplaysByTagOverlap(replays []MemoryRecord, minSharedTags int) [][]MemoryRecord {
	if len(replays) < 2 {
		return nil
	}
	noise := map[string]bool{
		"replay":               true,
		"nightly_consolidated": true,
		"synthesis":            true,
		"schema":               true,
	}
	tagSetOf := func(m MemoryRecord) map[string]bool {
		s := map[string]bool{}
		for _, t := range m.Tags {
			if noise[t] {
				continue
			}
			s[t] = true
		}
		return s
	}
	sets := make([]map[string]bool, len(replays))
	for i, m := range replays {
		sets[i] = tagSetOf(m)
	}
	used := make([]bool, len(replays))
	var clusters [][]MemoryRecord
	for i := range replays {
		if used[i] {
			continue
		}
		cluster := []MemoryRecord{replays[i]}
		clusterIdx := []int{i}
		for j := i + 1; j < len(replays); j++ {
			if used[j] {
				continue
			}
			shared := 0
			for t := range sets[i] {
				if sets[j][t] {
					shared++
				}
			}
			if shared >= minSharedTags {
				cluster = append(cluster, replays[j])
				clusterIdx = append(clusterIdx, j)
			}
		}
		if len(cluster) >= 2 {
			for _, idx := range clusterIdx {
				used[idx] = true
			}
			clusters = append(clusters, cluster)
		}
	}
	return clusters
}

// clusterAlreadySchematizedByIDs is the R8-shaped sibling of
// clusterAlreadySchematized — operates on a slice of MemoryRecord rather
// than memWithVec (replays don't go through findClusters / embedding).
func (pc *pipelineContext) clusterAlreadySchematizedByIDs(cluster []MemoryRecord) bool {
	if len(cluster) == 0 {
		return false
	}
	memberSet := map[string]bool{}
	for _, m := range cluster {
		memberSet[m.ID] = true
	}
	rows, err := pc.bank.db.Query(
		`SELECT schema_source_ids FROM memories
		 WHERE source IN ('nightly_schema', 'nightly_schema_from_replay') AND deleted_at = ''`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			continue
		}
		var ids []string
		if err := json.Unmarshal([]byte(raw), &ids); err != nil {
			continue
		}
		seen := map[string]bool{}
		for _, id := range ids {
			seen[id] = true
		}
		all := true
		for id := range memberSet {
			if !seen[id] {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func (pc *pipelineContext) clusterAlreadySchematized(cluster []memWithVec) bool {
	rows, err := pc.bank.db.Query(
		`SELECT schema_source_ids FROM memories
		 WHERE source = 'nightly_schema' AND deleted_at = ''`)
	if err != nil {
		return false
	}
	defer rows.Close()
	memberSet := map[string]bool{}
	for _, m := range cluster {
		memberSet[m.rec.ID] = true
	}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			continue
		}
		var ids []string
		if err := json.Unmarshal([]byte(raw), &ids); err != nil {
			continue
		}
		all := true
		seen := map[string]bool{}
		for _, id := range ids {
			seen[id] = true
		}
		for id := range memberSet {
			if !seen[id] {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 8.5 — Schema reframing (R5)
// ──────────────────────────────────────────────────────────────────────────

// runPhase8_5SchemaReframing propagates schema lineage onto memories
// whose summaries drift toward the schema's abstraction.
//
// [R5] CITATIONS.md #15 (Hutchison & Rathore 2015 REM theta in emotional
// memory). The biological idea: REM doesn't just produce schemas — it
// uses them retroactively to reshape individual memories that fit the
// pattern. We model "fits the pattern" with a cosine threshold against
// the schema's text + at least one shared tag.
//
// We do NOT rewrite the memory's text (preserves R10 raw-text immutability).
// We append the schema's ID to the memory's `schema_source_ids` JSON
// array. The dashboard's existing schema-source navigation surface
// already renders this — it'll just show more inbound edges per schema.
//
// Cap: SchemaReframeMaxPerRun (default 50). Skips sensitive memories,
// LocalConcept-tagged memories, and memories that already include this
// schema in their schema_source_ids.
func (pc *pipelineContext) runPhase8_5SchemaReframing() error {
	if len(pc.stats.Schemas) == 0 {
		return nil // no schemas produced this run -> nothing to propagate
	}
	cap := pc.settings.SchemaReframeMaxPerRun
	if cap <= 0 {
		cap = 50
	}
	model := envOr("SD_SYNAPSE_MODEL", envOr("SD_CLASSIFY_MODEL", "llama3.2:3b"))
	vecs, err := pc.bank.AllEmbeddingsForMemories(model)
	if err != nil {
		return err
	}
	mems, err := pc.bank.ListMemoriesWith(MemoryListOpts{Limit: 50000})
	if err != nil {
		return err
	}
	// Map memory ID → record for fast lookup.
	memByID := make(map[string]MemoryRecord, len(mems))
	for _, m := range mems {
		memByID[m.ID] = m
	}

	updated := 0
	for _, sref := range pc.stats.Schemas {
		if updated >= cap {
			break
		}
		schemaMem, ok := memByID[sref.ID]
		if !ok {
			continue
		}
		schemaVec, ok := vecs[sref.ID]
		if !ok {
			continue // no embedding -> can't compute similarity
		}
		schemaTags := map[string]bool{}
		for _, t := range schemaMem.Tags {
			lc := strings.ToLower(strings.TrimSpace(t))
			if lc == "" {
				continue
			}
			// Skip the structural tags that would over-match.
			if lc == "schema" || lc == "synthesis" || lc == "nightly_consolidated" {
				continue
			}
			schemaTags[lc] = true
		}
		alreadySources := map[string]bool{}
		for _, id := range schemaMem.SchemaSourceIDs {
			alreadySources[id] = true
		}

		// Walk live memories. Hit threshold: cosine ≥ 0.7 AND ≥ 1 shared
		// non-structural tag. Skip the schema row itself + its existing
		// source memories + sensitive + LocalConcept rows.
		for _, m := range mems {
			if updated >= cap {
				break
			}
			if m.ID == sref.ID || alreadySources[m.ID] {
				continue
			}
			if m.Sensitive {
				continue
			}
			if pc.tagsContainLocalConcept(m.Tags) {
				continue
			}
			// Already has this schema in its lineage? Skip.
			has := false
			for _, sid := range m.SchemaSourceIDs {
				if sid == sref.ID {
					has = true
					break
				}
			}
			if has {
				continue
			}
			// Tag overlap check.
			tagHit := false
			for _, t := range m.Tags {
				lc := strings.ToLower(strings.TrimSpace(t))
				if schemaTags[lc] {
					tagHit = true
					break
				}
			}
			if !tagHit {
				continue
			}
			// Cosine check.
			memVec, ok := vecs[m.ID]
			if !ok {
				continue
			}
			if float64(vecDot(schemaVec, memVec)) < 0.7 {
				continue
			}
			// Append the schema id to schema_source_ids and persist.
			newIDs := append([]string{}, m.SchemaSourceIDs...)
			newIDs = append(newIDs, sref.ID)
			j, _ := json.Marshal(newIDs)
			pc.bank.wmu.Lock()
			_, _ = pc.bank.db.Exec(
				`UPDATE memories SET schema_source_ids = ?, updated_at = ? WHERE id = ?`,
				string(j), pc.now.Format(time.RFC3339Nano), m.ID,
			)
			pc.bank.wmu.Unlock()
			_ = pc.bank.AppendAuditSilent(AuditEntry{
				Operation: "memory_schema_reframed", EntityType: "memory", EntityID: m.ID,
				AfterJSON: fmt.Sprintf(`{"schema_id":%q}`, sref.ID),
				AdapterID: "sd-core-nightly",
			})
			updated++
		}
	}
	if updated > 0 {
		log.Printf("nightly: phase8.5 reframed %d memories with schema lineage", updated)
	}
	pc.stats.SchemaReframed = updated
	return nil
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 9 — Recall-Weighted Reinforcement
// ──────────────────────────────────────────────────────────────────────────

// runPhase9Reinforcement bumps recall_strength on memories recalled since
// the previous run finished; applies graceful decay to non-reinforced
// memories. Caps audit rows at 50/run to avoid flooding on heavy-recall days.
func (pc *pipelineContext) runPhase9Reinforcement() error {
	mems, err := pc.bank.ListMemoriesWith(MemoryListOpts{Limit: 50000})
	if err != nil {
		return err
	}
	since := ""
	if !pc.lastRunFinish.IsZero() {
		since = pc.lastRunFinish.Format(time.RFC3339Nano)
	}

	// [R9] Weak-trace boost (CITATIONS.md #5 Tononi 2020 + #7 Schapiro 2018).
	// FIRST pass: salience > 0.6 AND recall_strength < 0.8 — these are
	// "weak-but-tagged" memories that strong-recall logic ignores. Bump
	// their strength by a small delta (+0.05) so they don't decay to
	// invisibility. Track which IDs got the boost so the recall-decay
	// branch below doesn't immediately undo it (the second loop iterates
	// the same `mems` slice with stale in-memory strength values).
	weakBoosted := 0
	r9Boosted := map[string]bool{}
	for _, m := range mems {
		if !(m.Salience > 0.6 && m.RecallStrength < 0.8) {
			continue
		}
		newStrength := m.RecallStrength + 0.05
		if newStrength > 5.0 {
			newStrength = 5.0
		}
		if newStrength == m.RecallStrength {
			continue
		}
		pc.bank.wmu.Lock()
		_, _ = pc.bank.db.Exec(
			`UPDATE memories SET recall_strength = ?, updated_at = ? WHERE id = ?`,
			newStrength, pc.now.Format(time.RFC3339Nano), m.ID,
		)
		pc.bank.wmu.Unlock()
		weakBoosted++
		r9Boosted[m.ID] = true
	}

	auditCap := 50
	auditWritten := 0
	reinforced := 0
	for _, m := range mems {
		recalled := m.LastRecalledAt != "" && (since == "" || m.LastRecalledAt > since)
		if recalled {
			// Bump strength: +min(0.5, count/10). We don't track per-cycle
			// recall counts yet, so model a single recall as +0.1.
			delta := 0.1
			newStrength := m.RecallStrength + delta
			if newStrength > 5.0 {
				newStrength = 5.0
			}
			if newStrength == m.RecallStrength {
				continue
			}
			pc.bank.wmu.Lock()
			_, _ = pc.bank.db.Exec(
				`UPDATE memories SET recall_strength = ?, updated_at = ? WHERE id = ?`,
				newStrength, pc.now.Format(time.RFC3339Nano), m.ID,
			)
			// [R7] Stage transition: consolidating → semantic when the
			// memory has been deep-encoded AND is well-trodden. Threshold
			// 1.5 matches the existing strength-chip surface in trace
			// detail (CITATIONS.md #2 — Stickgold & Walker 2010 systems
			// consolidation). Idempotent: rows already 'semantic' stay.
			if newStrength > 1.5 && m.DeepEncodedAt != "" {
				_, _ = pc.bank.db.Exec(
					`UPDATE memories SET consolidation_stage = 'semantic'
					 WHERE id = ? AND consolidation_stage IN ('episodic', 'consolidating')`,
					m.ID,
				)
			}
			pc.bank.wmu.Unlock()
			reinforced++
			if auditWritten < auditCap {
				// Silent: Phase 9 was the worst offender on the wire — capped
				// at 50/run but every single one fired audit.appended. The
				// summary count lives in stats.reinforced_count.
				_ = pc.bank.AppendAuditSilent(AuditEntry{
					Operation: "memory_reinforce", EntityType: "memory", EntityID: m.ID,
					AfterJSON: fmt.Sprintf(`{"recall_strength":%g}`, newStrength),
					AdapterID: "sd-core-nightly",
				})
				auditWritten++
			}
		} else if pc.settings.ReinforceDecayFactor > 0 && pc.settings.ReinforceDecayFactor < 1 {
			// [R9] Skip memories the weak-trace pass already boosted —
			// `mems` holds pre-boost strength values, so multiplying here
			// would overwrite the boost with stale arithmetic.
			if r9Boosted[m.ID] {
				continue
			}
			newStrength := m.RecallStrength * pc.settings.ReinforceDecayFactor
			// Floor at 0.1 so memories don't asymptote to invisible.
			if newStrength < 0.1 {
				newStrength = 0.1
			}
			if math.Abs(newStrength-m.RecallStrength) < 0.001 {
				continue
			}
			pc.bank.wmu.Lock()
			_, _ = pc.bank.db.Exec(
				`UPDATE memories SET recall_strength = ?, updated_at = ? WHERE id = ?`,
				newStrength, pc.now.Format(time.RFC3339Nano), m.ID,
			)
			pc.bank.wmu.Unlock()
		}
	}
	pc.stats.ReinforcedCount = reinforced + weakBoosted

	// [R14] Global recall_strength rebalancing (CITATIONS.md #17 Liu 2023
	// Science Advances — REM recalibrates *population dynamics*, not just
	// individual bumps). After the per-memory work above, compute the
	// bank's strength distribution and apply a soft squeeze toward the
	// target (mean=1.5, stddev=0.7). Memories well within range get
	// minimal nudge; outliers get pulled in.
	rebalanced := pc.applyGlobalStrengthRebalance(1.5, 0.7)

	// Visibility log when Phase 9 found nothing to reinforce. Without this
	// the user can't tell "no /recall hits since last run" from "Phase 9
	// silent-skipped due to a bug." Bank-side last_recalled_at is bumped
	// only by /recall (not the dashboard search), so empty recalled sets
	// are common — log it explicitly.
	// (Per handoff "dream pipeline tuning 2026-05-10" tuning section.)
	if reinforced == 0 {
		log.Printf("nightly: phase9 reinforced 0 memories (none recalled since last run finish=%s); weak-boost=%d, rebalanced=%d",
			since, weakBoosted, rebalanced)
	} else if weakBoosted > 0 || rebalanced > 0 {
		log.Printf("nightly: phase9 reinforced=%d, weak-boost=%d, rebalanced=%d",
			reinforced, weakBoosted, rebalanced)
	}
	return nil
}

// applyGlobalStrengthRebalance is the [R14] bank-wide strength squeeze.
// Reads current mean + stddev of recall_strength across live memories,
// and applies a soft pull toward the (target_mean, target_stddev). Each
// memory's new strength is:
//
//   blend = 0.9 * current + 0.1 * (target_mean + (current - actual_mean) *
//                                  (target_stddev / max(actual_stddev, 0.01)))
//
// Capped at [0.1, 5.0]. Returns the count of memories whose strength
// changed by ≥ 0.01. Sensitive memories are still rebalanced (this is
// not external egress; it's local arithmetic on a numeric column).
//
// (CITATIONS.md #17 Liu 2023 — Human REM recalibrates neural activity.)
func (pc *pipelineContext) applyGlobalStrengthRebalance(targetMean, targetStddev float64) int {
	if pc.bank == nil {
		return 0
	}
	// Read current distribution. Single pass; SQLite's stddev_samp isn't
	// always available so we compute in Go.
	rows, err := pc.bank.db.Query(
		`SELECT id, recall_strength FROM memories WHERE deleted_at = ''`,
	)
	if err != nil {
		return 0
	}
	type row struct {
		id       string
		strength float64
	}
	all := []row{}
	var sum, sumSq float64
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.strength); err != nil {
			continue
		}
		all = append(all, r)
		sum += r.strength
		sumSq += r.strength * r.strength
	}
	rows.Close()
	n := float64(len(all))
	if n < 2 {
		return 0
	}
	mean := sum / n
	variance := (sumSq / n) - (mean * mean)
	if variance < 0 {
		variance = 0
	}
	stddev := math.Sqrt(variance)
	if stddev < 0.01 {
		stddev = 0.01 // protect against divide-by-zero
	}

	// If we're already near target on both metrics, skip the rewrite to
	// save the writer-lock ping-pong.
	if approxEqual(mean, targetMean, 0.05) && approxEqual(stddev, targetStddev, 0.05) {
		return 0
	}

	scale := targetStddev / stddev
	now := pc.now.Format(time.RFC3339Nano)
	pc.bank.wmu.Lock()
	defer pc.bank.wmu.Unlock()
	stmt, err := pc.bank.db.Prepare(
		`UPDATE memories SET recall_strength = ?, updated_at = ? WHERE id = ?`,
	)
	if err != nil {
		return 0
	}
	defer stmt.Close()

	changed := 0
	for _, r := range all {
		recentered := targetMean + (r.strength-mean)*scale
		// Soft squeeze: 90% original, 10% rebalanced — gradual pull, not
		// a hard reset. Liu 2023's recalibration is incremental too.
		blend := 0.9*r.strength + 0.1*recentered
		if blend < 0.1 {
			blend = 0.1
		}
		if blend > 5.0 {
			blend = 5.0
		}
		if approxEqual(blend, r.strength, 0.01) {
			continue
		}
		if _, err := stmt.Exec(blend, now, r.id); err == nil {
			changed++
		}
	}
	return changed
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 10 — Replay (Tier 2, gated)
// ──────────────────────────────────────────────────────────────────────────

// runPhase10Replay generates dream-style replays mixing recent + pre-
// existing memories within each project map. Default OFF (replay_enabled).
//
// [R4] CITATIONS.md #6 (Buzsáki 2015 — sharp-wave ripples are generative,
// not just rehearsal). Each replay deliberately blends NEW material
// (<ReplayRecentDays) with OLD material (>ReplayPreexistingDays) from the
// same project so Tier 2 has to find connections across temporal gaps.
// That's the part the user reviews; it's where contradictions and
// integration insights tend to surface.
//
// Per-run cap: ReplayMaxPerRun (default 3, R12-scaled). Each replay's
// source memory IDs land in SynthesisSourceIDs so Phase 8's R8 path can
// cluster replays by shared sources.
func (pc *pipelineContext) runPhase10Replay() error {
	if !pc.settings.ReplayEnabled {
		return nil
	}
	if pc.router == nil {
		return nil
	}
	provider := pc.router.ForNightly()
	if provider == nil {
		return nil
	}
	cap := pc.settings.ReplayMaxPerRun
	if cap <= 0 {
		cap = 3
	}
	recentDays := pc.settings.ReplayRecentDays
	if recentDays <= 0 {
		recentDays = 7
	}
	preexistingDays := pc.settings.ReplayPreexistingDays
	if preexistingDays <= 0 {
		preexistingDays = 30
	}
	recentCutoff := pc.now.AddDate(0, 0, -recentDays).Format(time.RFC3339Nano)
	preexistingCutoff := pc.now.AddDate(0, 0, -preexistingDays).Format(time.RFC3339Nano)

	maps, err := pc.bank.ListMemoryMaps("project", 50)
	if err != nil || len(maps) == 0 {
		return nil
	}
	rand.Shuffle(len(maps), func(i, j int) { maps[i], maps[j] = maps[j], maps[i] })

	created := 0
	for _, mp := range maps {
		if created >= cap {
			break
		}
		mems, err := pc.bank.listMemoriesByTag(mp.Name)
		if err != nil {
			continue
		}
		recent, preexisting := pc.partitionReplayPools(mems, recentCutoff, preexistingCutoff)
		// Both pools need at least 2 — otherwise it's not a generative mix,
		// it's just sampling one era. Skip to the next map.
		if len(recent) < 2 || len(preexisting) < 2 {
			continue
		}
		// Sensitive screen: if any candidate is sensitive, skip this map
		// entirely. Generative replay routes through Tier 2 which may not
		// be local; safer to bail than to filter and risk leaking a partial
		// thread.
		if anySensitive(recent) || anySensitive(preexisting) {
			continue
		}
		// Sample 3 from each pool. rand.Shuffle then slice — gives a fresh
		// mix every run even on a small bank.
		rand.Shuffle(len(recent), func(i, j int) { recent[i], recent[j] = recent[j], recent[i] })
		rand.Shuffle(len(preexisting), func(i, j int) { preexisting[i], preexisting[j] = preexisting[j], preexisting[i] })
		const perPool = 3
		if len(recent) > perPool {
			recent = recent[:perPool]
		}
		if len(preexisting) > perPool {
			preexisting = preexisting[:perPool]
		}
		// Order chronologically when feeding Tier 2 so the narrative has a
		// natural temporal anchor.
		combined := append([]MemoryRecord{}, preexisting...)
		combined = append(combined, recent...)
		sort.Slice(combined, func(i, j int) bool { return combined[i].CreatedAt < combined[j].CreatedAt })

		prompt := buildReplayPrompt(mp.Name, combined)
		pc.signalProviderUse("nightly_run")
		nctx, cancel := context.WithTimeout(pc.ctx, pc.llmCallTimeout())
		out, err := provider.Chat(nctx, []Message{{Role: "user", Content: prompt}}, 600)
		cancel()
		if err != nil {
			continue
		}
		out = strings.TrimSpace(out)
		if len(out) < 30 {
			continue
		}
		if len(out) > 2000 {
			out = out[:2000]
		}
		sourceIDs := make([]string, 0, len(combined))
		tagSet := map[string]bool{
			"replay":               true,
			mp.Name:                true,
			"nightly_consolidated": true,
		}
		for _, m := range combined {
			sourceIDs = append(sourceIDs, m.ID)
			for _, t := range m.Tags {
				// Drop pipeline-internal tags so the replay's tag list
				// reflects topic content, not provenance noise.
				if t == "nightly_consolidated" || t == "replay" || t == "synthesis" || t == "schema" {
					continue
				}
				tagSet[t] = true
			}
		}
		tags := make([]string, 0, len(tagSet))
		for t := range tagSet {
			tags = append(tags, t)
		}
		sort.Strings(tags)
		newMem := MemoryRecord{
			Text:                out,
			Tags:                tags,
			RegionHint:          mp.TopRegion,
			Source:              "nightly_replay",
			NightlyConsolidated: true,
			LightEncoded:        false, // user reviews replays before standard recall
			SynthesisSourceIDs:  sourceIDs,
		}
		saved, err := pc.bank.SaveMemory(newMem)
		if err != nil {
			continue
		}
		tIn := estimateTokens(prompt)
		tOut := estimateTokens(out)
		pc.stats.TokensIn += tIn
		pc.stats.TokensOut += tOut
		pc.chargeBudget(provider, tIn, tOut, Tier2)
		_ = pc.bank.AppendAudit(AuditEntry{
			Operation: "memory_replay", EntityType: "memory", EntityID: saved.ID,
			AfterJSON: fmt.Sprintf(`{"id":"%s","project_map":"%s","source_count":%d,"recent_count":%d,"preexisting_count":%d}`,
				saved.ID, mp.Name, len(sourceIDs), len(recent), len(preexisting)),
			AdapterID: "sd-core-nightly",
		})
		pc.stats.Replays = append(pc.stats.Replays, ReplayRef{
			ProjectMap: mp.Name, ReplayID: saved.ID,
			SourceCount: len(sourceIDs), Excerpt: shortExcerpt(saved, 100),
		})
		created++
	}
	return nil
}

// partitionReplayPools splits a project map's memories into "recent" and
// "preexisting" buckets per [R4]'s generative-mix rule. recentCutoff +
// preexistingCutoff are RFC3339Nano strings; lexicographic compare on
// CreatedAt is well-defined for that format.
func (pc *pipelineContext) partitionReplayPools(mems []MemoryRecord, recentCutoff, preexistingCutoff string) (recent, preexisting []MemoryRecord) {
	for _, m := range mems {
		if m.DeletedAt != "" {
			continue
		}
		if m.Source == "nightly_replay" {
			// Don't replay replays. R8 will cluster them; here we want raw
			// observations + syntheses to draw from.
			continue
		}
		if m.CreatedAt >= recentCutoff {
			recent = append(recent, m)
			continue
		}
		if m.CreatedAt < preexistingCutoff {
			preexisting = append(preexisting, m)
		}
	}
	return recent, preexisting
}

// buildReplayPrompt assembles the [R4] generative-mix prompt. Pulled out
// so the test suite can assert on prompt shape without invoking the
// network path.
func buildReplayPrompt(mapName string, mems []MemoryRecord) string {
	var b strings.Builder
	b.WriteString("These memories from the \"")
	b.WriteString(mapName)
	b.WriteString("\" project span both recent activity and earlier work. Weave them into a single coherent narrative that surfaces connections across the time gap — contradictions, recurring patterns, missing pieces, or how an earlier idea evolved (or didn't). 4-6 sentences. No preamble.\n\nOBSERVATIONS (chronological):\n")
	for _, m := range mems {
		b.WriteString("- [")
		// Just the date — Tier 2 doesn't need RFC3339 precision and short
		// dates keep prompt size down on big banks.
		if len(m.CreatedAt) >= 10 {
			b.WriteString(m.CreatedAt[:10])
		} else {
			b.WriteString(m.CreatedAt)
		}
		b.WriteString("] ")
		b.WriteString(memoryDisplayText(m))
		b.WriteString("\n")
	}
	return b.String()
}

// anySensitive returns true if any record carries Sensitive=true. R4 uses
// this to bail on a project map rather than filter — partial threads risk
// leaking shape.
func anySensitive(mems []MemoryRecord) bool {
	for _, m := range mems {
		if m.Sensitive {
			return true
		}
	}
	return false
}

// ──────────────────────────────────────────────────────────────────────────
// Helpers shared by multiple phases
// ──────────────────────────────────────────────────────────────────────────

// chargeBudget records token spend under the given tier with the
// provider-derived externality.
func (pc *pipelineContext) chargeBudget(provider LLMProvider, tIn, tOut int, tier TierKey) {
	if pc.bank == nil || provider == nil {
		return
	}
	external := !provider.IsLocal()
	_ = pc.bank.AddTokensUsedV2(
		pc.now.Format("2006-01-02"),
		string(tier),
		providerKindFromName(provider.Name()), provider.Name(),
		external, tIn, tOut,
	)
}

func (pc *pipelineContext) tagsContainLocalConcept(tags []string) bool {
	if pc.localConcepts == nil {
		return false
	}
	for _, t := range tags {
		if pc.localConcepts[t] {
			return true
		}
	}
	return false
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 12 — Dream Entry (literary prose; distinct from the technical narrative)
// ──────────────────────────────────────────────────────────────────────────

// dreamArchetypes give Phase 12 a different stylistic frame each run.
// Hash of run_id maps to a slot so the same run always gets the same
// archetype (idempotent re-runs of dream-entry generation produce
// consistent output for testing).
var dreamArchetypes = []string{
	"library at night",
	"under-water observatory",
	"industrial workshop",
	"weather front",
	"small civil ceremony",
	"long train journey",
	"kitchen at 3am",
	"garden in early frost",
	"operating theatre",
	"warehouse stocktake",
	"unfamiliar city",
	"deep forest",
}

// dreamEntryHasActivity reports whether the run did anything worth
// narrating. Returns true if any of: a memory got encoded / merged /
// pruned, a synthesis / schema / replay was created, a map emerged or
// archived, lexicon pairs changed, augmentation ran, cross-region links
// were discovered, or recall reinforcement bumped a memory.
//
// Bug 3 in handoff "dream pipeline tuning 2026-05-10" expanded this from
// the original narrow predicate (synthesis/schema/replay/augment/cross-
// region/reinforce only) to include background-hygiene phases (encode /
// lexicon / maps / decay). Phase 11's narrative prompt already references
// every field below.
func dreamEntryHasActivity(stats NightlyStats) bool {
	return stats.Encoded > 0 ||
		stats.Merged > 0 ||
		len(stats.Syntheses) > 0 ||
		len(stats.Schemas) > 0 ||
		len(stats.Replays) > 0 ||
		len(stats.MapsEmerged) > 0 ||
		len(stats.MapsArchived) > 0 ||
		stats.AugmentedCount > 0 ||
		len(stats.CrossRegionLinks) > 0 ||
		stats.ReinforcedCount > 0 ||
		stats.LexiconPairsAdded > 0 ||
		stats.LexiconPairsRemoved > 0 ||
		stats.Pruned > 0
}

func pickArchetype(runID string) string {
	h := sha1.Sum([]byte(runID))
	i := int(binary.BigEndian.Uint32(h[:4])) % len(dreamArchetypes)
	if i < 0 {
		i += len(dreamArchetypes)
	}
	return dreamArchetypes[i]
}

// generateDreamEntry produces the Phase 12 prose. Returns
// (text, archetype, tokens_in, tokens_out, model, error). Returns empty
// strings on skip conditions (run failed, Tier 2 unconfigured, no activity).
func (n *NightlyRunner) generateDreamEntry(ctx context.Context, runID string, stats NightlyStats) (string, string, int, int, string, error) {
	if n.router == nil {
		return "", "", 0, 0, "", nil
	}
	provider := n.router.ForNightly()
	if provider == nil {
		return "", "", 0, 0, "", nil
	}
	// Skip when nothing happened. The skip predicate is intentionally
	// broad — if ANY phase moved the needle, the user wants a dream entry
	// narrating it. Background-hygiene phases (0 encode, 3 lexicon, 4 maps,
	// 5 decay) are real work and earn an entry too. The dashboard's
	// "quiet night" surface handles the genuinely-empty case gracefully.
	// (Per handoff "dream pipeline tuning 2026-05-10" Bug 3.)
	if !dreamEntryHasActivity(stats) {
		return "", "", 0, 0, "", nil
	}
	archetype := pickArchetype(runID)
	bubbleQuotes := n.collectBubbleQuotesForRun(stats, 3)
	topicNames := n.collectTopicNamesForRun(stats, 5)

	mergeExample := ""
	if len(stats.MergePairs) > 0 {
		mergeExample = stats.MergePairs[0].CanonicalExcerpt
	}

	var b strings.Builder
	b.WriteString(`You are writing a dream-journal entry for a personal-memory system. Generate 2-4 short paragraphs of prose that narrate the night's memory reorganisation as if it were an actual dream — surreal, evocative, second-person where natural. Use the provided thought-fragments verbatim at least once (in quotation marks). Refer to the topics obliquely or concretely. Do NOT recite statistics. Do NOT use phrases like "tonight your mind" or "during this dream cycle". End with a single sentence that reflects on what was let go or carried forward.`)
	b.WriteString("\n\nARCHETYPE: ")
	b.WriteString(archetype)
	b.WriteString("\n\nTHOUGHT FRAGMENTS (use one or two verbatim, in quotes):\n")
	if len(bubbleQuotes) == 0 {
		b.WriteString("(none available — improvise from the topics)\n")
	} else {
		for _, q := range bubbleQuotes {
			b.WriteString("- ")
			b.WriteString(q)
			b.WriteString("\n")
		}
	}
	b.WriteString("\nTOPICS THAT PARTICIPATED:\n")
	for _, t := range topicNames {
		b.WriteString("- ")
		b.WriteString(t)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\nNOTABLE EVENTS:\n- %d new memories encoded\n- %d near-duplicates merged", stats.Encoded, stats.Merged)
	if mergeExample != "" {
		fmt.Fprintf(&b, " (e.g., %q)", mergeExample)
	}
	fmt.Fprintf(&b, "\n- %d clusters synthesised\n- %d cross-domain associations discovered\n- %d schemas built (rare)\n- %d memories reinforced; %d let go",
		stats.Consolidated, len(stats.CrossRegionLinks), len(stats.Schemas), stats.ReinforcedCount, stats.Pruned)
	if len(stats.Replays) > 0 {
		fmt.Fprintf(&b, "\n- Replayed %s: %s", stats.Replays[0].ProjectMap, stats.Replays[0].Excerpt)
	}
	b.WriteString("\n\nWrite the dream entry now. 2-4 short paragraphs, total ~300-350 words. End with a complete sentence — never trail off mid-thought. No preamble.")

	// Bump lifecycle last_used_at right before the call so the idle
	// reaper doesn't stop the (potentially slow) Tier 2 sidecar mid-
	// inference. AirLLM at 1-3 tok/s can blow past the idle window
	// during a single 800-token completion.
	if n.lifecycle != nil {
		n.lifecycle.SignalProviderUse("nightly_run")
	}
	nctx, cancel := context.WithTimeout(ctx, n.llmCallTimeout())
	// max_tokens bumped 600→800 so the model has headroom to finish the
	// closing sentence naturally rather than getting cut mid-thought by
	// the token budget.
	out, err := provider.Chat(nctx, []Message{{Role: "user", Content: b.String()}}, 800)
	cancel()
	if err != nil {
		return "", archetype, 0, 0, "", err
	}
	out = strings.TrimSpace(out)
	// Sentence-aware runaway guard. 3000 chars is well past the "2-4
	// short paragraphs" target; we only trim if the model truly bolts.
	// When trimming, fall back to the last sentence boundary so the
	// dashboard never displays "tonight your mind drifted into the…"
	// with a mid-word cut. Min boundary at hardCap/2 keeps us from
	// truncating to a single early sentence if the only "." landed
	// near the prompt's first paragraph.
	const dreamEntryHardCap = 3000
	if len(out) > dreamEntryHardCap {
		trimmed := out[:dreamEntryHardCap]
		if i := strings.LastIndexAny(trimmed, ".!?"); i > dreamEntryHardCap/2 {
			trimmed = trimmed[:i+1]
		}
		out = trimmed
	}
	// Clean-ending safety: even under the hard cap, the 800-token budget can
	// cut the prose mid-thought. If it doesn't end on terminal punctuation,
	// trim back to the last complete sentence so the dashboard never shows a
	// mid-word cut.
	if n := len(out); n > 0 {
		switch out[n-1] {
		case '.', '!', '?', '"':
		default:
			if i := strings.LastIndexAny(out, ".!?"); i > n/2 {
				out = strings.TrimSpace(out[:i+1])
			}
		}
	}
	return out, archetype, estimateTokens(b.String()), estimateTokens(out), provider.Name(), nil
}

// collectBubbleQuotesForRun walks the IDs that participated in the run's
// phases (merges, syntheses, augmentations, cross-region links) and reads
// the bubble pool from disk to find verbatim thought-fragments.
//
// Skips bubbles whose source memory is sensitive — the dream entry must
// not quote private content. Prefers concrete bubbles (length 30-120 chars,
// not in mode-mutated form).
func (n *NightlyRunner) collectBubbleQuotesForRun(stats NightlyStats, max int) []string {
	if n.bank == nil {
		return nil
	}
	candidateIDs := map[string]bool{}
	for _, m := range stats.MergePairs {
		candidateIDs[m.CanonicalID] = true
	}
	for _, s := range stats.Syntheses {
		candidateIDs[s.ID] = true
	}
	for _, a := range stats.Augmented {
		candidateIDs[a.MemoryID] = true
	}
	for _, x := range stats.CrossRegionLinks {
		candidateIDs[x.AID] = true
		candidateIDs[x.BID] = true
	}
	if len(candidateIDs) == 0 {
		return nil
	}
	// Read the bubble pool from disk. The path is deterministic.
	dataDir := envOr("SD_DATA_DIR", ".")
	poolPath := dataDir + "/assets/thought_bubbles.json"
	raw, err := osReadFileNilSafe(poolPath)
	if err != nil {
		return nil
	}
	var f thoughtBubblesFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil
	}
	out := []string{}
	for _, t := range f.Thoughts {
		if !candidateIDs[t.MemoryID] {
			continue
		}
		// Sensitive check.
		mem, err := n.bank.GetMemory(t.MemoryID)
		if err == nil && mem.Sensitive {
			continue
		}
		text := strings.TrimSpace(t.Text)
		if len(text) < 30 || len(text) > 120 {
			continue
		}
		out = append(out, text)
		if len(out) >= max {
			return out
		}
	}
	return out
}

// collectTopicNamesForRun returns up to N distinct topic / map_key strings
// drawn from the run's artefacts. Filters out User:* and LocalConcepts so
// the dream entry can't reference private content.
func (n *NightlyRunner) collectTopicNamesForRun(stats NightlyStats, max int) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		if strings.HasPrefix(s, "User:") {
			return
		}
		if n.router != nil && n.router.LocalConcepts[s] {
			return
		}
		if seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, m := range stats.MapsEmerged {
		add(m.MapKey)
		if len(out) >= max {
			return out
		}
	}
	for _, a := range stats.Augmented {
		add(a.MapKey)
		if len(out) >= max {
			return out
		}
	}
	for _, s := range stats.Syntheses {
		// We don't have the synthesis tags on hand; skip — topics from
		// emerged maps + augmented + replays cover the same ground.
		_ = s
	}
	for _, r := range stats.Replays {
		add(r.ProjectMap)
		if len(out) >= max {
			return out
		}
	}
	return out
}
