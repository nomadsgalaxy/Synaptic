package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fireRequest is a tiny helper: build httptest req + recorder, route via the
// server's mux, return the recorder.
func fireRequest(t *testing.T, server *Server, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	return rec
}

func TestMemoryEditEndpoints(t *testing.T) {
	bank := newTestBank(t)
	server := &Server{bank: bank}

	rec, _ := bank.SaveMemory(MemoryRecord{Text: "OAuth flow", Tags: []string{"auth"}})

	// PATCH
	enriched := "OAuth flow (refined)"
	resp := fireRequest(t, server, http.MethodPatch, "/bank/memories/"+rec.ID,
		MemoryUpdate{EnrichedText: &enriched})
	if resp.Code != http.StatusOK {
		t.Fatalf("PATCH expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	var got MemoryRecord
	_ = json.Unmarshal(resp.Body.Bytes(), &got)
	if got.EnrichedText != enriched {
		t.Errorf("enriched_text not persisted: %q", got.EnrichedText)
	}

	// Lifecycle flag
	resp = fireRequest(t, server, http.MethodPost,
		"/bank/memories/"+rec.ID+"/lifecycle",
		map[string]interface{}{"flag": "light_encoded", "value": true})
	if resp.Code != http.StatusOK {
		t.Errorf("lifecycle expected 200, got %d: %s", resp.Code, resp.Body.String())
	}

	// Soft-delete
	resp = fireRequest(t, server, http.MethodDelete, "/bank/memories/"+rec.ID, nil)
	if resp.Code != http.StatusNoContent {
		t.Errorf("soft delete expected 204, got %d", resp.Code)
	}
	post, _ := bank.GetMemory(rec.ID)
	if post.DeletedAt == "" {
		t.Errorf("deleted_at should be set after soft delete")
	}

	// Restore
	resp = fireRequest(t, server, http.MethodPost, "/bank/memories/"+rec.ID+"/restore", nil)
	if resp.Code != http.StatusOK {
		t.Errorf("restore expected 200, got %d", resp.Code)
	}
	post, _ = bank.GetMemory(rec.ID)
	if post.DeletedAt != "" {
		t.Errorf("deleted_at should clear after restore")
	}

	// Audit log should record at least the patch + lifecycle + delete + restore
	entries, _ := bank.ListAuditLog(AuditFilter{EntityType: "trace", EntityID: rec.ID})
	if len(entries) < 4 {
		t.Errorf("expected ≥4 audit entries for the trace, got %d", len(entries))
	}
}

func TestMemoryMapEndpoints(t *testing.T) {
	bank := newTestBank(t)
	server := &Server{bank: bank}

	// Create
	resp := fireRequest(t, server, http.MethodPost, "/maps", MemoryMap{
		Name: "Gravity", Type: "project", AnchorTags: []string{"gravity"},
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("POST /maps expected 201, got %d: %s", resp.Code, resp.Body.String())
	}
	var created MemoryMap
	_ = json.Unmarshal(resp.Body.Bytes(), &created)
	if created.ID == "" {
		t.Fatal("created.ID empty")
	}

	// List
	resp = fireRequest(t, server, http.MethodGet, "/maps?type=project", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET /maps expected 200, got %d", resp.Code)
	}

	// Get
	resp = fireRequest(t, server, http.MethodGet, "/maps/"+created.ID, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET /maps/{id} expected 200, got %d", resp.Code)
	}

	// PATCH schema_text
	st := "Gravity is a CAD prototype."
	resp = fireRequest(t, server, http.MethodPatch, "/maps/"+created.ID,
		MemoryMapUpdate{SchemaText: &st})
	if resp.Code != http.StatusOK {
		t.Fatalf("PATCH /maps/{id} expected 200, got %d: %s", resp.Code, resp.Body.String())
	}

	// Add association
	other, _ := bank.SaveMemoryMap(MemoryMap{Name: "OpenCascade", Type: "technology"})
	resp = fireRequest(t, server, http.MethodPost,
		"/maps/"+created.ID+"/associations",
		MapAssociation{ToMapID: other.ID, Association: "uses_technology", Weight: 0.8})
	if resp.Code != http.StatusCreated {
		t.Fatalf("POST associations expected 201, got %d: %s", resp.Code, resp.Body.String())
	}

	// List associations
	resp = fireRequest(t, server, http.MethodGet,
		"/maps/"+created.ID+"/associations?direction=from", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET associations expected 200, got %d", resp.Code)
	}
	if !strings.Contains(resp.Body.String(), "uses_technology") {
		t.Errorf("expected uses_technology in body: %s", resp.Body.String())
	}

	// Link a trace
	tr, _ := bank.SaveMemory(MemoryRecord{Text: "linked"})
	resp = fireRequest(t, server, http.MethodPost,
		"/maps/"+created.ID+"/traces",
		map[string]string{"trace_id": tr.ID})
	if resp.Code != http.StatusCreated {
		t.Errorf("POST traces expected 201, got %d", resp.Code)
	}

	// Unlink
	resp = fireRequest(t, server, http.MethodDelete,
		"/maps/"+created.ID+"/traces/"+tr.ID, nil)
	if resp.Code != http.StatusNoContent {
		t.Errorf("DELETE trace expected 204, got %d", resp.Code)
	}

	// Delete map (cascades)
	resp = fireRequest(t, server, http.MethodDelete, "/maps/"+created.ID, nil)
	if resp.Code != http.StatusNoContent {
		t.Errorf("DELETE map expected 204, got %d", resp.Code)
	}
}

func TestLexiconAuditResearchBudgetEndpoints(t *testing.T) {
	bank := newTestBank(t)
	server := &Server{bank: bank}

	// Lexicon upsert + list
	resp := fireRequest(t, server, http.MethodPost, "/lexicon",
		LexiconRow{TagA: "oauth", TagB: "auth", Cooccurrence: 5, Weight: 0.4})
	if resp.Code != http.StatusOK {
		t.Fatalf("POST /lexicon expected 200, got %d", resp.Code)
	}
	resp = fireRequest(t, server, http.MethodGet, "/lexicon?tag=auth", nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "oauth") {
		t.Errorf("GET /lexicon: %d %s", resp.Code, resp.Body.String())
	}

	// Audit list (will contain the lexicon upsert)
	resp = fireRequest(t, server, http.MethodGet, "/audit", nil)
	if resp.Code != http.StatusOK {
		t.Errorf("GET /audit expected 200, got %d", resp.Code)
	}

	// Research GET + DELETE. (POST /research is now the user-triggered Oracle
	// path covered fully in handlers_research_test.go; we seed a cache row
	// directly via the bank to test the read endpoints.)
	if err := bank.SaveResearchCache(ResearchCacheEntry{
		Topic: "Foo", Payload: `{"x":1}`, FetchedAt: nowUTC(),
		ExpiresAt: "2099-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("SaveResearchCache seed: %v", err)
	}
	resp = fireRequest(t, server, http.MethodGet, "/research/Foo", nil)
	if resp.Code != http.StatusOK {
		t.Errorf("GET /research/Foo expected 200, got %d", resp.Code)
	}
	resp = fireRequest(t, server, http.MethodDelete, "/research/Foo", nil)
	if resp.Code != http.StatusNoContent {
		t.Errorf("DELETE /research/Foo expected 204, got %d", resp.Code)
	}

	// Budget POST + GET
	resp = fireRequest(t, server, http.MethodPost, "/budget",
		map[string]interface{}{"date": "2026-05-08", "tokens": 1000})
	if resp.Code != http.StatusNoContent {
		t.Errorf("POST /budget expected 204, got %d", resp.Code)
	}
	resp = fireRequest(t, server, http.MethodGet, "/budget?date=2026-05-08", nil)
	if resp.Code != http.StatusOK {
		t.Errorf("GET /budget expected 200, got %d", resp.Code)
	}
	if !strings.Contains(resp.Body.String(), `"tokens_used":1000`) {
		t.Errorf("budget body should include tokens_used:1000, got %s", resp.Body.String())
	}
}
