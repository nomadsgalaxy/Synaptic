// handlers_admin_embed_test.go — tests for Phase 0 actually-embeds behavior
// + the POST /admin/embed-all bulk endpoint.
//
// Per handoff "Phase 0 silent embedding failure 2026-05-10".
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// scriptedEmbedder is a minimal LLMProvider whose Embed call can be
// scripted to fail-or-succeed per call. fail tells whether call N (0-indexed)
// should error; nil → never fails.
type scriptedEmbedder struct {
	calls atomic.Int32
	fail  func(call int) error // returns nil to succeed
	vec   []float32            // returned on success; defaults to a 4-d unit vector
}

func newScriptedEmbedder() *scriptedEmbedder {
	return &scriptedEmbedder{vec: []float32{0.5, 0.5, 0.5, 0.5}}
}

func (s *scriptedEmbedder) Name() string  { return "ollama:scripted" }
func (s *scriptedEmbedder) IsLocal() bool { return true }
func (s *scriptedEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	n := int(s.calls.Add(1)) - 1
	if s.fail != nil {
		if err := s.fail(n); err != nil {
			return nil, err
		}
	}
	return s.vec, nil
}
func (s *scriptedEmbedder) Chat(_ context.Context, _ []Message, _ int) (string, error) {
	return "", nil
}

// newPipelineCtxWithEmbedder wires a scripted embedder as Tier 1 of a fresh
// router so Phase 0's pc.router.ForEmbedding() returns it.
func newPipelineCtxWithEmbedder(t *testing.T, bank *Bank, e LLMProvider) *pipelineContext {
	t.Helper()
	pc := newPipelineContextForTest(t, bank)
	pc.router = NewModelRouter(e, nil, nil, nil)
	return pc
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 0 — embed gating
// ──────────────────────────────────────────────────────────────────────────

func TestPhase0_EmbedSuccessFlipsAndPersists(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "hello", Tags: []string{"a"}})
	emb := newScriptedEmbedder()
	pc := newPipelineCtxWithEmbedder(t, bank, emb)

	if err := pc.runPhase0Encoding(); err != nil {
		t.Fatalf("phase0: %v", err)
	}
	got, _ := bank.GetMemory(rec.ID)
	if !got.LightEncoded {
		t.Errorf("expected light_encoded=true after successful embed")
	}
	if pc.stats.Encoded != 1 {
		t.Errorf("expected stats.Encoded=1, got %d", pc.stats.Encoded)
	}
	// Embedding should be persisted with the synapse hash key.
	tagsAny := []interface{}{"a"}
	hash := synapseTextHash(synapseEmbedText(map[string]interface{}{"text": "hello", "tags": tagsAny}))
	vec, _ := bank.GetEmbedding(hash)
	if vec == nil {
		t.Errorf("expected embedding row persisted under hash %s", hash)
	}
}

func TestPhase0_EmbedFailureSkipsFlip(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "hello", Tags: []string{"a"}})
	emb := newScriptedEmbedder()
	emb.fail = func(int) error { return errors.New("ollama unreachable") }
	pc := newPipelineCtxWithEmbedder(t, bank, emb)

	err := pc.runPhase0Encoding()
	// All-fail run: with only 1 memory and it failing → flipped=0 → return error.
	if err == nil {
		t.Errorf("expected error when 0 of 1 memories embedded")
	}
	got, _ := bank.GetMemory(rec.ID)
	if got.LightEncoded {
		t.Errorf("expected light_encoded=false on embed failure (must not lie)")
	}
	if len(pc.stats.PhaseFailures) == 0 {
		t.Fatalf("expected phase_failures populated")
	}
	if !strings.Contains(pc.stats.PhaseFailures[0], "encode:embed_failed") {
		t.Errorf("expected encode:embed_failed in phase_failures, got %q", pc.stats.PhaseFailures[0])
	}
}

func TestPhase0_AllEmbedsFailMarksFailed(t *testing.T) {
	bank := newTestBank(t)
	for i := 0; i < 5; i++ {
		bank.SaveMemory(MemoryRecord{Text: "m", Tags: []string{"x"}})
	}
	emb := newScriptedEmbedder()
	emb.fail = func(int) error { return errors.New("model not pulled") }
	pc := newPipelineCtxWithEmbedder(t, bank, emb)

	err := pc.runPhase0Encoding()
	if err == nil {
		t.Fatalf("expected error when all embeds fail (run should land in status=failed)")
	}
	if !strings.Contains(err.Error(), "phase0 encoded 0/") {
		t.Errorf("error should explain 0/N embedded, got %v", err)
	}
}

func TestPhase0_StatsEncodedReflectsSuccess(t *testing.T) {
	bank := newTestBank(t)
	for i := 0; i < 10; i++ {
		// Each memory needs a distinct text so the synapse hash is unique;
		// otherwise the cache hit on call 2 short-circuits the embedder.
		bank.SaveMemory(MemoryRecord{Text: "memory-" + string(rune('a'+i))})
	}
	emb := newScriptedEmbedder()
	// Fail every 4th call (calls 3, 7) — 2 failures total. The circuit
	// breaker (3 consecutive) won't trip because successes between resets.
	emb.fail = func(call int) error {
		if call == 3 || call == 7 {
			return errors.New("transient")
		}
		return nil
	}
	pc := newPipelineCtxWithEmbedder(t, bank, emb)

	if err := pc.runPhase0Encoding(); err != nil {
		t.Fatalf("phase0: %v", err)
	}
	if pc.stats.Encoded != 8 {
		t.Errorf("expected stats.Encoded=8 (10 memories - 2 transient failures), got %d", pc.stats.Encoded)
	}
	// 2 phase_failures captured.
	failCount := 0
	for _, f := range pc.stats.PhaseFailures {
		if strings.Contains(f, "encode:embed_failed") {
			failCount++
		}
	}
	if failCount != 2 {
		t.Errorf("expected 2 encode:embed_failed entries, got %d (%v)", failCount, pc.stats.PhaseFailures)
	}
}

func TestPhase0_CircuitBreakerOnConsecutiveFails(t *testing.T) {
	bank := newTestBank(t)
	for i := 0; i < 20; i++ {
		bank.SaveMemory(MemoryRecord{Text: "m" + string(rune('a'+i))})
	}
	emb := newScriptedEmbedder()
	// Always fail — circuit breaker should trip after 3 consecutive.
	emb.fail = func(int) error { return errors.New("ollama down") }
	pc := newPipelineCtxWithEmbedder(t, bank, emb)

	_ = pc.runPhase0Encoding() // expected to error (all fail)
	// Even though we have 20 unencoded memories, the breaker should bail
	// after a small handful of failures rather than trying all 20.
	if int(emb.calls.Load()) >= 20 {
		t.Errorf("circuit breaker didn't trip — Embed called %d times", emb.calls.Load())
	}
	if int(emb.calls.Load()) < 3 {
		t.Errorf("expected at least the breaker threshold (3) attempts, got %d", emb.calls.Load())
	}
}

func TestPhase0_NoTier1FailsClean(t *testing.T) {
	bank := newTestBank(t)
	bank.SaveMemory(MemoryRecord{Text: "lonely memory"})
	pc := newPipelineContextForTest(t, bank) // pc.router is nil
	_ = pc.runPhase0Encoding()
	// Should NOT have flipped light_encoded — but should have surfaced the
	// "Tier 1 unconfigured" gap as a phase_failure.
	if len(pc.stats.PhaseFailures) == 0 {
		t.Fatalf("expected phase_failures when Tier 1 unconfigured")
	}
	if !strings.Contains(pc.stats.PhaseFailures[0], "Tier 1") {
		t.Errorf("expected Tier 1 mention in phase_failures, got %q", pc.stats.PhaseFailures[0])
	}
}

// ──────────────────────────────────────────────────────────────────────────
// POST /admin/embed-all
// ──────────────────────────────────────────────────────────────────────────

func newAdminEmbedServer(t *testing.T, embedder LLMProvider) (*Server, *Bank) {
	t.Helper()
	bank := newTestBank(t)
	srv := &Server{
		bank:   bank,
		router: NewModelRouter(embedder, nil, nil, nil),
	}
	return srv, bank
}

func TestEmbedAll_HappyPath(t *testing.T) {
	emb := newScriptedEmbedder()
	srv, bank := newAdminEmbedServer(t, emb)
	for i := 0; i < 3; i++ {
		bank.SaveMemory(MemoryRecord{Text: "memory-" + string(rune('a'+i))})
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/embed-all",
		strings.NewReader(`{"skip_existing":true}`))
	w := httptest.NewRecorder()
	srv.handleAdminEmbedAll(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp embedAllResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Embedded != 3 {
		t.Errorf("expected embedded=3, got %d", resp.Embedded)
	}
	if len(resp.Errors) != 0 {
		t.Errorf("expected no errors, got %v", resp.Errors)
	}
}

func TestEmbedAll_SkipsExisting(t *testing.T) {
	emb := newScriptedEmbedder()
	srv, bank := newAdminEmbedServer(t, emb)
	bank.SaveMemory(MemoryRecord{Text: "existing", Tags: []string{"a"}})
	bank.SaveMemory(MemoryRecord{Text: "fresh", Tags: []string{"b"}})

	// Pre-seed embedding for the first memory.
	hash := synapseTextHash(synapseEmbedText(map[string]interface{}{
		"text": "existing", "tags": []interface{}{"a"},
	}))
	bank.SaveEmbedding(hash, []float32{1, 0, 0, 0}, "fake")

	req := httptest.NewRequest(http.MethodPost, "/admin/embed-all",
		strings.NewReader(`{"skip_existing":true}`))
	w := httptest.NewRecorder()
	srv.handleAdminEmbedAll(w, req)
	var resp embedAllResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.SkippedExisting != 1 {
		t.Errorf("expected skipped_existing=1, got %d", resp.SkippedExisting)
	}
	if resp.Embedded != 1 {
		t.Errorf("expected embedded=1 (only 'fresh'), got %d", resp.Embedded)
	}
	if int(emb.calls.Load()) != 1 {
		t.Errorf("expected provider called once (skip_existing), got %d", emb.calls.Load())
	}
}

func TestEmbedAll_ErrorsListed(t *testing.T) {
	emb := newScriptedEmbedder()
	emb.fail = func(call int) error {
		if call == 1 {
			return errors.New("model not pulled")
		}
		return nil
	}
	srv, bank := newAdminEmbedServer(t, emb)
	bank.SaveMemory(MemoryRecord{Text: "ok"})
	bank.SaveMemory(MemoryRecord{Text: "doomed"}) // will be call index 1
	bank.SaveMemory(MemoryRecord{Text: "ok2"})

	req := httptest.NewRequest(http.MethodPost, "/admin/embed-all", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	srv.handleAdminEmbedAll(w, req)
	var resp embedAllResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Embedded != 2 {
		t.Errorf("expected embedded=2 of 3, got %d", resp.Embedded)
	}
	if len(resp.Errors) != 1 {
		t.Fatalf("expected 1 error entry, got %d (%v)", len(resp.Errors), resp.Errors)
	}
	if !strings.Contains(resp.Errors[0], "model not pulled") {
		t.Errorf("expected error string to surface upstream message, got %q", resp.Errors[0])
	}
}

func TestEmbedAll_RejectsBadMethod(t *testing.T) {
	srv, _ := newAdminEmbedServer(t, newScriptedEmbedder())
	req := httptest.NewRequest(http.MethodGet, "/admin/embed-all", nil)
	w := httptest.NewRecorder()
	srv.handleAdminEmbedAll(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 on GET, got %d", w.Code)
	}
}

func TestEmbedAll_NoTier1Returns424(t *testing.T) {
	srv := &Server{bank: newTestBank(t), router: NewModelRouter(nil, nil, nil, nil)}
	req := httptest.NewRequest(http.MethodPost, "/admin/embed-all", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	srv.handleAdminEmbedAll(w, req)
	if w.Code != http.StatusFailedDependency {
		t.Errorf("expected 424 when Tier 1 unconfigured, got %d", w.Code)
	}
}

// Sweep semantics (2026-07-07): a memory already marked light_encoded whose
// embedding is missing (edited/enriched → new hash) must be re-embedded by
// Phase 0 rather than skipped forever. Guards the fix for the creeping
// unembedded count that tripped Phase 1's 5% coverage gate.
func TestPhase0_SweepBackfillsFlaggedButUnembedded(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "hello", Tags: []string{"a"}})
	// Simulate the leak: flag set, but no embedding row exists for the hash.
	if err := bank.SetMemoryLifecycle(rec.ID, "light_encoded", true); err != nil {
		t.Fatalf("set lifecycle: %v", err)
	}
	emb := newScriptedEmbedder()
	pc := newPipelineCtxWithEmbedder(t, bank, emb)

	if err := pc.runPhase0Encoding(); err != nil {
		t.Fatalf("phase0: %v", err)
	}
	if got := int(emb.calls.Load()); got != 1 {
		t.Errorf("expected exactly 1 embed call for the swept row, got %d", got)
	}
	hash := synapseTextHash(synapseEmbedText(map[string]interface{}{"text": "hello", "tags": []interface{}{"a"}}))
	if vec, _ := bank.GetEmbedding(hash); vec == nil {
		t.Errorf("expected sweep to persist the missing embedding")
	}
	// Flag was already set — Encoded counts flag flips only.
	if pc.stats.Encoded != 0 {
		t.Errorf("expected stats.Encoded=0 (no flag flipped), got %d", pc.stats.Encoded)
	}
	// Second run: fully encoded now, no further embed calls.
	if err := pc.runPhase0Encoding(); err != nil {
		t.Fatalf("phase0 second run: %v", err)
	}
	if got := int(emb.calls.Load()); got != 1 {
		t.Errorf("expected no additional embed calls on second run, got %d total", got)
	}
}
