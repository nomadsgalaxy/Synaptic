package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// augTestServer wires a Server with bank + a default-disabled augment
// settings + a mock Tier 3 oracle.
func augTestServer(t *testing.T) (*Server, *mockOracle) {
	t.Helper()
	bank := newTestBank(t)
	oracle := &mockOracle{name: "openai:gpt-4o-mini",
		respond: func(_ string) (string, error) { return "Augmented answer.", nil }}
	tier1 := &fakeProvider{name: "tier1-local", local: true}
	router := NewModelRouter(tier1, nil, oracle, nil)
	return &Server{bank: bank, router: router}, oracle
}

// seedThinTopicMap creates a map_override + N tiny memories tagged with
// the map key so the candidate scanner sees a thin map.
func seedThinTopicMap(t *testing.T, bank *Bank, key, category string, n, charsPerMem int) {
	t.Helper()
	must(t, bank.SaveMapOverride(MapOverride{
		MapKey: key, Category: category,
	}))
	body := strings.Repeat("x", charsPerMem)
	for i := 0; i < n; i++ {
		_, err := bank.SaveMemory(MemoryRecord{
			Text: body, Tags: []string{key},
		})
		if err != nil {
			t.Fatalf("seed memory: %v", err)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────
// findAugmentCandidates gating
// ──────────────────────────────────────────────────────────────────────────

func TestAugment_CategoryGate_TopicEligible(t *testing.T) {
	server, _ := augTestServer(t)
	seedThinTopicMap(t, server.bank, "vector-embeddings", "topic", 1, 50)

	cands, err := findAugmentCandidates(context.Background(), server.bank,
		DefaultAugmentSettings(), nil)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(cands) != 1 || cands[0].MapKey != "vector-embeddings" {
		t.Fatalf("expected 1 topic candidate, got %+v", cands)
	}
}

func TestAugment_CategoryGate_ProjectExcluded(t *testing.T) {
	server, _ := augTestServer(t)
	seedThinTopicMap(t, server.bank, "secret-project", "project", 1, 50)

	cands, _ := findAugmentCandidates(context.Background(), server.bank,
		DefaultAugmentSettings(), nil)
	for _, c := range cands {
		if c.MapKey == "secret-project" {
			t.Errorf("project category MUST NOT be a candidate; got %+v", c)
		}
	}
}

func TestAugment_CategoryGate_EntityExcluded(t *testing.T) {
	server, _ := augTestServer(t)
	seedThinTopicMap(t, server.bank, "anthony-friend", "entity", 1, 50)

	cands, _ := findAugmentCandidates(context.Background(), server.bank,
		DefaultAugmentSettings(), nil)
	for _, c := range cands {
		if c.MapKey == "anthony-friend" {
			t.Errorf("entity category MUST NOT be a candidate; got %+v", c)
		}
	}
}

func TestAugment_CategoryGate_ConceptAndTechnologyEligible(t *testing.T) {
	server, _ := augTestServer(t)
	seedThinTopicMap(t, server.bank, "vector-spaces", "concept", 1, 50)
	seedThinTopicMap(t, server.bank, "postgresql", "technology", 1, 50)

	cands, _ := findAugmentCandidates(context.Background(), server.bank,
		DefaultAugmentSettings(), nil)
	if len(cands) != 2 {
		t.Fatalf("expected concept+technology both eligible, got %d: %+v", len(cands), cands)
	}
}

func TestAugment_LocalConceptExcluded(t *testing.T) {
	server, _ := augTestServer(t)
	must(t, server.bank.SaveMapOverride(MapOverride{
		MapKey: "gravity", Category: "concept",
	}))
	// Memory tagged with the map key + a LocalConcept
	_, _ = server.bank.SaveMemory(MemoryRecord{
		Text: "thin", Tags: []string{"gravity", "GravityCAD"},
	})

	cands, _ := findAugmentCandidates(context.Background(), server.bank,
		DefaultAugmentSettings(), map[string]bool{"GravityCAD": true})
	for _, c := range cands {
		if c.MapKey == "gravity" {
			t.Errorf("LocalConcept-tagged memories should disqualify the map; got %+v", c)
		}
	}
}

func TestAugment_SensitiveExcluded(t *testing.T) {
	server, _ := augTestServer(t)
	must(t, server.bank.SaveMapOverride(MapOverride{
		MapKey: "private-tag", Category: "topic",
	}))
	_, _ = server.bank.SaveMemory(MemoryRecord{
		Text: "thin",
		// "private" tag triggers sensitive auto-flag
		Tags: []string{"private-tag", "private"},
	})

	cands, _ := findAugmentCandidates(context.Background(), server.bank,
		DefaultAugmentSettings(), nil)
	for _, c := range cands {
		if c.MapKey == "private-tag" {
			t.Errorf("sensitive memory in map should disqualify; got %+v", c)
		}
	}
}

func TestAugment_ThicknessThreshold_RichMapNotEligible(t *testing.T) {
	server, _ := augTestServer(t)
	// 10 memories of 500 chars each → rich, NOT a candidate.
	seedThinTopicMap(t, server.bank, "rich-topic", "topic", 10, 500)

	cands, _ := findAugmentCandidates(context.Background(), server.bank,
		DefaultAugmentSettings(), nil)
	for _, c := range cands {
		if c.MapKey == "rich-topic" {
			t.Errorf("rich map (10 memories × 500 chars) should NOT be a candidate; got %+v", c)
		}
	}
}

func TestAugment_ArchivedExcluded(t *testing.T) {
	server, _ := augTestServer(t)
	must(t, server.bank.SaveMapOverride(MapOverride{
		MapKey: "abandoned", Category: "topic", Archived: true,
	}))
	_, _ = server.bank.SaveMemory(MemoryRecord{Text: "thin", Tags: []string{"abandoned"}})

	cands, _ := findAugmentCandidates(context.Background(), server.bank,
		DefaultAugmentSettings(), nil)
	for _, c := range cands {
		if c.MapKey == "abandoned" {
			t.Errorf("archived map should never be a candidate; got %+v", c)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Settings round-trip
// ──────────────────────────────────────────────────────────────────────────

func TestAugmentSettings_RoundTrip(t *testing.T) {
	server, _ := augTestServer(t)
	in := AugmentSettings{
		Enabled: true, MaxPerRun: 5, MaxTokensPerRun: 10000,
		MinMemories: 8, MinAvgChars: 300, CooldownDays: 14,
	}
	if err := server.bank.SaveAugmentSettings(in); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, _ := server.bank.GetAugmentSettings()
	if got != in {
		t.Errorf("round-trip mismatch:\n want %+v\n  got %+v", in, got)
	}
}

func TestAugmentSettings_DefaultsOff(t *testing.T) {
	server, _ := augTestServer(t)
	got, _ := server.bank.GetAugmentSettings()
	if got.Enabled {
		t.Errorf("augment_enabled MUST default false (opt-in for paid egress)")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// HTTP
// ──────────────────────────────────────────────────────────────────────────

func TestAugment_Endpoint_Candidates(t *testing.T) {
	server, _ := augTestServer(t)
	seedThinTopicMap(t, server.bank, "thin-tech", "technology", 1, 50)

	req := httptest.NewRequest(http.MethodGet, "/maps/augmentation/candidates", nil)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET candidates expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["count"].(float64) < 1 {
		t.Errorf("expected >=1 candidate, got %v", resp["count"])
	}
}

func TestAugment_Endpoint_RunNow_DisabledByDefault(t *testing.T) {
	server, _ := augTestServer(t)
	seedThinTopicMap(t, server.bank, "thin-topic", "topic", 1, 50)

	body := []byte(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/maps/augmentation/run-now",
		strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	// Default settings have augment_enabled=false → 403.
	if rec.Code != http.StatusForbidden {
		t.Errorf("run-now with disabled default should 403, got %d: %s",
			rec.Code, rec.Body.String())
	}
}

func TestAugment_Endpoint_RunNow_HappyPath(t *testing.T) {
	server, oracle := augTestServer(t)
	seedThinTopicMap(t, server.bank, "thin-topic", "topic", 1, 50)
	// Enable the master switch.
	must(t, server.bank.SaveAugmentSettings(AugmentSettings{
		Enabled: true, MaxPerRun: 3, MaxTokensPerRun: 5000,
		MinMemories: 5, MinAvgChars: 200, CooldownDays: 30,
	}))

	body := []byte(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/maps/augmentation/run-now",
		strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if oracle.calls.Load() < 1 {
		t.Errorf("expected oracle called at least once, got %d", oracle.calls.Load())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["augmented_count"].(float64) < 1 {
		t.Errorf("expected at least 1 augmentation, got %v", resp["augmented_count"])
	}
}

func TestAugment_Endpoint_DryRun(t *testing.T) {
	server, oracle := augTestServer(t)
	seedThinTopicMap(t, server.bank, "thin-topic", "topic", 1, 50)
	must(t, server.bank.SaveAugmentSettings(AugmentSettings{
		Enabled: true, MaxPerRun: 3, MaxTokensPerRun: 5000,
		MinMemories: 5, MinAvgChars: 200, CooldownDays: 30,
	}))

	body := []byte(`{"dry_run":true}`)
	req := httptest.NewRequest(http.MethodPost, "/maps/augmentation/run-now",
		strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if oracle.calls.Load() != 0 {
		t.Errorf("dry_run must NOT call oracle, got %d calls", oracle.calls.Load())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["dry_run"] != true {
		t.Errorf("expected dry_run=true in response, got %v", resp["dry_run"])
	}
}

func TestAugment_Endpoint_RunNow_FetchedByAuto(t *testing.T) {
	server, _ := augTestServer(t)
	seedThinTopicMap(t, server.bank, "concept-x", "concept", 1, 50)
	must(t, server.bank.SaveAugmentSettings(AugmentSettings{
		Enabled: true, MaxPerRun: 1, MaxTokensPerRun: 5000,
		MinMemories: 5, MinAvgChars: 200, CooldownDays: 30,
	}))

	body := []byte(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/maps/augmentation/run-now",
		strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// The augmentation should have written a research_cache row with
	// fetched_by=auto (system-driven, not user-typed).
	rows, _ := server.bank.ListResearchCache(0, true)
	if len(rows) == 0 {
		t.Fatalf("expected research_cache row from augmentation")
	}
	for _, row := range rows {
		if row.FetchedBy != "auto" {
			t.Errorf("augmentation-created research entry should have fetched_by=auto, got %q",
				row.FetchedBy)
		}
	}
}
