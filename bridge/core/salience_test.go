// salience_test.go — tests for R1 salience runtime computation.
package main

import (
	"testing"
	"time"
)

func TestEmotionalScore_PlainText(t *testing.T) {
	// Plain factual prose — minimal emotional signal expected.
	s := computeEmotionalScore("the function takes a context and returns an error", nil)
	if s > 0.05 {
		t.Errorf("plain prose should score near 0, got %g", s)
	}
}

func TestEmotionalScore_ExclamationsAndCaps(t *testing.T) {
	s := computeEmotionalScore("URGENT!! the deploy is broken — must rollback NOW", nil)
	if s < 0.5 {
		t.Errorf("urgent caps+! prose should score >0.5, got %g", s)
	}
	// Should respect the 1.0 cap (not produce values >1).
	s2 := computeEmotionalScore("URGENT!!! CRITICAL!! DISASTER!! MUST FIX NOW NOW NOW NOW NOW", nil)
	if s2 > 1.0 {
		t.Errorf("score exceeded cap: %g", s2)
	}
}

func TestEmotionalScore_LexiconHits(t *testing.T) {
	// Three lexicon hits → 0.30.
	s := computeEmotionalScore("the urgent panic broke our deploy", nil)
	if s < 0.3 || s > 0.5 {
		t.Errorf("expected ~0.30 for 3 lexicon hits, got %g", s)
	}
}

func TestEmotionalScore_TagAffectBonus(t *testing.T) {
	plain := computeEmotionalScore("the system did the thing", nil)
	withTag := computeEmotionalScore("the system did the thing", []string{"urgent"})
	if !(withTag > plain) {
		t.Errorf("tag affect should bump score: plain=%g tagged=%g", plain, withTag)
	}
}

func TestRecencyScore_Curve(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		ago  time.Duration
		want float64 // expected approximate
	}{
		{0, 1.0},
		{6 * 24 * time.Hour, 1.0},   // within 7-day window
		{45 * 24 * time.Hour, 0.46}, // mid-curve
		{91 * 24 * time.Hour, 0},    // beyond 90-day floor
		{365 * 24 * time.Hour, 0},   // far past
	}
	for _, c := range cases {
		ts := now.Add(-c.ago).Format(time.RFC3339Nano)
		got := computeRecencyScore(ts, now)
		// Tolerance ±0.1 — exact linear values are fragile.
		if got < c.want-0.1 || got > c.want+0.1 {
			t.Errorf("ago=%v: want ~%g, got %g", c.ago, c.want, got)
		}
	}
}

func TestRecencyScore_EmptyOrInvalid(t *testing.T) {
	now := time.Now().UTC()
	if computeRecencyScore("", now) != 0 {
		t.Errorf("empty timestamp should score 0")
	}
	if computeRecencyScore("not a timestamp", now) != 0 {
		t.Errorf("garbage timestamp should score 0")
	}
}

func TestComputeSalience_Blend(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	// Plain memory, no recall, default novelty 0.5: emotional≈0, novelty=0.5, recency=0
	// → 0.4*0 + 0.4*0.5 + 0.2*0 = 0.2
	s := computeSalience("plain factual prose", nil, 0.5, "", now)
	if s < 0.18 || s > 0.22 {
		t.Errorf("plain memory baseline near 0.2, got %g", s)
	}

	// Hot memory: high emotional + low novelty (similar to existing) + recent recall
	// emotional ~0.5, novelty 0.1, recency 1.0 → 0.4*0.5 + 0.4*0.1 + 0.2*1.0 = 0.44
	hot := computeSalience("URGENT! must fix this critical disaster",
		[]string{"important"}, 0.1, now.Add(-1*time.Hour).Format(time.RFC3339Nano), now)
	if hot < 0.4 {
		t.Errorf("hot memory should score >0.4, got %g", hot)
	}
}

func TestComputeSalience_Clamping(t *testing.T) {
	now := time.Now().UTC()
	// Negative novelty input clamps to 0.
	s := computeSalience("text", nil, -1.0, "", now)
	if s < 0 {
		t.Errorf("expected non-negative result, got %g", s)
	}
	// >1 novelty input clamps to 1.
	s2 := computeSalience("URGENT! panic disaster terrible", []string{"urgent"}, 5.0, now.Format(time.RFC3339Nano), now)
	if s2 > 1.0 {
		t.Errorf("expected clamped <=1, got %g", s2)
	}
}

func TestComputeNoveltyForMemory_NeutralOnEmpty(t *testing.T) {
	bank := newTestBank(t)
	got := computeNoveltyForMemory(bank, "ghost-id", "no-such-hash")
	if got != 0.5 {
		t.Errorf("expected 0.5 default when embedding missing, got %g", got)
	}
}

func TestSaveMemory_PopulatesSalience(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "URGENT panic disaster broken"})
	got, _ := bank.GetMemory(rec.ID)
	// Emotional content should drive salience above the 0.2 baseline.
	if got.Salience <= 0.2 {
		t.Errorf("emotional memory should have salience >0.2, got %g", got.Salience)
	}
}
