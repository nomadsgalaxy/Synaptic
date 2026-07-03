package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// reflectTestServer wires a Server with an embedded Tier 1, a remote Tier 3
// mock (mockOracle from handlers_research_test.go), and an event hub so the
// reflect_done broadcast is observable via the ring buffer.
func reflectTestServer(t *testing.T, oracle *mockOracle) (*Server, *RingBuffer) {
	t.Helper()
	bank := newTestBank(t)
	tier1 := &fakeProvider{name: "tier1-local", local: true}
	router := NewModelRouter(tier1, nil, oracle, nil)
	ring := NewRingBuffer(64)
	hub := NewHub(ring)
	return &Server{bank: bank, router: router, hub: hub, ring: ring}, ring
}

// seedReflectFixture stores three memories with hand-crafted unit vectors
// arranged so mem_oauth ranks first, mem_pkce second, mem_sort last. Returns
// the records and the query vector (already cached so the embed call is a hit).
type reflectFixture struct {
	oauth  MemoryRecord
	pkce   MemoryRecord
	sort   MemoryRecord
	hidden MemoryRecord // sensitive=true; should be filtered before Tier 3
	model  string
}

func seedReflectFixture(t *testing.T, bank *Bank) reflectFixture {
	t.Helper()
	const dim = 64
	const model = "llama3.2:3b"

	oauth, err := bank.SaveMemory(MemoryRecord{Text: "OAuth token refresh flow", Tags: []string{"auth", "spec"}})
	if err != nil {
		t.Fatalf("save oauth: %v", err)
	}
	pkce, err := bank.SaveMemory(MemoryRecord{Text: "PKCE code challenge", Tags: []string{"auth", "pkce"}})
	if err != nil {
		t.Fatalf("save pkce: %v", err)
	}
	srt, err := bank.SaveMemory(MemoryRecord{Text: "Bubble sort algorithm", Tags: []string{"algorithms"}})
	if err != nil {
		t.Fatalf("save sort: %v", err)
	}
	hidden, err := bank.SaveMemory(MemoryRecord{
		Text:      "Personal OAuth client secret: shhh",
		Tags:      []string{"auth", "secret"},
		Sensitive: true,
	})
	if err != nil {
		t.Fatalf("save hidden: %v", err)
	}

	queryVec := makeUnitVec(dim, 1)
	// Strong alignment for the OAuth-flavoured records; near-orthogonal for sort.
	oauthVec := blendUnit(queryVec, makeUnitVec(dim, 10), 0.85)
	pkceVec := blendUnit(queryVec, makeUnitVec(dim, 20), 0.75)
	hiddenVec := blendUnit(queryVec, makeUnitVec(dim, 30), 0.80) // would otherwise outrank pkce
	sortVec := makeUnitVec(dim, 40)

	for _, m := range []struct {
		rec MemoryRecord
		vec []float32
	}{
		{oauth, oauthVec}, {pkce, pkceVec}, {srt, sortVec}, {hidden, hiddenVec},
	} {
		tagsAny := make([]interface{}, len(m.rec.Tags))
		for i, tag := range m.rec.Tags {
			tagsAny[i] = tag
		}
		embedText := synapseEmbedText(map[string]interface{}{
			"text": m.rec.Text,
			"tags": tagsAny,
		})
		if err := bank.SaveEmbedding(synapseTextHash(embedText), m.vec, model); err != nil {
			t.Fatalf("save embedding: %v", err)
		}
	}
	// Pre-cache the query embedding under its own hash so the handler doesn't
	// try to reach Ollama at 127.0.0.1:1.
	if err := bank.SaveEmbedding(synapseTextHash("how does OAuth work"), queryVec, model); err != nil {
		t.Fatalf("save query embed: %v", err)
	}

	return reflectFixture{oauth: oauth, pkce: pkce, sort: srt, hidden: hidden, model: model}
}

// postReflect routes through the full mux so any registration regressions
// surface in the test.
func postReflect(t *testing.T, server *Server, body interface{}) (int, map[string]interface{}, string) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/reflect", bytes.NewReader(raw))
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

func TestReflectPost_BasicSynthesis(t *testing.T) {
	t.Setenv("SD_OLLAMA_URL", "http://127.0.0.1:1")
	t.Setenv("SD_SYNAPSE_MODEL", "llama3.2:3b")

	oracle := &mockOracle{name: "oracle:gpt-4o-mini",
		respond: func(prompt string) (string, error) {
			return "SYNTHESIS: OAuth uses tokens; PKCE protects public clients.", nil
		}}
	server, ring := reflectTestServer(t, oracle)
	fix := seedReflectFixture(t, server.bank)

	code, resp, body := postReflect(t, server, map[string]interface{}{
		"query":  "how does OAuth work",
		"budget": "low", // top 5 → all 3 visible records (mem_hidden is sensitive)
	})
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}
	if oracle.calls.Load() != 1 {
		t.Fatalf("expected 1 oracle call, got %d", oracle.calls.Load())
	}

	synth, _ := resp["synthesis"].(string)
	if !strings.Contains(synth, "SYNTHESIS") {
		t.Errorf("synthesis should contain mock answer, got %q", synth)
	}
	if resp["model"] != "oracle:gpt-4o-mini" {
		t.Errorf("expected model=oracle:gpt-4o-mini, got %v", resp["model"])
	}

	// source_memory_ids should reference the safe (non-sensitive) records.
	idsRaw, _ := resp["source_memory_ids"].([]interface{})
	got := map[string]bool{}
	for _, v := range idsRaw {
		if s, ok := v.(string); ok {
			got[s] = true
		}
	}
	if !got[fix.oauth.ID] {
		t.Errorf("expected oauth memory in source_memory_ids, got %v", idsRaw)
	}
	if got[fix.hidden.ID] {
		t.Errorf("sensitive memory must NOT appear in source_memory_ids, got %v", idsRaw)
	}

	// sensitive_excluded should be exactly 1 (mem_hidden).
	if v, _ := resp["sensitive_excluded"].(float64); int(v) != 1 {
		t.Errorf("expected sensitive_excluded=1, got %v", resp["sensitive_excluded"])
	}
	// Canonical privacy field must match the disaggregated sum.
	if v, _ := resp["memories_excluded_by_privacy_filter"].(float64); int(v) != 1 {
		t.Errorf("expected memories_excluded_by_privacy_filter=1, got %v",
			resp["memories_excluded_by_privacy_filter"])
	}

	// Verify the prompt the oracle actually saw never contained the secret.
	rawPrompt, _ := oracle.lastPrompt.Load().(string)
	if strings.Contains(rawPrompt, "client secret") {
		t.Errorf("Tier 3 prompt leaked sensitive content: %q", rawPrompt)
	}

	// reflect_done should have landed in the ring buffer via Hub.Fanout.
	var sawReflectDone bool
	for _, ev := range ring.Recent(50) {
		if ev.Type == "reflect_done" {
			sawReflectDone = true
			if ev.Payload["model"] != "oracle:gpt-4o-mini" {
				t.Errorf("reflect_done.model = %v, want oracle:gpt-4o-mini", ev.Payload["model"])
			}
			if v, ok := ev.Payload["source_count"]; !ok || v == nil {
				t.Errorf("reflect_done missing source_count: %v", ev.Payload)
			}
		}
	}
	if !sawReflectDone {
		t.Fatal("reflect_done WS event was not broadcast")
	}

	// Token attribution: Tier 3 spend should be non-zero today.
	byTier, err := server.bank.ListTokenBudgetByTier(1, false)
	if err != nil {
		t.Fatalf("ListTokenBudgetByTier: %v", err)
	}
	t3 := byTier[string(Tier3)]
	if t3.TokensTotal == 0 {
		t.Errorf("expected non-zero Tier 3 spend in token_budget_lines, got %+v", byTier)
	}
}

func TestReflectPost_BudgetMapsToTopK(t *testing.T) {
	t.Setenv("SD_OLLAMA_URL", "http://127.0.0.1:1")
	t.Setenv("SD_SYNAPSE_MODEL", "llama3.2:3b")

	oracle := &mockOracle{name: "o"}
	server, _ := reflectTestServer(t, oracle)
	_ = seedReflectFixture(t, server.bank)

	// With only 3 safe records, "low" budget (top 5) should surface all 3.
	code, resp, body := postReflect(t, server, map[string]interface{}{
		"query":  "how does OAuth work",
		"budget": "low",
	})
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}
	if c, _ := resp["used_count"].(float64); int(c) != 3 {
		t.Errorf("low budget should pick up all 3 safe candidates, got used_count=%v", resp["used_count"])
	}

	// Override via Limit:1 — should clamp the result set.
	code, resp, body = postReflect(t, server, map[string]interface{}{
		"query": "how does OAuth work",
		"limit": 1,
	})
	if code != http.StatusOK {
		t.Fatalf("limit override: expected 200, got %d: %s", code, body)
	}
	if c, _ := resp["used_count"].(float64); int(c) != 1 {
		t.Errorf("limit=1 should clamp used_count to 1, got %v", resp["used_count"])
	}
}

func TestReflectPost_TagFilter(t *testing.T) {
	t.Setenv("SD_OLLAMA_URL", "http://127.0.0.1:1")
	t.Setenv("SD_SYNAPSE_MODEL", "llama3.2:3b")

	oracle := &mockOracle{name: "o"}
	server, _ := reflectTestServer(t, oracle)
	fix := seedReflectFixture(t, server.bank)

	// "pkce" tag exists only on the pkce record (and not on oauth/sort/hidden);
	// asking for tags=[pkce] mode=all should leave exactly that one survivor.
	code, resp, body := postReflect(t, server, map[string]interface{}{
		"query":      "how does OAuth work",
		"tags":       []string{"pkce"},
		"tags_match": "all",
	})
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}
	if c, _ := resp["used_count"].(float64); int(c) != 1 {
		t.Errorf("expected used_count=1 after pkce-tag filter, got %v", resp["used_count"])
	}
	ids, _ := resp["source_memory_ids"].([]interface{})
	if len(ids) != 1 || ids[0] != fix.pkce.ID {
		t.Errorf("expected only pkce memory survives the filter, got %v", ids)
	}
}

func TestReflectPost_TagsMatchAll_NoOverlap_Returns400(t *testing.T) {
	t.Setenv("SD_OLLAMA_URL", "http://127.0.0.1:1")
	t.Setenv("SD_SYNAPSE_MODEL", "llama3.2:3b")

	oracle := &mockOracle{name: "o"}
	server, _ := reflectTestServer(t, oracle)
	_ = seedReflectFixture(t, server.bank)

	code, _, body := postReflect(t, server, map[string]interface{}{
		"query":      "how does OAuth work",
		"tags":       []string{"does-not-exist"},
		"tags_match": "all",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 when tag filter matches nothing, got %d: %s", code, body)
	}
	if oracle.calls.Load() != 0 {
		t.Errorf("Tier 3 should not be called when no candidates match, got %d calls", oracle.calls.Load())
	}
}

func TestReflectPost_Tier3NotConfigured(t *testing.T) {
	t.Setenv("SD_OLLAMA_URL", "http://127.0.0.1:1")
	t.Setenv("SD_SYNAPSE_MODEL", "llama3.2:3b")

	bank := newTestBank(t)
	server := &Server{bank: bank, router: NewModelRouter(&fakeProvider{name: "t1", local: true}, nil, nil, nil)}

	code, _, body := postReflect(t, server, map[string]string{"query": "test"})
	if code != http.StatusForbidden {
		t.Fatalf("expected 403 when Tier 3 unconfigured, got %d: %s", code, body)
	}
}

func TestReflectPost_EmptyQuery(t *testing.T) {
	oracle := &mockOracle{name: "o"}
	server, _ := reflectTestServer(t, oracle)
	code, _, body := postReflect(t, server, map[string]string{"query": ""})
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 on empty query, got %d: %s", code, body)
	}
}

func TestReflectPost_BankDisabled(t *testing.T) {
	server := &Server{bank: nil}
	code, _, body := postReflect(t, server, map[string]string{"query": "test"})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when bank disabled, got %d: %s", code, body)
	}
}

// TestReflectPost_PrivacyProbe is the named acceptance test for the standing
// privacy rule: a sensitive memory present in the recall result set is NEVER
// included in the Tier 3 prompt, its ID never appears in source_memory_ids,
// and the response's `memories_excluded_by_privacy_filter` field reflects
// the count of suppressed rows. Anything else is a beta-blocker.
//
// The probe uses a memorable secret token ("MERCURIUS_ECLIPSE_42") that
// cannot appear in mock-oracle output unless the sensitive memory leaked
// into the prompt. We assert against the captured `lastPrompt` (what the
// oracle actually received), not the synthesis — the prompt is the
// load-bearing surface; the synthesis is just confirmation the call ran.
func TestReflectPost_PrivacyProbe(t *testing.T) {
	t.Setenv("SD_OLLAMA_URL", "http://127.0.0.1:1")
	t.Setenv("SD_SYNAPSE_MODEL", "llama3.2:3b")

	oracle := &mockOracle{name: "oracle:gpt-4o-mini",
		respond: func(_ string) (string, error) { return "synthesis ok", nil }}
	server, _ := reflectTestServer(t, oracle)
	fix := seedReflectFixture(t, server.bank)

	// Add a SECOND sensitive memory carrying a uniquely-recognisable token.
	// Aligned to the query vector so it would otherwise rank in the top-K.
	const secretToken = "MERCURIUS_ECLIPSE_42"
	probe, err := server.bank.SaveMemory(MemoryRecord{
		Text:      "Private operations log mentioning " + secretToken,
		Tags:      []string{"private"},
		Sensitive: true,
	})
	if err != nil {
		t.Fatalf("save probe memory: %v", err)
	}
	const model = "llama3.2:3b"
	probeVec := blendUnit(makeUnitVec(64, 1), makeUnitVec(64, 99), 0.95)
	tagsAny := []interface{}{"private"}
	probeEmbed := synapseEmbedText(map[string]interface{}{
		"text": probe.Text,
		"tags": tagsAny,
	})
	if err := server.bank.SaveEmbedding(synapseTextHash(probeEmbed), probeVec, model); err != nil {
		t.Fatalf("save probe embedding: %v", err)
	}

	code, resp, body := postReflect(t, server, map[string]interface{}{
		"query":  "how does OAuth work",
		"budget": "high", // top 30 — would absorb everything if the filter is absent
	})
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}

	// 1. The secret token must not appear in the prompt the oracle saw.
	rawPrompt, _ := oracle.lastPrompt.Load().(string)
	if strings.Contains(rawPrompt, secretToken) {
		t.Fatalf("PRIVACY VIOLATION — sensitive memory text leaked into Tier 3 prompt: %q", rawPrompt)
	}

	// 2. The probe's ID must not appear in source_memory_ids (otherwise the
	//    frontend would expose to the user which sensitive row was suppressed).
	idsRaw, _ := resp["source_memory_ids"].([]interface{})
	for _, v := range idsRaw {
		s, _ := v.(string)
		if s == probe.ID {
			t.Fatalf("PRIVACY VIOLATION — sensitive memory ID surfaced in source_memory_ids: %s", probe.ID)
		}
		if s == fix.hidden.ID {
			t.Fatalf("PRIVACY VIOLATION — fixture's sensitive memory ID surfaced: %s", fix.hidden.ID)
		}
	}

	// 3. memories_excluded_by_privacy_filter must be at least 2 (probe + fixture.hidden).
	excluded, _ := resp["memories_excluded_by_privacy_filter"].(float64)
	if int(excluded) < 2 {
		t.Errorf("memories_excluded_by_privacy_filter should be >= 2 (probe + hidden fixture), got %v",
			resp["memories_excluded_by_privacy_filter"])
	}

	// 4. The legacy disaggregated counts must agree with the canonical sum.
	sens, _ := resp["sensitive_excluded"].(float64)
	local, _ := resp["local_concept_excluded"].(float64)
	if int(sens+local) != int(excluded) {
		t.Errorf("disaggregated counts (%v + %v) must sum to canonical privacy count (%v)",
			sens, local, excluded)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Unit tests for the pure helpers (no DB, no router).

func TestReflectTagsMatch(t *testing.T) {
	cases := []struct {
		name      string
		memTags   []string
		wantTags  []string
		mode      string
		expect    bool
	}{
		{"empty filter passes", []string{"a"}, nil, "any", true},
		{"any: one hit", []string{"a", "b"}, []string{"b", "c"}, "any", true},
		{"any: no hit", []string{"x"}, []string{"a", "b"}, "any", false},
		{"all: full overlap", []string{"a", "b", "c"}, []string{"a", "b"}, "all", true},
		{"all: missing one", []string{"a"}, []string{"a", "b"}, "all", false},
		{"any case-insensitive", []string{"Auth"}, []string{"auth"}, "any", true},
		{"any_strict case-sensitive", []string{"Auth"}, []string{"auth"}, "any_strict", false},
		{"all_strict matches exact", []string{"Auth"}, []string{"Auth"}, "all_strict", true},
		{"default mode = any", []string{"a"}, []string{"a"}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reflectTagsMatch(tc.memTags, tc.wantTags, tc.mode); got != tc.expect {
				t.Errorf("reflectTagsMatch(%v, %v, %q) = %v, want %v",
					tc.memTags, tc.wantTags, tc.mode, got, tc.expect)
			}
		})
	}
}

func TestReflectBudgetForLabel(t *testing.T) {
	cases := []struct {
		in       string
		topK     int
		maxToks  int
		label    string
	}{
		{"low", 5, 512, "low"},
		{"mid", 15, 1024, "mid"},
		{"high", 30, 2048, "high"},
		{"", 15, 1024, "mid"},          // default
		{"garbage", 15, 1024, "mid"},   // unknown → mid
		{" HIGH ", 30, 2048, "high"},   // trim + lower
	}
	for _, tc := range cases {
		b := reflectBudgetForLabel(tc.in)
		if b.TopK != tc.topK || b.MaxTokens != tc.maxToks || b.Label != tc.label {
			t.Errorf("budget(%q) = %+v, want top=%d max=%d label=%s",
				tc.in, b, tc.topK, tc.maxToks, tc.label)
		}
	}
}
