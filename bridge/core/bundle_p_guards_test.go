// bundle_p_guards_test.go — guards for v2.7 Bundle P (default embed
// tier = nomic-embed-text:v1.5 + auto-migrate on drift):
//
//   - SD_SKIP_EMBED_MIGRATION=1 + drift → router's embed tier swaps
//     back to a provider whose Name() matches the legacy label, so
//     subsequent /recall queries against AllEmbeddingsForMemories(t1Label)
//     keep finding existing rows. No goroutine launched.
//   - SD_SKIP_EMBED_MIGRATION unset + drift → runEmbedAutoMigration
//     fires; iterates memories; SaveEmbedding writes rows under the
//     NEW label; audit row records the run.
//   - Idempotent re-run: rows already present under the new label
//     don't re-embed.
//   - Bank-empty: no goroutine, no audit row, no log.
//
// The migration goroutine is run synchronously (not via `go`) inside
// the test so we can assert on outcomes without polling. main.go's
// production path wraps the same function in a goroutine.
package main

import (
	"context"
	"testing"
	"time"
)

// ── SKIP env: legacy tier preserved ─────────────────────────────────────

func TestBundleP_SkipEnvKeepsLegacyEmbedTier(t *testing.T) {
	t.Setenv("SD_SKIP_EMBED_MIGRATION", "1")
	bank := newTestBank(t)
	for i := 0; i < 3; i++ {
		_ = bank.SaveEmbedding("h-"+string(rune('a'+i)), []float32{0.1, 0.2, 0.3}, "llama3.2:3b")
	}
	t1 := &fakeProvider{name: "ollama:llama3.2:3b", local: true}
	et := &fakeProvider{name: "ollama:nomic-embed-text:v1.5", local: true}
	r := NewModelRouter(t1, nil, nil, nil)
	r.SetEmbedTier(et)

	maybeMigrateEmbedLabelDrift(context.Background(), bank, r, nil)

	// Embed tier should now resolve to the legacy label so
	// AllEmbeddingsForMemories("llama3.2:3b") finds existing rows.
	got := r.EmbedTier()
	if got == nil {
		t.Fatalf("expected legacy embed tier installed, got nil")
	}
	if providerModelLabel(got) != "llama3.2:3b" {
		t.Errorf("expected legacy label %q, got %q", "llama3.2:3b", providerModelLabel(got))
	}
}

// ── Default (no SKIP env): migration runs ───────────────────────────────

// migrationStubProvider returns a deterministic 4-dim vector for every
// Embed call so the test doesn't need a live Ollama. Its name carries
// the bare "nomic-embed-text:v1.5" so providerModelLabel resolves to
// that.
type migrationStubProvider struct {
	calls int
}

func (p *migrationStubProvider) Name() string                                            { return "ollama:nomic-embed-text:v1.5" }
func (p *migrationStubProvider) IsLocal() bool                                           { return true }
func (p *migrationStubProvider) Embed(_ context.Context, _ string) ([]float32, error)    {
	p.calls++
	return []float32{0.5, 0.5, 0.5, 0.5}, nil
}
func (p *migrationStubProvider) Chat(_ context.Context, _ []Message, _ int) (string, error) {
	return "", nil
}

func TestBundleP_DefaultRunsMigration_ReEmbedsUnderNewLabel(t *testing.T) {
	// No SKIP env → migration path. The production main.go wraps the
	// call in `go`; we invoke runEmbedAutoMigration directly so the test
	// runs to completion before assertions.
	bank := newTestBank(t)
	// Seed 4 memories that will need migration. Persist them via
	// SaveMemory so the FTS5 triggers fire and the rows have real IDs.
	for i := 0; i < 4; i++ {
		rec, err := bank.SaveMemory(MemoryRecord{
			Text: "test memory " + string(rune('a'+i)),
			Tags: []string{"smoketest"},
		})
		if err != nil {
			t.Fatalf("SaveMemory: %v", err)
		}
		_ = rec
	}
	// Seed embeddings under the OLD label so countByModel sees drift.
	// (SaveMemory itself doesn't write embeddings — that's a separate
	// phase. So we manually inject what would have been there.)
	for i := 0; i < 4; i++ {
		hash := "old-row-" + string(rune('a'+i))
		_ = bank.SaveEmbedding(hash, []float32{0.1, 0.1, 0.1, 0.1}, "llama3.2:3b")
	}
	oldCount, _ := bank.CountEmbeddingsByModel("llama3.2:3b")
	if oldCount == 0 {
		t.Fatalf("test setup: expected legacy rows present, got 0")
	}

	stub := &migrationStubProvider{}
	runEmbedAutoMigration(context.Background(), bank, stub, "nomic-embed-text:v1.5", "llama3.2:3b", oldCount, nil)

	// After migration, the bank should have rows under the new label.
	newCount, err := bank.CountEmbeddingsByModel("nomic-embed-text:v1.5")
	if err != nil {
		t.Fatalf("CountEmbeddingsByModel new: %v", err)
	}
	if newCount == 0 {
		t.Errorf("expected rows under new label after migration, got 0")
	}
	// The stub should have been called once per memory (4 SaveMemory rows).
	if stub.calls < 4 {
		t.Errorf("expected ≥4 Embed calls (one per memory), got %d", stub.calls)
	}
}

func TestBundleP_AuditRowWrittenOnCompletion(t *testing.T) {
	bank := newTestBank(t)
	bank.SaveMemory(MemoryRecord{Text: "alpha", Tags: []string{"x"}})
	bank.SaveMemory(MemoryRecord{Text: "beta", Tags: []string{"x"}})
	_ = bank.SaveEmbedding("h1", []float32{0.1, 0.2, 0.3, 0.4}, "old-label")
	_ = bank.SaveEmbedding("h2", []float32{0.5, 0.5, 0.5, 0.5}, "old-label")

	stub := &migrationStubProvider{}
	runEmbedAutoMigration(context.Background(), bank, stub, "new-label", "old-label", 2, nil)

	rows, err := bank.ListAuditLog(AuditFilter{Operation: "embed_auto_migration", Limit: 1})
	if err != nil {
		t.Fatalf("ListAuditLog: %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("expected an embed_auto_migration audit row, found none")
	}
	if !bytesContains(rows[0].AfterJSON, `"old_label":"old-label"`) || !bytesContains(rows[0].AfterJSON, `"new_label":"new-label"`) {
		t.Errorf("audit row missing label fields:\n%s", rows[0].AfterJSON)
	}
}

func TestBundleP_BankEmpty_NoMigration(t *testing.T) {
	bank := newTestBank(t)
	t1 := &fakeProvider{name: "ollama:llama3.2:3b", local: true}
	et := &migrationStubProvider{}
	r := NewModelRouter(t1, nil, nil, nil)
	r.SetEmbedTier(et)
	out := captureLog(t, func() { maybeMigrateEmbedLabelDrift(context.Background(), bank, r, nil) })
	if out != "" {
		t.Errorf("expected no log on empty bank, got:\n%s", out)
	}
	if et.calls != 0 {
		t.Errorf("expected zero Embed calls on empty bank, got %d", et.calls)
	}
}

// ── Idempotency: re-run skips already-embedded hashes ───────────────────

func TestBundleP_IdempotentReRun_SkipsExistingHashes(t *testing.T) {
	bank := newTestBank(t)
	bank.SaveMemory(MemoryRecord{Text: "shared text", Tags: []string{"x"}})

	stub := &migrationStubProvider{}
	// First run: embeds the memory under new-label.
	runEmbedAutoMigration(context.Background(), bank, stub, "new-label", "old-label", 1, nil)
	firstCalls := stub.calls

	// Second run: every hash now exists in the cache → skipped.
	runEmbedAutoMigration(context.Background(), bank, stub, "new-label", "old-label", 1, nil)
	if stub.calls != firstCalls {
		t.Errorf("expected zero new Embed calls on re-run (idempotent), got %d new", stub.calls-firstCalls)
	}
}

// ── Context cancellation halts the run ──────────────────────────────────

func TestBundleP_CtxCancelHaltsMidRun(t *testing.T) {
	bank := newTestBank(t)
	for i := 0; i < 20; i++ {
		bank.SaveMemory(MemoryRecord{Text: "memory-" + string(rune('a'+i%26))})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the run starts

	stub := &migrationStubProvider{}
	start := time.Now()
	runEmbedAutoMigration(ctx, bank, stub, "new-label", "old-label", 20, nil)
	// Should return almost immediately since ctx is cancelled.
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("expected fast return on cancelled ctx, took %s", elapsed)
	}
}

// ── helpers ─────────────────────────────────────────────────────────────

func bytesContains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && contains_(haystack, needle)
}
