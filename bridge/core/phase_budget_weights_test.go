// phase_budget_weights_test.go — [R12] adaptive phase weighting.
// CITATIONS.md #20 (Sarangi 2021 — REM proportional to environmental novelty).
package main

import (
	"testing"
)

func TestPhaseBudgetWeights_BalancedQueue(t *testing.T) {
	w := computePhaseBudgetWeights(10, map[string]int{
		"created":  3,
		"periodic": 3,
		"edited":   2,
		"surprise": 2,
	})
	if w.Reason != "balanced" {
		t.Errorf("expected balanced, got %q", w.Reason)
	}
	if w.Phase0b != 1.0 || w.Phase7 != 1.0 || w.Phase8 != 1.0 || w.Phase10 != 1.0 {
		t.Errorf("expected all weights 1.0, got %+v", w)
	}
}

func TestPhaseBudgetWeights_EmptyQueue(t *testing.T) {
	w := computePhaseBudgetWeights(0, map[string]int{})
	if w.Reason != "balanced" {
		t.Errorf("expected balanced on empty queue, got %q", w.Reason)
	}
}

func TestPhaseBudgetWeights_CreatedDominantBoostsPhase0bAndReplay(t *testing.T) {
	w := computePhaseBudgetWeights(10, map[string]int{
		"created":  7,
		"periodic": 1,
		"edited":   2,
	})
	if w.Reason != "created_dominant" {
		t.Errorf("expected created_dominant, got %q", w.Reason)
	}
	if w.Phase0b != 1.4 {
		t.Errorf("expected Phase0b=1.4, got %g", w.Phase0b)
	}
	if w.Phase10 != 1.4 {
		t.Errorf("expected Phase10=1.4, got %g", w.Phase10)
	}
	if w.Phase7 != 1.0 || w.Phase8 != 1.0 {
		t.Errorf("expected Phase7/8 unboosted, got %+v", w)
	}
}

func TestPhaseBudgetWeights_NeverEncodedCountsAsCreated(t *testing.T) {
	// Cold-start: rows that have no prior deep_encoded_at AND no
	// dirty_reason land in the synthetic "never_encoded" bucket.
	w := computePhaseBudgetWeights(10, map[string]int{
		"never_encoded": 7,
		"periodic":      3,
	})
	if w.Reason != "created_dominant" {
		t.Errorf("expected never_encoded to count as created, got %q", w.Reason)
	}
}

func TestPhaseBudgetWeights_PeriodicDominantBoostsPhase7And8(t *testing.T) {
	w := computePhaseBudgetWeights(10, map[string]int{
		"periodic": 8,
		"created":  1,
		"edited":   1,
	})
	if w.Reason != "periodic_dominant" {
		t.Errorf("expected periodic_dominant, got %q", w.Reason)
	}
	if w.Phase7 != 1.4 || w.Phase8 != 1.4 {
		t.Errorf("expected Phase7/8=1.4, got %+v", w)
	}
	if w.Phase0b != 1.0 || w.Phase10 != 1.0 {
		t.Errorf("expected Phase0b/10 unboosted, got %+v", w)
	}
}

func TestPhaseBudgetWeights_CreatedTieBreaksToCreated(t *testing.T) {
	// 60/40 created/periodic — created path wins because the comparison
	// is "createdFrac >= periodicFrac". Documenting the tiebreaker.
	w := computePhaseBudgetWeights(10, map[string]int{
		"created":  6,
		"periodic": 4,
	})
	if w.Reason != "created_dominant" {
		t.Errorf("expected created_dominant at 60%%, got %q", w.Reason)
	}
}

func TestPhaseBudgetWeights_BelowThresholdStaysBalanced(t *testing.T) {
	// 50% periodic — below the 60% threshold, balanced regardless.
	w := computePhaseBudgetWeights(10, map[string]int{
		"periodic": 5,
		"created":  2,
		"edited":   3,
	})
	if w.Reason != "balanced" {
		t.Errorf("expected balanced at 50%%, got %q", w.Reason)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// applyPhaseBudgetWeights — verify the multipliers land on the right fields
// ──────────────────────────────────────────────────────────────────────────

func TestApplyPhaseBudgetWeights_CreatedDominantScalesPhase0b(t *testing.T) {
	pc := &pipelineContext{
		settings: DreamPipelineSettings{
			DeepEnrichMaxPerRun:   100,
			DeepEnrichTokenBudget: 50000,
			CrossRegionMaxPerRun:  20,
			SchemaMaxPerRun:       2,
		},
	}
	pc.applyPhaseBudgetWeights(PhaseBudgetWeights{
		Phase0b: 1.4, Phase7: 1.0, Phase8: 1.0, Phase10: 1.4,
		Reason: "created_dominant",
	})
	if pc.settings.DeepEnrichMaxPerRun != 140 {
		t.Errorf("DeepEnrichMaxPerRun: want 140 got %d", pc.settings.DeepEnrichMaxPerRun)
	}
	if pc.settings.DeepEnrichTokenBudget != 70000 {
		t.Errorf("DeepEnrichTokenBudget: want 70000 got %d", pc.settings.DeepEnrichTokenBudget)
	}
	if pc.settings.CrossRegionMaxPerRun != 20 {
		t.Errorf("CrossRegionMaxPerRun: want unchanged 20 got %d", pc.settings.CrossRegionMaxPerRun)
	}
	if pc.settings.SchemaMaxPerRun != 2 {
		t.Errorf("SchemaMaxPerRun: want unchanged 2 got %d", pc.settings.SchemaMaxPerRun)
	}
}

func TestApplyPhaseBudgetWeights_PeriodicDominantScalesPhase7And8(t *testing.T) {
	pc := &pipelineContext{
		settings: DreamPipelineSettings{
			DeepEnrichMaxPerRun:   100,
			DeepEnrichTokenBudget: 50000,
			CrossRegionMaxPerRun:  20,
			SchemaMaxPerRun:       10,
		},
	}
	pc.applyPhaseBudgetWeights(PhaseBudgetWeights{
		Phase0b: 1.0, Phase7: 1.4, Phase8: 1.4, Phase10: 1.0,
		Reason: "periodic_dominant",
	})
	if pc.settings.CrossRegionMaxPerRun != 28 {
		t.Errorf("CrossRegionMaxPerRun: want 28 got %d", pc.settings.CrossRegionMaxPerRun)
	}
	if pc.settings.SchemaMaxPerRun != 14 {
		t.Errorf("SchemaMaxPerRun: want 14 got %d", pc.settings.SchemaMaxPerRun)
	}
	if pc.settings.DeepEnrichMaxPerRun != 100 {
		t.Errorf("DeepEnrichMaxPerRun: want unchanged 100 got %d", pc.settings.DeepEnrichMaxPerRun)
	}
}

func TestApplyPhaseBudgetWeights_BalancedNoOp(t *testing.T) {
	pc := &pipelineContext{
		settings: DreamPipelineSettings{
			DeepEnrichMaxPerRun:   100,
			DeepEnrichTokenBudget: 50000,
			CrossRegionMaxPerRun:  20,
			SchemaMaxPerRun:       2,
		},
	}
	orig := pc.settings
	pc.applyPhaseBudgetWeights(PhaseBudgetWeights{
		Phase0b: 1.0, Phase7: 1.0, Phase8: 1.0, Phase10: 1.0,
		Reason: "balanced",
	})
	if pc.settings != orig {
		t.Errorf("balanced weights should leave settings untouched; got %+v want %+v", pc.settings, orig)
	}
}
