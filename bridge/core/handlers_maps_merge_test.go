// handlers_maps_merge_test.go — tests for /maps/merge bulk tag-merge.
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureHub wraps a Hub-shaped struct that records Fanout events for
// assertion. Mirrors the existing test pattern used by handlers_ws_test.
type captureHub struct {
	*Hub
	captured []Event
}

// newCaptureServer returns a *Server with a Hub whose Fanout we can assert on.
// Wires the same bank.Emit → hub.Fanout closure that main.go uses, so the
// audit.appended-vs-bulk.completed assertions exercise the real path.
func newCaptureServer(t *testing.T) (*Server, *captureHub, *Bank) {
	t.Helper()
	bank := newTestBank(t)
	ring := NewRingBuffer(256)
	hub := NewHub(ring)
	bank.Emit = func(eventType, adapterID string, payload map[string]interface{}) {
		hub.Fanout(Event{
			SchemaVersion: SchemaVersion,
			Type:          eventType,
			Timestamp:     nowUTC(),
			AdapterID:     adapterID,
			Payload:       payload,
		})
	}
	cap := &captureHub{Hub: hub}
	return &Server{
		hub:  hub,
		ring: ring,
		bank: bank,
	}, cap, bank
}

// drainEvents pulls events from the ring buffer (which the hub pushes to
// via Fanout) filtered to those occurring at or after `since`. Hub.Fanout
// stamps the ring; we don't need a separate websocket subscriber in tests.
func drainEvents(t *testing.T, srv *Server, since string) []Event {
	t.Helper()
	all := srv.ring.Recent(0)
	out := []Event{}
	for _, e := range all {
		if e.Timestamp >= since {
			out = append(out, e)
		}
	}
	return out
}

func TestMapsMerge_HappyPath(t *testing.T) {
	srv, _, bank := newCaptureServer(t)
	bank.SaveMemory(MemoryRecord{Text: "a", Tags: []string{"project:event-tracker", "ui"}})
	bank.SaveMemory(MemoryRecord{Text: "b", Tags: []string{"project:event-tracker", "code"}})
	bank.SaveMemory(MemoryRecord{Text: "c", Tags: []string{"unrelated"}})

	body := `{"from":["project:event-tracker"],"to":"event-tracker","dry_run":false}`
	req := httptest.NewRequest(http.MethodPost, "/maps/merge", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleMapsMerge(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := resp["memories_updated"]; got != float64(2) {
		t.Errorf("expected 2 memories_updated, got %v", got)
	}
	if resp["audit_run_id"] == "" {
		t.Errorf("expected audit_run_id present")
	}
	// Verify tags actually rewritten.
	mems, _ := bank.ListMemoriesWith(MemoryListOpts{Limit: 10})
	for _, m := range mems {
		for _, tag := range m.Tags {
			if tag == "project:event-tracker" {
				t.Errorf("expected tag rewritten, still found 'project:event-tracker' on %s", m.ID)
			}
		}
	}
}

func TestMapsMerge_DryRunDoesNotMutate(t *testing.T) {
	srv, _, bank := newCaptureServer(t)
	bank.SaveMemory(MemoryRecord{Text: "a", Tags: []string{"old", "ui"}})

	body := `{"from":["old"],"to":"new","dry_run":true}`
	req := httptest.NewRequest(http.MethodPost, "/maps/merge", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleMapsMerge(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if got := resp["dry_run"]; got != true {
		t.Errorf("expected dry_run=true echoed back")
	}
	mems, _ := bank.ListMemoriesWith(MemoryListOpts{Limit: 10})
	hasOld := false
	for _, m := range mems {
		for _, tag := range m.Tags {
			if tag == "old" {
				hasOld = true
			}
		}
	}
	if !hasOld {
		t.Errorf("dry_run should not mutate; expected 'old' tag to remain")
	}
}

func TestMapsMerge_AuditTruth(t *testing.T) {
	srv, _, bank := newCaptureServer(t)
	bank.SaveMemory(MemoryRecord{Text: "a", Tags: []string{"x", "ui"}})
	bank.SaveMemory(MemoryRecord{Text: "b", Tags: []string{"x"}})

	body := `{"from":["x"],"to":"y","dry_run":false}`
	req := httptest.NewRequest(http.MethodPost, "/maps/merge", strings.NewReader(body))
	srv.handleMapsMerge(httptest.NewRecorder(), req)

	rows, err := bank.ListAuditLog(AuditFilter{Operation: "tag_merge", Limit: 10})
	if err != nil {
		t.Fatalf("ListAuditLog: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 tag_merge audit row, got %d", len(rows))
	}
	if !strings.Contains(rows[0].AfterJSON, `"memories_updated":2`) {
		t.Errorf("audit after_json should mention memories_updated:2, got %s", rows[0].AfterJSON)
	}
}

func TestMapsMerge_SingleBulkEvent(t *testing.T) {
	srv, _, bank := newCaptureServer(t)
	for i := 0; i < 5; i++ {
		bank.SaveMemory(MemoryRecord{Text: "m" + string(rune('a'+i)), Tags: []string{"x"}})
	}
	beforeAt := nowUTC()

	body := `{"from":["x"],"to":"y","dry_run":false}`
	req := httptest.NewRequest(http.MethodPost, "/maps/merge", strings.NewReader(body))
	srv.handleMapsMerge(httptest.NewRecorder(), req)

	events := drainEvents(t, srv, beforeAt)
	bulkCount, auditCount := 0, 0
	for _, e := range events {
		switch e.Type {
		case "bulk.completed":
			bulkCount++
		case "audit.appended":
			auditCount++
		}
	}
	if bulkCount != 1 {
		t.Errorf("expected exactly 1 bulk.completed event, got %d", bulkCount)
	}
	if auditCount != 0 {
		t.Errorf("expected 0 audit.appended events during bulk merge, got %d (firehose suppression broken)", auditCount)
	}
}

func TestMapsMerge_RestoresAfterTx(t *testing.T) {
	// After a bulk merge, ordinary writes should resume firing audit.appended.
	srv, _, bank := newCaptureServer(t)
	bank.SaveMemory(MemoryRecord{Text: "a", Tags: []string{"x"}})

	srv.handleMapsMerge(httptest.NewRecorder(), httptest.NewRequest(
		http.MethodPost, "/maps/merge", strings.NewReader(`{"from":["x"],"to":"y"}`)))

	beforeAt := nowUTC()
	// Plain audit write — should emit audit.appended.
	bank.AppendAudit(AuditEntry{
		Operation: "test_marker", EntityType: "trace", EntityID: "t1",
	})
	events := drainEvents(t, srv, beforeAt)
	saw := false
	for _, e := range events {
		if e.Type == "audit.appended" {
			saw = true
			break
		}
	}
	if !saw {
		t.Errorf("expected audit.appended after bulk op completed (suppression must not leak)")
	}
}

func TestMapsMerge_RejectsEmptyBody(t *testing.T) {
	srv, _, _ := newCaptureServer(t)
	cases := []string{
		`{}`,
		`{"to":"x"}`,
		`{"from":["x"]}`,
	}
	for _, body := range cases {
		req := httptest.NewRequest(http.MethodPost, "/maps/merge", strings.NewReader(body))
		w := httptest.NewRecorder()
		srv.handleMapsMerge(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: expected 400, got %d", body, w.Code)
		}
	}
}

func TestMapsMerge_DeletesFromKeyOverrides(t *testing.T) {
	srv, _, bank := newCaptureServer(t)
	bank.SaveMapOverride(MapOverride{MapKey: "old-key", Category: "topic"})
	bank.SaveMemory(MemoryRecord{Text: "a", Tags: []string{"old-key"}})

	body := `{"from":["old-key"],"to":"new-key"}`
	srv.handleMapsMerge(httptest.NewRecorder(), httptest.NewRequest(
		http.MethodPost, "/maps/merge", bytes.NewReader([]byte(body))))

	if _, err := bank.GetMapOverride("old-key"); err == nil {
		t.Errorf("expected old-key map_override deleted")
	}
}
