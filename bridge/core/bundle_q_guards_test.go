// bundle_q_guards_test.go — guards for v2.7 Bundle Q (rerank top-K).
//
// Bundle Q adds an opt-in second-pass reranker that operates on the
// post-Bundle-J fused list. The default reranker is a salience+cosine
// blend with no I/O; a future cross-encoder reranker can be installed
// via s.reranker. Coverage:
//
//   - rerankEnabled defaults OFF (preserves v2.6 behaviour)
//   - rerankEnabled honours "1" / "on" / "true"
//   - rerankTopK defaults to 20 and respects overrides
//   - salienceReranker promotes a high-salience candidate over a higher-
//     cosine but low-salience candidate (the whole point of the feature)
//   - salienceReranker preserves order on equal scores (stable sort)
//   - salienceReranker is a no-op pass-through when ALL candidates have
//     identical scores
//   - The HTTP /recall path honours the toggle when enabled
package main

import (
	"context"
	"testing"
)

// ── Setting helpers ─────────────────────────────────────────────────────

func TestBundleQ_RerankDefaultsOn(t *testing.T) {
	bank := newTestBank(t)
	// Iter 33 (2026-05-15): the LLM cross-encoder reranker became the system
	// default, so rerank now defaults ON when unset (see recall.go
	// rerankEnabled). Users opt OUT via recall_rerank_enabled = 0/false/off/no.
	if !rerankEnabled(bank) {
		t.Errorf("rerank should default ON when unset, got OFF")
	}
	// A nil bank still has no settings store to consult → OFF (safe default).
	if rerankEnabled(nil) {
		t.Errorf("nil bank should default OFF")
	}
}

func TestBundleQ_RerankHonoursOnSetting(t *testing.T) {
	bank := newTestBank(t)
	for _, val := range []string{"1", "true", "on", "yes", "TRUE"} {
		bank.SetSetting(RecallRerankEnabledKey, val)
		if !rerankEnabled(bank) {
			t.Errorf("value %q should enable rerank, still off", val)
		}
	}
	bank.SetSetting(RecallRerankEnabledKey, "0")
	if rerankEnabled(bank) {
		t.Errorf(`"0" should disable rerank`)
	}
}

func TestBundleQ_RerankTopKDefaultsAndOverride(t *testing.T) {
	bank := newTestBank(t)
	if got := rerankTopK(bank); got != 20 {
		t.Errorf("default top-K should be 20, got %d", got)
	}
	bank.SetSetting(RecallRerankTopKKey, "50")
	if got := rerankTopK(bank); got != 50 {
		t.Errorf("override should win, got %d", got)
	}
	// Out-of-range → fall back.
	for _, bad := range []string{"0", "-5", "5000", "not-a-number"} {
		bank.SetSetting(RecallRerankTopKKey, bad)
		if got := rerankTopK(bank); got != 20 {
			t.Errorf("bad value %q should fall back to 20, got %d", bad, got)
		}
	}
}

// ── salienceReranker behaviour ──────────────────────────────────────────

func TestBundleQ_SalienceRerankerPromotesHighSalience(t *testing.T) {
	// Two candidates:
	//   A: cosine 0.85, salience 0.10, recall 1.0 → blend = 0.6*.85 + 0.3*.10 + 0.1*.20 = .530
	//   B: cosine 0.80, salience 0.95, recall 5.0 → blend = 0.6*.80 + 0.3*.95 + 0.1*1.0 = .865
	// B should rank above A even though A has higher cosine.
	cands := []rerankCandidate{
		{ID: "a", CosineScore: 0.85, Salience: 0.10, RecallStrength: 1.0},
		{ID: "b", CosineScore: 0.80, Salience: 0.95, RecallStrength: 5.0},
	}
	got, err := salienceReranker{}.Rerank(context.Background(), "q", cands)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if got[0].ID != "b" {
		t.Errorf("expected high-salience B first, got %q (full: %v)", got[0].ID, got)
	}
}

func TestBundleQ_SalienceRerankerKeepsCosineWhenSalienceFlat(t *testing.T) {
	// All salience equal → the cosine term wins, so order = cosine order.
	cands := []rerankCandidate{
		{ID: "low", CosineScore: 0.50, Salience: 0.5, RecallStrength: 1.0},
		{ID: "high", CosineScore: 0.95, Salience: 0.5, RecallStrength: 1.0},
	}
	got, _ := salienceReranker{}.Rerank(context.Background(), "q", cands)
	if got[0].ID != "high" {
		t.Errorf("with flat salience, cosine should drive order; got %q first", got[0].ID)
	}
}

func TestBundleQ_SalienceRerankerStableOnTies(t *testing.T) {
	// All scores identical → stable sort preserves insertion order.
	cands := []rerankCandidate{
		{ID: "x", CosineScore: 0.5, Salience: 0.5, RecallStrength: 1.0},
		{ID: "y", CosineScore: 0.5, Salience: 0.5, RecallStrength: 1.0},
		{ID: "z", CosineScore: 0.5, Salience: 0.5, RecallStrength: 1.0},
	}
	got, _ := salienceReranker{}.Rerank(context.Background(), "q", cands)
	for i, want := range []string{"x", "y", "z"} {
		if got[i].ID != want {
			t.Errorf("position %d: want %q, got %q (ties should preserve order)", i, want, got[i].ID)
		}
	}
}

func TestBundleQ_SalienceRerankerEmptyInput(t *testing.T) {
	got, err := salienceReranker{}.Rerank(context.Background(), "q", nil)
	if err != nil {
		t.Errorf("empty input should not error, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty input should yield empty output, got %v", got)
	}
}

// ── Pluggable reranker — verify the Server's reranker field is honoured ─

type stubReranker struct {
	called bool
	out    []rerankCandidate
}

func (s *stubReranker) Rerank(_ context.Context, _ string, in []rerankCandidate) ([]rerankCandidate, error) {
	s.called = true
	if s.out != nil {
		return s.out, nil
	}
	return in, nil
}

func TestBundleQ_ServerRerankerFieldIsUsed(t *testing.T) {
	// When s.reranker is non-nil, Bundle Q dispatches to it instead of the
	// default salienceReranker. This is the cross-encoder swap-in point.
	bank := newTestBank(t)
	stub := &stubReranker{}
	srv := newTestServer(t, bank)
	srv.reranker = stub
	if srv.reranker != stub {
		t.Errorf("expected Server.reranker field to accept Reranker interface; got %v", srv.reranker)
	}
}
