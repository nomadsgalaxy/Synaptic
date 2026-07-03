// bundle_d_guards_test.go — guards for v2.6 Bundle D (embed tier wiring).
//
//   - Router.ForEmbedding() prefers the dedicated EmbedTier when set & local
//   - Router.ForEmbedding() falls back to Tier 1 when no EmbedTier (default)
//   - SetEmbedTier / EmbedTier / ReplaceTier(TierEmbedding) plumbing works
//   - ReplaceTier(TierEmbedding, remote-provider) is REFUSED (cosine-compat)
//   - Bank.CountEmbeddingsByModel returns accurate counts
//   - maybeMigrateEmbedLabelDrift (v2.7 Bundle P evolution of the v2.6
//     warn-only helper) fires the auto-migrate path when bank label !=
//     current embed label, and stays quiet when they match or no rows
//     exist (no false positives). The migration goroutine itself is
//     exercised in bundle_p_guards_test.go; this file covers the gating.
//
// Default behaviour preservation (the v2.6 principle): with no embed
// tier configured, every result here is identical to v2.5.
package main

import (
	"bytes"
	"context"
	"log"
	"testing"
)

// ── ForEmbedding routing ────────────────────────────────────────────────

func TestForEmbedding_FallsBackToTier1WhenNoEmbedTier(t *testing.T) {
	t1 := &fakeProvider{name: "ollama:llama3.2:3b", local: true}
	r := NewModelRouter(t1, nil, nil, nil)
	got := r.ForEmbedding()
	if got != t1 {
		t.Errorf("expected Tier 1 fallback, got %v", got)
	}
}

func TestForEmbedding_PrefersEmbedTierOverTier1(t *testing.T) {
	t1 := &fakeProvider{name: "ollama:llama3.2:3b", local: true}
	et := &fakeProvider{name: "ollama:nomic-embed-text-v1.5", local: true}
	r := NewModelRouter(t1, nil, nil, nil)
	r.SetEmbedTier(et)
	got := r.ForEmbedding()
	if got != et {
		t.Errorf("expected EmbedTier to win, got %v", got)
	}
}

func TestForEmbedding_RejectsNonLocalEmbedTier(t *testing.T) {
	// A non-local embed tier in the field would breach cosine-compat;
	// ForEmbedding skips it and falls back to Tier 1. (ReplaceTier
	// refuses to install one in the first place — see below — but
	// SetEmbedTier doesn't gate, so this is the runtime safety net.)
	t1 := &fakeProvider{name: "ollama:llama3.2:3b", local: true}
	remote := &fakeProvider{name: "openai:text-embedding-3-small", local: false}
	r := NewModelRouter(t1, nil, nil, nil)
	r.SetEmbedTier(remote)
	if got := r.ForEmbedding(); got != t1 {
		t.Errorf("non-local embed tier should be skipped, got %v", got)
	}
}

func TestForEmbedding_NilWhenTier1RemoteAndNoEmbedTier(t *testing.T) {
	t1 := &fakeProvider{name: "openai:gpt-4o-mini", local: false}
	r := NewModelRouter(t1, nil, nil, nil)
	if got := r.ForEmbedding(); got != nil {
		t.Errorf("expected nil when Tier 1 is remote and no embed tier, got %v", got)
	}
}

// ── ReplaceTier(TierEmbedding, ...) ─────────────────────────────────────

func TestReplaceTier_EmbedAcceptsLocal(t *testing.T) {
	r := NewModelRouter(nil, nil, nil, nil)
	et := &fakeProvider{name: "ollama:nomic", local: true}
	old, ok := r.ReplaceTier(TierEmbedding, et)
	if !ok {
		t.Fatalf("expected ReplaceTier to accept local embed provider")
	}
	if old != nil {
		t.Errorf("first install: old should be nil, got %v", old)
	}
	if r.EmbedTier() != et {
		t.Errorf("EmbedTier should now be the installed provider")
	}

	// Swap to a new local provider.
	et2 := &fakeProvider{name: "ollama:nomic-v2", local: true}
	old2, ok2 := r.ReplaceTier(TierEmbedding, et2)
	if !ok2 || old2 != et {
		t.Errorf("swap: expected old==et, got old=%v ok=%v", old2, ok2)
	}
	if r.EmbedTier() != et2 {
		t.Errorf("EmbedTier should now be et2")
	}
}

func TestReplaceTier_EmbedRefusesRemote(t *testing.T) {
	r := NewModelRouter(nil, nil, nil, nil)
	remote := &fakeProvider{name: "openai:text-embedding-3-small", local: false}
	_, ok := r.ReplaceTier(TierEmbedding, remote)
	if ok {
		t.Errorf("ReplaceTier(TierEmbedding, remote) should be refused")
	}
	if r.EmbedTier() != nil {
		t.Errorf("EmbedTier should remain nil after refused install")
	}
}

// ── Bank.CountEmbeddingsByModel ─────────────────────────────────────────

func TestCountEmbeddingsByModel_AccurateCounts(t *testing.T) {
	bank := newTestBank(t)
	// Drop a couple of embeddings under two different labels.
	vec := []float32{0.1, 0.2, 0.3, 0.4}
	for i, label := range []string{"llama3.2:3b", "llama3.2:3b", "nomic-embed-text"} {
		hash := "h" + string(rune('a'+i))
		if err := bank.SaveEmbedding(hash, vec, label); err != nil {
			t.Fatalf("SaveEmbedding: %v", err)
		}
	}
	if got, _ := bank.CountEmbeddingsByModel("llama3.2:3b"); got != 2 {
		t.Errorf("llama3.2:3b: want 2, got %d", got)
	}
	if got, _ := bank.CountEmbeddingsByModel("nomic-embed-text"); got != 1 {
		t.Errorf("nomic: want 1, got %d", got)
	}
	if got, _ := bank.CountEmbeddingsByModel("does-not-exist"); got != 0 {
		t.Errorf("absent label: want 0, got %d", got)
	}
}

func TestCountEmbeddingsByModel_NilBankSafe(t *testing.T) {
	var b *Bank
	got, err := b.CountEmbeddingsByModel("anything")
	if err != nil || got != 0 {
		t.Errorf("nil-bank should return (0, nil), got (%d, %v)", got, err)
	}
}

// ── maybeMigrateEmbedLabelDrift gating (Bundle D + P) ───────────────────

// captureLog runs fn while routing log.Printf into buf. Restores the
// global logger destination on exit.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	fn()
	return buf.String()
}

func TestMaybeMigrateEmbedLabelDrift_LogsOnMismatch(t *testing.T) {
	// Use the SKIP_MIGRATION path so the gating fires WITHOUT spawning
	// the background goroutine — keeps this test deterministic. The
	// goroutine itself is tested in bundle_p_guards_test.go with a
	// stub provider.
	t.Setenv("SD_SKIP_EMBED_MIGRATION", "1")
	bank := newTestBank(t)
	vec := []float32{0.1, 0.2, 0.3}
	for i := 0; i < 5; i++ {
		hash := "row-" + string(rune('a'+i))
		_ = bank.SaveEmbedding(hash, vec, "llama3.2:3b")
	}
	t1 := &fakeProvider{name: "ollama:llama3.2:3b", local: true}
	et := &fakeProvider{name: "ollama:nomic-embed-text-v1.5", local: true}
	r := NewModelRouter(t1, nil, nil, nil)
	r.SetEmbedTier(et)

	out := captureLog(t, func() { maybeMigrateEmbedLabelDrift(context.Background(), bank, r, nil) })
	if out == "" {
		t.Fatalf("expected a log line, got nothing")
	}
	if !contains_(out, "keeping legacy embed tier") {
		t.Errorf("expected skip-path log, got:\n%s", out)
	}
}

func TestMaybeMigrateEmbedLabelDrift_QuietWhenLabelsMatch(t *testing.T) {
	t.Setenv("SD_SKIP_EMBED_MIGRATION", "1")
	bank := newTestBank(t)
	vec := []float32{0.1, 0.2, 0.3}
	_ = bank.SaveEmbedding("h", vec, "llama3.2:3b")

	t1 := &fakeProvider{name: "ollama:llama3.2:3b", local: true}
	r := NewModelRouter(t1, nil, nil, nil)
	// No embed tier set — labels match (both bare "llama3.2:3b").

	out := captureLog(t, func() { maybeMigrateEmbedLabelDrift(context.Background(), bank, r, nil) })
	if out != "" {
		t.Errorf("expected no log when labels match, got:\n%s", out)
	}
}

func TestMaybeMigrateEmbedLabelDrift_QuietWhenBankEmpty(t *testing.T) {
	t.Setenv("SD_SKIP_EMBED_MIGRATION", "1")
	bank := newTestBank(t)
	t1 := &fakeProvider{name: "ollama:llama3.2:3b", local: true}
	et := &fakeProvider{name: "ollama:nomic", local: true}
	r := NewModelRouter(t1, nil, nil, nil)
	r.SetEmbedTier(et)
	out := captureLog(t, func() { maybeMigrateEmbedLabelDrift(context.Background(), bank, r, nil) })
	if out != "" {
		t.Errorf("expected no log when bank empty, got:\n%s", out)
	}
}

func TestMaybeMigrateEmbedLabelDrift_QuietWhenNoEmbedProvider(t *testing.T) {
	t.Setenv("SD_SKIP_EMBED_MIGRATION", "1")
	bank := newTestBank(t)
	vec := []float32{0.1, 0.2, 0.3}
	_ = bank.SaveEmbedding("h", vec, "anything")
	r := NewModelRouter(nil, nil, nil, nil)
	out := captureLog(t, func() { maybeMigrateEmbedLabelDrift(context.Background(), bank, r, nil) })
	if out != "" {
		t.Errorf("expected no log when no embed provider, got:\n%s", out)
	}
}

// ── Tiny helper (avoids importing strings just for this) ───────────────

func contains_(s, sub string) bool {
	return bytes.Contains([]byte(s), []byte(sub))
}
