// airllm_provider_test.go — exercises AirLLMProvider against an
// httptest sidecar so we don't need a real Python AirLLM install
// running during CI.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAirLLM_Embed_AlwaysUnsupported(t *testing.T) {
	p := NewAirLLMProvider("http://example.invalid", "any/model", "", "")
	if _, err := p.Embed(context.Background(), "anything"); !errors.Is(err, ErrAirLLMEmbedUnsupported) {
		t.Errorf("expected ErrAirLLMEmbedUnsupported, got %v", err)
	}
}

func TestAirLLM_Ping_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok","model":"test/model","loaded":true,"compression":"4bit"}`))
	}))
	defer srv.Close()

	p := NewAirLLMProvider(srv.URL, "test/model", "", "")
	if err := p.Ping(context.Background()); err != nil {
		t.Errorf("Ping should succeed against healthy sidecar, got: %v", err)
	}
}

func TestAirLLM_Ping_NotLoadedYet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok","model":"test/model","loaded":false,"compression":"4bit"}`))
	}))
	defer srv.Close()
	p := NewAirLLMProvider(srv.URL, "test/model", "", "")
	err := p.Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "model is not loaded") {
		t.Errorf("expected loaded:false error, got: %v", err)
	}
}

func TestAirLLM_Ping_BadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"loading"}`))
	}))
	defer srv.Close()
	p := NewAirLLMProvider(srv.URL, "test/model", "", "")
	if err := p.Ping(context.Background()); err == nil {
		t.Errorf("expected error on 503, got nil")
	}
}

func TestAirLLM_Chat_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/generate" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		// Verify the request shape — the provider should send prompt + max_tokens + temperature.
		if body["prompt"] == nil || body["max_tokens"] == nil {
			http.Error(w, "bad request shape", http.StatusBadRequest)
			return
		}
		w.Write([]byte(`{
		  "text": "hello back",
		  "tokens_in": 12,
		  "tokens_out": 4,
		  "elapsed_ms": 50,
		  "model": "test/model",
		  "compression": "4bit"
		}`))
	}))
	defer srv.Close()

	p := NewAirLLMProvider(srv.URL, "test/model", "", "")
	out, err := p.Chat(context.Background(),
		[]Message{{Role: "user", Content: "hello"}}, 64)
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if out != "hello back" {
		t.Errorf("expected 'hello back', got %q", out)
	}
}

func TestAirLLM_Chat_PropagatesSidecarError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"OOM at layer 23"}`))
	}))
	defer srv.Close()

	p := NewAirLLMProvider(srv.URL, "test/model", "", "")
	_, err := p.Chat(context.Background(),
		[]Message{{Role: "user", Content: "hi"}}, 64)
	if err == nil || !strings.Contains(err.Error(), "OOM at layer 23") {
		t.Errorf("expected upstream error surfaced, got: %v", err)
	}
}

func TestAirLLM_Chat_RespectsContextDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Slower than the test's deadline.
		time.Sleep(200 * time.Millisecond)
		w.Write([]byte(`{"text":"too late"}`))
	}))
	defer srv.Close()

	p := NewAirLLMProvider(srv.URL, "test/model", "", "")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := p.Chat(ctx, []Message{{Role: "user", Content: "x"}}, 64)
	if err == nil {
		t.Errorf("expected deadline-exceeded error")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Validate: AirLLM is Tier 2 + Tier 3 only.
// ──────────────────────────────────────────────────────────────────────────

func TestAirLLM_ValidateRejectsTier1AndEmbedding(t *testing.T) {
	cfg := ProviderConfig{Kind: ProviderAirLLM, BaseURL: "http://x", Model: "m"}
	for _, tier := range []TierKey{TierEmbedding, Tier1} {
		if err := cfg.Validate(tier); err == nil {
			t.Errorf("expected Validate to reject AirLLM at tier=%s", tier)
		}
	}
}

func TestAirLLM_ValidateAcceptsTier2AndTier3(t *testing.T) {
	cfg := ProviderConfig{Kind: ProviderAirLLM, BaseURL: "http://127.0.0.1:9912", Model: "meta/Llama-3.1-70B"}
	for _, tier := range []TierKey{Tier2, Tier3} {
		if err := cfg.Validate(tier); err != nil {
			t.Errorf("AirLLM config should validate at tier=%s, got: %v", tier, err)
		}
	}
}

func TestAirLLM_ValidateRequiresBaseURLAndModel(t *testing.T) {
	cases := []ProviderConfig{
		{Kind: ProviderAirLLM, Model: "m"},                     // missing base_url
		{Kind: ProviderAirLLM, BaseURL: "http://x"},            // missing model
		{Kind: ProviderAirLLM},                                 // both
	}
	for _, c := range cases {
		if err := c.Validate(Tier3); err == nil {
			t.Errorf("expected Validate to reject incomplete config at Tier3: %+v", c)
		}
		if err := c.Validate(Tier2); err == nil {
			t.Errorf("expected Validate to reject incomplete config at Tier2: %+v", c)
		}
	}
}

func TestAirLLM_BuildProviderConstructs(t *testing.T) {
	cfg := ProviderConfig{
		Kind: ProviderAirLLM, BaseURL: "http://x:9912", Model: "test/model",
	}
	prov := BuildProvider(cfg)
	if prov == nil {
		t.Fatalf("BuildProvider returned nil for AirLLM config")
	}
	if !prov.IsLocal() {
		t.Errorf("AirLLM provider should be IsLocal()=true")
	}
	if !strings.Contains(prov.Name(), "airllm") {
		t.Errorf("Name should contain airllm prefix, got %q", prov.Name())
	}
}

func TestAirLLM_KindLabelsHasEntry(t *testing.T) {
	if _, ok := KindLabels[ProviderAirLLM]; !ok {
		t.Errorf("KindLabels missing ProviderAirLLM entry")
	}
}

func TestProviderIsExternal_AirLLMIsLocal(t *testing.T) {
	if providerIsExternal("airllm") {
		t.Errorf("airllm should be local (cost_usd: 0)")
	}
}
