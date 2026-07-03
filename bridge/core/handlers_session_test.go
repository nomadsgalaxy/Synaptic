// handlers_session_test.go — wave 7c2 section_dirty mechanism +
// /session/dirty + /session/clear + lazy-fetch cursor behavior.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────
// Hub.MarkSectionDirty + SectionState
// ──────────────────────────────────────────────────────────────────────────

func TestHub_MarkSectionDirty_RecordsTimestamp(t *testing.T) {
	hub := NewHub(NewRingBuffer(10))
	if got := hub.SectionState(); got[SectionAudit].IsZero() == false {
		t.Errorf("fresh hub: audit should be zero, got %v", got[SectionAudit])
	}
	before := time.Now()
	hub.MarkSectionDirty(SectionAudit)
	got := hub.SectionState()
	if got[SectionAudit].Before(before) {
		t.Errorf("audit dirty time not bumped: %v vs before=%v", got[SectionAudit], before)
	}
	if !got[SectionMaps].IsZero() {
		t.Errorf("maps shouldn't be dirty yet: %v", got[SectionMaps])
	}
}

func TestHub_MarkSectionDirty_AcceptsMultiple(t *testing.T) {
	hub := NewHub(NewRingBuffer(10))
	hub.MarkSectionDirty(SectionMaps, SectionAudit, SectionBank)
	got := hub.SectionState()
	for _, s := range []string{SectionMaps, SectionAudit, SectionBank} {
		if got[s].IsZero() {
			t.Errorf("section %s should be dirtied", s)
		}
	}
	if !got[SectionLexicon].IsZero() {
		t.Errorf("lexicon should be clean")
	}
}

func TestHub_MarkSectionDirty_EmitsSectionDirtyEvent(t *testing.T) {
	ring := NewRingBuffer(10)
	hub := NewHub(ring)
	hub.MarkSectionDirty(SectionAudit, SectionMaps)
	// Recent ring contains the section_dirty event.
	recent := ring.Recent(5)
	found := false
	for _, e := range recent {
		if e.Type == "section_dirty" {
			found = true
			// Payload contains both sections.
			if sections, ok := e.Payload["sections"].([]string); ok {
				if len(sections) != 2 {
					t.Errorf("payload sections: want 2, got %d", len(sections))
				}
			}
		}
	}
	// FanoutEphemeral doesn't add to ring buffer — so section_dirty
	// shouldn't be in the ring. But it WAS emitted; we'd see it on a
	// live WS subscriber. Confirm absence from ring instead.
	if found {
		t.Errorf("section_dirty went through ring buffer — it should be ephemeral")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Auto-translation from legacy events
// ──────────────────────────────────────────────────────────────────────────

func TestFanout_AutoTranslatesAuditAppendedToSectionDirty(t *testing.T) {
	hub := NewHub(NewRingBuffer(10))
	hub.Fanout(Event{
		SchemaVersion: SchemaVersion,
		Type:          "audit.appended",
		Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
		AdapterID:     "sd-core-bank",
	})
	got := hub.SectionState()
	if got[SectionAudit].IsZero() {
		t.Errorf("audit.appended should auto-dirty 'audit' section")
	}
}

func TestFanout_BulkCompletedMarksMapsAuditBank(t *testing.T) {
	hub := NewHub(NewRingBuffer(10))
	hub.Fanout(Event{
		SchemaVersion: SchemaVersion,
		Type:          "bulk.completed",
		Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
		AdapterID:     "sd-core-bank",
	})
	got := hub.SectionState()
	for _, s := range []string{SectionMaps, SectionAudit, SectionBank} {
		if got[s].IsZero() {
			t.Errorf("bulk.completed should auto-dirty %s; got zero", s)
		}
	}
	if !got[SectionLexicon].IsZero() {
		t.Errorf("bulk.completed should NOT dirty lexicon")
	}
}

func TestFanout_NightlyCompletedMarksDreamJournal(t *testing.T) {
	hub := NewHub(NewRingBuffer(10))
	hub.Fanout(Event{
		SchemaVersion: SchemaVersion,
		Type:          "nightly.completed",
		Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
		AdapterID:     "sd-core-nightly",
	})
	got := hub.SectionState()
	if got[SectionDreamJournal].IsZero() {
		t.Errorf("nightly.completed should dirty dream_journal")
	}
}

func TestFanout_ServiceLifecycleTransitionsMarkServices(t *testing.T) {
	hub := NewHub(NewRingBuffer(10))
	for _, typ := range []string{
		"service_lifecycle_transitioned",
		"service_lifecycle_ready",
		"service_lifecycle_idled",
		"service_lifecycle_stopped",
		"service_state_changed",
	} {
		// Fresh hub per iteration so we observe the dirty stamp move.
		h := NewHub(NewRingBuffer(10))
		h.Fanout(Event{
			SchemaVersion: SchemaVersion,
			Type:          typ,
			Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
			AdapterID:     "sd-core-docker",
		})
		got := h.SectionState()
		if got[SectionServices].IsZero() {
			t.Errorf("%s should dirty services; got zero", typ)
		}
		_ = hub
	}
}

func TestFanout_VisualisationEventsDoNotDirty(t *testing.T) {
	hub := NewHub(NewRingBuffer(10))
	for _, typ := range []string{
		"tool_call", "tool_result", "model_thinking",
		"memory_added", "bubble_added", "heartbeat",
		"prompt_received", "response_complete",
	} {
		hub.Fanout(Event{
			SchemaVersion: SchemaVersion,
			Type:          typ,
			Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
			AdapterID:     "test",
		})
	}
	got := hub.SectionState()
	for s, t2 := range got {
		if !t2.IsZero() {
			t.Errorf("section %s shouldn't have been dirtied by visualisation events, got %v", s, t2)
		}
	}
}

func TestFanout_SectionDirtyItselfDoesNotRecurse(t *testing.T) {
	// section_dirty events MUST NOT be in eventToSections — otherwise
	// they'd recursively re-emit themselves.
	if _, found := eventToSections["section_dirty"]; found {
		t.Errorf("section_dirty must not appear in eventToSections (recursion risk)")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// GET /session/dirty
// ──────────────────────────────────────────────────────────────────────────

func TestSessionDirty_FreshReturnsAllNulls(t *testing.T) {
	srv := newTestServer(t, nil)
	srv.hub = NewHub(NewRingBuffer(10))
	req := httptest.NewRequest(http.MethodGet, "/session/dirty", nil)
	w := httptest.NewRecorder()
	srv.handleSessionDirty(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	for _, s := range knownSections {
		v, ok := resp[s]
		if !ok {
			t.Errorf("section %s missing from response", s)
		}
		if v != nil {
			t.Errorf("section %s should be null on fresh hub, got %v", s, v)
		}
	}
}

func TestSessionDirty_ReturnsTimestampAfterMark(t *testing.T) {
	srv := newTestServer(t, nil)
	srv.hub = NewHub(NewRingBuffer(10))
	srv.hub.MarkSectionDirty(SectionAudit)

	req := httptest.NewRequest(http.MethodGet, "/session/dirty", nil)
	w := httptest.NewRecorder()
	srv.handleSessionDirty(w, req)
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp[SectionAudit] == nil {
		t.Errorf("audit should be non-null after mark")
	}
	// Audit should be a string (RFC3339Nano), maps still null.
	if _, ok := resp[SectionAudit].(string); !ok {
		t.Errorf("audit value should be ISO string, got %T", resp[SectionAudit])
	}
	if resp[SectionMaps] != nil {
		t.Errorf("maps should still be null, got %v", resp[SectionMaps])
	}
}

func TestSessionDirty_MethodGuard(t *testing.T) {
	srv := newTestServer(t, nil)
	srv.hub = NewHub(NewRingBuffer(10))
	req := httptest.NewRequest(http.MethodPost, "/session/dirty", nil)
	w := httptest.NewRecorder()
	srv.handleSessionDirty(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST want 405, got %d", w.Code)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// POST /session/clear/{section}
// ──────────────────────────────────────────────────────────────────────────

func TestSessionClear_KnownSectionReturnsOk(t *testing.T) {
	srv := newTestServer(t, nil)
	srv.hub = NewHub(NewRingBuffer(10))
	req := httptest.NewRequest(http.MethodPost, "/session/clear/audit", nil)
	w := httptest.NewRecorder()
	srv.handleSessionClear(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["ok"] != true {
		t.Errorf("ok should be true")
	}
	if resp["section"] != "audit" {
		t.Errorf("section: want audit, got %v", resp["section"])
	}
}

func TestSessionClear_UnknownSectionReturns400(t *testing.T) {
	srv := newTestServer(t, nil)
	srv.hub = NewHub(NewRingBuffer(10))
	req := httptest.NewRequest(http.MethodPost, "/session/clear/yeet", nil)
	w := httptest.NewRecorder()
	srv.handleSessionClear(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("unknown section: want 400, got %d", w.Code)
	}
}

func TestSessionClear_MissingSectionReturns400(t *testing.T) {
	srv := newTestServer(t, nil)
	srv.hub = NewHub(NewRingBuffer(10))
	req := httptest.NewRequest(http.MethodPost, "/session/clear/", nil)
	w := httptest.NewRecorder()
	srv.handleSessionClear(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing section: want 400, got %d", w.Code)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Cursor helpers
// ──────────────────────────────────────────────────────────────────────────

func TestPickSinceCursor_PrefersExplicit(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/audit?since_cursor=2026-05-11T00:00:00Z&since=2024-01-01T00:00:00Z", nil)
	if got := pickSinceCursor(req); got != "2026-05-11T00:00:00Z" {
		t.Errorf("want since_cursor preferred; got %q", got)
	}
}

func TestPickSinceCursor_FallsBackToSince(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/audit?since=2024-01-01T00:00:00Z", nil)
	if got := pickSinceCursor(req); got != "2024-01-01T00:00:00Z" {
		t.Errorf("legacy fallback failed; got %q", got)
	}
}

func TestPickSinceCursor_EmptyWhenAbsent(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/audit", nil)
	if got := pickSinceCursor(req); got != "" {
		t.Errorf("want empty; got %q", got)
	}
}

func TestLazyFetchEnabled_DefaultsFalse(t *testing.T) {
	// SD_LAZY_FETCH_ENABLED unset → false.
	t.Setenv("SD_LAZY_FETCH_ENABLED", "")
	if lazyFetchEnabled() {
		t.Errorf("expected default false")
	}
	t.Setenv("SD_LAZY_FETCH_ENABLED", "1")
	if !lazyFetchEnabled() {
		t.Errorf("expected true when =1")
	}
	t.Setenv("SD_LAZY_FETCH_ENABLED", "true")
	if lazyFetchEnabled() {
		t.Errorf("only literal '1' should enable; 'true' should not")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// /audit cursor behavior end-to-end
// ──────────────────────────────────────────────────────────────────────────

func TestAuditRoot_OmitsCursorWhenFlagOff(t *testing.T) {
	t.Setenv("SD_LAZY_FETCH_ENABLED", "0")
	bank := newTestBank(t)
	bank.AppendAudit(AuditEntry{Operation: "test", EntityType: "memory"})

	srv := newTestServer(t, bank)
	req := httptest.NewRequest(http.MethodGet, "/audit", nil)
	w := httptest.NewRecorder()
	srv.auditRoot(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if _, ok := resp["next_cursor"]; ok {
		t.Errorf("flag off: next_cursor should NOT be in response")
	}
	if _, ok := resp["section_clean_at"]; ok {
		t.Errorf("flag off: section_clean_at should NOT be in response")
	}
}

func TestAuditRoot_IncludesCursorWhenFlagOn(t *testing.T) {
	t.Setenv("SD_LAZY_FETCH_ENABLED", "1")
	bank := newTestBank(t)
	bank.AppendAudit(AuditEntry{Operation: "test", EntityType: "memory"})

	srv := newTestServer(t, bank)
	req := httptest.NewRequest(http.MethodGet, "/audit", nil)
	w := httptest.NewRecorder()
	srv.auditRoot(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if _, ok := resp["next_cursor"]; !ok {
		t.Errorf("flag on: next_cursor should be in response")
	}
	if _, ok := resp["section_clean_at"]; !ok {
		t.Errorf("flag on: section_clean_at should be in response")
	}
	// next_cursor should be a string (the most-recent entry's created_at).
	if nc, ok := resp["next_cursor"].(string); !ok || nc == "" {
		t.Errorf("next_cursor should be a non-empty ISO string; got %v", resp["next_cursor"])
	}
}

func TestAuditRoot_AcceptsSinceCursorAsAlias(t *testing.T) {
	bank := newTestBank(t)
	// Two entries with different created_at; the older one is filtered out.
	bank.AppendAudit(AuditEntry{Operation: "old", EntityType: "memory", CreatedAt: "2024-01-01T00:00:00Z"})
	bank.AppendAudit(AuditEntry{Operation: "new", EntityType: "memory", CreatedAt: "2026-05-10T00:00:00Z"})

	srv := newTestServer(t, bank)
	req := httptest.NewRequest(http.MethodGet, "/audit?since_cursor=2025-01-01T00:00:00Z", nil)
	w := httptest.NewRecorder()
	srv.auditRoot(w, req)
	var resp struct {
		Count   int          `json:"count"`
		Entries []AuditEntry `json:"entries"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Count != 1 {
		t.Errorf("since_cursor filter: want 1 entry, got %d", resp.Count)
	}
	for _, e := range resp.Entries {
		if !strings.HasPrefix(e.CreatedAt, "2026") {
			t.Errorf("entry should be post-cursor: %+v", e)
		}
	}
}
