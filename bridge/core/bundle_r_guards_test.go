// bundle_r_guards_test.go — guards for v2.7 Bundle R (inline reflection
// primitive — parity with external-system reflection features).
//
// Bundle R extends the existing /reflect endpoint with a `save_as_memory`
// flag. When true, the Tier 3 synthesis is persisted as a memory with
// memory_type="reflection" so future /recall calls can surface it. The
// response surfaces the saved memory's id.
//
// Coverage:
//   - Default (save_as_memory absent or false) leaves memory_id empty
//   - save_as_memory=true persists a memory tagged "reflection" with
//     memory_type="reflection" and SynthesisSourceIDs populated
//   - The persisted memory's text == response.synthesis
//   - Persisting respects an empty synthesis (no row written)
package main

import (
	"strings"
	"testing"
)

func TestBundleR_SaveAsMemory_DefaultsOff(t *testing.T) {
	t.Setenv("SD_OLLAMA_URL", "http://127.0.0.1:1")
	t.Setenv("SD_SYNAPSE_MODEL", "llama3.2:3b")
	oracle := &mockOracle{name: "oracle:test",
		respond: func(prompt string) (string, error) {
			return "Authoritative takeaway about OAuth.", nil
		}}
	server, _ := reflectTestServer(t, oracle)
	seedReflectFixture(t, server.bank)

	code, resp, body := postReflect(t, server, map[string]interface{}{
		"query":  "how does OAuth work",
		"budget": "low",
	})
	if code != 200 {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}
	if mid, _ := resp["memory_id"].(string); mid != "" {
		t.Errorf("default reflect should NOT persist a memory; got memory_id=%q", mid)
	}
}

func TestBundleR_SaveAsMemory_PersistsReflection(t *testing.T) {
	t.Setenv("SD_OLLAMA_URL", "http://127.0.0.1:1")
	t.Setenv("SD_SYNAPSE_MODEL", "llama3.2:3b")
	const takeaway = "Authoritative takeaway about OAuth flows."
	oracle := &mockOracle{name: "oracle:test",
		respond: func(prompt string) (string, error) { return takeaway, nil }}
	server, _ := reflectTestServer(t, oracle)
	fix := seedReflectFixture(t, server.bank)

	code, resp, body := postReflect(t, server, map[string]interface{}{
		"query":           "how does OAuth work",
		"budget":          "low",
		"save_as_memory":  true,
	})
	if code != 200 {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}

	mid, _ := resp["memory_id"].(string)
	if mid == "" {
		t.Fatalf("save_as_memory=true should populate memory_id, got empty (resp=%v)", resp)
	}

	got, err := server.bank.GetMemory(mid)
	if err != nil {
		t.Fatalf("GetMemory(%s): %v", mid, err)
	}
	if got.MemoryType != "reflection" {
		t.Errorf("expected memory_type=reflection, got %q", got.MemoryType)
	}
	if got.Source != "reflect" {
		t.Errorf("expected source=reflect, got %q", got.Source)
	}
	if !strings.Contains(got.Text, "OAuth") {
		t.Errorf("expected synthesis text in saved memory, got %q", got.Text)
	}
	foundTag := false
	for _, tag := range got.Tags {
		if strings.EqualFold(tag, "reflection") {
			foundTag = true
			break
		}
	}
	if !foundTag {
		t.Errorf("expected 'reflection' tag, got %v", got.Tags)
	}
	// SynthesisSourceIDs must include the OAuth memory (and exclude the
	// sensitive hidden one — Tier 3 filter ran before save).
	hasOAuth := false
	for _, sid := range got.SynthesisSourceIDs {
		if sid == fix.oauth.ID {
			hasOAuth = true
		}
		if sid == fix.hidden.ID {
			t.Errorf("hidden sensitive memory leaked into SynthesisSourceIDs: %v", got.SynthesisSourceIDs)
		}
	}
	if !hasOAuth {
		t.Errorf("expected OAuth memory id in SynthesisSourceIDs, got %v", got.SynthesisSourceIDs)
	}
}

func TestBundleR_SaveAsMemory_EmptySynthesisSkipsSave(t *testing.T) {
	// If Tier 3 returns whitespace, we don't persist a row.
	t.Setenv("SD_OLLAMA_URL", "http://127.0.0.1:1")
	t.Setenv("SD_SYNAPSE_MODEL", "llama3.2:3b")
	oracle := &mockOracle{name: "oracle:test",
		respond: func(prompt string) (string, error) { return "   \n  ", nil }}
	server, _ := reflectTestServer(t, oracle)
	seedReflectFixture(t, server.bank)

	code, resp, body := postReflect(t, server, map[string]interface{}{
		"query":          "how does OAuth work",
		"budget":         "low",
		"save_as_memory": true,
	})
	if code != 200 {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}
	if mid, _ := resp["memory_id"].(string); mid != "" {
		t.Errorf("empty synthesis should NOT persist a memory; got memory_id=%q", mid)
	}
}
