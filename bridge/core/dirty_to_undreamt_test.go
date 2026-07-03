// dirty_to_undreamt_test.go — wave-6 rename aliases per
// `CODE_HANDOFF — Rename dirty to undreamt (2026-05-10).md`. Verifies
// that JSON responses carry both forms, the /redream sub-route works,
// and audit ops use the new operation name.
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMemoryRecord_MarshalEmitsBothAliases(t *testing.T) {
	rec := MemoryRecord{
		ID:            "m1",
		Text:          "hello",
		MarkedDirtyAt: "2026-05-10T12:00:00Z",
		DirtyReason:   "tmr_user",
	}
	body, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(body)
	for _, want := range []string{
		`"marked_dirty_at":"2026-05-10T12:00:00Z"`,
		`"dirty_reason":"tmr_user"`,
		`"marked_for_redream_at":"2026-05-10T12:00:00Z"`,
		`"redream_reason":"tmr_user"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("want %q in %s", want, s)
		}
	}
}

func TestMemoryRecord_MarshalOmitsAliasesWhenEmpty(t *testing.T) {
	rec := MemoryRecord{ID: "m1", Text: "fresh"}
	body, _ := json.Marshal(rec)
	s := string(body)
	if strings.Contains(s, "marked_for_redream_at") || strings.Contains(s, "redream_reason") {
		t.Errorf("alias fields should be omitempty when empty; got %s", s)
	}
}

func TestRedreamEndpoint_AliasOfDirty(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "test"})
	srv := newTestServer(t, bank)
	body := bytes.NewBufferString(`{"reason":"tmr_user"}`)
	req := httptest.NewRequest(http.MethodPost, "/bank/memories/"+rec.ID+"/redream", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.bankByIDExtended(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/redream want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	for _, k := range []string{"dirty_reason", "marked_dirty_at", "redream_reason", "marked_for_redream_at"} {
		if _, ok := resp[k]; !ok {
			t.Errorf("response missing key %q", k)
		}
	}
}

func TestDirtyEndpoint_StillWorks(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "test"})
	srv := newTestServer(t, bank)
	body := bytes.NewBufferString(`{"reason":"tmr_user"}`)
	req := httptest.NewRequest(http.MethodPost, "/bank/memories/"+rec.ID+"/dirty", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.bankByIDExtended(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("legacy /dirty want 200, got %d", w.Code)
	}
}

func TestRedreamEndpoint_WritesNewAuditOperation(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "test"})
	srv := newTestServer(t, bank)
	body := bytes.NewBufferString(`{"reason":"tmr_user"}`)
	req := httptest.NewRequest(http.MethodPost, "/bank/memories/"+rec.ID+"/redream", body)
	req.Header.Set("Content-Type", "application/json")
	srv.bankByIDExtended(httptest.NewRecorder(), req)

	entries, _ := bank.ListAuditLog(AuditFilter{Operation: "memory_marked_for_redream", Limit: 10})
	if len(entries) == 0 {
		t.Errorf("expected at least one memory_marked_for_redream audit row")
	}
	// Old op name must NOT appear for this new call.
	old, _ := bank.ListAuditLog(AuditFilter{Operation: "memory_marked_dirty", Limit: 10})
	if len(old) != 0 {
		t.Errorf("new endpoint must not write under old op name; got %d rows", len(old))
	}
}

// helper — build a minimal Server with the given bank for handler tests.
func newTestServer(t *testing.T, bank *Bank) *Server {
	t.Helper()
	return &Server{
		bank: bank,
		hub:  &Hub{},
	}
}
