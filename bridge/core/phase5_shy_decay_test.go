// phase5_shy_decay_test.go — tests for [R2] weighted SHY decay.
//
// Handoff: `CODE_HANDOFF — R-refinement followups (2026-05-10).md` §R2.
// CITATIONS.md #4 (Tononi & Cirelli SHY 2014) + #5 (2020 update).
package main

import (
	"testing"
	"time"
)

// shyTestCtx wires a pipelineContext with R2-relevant settings dialed up
// so we can observe decay behavior in a single phase invocation.
func shyTestCtx(t *testing.T, bank *Bank) *pipelineContext {
	t.Helper()
	pc := newPipelineContextForTest(t, bank)
	// Make eligibility predicates loose so the test isolates the decay math.
	pc.settings.DecayAgeDays = 1
	pc.settings.DecayRecallDays = 1
	pc.settings.DecayMaxPerRun = 100
	pc.settings.DecayBaseRate = 0.20      // amplify the per-cycle bleed for visibility
	pc.settings.DecayStrengthFloor = 0.05 // standard floor
	return pc
}

func TestPhase5SHY_HighSalienceProtected(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{
		Text: "important", Tags: []string{"low_priority"}, // tag-eligible
	})
	// Simulate a salient, well-trodden memory aged past the decay threshold.
	bank.db.Exec(`UPDATE memories SET
		light_encoded = 1, salience = 0.95, recall_strength = 4.5,
		created_at = ?, last_recalled_at = ''
		WHERE id = ?`,
		"2020-01-01T00:00:00Z", rec.ID)

	pc := shyTestCtx(t, bank)
	if err := pc.runPhase5Decay(); err != nil {
		t.Fatalf("Phase 5: %v", err)
	}
	got, _ := bank.GetMemory(rec.ID)
	if got.DeletedAt != "" {
		t.Errorf("high-salience memory should NOT be pruned, got deleted_at=%q", got.DeletedAt)
	}
	// Strength should still drop a tiny bit (no protection is total) but not
	// below ~3.5 from a starting 4.5 with high protection.
	if got.RecallStrength < 3.5 {
		t.Errorf("expected mild decay only on protected memory, got strength=%g", got.RecallStrength)
	}
}

func TestPhase5SHY_WeakUnprotectedDecays(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "trivial"}) // no tags
	bank.db.Exec(`UPDATE memories SET
		light_encoded = 1, salience = 0.1, recall_strength = 0.4,
		created_at = ?, last_recalled_at = ''
		WHERE id = ?`,
		"2020-01-01T00:00:00Z", rec.ID)

	pc := shyTestCtx(t, bank)
	pc.runPhase5Decay()

	got, _ := bank.GetMemory(rec.ID)
	// With 20% base rate and ~95% unprotected: decayed = 0.4 * (1 - 0.2*0.95) ≈ 0.32
	// Strength MUST have decreased.
	if got.RecallStrength >= 0.4 {
		t.Errorf("weak unprotected memory should decay, got strength=%g (was 0.4)", got.RecallStrength)
	}
}

func TestPhase5SHY_DecaysToFloorAndPrunes(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{
		Text: "ancient junk", Tags: []string{"noise"},
	})
	bank.db.Exec(`UPDATE memories SET
		light_encoded = 1, salience = 0.0, recall_strength = 0.04,
		created_at = ?, last_recalled_at = ''
		WHERE id = ?`,
		"2020-01-01T00:00:00Z", rec.ID)

	pc := shyTestCtx(t, bank)
	pc.runPhase5Decay()

	got, _ := bank.GetMemory(rec.ID)
	if got.DeletedAt == "" {
		t.Errorf("memory at strength %g should be pruned at floor %g", got.RecallStrength, pc.settings.DecayStrengthFloor)
	}
	if got.DeletedReason != "nightly_decay" {
		t.Errorf("expected deleted_reason=nightly_decay, got %q", got.DeletedReason)
	}
}

func TestPhase5SHY_NeverDecaysSensitive(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{
		Text: "private", Tags: []string{"noise"},
		Sensitive: true,
	})
	bank.db.Exec(`UPDATE memories SET
		light_encoded = 1, salience = 0.0, recall_strength = 0.04,
		created_at = ?, last_recalled_at = ''
		WHERE id = ?`,
		"2020-01-01T00:00:00Z", rec.ID)

	beforeStrength := 0.04
	pc := shyTestCtx(t, bank)
	pc.runPhase5Decay()

	got, _ := bank.GetMemory(rec.ID)
	if got.DeletedAt != "" {
		t.Errorf("sensitive memory must not be decayed/pruned, got deleted_at=%q", got.DeletedAt)
	}
	if got.RecallStrength != beforeStrength {
		t.Errorf("sensitive memory strength should be unchanged; got %g (was %g)",
			got.RecallStrength, beforeStrength)
	}
}

func TestPhase5SHY_UnEncodedSkipped(t *testing.T) {
	// Memories that haven't been light-encoded yet must NOT participate in
	// decay — they're still in flight from the synapse builder.
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "fresh"})
	bank.db.Exec(`UPDATE memories SET
		light_encoded = 0, salience = 0.1, recall_strength = 0.3
		WHERE id = ?`, rec.ID)

	pc := shyTestCtx(t, bank)
	pc.runPhase5Decay()

	got, _ := bank.GetMemory(rec.ID)
	if got.RecallStrength != 0.3 {
		t.Errorf("unencoded memory shouldn't participate in decay; strength changed to %g", got.RecallStrength)
	}
}

func TestPhase5SHY_DecayMaxPerRunCap(t *testing.T) {
	bank := newTestBank(t)
	for i := 0; i < 5; i++ {
		rec, _ := bank.SaveMemory(MemoryRecord{
			Text: "low " + string(rune('a'+i)),
			Tags: []string{"noise"},
		})
		bank.db.Exec(`UPDATE memories SET
			light_encoded = 1, salience = 0.0, recall_strength = 0.04,
			created_at = ?, last_recalled_at = ''
			WHERE id = ?`,
			"2020-01-01T00:00:00Z", rec.ID)
	}
	pc := shyTestCtx(t, bank)
	pc.settings.DecayMaxPerRun = 2 // cap to 2 prunes
	pc.runPhase5Decay()

	if pc.stats.Pruned != 2 {
		t.Errorf("expected pruned=2 (cap), got %d", pc.stats.Pruned)
	}
}

func TestApproxEqual(t *testing.T) {
	// Sanity for the helper.
	cases := []struct {
		a, b, tol float64
		want      bool
	}{
		{1.0, 1.0001, 0.001, true},
		{1.0, 1.01, 0.001, false},
		{0.05, 0.0501, 0.001, true},
		{-1.0, 1.0, 0.001, false},
	}
	for _, c := range cases {
		got := approxEqual(c.a, c.b, c.tol)
		if got != c.want {
			t.Errorf("approxEqual(%g,%g,%g)=%v want %v", c.a, c.b, c.tol, got, c.want)
		}
	}
}

func TestPhase5SHY_StrengthFloorSettingsRoundTrip(t *testing.T) {
	bank := newTestBank(t)
	in := DefaultDreamPipelineSettings()
	in.DecayBaseRate = 0.07
	in.DecayStrengthFloor = 0.10
	if err := bank.SaveDreamPipelineSettings(in); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, _ := bank.GetDreamPipelineSettings()
	if out.DecayBaseRate != 0.07 {
		t.Errorf("decay_base_rate roundtrip: got %g", out.DecayBaseRate)
	}
	if out.DecayStrengthFloor != 0.10 {
		t.Errorf("decay_strength_floor roundtrip: got %g", out.DecayStrengthFloor)
	}
	_ = time.Now() // silence unused import in case test gets pruned
}
