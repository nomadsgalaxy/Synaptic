// dirty_triggers_test.go — tests for R3 dirty-flag trigger expansion +
// R7 consolidation_stage transitions + R11 manual TMR endpoint.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────
// R3 cascading
// ──────────────────────────────────────────────────────────────────────────

func TestCascading_MarksSynthesisProductsDirty(t *testing.T) {
	bank := newTestBank(t)
	// Two source memories that produced one synthesis.
	src1, _ := bank.SaveMemory(MemoryRecord{Text: "source one"})
	src2, _ := bank.SaveMemory(MemoryRecord{Text: "source two"})
	synth, _ := bank.SaveMemory(MemoryRecord{
		Text:               "synthesis output",
		Source:             "nightly_synthesis",
		SynthesisSourceIDs: []string{src1.ID, src2.ID},
	})
	// Clean the synthesis so we can detect re-dirtying.
	bank.db.Exec(`UPDATE memories SET marked_dirty_at = '', dirty_reason = '', deep_encoded_at = ? WHERE id = ?`,
		"2026-05-09T00:00:00Z", synth.ID)

	n, err := bank.MarkSynthesisProductsDirty(src1.ID)
	if err != nil {
		t.Fatalf("MarkSynthesisProductsDirty: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 synthesis product marked, got %d", n)
	}
	got, _ := bank.GetMemory(synth.ID)
	if got.DirtyReason != "cascading" {
		t.Errorf("expected dirty_reason=cascading, got %q", got.DirtyReason)
	}
}

func TestCascading_NotMarkedForUnrelatedSource(t *testing.T) {
	bank := newTestBank(t)
	bank.SaveMemory(MemoryRecord{
		Text: "synthesis", Source: "nightly_synthesis",
		SynthesisSourceIDs: []string{"a-id", "b-id"},
	})
	n, _ := bank.MarkSynthesisProductsDirty("totally-different-id")
	if n != 0 {
		t.Errorf("expected 0 marks for unrelated source, got %d", n)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// R3 temporal_neighbor
// ──────────────────────────────────────────────────────────────────────────

func TestTemporalNeighbor_MarksSameRegionWithinWindow(t *testing.T) {
	bank := newTestBank(t)
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)

	anchor, _ := bank.SaveMemory(MemoryRecord{Text: "anchor", RegionHint: "frontal_lobe"})
	near, _ := bank.SaveMemory(MemoryRecord{Text: "near", RegionHint: "frontal_lobe"})
	farTime, _ := bank.SaveMemory(MemoryRecord{Text: "old", RegionHint: "frontal_lobe"})
	otherRegion, _ := bank.SaveMemory(MemoryRecord{Text: "elsewhere", RegionHint: "amygdala"})

	// Override created_at + deep_encoded_at — temporal_neighbor only marks
	// rows that are already deep-encoded (they got their first pass).
	bank.db.Exec(`UPDATE memories SET created_at = ?, deep_encoded_at = ?, marked_dirty_at = '', dirty_reason = '' WHERE id = ?`,
		now.Add(-10*time.Minute).Format(time.RFC3339Nano), "2026-05-09T00:00:00Z", near.ID)
	bank.db.Exec(`UPDATE memories SET created_at = ?, deep_encoded_at = ?, marked_dirty_at = '', dirty_reason = '' WHERE id = ?`,
		now.Add(-2*time.Hour).Format(time.RFC3339Nano), "2026-05-09T00:00:00Z", farTime.ID)
	bank.db.Exec(`UPDATE memories SET created_at = ?, deep_encoded_at = ?, marked_dirty_at = '', dirty_reason = '' WHERE id = ?`,
		now.Add(-10*time.Minute).Format(time.RFC3339Nano), "2026-05-09T00:00:00Z", otherRegion.ID)

	n, err := bank.MarkTemporalNeighborsDirty(anchor.ID, "frontal_lobe", now.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatalf("MarkTemporalNeighborsDirty: %v", err)
	}
	if n != 1 {
		t.Errorf("expected exactly 1 in-window same-region neighbour, got %d", n)
	}
	gotNear, _ := bank.GetMemory(near.ID)
	if gotNear.DirtyReason != "temporal_neighbor" {
		t.Errorf("expected near memory marked temporal_neighbor, got %q", gotNear.DirtyReason)
	}
	gotFar, _ := bank.GetMemory(farTime.ID)
	if gotFar.DirtyReason == "temporal_neighbor" {
		t.Errorf("far-in-time memory should NOT be marked")
	}
	gotOther, _ := bank.GetMemory(otherRegion.ID)
	if gotOther.DirtyReason == "temporal_neighbor" {
		t.Errorf("different-region memory should NOT be marked")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// R3 periodic
// ──────────────────────────────────────────────────────────────────────────

func TestPeriodic_MarksStaleDeepEncoded(t *testing.T) {
	bank := newTestBank(t)
	stale, _ := bank.SaveMemory(MemoryRecord{Text: "old enrichment"})
	fresh, _ := bank.SaveMemory(MemoryRecord{Text: "new enrichment"})
	// stale was enriched 100 days ago; fresh 10 days ago.
	bank.db.Exec(`UPDATE memories SET deep_encoded_at = ?, marked_dirty_at = '', dirty_reason = '' WHERE id = ?`,
		"2026-01-30T00:00:00Z", stale.ID)
	bank.db.Exec(`UPDATE memories SET deep_encoded_at = ?, marked_dirty_at = '', dirty_reason = '' WHERE id = ?`,
		"2026-04-30T00:00:00Z", fresh.ID)

	cutoff := "2026-02-09T00:00:00Z" // 90 days before 2026-05-10
	n, err := bank.MarkPeriodicallyStale(cutoff)
	if err != nil {
		t.Fatalf("MarkPeriodicallyStale: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 stale row marked, got %d", n)
	}
	gotStale, _ := bank.GetMemory(stale.ID)
	if gotStale.DirtyReason != "periodic" {
		t.Errorf("expected stale memory dirty_reason=periodic, got %q", gotStale.DirtyReason)
	}
	gotFresh, _ := bank.GetMemory(fresh.ID)
	if gotFresh.DirtyReason == "periodic" {
		t.Errorf("fresh memory should NOT be marked periodic")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// R3 surprise / emotional via PersistDeepEncoding
// ──────────────────────────────────────────────────────────────────────────

func TestPersistDeepEncoding_RegionChangeMarksSurprise(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{
		Text: "memory", Tags: []string{"a"}, RegionHint: "frontal_lobe",
	})
	err := bank.PersistDeepEncoding(
		rec.ID, "improved summary", []string{"a"},
		"motor_cortex", // different region
		"ollama:test", "reasoning", 100, 50, "region change", "edited",
	)
	if err != nil {
		t.Fatalf("PersistDeepEncoding: %v", err)
	}
	got, _ := bank.GetMemory(rec.ID)
	if got.DirtyReason != "surprise" {
		t.Errorf("expected dirty_reason=surprise after region change, got %q", got.DirtyReason)
	}
}

func TestPersistDeepEncoding_BigTagDeltaMarksSurprise(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{
		Text: "memory", Tags: []string{"a", "b", "c", "d"}, RegionHint: "frontal_lobe",
	})
	// Tier 2 dropped 4 tags and added 4 new ones — fully disjoint sets.
	err := bank.PersistDeepEncoding(
		rec.ID, "improved", []string{"x", "y", "z", "w"},
		"frontal_lobe", "ollama:test", "reasoning", 100, 50, "tags rewrite", "edited",
	)
	if err != nil {
		t.Fatalf("PersistDeepEncoding: %v", err)
	}
	got, _ := bank.GetMemory(rec.ID)
	if got.DirtyReason != "surprise" {
		t.Errorf("expected dirty_reason=surprise after >50%% tag delta, got %q", got.DirtyReason)
	}
}

func TestTagDeltaFraction_MathChecks(t *testing.T) {
	cases := []struct {
		before, after []string
		want          float64 // approximate
	}{
		{nil, nil, 0},
		{[]string{"a"}, []string{"a"}, 0},
		{[]string{"a"}, []string{"b"}, 1.0},
		{[]string{"a", "b"}, []string{"b", "c"}, 1.0 - 1.0/3.0}, // intersect=1, union=3
	}
	for _, c := range cases {
		got := tagDeltaFraction(c.before, c.after)
		if got < c.want-0.01 || got > c.want+0.01 {
			t.Errorf("delta(%v,%v): got %g, want ~%g", c.before, c.after, got, c.want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────
// R7 consolidation_stage transitions
// ──────────────────────────────────────────────────────────────────────────

func TestConsolidationStage_DefaultsToEpisodic(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "fresh"})
	got, _ := bank.GetMemory(rec.ID)
	if got.ConsolidationStage != "episodic" {
		t.Errorf("expected episodic default, got %q", got.ConsolidationStage)
	}
}

func TestConsolidationStage_LightEncodedFlipsToConsolidating(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "memory"})
	if err := bank.SetMemoryLifecycle(rec.ID, "light_encoded", true); err != nil {
		t.Fatalf("SetMemoryLifecycle: %v", err)
	}
	got, _ := bank.GetMemory(rec.ID)
	if got.ConsolidationStage != "consolidating" {
		t.Errorf("expected consolidating after light_encoded=true, got %q", got.ConsolidationStage)
	}
}

func TestConsolidationStage_StagesNeverRegress(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "memory"})
	bank.db.Exec(`UPDATE memories SET consolidation_stage = 'semantic' WHERE id = ?`, rec.ID)
	if err := bank.SetMemoryLifecycle(rec.ID, "light_encoded", true); err != nil {
		t.Fatalf("SetMemoryLifecycle: %v", err)
	}
	got, _ := bank.GetMemory(rec.ID)
	if got.ConsolidationStage != "semantic" {
		t.Errorf("stage should not regress from semantic, got %q", got.ConsolidationStage)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// PickNextDirtyMemory priority — TMR wins
// ──────────────────────────────────────────────────────────────────────────

func TestPickNextDirtyMemory_TMRBeatsCreated(t *testing.T) {
	bank := newTestBank(t)
	now := time.Now().UTC()

	rCreated, _ := bank.SaveMemory(MemoryRecord{Text: "c"})
	rTMR, _ := bank.SaveMemory(MemoryRecord{Text: "tmr"})

	// rCreated is naturally created (default reason)
	// Override rTMR to look like a TMR mark (post-encoded, then user-tagged).
	bank.db.Exec(`UPDATE memories SET deep_encoded_at = ?, marked_dirty_at = ?, dirty_reason = 'tmr_user' WHERE id = ?`,
		"2026-05-09T00:00:00Z", now.Format(time.RFC3339Nano), rTMR.ID)

	first, ok, err := bank.PickNextDirtyMemory()
	if err != nil || !ok {
		t.Fatalf("PickNextDirtyMemory: ok=%v err=%v", ok, err)
	}
	if first.ID != rTMR.ID {
		t.Errorf("expected tmr_user to outrank created, got id=%s reason=%s",
			first.ID, first.DirtyReason)
	}
	_ = rCreated
}

// ──────────────────────────────────────────────────────────────────────────
// R11 POST /bank/memories/{id}/dirty
// ──────────────────────────────────────────────────────────────────────────

func newDirtyHandlerSrv(t *testing.T) (*Server, *Bank) {
	t.Helper()
	bank := newTestBank(t)
	return &Server{bank: bank}, bank
}

func TestTMREndpoint_HappyPath(t *testing.T) {
	srv, bank := newDirtyHandlerSrv(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "m"})
	bank.db.Exec(`UPDATE memories SET deep_encoded_at = ?, marked_dirty_at = '', dirty_reason = '' WHERE id = ?`,
		"2026-05-09T00:00:00Z", rec.ID)

	body := `{"reason":"tmr_user"}`
	req := httptest.NewRequest(http.MethodPost, "/bank/memories/"+rec.ID+"/dirty",
		strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.bankByIDExtended(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["dirty_reason"] != "tmr_user" {
		t.Errorf("expected dirty_reason=tmr_user in response, got %v", resp)
	}
	got, _ := bank.GetMemory(rec.ID)
	if got.DirtyReason != "tmr_user" {
		t.Errorf("expected DB reason=tmr_user, got %q", got.DirtyReason)
	}
}

func TestTMREndpoint_DefaultsReasonToTMRUser(t *testing.T) {
	srv, bank := newDirtyHandlerSrv(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "m"})
	req := httptest.NewRequest(http.MethodPost, "/bank/memories/"+rec.ID+"/dirty", nil)
	w := httptest.NewRecorder()
	srv.bankByIDExtended(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on empty-body, got %d", w.Code)
	}
	got, _ := bank.GetMemory(rec.ID)
	if got.DirtyReason != "tmr_user" {
		t.Errorf("expected default reason tmr_user, got %q", got.DirtyReason)
	}
}

func TestTMREndpoint_RejectsUnknownReason(t *testing.T) {
	srv, bank := newDirtyHandlerSrv(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "m"})
	req := httptest.NewRequest(http.MethodPost, "/bank/memories/"+rec.ID+"/dirty",
		strings.NewReader(`{"reason":"made-up-reason"}`))
	w := httptest.NewRecorder()
	srv.bankByIDExtended(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 on invalid reason, got %d", w.Code)
	}
}

func TestTMREndpoint_NotFound(t *testing.T) {
	srv, _ := newDirtyHandlerSrv(t)
	req := httptest.NewRequest(http.MethodPost, "/bank/memories/ghost-id/dirty",
		strings.NewReader(`{"reason":"tmr_user"}`))
	w := httptest.NewRecorder()
	srv.bankByIDExtended(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestTMREndpoint_RejectsBadMethod(t *testing.T) {
	srv, bank := newDirtyHandlerSrv(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "m"})
	req := httptest.NewRequest(http.MethodGet, "/bank/memories/"+rec.ID+"/dirty", nil)
	w := httptest.NewRecorder()
	srv.bankByIDExtended(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 on GET, got %d", w.Code)
	}
}
