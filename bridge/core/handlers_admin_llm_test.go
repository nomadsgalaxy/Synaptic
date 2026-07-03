// handlers_admin_llm_test.go — Wave 8e tests for the LLM benchmark +
// nightly-duration estimator. Live Chat() against a real provider is out
// of scope (no Ollama / AirLLM in the test sandbox). What we cover here:
//
//   - Bank roundtrip: Upsert / Get / List with replace-on-conflict
//     semantics keyed by (provider_kind, model, compression).
//   - GET /admin/llm/benchmark returns the persisted records as JSON.
//   - GET /admin/llm/estimate_nightly with no nightly runs and no cached
//     benchmark surfaces actionable caveats and a 0 estimate.
//   - GET /admin/llm/estimate_nightly with seeded runs + a cached Tier 2
//     benchmark returns total_seconds ≈ avg_tokens / tokens_per_sec, and
//     the per-phase breakdown matches the seeded stats.
//   - handleAdminLLMBenchmark gates: method, missing tier, disabled tier.
//   - humaniseDuration + nullIfZero edge cases.
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ──────────────────────────────────────────────────────────────────────────
// Bank roundtrip
// ──────────────────────────────────────────────────────────────────────────

func TestProviderBenchmark_BankRoundtrip(t *testing.T) {
	bank := newTestBank(t)

	rec := ProviderBenchmark{
		ProviderKind:     "airllm",
		Model:            "meta-llama/Llama-3.1-70B-Instruct",
		Compression:      "4bit",
		ElapsedMS:        50000,
		PromptTokens:     50,
		CompletionTokens: 50,
		TokensPerSec:     1.0,
		SampleExcerpt:    "Sample.",
	}
	if err := bank.UpsertProviderBenchmark(rec); err != nil {
		t.Fatalf("UpsertProviderBenchmark: %v", err)
	}

	got, ok, err := bank.GetProviderBenchmark(rec.ProviderKind, rec.Model, rec.Compression)
	if err != nil {
		t.Fatalf("GetProviderBenchmark: %v", err)
	}
	if !ok {
		t.Fatalf("GetProviderBenchmark: row missing after upsert")
	}
	if got.TokensPerSec != 1.0 || got.CompletionTokens != 50 || got.ElapsedMS != 50000 {
		t.Errorf("readback mismatch: %+v", got)
	}
	if got.BenchmarkedAt == "" {
		t.Errorf("benchmarked_at should be auto-populated when blank on insert")
	}

	// Replace on conflict — same tuple, new measurement.
	rec.TokensPerSec = 2.5
	rec.ElapsedMS = 20000
	rec.CompletionTokens = 50
	if err := bank.UpsertProviderBenchmark(rec); err != nil {
		t.Fatalf("UpsertProviderBenchmark (replace): %v", err)
	}
	got, ok, _ = bank.GetProviderBenchmark(rec.ProviderKind, rec.Model, rec.Compression)
	if !ok || got.TokensPerSec != 2.5 || got.ElapsedMS != 20000 {
		t.Errorf("replace-on-conflict failed; got %+v", got)
	}

	// Different compression is a different tuple — separate row.
	rec.Compression = "8bit"
	rec.TokensPerSec = 0.7
	if err := bank.UpsertProviderBenchmark(rec); err != nil {
		t.Fatalf("UpsertProviderBenchmark (8bit): %v", err)
	}
	rows, err := bank.ListProviderBenchmarks()
	if err != nil {
		t.Fatalf("ListProviderBenchmarks: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (4bit + 8bit), got %d", len(rows))
	}
}

func TestProviderBenchmark_Get_NoRow_ReturnsOKFalse(t *testing.T) {
	bank := newTestBank(t)
	_, ok, err := bank.GetProviderBenchmark("airllm", "nonexistent", "")
	if err != nil {
		t.Errorf("missing row should not surface an error; got %v", err)
	}
	if ok {
		t.Errorf("missing row should return ok=false")
	}
}

func TestProviderBenchmark_Upsert_RequiresKindAndModel(t *testing.T) {
	bank := newTestBank(t)
	if err := bank.UpsertProviderBenchmark(ProviderBenchmark{}); err == nil {
		t.Errorf("expected error when kind+model are empty")
	}
	if err := bank.UpsertProviderBenchmark(ProviderBenchmark{ProviderKind: "ollama"}); err == nil {
		t.Errorf("expected error when model is empty")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// GET /admin/llm/benchmark
// ──────────────────────────────────────────────────────────────────────────

func TestAdminLLMBenchmarkList_EmptyAndPopulated(t *testing.T) {
	bank := newTestBank(t)
	srv := newTestServer(t, bank)

	req := httptest.NewRequest(http.MethodGet, "/admin/llm/benchmark", nil)
	w := httptest.NewRecorder()
	srv.adminLLMRouter(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("empty list want 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Records []ProviderBenchmark `json:"records"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Records) != 0 {
		t.Errorf("empty list should return [], got %v", resp.Records)
	}

	_ = bank.UpsertProviderBenchmark(ProviderBenchmark{
		ProviderKind: "ollama", Model: "llama3.2:3b", TokensPerSec: 30.0,
		ElapsedMS: 1000, CompletionTokens: 30,
	})
	req = httptest.NewRequest(http.MethodGet, "/admin/llm/benchmark", nil)
	w = httptest.NewRecorder()
	srv.adminLLMRouter(w, req)
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Records) != 1 || resp.Records[0].Model != "llama3.2:3b" {
		t.Errorf("populated list mismatch: %+v", resp.Records)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// GET /admin/llm/estimate_nightly
// ──────────────────────────────────────────────────────────────────────────

func TestAdminLLMEstimateNightly_NoDataReturnsCaveats(t *testing.T) {
	bank := newTestBank(t)
	srv := newTestServer(t, bank)

	req := httptest.NewRequest(http.MethodGet, "/admin/llm/estimate_nightly", nil)
	w := httptest.NewRecorder()
	srv.adminLLMRouter(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("empty estimate want 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		BasedOnRuns          int      `json:"based_on_runs"`
		TokensPerSec         any      `json:"tokens_per_sec"`
		EstimatedTotalSec    float64  `json:"estimated_total_seconds"`
		EstimatedHuman       string   `json:"estimated_human"`
		Caveats              []string `json:"caveats"`
		PerPhase             []NightlyEstimatePhase `json:"per_phase"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.BasedOnRuns != 0 {
		t.Errorf("based_on_runs: want 0, got %d", resp.BasedOnRuns)
	}
	if resp.TokensPerSec != nil {
		t.Errorf("tokens_per_sec: want null, got %v", resp.TokensPerSec)
	}
	if resp.EstimatedTotalSec != 0 {
		t.Errorf("estimated_total_seconds: want 0, got %f", resp.EstimatedTotalSec)
	}
	if resp.EstimatedHuman != "" {
		t.Errorf("estimated_human: want empty string, got %q", resp.EstimatedHuman)
	}
	if len(resp.Caveats) < 2 {
		t.Errorf("want both 'no runs' and 'no benchmark' caveats; got %v", resp.Caveats)
	}
	if len(resp.PerPhase) != 0 {
		t.Errorf("per_phase: want empty, got %v", resp.PerPhase)
	}
}

// seedStatsJSON renders a NightlyStats blob with the given per-phase tokens.
func seedStatsJSON(t *testing.T, deepIn, deepOut, augment, total int) string {
	t.Helper()
	stats := NightlyStats{
		TokensTotal: total,
		AugmentedTokens: augment,
		DeepEncoding: &DeepEncodingStats{
			TokensIn:  deepIn,
			TokensOut: deepOut,
		},
		SchemaVersion: 3,
	}
	b, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("marshal stats: %v", err)
	}
	return string(b)
}

func TestAdminLLMEstimateNightly_Populated(t *testing.T) {
	bank := newTestBank(t)
	srv := newTestServer(t, bank)

	// Configure Tier 2 — Ollama llama3.2:3b — and cache a benchmark for it.
	tier2 := ProviderConfig{Kind: ProviderOllama, Model: "llama3.2:3b"}
	if err := bank.SaveProviderConfig(Tier2, tier2); err != nil {
		t.Fatalf("SaveProviderConfig: %v", err)
	}
	if err := bank.UpsertProviderBenchmark(ProviderBenchmark{
		ProviderKind:     string(ProviderOllama),
		Model:            "llama3.2:3b",
		Compression:      "",
		TokensPerSec:     20.0,
		ElapsedMS:        2500,
		CompletionTokens: 50,
	}); err != nil {
		t.Fatalf("UpsertProviderBenchmark: %v", err)
	}

	// Seed three completed nightly runs. Per run: 100 in + 100 out for
	// Phase 0b, 400 augment tokens, total 1000 → 500 "other_phases".
	for i := 0; i < 3; i++ {
		stats := seedStatsJSON(t, 100, 100, 400, 1000)
		rec := NightlyRun{
			ID:         fmt.Sprintf("nightly-est-%d", i),
			StartedAt:  nowUTC(),
			FinishedAt: nowUTC(),
			Status:     "completed",
			StatsJSON:  stats,
			Model:      "ollama:llama3.2:3b",
			CreatedAt:  nowUTC(),
		}
		if err := bank.InsertNightlyRun(rec); err != nil {
			t.Fatalf("InsertNightlyRun: %v", err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/llm/estimate_nightly", nil)
	w := httptest.NewRecorder()
	srv.adminLLMRouter(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("populated estimate want 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Tier2Provider     string                 `json:"tier2_provider"`
		Tier2Model        string                 `json:"tier2_model"`
		Compression       string                 `json:"compression"`
		TokensPerSec      float64                `json:"tokens_per_sec"`
		BasedOnRuns       int                    `json:"based_on_runs"`
		AvgTokensPerRun   int                    `json:"avg_tokens_per_run"`
		EstimatedTotalSec float64                `json:"estimated_total_seconds"`
		EstimatedHuman    string                 `json:"estimated_human"`
		PerPhase          []NightlyEstimatePhase `json:"per_phase"`
		Caveats           []string               `json:"caveats"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Tier2Provider != string(ProviderOllama) || resp.Tier2Model != "llama3.2:3b" {
		t.Errorf("tier2 echo wrong: %s/%s", resp.Tier2Provider, resp.Tier2Model)
	}
	if resp.TokensPerSec != 20.0 {
		t.Errorf("tokens_per_sec: want 20, got %f", resp.TokensPerSec)
	}
	if resp.BasedOnRuns != 3 {
		t.Errorf("based_on_runs: want 3, got %d", resp.BasedOnRuns)
	}
	if resp.AvgTokensPerRun != 1000 {
		t.Errorf("avg_tokens_per_run: want 1000, got %d", resp.AvgTokensPerRun)
	}
	// 1000 tokens / 20 tok/s = 50 seconds.
	if math.Abs(resp.EstimatedTotalSec-50.0) > 0.01 {
		t.Errorf("estimated_total_seconds: want ~50, got %f", resp.EstimatedTotalSec)
	}
	if resp.EstimatedHuman == "" {
		t.Errorf("estimated_human should be populated")
	}
	// Per-phase breakdown: 0b_deep_encode=200, 6_augment=400, other_phases=400.
	phases := map[string]int{}
	for _, p := range resp.PerPhase {
		phases[p.Phase] = p.Tokens
	}
	if phases["0b_deep_encode"] != 200 {
		t.Errorf("phase 0b tokens: want 200, got %d", phases["0b_deep_encode"])
	}
	if phases["6_augment"] != 400 {
		t.Errorf("phase 6 tokens: want 400, got %d", phases["6_augment"])
	}
	if phases["other_phases"] != 400 {
		t.Errorf("other_phases tokens: want 400, got %d", phases["other_phases"])
	}
}

func TestAdminLLMEstimateNightly_BenchmarkMissingFlagsCaveat(t *testing.T) {
	bank := newTestBank(t)
	srv := newTestServer(t, bank)

	// Tier 2 configured but no benchmark cached for it.
	_ = bank.SaveProviderConfig(Tier2, ProviderConfig{Kind: ProviderOllama, Model: "llama3.2:3b"})
	_ = bank.InsertNightlyRun(NightlyRun{
		ID: "nightly-no-bench", StartedAt: nowUTC(), FinishedAt: nowUTC(),
		Status: "completed", StatsJSON: seedStatsJSON(t, 100, 100, 400, 1000),
		CreatedAt: nowUTC(),
	})

	req := httptest.NewRequest(http.MethodGet, "/admin/llm/estimate_nightly", nil)
	w := httptest.NewRecorder()
	srv.adminLLMRouter(w, req)

	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, body)
	}
	// Should still carry the "run benchmark first" caveat.
	var resp struct {
		Caveats           []string `json:"caveats"`
		EstimatedTotalSec float64  `json:"estimated_total_seconds"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.EstimatedTotalSec != 0 {
		t.Errorf("no benchmark → total seconds must be 0, got %f", resp.EstimatedTotalSec)
	}
	found := false
	for _, c := range resp.Caveats {
		if containsSubstr(c, "benchmark") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("missing benchmark caveat in %v", resp.Caveats)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// POST /admin/llm/benchmark/{tier} guards
// ──────────────────────────────────────────────────────────────────────────

func TestAdminLLMBenchmark_MethodGuard(t *testing.T) {
	bank := newTestBank(t)
	srv := newTestServer(t, bank)

	req := httptest.NewRequest(http.MethodGet, "/admin/llm/benchmark/tier2_provider", nil)
	w := httptest.NewRecorder()
	srv.adminLLMRouter(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET on /benchmark/{tier} should be 405, got %d", w.Code)
	}
}

func TestAdminLLMBenchmark_UnknownTier(t *testing.T) {
	bank := newTestBank(t)
	srv := newTestServer(t, bank)
	req := httptest.NewRequest(http.MethodPost, "/admin/llm/benchmark/tier9000", nil)
	w := httptest.NewRecorder()
	srv.adminLLMRouter(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("unknown tier should be 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestAdminLLMBenchmark_DisabledTierIsBadGateway(t *testing.T) {
	bank := newTestBank(t)
	srv := newTestServer(t, bank)
	// Tier 2 defaults to ProviderDisabled. POST should surface that as 502
	// with a "set kind first" hint rather than panic in BuildProvider.
	req := httptest.NewRequest(http.MethodPost, "/admin/llm/benchmark/tier2_provider", nil)
	w := httptest.NewRecorder()
	srv.adminLLMRouter(w, req)
	if w.Code != http.StatusBadGateway {
		t.Errorf("disabled tier POST: want 502, got %d body=%s", w.Code, w.Body.String())
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Router dispatch
// ──────────────────────────────────────────────────────────────────────────

func TestAdminLLMRouter_UnknownPathIs404(t *testing.T) {
	srv := newTestServer(t, newTestBank(t))
	req := httptest.NewRequest(http.MethodGet, "/admin/llm/nope", nil)
	w := httptest.NewRecorder()
	srv.adminLLMRouter(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown path should be 404, got %d", w.Code)
	}
}

func TestAdminLLMRouter_MissingTierIs400(t *testing.T) {
	srv := newTestServer(t, newTestBank(t))
	req := httptest.NewRequest(http.MethodPost, "/admin/llm/benchmark/", nil)
	w := httptest.NewRecorder()
	srv.adminLLMRouter(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing tier should be 400, got %d", w.Code)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Helper-function unit coverage
// ──────────────────────────────────────────────────────────────────────────

func TestHumaniseDuration(t *testing.T) {
	cases := []struct {
		secs float64
		want string
	}{
		{0, ""},
		{-1, ""},
		{30, "30s"},
		{60, "1min"},
		{60 * 23, "23min"},
		{3600, "1h"},
		{3600 + 60*12, "1h 12min"},
		{4*3600 + 12*60, "4h 12min"},
		{86400, "1d"},
		{86400 + 7200, "1d 2h"},
	}
	for _, c := range cases {
		if got := humaniseDuration(c.secs); got != c.want {
			t.Errorf("humaniseDuration(%v) = %q, want %q", c.secs, got, c.want)
		}
	}
}

func TestNullIfZero(t *testing.T) {
	if v := nullIfZero(0); v != nil {
		t.Errorf("nullIfZero(0) should be nil, got %v", v)
	}
	if v := nullIfZero(1.5); v == nil {
		t.Errorf("nullIfZero(1.5) should not be nil")
	}
}

func TestParseStatsJSON_BadInputReturnsFalse(t *testing.T) {
	if _, ok := parseStatsJSON("not json"); ok {
		t.Errorf("parseStatsJSON should return ok=false on garbage")
	}
	if _, ok := parseStatsJSON(`{"tokens_total":42}`); !ok {
		t.Errorf("parseStatsJSON should accept valid stats")
	}
}

// containsSubstr — tiny strings.Contains alias so this test file doesn't
// have to add a strings import just for one call.
func containsSubstr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
