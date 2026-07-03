// handlers_nightly_delete_test.go — tests for DELETE /nightly/runs/{id}.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// seedNightlyRun writes one nightly_runs row directly via the bank.
func seedNightlyRun(t *testing.T, bank *Bank, id, status, dreamEntry string) NightlyRun {
	t.Helper()
	rec := NightlyRun{
		ID:        id,
		StartedAt: nowUTC(),
		Status:    status,
		StatsJSON: `{"encoded":1,"schema_version":2}`,
		Narrative: "test narrative",
		Model:     "ollama:llama3.2:3b",
		CreatedAt: nowUTC(),
	}
	if status != "in_progress" {
		rec.FinishedAt = nowUTC()
	}
	if err := bank.InsertNightlyRun(rec); err != nil {
		t.Fatalf("InsertNightlyRun: %v", err)
	}
	if dreamEntry != "" {
		if err := bank.SetNightlyDreamEntry(id, dreamEntry, "warehouse stocktake"); err != nil {
			t.Fatalf("SetNightlyDreamEntry: %v", err)
		}
	}
	got, _ := bank.GetNightlyRun(id)
	return got
}

func TestNightlyRunDelete_HappyPath(t *testing.T) {
	srv, _, bank := newCaptureServer(t)
	seedNightlyRun(t, bank, "nightly-test-1", "completed", "dream prose here")

	req := httptest.NewRequest(http.MethodDelete, "/nightly/runs/nightly-test-1", nil)
	w := httptest.NewRecorder()
	srv.nightlyRoot(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["deleted"] != true || resp["id"] != "nightly-test-1" {
		t.Errorf("unexpected response body: %v", resp)
	}
	if _, err := bank.GetNightlyRun("nightly-test-1"); err == nil {
		t.Errorf("expected GetNightlyRun to error after delete")
	}
}

func TestNightlyRunDelete_NotFound(t *testing.T) {
	srv, _, _ := newCaptureServer(t)
	req := httptest.NewRequest(http.MethodDelete, "/nightly/runs/does-not-exist", nil)
	w := httptest.NewRecorder()
	srv.nightlyRoot(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if !strings.Contains(resp["error"].(string), "no such run") {
		t.Errorf("expected 'no such run' error, got %v", resp)
	}
}

func TestNightlyRunDelete_InProgressBlocks(t *testing.T) {
	srv, _, bank := newCaptureServer(t)
	seedNightlyRun(t, bank, "nightly-flight", "in_progress", "")

	req := httptest.NewRequest(http.MethodDelete, "/nightly/runs/nightly-flight", nil)
	w := httptest.NewRecorder()
	srv.nightlyRoot(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
	// Row should still exist.
	if _, err := bank.GetNightlyRun("nightly-flight"); err != nil {
		t.Errorf("expected in-progress row preserved on 409, got err %v", err)
	}
}

func TestNightlyRunDelete_AuditRow(t *testing.T) {
	srv, _, bank := newCaptureServer(t)
	seedNightlyRun(t, bank, "nightly-audit", "completed", "")

	req := httptest.NewRequest(http.MethodDelete, "/nightly/runs/nightly-audit", nil)
	srv.nightlyRoot(httptest.NewRecorder(), req)

	rows, err := bank.ListAuditLog(AuditFilter{Operation: "nightly_run_deleted", Limit: 10})
	if err != nil {
		t.Fatalf("ListAuditLog: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 nightly_run_deleted audit, got %d", len(rows))
	}
	if rows[0].EntityID != "nightly-audit" {
		t.Errorf("entity_id mismatch: %q", rows[0].EntityID)
	}
	if rows[0].Reason != "user_deleted_via_dashboard" {
		t.Errorf("reason mismatch: %q", rows[0].Reason)
	}
}

func TestNightlyRunDelete_AuditRedactsDreamEntry(t *testing.T) {
	srv, _, bank := newCaptureServer(t)
	const dreamProse = "in the warehouse the boxes were rearranged"
	seedNightlyRun(t, bank, "nightly-redact", "completed", dreamProse)

	req := httptest.NewRequest(http.MethodDelete, "/nightly/runs/nightly-redact", nil)
	srv.nightlyRoot(httptest.NewRecorder(), req)

	rows, _ := bank.ListAuditLog(AuditFilter{Operation: "nightly_run_deleted", Limit: 10})
	if len(rows) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(rows))
	}
	if strings.Contains(rows[0].BeforeJSON, dreamProse) {
		t.Errorf("dream_entry prose should be redacted from before_json, found verbatim: %s", rows[0].BeforeJSON)
	}
	if !strings.Contains(rows[0].BeforeJSON, "REDACTED") {
		t.Errorf("expected REDACTED placeholder in before_json, got %s", rows[0].BeforeJSON)
	}
}

func TestNightlyRunDelete_PreservesMemories(t *testing.T) {
	srv, _, bank := newCaptureServer(t)
	seedNightlyRun(t, bank, "nightly-keep-mems", "completed", "")
	// Synthetic synthesis memory created during the run.
	mem, _ := bank.SaveMemory(MemoryRecord{
		Text:                "synthesis output",
		Tags:                []string{"synthesis"},
		Source:              "nightly_synthesis",
		NightlyConsolidated: true,
	})

	req := httptest.NewRequest(http.MethodDelete, "/nightly/runs/nightly-keep-mems", nil)
	srv.nightlyRoot(httptest.NewRecorder(), req)

	got, err := bank.GetMemory(mem.ID)
	if err != nil || got.ID != mem.ID {
		t.Errorf("synthesis memory should survive run deletion, got err=%v id=%q", err, got.ID)
	}
}

func TestNightlyRunDelete_WSEvent(t *testing.T) {
	srv, _, bank := newCaptureServer(t)
	seedNightlyRun(t, bank, "nightly-ws", "completed", "")
	beforeAt := nowUTC()

	req := httptest.NewRequest(http.MethodDelete, "/nightly/runs/nightly-ws", nil)
	srv.nightlyRoot(httptest.NewRecorder(), req)

	events := drainEvents(t, srv, beforeAt)
	saw := false
	for _, e := range events {
		if e.Type == "nightly.deleted" {
			if e.Payload["run_id"] != "nightly-ws" {
				t.Errorf("payload run_id mismatch: %v", e.Payload)
			}
			if e.Payload["deleted_status"] != "completed" {
				t.Errorf("payload deleted_status mismatch: %v", e.Payload)
			}
			saw = true
		}
	}
	if !saw {
		t.Errorf("expected nightly.deleted WS event")
	}
}

func TestNightlyRunDelete_RoutePrecedence(t *testing.T) {
	// /nightly/runs (collection GET) and /nightly/runs/{id} (item DELETE)
	// must NOT collide.
	srv, _, bank := newCaptureServer(t)
	seedNightlyRun(t, bank, "nightly-route", "completed", "")

	// Collection GET should still work after item route is registered.
	req := httptest.NewRequest(http.MethodGet, "/nightly/runs", nil)
	w := httptest.NewRecorder()
	srv.nightlyRoot(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("collection GET broken by item route: %d body=%s", w.Code, w.Body.String())
	}

	// Item DELETE should hit the item handler.
	req2 := httptest.NewRequest(http.MethodDelete, "/nightly/runs/nightly-route", nil)
	w2 := httptest.NewRecorder()
	srv.nightlyRoot(w2, req2)
	if w2.Code != http.StatusOK {
		t.Errorf("item DELETE broken: %d body=%s", w2.Code, w2.Body.String())
	}
}

func TestNightlyRunDelete_RejectsBadID(t *testing.T) {
	srv, _, _ := newCaptureServer(t)
	// Trailing slash without id, and id with embedded slash.
	for _, path := range []string{"/nightly/runs/", "/nightly/runs/a/b"} {
		req := httptest.NewRequest(http.MethodDelete, path, nil)
		w := httptest.NewRecorder()
		srv.nightlyRoot(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("path %s: expected 400, got %d", path, w.Code)
		}
	}
}
