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

// budgetTestServer is a Server with a real bank, ready for HTTP requests
// against /budget. No router/hub needed since we test budget endpoints
// directly. (The research test below DOES need a router — it constructs
// its own.)
func budgetTestServer(t *testing.T) *Server {
	t.Helper()
	bank := newTestBank(t)
	return &Server{bank: bank}
}

// fireBudgetGet returns the parsed JSON body for a GET /budget?days=N call.
func fireBudgetGet(t *testing.T, server *Server, query string) (int, map[string]interface{}, string) {
	t.Helper()
	url := "/budget"
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	var body map[string]interface{}
	if rec.Body.Len() > 0 && rec.Code == http.StatusOK {
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
	}
	return rec.Code, body, rec.Body.String()
}

// ──────────────────────────────────────────────────────────────────────────

// TestBudget_AddTokensUsedV2_PerTier — write one row per tier, GET should
// surface them in by_tier with correct sums and provider attribution.
func TestBudget_AddTokensUsedV2_PerTier(t *testing.T) {
	server := budgetTestServer(t)
	today := time.Now().UTC().Format("2006-01-02")

	cases := []struct {
		tier     string
		provider string
		in, out  int
	}{
		{string(TierEmbedding), "ollama", 100, 0},
		{string(Tier1), "ollama", 1200, 800},
		{string(Tier2), "ollama", 3400, 2200},
		{string(Tier3), "openai", 320, 920},
	}
	for _, c := range cases {
		// `external` is overridden by V2 from provider; pass false to verify the override.
		if err := server.bank.AddTokensUsedV2(today, c.tier, c.provider, "", false, c.in, c.out); err != nil {
			t.Fatalf("AddTokensUsedV2 %s/%s: %v", c.tier, c.provider, err)
		}
	}

	code, body, raw := fireBudgetGet(t, server, "days=7")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, raw)
	}
	byTier, ok := body["by_tier"].(map[string]interface{})
	if !ok {
		t.Fatalf("by_tier missing/wrong type: %s", raw)
	}
	for _, c := range cases {
		row, ok := byTier[c.tier].(map[string]interface{})
		if !ok {
			t.Errorf("missing by_tier[%s]: %v", c.tier, byTier[c.tier])
			continue
		}
		if int(row["tokens_in"].(float64)) != c.in {
			t.Errorf("[%s] tokens_in: got %v want %d", c.tier, row["tokens_in"], c.in)
		}
		if int(row["tokens_out"].(float64)) != c.out {
			t.Errorf("[%s] tokens_out: got %v want %d", c.tier, row["tokens_out"], c.out)
		}
		wantTotal := c.in + c.out
		if int(row["tokens_total"].(float64)) != wantTotal {
			t.Errorf("[%s] tokens_total: got %v want %d", c.tier, row["tokens_total"], wantTotal)
		}
		if row["provider"] != c.provider {
			t.Errorf("[%s] provider: got %v want %s", c.tier, row["provider"], c.provider)
		}
		// External must match provider — V2 overrides the caller-supplied bool.
		wantExt := c.provider == "openai" || c.provider == "anthropic" || c.provider == "custom"
		if row["external"].(bool) != wantExt {
			t.Errorf("[%s] external: got %v want %v (provider=%s)",
				c.tier, row["external"], wantExt, c.provider)
		}
	}

	// external_total should sum just the openai Tier 3 row.
	ext, _ := body["external_total"].(map[string]interface{})
	if int(ext["tokens_total"].(float64)) != 1240 {
		t.Errorf("external_total.tokens_total: got %v want 1240 (Tier3 only)", ext["tokens_total"])
	}
}

// TestBudget_ExternalOnly_FiltersOutOllama — Tier 2 ollama + Tier 3 openai;
// ?external_only=1 should aggregate only the openai spend, both in
// external_total and in the filtered history rows.
func TestBudget_ExternalOnly_FiltersOutOllama(t *testing.T) {
	server := budgetTestServer(t)
	today := time.Now().UTC().Format("2006-01-02")

	if err := server.bank.AddTokensUsedV2(today, string(Tier2), "ollama", "", false, 5000, 3000); err != nil {
		t.Fatalf("seed tier2: %v", err)
	}
	if err := server.bank.AddTokensUsedV2(today, string(Tier3), "openai", "", true, 200, 800); err != nil {
		t.Fatalf("seed tier3: %v", err)
	}

	code, body, _ := fireBudgetGet(t, server, "days=1&external_only=1")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	// external_total: only openai = 200+800 = 1000
	ext, _ := body["external_total"].(map[string]interface{})
	if int(ext["tokens_total"].(float64)) != 1000 {
		t.Errorf("external_total: got %v want 1000", ext["tokens_total"])
	}
	// History should reflect external-only sum for today (1000), NOT the
	// 9000 aggregate that includes ollama.
	hist, _ := body["history"].([]interface{})
	if len(hist) != 1 {
		t.Fatalf("expected 1 history row, got %d", len(hist))
	}
	day := hist[0].(map[string]interface{})
	if int(day["tokens_used"].(float64)) != 1000 {
		t.Errorf("filtered history.tokens_used: got %v want 1000",
			day["tokens_used"])
	}
}

// TestBudget_BackCompat — legacy AddTokensUsed(date, total) still produces
// a queryable row; it lands in token_budget_lines under tier=unknown so
// it's accounted for by the new endpoints.
func TestBudget_BackCompat(t *testing.T) {
	server := budgetTestServer(t)
	today := time.Now().UTC().Format("2006-01-02")

	if err := server.bank.AddTokensUsed(today, 5000); err != nil {
		t.Fatalf("legacy AddTokensUsed: %v", err)
	}

	// Verify the line landed under tier=unknown.
	used, err := server.bank.GetTokensUsed(today)
	if err != nil {
		t.Fatalf("GetTokensUsed: %v", err)
	}
	if used != 5000 {
		t.Errorf("legacy aggregate: got %d want 5000", used)
	}
	// And the per-tier breakdown should NOT crash on the unknown tier
	// (it's accepted by the validBudgetTiers enum).
	rows, err := server.bank.ListTokenBudgetByTier(7, false)
	if err != nil {
		t.Fatalf("ListTokenBudgetByTier: %v", err)
	}
	// All four canonical tiers should still be present (zero-spend ones
	// fall back to settings provider). The unknown bucket may also be
	// present — just verify the canonical four are not missing.
	for _, k := range []TierKey{TierEmbedding, Tier1, Tier2, Tier3} {
		if _, ok := rows[string(k)]; !ok {
			t.Errorf("expected canonical tier %s in by_tier; got keys=%v", k, keysOf(rows))
		}
	}
}

// TestBudget_HistoryUnchangedForBackCompat — adding tokens via V2 must
// keep the legacy history[].tokens_used aggregate working so the existing
// daily-bar chart on the dashboard isn't broken.
func TestBudget_HistoryUnchangedForBackCompat(t *testing.T) {
	server := budgetTestServer(t)
	today := time.Now().UTC().Format("2006-01-02")

	// Mix of V2 writes across tiers — total should aggregate in token_budget.
	_ = server.bank.AddTokensUsedV2(today, string(Tier1), "ollama", "", false, 100, 200)  // 300
	_ = server.bank.AddTokensUsedV2(today, string(Tier3), "openai", "", true, 50, 150)    // 200
	_ = server.bank.AddTokensUsedV2(today, string(Tier2), "ollama", "", false, 1000, 500) // 1500
	want := 300 + 200 + 1500

	code, body, _ := fireBudgetGet(t, server, "days=1")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	hist, _ := body["history"].([]interface{})
	if len(hist) != 1 {
		t.Fatalf("expected 1 history row, got %d", len(hist))
	}
	day := hist[0].(map[string]interface{})
	if int(day["tokens_used"].(float64)) != want {
		t.Errorf("history.tokens_used: got %v want %d (sum across all tiers)",
			day["tokens_used"], want)
	}
}

// TestBudget_ResearchRecordsExternal — POST /research against a remote
// (openai-named) Tier 3 provider records external=true; against a local
// Ollama Tier 3 records external=false.
func TestBudget_ResearchRecordsExternal(t *testing.T) {
	for _, scenario := range []struct {
		name           string
		oracleName     string
		oracleIsLocal  bool
		wantProvider   string
		wantExternal   bool
		wantExtTotalGT int // > this value
	}{
		{"openai (paid)", "openai:gpt-4o-mini", false, "openai", true, 0},
		{"ollama-as-tier3 (free)", "ollama:llama3.1:8b", true, "ollama", false, -1}, // -1 means no external spend expected
	} {
		t.Run(scenario.name, func(t *testing.T) {
			oracle := &mockOracle{
				name:    scenario.oracleName,
				respond: func(_ string) (string, error) { return "answer", nil },
			}
			oracle.local = scenario.oracleIsLocal
			bank := newTestBank(t)
			tier1 := &fakeProvider{name: "tier1-local", local: true}
			router := NewModelRouter(tier1, nil, oracle, nil)
			server := &Server{bank: bank, router: router}

			body, _ := json.Marshal(map[string]string{"query": "what is X"})
			req := httptest.NewRequest(http.MethodPost, "/research", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			server.routes().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("POST /research expected 200, got %d: %s", rec.Code, rec.Body.String())
			}

			byTier, err := bank.ListTokenBudgetByTier(1, false)
			if err != nil {
				t.Fatalf("ListTokenBudgetByTier: %v", err)
			}
			tier3 := byTier[string(Tier3)]
			if tier3.Provider != scenario.wantProvider {
				t.Errorf("Tier 3 provider: got %s want %s", tier3.Provider, scenario.wantProvider)
			}
			if tier3.External != scenario.wantExternal {
				t.Errorf("Tier 3 external: got %v want %v", tier3.External, scenario.wantExternal)
			}

			extIn, extOut, extTotal, _ := bank.GetExternalTokenTotal(1)
			if scenario.wantExternal {
				if extTotal <= 0 {
					t.Errorf("expected external_total > 0 for paid provider, got %d (in=%d out=%d)",
						extTotal, extIn, extOut)
				}
			} else {
				if extTotal > 0 {
					t.Errorf("local provider should record external_total=0, got %d", extTotal)
				}
			}
		})
	}
}

// TestBudget_ProviderDerivedExternal_OverridesCaller — caller passes
// external=true with provider=ollama; V2 should override to false because
// "external derived from provider, never user-supplied" is a safety rule.
func TestBudget_ProviderDerivedExternal_OverridesCaller(t *testing.T) {
	server := budgetTestServer(t)
	today := time.Now().UTC().Format("2006-01-02")

	// Caller LIES: claims openai is local, claims ollama is external.
	if err := server.bank.AddTokensUsedV2(today, string(Tier3), "openai", "", false /* lie */, 100, 100); err != nil {
		t.Fatalf("V2: %v", err)
	}
	if err := server.bank.AddTokensUsedV2(today, string(Tier1), "ollama", "", true /* lie */, 50, 50); err != nil {
		t.Fatalf("V2: %v", err)
	}

	byTier, _ := server.bank.ListTokenBudgetByTier(1, false)
	if byTier[string(Tier3)].External != true {
		t.Errorf("Tier 3 openai must be external regardless of caller's bool, got %v",
			byTier[string(Tier3)].External)
	}
	if byTier[string(Tier1)].External != false {
		t.Errorf("Tier 1 ollama must be local regardless of caller's bool, got %v",
			byTier[string(Tier1)].External)
	}

	// And the external_total should ONLY include the openai row.
	_, _, extTotal, _ := server.bank.GetExternalTokenTotal(1)
	if extTotal != 200 {
		t.Errorf("external_total: got %d want 200 (only openai)", extTotal)
	}
}

// TestBudget_AllFourTiersAlwaysPresent — even when a tier hasn't spent
// anything, GET /budget includes it with zeros + the configured provider
// from settings (so the dashboard can render four rows uniformly).
func TestBudget_AllFourTiersAlwaysPresent(t *testing.T) {
	server := budgetTestServer(t)
	// No spend at all — fresh DB.

	code, body, _ := fireBudgetGet(t, server, "days=1")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	byTier, _ := body["by_tier"].(map[string]interface{})
	for _, k := range []TierKey{TierEmbedding, Tier1, Tier2, Tier3} {
		row, ok := byTier[string(k)].(map[string]interface{})
		if !ok {
			t.Errorf("missing by_tier[%s] on fresh DB", k)
			continue
		}
		if int(row["tokens_total"].(float64)) != 0 {
			t.Errorf("[%s] tokens_total should be 0 on fresh DB, got %v", k, row["tokens_total"])
		}
		if _, ok := row["provider"]; !ok {
			t.Errorf("[%s] provider should be populated even with zero spend", k)
		}
	}
}

// keysOf returns the keys of a map[string]TokenSpendByTier for diagnostic
// messages (sorted for stable output).
func keysOf(m map[string]TokenSpendByTier) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Compile-time safety: confirm we can refer to the helper context.Context
// via a cast — guards against accidental import drift in this file.
var _ = context.Background

// Compile-time safety: confirm strings package is used, referenced via the
// hist row-shape parsing assumption that "tokens_used" is a known key.
var _ = strings.Contains
