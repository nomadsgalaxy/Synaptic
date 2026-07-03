// nightly_phase8_7_test.go — [R13] Phase 8.7 Context Memory Formation +
// [R15] augmentation fallback. CITATIONS.md #18 (Johnson 2005).
package main

import (
	"strings"
	"testing"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────
// Pure helpers — no Tier 2/3 required
// ──────────────────────────────────────────────────────────────────────────

func TestGroupDeepEncodedByRegion_FiltersUnencodedAndDeleted(t *testing.T) {
	mems := []MemoryRecord{
		{ID: "a", RegionHint: "frontal_lobe", DeepEncodedAt: "2026-05-01T00:00:00Z"},
		{ID: "b", RegionHint: "frontal_lobe", DeepEncodedAt: ""},          // not deep-encoded
		{ID: "c", RegionHint: "", DeepEncodedAt: "2026-05-01T00:00:00Z"},  // no region
		{ID: "d", RegionHint: "frontal_lobe", DeepEncodedAt: "2026-05-01T00:00:00Z", DeletedAt: "2026-05-09T00:00:00Z"}, // deleted
		{ID: "e", RegionHint: "temporal_lobe", DeepEncodedAt: "2026-05-01T00:00:00Z"},
	}
	got := groupDeepEncodedByRegion(mems)
	if len(got["frontal_lobe"]) != 1 || got["frontal_lobe"][0].ID != "a" {
		t.Errorf("frontal_lobe: want [a], got %+v", got["frontal_lobe"])
	}
	if len(got["temporal_lobe"]) != 1 {
		t.Errorf("temporal_lobe: want 1, got %d", len(got["temporal_lobe"]))
	}
	if _, ok := got[""]; ok {
		t.Errorf("empty region must not appear in result")
	}
}

func TestPickEligibleRegions_ThresholdAndOrdering(t *testing.T) {
	byRegion := map[string][]MemoryRecord{
		"frontal_lobe":  fakeMems(20),
		"temporal_lobe": fakeMems(15), // below threshold
		"associative":   fakeMems(25),
	}
	got := pickEligibleRegions(byRegion, 20, 0.5)
	if len(got) != 2 {
		t.Fatalf("want 2 eligible regions, got %d: %v", len(got), got)
	}
	// Alphabetic ordering (deterministic for tests).
	if got[0] != "associative" || got[1] != "frontal_lobe" {
		t.Errorf("ordering: want [associative frontal_lobe], got %v", got)
	}
}

func TestSelectTopBySalience_PicksHighestFirst(t *testing.T) {
	mems := []MemoryRecord{
		{ID: "low", Salience: 0.1, CreatedAt: "2026-05-09T00:00:00Z"},
		{ID: "high", Salience: 0.9, CreatedAt: "2026-05-01T00:00:00Z"},
		{ID: "mid", Salience: 0.5, CreatedAt: "2026-05-05T00:00:00Z"},
	}
	got := selectTopBySalience(mems, 2)
	if len(got) != 2 {
		t.Fatalf("want 2, got %d", len(got))
	}
	if got[0].ID != "high" || got[1].ID != "mid" {
		t.Errorf("order: want [high mid], got [%s %s]", got[0].ID, got[1].ID)
	}
}

func TestSelectTopBySalience_TieBrokenByRecency(t *testing.T) {
	mems := []MemoryRecord{
		{ID: "older", Salience: 0.5, CreatedAt: "2026-04-01T00:00:00Z"},
		{ID: "newer", Salience: 0.5, CreatedAt: "2026-05-01T00:00:00Z"},
	}
	got := selectTopBySalience(mems, 1)
	if got[0].ID != "newer" {
		t.Errorf("tie should resolve to newer, got %s", got[0].ID)
	}
}

func TestBuildContextMemoryPrompt_StructureAndAsksForJSON(t *testing.T) {
	mems := []MemoryRecord{
		{Text: "memory one", Tags: []string{"alpha"}},
		{Text: "memory two", Tags: []string{"beta"}},
	}
	out := buildContextMemoryPrompt("frontal_lobe", mems)
	if !strings.Contains(out, "frontal_lobe") {
		t.Errorf("prompt should name the region")
	}
	if !strings.Contains(out, "JSON object") {
		t.Errorf("prompt should request JSON output")
	}
	if !strings.Contains(out, "confidence") {
		t.Errorf("prompt should request confidence field")
	}
	if !strings.Contains(out, "MEMORIES:") {
		t.Errorf("prompt should include MEMORIES section")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// parseContextMemoryResponse — forgiving JSON parsing
// ──────────────────────────────────────────────────────────────────────────

func TestParseContextMemoryResponse_StraightJSON(t *testing.T) {
	in := `{"framework": "this region is about auth flows.", "confidence": "high"}`
	f, c := parseContextMemoryResponse(in)
	if f != "this region is about auth flows." {
		t.Errorf("framework: got %q", f)
	}
	if c != "high" {
		t.Errorf("confidence: got %q", c)
	}
}

func TestParseContextMemoryResponse_CodeFenceWrapped(t *testing.T) {
	in := "```json\n{\"framework\": \"auth.\", \"confidence\": \"medium\"}\n```"
	f, c := parseContextMemoryResponse(in)
	if f != "auth." || c != "medium" {
		t.Errorf("got framework=%q confidence=%q", f, c)
	}
}

func TestParseContextMemoryResponse_PreamblePrefix(t *testing.T) {
	in := "Here's the JSON: {\"framework\": \"x.\", \"confidence\": \"low\"}"
	f, c := parseContextMemoryResponse(in)
	if f != "x." || c != "low" {
		t.Errorf("got framework=%q confidence=%q", f, c)
	}
}

func TestParseContextMemoryResponse_InvalidConfidenceFallsBackToMedium(t *testing.T) {
	in := `{"framework": "valid framework.", "confidence": "extremely high"}`
	_, c := parseContextMemoryResponse(in)
	if c != "medium" {
		t.Errorf("off-spec confidence should default to medium, got %q", c)
	}
}

func TestParseContextMemoryResponse_GarbledInputReturnsEmpty(t *testing.T) {
	f, c := parseContextMemoryResponse("definitely not JSON here")
	if f != "" || c != "" {
		t.Errorf("expected empties on garbled input, got (%q, %q)", f, c)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// End-to-end via bank state — Phase 8.7 disabled by default
// ──────────────────────────────────────────────────────────────────────────

func TestPhase8_7_DisabledByDefault(t *testing.T) {
	bank := newTestBank(t)
	// Seed 20 deep-encoded memories in one region.
	for i := 0; i < 20; i++ {
		rec, _ := bank.SaveMemory(MemoryRecord{
			Text:       "deep memory",
			RegionHint: "frontal_lobe",
		})
		bank.db.Exec(`UPDATE memories SET deep_encoded_at = ? WHERE id = ?`,
			"2026-05-09T00:00:00Z", rec.ID)
	}
	pc := newPipelineContextForTest(t, bank)
	// Default ContextMemoryEnabled=false → no Tier 2 call attempted.
	pc.router = nil
	if err := pc.runPhase8_7ContextMemory(); err != nil {
		t.Fatalf("disabled path should not error: %v", err)
	}
	if len(pc.stats.ContextMemories) != 0 {
		t.Errorf("expected 0 context memories, got %d", len(pc.stats.ContextMemories))
	}
}

func TestPhase8_7_SkipsWhenBelowMinMemories(t *testing.T) {
	bank := newTestBank(t)
	// Only 5 deep-encoded — below the default 20 threshold.
	for i := 0; i < 5; i++ {
		rec, _ := bank.SaveMemory(MemoryRecord{
			Text: "deep", RegionHint: "frontal_lobe",
		})
		bank.db.Exec(`UPDATE memories SET deep_encoded_at = ? WHERE id = ?`,
			"2026-05-09T00:00:00Z", rec.ID)
	}
	pc := newPipelineContextForTest(t, bank)
	pc.settings.ContextMemoryEnabled = true
	if err := pc.runPhase8_7ContextMemory(); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(pc.stats.ContextMemories) != 0 {
		t.Errorf("below-threshold region should yield 0 context memories, got %d", len(pc.stats.ContextMemories))
	}
}

// ──────────────────────────────────────────────────────────────────────────
// ListContextMemoriesByRegion — bank-side helper
// ──────────────────────────────────────────────────────────────────────────

func TestListContextMemoriesByRegion_ReturnsMostRecentPerRegion(t *testing.T) {
	bank := newTestBank(t)
	older, _ := bank.SaveMemory(MemoryRecord{
		Text: "old framework", Source: "nightly_context",
		MemoryType: "context", ContextRegion: "frontal_lobe", ContextConfidence: "high",
	})
	// Force older.updated_at to be earlier.
	bank.db.Exec(`UPDATE memories SET updated_at = '2026-03-01T00:00:00Z' WHERE id = ?`, older.ID)
	newer, _ := bank.SaveMemory(MemoryRecord{
		Text: "fresh framework", Source: "nightly_context",
		MemoryType: "context", ContextRegion: "frontal_lobe", ContextConfidence: "medium",
	})
	bank.db.Exec(`UPDATE memories SET updated_at = '2026-05-09T00:00:00Z' WHERE id = ?`, newer.ID)

	got, err := bank.ListContextMemoriesByRegion()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got["frontal_lobe"].ID != newer.ID {
		t.Errorf("want newer (%s) for frontal_lobe, got %s", newer.ID, got["frontal_lobe"].ID)
	}
}

func TestListContextMemoriesByRegion_SkipsNonContext(t *testing.T) {
	bank := newTestBank(t)
	bank.SaveMemory(MemoryRecord{
		Text: "synthesis output", Source: "nightly_synthesis",
		MemoryType: "synthesis", RegionHint: "frontal_lobe",
	})
	got, _ := bank.ListContextMemoriesByRegion()
	if len(got) != 0 {
		t.Errorf("synthesis-typed rows must not appear in context list, got %d", len(got))
	}
}

// ──────────────────────────────────────────────────────────────────────────
// MemoryType auto-assignment from Source
// ──────────────────────────────────────────────────────────────────────────

func TestSaveMemory_AssignsMemoryTypeFromSource(t *testing.T) {
	bank := newTestBank(t)
	cases := []struct {
		source, wantType string
	}{
		{"adapter", "episodic"},
		{"nightly_synthesis", "synthesis"},
		{"nightly_schema", "schema"},
		{"nightly_schema_from_replay", "schema"},
		{"nightly_context", "context"},
	}
	for _, c := range cases {
		rec, err := bank.SaveMemory(MemoryRecord{Text: "x", Source: c.source})
		if err != nil {
			t.Fatalf("save source=%s: %v", c.source, err)
		}
		got, _ := bank.GetMemory(rec.ID)
		if got.MemoryType != c.wantType {
			t.Errorf("source=%s: want memory_type=%q got %q", c.source, c.wantType, got.MemoryType)
		}
	}
}

// helper — N memories in a region, all deep-encoded.
func fakeMems(n int) []MemoryRecord {
	out := make([]MemoryRecord, n)
	for i := 0; i < n; i++ {
		out[i] = MemoryRecord{
			DeepEncodedAt: "2026-05-09T00:00:00Z",
			CreatedAt:     "2026-05-09T00:00:00Z",
		}
	}
	return out
}

// silence unused-import in test file when time is referenced only in fakeMems
var _ = time.Now