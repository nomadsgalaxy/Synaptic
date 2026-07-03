// bundle_a_guards_test.go — guards for v2.6 Bundle A:
//
//   - Tier 1 [think:off] toggle (settings.tier1_think_off + ThinkOffPrefix)
//   - providerModelLabel strips the kind prefix from Provider.Name()
//   - classifier.go's response parser tolerates [think:off] echo AND
//     reasoning-model <think>…</think> trace blocks (regression guards)
//   - recallEmbedQuery surfaces "no embedding provider" when called with nil
//
// These tests exercise pure helpers + the response parser. They are
// independent of Ollama, the bank schema, and HTTP wiring — they should
// run in well under a second and be safe to keep in the suite when later
// bundles refactor MemoryClassifier (the parser logic survives unchanged).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// ── settings.tier1_think_off ──────────────────────────────────────────────

func TestThinkOffEnabled_DefaultsOn(t *testing.T) {
	// nil bank → default ON. Preserves documented latency-win behaviour
	// for users who never touch the toggle.
	var b *Bank
	if !b.ThinkOffEnabled() {
		t.Fatalf("nil bank should default ON")
	}
	if b.ThinkOffPrefix() != "[think:off]\n\n" {
		t.Fatalf("nil bank prefix should be the directive, got %q", b.ThinkOffPrefix())
	}

	// Real bank with no setting row → default ON.
	bank := newTestBank(t)
	if !bank.ThinkOffEnabled() {
		t.Fatalf("absent setting should default ON")
	}
}

func TestThinkOffEnabled_DisabledValues(t *testing.T) {
	bank := newTestBank(t)
	for _, val := range []string{"0", "false", "off", "no", "FALSE", "  off  "} {
		if err := bank.SetSetting(ThinkOffEnabledKey, val); err != nil {
			t.Fatalf("SetSetting %q: %v", val, err)
		}
		if bank.ThinkOffEnabled() {
			t.Errorf("setting %q should disable, got enabled", val)
		}
		if bank.ThinkOffPrefix() != "" {
			t.Errorf("setting %q should give empty prefix, got %q", val, bank.ThinkOffPrefix())
		}
	}
}

func TestThinkOffEnabled_EnabledValues(t *testing.T) {
	bank := newTestBank(t)
	for _, val := range []string{"1", "true", "on", "yes", "anything-else", ""} {
		if err := bank.SetSetting(ThinkOffEnabledKey, val); err != nil {
			t.Fatalf("SetSetting %q: %v", val, err)
		}
		if !bank.ThinkOffEnabled() {
			t.Errorf("setting %q should enable, got disabled", val)
		}
	}
}

// ── providerModelLabel ────────────────────────────────────────────────────

func TestProviderModelLabel(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ollama prefix", "ollama:llama3.2:3b", "llama3.2:3b"},
		{"remote prefix", "remote:gpt-4o", "gpt-4o"},
		{"local prefix", "local:nomic-embed-text-v1.5", "nomic-embed-text-v1.5"},
		{"no prefix", "scripted", "scripted"},
		{"empty name", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := providerModelLabel(&fakeProvider{name: tc.in, local: true})
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	if got := providerModelLabel(nil); got != "" {
		t.Errorf("nil provider should return empty, got %q", got)
	}
}

// ── classifier parser robustness ──────────────────────────────────────────

// parserStubProvider is a minimal LLMProvider whose Chat() returns a
// canned string. The Bundle C provider-agnostic refactor moved the
// classifier off raw HTTP, so the parser test no longer needs an
// httptest Ollama — just a Chat stub.
type parserStubProvider struct{ content string }

func (p *parserStubProvider) Name() string                                              { return "ollama:test-model" }
func (p *parserStubProvider) IsLocal() bool                                             { return true }
func (p *parserStubProvider) Embed(_ context.Context, _ string) ([]float32, error)      { return nil, nil }
func (p *parserStubProvider) Chat(_ context.Context, _ []Message, _ int) (string, error) {
	return p.content, nil
}

// classifierParserHarness wires a stub provider returning the given
// content, then routes a single classify call through tier3Ollama to
// verify the parser picks the right region key out.
func classifierParserHarness(t *testing.T, modelContent string) string {
	t.Helper()
	tmpDir := t.TempDir()
	cache := filepath.Join(tmpDir, "cache.json")
	mem := filepath.Join(tmpDir, "memories.json")
	stub := &parserStubProvider{content: modelContent}
	mc := NewMemoryClassifier(mem, cache, func() LLMProvider { return stub }, nil, nil)

	rec := map[string]interface{}{
		"id":      "test-1",
		"text":    "test memory text",
		"tags":    []interface{}{},
		"context": "",
	}
	return mc.tier3Ollama(context.Background(), rec)
}

func TestClassifierParser_ExactKey(t *testing.T) {
	got := classifierParserHarness(t, "hippocampus")
	if got != "hippocampus" {
		t.Errorf("got %q, want hippocampus", got)
	}
}

func TestClassifierParser_ThinkOffPrefixEcho(t *testing.T) {
	// Regression: when the model echoes the `[think:off]` token back
	// before the region key, the substring scan should still find it.
	got := classifierParserHarness(t, "[think:off] hippocampus")
	if got != "hippocampus" {
		t.Errorf("got %q, want hippocampus", got)
	}
}

func TestClassifierParser_ThinkBlockStripped(t *testing.T) {
	// Reasoning-model trace block before the answer.
	got := classifierParserHarness(t, "<think>let me consider this</think>\nhippocampus")
	if got != "hippocampus" {
		t.Errorf("got %q, want hippocampus", got)
	}
}

func TestClassifierParser_ThinkBlockInline(t *testing.T) {
	// Trace block on the same line as the answer.
	got := classifierParserHarness(t, "<think>weighing</think> amygdala")
	if got != "amygdala" {
		t.Errorf("got %q, want amygdala", got)
	}
}

func TestClassifierParser_UnknownReturnsEmpty(t *testing.T) {
	got := classifierParserHarness(t, "i have no idea what region this is")
	if got != "" {
		t.Errorf("unrecognised content should return empty, got %q", got)
	}
}

// ── recallEmbedQuery nil-provider behaviour ───────────────────────────────

func TestRecallEmbedQuery_NilProviderReturnsErr(t *testing.T) {
	bank := newTestBank(t)
	_, err := recallEmbedQuery(context.Background(), bank, "anything", nil)
	if err == nil {
		t.Fatalf("expected error when provider is nil")
	}
	if !strings.Contains(err.Error(), "embedding provider") {
		t.Errorf("error should mention embedding provider, got: %v", err)
	}
}

// ── postRecall returns 503 when router has no embed provider ──────────────

func TestPostRecall_ReturnsServiceUnavailable_WhenRouterEmpty(t *testing.T) {
	bank := newTestBank(t)
	// Router exists but Tier 1 is remote (IsLocal=false) → ForEmbedding returns nil.
	remoteTier1 := &fakeProvider{name: "remote:gpt-4", local: false}
	server := &Server{
		bank:   bank,
		router: NewModelRouter(remoteTier1, nil, nil, nil),
	}
	body, _ := json.Marshal(recallRequest{Query: "anything", Limit: 5})
	req := httptest.NewRequest(http.MethodPost, "/recall", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	server.postRecall(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
}
