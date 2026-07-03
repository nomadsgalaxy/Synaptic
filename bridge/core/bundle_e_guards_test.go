// bundle_e_guards_test.go — guards for v2.6 Bundle E (custom-as-local).
//
//   - isLocalURL classifies host strings correctly: accepts loopback,
//     RFC1918, link-local, .local / .lan / .internal; rejects public DNS
//     and public IPs.
//   - Validate(kind=custom) honours the embed-tier rule:
//       cloud BaseURL  → rejected at TierEmbedding
//       local BaseURL  → accepted at TierEmbedding
//   - BuildProvider(kind=custom, local BaseURL) produces RemoteProvider
//     with Local=true, Timeout set per providerTimeout default.
//   - BuildProvider(kind=custom, cloud BaseURL) produces RemoteProvider
//     with Local=false (default treatment preserved).
//   - normalizeProviderKindEnv accepts the submitter's `openai_local`
//     synonym + variants (lm_studio, llamacpp, vllm).
//   - buildProviderFromEnvVars wires env-var configs end-to-end and
//     refuses an obviously-broken one (e.g. missing model).
package main

import (
	"testing"
)

// ── isLocalURL classification ───────────────────────────────────────────

func TestIsLocalURL_Accepts(t *testing.T) {
	cases := []string{
		"http://localhost:1234/v1",
		"http://127.0.0.1:11434",
		"http://[::1]:8000/v1",
		"http://192.168.1.50:1234/v1",
		"http://10.0.0.5/v1",
		"http://172.16.5.5:8080",
		"http://my-server.local/v1",
		"http://printer.lan:9000",
		"https://llm.corp.internal/v1",
		"http://169.254.0.5", // link-local
	}
	for _, u := range cases {
		t.Run(u, func(t *testing.T) {
			if !isLocalURL(u) {
				t.Errorf("isLocalURL(%q) = false, want true", u)
			}
		})
	}
}

func TestIsLocalURL_Rejects(t *testing.T) {
	cases := []string{
		"https://api.openai.com/v1",
		"https://api.anthropic.com",
		"http://example.com",
		"http://8.8.8.8/v1",
		"https://huggingface.co",
		"http://my-server.com", // public DNS
		"",
		"not-a-url",
	}
	for _, u := range cases {
		t.Run(u, func(t *testing.T) {
			if isLocalURL(u) {
				t.Errorf("isLocalURL(%q) = true, want false", u)
			}
		})
	}
}

// ── Validate respects local-vs-cloud for embed tier ─────────────────────

func TestValidate_CustomCloudRejectedAtTierEmbedding(t *testing.T) {
	cfg := ProviderConfig{
		Kind:    ProviderCustom,
		BaseURL: "https://api.openai.com/v1",
		Model:   "text-embedding-3-small",
	}
	if err := cfg.Validate(TierEmbedding); err == nil {
		t.Errorf("expected reject for cloud custom at TierEmbedding")
	}
}

func TestValidate_CustomLocalAcceptedAtTierEmbedding(t *testing.T) {
	cfg := ProviderConfig{
		Kind:    ProviderCustom,
		BaseURL: "http://192.168.1.50:1236/v1",
		Model:   "nomic-embed-text-v1.5",
	}
	if err := cfg.Validate(TierEmbedding); err != nil {
		t.Errorf("expected accept for local custom at TierEmbedding, got %v", err)
	}
}

func TestValidate_CustomCloudAcceptedAtTier1(t *testing.T) {
	cfg := ProviderConfig{
		Kind:    ProviderCustom,
		BaseURL: "https://api.openai.com/v1",
		Model:   "gpt-4o-mini",
	}
	if err := cfg.Validate(Tier1); err != nil {
		t.Errorf("cloud custom should still be valid at Tier 1, got %v", err)
	}
}

// ── BuildProvider auto-classifies local ─────────────────────────────────

func TestBuildProvider_CustomLocalSetsLocalTrue(t *testing.T) {
	cfg := ProviderConfig{
		Kind:    ProviderCustom,
		BaseURL: "http://192.168.1.50:1234/v1",
		Model:   "qwen3.5-4b-heretic-v2-i1",
	}
	p := BuildProvider(cfg)
	if p == nil {
		t.Fatalf("expected non-nil provider")
	}
	if !p.IsLocal() {
		t.Errorf("expected IsLocal=true for local BaseURL")
	}
	if got := p.Name(); got != "local:qwen3.5-4b-heretic-v2-i1" {
		t.Errorf("Name() = %q, want local:qwen3.5-4b-heretic-v2-i1", got)
	}
}

func TestBuildProvider_CustomCloudKeepsLocalFalse(t *testing.T) {
	cfg := ProviderConfig{
		Kind:    ProviderCustom,
		BaseURL: "https://api.openai.com/v1",
		Model:   "gpt-4o-mini",
	}
	p := BuildProvider(cfg)
	if p == nil {
		t.Fatalf("expected non-nil provider")
	}
	if p.IsLocal() {
		t.Errorf("cloud custom should NOT classify as local")
	}
	if got := p.Name(); got != "custom:gpt-4o-mini" {
		t.Errorf("Name() = %q, want custom:gpt-4o-mini", got)
	}
}

// ── normalizeProviderKindEnv accepts submitter aliases ──────────────────

func TestNormalizeProviderKindEnv_Aliases(t *testing.T) {
	cases := map[string]ProviderKind{
		"openai_local": ProviderCustom,
		"OPENAI_LOCAL": ProviderCustom,
		"lm_studio":    ProviderCustom,
		"LMSTUDIO":     ProviderCustom,
		"llama_cpp":    ProviderCustom,
		"llamacpp":     ProviderCustom,
		"vllm":         ProviderCustom,
		"VLLM":         ProviderCustom,
		"custom":       ProviderCustom,
		"ollama":       ProviderOllama,
		"openai":       ProviderOpenAI,
		"":             "",
	}
	for raw, want := range cases {
		t.Run(raw, func(t *testing.T) {
			if got := normalizeProviderKindEnv(raw); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

// ── buildProviderFromEnvVars end-to-end ─────────────────────────────────

func TestBuildProviderFromEnvVars_LocalCustomSucceeds(t *testing.T) {
	t.Setenv("SD_TIER1_KIND", "openai_local") // alias
	t.Setenv("SD_TIER1_URL", "http://192.168.1.50:1234/v1")
	t.Setenv("SD_TIER1_MODEL", "qwen3.5-4b-heretic-v2-i1")
	t.Setenv("SD_TIER1_TIMEOUT_S", "120")
	p := buildProviderFromEnvVars("1")
	if p == nil {
		t.Fatalf("expected non-nil provider from env")
	}
	if !p.IsLocal() {
		t.Errorf("expected IsLocal=true")
	}
}

func TestBuildProviderFromEnvVars_RejectsCloudAtEmbedding(t *testing.T) {
	// Cloud BaseURL at the embed tier should be rejected by Validate
	// (which runs inside buildProviderFromEnvVars).
	t.Setenv("SD_EMBED_KIND", "custom")
	t.Setenv("SD_EMBED_URL", "https://api.openai.com/v1")
	t.Setenv("SD_EMBED_MODEL", "text-embedding-3-small")
	p := buildProviderFromEnvVars("EMBED")
	if p != nil {
		t.Errorf("expected nil (rejected at validator), got %v", p)
	}
}

func TestBuildProviderFromEnvVars_EmptyKindReturnsNil(t *testing.T) {
	// Most users don't set SD_TIER{N}_KIND at all; should return nil
	// and let the legacy Ollama fallback take over.
	t.Setenv("SD_TIER2_KIND", "")
	p := buildProviderFromEnvVars("2")
	if p != nil {
		t.Errorf("empty KIND should return nil, got %v", p)
	}
}

func TestBuildProviderFromEnvVars_Tier3LocalSucceeds(t *testing.T) {
	// T19 — Tier 3 env-var path. Pre-Bundle-E this returned nil;
	// after Bundle E the path builds a Tier 3 provider from env.
	t.Setenv("SD_TIER3_KIND", "openai_local")
	t.Setenv("SD_TIER3_URL", "http://192.168.1.50:8001/v1")
	t.Setenv("SD_TIER3_MODEL", "qwen3.5-35b-a3b-heretic")
	t.Setenv("SD_TIER3_TIMEOUT_S", "600")
	p := buildProviderFromEnvVars("3")
	if p == nil {
		t.Fatalf("expected non-nil Tier 3 provider from env")
	}
	if !p.IsLocal() {
		t.Errorf("expected Tier 3 local")
	}
}

// ── Timeout plumbing ────────────────────────────────────────────────────

func TestProviderTimeout_Defaults(t *testing.T) {
	cases := []struct {
		name string
		cfg  ProviderConfig
		want int // seconds
	}{
		{"explicit override wins", ProviderConfig{Kind: ProviderCustom, TimeoutSeconds: 42, BaseURL: "https://api.openai.com/v1"}, 42},
		{"airllm default 600", ProviderConfig{Kind: ProviderAirLLM}, 600},
		{"custom local default 120", ProviderConfig{Kind: ProviderCustom, BaseURL: "http://192.168.1.50/v1"}, 120},
		{"custom cloud default 60", ProviderConfig{Kind: ProviderCustom, BaseURL: "https://api.openai.com/v1"}, 60},
		{"ollama default 60", ProviderConfig{Kind: ProviderOllama}, 60},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := int(providerTimeout(tc.cfg).Seconds())
			if got != tc.want {
				t.Errorf("got %d s, want %d s", got, tc.want)
			}
		})
	}
}
