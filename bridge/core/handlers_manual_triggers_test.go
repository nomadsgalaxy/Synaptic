package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────
// Lexicon rebuild
// ──────────────────────────────────────────────────────────────────────────

func TestLexiconRebuild_QueuesAndCompletes(t *testing.T) {
	bank := newTestBank(t)
	ring := NewRingBuffer(50)
	hub := NewHub(ring)
	server := &Server{bank: bank, hub: hub}

	// Seed memories with overlapping tags.
	_, _ = bank.SaveMemory(MemoryRecord{Text: "a", Tags: []string{"x", "y"}})
	_, _ = bank.SaveMemory(MemoryRecord{Text: "b", Tags: []string{"y", "z"}})
	_, _ = bank.SaveMemory(MemoryRecord{Text: "c", Tags: []string{"x", "z"}})

	req := httptest.NewRequest(http.MethodPost, "/lexicon/rebuild", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.HasPrefix(resp["run_id"].(string), "lexicon-") {
		t.Errorf("expected run_id prefix lexicon-, got %v", resp["run_id"])
	}
	if resp["status"] != "queued" {
		t.Errorf("expected status=queued, got %v", resp["status"])
	}

	// Wait for completion (rebuild is fast on 3 memories).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if running, _ := server.lexiconRebuildID.Load().(string); running == "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Verify pairs were written.
	pairs, _ := bank.ListLexicon("", 0)
	if len(pairs) < 3 {
		t.Errorf("expected >=3 lexicon pairs (x,y / y,z / x,z), got %d", len(pairs))
	}
}

func TestLexiconRebuild_409WhenInProgress(t *testing.T) {
	bank := newTestBank(t)
	server := &Server{bank: bank}

	// Hold the rebuild mutex artificially so the 2nd POST collides.
	server.lexiconRebuildMu.Lock()
	server.lexiconRebuildID.Store("lexicon-stuck")
	defer func() {
		server.lexiconRebuildID.Store("")
		server.lexiconRebuildMu.Unlock()
	}()

	req := httptest.NewRequest(http.MethodPost, "/lexicon/rebuild", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "lexicon-stuck") {
		t.Errorf("409 body should include current run_id, got %s", rec.Body.String())
	}
}

// ──────────────────────────────────────────────────────────────────────────
// /nightly/run alias
// ──────────────────────────────────────────────────────────────────────────

func TestNightlyRun_AliasOfTrigger(t *testing.T) {
	server, _ := nightlyTestServer(t)

	body, _ := json.Marshal(map[string]interface{}{})
	req := httptest.NewRequest(http.MethodPost, "/nightly/run", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /nightly/run expected 200 (alias of /trigger), got %d: %s",
			rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.HasPrefix(resp["id"].(string), "nightly-") {
		t.Errorf("expected nightly- id, got %v", resp["id"])
	}
}

// ──────────────────────────────────────────────────────────────────────────
// GET /research/{id} rich detail
// ──────────────────────────────────────────────────────────────────────────

func TestResearch_DetailById(t *testing.T) {
	bank := newTestBank(t)
	server := &Server{bank: bank}
	// Seed a rich payload identical to what POST /research would write.
	rich := map[string]interface{}{
		"id":           "research-test1",
		"query":        "What is OAuth?",
		"content":      "OAuth is an authorization framework.",
		"sources":      []map[string]string{{"url": "https://oauth.net", "title": "OAuth"}},
		"model":        "openai:gpt-4o-mini",
		"tokens_in":    100,
		"tokens_out":   200,
		"tokens_total": 300,
	}
	payload, _ := json.Marshal(rich)
	if err := bank.SaveResearchCache(ResearchCacheEntry{
		Topic: "research-test1", Payload: string(payload),
		FetchedAt: nowUTC(), ExpiresAt: "2099-01-01T00:00:00Z",
		FetchedBy: "manual",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/research/research-test1", nil)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	// Rich fields must be surfaced at the top level.
	for _, want := range []string{"query", "content", "sources", "model", "tokens_in", "tokens_out", "tokens_total", "fetched_by", "provider"} {
		if _, ok := resp[want]; !ok {
			t.Errorf("response missing %q key, got: %v", want, resp)
		}
	}
	if resp["query"] != "What is OAuth?" {
		t.Errorf("query mismatch: %v", resp["query"])
	}
	if resp["fetched_by"] != "manual" {
		t.Errorf("expected fetched_by=manual, got %v", resp["fetched_by"])
	}
	if resp["provider"] != "openai" {
		t.Errorf("expected provider=openai (parsed from model), got %v", resp["provider"])
	}
}

func TestResearch_FetchedByManualVsAuto(t *testing.T) {
	bank := newTestBank(t)
	server := &Server{bank: bank}

	manualPayload, _ := json.Marshal(map[string]string{"query": "manual q"})
	autoPayload, _ := json.Marshal(map[string]string{"query": "auto q"})
	_ = bank.SaveResearchCache(ResearchCacheEntry{
		Topic: "manual-1", Payload: string(manualPayload),
		FetchedAt: nowUTC(), ExpiresAt: "2099-01-01T00:00:00Z", FetchedBy: "manual",
	})
	_ = bank.SaveResearchCache(ResearchCacheEntry{
		Topic: "auto-1", Payload: string(autoPayload),
		FetchedAt: nowUTC(), ExpiresAt: "2099-01-01T00:00:00Z", FetchedBy: "auto",
	})

	// List both
	req := httptest.NewRequest(http.MethodGet, "/research", nil)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	var listResp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &listResp)
	entries, _ := listResp["entries"].([]interface{})
	gotByTopic := map[string]string{}
	for _, e := range entries {
		em := e.(map[string]interface{})
		gotByTopic[em["topic"].(string)] = em["fetched_by"].(string)
	}
	if gotByTopic["manual-1"] != "manual" {
		t.Errorf("list: manual-1 fetched_by mismatch: %v", gotByTopic)
	}
	if gotByTopic["auto-1"] != "auto" {
		t.Errorf("list: auto-1 fetched_by mismatch: %v", gotByTopic)
	}

	// Detail
	req = httptest.NewRequest(http.MethodGet, "/research/auto-1", nil)
	rec = httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	var detail map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &detail)
	if detail["fetched_by"] != "auto" {
		t.Errorf("detail fetched_by mismatch: %v", detail["fetched_by"])
	}
}

func TestResearch_404OnUnknownId(t *testing.T) {
	bank := newTestBank(t)
	server := &Server{bank: bank}

	req := httptest.NewRequest(http.MethodGet, "/research/does-not-exist", nil)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

// Ensure that POST /research now sets fetched_by=manual on the row.
func TestResearch_PostMarksManual(t *testing.T) {
	oracle := &mockOracle{name: "openai:gpt-4o-mini",
		respond: func(_ string) (string, error) { return "answer", nil }}
	bank := newTestBank(t)
	tier1 := &fakeProvider{name: "tier1-local", local: true}
	router := NewModelRouter(tier1, nil, oracle, nil)
	server := &Server{bank: bank, router: router}

	body, _ := json.Marshal(map[string]string{"query": "trigger me"})
	req := httptest.NewRequest(http.MethodPost, "/research", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// Verify the cached row has fetched_by=manual.
	rows, _ := bank.ListResearchCache(0, true)
	if len(rows) == 0 {
		t.Fatalf("expected at least 1 cached row after POST /research")
	}
	if rows[0].FetchedBy != "manual" {
		t.Errorf("expected fetched_by=manual, got %q", rows[0].FetchedBy)
	}
}

// Ensure context propagation doesn't break anything.
var _ = context.Background
