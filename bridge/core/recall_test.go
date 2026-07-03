package main

import (
	"bytes"
	"encoding/json"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// makeUnitVec returns a normalised float32 slice of length dim, seeded.
func makeUnitVec(dim int, seed int64) []float32 {
	rng := rand.New(rand.NewSource(seed))
	v := make([]float32, dim)
	var sum float64
	for i := range v {
		x := rng.Float64()*2 - 1
		v[i] = float32(x)
		sum += x * x
	}
	if sum == 0 {
		return v
	}
	inv := float32(1.0 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
	return v
}

// blendUnit returns normalize(a*alpha + b*(1-alpha)).
func blendUnit(a, b []float32, alpha float32) []float32 {
	out := make([]float32, len(a))
	for i := range out {
		out[i] = a[i]*alpha + b[i]*(1-alpha)
	}
	var sum float64
	for _, x := range out {
		sum += float64(x) * float64(x)
	}
	inv := float32(1.0 / math.Sqrt(sum))
	for i := range out {
		out[i] *= inv
	}
	return out
}

func TestPostRecall_RankingAndCacheHit(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	bank, err := NewBank(dbPath)
	if err != nil {
		t.Fatalf("NewBank: %v", err)
	}
	defer bank.Close()

	mem1, err := bank.SaveMemory(MemoryRecord{Text: "OAuth token refresh flow", Tags: []string{"auth"}})
	if err != nil {
		t.Fatalf("SaveMemory mem1: %v", err)
	}
	mem2, err := bank.SaveMemory(MemoryRecord{Text: "Bubble sort algorithm", Tags: []string{"algorithms"}})
	if err != nil {
		t.Fatalf("SaveMemory mem2: %v", err)
	}
	mem3, err := bank.SaveMemory(MemoryRecord{Text: "OAuth PKCE code challenge", Tags: []string{"auth", "pkce"}})
	if err != nil {
		t.Fatalf("SaveMemory mem3: %v", err)
	}

	const dim = 128
	const model = "llama3.2:3b"

	// Construct vectors so mem3 > mem1 >> mem2 by similarity to queryVec.
	queryVec := makeUnitVec(dim, 42)
	noise1 := makeUnitVec(dim, 101)
	noise2 := makeUnitVec(dim, 202)
	noise3 := makeUnitVec(dim, 303)

	mem1Vec := blendUnit(queryVec, noise1, 0.6) // moderate alignment
	mem2Vec := noise2                           // near-orthogonal (no query component)
	mem3Vec := blendUnit(queryVec, noise3, 0.85) // strongest alignment

	// Persist memory embeddings under the keys that AllEmbeddingsForMemories will look up.
	mems := []struct {
		rec MemoryRecord
		vec []float32
	}{
		{mem1, mem1Vec},
		{mem2, mem2Vec},
		{mem3, mem3Vec},
	}
	for _, m := range mems {
		tagsAny := make([]interface{}, len(m.rec.Tags))
		for i, t := range m.rec.Tags {
			tagsAny[i] = t
		}
		embedText := synapseEmbedText(map[string]interface{}{
			"text": m.rec.Text,
			"tags": tagsAny,
		})
		if err := bank.SaveEmbedding(synapseTextHash(embedText), m.vec, model); err != nil {
			t.Fatalf("SaveEmbedding: %v", err)
		}
	}

	// Pre-cache the query embedding so postRecall does NOT need to call Ollama.
	queryText := "OAuth token"
	if err := bank.SaveEmbedding(synapseTextHash(queryText), queryVec, model); err != nil {
		t.Fatalf("SaveEmbedding query: %v", err)
	}

	// Inject a router with a fake local provider whose Name() yields the
	// bare model label that AllEmbeddingsForMemories will filter by.
	// providerModelLabel strips the "ollama:" prefix → "llama3.2:3b".
	// Embed() is a no-op (returns nil, nil); the test pre-caches the
	// query embedding so this path is never hit.
	t.Setenv("SD_OLLAMA_URL", "http://127.0.0.1:1") // unreachable; defence-in-depth
	t.Setenv("SD_SYNAPSE_MODEL", model)

	// v2.7 Bundle J shipped hybrid retrieval ON by default. This test
	// asserts pure cosine ordering with controlled embeddings, so disable
	// the BM25 fusion path explicitly (a dedicated test for hybrid
	// ranking lives in bundle_j_guards_test.go).
	bank.SetSetting(HybridRecallEnabledKey, "0")

	stubTier1 := &fakeProvider{name: "ollama:" + model, local: true}
	router := NewModelRouter(stubTier1, nil, nil, nil)
	server := &Server{bank: bank, router: router}

	body, _ := json.Marshal(recallRequest{Query: queryText, Limit: 3})
	req := httptest.NewRequest(http.MethodPost, "/recall", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	server.postRecall(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp recallResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Count != 3 {
		t.Fatalf("expected count=3, got %d", resp.Count)
	}

	// Scores must be non-increasing.
	for i := 1; i < len(resp.Results); i++ {
		if resp.Results[i].Score > resp.Results[i-1].Score {
			t.Errorf("results not sorted: [%d].score=%.4f > [%d].score=%.4f",
				i, resp.Results[i].Score, i-1, resp.Results[i-1].Score)
		}
	}

	// mem3 should rank first (strongest alignment to queryVec).
	if got := resp.Results[0].Memory.ID; got != mem3.ID {
		t.Errorf("expected top result %s (mem3), got %s (%s)",
			mem3.ID, got, resp.Results[0].Memory.Text)
	}
	// mem2 (bubble sort) should rank last (near-orthogonal).
	if got := resp.Results[resp.Count-1].Memory.ID; got != mem2.ID {
		t.Errorf("expected last result %s (mem2), got %s (%s)",
			mem2.ID, got, resp.Results[resp.Count-1].Memory.Text)
	}

	t.Logf("recall returned %d results; top=%s score=%.4f",
		resp.Count, resp.Results[0].Memory.Text, resp.Results[0].Score)
}

func TestPostRecall_MissingQuery(t *testing.T) {
	dir := t.TempDir()
	bank, _ := NewBank(filepath.Join(dir, "b.db"))
	defer bank.Close()
	server := &Server{bank: bank}

	body := []byte(`{"limit":5}`) // no query field
	req := httptest.NewRequest(http.MethodPost, "/recall", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	server.postRecall(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestPostRecall_BankDisabled(t *testing.T) {
	server := &Server{bank: nil}
	body, _ := json.Marshal(recallRequest{Query: "test"})
	req := httptest.NewRequest(http.MethodPost, "/recall", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	server.postRecall(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}
}
