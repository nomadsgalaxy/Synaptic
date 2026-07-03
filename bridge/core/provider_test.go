package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeProvider implements LLMProvider for router tests.
type fakeProvider struct {
	name  string
	local bool
}

func (f *fakeProvider) Name() string                                              { return f.name }
func (f *fakeProvider) IsLocal() bool                                             { return f.local }
func (f *fakeProvider) Embed(_ context.Context, _ string) ([]float32, error)      { return nil, nil }
func (f *fakeProvider) Chat(_ context.Context, _ []Message, _ int) (string, error) { return "", nil }

func TestModelRouterTierSelection(t *testing.T) {
	t1 := &fakeProvider{name: "t1", local: true} // Tier 1 must be local for ForEmbedding
	t2 := &fakeProvider{name: "t2", local: true}
	t3 := &fakeProvider{name: "t3"}

	r := NewModelRouter(t1, t2, t3, []string{"Gravity"})
	if r.ForEmbedding().Name() != "t1" {
		t.Errorf("ForEmbedding must always be Tier 1")
	}
	if r.ForRealtime().Name() != "t1" {
		t.Errorf("ForRealtime must be Tier 1")
	}
	if r.ForNightly().Name() != "t2" {
		t.Errorf("ForNightly should be Tier 2 when configured")
	}
	if r.ForOracle("OAuth").Name() != "t3" {
		t.Errorf("ForOracle should return Tier 3 for non-blocked term")
	}
	if r.ForOracle("Gravity") != nil {
		t.Errorf("ForOracle must return nil for LocalConcepts term")
	}

	// Degradation: nil Tier 2 falls back to Tier 1.
	r2 := NewModelRouter(t1, nil, nil, nil)
	if r2.ForNightly().Name() != "t1" {
		t.Errorf("ForNightly should degrade to Tier 1 when Tier 2 is nil")
	}
	if r2.ForOracle("Anything") != nil {
		t.Errorf("ForOracle must return nil when Tier 3 is unconfigured")
	}
}

func TestOllamaProviderEmbedNormalizes(t *testing.T) {
	// Mock Ollama embeddings endpoint returning a non-unit vector.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embeddings" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"embedding": []float64{3.0, 4.0}, // length 5 → after normalize: (0.6, 0.8)
		})
	}))
	defer srv.Close()

	p := NewOllamaProvider(srv.URL, "llama3.2:3b")
	vec, err := p.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != 2 {
		t.Fatalf("expected 2 dims, got %d", len(vec))
	}
	// Verify unit length (within float tolerance).
	var sum float64
	for _, v := range vec {
		sum += float64(v) * float64(v)
	}
	if sum < 0.999 || sum > 1.001 {
		t.Errorf("embedding not normalised: |v|² = %f", sum)
	}
}

func TestOllamaProviderEmbedHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "model not found", http.StatusNotFound)
	}))
	defer srv.Close()

	p := NewOllamaProvider(srv.URL, "missing")
	if _, err := p.Embed(context.Background(), "x"); err == nil {
		t.Errorf("expected error from non-2xx response")
	}
}
