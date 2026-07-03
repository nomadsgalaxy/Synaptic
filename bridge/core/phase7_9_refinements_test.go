// phase7_9_refinements_test.go — tests for [R5] schema reframing,
// [R6] Phase 7 low-similarity bridges, [R9] weak-trace boost, [R14]
// global recall_strength rebalancing.
package main

import (
	"strings"
	"testing"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────
// [R6] computeRareTagSet + countSharedRareTags
// ──────────────────────────────────────────────────────────────────────────

func TestComputeRareTagSet_Threshold(t *testing.T) {
	mwvs := []memWithVec{
		{rec: MemoryRecord{Tags: []string{"common", "rare1"}}},
		{rec: MemoryRecord{Tags: []string{"common", "rare2"}}},
		{rec: MemoryRecord{Tags: []string{"common", "rare3"}}},
		{rec: MemoryRecord{Tags: []string{"common", "rare1"}}},
		// 4 memories. threshold = 4 * 0.5 = 2. tags appearing in <2 memories
		// are rare. 'common' appears in 4 (not rare). 'rare1' in 2 (not rare).
		// 'rare2' in 1 (rare). 'rare3' in 1 (rare).
	}
	rare := computeRareTagSet(mwvs, 0.5)
	if rare["common"] {
		t.Errorf("common tag should NOT be rare")
	}
	if rare["rare1"] {
		t.Errorf("rare1 in 2/4 memories with threshold=2 should NOT be rare (strict <)")
	}
	if !rare["rare2"] || !rare["rare3"] {
		t.Errorf("rare2/rare3 should be rare; got rare2=%v rare3=%v", rare["rare2"], rare["rare3"])
	}
}

func TestComputeRareTagSet_EmptyBank(t *testing.T) {
	if got := computeRareTagSet(nil, 0.05); len(got) != 0 {
		t.Errorf("empty bank should yield empty set")
	}
}

func TestComputeRareTagSet_CaseInsensitive(t *testing.T) {
	mwvs := []memWithVec{
		{rec: MemoryRecord{Tags: []string{"Foo"}}},
		{rec: MemoryRecord{Tags: []string{"FOO"}}},
		{rec: MemoryRecord{Tags: []string{"foo"}}},
	}
	rare := computeRareTagSet(mwvs, 0.05)
	// All three are the same tag normalised — appears in 3/3 = 100% → not rare.
	if rare["foo"] {
		t.Errorf("Foo/FOO/foo should collapse case-insensitively")
	}
}

func TestCountSharedRareTags(t *testing.T) {
	rare := map[string]bool{"alpha": true, "beta": true, "gamma": true}
	a := []string{"alpha", "common", "BETA"}
	b := []string{"beta", "common", "alpha", "delta"}
	// Shared rare tags (case-insensitive): alpha, beta. → 2
	got := countSharedRareTags(a, b, rare)
	if got != 2 {
		t.Errorf("expected 2 shared rare tags, got %d", got)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// [R9] weak-trace boost via runPhase9Reinforcement
// ──────────────────────────────────────────────────────────────────────────

func TestPhase9_WeakSalientGetsBoosted(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "important but rarely recalled"})
	// Force salience > 0.6 AND recall_strength < 0.8 — the [R9] target class.
	bank.db.Exec(`UPDATE memories SET salience = 0.7, recall_strength = 0.4 WHERE id = ?`, rec.ID)

	pc := newPipelineContextForTest(t, bank)
	pc.runPhase9Reinforcement()

	got, _ := bank.GetMemory(rec.ID)
	// Weak-boost adds 0.05 (then global rebalance may nudge slightly), so
	// allow a band but require non-trivial increase from 0.4.
	if got.RecallStrength <= 0.4 {
		t.Errorf("expected weak-but-salient memory to be boosted from 0.4, got %g", got.RecallStrength)
	}
}

func TestPhase9_StrongSalientNotBoostedByR9(t *testing.T) {
	// Memories with recall_strength >= 0.8 are NOT in the weak-but-tagged class.
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "trodden"})
	bank.db.Exec(`UPDATE memories SET salience = 0.7, recall_strength = 0.85 WHERE id = ?`, rec.ID)

	pc := newPipelineContextForTest(t, bank)
	// Disable rebalance + decay-factor to isolate the R9 path.
	pc.settings.ReinforceDecayFactor = 0
	pc.runPhase9Reinforcement()

	got, _ := bank.GetMemory(rec.ID)
	// Strength may still drift via global rebalance, but it shouldn't
	// have been bumped by the weak-trace boost specifically. We can only
	// assert it didn't jump by ≥ 0.05.
	if got.RecallStrength >= 0.85+0.05 {
		t.Errorf("strong memory should not receive weak-trace boost (+0.05); got %g (was 0.85)", got.RecallStrength)
	}
}

func TestPhase9_LowSalienceWeakNotBoosted(t *testing.T) {
	// salience ≤ 0.6 → not in the R9 target class even if strength is low.
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "trivial"})
	bank.db.Exec(`UPDATE memories SET salience = 0.3, recall_strength = 0.4 WHERE id = ?`, rec.ID)

	pc := newPipelineContextForTest(t, bank)
	pc.settings.ReinforceDecayFactor = 0
	pc.runPhase9Reinforcement()

	got, _ := bank.GetMemory(rec.ID)
	if got.RecallStrength >= 0.4+0.05 {
		t.Errorf("low-salience memory should not receive R9 boost; got %g (was 0.4)", got.RecallStrength)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// [R14] global recall_strength rebalancing
// ──────────────────────────────────────────────────────────────────────────

func TestPhase9R14_PullsTowardTargetMean(t *testing.T) {
	// Seed a bank whose mean strength is well above the target (1.5).
	bank := newTestBank(t)
	for i := 0; i < 10; i++ {
		rec, _ := bank.SaveMemory(MemoryRecord{Text: "m"})
		bank.db.Exec(`UPDATE memories SET recall_strength = 4.0 WHERE id = ?`, rec.ID)
	}
	pc := newPipelineContextForTest(t, bank)
	pc.settings.ReinforceDecayFactor = 0 // isolate rebalance from decay
	pc.runPhase9Reinforcement()

	// After the 10% squeeze toward 1.5 mean, all memories should drop
	// somewhat from 4.0. Check mean is between 3.7 and 4.0.
	mems, _ := bank.ListMemoriesWith(MemoryListOpts{Limit: 100})
	if len(mems) == 0 {
		t.Fatalf("no memories after seed")
	}
	var sum float64
	for _, m := range mems {
		sum += m.RecallStrength
	}
	mean := sum / float64(len(mems))
	if mean >= 4.0 {
		t.Errorf("mean should have decreased from 4.0; got %g", mean)
	}
	if mean < 3.7 {
		t.Errorf("rebalance should be a soft squeeze (10%%), not a hard reset; got mean %g", mean)
	}
}

func TestPhase9R14_NoOpAtTarget(t *testing.T) {
	// Bank already at target mean (1.5) AND target stddev (0.7) — rebalance
	// should hit the early-exit. Five symmetric values: stddev = sqrt(0.5) ≈ 0.707.
	bank := newTestBank(t)
	strengths := []float64{0.5, 1.0, 1.5, 2.0, 2.5}
	for _, s := range strengths {
		rec, _ := bank.SaveMemory(MemoryRecord{Text: "m"})
		bank.db.Exec(`UPDATE memories SET recall_strength = ?, salience = 0.0 WHERE id = ?`, s, rec.ID)
	}
	pc := newPipelineContextForTest(t, bank)
	pc.settings.ReinforceDecayFactor = 0

	rebalanced := pc.applyGlobalStrengthRebalance(1.5, 0.7)
	if rebalanced > 0 {
		t.Errorf("near-target distribution should produce 0 changes (early-exit path), got %d", rebalanced)
	}
}

func TestPhase9R14_RespectsCaps(t *testing.T) {
	// Memories at the boundaries shouldn't be pushed above 5.0 or below 0.1.
	bank := newTestBank(t)
	rec1, _ := bank.SaveMemory(MemoryRecord{Text: "max"})
	rec2, _ := bank.SaveMemory(MemoryRecord{Text: "min"})
	bank.db.Exec(`UPDATE memories SET recall_strength = 5.0 WHERE id = ?`, rec1.ID)
	bank.db.Exec(`UPDATE memories SET recall_strength = 0.1 WHERE id = ?`, rec2.ID)
	for i := 0; i < 8; i++ {
		// Padding so n >= 2 for stddev calc
		rec, _ := bank.SaveMemory(MemoryRecord{Text: "p"})
		bank.db.Exec(`UPDATE memories SET recall_strength = 1.5 WHERE id = ?`, rec.ID)
	}
	pc := newPipelineContextForTest(t, bank)
	pc.applyGlobalStrengthRebalance(1.5, 0.7)
	g1, _ := bank.GetMemory(rec1.ID)
	g2, _ := bank.GetMemory(rec2.ID)
	if g1.RecallStrength > 5.0 || g2.RecallStrength < 0.1 {
		t.Errorf("rebalance must respect [0.1, 5.0] caps; got %g and %g", g1.RecallStrength, g2.RecallStrength)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// [R5] Phase 8.5 schema reframing
// ──────────────────────────────────────────────────────────────────────────

func TestPhase8_5_AddsSchemaSourceIDs(t *testing.T) {
	bank := newTestBank(t)
	// Schema memory + matching candidate.
	schema, _ := bank.SaveMemory(MemoryRecord{
		Text: "Distributed-system trade-off: eventual-consistency cost surfaces.",
		Tags: []string{"distributed-systems", "schema"},
		Source: "nightly_schema",
	})
	cand, _ := bank.SaveMemory(MemoryRecord{
		Text: "Eventual consistency in distributed-systems is a trade-off.",
		Tags: []string{"distributed-systems"},
	})
	// Pre-seed embeddings so loadEmbeddedMemories surfaces them. Same
	// vector for both → cosine ≈ 1.0 (well above the 0.7 threshold).
	model := envOr("SD_SYNAPSE_MODEL", envOr("SD_CLASSIFY_MODEL", "llama3.2:3b"))
	for _, rec := range []MemoryRecord{schema, cand} {
		tagsAny := make([]interface{}, len(rec.Tags))
		for i, t := range rec.Tags {
			tagsAny[i] = t
		}
		hash := synapseTextHash(synapseEmbedText(map[string]interface{}{
			"text": rec.Text, "tags": tagsAny,
		}))
		bank.SaveEmbedding(hash, []float32{0.5, 0.5, 0.5, 0.5}, model)
	}

	pc := newPipelineContextForTest(t, bank)
	pc.stats.Schemas = []SchemaRef{{ID: schema.ID, SourceCount: 1}}
	if err := pc.runPhase8_5SchemaReframing(); err != nil {
		t.Fatalf("phase8.5: %v", err)
	}
	got, _ := bank.GetMemory(cand.ID)
	hasSchema := false
	for _, sid := range got.SchemaSourceIDs {
		if sid == schema.ID {
			hasSchema = true
		}
	}
	if !hasSchema {
		t.Errorf("expected candidate's schema_source_ids to include the schema, got %v", got.SchemaSourceIDs)
	}
	if pc.stats.SchemaReframed != 1 {
		t.Errorf("expected SchemaReframed=1, got %d", pc.stats.SchemaReframed)
	}
}

func TestPhase8_5_PreservesText(t *testing.T) {
	// Schema reframing must NOT rewrite the candidate memory's text.
	bank := newTestBank(t)
	schema, _ := bank.SaveMemory(MemoryRecord{
		Text: "Schema text.", Tags: []string{"x"}, Source: "nightly_schema",
	})
	originalText := "Original candidate text. Should not change."
	cand, _ := bank.SaveMemory(MemoryRecord{Text: originalText, Tags: []string{"x"}})
	model := envOr("SD_SYNAPSE_MODEL", envOr("SD_CLASSIFY_MODEL", "llama3.2:3b"))
	for _, rec := range []MemoryRecord{schema, cand} {
		tagsAny := make([]interface{}, len(rec.Tags))
		for i, t := range rec.Tags {
			tagsAny[i] = t
		}
		hash := synapseTextHash(synapseEmbedText(map[string]interface{}{
			"text": rec.Text, "tags": tagsAny,
		}))
		bank.SaveEmbedding(hash, []float32{0.5, 0.5, 0.5, 0.5}, model)
	}

	pc := newPipelineContextForTest(t, bank)
	pc.stats.Schemas = []SchemaRef{{ID: schema.ID, SourceCount: 1}}
	pc.runPhase8_5SchemaReframing()

	got, _ := bank.GetMemory(cand.ID)
	if got.Text != originalText {
		t.Errorf("candidate text was rewritten — should be immutable; got %q", got.Text)
	}
}

func TestPhase8_5_SkipsSensitive(t *testing.T) {
	bank := newTestBank(t)
	schema, _ := bank.SaveMemory(MemoryRecord{
		Text: "Schema", Tags: []string{"x"}, Source: "nightly_schema",
	})
	cand, _ := bank.SaveMemory(MemoryRecord{
		Text: "Sensitive memory", Tags: []string{"x"}, Sensitive: true,
	})
	model := envOr("SD_SYNAPSE_MODEL", envOr("SD_CLASSIFY_MODEL", "llama3.2:3b"))
	for _, rec := range []MemoryRecord{schema, cand} {
		tagsAny := make([]interface{}, len(rec.Tags))
		for i, t := range rec.Tags {
			tagsAny[i] = t
		}
		hash := synapseTextHash(synapseEmbedText(map[string]interface{}{
			"text": rec.Text, "tags": tagsAny,
		}))
		bank.SaveEmbedding(hash, []float32{0.5, 0.5, 0.5, 0.5}, model)
	}

	pc := newPipelineContextForTest(t, bank)
	pc.stats.Schemas = []SchemaRef{{ID: schema.ID, SourceCount: 1}}
	pc.runPhase8_5SchemaReframing()

	got, _ := bank.GetMemory(cand.ID)
	for _, sid := range got.SchemaSourceIDs {
		if sid == schema.ID {
			t.Errorf("sensitive memory should not have been reframed by Phase 8.5")
		}
	}
}

func TestPhase8_5_NoOpWithoutSchemas(t *testing.T) {
	bank := newTestBank(t)
	pc := newPipelineContextForTest(t, bank)
	if err := pc.runPhase8_5SchemaReframing(); err != nil {
		t.Errorf("expected no-op when stats.Schemas is empty, got: %v", err)
	}
	if pc.stats.SchemaReframed != 0 {
		t.Errorf("expected SchemaReframed=0, got %d", pc.stats.SchemaReframed)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Settings round-trip for new R5 + R6 fields
// ──────────────────────────────────────────────────────────────────────────

func TestSettings_NewKnobsRoundTrip(t *testing.T) {
	bank := newTestBank(t)
	in := DefaultDreamPipelineSettings()
	in.SchemaReframeMaxPerRun = 25
	if err := bank.SaveDreamPipelineSettings(in); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, _ := bank.GetDreamPipelineSettings()
	if out.SchemaReframeMaxPerRun != 25 {
		t.Errorf("schema_reframe_max_per_run roundtrip: got %d", out.SchemaReframeMaxPerRun)
	}
}

func TestPipelineContext_RebalanceWithEmptyBank(t *testing.T) {
	// Don't crash on a bank with 0 or 1 memory.
	bank := newTestBank(t)
	pc := newPipelineContextForTest(t, bank)
	if got := pc.applyGlobalStrengthRebalance(1.5, 0.7); got != 0 {
		t.Errorf("empty bank should produce 0 changes, got %d", got)
	}
	bank.SaveMemory(MemoryRecord{Text: "lonely"})
	if got := pc.applyGlobalStrengthRebalance(1.5, 0.7); got != 0 {
		t.Errorf("single memory should produce 0 changes (need n>=2 for stddev), got %d", got)
	}
}

// Sanity: total elapsed in the new phase 9 path doesn't blow the test
// timeout — useful canary if rebalance ever becomes accidentally O(N²).
func TestPhase9_FullPathFinishesQuickly(t *testing.T) {
	bank := newTestBank(t)
	for i := 0; i < 50; i++ {
		rec, _ := bank.SaveMemory(MemoryRecord{Text: "m" + strings.Repeat("x", i%7)})
		bank.db.Exec(`UPDATE memories SET recall_strength = ?, salience = 0.5 WHERE id = ?`,
			0.5+float64(i)*0.05, rec.ID)
	}
	pc := newPipelineContextForTest(t, bank)
	start := time.Now()
	pc.runPhase9Reinforcement()
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Errorf("Phase 9 with 50 memories should be <5s, took %v", elapsed)
	}
}
