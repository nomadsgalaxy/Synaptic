// handlers_wave8a_test.go — wave 8a tests for the new-spec body on
// /maps/merge + the AirLLM configure_tier / health/{port} endpoints.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ──────────────────────────────────────────────────────────────────────────
// /maps/merge — new spec body shape
// ──────────────────────────────────────────────────────────────────────────

func seedMergeFixture(t *testing.T, bank *Bank) (primary, srcA, srcB string) {
	t.Helper()
	// Three memories: one carries srcA, one carries srcB, one carries both.
	primary = "event-tracker"
	srcA = "project:event-tracker"
	srcB = "ET"
	for _, tags := range [][]string{
		{srcA, "common"},
		{srcB, "common"},
		{srcA, srcB, "common"},
	} {
		_, err := bank.SaveMemory(MemoryRecord{Text: "x", Tags: tags})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return
}

func TestMapsMerge_NewBodyShape_DryRun(t *testing.T) {
	bank := newTestBank(t)
	primary, srcA, srcB := seedMergeFixture(t, bank)
	srv := newTestServer(t, bank)
	body := fmt.Sprintf(`{"primary_id":%q,"source_ids":[%q,%q],"dry_run":true}`, primary, srcA, srcB)
	req := httptest.NewRequest(http.MethodPost, "/maps/merge", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleMapsMerge(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	// New-spec keys present.
	if resp["primary_id"] != primary {
		t.Errorf("primary_id: want %q, got %v", primary, resp["primary_id"])
	}
	if _, ok := resp["source_ids"].([]any); !ok {
		t.Errorf("source_ids should be a list")
	}
	if got := int(resp["would_update"].(float64)); got != 3 {
		t.Errorf("would_update: want 3, got %d", got)
	}
	sb, ok := resp["source_breakdown"].(map[string]any)
	if !ok {
		t.Fatalf("source_breakdown missing or wrong type")
	}
	if got := int(sb[srcA].(float64)); got != 2 {
		t.Errorf("source_breakdown[%s]: want 2, got %d", srcA, got)
	}
	if got := int(sb[srcB].(float64)); got != 2 {
		t.Errorf("source_breakdown[%s]: want 2, got %d", srcB, got)
	}
	// Legacy keys still present.
	if resp["from"] == nil || resp["to"] == nil {
		t.Errorf("legacy from/to keys should still be present")
	}
	// Dry-run: no audit row written.
	audit, _ := bank.ListAuditLog(AuditFilter{Operation: "tag_merge", Limit: 5})
	if len(audit) != 0 {
		t.Errorf("dry-run shouldn't write audit; got %d rows", len(audit))
	}
}

func TestMapsMerge_LegacyBodyShape_StillWorks(t *testing.T) {
	bank := newTestBank(t)
	primary, srcA, _ := seedMergeFixture(t, bank)
	srv := newTestServer(t, bank)
	body := fmt.Sprintf(`{"from":[%q],"to":%q,"dry_run":true}`, srcA, primary)
	req := httptest.NewRequest(http.MethodPost, "/maps/merge", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleMapsMerge(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("legacy body: want 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	// Both legacy + new-spec keys should be in the response.
	if resp["primary_id"] != primary {
		t.Errorf("primary_id should be populated even from legacy body")
	}
	if _, ok := resp["would_update"]; !ok {
		t.Errorf("would_update should appear even from legacy body")
	}
}

func TestMapsMerge_RealRun_WritesPerSourceAudit(t *testing.T) {
	bank := newTestBank(t)
	primary, srcA, srcB := seedMergeFixture(t, bank)
	srv := newTestServer(t, bank)
	srv.hub = NewHub(NewRingBuffer(10))
	body := fmt.Sprintf(`{"primary_id":%q,"source_ids":[%q,%q]}`, primary, srcA, srcB)
	req := httptest.NewRequest(http.MethodPost, "/maps/merge", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleMapsMerge(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	// Summary tag_merge row.
	summary, _ := bank.ListAuditLog(AuditFilter{Operation: "tag_merge", Limit: 5})
	if len(summary) != 1 {
		t.Errorf("want 1 tag_merge row, got %d", len(summary))
	}
	// Per-source map_merged rows: one per source.
	perSource, _ := bank.ListAuditLog(AuditFilter{Operation: "map_merged", Limit: 10})
	if len(perSource) != 2 {
		t.Fatalf("want 2 map_merged rows (one per source), got %d", len(perSource))
	}
	gotIDs := map[string]bool{}
	for _, e := range perSource {
		gotIDs[e.EntityID] = true
	}
	if !gotIDs[srcA] || !gotIDs[srcB] {
		t.Errorf("map_merged entity_ids: want both sources, got %v", gotIDs)
	}
}

func TestMapsMerge_MissingPrimaryReturns400(t *testing.T) {
	bank := newTestBank(t)
	srv := newTestServer(t, bank)
	body := `{"source_ids":["x"]}`
	req := httptest.NewRequest(http.MethodPost, "/maps/merge", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleMapsMerge(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "primary_id") {
		t.Errorf("error should mention primary_id; got %s", w.Body.String())
	}
}

func TestMapsMerge_MissingSourcesReturns400(t *testing.T) {
	bank := newTestBank(t)
	srv := newTestServer(t, bank)
	body := `{"primary_id":"x"}`
	req := httptest.NewRequest(http.MethodPost, "/maps/merge", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleMapsMerge(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "source_ids") {
		t.Errorf("error should mention source_ids; got %s", w.Body.String())
	}
}

// ──────────────────────────────────────────────────────────────────────────
// /admin/airllm/configure_tier
// ──────────────────────────────────────────────────────────────────────────

func TestConfigureTier_RejectsTier1(t *testing.T) {
	bank := newTestBank(t)
	srv := newTestServer(t, bank)
	body := `{"tier":"tier1","endpoint":"http://1.2.3.4:9912","model":"x"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/airllm/configure_tier", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAdminAirLLMConfigureTier(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("Tier 1: want 400, got %d", w.Code)
	}
}

func TestConfigureTier_UnreachableEndpointReturns400(t *testing.T) {
	bank := newTestBank(t)
	srv := newTestServer(t, bank)
	// Mock nvidia present so the GPU gate does NOT short-circuit; the
	// test specifically exercises the unreachable-endpoint branch.
	// The no-nvidia path is covered by TestConfigureTier_NoNvidia_4bit_ReturnsCUDARequired.
	prevProc := nvidiaProcVersionPath
	prevInfo := dockerInfoFn
	t.Cleanup(func() {
		nvidiaProcVersionPath = prevProc
		dockerInfoFn = prevInfo
	})
	nvidiaProcVersionPath = "/nonexistent-on-purpose"
	dockerInfoFn = func(_ context.Context, _ *DockerControlState) (SystemInfo, error) {
		return SystemInfo{Runtimes: map[string]struct{}{"runc": {}, "nvidia": {}}}, nil
	}
	body := `{"tier":"tier3","endpoint":"http://127.0.0.1:1","model":"x","compression":"4bit"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/airllm/configure_tier", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAdminAirLLMConfigureTier(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("unreachable endpoint: want 400, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["error"] != "unreachable" {
		t.Errorf("error tag: want unreachable, got %v", resp["error"])
	}
	// No provider config persisted.
	cfg, _ := bank.GetProviderConfig(Tier3)
	if cfg.Kind == ProviderAirLLM {
		t.Errorf("unreachable endpoint should NOT persist provider config; got %+v", cfg)
	}
}

func TestConfigureTier_HappyPathPersists(t *testing.T) {
	// Spin up a fake /healthz endpoint.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			// AirLLMProvider.Ping expects status=ok + loaded=true.
			w.Write([]byte(`{"status":"ok","loaded":true,"model":"stub"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	bank := newTestBank(t)
	server := newTestServer(t, bank)
	body := fmt.Sprintf(`{"tier":"tier2","endpoint":%q,"model":"stub","compression":"8bit"}`, srv.URL)
	req := httptest.NewRequest(http.MethodPost, "/admin/airllm/configure_tier", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.handleAdminAirLLMConfigureTier(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	cfg, _ := bank.GetProviderConfig(Tier2)
	if cfg.Kind != ProviderAirLLM {
		t.Errorf("tier2 kind: want airllm, got %s", cfg.Kind)
	}
	if cfg.Model != "stub" {
		t.Errorf("tier2 model: want stub, got %s", cfg.Model)
	}
	// Compression saved alongside.
	v, ok, _ := bank.GetSetting("airllm.tier2_provider.compression")
	if !ok || v != "8bit" {
		t.Errorf("compression setting: want 8bit, got ok=%v v=%q", ok, v)
	}
}

func TestConfigureTier_InvalidCompressionReturns400(t *testing.T) {
	bank := newTestBank(t)
	srv := newTestServer(t, bank)
	body := `{"tier":"tier3","endpoint":"http://x","model":"y","compression":"yeet"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/airllm/configure_tier", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAdminAirLLMConfigureTier(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid compression: want 400, got %d", w.Code)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// /admin/airllm/health/{port}
// ──────────────────────────────────────────────────────────────────────────

func TestAirLLMHealthPort_PortOutOfRange(t *testing.T) {
	srv := newTestServer(t, nil)
	for _, p := range []string{"80", "443", "11434", "10000"} {
		req := httptest.NewRequest(http.MethodGet, "/admin/airllm/health/"+p, nil)
		w := httptest.NewRecorder()
		srv.handleAdminAirLLMHealthPort(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("port %s: want 400, got %d", p, w.Code)
		}
	}
}

func TestAirLLMHealthPort_AcceptsInRangePortAndReturnsUnreachable(t *testing.T) {
	srv := newTestServer(t, nil)
	srv.docker = NewDockerControlState()
	// Pick a free port in the 9900-9999 range that nothing's listening on.
	req := httptest.NewRequest(http.MethodGet, "/admin/airllm/health/9901", nil)
	w := httptest.NewRecorder()
	srv.handleAdminAirLLMHealthPort(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["reachable"] != false {
		t.Errorf("port 9901 (likely nothing listening): want reachable=false, got %v", resp["reachable"])
	}
}

func TestAirLLMHealthPort_ForbidsArbitraryHost(t *testing.T) {
	srv := newTestServer(t, nil)
	srv.docker = NewDockerControlState() // allowlist regex ^synaptic-
	req := httptest.NewRequest(http.MethodGet, "/admin/airllm/health/9912?host=evil.example.com", nil)
	w := httptest.NewRecorder()
	srv.handleAdminAirLLMHealthPort(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("arbitrary host: want 403, got %d", w.Code)
	}
}

func TestAirLLMHealthPort_MissingPort(t *testing.T) {
	srv := newTestServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/admin/airllm/health/", nil)
	w := httptest.NewRecorder()
	srv.handleAdminAirLLMHealthPort(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing port: want 400, got %d", w.Code)
	}
}
