// bundle_o_guards_test.go — guards for v2.7 Bundle O (procedural
// memories — directives that auto-inject into Tier 1 prompts).
//
// Coverage:
//   - ListActiveProcedures returns only memory_type="procedure" rows
//   - Soft-deleted procedures are excluded
//   - Dormant procedures are excluded (Bundle L interaction)
//   - Scope filter narrows by tag (case-insensitive)
//   - Limit is honoured + capped at 200
//   - ProcedurePromptPrefix renders a bullet list with the heading
//   - ProcedurePromptPrefix returns empty when no procedures exist
//   - Prefix respects the byte cap (drops procedures that don't fit)
//   - Enriched text wins over raw text when both are set
//   - BuildSystemPromptWithProcedures prepends correctly
//   - GET /procedures returns the list
//   - GET /procedures/prefix returns the rendered fragment
//   - Maybe-persist on /event with memory_type="procedure" lands as
//     a procedure row (sd_save_procedure end-to-end shape)
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ── ListActiveProcedures ────────────────────────────────────────────────

func TestBundleO_ListActiveProcedures_FiltersByType(t *testing.T) {
	bank := newTestBank(t)
	// One procedure + one episodic memory.
	bank.SaveMemory(MemoryRecord{Text: "prefer pnpm over npm", MemoryType: ProcedureMemoryType})
	bank.SaveMemory(MemoryRecord{Text: "I ran the build today", MemoryType: "episodic"})
	got, err := bank.ListActiveProcedures("", 0)
	if err != nil {
		t.Fatalf("ListActiveProcedures: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("expected 1 procedure, got %d: %v", len(got), got)
	}
	if got[0].MemoryType != ProcedureMemoryType {
		t.Errorf("expected memory_type=procedure, got %q", got[0].MemoryType)
	}
}

func TestBundleO_ListActiveProcedures_ExcludesDeletedAndDormant(t *testing.T) {
	bank := newTestBank(t)
	live, _ := bank.SaveMemory(MemoryRecord{Text: "live procedure", MemoryType: ProcedureMemoryType})
	deleted, _ := bank.SaveMemory(MemoryRecord{Text: "soft-deleted procedure", MemoryType: ProcedureMemoryType})
	dormant, _ := bank.SaveMemory(MemoryRecord{Text: "dormant procedure", MemoryType: ProcedureMemoryType})
	bank.SoftDeleteMemory(deleted.ID)
	bank.UpdateMemory(dormant.ID, MemoryUpdate{Dormant: ptrBool(true)})

	got, _ := bank.ListActiveProcedures("", 0)
	if len(got) != 1 || got[0].ID != live.ID {
		t.Errorf("expected only live procedure, got %v", got)
	}
}

func TestBundleO_ListActiveProcedures_ScopeFilter(t *testing.T) {
	bank := newTestBank(t)
	bank.SaveMemory(MemoryRecord{
		Text: "auth: always use OAuth2 with PKCE",
		Tags: []string{"procedure", "auth"},
		MemoryType: ProcedureMemoryType,
	})
	bank.SaveMemory(MemoryRecord{
		Text: "writing: avoid passive voice",
		Tags: []string{"procedure", "writing"},
		MemoryType: ProcedureMemoryType,
	})
	got, _ := bank.ListActiveProcedures("auth", 0)
	if len(got) != 1 {
		t.Errorf("expected 1 auth-scoped procedure, got %d", len(got))
	}
	// Case-insensitive.
	got2, _ := bank.ListActiveProcedures("AUTH", 0)
	if len(got2) != 1 {
		t.Errorf("case-insensitive scope filter expected 1, got %d", len(got2))
	}
}

func TestBundleO_ListActiveProcedures_LimitCap(t *testing.T) {
	bank := newTestBank(t)
	bank.SaveMemory(MemoryRecord{Text: "p1", MemoryType: ProcedureMemoryType})
	bank.SaveMemory(MemoryRecord{Text: "p2", MemoryType: ProcedureMemoryType})
	bank.SaveMemory(MemoryRecord{Text: "p3", MemoryType: ProcedureMemoryType})
	got, _ := bank.ListActiveProcedures("", 2)
	if len(got) != 2 {
		t.Errorf("limit=2 should return 2, got %d", len(got))
	}
	// Crazy-high limit gets clamped silently to 200 (we only have 3 — ensure no panic).
	got2, _ := bank.ListActiveProcedures("", 100000)
	if len(got2) != 3 {
		t.Errorf("over-cap limit should return all 3 rows, got %d", len(got2))
	}
}

// ── ProcedurePromptPrefix ───────────────────────────────────────────────

func TestBundleO_ProcedurePromptPrefix_RendersBullets(t *testing.T) {
	bank := newTestBank(t)
	bank.SaveMemory(MemoryRecord{Text: "prefer pnpm over npm", MemoryType: ProcedureMemoryType})
	bank.SaveMemory(MemoryRecord{Text: "quote file paths with spaces", MemoryType: ProcedureMemoryType})

	prefix, err := bank.ProcedurePromptPrefix("", 0)
	if err != nil {
		t.Fatalf("ProcedurePromptPrefix: %v", err)
	}
	if !strings.Contains(prefix, "Active procedures") {
		t.Errorf("expected heading, got %q", prefix)
	}
	if !strings.Contains(prefix, "- prefer pnpm over npm") {
		t.Errorf("expected pnpm bullet, got %q", prefix)
	}
	if !strings.Contains(prefix, "- quote file paths with spaces") {
		t.Errorf("expected paths bullet, got %q", prefix)
	}
}

func TestBundleO_ProcedurePromptPrefix_EmptyWhenNone(t *testing.T) {
	bank := newTestBank(t)
	bank.SaveMemory(MemoryRecord{Text: "regular episodic", MemoryType: "episodic"})
	prefix, _ := bank.ProcedurePromptPrefix("", 0)
	if prefix != "" {
		t.Errorf("expected empty prefix with no procedures, got %q", prefix)
	}
}

func TestBundleO_ProcedurePromptPrefix_RespectsBudget(t *testing.T) {
	bank := newTestBank(t)
	// Three procedures with predictable length.
	for i, p := range []string{"first procedure body", "second procedure body", "third procedure body"} {
		_ = i
		bank.SaveMemory(MemoryRecord{Text: p, MemoryType: ProcedureMemoryType})
	}
	// Cap small enough that only one bullet fits after the heading.
	prefix, _ := bank.ProcedurePromptPrefix("", 60)
	if strings.Count(prefix, "\n- ") > 1 {
		t.Errorf("budget cap=60 should fit at most 1 bullet, got %q", prefix)
	}
}

func TestBundleO_ProcedurePromptPrefix_EnrichedWinsOverRaw(t *testing.T) {
	bank := newTestBank(t)
	bank.SaveMemory(MemoryRecord{
		Text:         "raw text version",
		EnrichedText: "cleaner enriched version",
		MemoryType:   ProcedureMemoryType,
	})
	prefix, _ := bank.ProcedurePromptPrefix("", 0)
	if !strings.Contains(prefix, "cleaner enriched version") {
		t.Errorf("expected enriched text to win, got %q", prefix)
	}
	if strings.Contains(prefix, "raw text version") {
		t.Errorf("raw text should NOT appear when enriched is set, got %q", prefix)
	}
}

// ── BuildSystemPromptWithProcedures ─────────────────────────────────────

func TestBundleO_BuildSystemPrompt_Prepends(t *testing.T) {
	bank := newTestBank(t)
	bank.SaveMemory(MemoryRecord{Text: "be terse", MemoryType: ProcedureMemoryType})
	out := BuildSystemPromptWithProcedures(bank, "", "You are a helpful assistant.")
	if !strings.Contains(out, "- be terse") {
		t.Errorf("expected procedure prepended to base prompt; got %q", out)
	}
	if !strings.HasSuffix(out, "You are a helpful assistant.") {
		t.Errorf("base prompt should be at the end; got %q", out)
	}
}

func TestBundleO_BuildSystemPrompt_NoProceduresPassthrough(t *testing.T) {
	bank := newTestBank(t)
	const base = "You are a helpful assistant."
	out := BuildSystemPromptWithProcedures(bank, "", base)
	if out != base {
		t.Errorf("no procedures should pass base through unchanged; got %q", out)
	}
}

func TestBundleO_BuildSystemPrompt_NilBankPassthrough(t *testing.T) {
	const base = "You are a helpful assistant."
	out := BuildSystemPromptWithProcedures(nil, "", base)
	if out != base {
		t.Errorf("nil bank should pass base through unchanged; got %q", out)
	}
}

// ── HTTP /procedures ───────────────────────────────────────────────────

func TestBundleO_HandleProcedures_ListsActive(t *testing.T) {
	bank := newTestBank(t)
	bank.SaveMemory(MemoryRecord{Text: "always use UTC", MemoryType: ProcedureMemoryType})
	srv := newTestServer(t, bank)

	req := httptest.NewRequest(http.MethodGet, "/procedures", nil)
	w := httptest.NewRecorder()
	srv.handleProcedures(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /procedures want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if c, _ := resp["count"].(float64); c != 1 {
		t.Errorf("expected count=1, got %v", resp["count"])
	}
}

func TestBundleO_HandleProcedures_PrefixEndpoint(t *testing.T) {
	bank := newTestBank(t)
	bank.SaveMemory(MemoryRecord{Text: "be precise", MemoryType: ProcedureMemoryType})
	srv := newTestServer(t, bank)

	req := httptest.NewRequest(http.MethodGet, "/procedures/prefix", nil)
	w := httptest.NewRecorder()
	srv.handleProcedures(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /procedures/prefix want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	prefix, _ := resp["prefix"].(string)
	if !strings.Contains(prefix, "- be precise") {
		t.Errorf("expected prefix to contain bullet, got %q", prefix)
	}
}

// ── Hub.maybePersist now honours memory_type ───────────────────────────

func TestBundleO_HubPersistsMemoryType(t *testing.T) {
	// Simulates sd_save_procedure → POST /event with memory_type in
	// payload. The hub persists with the right memory_type so the
	// row surfaces in ListActiveProcedures.
	bank := newTestBank(t)
	hub := NewHub(NewRingBuffer(8))
	hub.AttachBank(bank)
	ev := Event{
		SchemaVersion: SchemaVersion,
		Type:          "memory_added",
		Timestamp:     "2026-05-13T00:00:00Z",
		AdapterID:     "mcp",
		Payload: map[string]any{
			"memory_id":   "proc-test-1",
			"text":        "always run tests before commit",
			"tags":        []any{"procedure"},
			"memory_type": "procedure",
		},
	}
	hub.Fanout(ev)

	// maybePersist runs in a goroutine; poll with backoff up to ~2s.
	var got MemoryRecord
	for i := 0; i < 40; i++ {
		got, _ = bank.GetMemory("proc-test-1")
		if got.MemoryType == ProcedureMemoryType {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got.MemoryType != ProcedureMemoryType {
		t.Errorf("expected memory_type=procedure after persistence, got %q", got.MemoryType)
	}
}
