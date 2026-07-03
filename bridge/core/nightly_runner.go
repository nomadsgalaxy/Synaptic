// nightly_runner.go — Dream Cycle scheduler + framework.
//
// Responsibility split:
//
//   This file ships the *framework* for nightly consolidation runs:
//     - Single-instance concurrency lock (TryLock; second trigger → 409)
//     - Atomic in_progress → completed | failed lifecycle in nightly_runs
//     - Schedule-based auto-trigger (gates on bank.GetNightlySchedule().IsActive)
//     - Once-per-day idempotence (skip if today's date already has a run)
//     - Token-budget charging via bank.AddTokensUsed
//     - One audit_log summary entry per run (action="nightly_runner")
//     - WS event emission: `nightly.completed` on finish (control channel)
//     - Optional Tier 2 narrative generation (skipped silently on error)
//
//   The actual *consolidation logic* — Tier 2 step pipeline that flips
//   light_encoded → consolidated, populates merged_from, prunes by retention,
//   updates maps, rebuilds the lexicon — lands in P5-8/9/10 by replacing the
//   default no-op `runStepsFn`. The framework counts deltas via before/after
//   snapshots, so when the real steps land, the dashboard's stat cards get
//   non-zero numbers automatically.
//
// Concurrency model:
//
//   `mu` (sync.Mutex) protects `inFlight` + `currentID`. DoRun calls
//   TryLock; if already locked, returns ErrAlreadyRunning. The HTTP handler
//   maps that to 409. The Run() goroutine's auto-loop also calls DoRun and
//   simply skips on ErrAlreadyRunning.
//
//   The DoRun work itself does NOT hold mu — it sets inFlight=true under the
//   lock then releases, so other goroutines can read inFlight via IsRunning()
//   without blocking on the long-running consolidation pass. A defer
//   resets inFlight on exit (panic-safe).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNightlyAlreadyRunning is returned by DoRun when a run is already in
// flight. The HTTP handler turns this into a 409 with current_run_id.
var ErrNightlyAlreadyRunning = errors.New("nightly run already in progress")

// NightlyStats is the schema for nightly_runs.stats_json. Schema v2 (2026-05-09)
// added concrete artefact arrays produced by the dream-pipeline phases (merge
// pairs, syntheses, cross-region links, schemas, replays, augmentations).
// All new fields are `omitempty` so old dashboards still parse v1 stats blobs
// cleanly.
type NightlyStats struct {
	// v1 fields — preserved for back-compat with existing dashboard cards.
	Encoded      int `json:"encoded"`
	Consolidated int `json:"consolidated"`
	Merged       int `json:"merged"`
	Pruned       int `json:"pruned"`
	MapsUpdated  int `json:"maps_updated"`
	LexiconPairs int `json:"lexicon_pairs"`
	TokensTotal  int `json:"tokens_total"`
	TokensIn     int `json:"tokens_in"`
	TokensOut    int `json:"tokens_out"`

	// v2 — per-phase concrete artefacts (see CODE_HANDOFF dream cycle 2026-05-09).
	MergePairs          []MergePairRef    `json:"merge_pairs,omitempty"`        // Phase 1
	Syntheses           []SynthesisRef    `json:"syntheses,omitempty"`          // Phase 2
	LexiconPairsAdded   int               `json:"lexicon_pairs_added,omitempty"`
	LexiconPairsRemoved int               `json:"lexicon_pairs_removed,omitempty"`
	MapsEmerged         []MapEmergenceRef `json:"maps_emerged,omitempty"`       // Phase 4
	MapsArchived        []string          `json:"maps_archived,omitempty"`      // Phase 4
	AugmentedCount      int               `json:"augmented_count,omitempty"`    // Phase 6
	AugmentedTokens     int               `json:"augmented_tokens,omitempty"`   // Phase 6
	Augmented           []AugmentRef      `json:"augmented,omitempty"`          // Phase 6
	CrossRegionLinks    []CrossRegionRef  `json:"cross_region_links,omitempty"` // Phase 7
	Schemas             []SchemaRef       `json:"schemas,omitempty"`            // Phase 8
	// Phase 8 vacuousness-gate drop counters. The gate (isVacuousSchema) is
	// deliberately strict — abstract-but-useful schemas can be dropped — so we
	// must measure the real drop-rate and eyeball dropped samples after deploy
	// rather than flying blind. With SchemaMaxPerRun=2, two silent drops yield
	// zero schemas with no trace; these counters + the per-drop log lines are
	// how we decide whether to relax the gate. See isVacuousSchema's classifier.
	SchemasDroppedNoAnchor int `json:"schemas_dropped_no_anchor,omitempty"` // Phase 8: no concrete anchor
	SchemasDroppedFiller   int `json:"schemas_dropped_filler,omitempty"`    // Phase 8: matched filler blocklist
	SchemasSkipped         int `json:"schemas_skipped,omitempty"`           // Phase 8: model emitted SKIP sentinel

	SchemaReframed      int               `json:"schema_reframed,omitempty"`    // Phase 8.5 [R5]
	ReinforcedCount     int               `json:"reinforced_count,omitempty"`   // Phase 9
	Replays             []ReplayRef       `json:"replays,omitempty"`            // Phase 10
	ContextMemories     []ContextMemoryRef `json:"context_memories,omitempty"`  // Phase 8.7 [R13]
	// EmbeddingCoverage exposes the gap between live memories and memories
	// with cached embeddings. Populated once at run start. When Unembedded
	// > 5% of Live, Phase 1 (and other embedding-dependent phases) silent-
	// skip — surfacing this lets the user see "1200/3608 embedded" instead
	// of having to guess why dedup found 0 pairs. (Bug 4 in handoff
	// "dream pipeline tuning 2026-05-10".)
	EmbeddingCoverage *EmbeddingCoverageStats `json:"embedding_coverage,omitempty"`
	// DeepEncoding reports Phase 0b progress: how many memories got Tier 2
	// enrichment this run, queue depth + breakdown by dirty_reason, and a
	// cursor so a frontend can render "27/3611 enriched · 33% coverage ·
	// ~76 more nightly runs to drain the queue." Stats schema bumped from
	// v2 → v3 when DeepEncoding is non-nil. See handoff "deep per-memory
	// enrichment phase 2026-05-10" §Stats v3.
	DeepEncoding *DeepEncodingStats `json:"deep_encoding,omitempty"`
	// PhaseBudgetWeights records the per-phase budget multipliers R12 picked
	// for this run, derived from the dirty-queue's by_reason distribution.
	// See computePhaseBudgetWeights + CITATIONS.md #20 (Sarangi 2021 — REM
	// proportional to environmental novelty). nil when no adjustment was made.
	PhaseBudgetWeights *PhaseBudgetWeights `json:"phase_budget_weights,omitempty"`
	// PhaseFailures lists phases that errored but didn't abort the run.
	// Drives status=partial when non-empty.
	PhaseFailures []string `json:"phase_failures,omitempty"`

	// v2.7 Bundle L — count of memories Phase 5b moved into dormancy
	// this run. Lets the dashboard show "X memories fell asleep last
	// night" alongside the "Y memories pruned" / "Z memories merged"
	// cards.
	DormantNew int `json:"dormant_new,omitempty"`

	SchemaVersion int `json:"schema_version"`
}

// EmbeddingCoverageStats reports the live-vs-embedded gap. Populated once
// at the start of every run by the pipeline orchestrator.
type EmbeddingCoverageStats struct {
	Live       int `json:"live"`
	Embedded   int `json:"embedded"`
	Unembedded int `json:"unembedded"`
}

// DeepEncodingStats reports Phase 0b's progress. EnrichedThisRun is just
// the memories Tier 2 successfully re-summarized in THIS run; Remaining
// is the dirty-queue depth after this run exits. CoveragePercent and
// EstimatedRunsToCompletion are derived for frontend convenience.
type DeepEncodingStats struct {
	EnrichedThisRun           int                  `json:"enriched_this_run"`
	Remaining                 int                  `json:"remaining"`
	CoveragePercent           float64              `json:"coverage_percent"`
	Cursor                    string               `json:"cursor,omitempty"`
	Queue                     DeepEncodingQueueRef `json:"queue"`
	EstimatedRunsToCompletion int                  `json:"estimated_runs_to_completion"`
	TokensIn                  int                  `json:"tokens_in"`
	TokensOut                 int                  `json:"tokens_out"`
}

// DeepEncodingQueueRef breaks the dirty queue down by reason so the
// dashboard can render "12 just-created · 8 user-edited · 3540 model upgrade".
type DeepEncodingQueueRef struct {
	TotalDirty int            `json:"total_dirty"`
	// TotalQueued is the wave-6 alias for TotalDirty per CODE_HANDOFF —
	// Rename dirty to undreamt (2026-05-10). Same value; surfaced under
	// both keys for the back-compat transition window.
	TotalQueued int            `json:"total_queued"`
	ByReason    map[string]int `json:"by_reason"`
}

// PhaseBudgetWeights is [R12]'s per-phase multiplier set, surfaced in
// stats so the dashboard can render "tonight: 1.4× Phase 0b" etc. Reason
// names the dominant queue-class that triggered the boost — one of
// "created_dominant", "periodic_dominant", or "balanced".
type PhaseBudgetWeights struct {
	Phase0b float64 `json:"phase_0b"`
	Phase7  float64 `json:"phase_7"`
	Phase8  float64 `json:"phase_8"`
	Phase10 float64 `json:"phase_10"`
	Reason  string  `json:"reason"`
}

// MergePairRef is one Phase 1 dedup outcome — kept tiny so 20 of these in a
// stats blob is still under a couple of kilobytes.
type MergePairRef struct {
	CanonicalID       string  `json:"canonical_id"`
	MergedID          string  `json:"merged_id"`
	Similarity        float64 `json:"similarity"`
	CanonicalExcerpt  string  `json:"canonical_excerpt,omitempty"`
	MergedExcerpt     string  `json:"merged_excerpt,omitempty"`
}

// SynthesisRef is one Phase 2 cluster→synthesis output.
type SynthesisRef struct {
	ID             string `json:"id"`
	SourceCount    int    `json:"source_count"`
	SummaryExcerpt string `json:"summary_excerpt,omitempty"`
}

// MapEmergenceRef is one Phase 4 newly-eligible map.
type MapEmergenceRef struct {
	MapKey      string `json:"map_key"`
	MemoryCount int    `json:"memory_count"`
}

// AugmentRef is one Phase 6 oracle_augmented memory.
type AugmentRef struct {
	MapKey     string `json:"map_key"`
	MemoryID   string `json:"memory_id"`
	Tokens     int    `json:"tokens"`
	ResearchID string `json:"research_id,omitempty"`
}

// CrossRegionRef is one Phase 7 association across regions.
type CrossRegionRef struct {
	AID        string  `json:"a_id"`
	BID        string  `json:"b_id"`
	Similarity float64 `json:"similarity"`
	TagOverlap float64 `json:"tag_overlap"`
	AExcerpt   string  `json:"a_excerpt,omitempty"`
	BExcerpt   string  `json:"b_excerpt,omitempty"`
}

// SchemaRef is one Phase 8 meta-synthesis.
type SchemaRef struct {
	ID                 string `json:"id"`
	SourceCount        int    `json:"source_count"`
	AbstractionExcerpt string `json:"abstraction_excerpt,omitempty"`
}

// ReplayRef is one Phase 10 replay output.
type ReplayRef struct {
	ProjectMap  string `json:"project_map"`
	ReplayID    string `json:"replay_id"`
	SourceCount int    `json:"source_count"`
	Excerpt     string `json:"excerpt,omitempty"`
}

// ContextMemoryRef is one Phase 8.7 context-memory output. Confidence is
// the self-reported low|medium|high from Tier 2; UsedAugment is [R15]'s
// fallback indicator.
type ContextMemoryRef struct {
	MemoryID    string `json:"memory_id"`
	Region      string `json:"region"`
	SourceCount int    `json:"source_count"`
	Confidence  string `json:"confidence"`
	UsedAugment bool   `json:"used_augment,omitempty"`
	Excerpt     string `json:"excerpt,omitempty"`
}

// NightlyRunStepsFn is the pluggable consolidation pipeline. The default
// no-op returns nil; P5-8/9/10 replaces it with the real Tier 2 work.
// Receives the run id for logging context.
type NightlyRunStepsFn func(ctx context.Context, runID string) error

// NightlyNarrativeFn turns the run's stats into a 1-3 sentence prose
// summary. Returns (text, tokens_in, tokens_out, model, error). Errors are
// non-fatal — the run completes without a narrative.
type NightlyNarrativeFn func(ctx context.Context, stats NightlyStats) (text string, tokensIn, tokensOut int, model string, err error)

// NightlyRunner is the framework that drives one Dream Cycle at a time.
type NightlyRunner struct {
	bank   *Bank
	hub    *Hub
	router *ModelRouter
	// Wave 7b2 — optional lifecycle manager. When set, DoRun ensures
	// any service with WarmForWorkflow="nightly_run" is started before
	// the pipeline begins and signals burst-complete afterward.
	lifecycle *LifecycleManager

	// Polling cadence for the auto-loop. Default: 5 minutes.
	pollInterval time.Duration

	// Injectable for tests / future P5-8 work.
	runStepsFn  NightlyRunStepsFn
	narrativeFn NightlyNarrativeFn
	nowFn       func() time.Time // for time-skew tests

	// pipelineStats accumulates per-phase artefacts during a run. Distinct
	// from the v1 stats (which tracked only counts via before/after
	// snapshots). When the dream pipeline is wired in, runStepsFn fills
	// this so the framework's stats blob includes the rich shape.
	pipelineStats *NightlyStats

	// bubbleEnqueue, when non-nil, lets phases 1/2/6/8 ask the bubble
	// generator to refresh a memory's bubble after it gets merged or
	// created. Set from main.go after the bubble generator boots.
	bubbleEnqueue func(memID string)

	// Single-instance gate.
	mu        sync.Mutex
	inFlight  atomic.Bool
	currentID atomic.Value // string

	// 2026-05-23 (Feature #6) — current phase + phase start time for the
	// in-flight run. Read by /nightly/runs to populate `current_phase` +
	// `phase_started_at` on the in_progress row so polling clients see
	// real-time progress without having to subscribe to the WS. Updated
	// by pipelineContext.emitProgress(); cleared when DoRun returns.
	currentPhase     atomic.Value // string
	phaseStartedAt   atomic.Value // string (RFC3339Nano)
}

// SetCurrentPhase records the active phase name + a start timestamp.
// Called from pipelineContext.emitProgress so the value survives across
// emitProgress calls within a run AND is visible to /nightly/runs polls.
func (n *NightlyRunner) SetCurrentPhase(phase string) {
	if n == nil {
		return
	}
	n.currentPhase.Store(phase)
	n.phaseStartedAt.Store(time.Now().UTC().Format(time.RFC3339Nano))
}

// CurrentPhase returns the in-flight run's phase + the time that phase
// started. Both empty when no run is active (or the runner is nil).
func (n *NightlyRunner) CurrentPhase() (phase string, startedAt string) {
	if n == nil {
		return "", ""
	}
	if v, ok := n.currentPhase.Load().(string); ok {
		phase = v
	}
	if v, ok := n.phaseStartedAt.Load().(string); ok {
		startedAt = v
	}
	return
}

// SetBubbleEnqueue wires the bubble-generator's invalidate-and-enqueue hook
// into the runner. Optional — runs without it are silent on bubble freshness.
func (n *NightlyRunner) SetBubbleEnqueue(fn func(memID string)) {
	if n != nil {
		n.bubbleEnqueue = fn
	}
}

// NewNightlyRunner wires the framework. router/hub may be nil (the runner
// degrades cleanly: no narrative, no WS emission). bank is required.
func NewNightlyRunner(bank *Bank, router *ModelRouter, hub *Hub) *NightlyRunner {
	r := &NightlyRunner{
		bank:         bank,
		hub:          hub,
		router:       router,
		pollInterval: 5 * time.Minute,
		nowFn:        func() time.Time { return time.Now().UTC() },
	}
	// Default step function: the dream pipeline. Closes over `r` so phases
	// can charge the budget, hit the hub, and use the configured router.
	// pipelineStats is allocated per-run inside DoRun so concurrent runs
	// (which the single-instance gate forbids, but: defensive) don't share.
	r.runStepsFn = func(ctx context.Context, runID string) error {
		if r.pipelineStats == nil {
			r.pipelineStats = &NightlyStats{}
		}
		return r.runDreamPipeline(ctx, runID, r.pipelineStats)
	}
	// Default narrative: ask Tier 2 if available, else skip silently.
	r.narrativeFn = r.defaultNarrative
	return r
}

// IsRunning reports whether a Dream Cycle is currently executing, plus the
// run id so the HTTP handler can include it in a 409 conflict response.
func (n *NightlyRunner) IsRunning() (bool, string) {
	if !n.inFlight.Load() {
		return false, ""
	}
	id, _ := n.currentID.Load().(string)
	return true, id
}

// runID returns the canonical id format. Stable so MCP tool sd_trigger_nightly
// can predict + return ids before they hit the dashboard.
func (n *NightlyRunner) runID(t time.Time) string {
	return "nightly-" + t.Format("2006-01-02-15-04")
}

// llmCallTimeout returns the per-LLM-call deadline for any Chat-style
// inference call originating at the NightlyRunner layer (narrativeFn,
// generateDreamEntry). Reads from the dream-pipeline settings — same
// knob the per-phase code uses via pipelineContext.llmCallTimeout.
// Falls back to 120s on any read failure; never returns zero.
//
// Wave 8e — replaces the previously-hardcoded 60s (narrative) and 90s
// (dream entry) ceilings with the unified setting.
func (n *NightlyRunner) llmCallTimeout() time.Duration {
	const fallback = 120 * time.Second
	if n == nil || n.bank == nil {
		return fallback
	}
	s, err := n.bank.GetDreamPipelineSettings()
	if err != nil || s.LLMCallTimeoutSec <= 0 {
		return fallback
	}
	return time.Duration(s.LLMCallTimeoutSec) * time.Second
}

// DoRun executes one Dream Cycle synchronously. Returns the persisted row
// (post-update, status=completed | failed | partial) or an error if a
// run was already in progress (ErrNightlyAlreadyRunning).
//
// Flow:
//
//   1. TryLock — fail fast with ErrNightlyAlreadyRunning if locked.
//   2. INSERT in_progress row.
//   3. Snapshot stats BEFORE work.
//   4. runStepsFn(ctx) — the consolidation pipeline (no-op by default).
//   5. Snapshot stats AFTER. Compute deltas.
//   6. (Optional) narrativeFn — generates 1-3 sentence prose. Best-effort.
//   7. UPDATE row to completed (or failed if step error / partial).
//   8. Charge token budget for narrative tokens.
//   9. Append one audit_log summary entry (NEVER includes the narrative —
//      keeps the audit row small and avoids leaking generated prose into
//      a feed that may be rendered elsewhere).
//   10. Emit `nightly.completed` WS event with summary payload.
func (n *NightlyRunner) DoRun(ctx context.Context) (NightlyRun, error) {
	if n == nil || n.bank == nil {
		return NightlyRun{}, errors.New("nightly runner not configured")
	}

	// v2.6 Bundle B — pipeline-wide retry budget. Caps total backoff time
	// across all withRateLimitRetry calls in this run. Read from the
	// `nightly_retry_budget` setting (Config → AI/API); default 0 means
	// unlimited — preserves v2.5 behaviour for users who haven't opted
	// into a cap. Users who want to bound worst-case nightly stall time
	// (e.g. 20 retries × 62 s = ~20 minutes max) set a positive integer.
	// WithRetryBudget treats 0 / negative as "no budget attached", so
	// tryConsumeRetry continues to return true unconditionally for
	// unconfigured deployments.
	ctx = WithRetryBudget(ctx, n.bank.NightlyRetryBudget())

	// 1. Single-instance gate. Use TryLock so the caller gets a fast 409.
	if !n.mu.TryLock() {
		isRunning, id := n.IsRunning()
		_ = isRunning // we know it's true; surface the id
		return NightlyRun{ID: id}, ErrNightlyAlreadyRunning
	}
	// Mark in-flight ASAP so concurrent callers see it via IsRunning() too.
	n.inFlight.Store(true)
	defer func() {
		// Clear per-run progress markers so a stale CurrentPhase() doesn't
		// linger after DoRun returns (would otherwise mis-report a
		// completed run as still "in phase X").
		n.currentPhase.Store("")
		n.phaseStartedAt.Store("")
		n.inFlight.Store(false)
		n.mu.Unlock()
	}()

	// 1.5. Reconcile any stale in-progress rows from a prior dead process.
	// Cheap; skipped silently on error (don't block a fresh run on a
	// reconciliation hiccup). (Handoff "dream cycle resumption + crash
	// recovery 2026-05-10" §3b.)
	if reconciled, err := n.bank.ReconcileStaleInProgress(5 * time.Minute); err == nil && reconciled > 0 {
		log.Printf("nightly: DoRun reconciled %d stale in_progress run(s) -> interrupted", reconciled)
	}

	now := n.nowFn()
	id := n.runID(now)
	n.currentID.Store(id)
	startedAt := now.Format(time.RFC3339Nano)

	// 2. Insert in_progress row WITH initial heartbeat. Heartbeat ticker
	// (below) keeps it bumped every 30s while the run is alive.
	rec := NightlyRun{
		ID:          id,
		StartedAt:   startedAt,
		Status:      "in_progress",
		HeartbeatAt: startedAt,
		CreatedAt:   startedAt,
	}
	if err := n.bank.InsertNightlyRun(rec); err != nil {
		return rec, fmt.Errorf("insert nightly run: %w", err)
	}

	// Heartbeat ticker — bumps heartbeat_at every 30s. Cancelled when DoRun
	// returns (defer below) so it doesn't outlive the run. The bank-side
	// UPDATE is gated on status='in_progress' so a late tick after the
	// final UPDATE landing status=completed/partial/failed is a no-op.
	heartbeatStop := make(chan struct{})
	defer close(heartbeatStop)
	go func(runID string) {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatStop:
				return
			case <-ticker.C:
				_ = n.bank.BumpNightlyHeartbeat(runID)
			}
		}
	}(id)

	// 3. Before snapshot.
	before := n.snapshotStats()
	tokensBeforeAll, _ := n.bank.GetTokensUsed(now.Format("2006-01-02"))

	// 3.5. Wave 7b2 — ensure any managed services configured with
	// WarmForWorkflow=["nightly_run"] are running before phases start.
	// Failures are non-fatal; the pipeline continues with whatever
	// providers are currently available + records the per-service errors
	// in phase_failures so the user can see what didn't come up.
	preEnsureErrors := []string{}
	if n.lifecycle != nil {
		// Only warm the sidecar matching the currently-active Tier 2
		// provider — not every sidecar with a `nightly_run` policy. Prior
		// behaviour ensured both ollama-tier2 AND airllm-tier2 by default,
		// which cold-started the inactive one (multi-minute layer-stream
		// for AirLLM) for zero benefit when the dream itself only used
		// the active provider. Active-filter computed from the router.
		var activeFilter map[string]bool
		if n.router != nil {
			activeFilter = map[string]bool{}
			if s := sidecarNameForTier2Provider(n.router.Tier2()); s != "" {
				activeFilter[s] = true
			}
		}
		for _, err := range n.lifecycle.EnsureForWorkflow(ctx, "nightly_run", activeFilter) {
			preEnsureErrors = append(preEnsureErrors, "lifecycle: "+err.Error())
		}
	}

	// 4. Run the consolidation steps. Wrap in recover so a panic still
	//    completes the row with status=failed. Allocate per-run pipeline
	//    stats so concurrent reads from /nightly/list don't see partial
	//    state from a prior run.
	n.pipelineStats = &NightlyStats{SchemaVersion: 2}
	stepErr := safeCall(func() error {
		return n.runStepsFn(ctx, id)
	})
	// Carry the pre-ensure failures forward into the run's phase_failures
	// so the user can correlate "service didn't start" with "phase produced 0".
	if len(preEnsureErrors) > 0 {
		n.pipelineStats.PhaseFailures = append(n.pipelineStats.PhaseFailures, preEnsureErrors...)
	}
	// 4.5. Wave 7b2 — signal burst-complete so on_demand_burst services
	// shut down once the pipeline exits. on_demand_lazy services keep
	// running and get reaped by the idle goroutine.
	if n.lifecycle != nil {
		n.lifecycle.SignalWorkflowComplete(ctx, "nightly_run")
	}

	// 5. Merge framework deltas with pipeline-reported stats. The pipeline
	//    fills the rich fields directly; the framework still computes the
	//    coarse before/after counts so legacy callers reading `encoded`,
	//    `merged`, etc. without the new arrays still see sensible numbers.
	//
	// SchemaVersion: phases stamp their own version (Phase 0b bumps to 3
	// when DeepEncoding is non-nil). Don't clobber with a framework default;
	// only fall back to 2 when the pipeline left it at 0 (degenerate case).
	after := n.snapshotStats()
	stats := *n.pipelineStats
	if stats.SchemaVersion == 0 {
		stats.SchemaVersion = 2
	}
	// If a phase didn't fill a coarse count, derive it from the snapshot
	// delta. (Phase 0 fills Encoded; Phase 1 fills Merged; Phase 2 fills
	// Consolidated; Phase 5 fills Pruned. The derivation is a fallback.)
	if stats.Encoded == 0 {
		stats.Encoded = after.lightEncoded - before.lightEncoded
	}
	if stats.Consolidated == 0 {
		stats.Consolidated = after.nightlyConsolidated - before.nightlyConsolidated
	}
	if stats.Merged == 0 {
		stats.Merged = after.merged - before.merged
	}
	if stats.Pruned == 0 {
		stats.Pruned = after.deleted - before.deleted
	}
	// LexiconPairs intentionally NOT backfilled from the snapshot. Phase 3
	// fills it directly from its post-rebuild row count; the snapshot view
	// can race with mid-phase concurrent writes and overwrite a legitimate
	// value with a stale one. If Phase 3 failed (added to PhaseFailures),
	// LexiconPairs stays at whatever Phase 3 set before erroring (typically
	// 0). See handoff "dream pipeline tuning 2026-05-10" Bug 2.
	// Negative deltas → 0.
	if stats.Encoded < 0 {
		stats.Encoded = 0
	}
	if stats.Consolidated < 0 {
		stats.Consolidated = 0
	}
	if stats.Merged < 0 {
		stats.Merged = 0
	}
	if stats.Pruned < 0 {
		stats.Pruned = 0
	}

	// 6. Narrative (best-effort). Skipped on stepErr (no point summarising a
	//    failed run with a model call).
	narrative := ""
	model := ""
	if stepErr == nil && n.narrativeFn != nil {
		// Bump lifecycle last_used_at right before the narrative call so
		// the idle reaper doesn't stop the Tier 2 sidecar mid-inference
		// (wave 8e — matters for AirLLM-class providers).
		if n.lifecycle != nil {
			n.lifecycle.SignalProviderUse("nightly_run")
		}
		nctx, cancel := context.WithTimeout(ctx, n.llmCallTimeout())
		text, tIn, tOut, mdl, err := n.narrativeFn(nctx, stats)
		cancel()
		if err == nil {
			narrative = text
			model = mdl
			stats.TokensIn = tIn
			stats.TokensOut = tOut
			stats.TokensTotal = tIn + tOut
		} else {
			log.Printf("nightly: narrative skipped: %v", err)
		}
	}

	// 7. Update row. Status: failed only when Phase 0 aborted (stepErr non-nil);
	// partial when stepErr=nil BUT some non-fatal phase failed (PhaseFailures
	// non-empty); completed otherwise.
	finishedAt := n.nowFn().Format(time.RFC3339Nano)
	status := "completed"
	errMsg := ""
	if stepErr != nil {
		status = "failed"
		errMsg = stepErr.Error()
	} else if len(stats.PhaseFailures) > 0 {
		status = "partial"
		errMsg = strings.Join(stats.PhaseFailures, " | ")
	}
	statsJSON, _ := json.Marshal(stats)
	if err := n.bank.UpdateNightlyRun(id, finishedAt, status, string(statsJSON),
		narrative, model, errMsg); err != nil {
		log.Printf("nightly: update row: %v", err)
	}

	// 7b. Phase 12 — Dream Entry. Skipped silently when run failed, when
	// Tier 2 is unconfigured, or when nothing meaningful happened. Errors
	// here NEVER taint the run — a Tier 2 hiccup writing prose shouldn't
	// flip a successful consolidation pass to partial.
	if stepErr == nil {
		if entry, archetype, dIn, dOut, dModel, derr := n.generateDreamEntry(ctx, id, stats); derr == nil && entry != "" {
			_ = n.bank.SetNightlyDreamEntry(id, entry, archetype)
			if dIn > 0 || dOut > 0 {
				external := false
				if t2 := n.router.Tier2(); t2 != nil {
					external = !t2.IsLocal()
				}
				_ = n.bank.AddTokensUsedV2(now.Format("2006-01-02"),
					string(Tier2), providerKindFromName(dModel), dModel, external, dIn, dOut)
			}
			_ = n.bank.AppendAudit(AuditEntry{
				Operation: "dream_entry_generated", EntityType: "nightly_run", EntityID: id,
				AfterJSON: fmt.Sprintf(`{"archetype":%q,"tokens_in":%d,"tokens_out":%d}`, archetype, dIn, dOut),
				AdapterID: "sd-core-nightly",
			})
		} else if derr != nil {
			log.Printf("nightly: dream entry skipped: %v", derr)
		}
	}

	// 8. Charge token budget for the narrative under tier2_provider with the
	//    actual provider kind so the dashboard's per-tier breakdown shows
	//    nightly spend separately from realtime / oracle. Top-up logic
	//    handles narrativeFn implementations that returned token counts
	//    but didn't charge themselves: we record the difference between
	//    what the day's bucket grew during the run vs. what stats claim.
	tokensAfterAll, _ := n.bank.GetTokensUsed(now.Format("2006-01-02"))
	consumedDuringRun := tokensAfterAll - tokensBeforeAll
	if stats.TokensTotal > 0 && consumedDuringRun < stats.TokensTotal {
		topUp := stats.TokensTotal - consumedDuringRun
		// Split the top-up proportionally across in/out so the per-tier
		// breakdown reflects the same shape the narrativeFn reported.
		topUpIn, topUpOut := splitTokensProportional(topUp, stats.TokensIn, stats.TokensOut)
		// `model` here is the provider's Name() (e.g. "ollama:llama3.1:8b").
		providerKind := providerKindFromName(model)
		external := false
		if t2 := n.router.Tier2(); t2 != nil {
			external = !t2.IsLocal()
		}
		if err := n.bank.AddTokensUsedV2(now.Format("2006-01-02"),
			string(Tier2), providerKind, model, external, topUpIn, topUpOut); err != nil {
			log.Printf("nightly: top-up token budget: %v", err)
		}
	}

	// 9. Single audit summary row.
	auditAfter := map[string]interface{}{
		"id":            id,
		"status":        status,
		"started_at":    startedAt,
		"finished_at":   finishedAt,
		"stats":         stats,
		"elapsed_ms":    elapsedMs(startedAt, finishedAt),
	}
	if errMsg != "" {
		auditAfter["error"] = truncate(errMsg, 500)
	}
	auditJSON, _ := json.Marshal(auditAfter)
	_ = n.bank.AppendAudit(AuditEntry{
		Operation:  "nightly_runner",
		EntityType: "nightly_run",
		EntityID:   id,
		AfterJSON:  string(auditJSON),
		Reason:     status,
		AdapterID:  "sd-core-nightly",
	})

	// 10. WS emission. Dot-namespace per the control-channel convention.
	if n.hub != nil {
		n.hub.Fanout(Event{
			SchemaVersion: SchemaVersion,
			Type:          "nightly.completed",
			Timestamp:     finishedAt,
			AdapterID:     "sd-core-nightly",
			Payload: map[string]interface{}{
				"run_id": id,
				"status": status,
				"stats":  stats,
			},
		})
	}

	log.Printf("nightly: %s status=%s elapsed=%dms tokens=%d",
		id, status, elapsedMs(startedAt, finishedAt), stats.TokensTotal)

	final, _ := n.bank.GetNightlyRun(id)
	return final, nil
}

// Run is the auto-loop goroutine. Polls every pollInterval and kicks off
// DoRun when:
//   (a) the configured nightly schedule is currently active, AND
//   (b) no run started today already (started_at within the schedule's tz day).
// Returns when ctx is cancelled.
func (n *NightlyRunner) Run(ctx context.Context) {
	if n == nil || n.bank == nil {
		log.Printf("nightly: disabled (bank nil)")
		return
	}
	log.Printf("nightly: auto-loop starting (poll=%s)", n.pollInterval)
	t := time.NewTicker(n.pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.maybeAutoTrigger(ctx)
		}
	}
}

func (n *NightlyRunner) maybeAutoTrigger(ctx context.Context) {
	sched, err := n.bank.GetNightlySchedule()
	if err != nil || !sched.Enabled {
		return
	}
	now := n.nowFn()
	if !sched.IsActive(now) {
		return
	}
	if running, _ := n.IsRunning(); running {
		return
	}
	// Multi-dream-per-night gating. Replaces the legacy "once per day"
	// alreadyRanToday short-circuit. Decision tree:
	//
	//   1. Count completed cycles since the current window opened.
	//   2. If DreamsPerNightMax > 0 and we've hit it → skip.
	//   3. If this is cycle #1 of the night → ALWAYS fire (calibration —
	//      we need the first cycle's duration to estimate further ones).
	//   4. Else compute avg duration from completed cycles. Compare to
	//      time remaining in the window with a 10% slack. If a cycle
	//      wouldn't fit → skip. Otherwise → fire.
	//
	// The 5-min poll cadence is what serialises cycles: while a cycle is
	// running, IsRunning() returns true above and we wait until next
	// tick. After it finishes, the next tick re-enters this function,
	// re-evaluates, and fires the next cycle if there's time.
	settings, _ := n.bank.GetDreamPipelineSettings()
	windowStart := sched.WindowStartAt(now)
	windowEnd := sched.WindowEndAt(now)
	if windowStart.IsZero() || windowEnd.IsZero() {
		return // defensive: schedule mis-parses → don't fire
	}
	completed, avgDuration := n.cyclesSinceWindowOpen(windowStart)
	if settings.DreamsPerNightMax > 0 && completed >= settings.DreamsPerNightMax {
		return // user cap reached
	}
	if completed > 0 {
		// Not the calibration cycle. Decide whether the next cycle fits.
		remaining := windowEnd.Sub(now)
		// 10% safety margin so an above-average cycle doesn't run out of
		// window mid-stream. Apply against the longest reasonable single-
		// cycle estimate: max(avg, 5min) so a sub-5-min calibration
		// doesn't let us cram cycles into the last 90 seconds.
		minEstimate := 5 * time.Minute
		estimate := avgDuration
		if estimate < minEstimate {
			estimate = minEstimate
		}
		needed := time.Duration(float64(estimate) * 1.1)
		if remaining < needed {
			return // not enough window left for another cycle to finish cleanly
		}
	}
	if _, err := n.DoRun(ctx); err != nil && !errors.Is(err, ErrNightlyAlreadyRunning) {
		log.Printf("nightly: auto-trigger DoRun: %v", err)
	}
}

// cyclesSinceWindowOpen reports how many nightly runs started inside the
// currently-active window AND have a finished_at set (i.e. completed,
// failed, partial, or interrupted — anything non-"in_progress"), plus
// the mean wall-clock duration of those runs. Used by maybeAutoTrigger
// to decide whether to fire the next cycle.
//
// `failed` runs ARE counted toward completion (they ate time even if
// they didn't produce useful output). `interrupted` runs are NOT
// counted toward avg (they were killed mid-stream so their duration
// isn't representative); they DO count toward `completed` so a crash
// loop doesn't auto-trigger a fresh cycle every poll.
func (n *NightlyRunner) cyclesSinceWindowOpen(windowStart time.Time) (completed int, avgDuration time.Duration) {
	if n == nil || n.bank == nil {
		return 0, 0
	}
	runs, err := n.bank.ListNightlyRuns(NightlyRunListOpts{Limit: 100})
	if err != nil || len(runs) == 0 {
		return 0, 0
	}
	var totalDur time.Duration
	var durSamples int
	for _, r := range runs {
		started, err := time.Parse(time.RFC3339Nano, r.StartedAt)
		if err != nil {
			started, err = time.Parse(time.RFC3339, r.StartedAt)
			if err != nil {
				continue
			}
		}
		if started.Before(windowStart) {
			continue // older than this window
		}
		if r.FinishedAt == "" {
			continue // still in flight (the IsRunning gate above handles this)
		}
		completed++
		if r.Status == "interrupted" {
			continue // don't pollute avg with mid-stream kills
		}
		finished, err := time.Parse(time.RFC3339Nano, r.FinishedAt)
		if err != nil {
			finished, err = time.Parse(time.RFC3339, r.FinishedAt)
			if err != nil {
				continue
			}
		}
		dur := finished.Sub(started)
		if dur <= 0 {
			continue // clock skew or buggy timestamps
		}
		totalDur += dur
		durSamples++
	}
	if durSamples > 0 {
		avgDuration = totalDur / time.Duration(durSamples)
	}
	return completed, avgDuration
}

// alreadyRanToday checks the most recent run's started_at against today's
// date in the schedule's timezone. Defensive — if the latest row's status
// is `failed`, we DO NOT skip; the user should be free to retry the same day.
func (n *NightlyRunner) alreadyRanToday(now time.Time, sched NightlySchedule) bool {
	loc := time.Local
	if sched.Timezone != "" {
		if l, err := time.LoadLocation(sched.Timezone); err == nil {
			loc = l
		}
	}
	today := now.In(loc).Format("2006-01-02")
	runs, err := n.bank.ListNightlyRuns(NightlyRunListOpts{Limit: 5})
	if err != nil || len(runs) == 0 {
		return false
	}
	for _, r := range runs {
		if r.Status == "failed" {
			continue // failed runs don't block a retry the same day
		}
		t, err := time.Parse(time.RFC3339Nano, r.StartedAt)
		if err != nil {
			t, err = time.Parse(time.RFC3339, r.StartedAt)
			if err != nil {
				continue
			}
		}
		if t.In(loc).Format("2006-01-02") == today {
			return true
		}
	}
	return false
}

// ──────────────────────────────────────────────────────────────────────────
// Snapshots & helpers
// ──────────────────────────────────────────────────────────────────────────

type nightlySnapshot struct {
	lightEncoded        int
	nightlyConsolidated int
	merged              int
	deleted             int
	lexiconPairs        int
	memoryMaps          int
}

// snapshotStats reads count(*) on the relevant predicates. Cheap (single-
// digit ms on a few-thousand-row bank). Errors are swallowed — a missing
// snapshot just means the delta shows as 0, which is honest.
func (n *NightlyRunner) snapshotStats() nightlySnapshot {
	var s nightlySnapshot
	if n == nil || n.bank == nil {
		return s
	}
	_ = n.bank.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE light_encoded = 1 AND deleted_at = ''`).Scan(&s.lightEncoded)
	_ = n.bank.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE nightly_consolidated = 1 AND deleted_at = ''`).Scan(&s.nightlyConsolidated)
	_ = n.bank.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE merged_from <> '[]' AND merged_from <> '' AND deleted_at = ''`).Scan(&s.merged)
	_ = n.bank.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE deleted_at <> ''`).Scan(&s.deleted)
	_ = n.bank.db.QueryRow(`SELECT COUNT(*) FROM lexicon`).Scan(&s.lexiconPairs)
	_ = n.bank.db.QueryRow(`SELECT COUNT(*) FROM memory_maps`).Scan(&s.memoryMaps)
	return s
}

// defaultNarrative asks Tier 2 to summarise the run as Phase 11 of the
// dream pipeline. Skipped (returns "") when Tier 2 is unconfigured.
//
// The prompt is upgraded for v2 stats — instead of dumping the raw stats
// JSON, it foregrounds the per-phase artefacts (specific merges, specific
// cross-region links, specific abstractions) so the output reads like a
// sleep-lab observation rather than a counter dump. Length cap raised
// from 500 → 800 chars to accommodate richer content.
func (n *NightlyRunner) defaultNarrative(ctx context.Context, stats NightlyStats) (string, int, int, string, error) {
	if n.router == nil {
		return "", 0, 0, "", errors.New("no router configured")
	}
	provider := n.router.ForNightly()
	if provider == nil {
		return "", 0, 0, "", errors.New("Tier 2 not configured")
	}
	var b strings.Builder
	b.WriteString(`You are narrating a night of REM-style memory consolidation for a personal brain. Write 3-5 sentences in a calm, observational voice — like a sleep researcher describing what the brain did overnight, not a system summary. Be concrete: name specific topics, specific cross-domain connections, specific abstractions. Cite numbers only when they're load-bearing. No preamble, no bullet points, no "tonight your mind".`)
	b.WriteString("\n\nTONIGHT'S CYCLE:\n")
	fmt.Fprintf(&b, "- Encoded %d new traces; merged %d near-duplicate pairs.\n", stats.Encoded, stats.Merged)
	if len(stats.Syntheses) > 0 {
		fmt.Fprintf(&b, "- Synthesised %d clusters: ", stats.Consolidated)
		for i, s := range stats.Syntheses {
			if i > 2 {
				break
			}
			if i > 0 {
				b.WriteString("; ")
			}
			b.WriteString(s.SummaryExcerpt)
		}
		b.WriteString("\n")
	}
	if len(stats.Schemas) > 0 {
		fmt.Fprintf(&b, "- Built %d new schema(s) from prior syntheses: ", len(stats.Schemas))
		for i, s := range stats.Schemas {
			if i > 1 {
				break
			}
			if i > 0 {
				b.WriteString("; ")
			}
			b.WriteString(s.AbstractionExcerpt)
		}
		b.WriteString("\n")
	}
	if len(stats.CrossRegionLinks) > 0 {
		fmt.Fprintf(&b, "- Discovered %d cross-domain associations:\n", len(stats.CrossRegionLinks))
		for i, c := range stats.CrossRegionLinks {
			if i > 2 {
				break
			}
			fmt.Fprintf(&b, "  • %q ⟷ %q (sim %.2f)\n", c.AExcerpt, c.BExcerpt, c.Similarity)
		}
	}
	if len(stats.MapsEmerged) > 0 {
		b.WriteString("- Maps emerged: ")
		for i, m := range stats.MapsEmerged {
			if i > 3 {
				break
			}
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(m.MapKey)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "- Reinforced %d recently-recalled memories; pruned %d dormant ones.\n",
		stats.ReinforcedCount, stats.Pruned)
	if len(stats.Augmented) > 0 {
		b.WriteString("- Researched: ")
		for i, a := range stats.Augmented {
			if i > 3 {
				break
			}
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(a.MapKey)
		}
		b.WriteString("\n")
	}
	if len(stats.Replays) > 0 {
		fmt.Fprintf(&b, "- Replayed %s: %s\n", stats.Replays[0].ProjectMap, stats.Replays[0].Excerpt)
	}
	prompt := b.String()
	// 400-token budget so the model can finish its 3-5 sentences naturally
	// rather than being cut mid-thought by too small a budget.
	out, err := provider.Chat(ctx, []Message{{Role: "user", Content: prompt}}, 400)
	if err != nil {
		return "", 0, 0, "", err
	}
	out = strings.TrimSpace(out)
	// Cap the narrative AND always end on a complete sentence. The old blind
	// `out[:800]` chopped the last sentence mid-word (e.g. "...optimizing
	// its"), which surfaced in the dashboard's dream journal. Cap at 1100
	// (room for the full 3-5 sentences the prompt asks for); then, if the
	// text doesn't end on terminal punctuation (the cap OR the token budget
	// cut it mid-thought), trim back to the last complete sentence so a
	// mid-word cut is never displayed.
	const narrativeCap = 1100
	if len(out) > narrativeCap {
		out = out[:narrativeCap]
	}
	if n := len(out); n > 0 {
		switch out[n-1] {
		case '.', '!', '?', '"':
			// already ends cleanly
		default:
			if i := strings.LastIndexAny(out, ".!?"); i > n/2 {
				out = strings.TrimSpace(out[:i+1])
			}
		}
	}
	tIn := estimateTokens(prompt)
	tOut := estimateTokens(out)
	return out, tIn, tOut, provider.Name(), nil
}

// safeCall runs fn and converts any panic into an error. Keeps the
// nightly_runs row consistent even when a step blows up unexpectedly.
func safeCall(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return fn()
}

// elapsedMs computes finished - started in milliseconds. Returns 0 on
// parse error (defensive — never crashes the audit row).
func elapsedMs(startedAt, finishedAt string) int64 {
	s, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		s, _ = time.Parse(time.RFC3339, startedAt)
	}
	f, err := time.Parse(time.RFC3339Nano, finishedAt)
	if err != nil {
		f, _ = time.Parse(time.RFC3339, finishedAt)
	}
	return f.Sub(s).Milliseconds()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// splitTokensProportional divides `total` into in/out parts that mirror
// the ratio (refIn : refOut). Used for budget top-up so a narrative
// reporting 100in/200out gets re-recorded with the same shape even when
// only the sum is known.
func splitTokensProportional(total, refIn, refOut int) (int, int) {
	if total <= 0 {
		return 0, 0
	}
	ref := refIn + refOut
	if ref <= 0 {
		// No reference — split all to "in" so it's still recorded.
		return total, 0
	}
	in := total * refIn / ref
	out := total - in
	return in, out
}
