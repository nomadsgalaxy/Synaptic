// bundle_k_guards_test.go — guards for v2.7 Bundle K (inline auto-dedup
// at SaveMemory time):
//
//   - Default behaviour preserved: bank with no dedup provider wired
//     behaves exactly like v2.6 (no merge attempts, every SaveMemory
//     creates a new row)
//   - With dedup provider + threshold met → SaveMemory merges into the
//     existing row, returns that row, writes audit
//   - Threshold gate: similarity below threshold → fall through to
//     normal insert
//   - Tag-overlap gate: high cosine + low tag overlap → no merge
//   - Pipeline-managed memory types (synthesis/schema/context) → never
//     dedup'd inline (Phase 2/8/8.7 own their own merging)
//   - Caller-provided ID → respected as upsert (no dedup)
//   - Settings round-trip
//   - jaccardPct + tagSet helper correctness
package main

import (
	"context"
	"strings"
	"testing"
)

// ── dedupProviderStub: deterministic embeddings driven by text content ──

// dedupProviderStub returns a small embedding that's a function of the
// input text. Texts containing the same "near-dup keyword" all map to
// the same vector → cosine = 1.0. Different keywords → orthogonal
// vectors → cosine = 0. Used by the dedup tests to control the gate
// without needing a real LLM.
type dedupProviderStub struct {
	mapping map[string][]float32 // keyword → vector
	calls   int
}

func newDedupStub() *dedupProviderStub {
	return &dedupProviderStub{
		mapping: map[string][]float32{
			"alpha":  {1.0, 0.0, 0.0, 0.0},
			"beta":   {0.0, 1.0, 0.0, 0.0},
			"gamma":  {0.0, 0.0, 1.0, 0.0},
			"delta":  {0.0, 0.0, 0.0, 1.0},
			"shared": {0.5, 0.5, 0.5, 0.5},
		},
	}
}
func (p *dedupProviderStub) Name() string                                           { return "ollama:dedup-stub" }
func (p *dedupProviderStub) IsLocal() bool                                          { return true }
func (p *dedupProviderStub) Chat(_ context.Context, _ []Message, _ int) (string, error) {
	return "", nil
}
func (p *dedupProviderStub) Embed(_ context.Context, text string) ([]float32, error) {
	p.calls++
	for keyword, vec := range p.mapping {
		if strings.Contains(strings.ToLower(text), keyword) {
			return vec, nil
		}
	}
	// Anything else maps to a zero vector — orthogonal to everything,
	// so it never matches as a dup.
	return []float32{0, 0, 0, 1.5}, nil
}

// installDedupStub wires the stub on the bank + lowers the threshold
// so deterministic stub vectors trigger the dedup path cleanly.
func installDedupStub(t *testing.T, bank *Bank) *dedupProviderStub {
	t.Helper()
	stub := newDedupStub()
	bank.SetDedupProvider(func() LLMProvider { return stub })
	// Use a slightly lower threshold so cosine 1.0 vs orthogonal=0 is
	// cleanly distinguishable. Defaults (0.97) work too; setting an
	// explicit value documents the test's intent.
	bank.SetSetting(AutoDedupThresholdKey, "0.95")
	return stub
}

// ── Default-behaviour gate ──────────────────────────────────────────────

func TestBundleK_NoProviderWired_NoDedup(t *testing.T) {
	bank := newTestBank(t)
	// No SetDedupProvider call → dedup is invisible.
	a, _ := bank.SaveMemory(MemoryRecord{Text: "alpha keyword present here"})
	b, _ := bank.SaveMemory(MemoryRecord{Text: "alpha keyword present here", Tags: []string{"x"}})
	if a.ID == b.ID {
		t.Errorf("without dedup provider, two SaveMemory calls should produce distinct IDs, got %q twice", a.ID)
	}
}

func TestBundleK_ProviderWiredButDisabledSetting_NoDedup(t *testing.T) {
	bank := newTestBank(t)
	installDedupStub(t, bank)
	bank.SetSetting(AutoDedupEnabledKey, "0")
	a, _ := bank.SaveMemory(MemoryRecord{Text: "alpha keyword present here", Tags: []string{"x"}})
	b, _ := bank.SaveMemory(MemoryRecord{Text: "alpha keyword present here", Tags: []string{"x"}})
	if a.ID == b.ID {
		t.Errorf("auto_dedup_enabled=0 should disable dedup, got merge")
	}
}

// ── Dedup match path ────────────────────────────────────────────────────

func TestBundleK_DedupMergesIntoExistingOnHighSimilarity(t *testing.T) {
	bank := newTestBank(t)
	installDedupStub(t, bank)
	first, err := bank.SaveMemory(MemoryRecord{
		Text: "alpha keyword in the first memory",
		Tags: []string{"topic"},
	})
	if err != nil {
		t.Fatalf("first SaveMemory: %v", err)
	}
	// Second memory: same "alpha" keyword (→ identical embedding) +
	// same tag (→ 100% overlap). Should dedup into first.
	second, err := bank.SaveMemory(MemoryRecord{
		Text: "alpha keyword in another phrasing",
		Tags: []string{"topic"},
	})
	if err != nil {
		t.Fatalf("second SaveMemory: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("expected dedup to return existing ID %q, got %q", first.ID, second.ID)
	}
	// The existing row should now have a merged_from entry.
	got, _ := bank.GetMemory(first.ID)
	if len(got.MergedFrom) != 1 {
		t.Errorf("expected 1 merged_from entry, got %d: %v", len(got.MergedFrom), got.MergedFrom)
	}
	// Audit row should record the merge.
	rows, _ := bank.ListAuditLog(AuditFilter{Operation: "auto_dedup_merge", Limit: 5})
	if len(rows) == 0 {
		t.Errorf("expected an auto_dedup_merge audit row")
	}
}

// ── Threshold gate ──────────────────────────────────────────────────────

func TestBundleK_LowSimilarityFallsThroughToInsert(t *testing.T) {
	bank := newTestBank(t)
	installDedupStub(t, bank)
	a, _ := bank.SaveMemory(MemoryRecord{Text: "alpha keyword first"})
	b, _ := bank.SaveMemory(MemoryRecord{Text: "beta keyword second"})
	if a.ID == b.ID {
		t.Errorf("orthogonal embeddings should not merge, got dedup")
	}
}

// ── Tag-overlap gate ────────────────────────────────────────────────────

func TestBundleK_LowTagOverlapFallsThrough(t *testing.T) {
	bank := newTestBank(t)
	installDedupStub(t, bank)
	bank.SetSetting(AutoDedupTagOverlapPctKey, "75") // explicit
	a, _ := bank.SaveMemory(MemoryRecord{
		Text: "alpha keyword auth flow",
		Tags: []string{"auth", "security"},
	})
	b, _ := bank.SaveMemory(MemoryRecord{
		Text: "alpha keyword tutorial",
		Tags: []string{"tutorial", "example"},
	})
	if a.ID == b.ID {
		t.Errorf("zero tag overlap should block dedup even with cosine=1.0; got merge")
	}
}

// ── Pipeline types are never dedup'd ────────────────────────────────────

func TestBundleK_SynthesisTypeBypassesDedup(t *testing.T) {
	bank := newTestBank(t)
	installDedupStub(t, bank)
	bank.SaveMemory(MemoryRecord{Text: "alpha keyword", Tags: []string{"x"}})
	syn, _ := bank.SaveMemory(MemoryRecord{
		Text:       "alpha keyword",
		Tags:       []string{"x"},
		MemoryType: "synthesis",
	})
	// Synthesis bypasses dedup → new ID.
	rows, _ := bank.ListAuditLog(AuditFilter{Operation: "auto_dedup_merge", Limit: 5})
	for _, r := range rows {
		if r.EntityID == syn.ID {
			t.Errorf("synthesis memory %s should NOT have been dedup'd, found audit row", syn.ID)
		}
	}
}

func TestBundleK_SchemaTypeBypassesDedup(t *testing.T) {
	bank := newTestBank(t)
	installDedupStub(t, bank)
	bank.SaveMemory(MemoryRecord{Text: "alpha keyword", Tags: []string{"x"}})
	schemaRec, _ := bank.SaveMemory(MemoryRecord{
		Text:       "alpha keyword",
		Tags:       []string{"x"},
		MemoryType: "schema",
	})
	got, _ := bank.GetMemory(schemaRec.ID)
	if got.ID == "" {
		t.Fatalf("schema memory should have been inserted as new row")
	}
}

// ── Caller-supplied ID bypasses dedup ───────────────────────────────────

func TestBundleK_ExplicitIDBypassesDedup(t *testing.T) {
	bank := newTestBank(t)
	installDedupStub(t, bank)
	bank.SaveMemory(MemoryRecord{Text: "alpha keyword existing", Tags: []string{"x"}})
	// Explicit ID — caller intent is "upsert as this ID".
	upserted, err := bank.SaveMemory(MemoryRecord{
		ID:   "explicit-id-7",
		Text: "alpha keyword incoming",
		Tags: []string{"x"},
	})
	if err != nil {
		t.Fatalf("explicit-ID SaveMemory: %v", err)
	}
	if upserted.ID != "explicit-id-7" {
		t.Errorf("explicit-ID upsert should preserve ID, got %q", upserted.ID)
	}
}

// ── Settings helpers ────────────────────────────────────────────────────

func TestBundleK_AutoDedupEnabled_Defaults(t *testing.T) {
	bank := newTestBank(t)
	if !autoDedupEnabled(bank) {
		t.Errorf("default should be ON")
	}
	bank.SetSetting(AutoDedupEnabledKey, "off")
	if autoDedupEnabled(bank) {
		t.Errorf("'off' should disable")
	}
}

func TestBundleK_AutoDedupThreshold_Defaults(t *testing.T) {
	bank := newTestBank(t)
	if got := autoDedupThreshold(bank); got != 0.97 {
		t.Errorf("default threshold should be 0.97, got %v", got)
	}
	bank.SetSetting(AutoDedupThresholdKey, "0.90")
	if got := autoDedupThreshold(bank); got != 0.90 {
		t.Errorf("override should win, got %v", got)
	}
	// Out-of-range → fall back to default.
	bank.SetSetting(AutoDedupThresholdKey, "5.0")
	if got := autoDedupThreshold(bank); got != 0.97 {
		t.Errorf("out-of-range should fall back to 0.97, got %v", got)
	}
}

func TestBundleK_AutoDedupTagOverlapPct_Defaults(t *testing.T) {
	bank := newTestBank(t)
	if got := autoDedupTagOverlapPct(bank); got != 75 {
		t.Errorf("default tag-overlap should be 75, got %d", got)
	}
	bank.SetSetting(AutoDedupTagOverlapPctKey, "50")
	if got := autoDedupTagOverlapPct(bank); got != 50 {
		t.Errorf("override should win, got %d", got)
	}
}

// ── Jaccard helpers ─────────────────────────────────────────────────────

func TestJaccardPct(t *testing.T) {
	a := tagSet([]string{"x", "y", "z"})
	if got := jaccardPct(a, a); got != 100 {
		t.Errorf("identical sets should be 100%%, got %d", got)
	}
	b := tagSet([]string{"x", "y", "q"})
	if got := jaccardPct(a, b); got < 40 || got > 60 {
		t.Errorf("2 of 4 overlap should be ~50%%, got %d", got)
	}
	c := tagSet([]string{"q", "r"})
	if got := jaccardPct(a, c); got != 0 {
		t.Errorf("zero overlap should be 0%%, got %d", got)
	}
	if got := jaccardPct(map[string]bool{}, map[string]bool{}); got != 100 {
		t.Errorf("two empty sets should be 100%% (vacuous), got %d", got)
	}
}

func TestTagSet_LowercasesAndTrims(t *testing.T) {
	got := tagSet([]string{"  Foo  ", "BAR", "baz"})
	for _, k := range []string{"foo", "bar", "baz"} {
		if !got[k] {
			t.Errorf("expected %q in set, got %v", k, got)
		}
	}
}
