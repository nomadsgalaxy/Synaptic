// nightly_pipeline_llm_timeout_test.go — wave 8e tests for the
// llm_call_timeout_sec setting + provider-aware default + helper.
package main

import (
	"testing"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────
// DefaultDreamPipelineSettings.LLMCallTimeoutSec
// ──────────────────────────────────────────────────────────────────────────

func TestDefaultDreamPipelineSettings_LLMCallTimeoutDefault(t *testing.T) {
	s := DefaultDreamPipelineSettings()
	if s.LLMCallTimeoutSec != 120 {
		t.Errorf("default LLMCallTimeoutSec: want 120, got %d", s.LLMCallTimeoutSec)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Settings round-trip
// ──────────────────────────────────────────────────────────────────────────

func TestLLMCallTimeout_PersistsExplicitValue(t *testing.T) {
	bank := newTestBank(t)
	s := DefaultDreamPipelineSettings()
	s.LLMCallTimeoutSec = 600
	if err := bank.SaveDreamPipelineSettings(s); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, _ := bank.GetDreamPipelineSettings()
	if got.LLMCallTimeoutSec != 600 {
		t.Errorf("roundtrip: want 600, got %d", got.LLMCallTimeoutSec)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Provider-aware default: unset + Tier 2 AirLLM → 1800
// ──────────────────────────────────────────────────────────────────────────

func TestLLMCallTimeout_ProviderAwareDefaultBumpsToAirLLMCeiling(t *testing.T) {
	bank := newTestBank(t)
	// Configure Tier 2 as AirLLM. Don't write the timeout setting; we
	// want the provider-aware-default path.
	if err := bank.SaveProviderConfig(Tier2, ProviderConfig{
		Kind: ProviderAirLLM, BaseURL: "http://x:9912", Model: "test",
	}); err != nil {
		t.Fatalf("save provider: %v", err)
	}
	got, _ := bank.GetDreamPipelineSettings()
	if got.LLMCallTimeoutSec != 1800 {
		t.Errorf("AirLLM + unset: want 1800 auto-default, got %d", got.LLMCallTimeoutSec)
	}
}

func TestLLMCallTimeout_ProviderAwareDoesNotOverrideExplicit(t *testing.T) {
	bank := newTestBank(t)
	// Tier 2 as AirLLM AND an explicit user-set timeout — user wins.
	bank.SaveProviderConfig(Tier2, ProviderConfig{
		Kind: ProviderAirLLM, BaseURL: "http://x:9912", Model: "test",
	})
	s := DefaultDreamPipelineSettings()
	s.LLMCallTimeoutSec = 900
	bank.SaveDreamPipelineSettings(s)
	got, _ := bank.GetDreamPipelineSettings()
	if got.LLMCallTimeoutSec != 900 {
		t.Errorf("explicit user value should win over AirLLM auto-bump; got %d", got.LLMCallTimeoutSec)
	}
}

func TestLLMCallTimeout_OllamaTier2KeepsBaseline(t *testing.T) {
	bank := newTestBank(t)
	// Default Tier 2 is Ollama (or whatever DefaultProviderConfig
	// returns — verify the auto-bump doesn't fire for non-AirLLM).
	bank.SaveProviderConfig(Tier2, ProviderConfig{
		Kind: ProviderOllama, BaseURL: "http://ollama-tier2:11434", Model: "llama3.1:8b",
	})
	got, _ := bank.GetDreamPipelineSettings()
	if got.LLMCallTimeoutSec != 120 {
		t.Errorf("Ollama Tier 2 should keep 120s baseline; got %d", got.LLMCallTimeoutSec)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// pipelineContext.llmCallTimeout helper
// ──────────────────────────────────────────────────────────────────────────

func TestPipelineContext_LLMCallTimeoutHelper(t *testing.T) {
	pc := &pipelineContext{settings: DreamPipelineSettings{LLMCallTimeoutSec: 600}}
	if got := pc.llmCallTimeout(); got != 600*time.Second {
		t.Errorf("want 600s, got %s", got)
	}
}

func TestPipelineContext_LLMCallTimeoutZeroFallsBackToBaseline(t *testing.T) {
	pc := &pipelineContext{settings: DreamPipelineSettings{LLMCallTimeoutSec: 0}}
	if got := pc.llmCallTimeout(); got != 120*time.Second {
		t.Errorf("zero should fall back to 120s, got %s", got)
	}
}

func TestPipelineContext_LLMCallTimeoutNilSafe(t *testing.T) {
	var pc *pipelineContext
	if got := pc.llmCallTimeout(); got != 120*time.Second {
		t.Errorf("nil pc should return 120s, got %s", got)
	}
}

func TestPipelineContext_LLMCallTimeoutNegativeFallsBackToBaseline(t *testing.T) {
	pc := &pipelineContext{settings: DreamPipelineSettings{LLMCallTimeoutSec: -42}}
	if got := pc.llmCallTimeout(); got != 120*time.Second {
		t.Errorf("negative should fall back to 120s, got %s", got)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// NightlyRunner.llmCallTimeout helper
// ──────────────────────────────────────────────────────────────────────────

func TestNightlyRunner_LLMCallTimeoutReadsBank(t *testing.T) {
	bank := newTestBank(t)
	s := DefaultDreamPipelineSettings()
	s.LLMCallTimeoutSec = 480
	bank.SaveDreamPipelineSettings(s)

	n := &NightlyRunner{bank: bank}
	if got := n.llmCallTimeout(); got != 480*time.Second {
		t.Errorf("want 480s, got %s", got)
	}
}

func TestNightlyRunner_LLMCallTimeoutNilBankFalls(t *testing.T) {
	n := &NightlyRunner{}
	if got := n.llmCallTimeout(); got != 120*time.Second {
		t.Errorf("nil-bank fallback: want 120s, got %s", got)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// signalProviderUse — nil-safe
// ──────────────────────────────────────────────────────────────────────────

func TestSignalProviderUse_NilLifecycleNoCrash(t *testing.T) {
	pc := &pipelineContext{} // lifecycle is nil
	// Must not panic.
	pc.signalProviderUse("nightly_run")
}

func TestSignalProviderUse_BumpsOnDemandServices(t *testing.T) {
	bank := newTestBank(t)
	lm := NewLifecycleManager(bank, nil, nil)
	// Default policy for ollama-tier2 is on_demand_lazy (per wave 8c).
	// SignalProviderUse should bump its last_used_at.
	beforeRow, _ := bank.GetServiceLifecycle("ollama-tier2")
	beforeUsedAt := beforeRow.LastUsedAt
	lm.SignalProviderUse("nightly_run")
	afterRow, _ := bank.GetServiceLifecycle("ollama-tier2")
	if afterRow.LastUsedAt == beforeUsedAt {
		t.Errorf("ollama-tier2 last_used_at should have advanced; got %q both times", afterRow.LastUsedAt)
	}
	if afterRow.LastWorkflow != "nightly_run" {
		t.Errorf("workflow should be recorded; got %q", afterRow.LastWorkflow)
	}
}

func TestSignalProviderUse_DoesNotTouchAlwaysOnServices(t *testing.T) {
	bank := newTestBank(t)
	lm := NewLifecycleManager(bank, nil, nil)
	// ollama-tier1 defaults to always_on. SignalProviderUse should skip it
	// — we don't want a phantom row creation for a service that doesn't
	// need lifecycle management.
	lm.SignalProviderUse("nightly_run")
	// GetServiceLifecycle returns a stopped/zero row for never-touched
	// services. If we touched it, LastUsedAt would be non-empty.
	row, _ := bank.GetServiceLifecycle("ollama-tier1")
	if row.LastUsedAt != "" {
		t.Errorf("always_on service should not get a row from SignalProviderUse; LastUsedAt=%q", row.LastUsedAt)
	}
}
