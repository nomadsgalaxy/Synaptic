// bundle_i_temporal_test.go — guards for the temporal-aware retrieval
// lesson learned from the LongMemEval Oracle baseline run.
//
// Symptom: temporal-reasoning category scored 5/14 (36%). Queries like
// "When did I first start using X?" returned the most cosine-similar
// X memory rather than the OLDEST X memory. Cosine alone cannot
// distinguish chronology.
//
// Ship: detect unambiguous temporal markers in the query and fuse a
// third candidate list — topical-prefilter sorted by created_at in
// the direction the marker implies — via the same RRF used by
// cosine+BM25 fusion.
//
// Setting `recall_temporal_enabled` (default ON). Same shape as the
// other recall toggles so users can disable.
package main

import (
	"testing"
	"time"
)

// ── detectTemporalIntent ────────────────────────────────────────────

func TestBundleI_TemporalIntent_Asc(t *testing.T) {
	asc := []string{
		"When did I first start using pnpm?",
		"What was the first time I tried sushi?",
		"Where did we originally meet?",
		"Tell me about my initial reaction.",
		"Where is the very first place I lived?",
		"When did I begin learning Go?",
	}
	for _, q := range asc {
		t.Run(q, func(t *testing.T) {
			if got := detectTemporalIntent(q); got != "asc" {
				t.Errorf("detectTemporalIntent(%q) = %q, want asc", q, got)
			}
		})
	}
}

func TestBundleI_TemporalIntent_Desc(t *testing.T) {
	desc := []string{
		"What's the most recent thing I worked on?",
		"When was the last time I exercised?",
		"What have I been working on lately?",
		"Anything new this week?",
		"What did I do today?",
		"When was my latest deployment?",
	}
	for _, q := range desc {
		t.Run(q, func(t *testing.T) {
			if got := detectTemporalIntent(q); got != "desc" {
				t.Errorf("detectTemporalIntent(%q) = %q, want desc", q, got)
			}
		})
	}
}

func TestBundleI_TemporalIntent_NoMarker(t *testing.T) {
	// Non-temporal queries should NOT trigger temporal fusion —
	// that would re-order results for queries where chronology is
	// irrelevant. Conservative detection is on purpose.
	queries := []string{
		"What's my favourite restaurant?",
		"How does TTL work in Bundle L?",
		"Tell me about the OAuth refresh flow.",
		"Who is Sarah?",
	}
	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			if got := detectTemporalIntent(q); got != "" {
				t.Errorf("detectTemporalIntent(%q) = %q, want '' (non-temporal)", q, got)
			}
		})
	}
}

// ── temporalRecallEnabled ───────────────────────────────────────────

func TestBundleI_TemporalEnabled_DefaultsOn(t *testing.T) {
	bank := newTestBank(t)
	if !temporalRecallEnabled(bank) {
		t.Errorf("temporal retrieval should default ON")
	}
	if !temporalRecallEnabled(nil) {
		t.Errorf("nil bank should default ON too")
	}
}

func TestBundleI_TemporalEnabled_HonoursOff(t *testing.T) {
	bank := newTestBank(t)
	for _, v := range []string{"0", "false", "off", "no"} {
		bank.SetSetting(TemporalRecallEnabledKey, v)
		if temporalRecallEnabled(bank) {
			t.Errorf("value %q should disable", v)
		}
	}
}

// ── recallTemporalIDs ──────────────────────────────────────────────

func TestBundleI_RecallTemporalIDs_AscAndDesc(t *testing.T) {
	bank := newTestBank(t)
	// Seed three memories with controlled created_at. SaveMemory
	// respects CreatedAt when non-empty.
	a, _ := bank.SaveMemory(MemoryRecord{
		Text: "first thing", CreatedAt: "2024-01-01T00:00:00Z",
	})
	b, _ := bank.SaveMemory(MemoryRecord{
		Text: "middle thing", CreatedAt: "2024-06-01T00:00:00Z",
	})
	c, _ := bank.SaveMemory(MemoryRecord{
		Text: "last thing", CreatedAt: "2024-12-01T00:00:00Z",
	})
	candidates := []string{c.ID, a.ID, b.ID} // intentionally not sorted

	ascIDs, err := recallTemporalIDs(bank, candidates, "asc", 3)
	if err != nil {
		t.Fatalf("recallTemporalIDs(asc): %v", err)
	}
	if len(ascIDs) != 3 || ascIDs[0] != a.ID || ascIDs[2] != c.ID {
		t.Errorf("asc order wrong: %v (expected %s,%s,%s)", ascIDs, a.ID, b.ID, c.ID)
	}

	descIDs, err := recallTemporalIDs(bank, candidates, "desc", 3)
	if err != nil {
		t.Fatalf("recallTemporalIDs(desc): %v", err)
	}
	if len(descIDs) != 3 || descIDs[0] != c.ID || descIDs[2] != a.ID {
		t.Errorf("desc order wrong: %v (expected %s,%s,%s)", descIDs, c.ID, b.ID, a.ID)
	}
}

func TestBundleI_RecallTemporalIDs_FiltersDeletedAndDormant(t *testing.T) {
	bank := newTestBank(t)
	live, _ := bank.SaveMemory(MemoryRecord{
		Text: "still here", CreatedAt: "2024-01-01T00:00:00Z",
	})
	deleted, _ := bank.SaveMemory(MemoryRecord{
		Text: "soft-deleted", CreatedAt: "2024-02-01T00:00:00Z",
	})
	dormant, _ := bank.SaveMemory(MemoryRecord{
		Text: "sleeping", CreatedAt: "2024-03-01T00:00:00Z",
	})
	bank.SoftDeleteMemory(deleted.ID)
	bank.UpdateMemory(dormant.ID, MemoryUpdate{Dormant: ptrBool(true)})

	got, _ := recallTemporalIDs(bank,
		[]string{live.ID, deleted.ID, dormant.ID}, "asc", 10)
	if len(got) != 1 || got[0] != live.ID {
		t.Errorf("expected only live memory, got %v", got)
	}
}

func TestBundleI_RecallTemporalIDs_EmptyCandidatesReturnsNothing(t *testing.T) {
	bank := newTestBank(t)
	bank.SaveMemory(MemoryRecord{Text: "x", CreatedAt: "2024-01-01T00:00:00Z"})
	got, err := recallTemporalIDs(bank, []string{}, "asc", 10)
	if err != nil {
		t.Errorf("empty candidates should not error, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty candidates should return empty, got %v", got)
	}
}

func TestBundleI_RecallTemporalIDs_InvalidDirection(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "x", CreatedAt: "2024-01-01T00:00:00Z"})
	got, _ := recallTemporalIDs(bank, []string{rec.ID}, "sideways", 10)
	if len(got) != 0 {
		t.Errorf("invalid direction should return empty, got %v", got)
	}
}

// time package import-gate (compiler doesn't complain when unused above)
var _ = time.Now
