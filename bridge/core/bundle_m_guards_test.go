// bundle_m_guards_test.go — guards for v2.7 Bundle M (agent-callable
// mutation MCP tools). The new tools (sd_forget, sd_supersede,
// sd_set_dormant, sd_set_ttl) are thin JS wrappers in
// `Dev/bridge/mcp-adapter/index.js` that route to existing backend
// HTTP endpoints. These tests pin the backend contract those wrappers
// depend on:
//
//   - DELETE /bank/memories/{id}?reason=X writes the reason into the
//     audit row (sd_forget)
//   - POST /bank/memories/{id}/supersede with body {superseded_id, reason}
//     creates a supersede edge AND writes the audit row (sd_supersede)
//   - PATCH /bank/memories/{id} accepts dormant + expires_at JSON keys
//     and round-trips them (sd_set_dormant, sd_set_ttl) — covered by
//     bundle_l_guards_test.go at the bank level; here we exercise the
//     HTTP boundary
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ── sd_forget: DELETE /bank/memories/{id}?reason=… ─────────────────────

func TestBundleM_Forget_RecordsReasonInAudit(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "to be forgotten"})
	srv := newTestServer(t, bank)

	req := httptest.NewRequest(http.MethodDelete,
		"/bank/memories/"+rec.ID+"?reason=superseded+by+newer+guidance", nil)
	w := httptest.NewRecorder()
	srv.bankByIDExtended(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE want 204, got %d: %s", w.Code, w.Body.String())
	}

	entries, err := bank.ListAuditLog(AuditFilter{Operation: "soft_delete", Limit: 5})
	if err != nil {
		t.Fatalf("ListAuditLog: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.EntityID == rec.ID && e.Reason == "superseded by newer guidance" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected audit row with reason='superseded by newer guidance' for %s, got %+v",
			rec.ID, entries)
	}
}

func TestBundleM_Forget_SoftDeleteIsRestorable(t *testing.T) {
	// Defends the sd_forget UX contract: the memory must still exist as a
	// soft-deleted row that sd_restore_memory can wake up.
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "soft-deleted, not hard"})
	srv := newTestServer(t, bank)

	req := httptest.NewRequest(http.MethodDelete,
		"/bank/memories/"+rec.ID+"?reason=test", nil)
	srv.bankByIDExtended(httptest.NewRecorder(), req)

	got, err := bank.GetMemory(rec.ID)
	if err != nil {
		t.Fatalf("GetMemory after sd_forget: %v", err)
	}
	if got.DeletedAt == "" {
		t.Errorf("expected deleted_at set, got empty (was it hard-deleted?)")
	}
}

// ── sd_supersede: POST /bank/memories/{id}/supersede ──────────────────

func TestBundleM_Supersede_CreatesEdgeAndAudit(t *testing.T) {
	bank := newTestBank(t)
	older, _ := bank.SaveMemory(MemoryRecord{Text: "old fact: address is 123 Main St"})
	newer, _ := bank.SaveMemory(MemoryRecord{Text: "new fact: address is 456 Oak Ave"})
	srv := newTestServer(t, bank)

	body := map[string]string{
		"superseded_id": older.ID,
		"reason":        "user moved",
	}
	jb, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost,
		"/bank/memories/"+newer.ID+"/supersede", bytes.NewReader(jb))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.bankByIDExtended(w, req)

	if w.Code < 200 || w.Code >= 300 {
		t.Fatalf("supersede POST want 2xx, got %d: %s", w.Code, w.Body.String())
	}

	// Edge exists from newer → older.
	edges, err := bank.SupersedeChainFor(newer.ID)
	if err != nil {
		t.Fatalf("SupersedeChainFor: %v", err)
	}
	found := false
	for _, e := range edges {
		if e.SupersedingID == newer.ID && e.SupersededID == older.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected supersede edge %s → %s, got %+v", newer.ID, older.ID, edges)
	}

	// Audit row written.
	auds, _ := bank.ListAuditLog(AuditFilter{Operation: "memory_superseded", Limit: 5})
	if len(auds) == 0 {
		t.Errorf("expected memory_superseded audit row, found none")
	}
}

func TestBundleM_Supersede_RejectsSelfReference(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "only memory"})
	srv := newTestServer(t, bank)

	body := map[string]string{"superseded_id": rec.ID}
	jb, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost,
		"/bank/memories/"+rec.ID+"/supersede", bytes.NewReader(jb))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.bankByIDExtended(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("self-reference want 400, got %d: %s", w.Code, w.Body.String())
	}
}

// ── sd_set_dormant: PATCH .../{id} with {"dormant": true} ─────────────

func TestBundleM_PatchDormant_RoundtripsOverHTTP(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "sleepable"})
	srv := newTestServer(t, bank)

	jb, _ := json.Marshal(map[string]any{"dormant": true})
	req := httptest.NewRequest(http.MethodPatch,
		"/bank/memories/"+rec.ID, bytes.NewReader(jb))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.bankByIDExtended(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH dormant want 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp MemoryRecord
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.DormantAt == "" {
		t.Errorf("expected dormant_at populated in response, got empty")
	}
	if resp.DormantReason != "manual" {
		t.Errorf("expected dormant_reason=manual, got %q", resp.DormantReason)
	}
}

// ── sd_set_ttl: PATCH .../{id} with {"expires_at": "..."} ─────────────

func TestBundleM_PatchTTL_RoundtripsOverHTTP(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "ages out"})
	srv := newTestServer(t, bank)

	jb, _ := json.Marshal(map[string]any{"expires_at": "2027-01-01T00:00:00Z"})
	req := httptest.NewRequest(http.MethodPatch,
		"/bank/memories/"+rec.ID, bytes.NewReader(jb))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.bankByIDExtended(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH expires_at want 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp MemoryRecord
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.ExpiresAt != "2027-01-01T00:00:00Z" {
		t.Errorf("expected expires_at roundtrip, got %q", resp.ExpiresAt)
	}
}

func TestBundleM_PatchTTL_RejectsOnSensitive(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "secret", Sensitive: true})
	srv := newTestServer(t, bank)

	jb, _ := json.Marshal(map[string]any{"expires_at": "2027-01-01T00:00:00Z"})
	req := httptest.NewRequest(http.MethodPatch,
		"/bank/memories/"+rec.ID, bytes.NewReader(jb))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.bankByIDExtended(w, req)
	if w.Code < 400 || w.Code >= 500 {
		// UpdateMemory returns an error; the handler bubbles it as 4xx/500.
		// Either is acceptable as long as it's not 2xx.
		if w.Code >= 200 && w.Code < 300 {
			t.Errorf("expected non-2xx for TTL-on-sensitive, got %d: %s",
				w.Code, w.Body.String())
		}
	}
}
