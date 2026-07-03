package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mockOracle is a Tier 3 stand-in for the research handler tests. It
// records every prompt the handler hands it (so we can assert sensitive
// content never reaches it) and returns a configurable canned response.
type mockOracle struct {
	calls      atomic.Int32
	lastPrompt atomic.Value // string
	respond    func(prompt string) (string, error)
	name       string
	// local controls IsLocal()'s return — defaults to false (remote/paid).
	// Tests like TestBudget_ResearchRecordsExternal flip this to verify
	// local-as-Tier-3 records external=false in token_budget_lines.
	local bool
}

func (m *mockOracle) Name() string                                              { return m.name }
func (m *mockOracle) IsLocal() bool                                             { return m.local }
func (m *mockOracle) Embed(_ context.Context, _ string) ([]float32, error)      { return nil, nil }
func (m *mockOracle) Chat(_ context.Context, msgs []Message, _ int) (string, error) {
	m.calls.Add(1)
	prompt := ""
	if len(msgs) > 0 {
		prompt = msgs[len(msgs)-1].Content
	}
	m.lastPrompt.Store(prompt)
	if m.respond != nil {
		return m.respond(prompt)
	}
	return "MOCK ORACLE ANSWER", nil
}

// researchTestServer wires a Server with a fake Tier 3 wired into the router.
// localConcepts may be empty.
func researchTestServer(t *testing.T, oracle *mockOracle, localConcepts []string) *Server {
	t.Helper()
	bank := newTestBank(t)
	tier1 := &fakeProvider{name: "tier1-local", local: true}
	router := NewModelRouter(tier1, nil, oracle, localConcepts)
	return &Server{bank: bank, router: router}
}

// postResearch fires POST /research through the full mux (so we exercise
// the dispatcher route precedence) and returns code + parsed JSON map.
func postResearch(t *testing.T, server *Server, body interface{}) (int, map[string]interface{}, string) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/research", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	var out map[string]interface{}
	if rec.Body.Len() > 0 && rec.Code == http.StatusOK {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out, rec.Body.String()
}

// ──────────────────────────────────────────────────────────────────────────

func TestResearchPost_BasicSuccess(t *testing.T) {
	oracle := &mockOracle{name: "oracle:gpt-4o-mini",
		respond: func(_ string) (string, error) { return "OAuth is an authorization framework.", nil }}
	server := researchTestServer(t, oracle, nil)

	code, resp, body := postResearch(t, server, map[string]string{"query": "what is OAuth"})
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}
	if oracle.calls.Load() != 1 {
		t.Errorf("expected exactly 1 oracle call, got %d", oracle.calls.Load())
	}
	if resp["cache_hit"] != false {
		t.Errorf("first call should not be a cache_hit, got %v", resp["cache_hit"])
	}
	entry, _ := resp["entry"].(map[string]interface{})
	if entry["query"] != "what is OAuth" {
		t.Errorf("entry.query mismatch, got %v", entry["query"])
	}
	if !strings.Contains(entry["content"].(string), "OAuth is") {
		t.Errorf("entry.content should be Oracle answer, got %v", entry["content"])
	}
	if entry["model"] != "oracle:gpt-4o-mini" {
		t.Errorf("entry.model should reflect Tier 3 provider name, got %v", entry["model"])
	}
	if !strings.HasPrefix(entry["id"].(string), "research-") {
		t.Errorf("entry.id should be `research-<hash>`, got %v", entry["id"])
	}
}

func TestResearchPost_CacheHit(t *testing.T) {
	oracle := &mockOracle{name: "o", respond: func(_ string) (string, error) { return "answer", nil }}
	server := researchTestServer(t, oracle, nil)

	// First call → fresh.
	if code, _, body := postResearch(t, server, map[string]string{"query": "stable"}); code != http.StatusOK {
		t.Fatalf("first call: %d %s", code, body)
	}
	firstHits := oracle.calls.Load()
	if firstHits != 1 {
		t.Fatalf("expected 1 oracle call after first request, got %d", firstHits)
	}

	// Second call same query → cache_hit.
	code, resp, body := postResearch(t, server, map[string]string{"query": "stable"})
	if code != http.StatusOK {
		t.Fatalf("second call: %d %s", code, body)
	}
	if resp["cache_hit"] != true {
		t.Errorf("second call should be cache_hit:true, got %v", resp["cache_hit"])
	}
	if oracle.calls.Load() != firstHits {
		t.Errorf("cache hit should NOT trigger another oracle call (was %d, now %d)",
			firstHits, oracle.calls.Load())
	}
	// And cache hit shouldn't add tokens.
	entry, _ := resp["entry"].(map[string]interface{})
	if entry["query"] != "stable" {
		t.Errorf("cached entry.query mismatch, got %v", entry["query"])
	}
}

func TestResearchPost_SensitiveExcluded(t *testing.T) {
	oracle := &mockOracle{name: "o", respond: func(_ string) (string, error) { return "answer", nil }}
	server := researchTestServer(t, oracle, nil)

	// Seed: one normal memory + one sensitive memory.
	_, _ = server.bank.SaveMemory(MemoryRecord{
		Text: "Normal memory about OAuth flows", Tags: []string{"auth"},
	})
	const secret = "Sneaky private journal entry that must NOT reach the oracle"
	_, _ = server.bank.SaveMemory(MemoryRecord{
		Text: secret, Tags: []string{"private"}, // tag triggers sensitive auto-flag
	})

	code, resp, body := postResearch(t, server, map[string]string{"query": "anything"})
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}

	excl, _ := resp["sensitive_excluded"].(float64)
	if excl < 1 {
		t.Errorf("expected sensitive_excluded >= 1, got %v", resp["sensitive_excluded"])
	}

	// Defense-in-depth: the prompt the oracle saw must NOT contain the
	// sensitive memory body.
	prompt, _ := oracle.lastPrompt.Load().(string)
	if strings.Contains(prompt, "Sneaky private journal") {
		t.Errorf("INVARIANT VIOLATION: sensitive memory text reached oracle prompt: %q", prompt)
	}
	// And the normal memory SHOULD be present (otherwise the filter is too aggressive).
	if !strings.Contains(prompt, "Normal memory about OAuth") {
		t.Errorf("non-sensitive memory should be in prompt; got: %q", prompt)
	}
}

func TestResearchPost_LocalConceptExcluded(t *testing.T) {
	oracle := &mockOracle{name: "o", respond: func(_ string) (string, error) { return "answer", nil }}
	server := researchTestServer(t, oracle, []string{"GravityCAD"})

	// Memory with a LocalConcept tag — must be excluded.
	_, _ = server.bank.SaveMemory(MemoryRecord{
		Text: "I use the tool daily", Tags: []string{"GravityCAD", "design"},
	})
	// Memory with the LocalConcept in TEXT — also excluded.
	_, _ = server.bank.SaveMemory(MemoryRecord{
		Text: "Tested GravityCAD for hours", Tags: []string{"work"},
	})
	// Clean memory — should pass through.
	_, _ = server.bank.SaveMemory(MemoryRecord{
		Text: "Generic OAuth notes", Tags: []string{"auth"},
	})

	code, resp, body := postResearch(t, server, map[string]string{"query": "design tools"})
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}
	excl, _ := resp["local_concept_excluded"].(float64)
	if excl < 2 {
		t.Errorf("expected local_concept_excluded >= 2, got %v", resp["local_concept_excluded"])
	}
	prompt, _ := oracle.lastPrompt.Load().(string)
	if strings.Contains(prompt, "GravityCAD") {
		t.Errorf("INVARIANT VIOLATION: GravityCAD reached oracle prompt: %q", prompt)
	}
}

func TestResearchPost_OracleDisabled_KillSwitch(t *testing.T) {
	t.Setenv("SD_DISABLE_ORACLE", "1")
	oracle := &mockOracle{name: "o"}
	server := researchTestServer(t, oracle, nil)

	code, _, body := postResearch(t, server, map[string]string{"query": "anything"})
	if code != http.StatusForbidden {
		t.Errorf("expected 403 with kill switch, got %d: %s", code, body)
	}
	if oracle.calls.Load() != 0 {
		t.Errorf("oracle MUST NOT be called when kill switch is on")
	}
}

func TestResearchPost_Tier3Unconfigured(t *testing.T) {
	bank := newTestBank(t)
	tier1 := &fakeProvider{name: "t1", local: true}
	router := NewModelRouter(tier1, nil, nil /* tier3 nil */, nil)
	server := &Server{bank: bank, router: router}

	code, _, body := postResearch(t, server, map[string]string{"query": "anything"})
	if code != http.StatusForbidden {
		t.Errorf("expected 403 when tier3 is nil, got %d: %s", code, body)
	}
}

func TestResearchPost_EmptyQuery(t *testing.T) {
	server := researchTestServer(t, &mockOracle{name: "o"}, nil)
	code, _, body := postResearch(t, server, map[string]string{"query": "   "})
	if code != http.StatusBadRequest {
		t.Errorf("expected 400 for whitespace-only query, got %d: %s", code, body)
	}
}

func TestResearchPost_QueryTooLong(t *testing.T) {
	server := researchTestServer(t, &mockOracle{name: "o"}, nil)
	huge := strings.Repeat("x", 1001)
	code, _, _ := postResearch(t, server, map[string]string{"query": huge})
	if code != http.StatusBadRequest {
		t.Errorf("expected 400 for >1000 char query, got %d", code)
	}
}

func TestResearchPost_BudgetUpdated(t *testing.T) {
	oracle := &mockOracle{name: "o",
		respond: func(_ string) (string, error) {
			return strings.Repeat("response ", 100), nil // ~900 chars → ~225 tokens out
		}}
	server := researchTestServer(t, oracle, nil)

	today := time.Now().UTC().Format("2006-01-02")
	before, _ := server.bank.GetTokensUsed(today)

	code, _, body := postResearch(t, server, map[string]string{"query": "what is the answer"})
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}

	after, _ := server.bank.GetTokensUsed(today)
	if after <= before {
		t.Errorf("token budget should have grown after research; before=%d after=%d", before, after)
	}
}

func TestResearchPost_AuditLogged(t *testing.T) {
	const sneaky = "DO-NOT-LEAK-THIS-CONTENT-zzzz9999"
	oracle := &mockOracle{name: "o",
		respond: func(_ string) (string, error) { return sneaky, nil }}
	server := researchTestServer(t, oracle, nil)

	code, _, _ := postResearch(t, server, map[string]string{"query": "trigger audit"})
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}

	entries, err := server.bank.ListAuditLog(AuditFilter{
		EntityType: "research_cache", Operation: "oracle_research",
	})
	if err != nil {
		t.Fatalf("ListAuditLog: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(entries))
	}
	e := entries[0]
	if e.EntityID == "" || !strings.HasPrefix(e.EntityID, "research-") {
		t.Errorf("expected entity_id to be research-<id>, got %q", e.EntityID)
	}
	// Audit MUST NOT contain the full content. It should record counts +
	// query length only.
	for _, field := range []string{e.AfterJSON, e.BeforeJSON, e.Reason} {
		if strings.Contains(field, sneaky) {
			t.Errorf("INVARIANT VIOLATION: audit field contained Oracle response: %q", field)
		}
	}
	if !strings.Contains(e.AfterJSON, "tokens_total") {
		t.Errorf("audit AfterJSON should record tokens_total, got %q", e.AfterJSON)
	}
}

func TestResearchPost_AuthRequired(t *testing.T) {
	t.Setenv("SD_API_TOKEN", "research-test-token")
	server := researchTestServer(t, &mockOracle{name: "o"}, nil)
	handler := newAuthMiddleware(server.routes())

	body, _ := json.Marshal(map[string]string{"query": "anything"})
	req := httptest.NewRequest(http.MethodPost, "/research", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without token, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestResearchPost_RoutePrecedence — POST /research must reach the new
// handler, not be absorbed by the /research/{topic} GET-only catchall or
// any other dispatcher branch.
func TestResearchPost_RoutePrecedence(t *testing.T) {
	oracle := &mockOracle{name: "o", respond: func(_ string) (string, error) { return "ok", nil }}
	server := researchTestServer(t, oracle, nil)

	body, _ := json.Marshal(map[string]string{"query": "test"})
	req := httptest.NewRequest(http.MethodPost, "/research", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /research should reach handler with 200, got %d (catchall absorbed?): %s",
			rec.Code, rec.Body.String())
	}
	// Sanity: GET /research still hits the list handler.
	req2 := httptest.NewRequest(http.MethodGet, "/research", nil)
	rec2 := httptest.NewRecorder()
	server.routes().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Errorf("GET /research should still 200, got %d", rec2.Code)
	}
}

// TestResearchPost_OracleErrorPropagated — when Tier 3 returns a network
// error mid-call, we surface 500. Doesn't crash, doesn't leak stack.
func TestResearchPost_OracleErrorPropagated(t *testing.T) {
	oracle := &mockOracle{name: "o",
		respond: func(_ string) (string, error) { return "", errors.New("upstream timeout") }}
	server := researchTestServer(t, oracle, nil)

	code, _, body := postResearch(t, server, map[string]string{"query": "trigger error"})
	if code != http.StatusInternalServerError {
		t.Errorf("expected 500 on oracle error, got %d: %s", code, body)
	}
	if !strings.Contains(body, "oracle") {
		t.Errorf("error body should mention oracle, got %q", body)
	}
}
