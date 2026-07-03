package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// wsTestServer wires a Server with a fully-functional hub + ring + bank,
// and connects bank.Emit through hub.Fanout the same way main.go does in
// production. Returns a `events()` getter that snapshots the ring.
func wsTestServer(t *testing.T) (*Server, func() []Event) {
	t.Helper()
	bank := newTestBank(t)
	ring := NewRingBuffer(200)
	hub := NewHub(ring)
	bank.Emit = func(eventType, adapterID string, payload map[string]interface{}) {
		hub.Fanout(Event{
			SchemaVersion: SchemaVersion,
			Type:          eventType,
			Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
			AdapterID:     adapterID,
			Payload:       payload,
		})
	}
	server := &Server{bank: bank, hub: hub}
	return server, func() []Event { return ring.Recent(200) }
}

// findEvents returns the subset of `events` whose Type matches.
func findEvents(events []Event, typ string) []Event {
	out := []Event{}
	for _, e := range events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// ──────────────────────────────────────────────────────────────────────────
// 1. sensitive.flipped on UpdateMemory
// ──────────────────────────────────────────────────────────────────────────

func TestWS_SensitiveFlipped_OnUpdate(t *testing.T) {
	server, getEvents := wsTestServer(t)
	rec, _ := server.bank.SaveMemory(MemoryRecord{Text: "neutral", Tags: []string{"work"}})

	tr := true
	if _, err := server.bank.UpdateMemory(rec.ID, MemoryUpdate{Sensitive: &tr}); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}

	flips := findEvents(getEvents(), "sensitive.flipped")
	// SaveMemory of a non-sensitive record shouldn't emit, so we expect exactly 1.
	if len(flips) != 1 {
		t.Fatalf("expected 1 sensitive.flipped event, got %d", len(flips))
	}
	e := flips[0]
	if e.AdapterID != "sd-core-bank" {
		t.Errorf("expected adapter_id=sd-core-bank, got %q", e.AdapterID)
	}
	if e.Payload["trace_id"] != rec.ID {
		t.Errorf("payload.trace_id mismatch, got %v", e.Payload["trace_id"])
	}
	if e.Payload["sensitive"] != true {
		t.Errorf("payload.sensitive should be true, got %v", e.Payload["sensitive"])
	}
	if e.Payload["source"] != "manual" {
		t.Errorf("explicit PATCH should attribute source=manual, got %v", e.Payload["source"])
	}
}

// ──────────────────────────────────────────────────────────────────────────
// 2. sensitive.flipped from the AI classifier (auto-ai source)
// ──────────────────────────────────────────────────────────────────────────

func TestWS_SensitiveFlipped_FromClassifier(t *testing.T) {
	server, getEvents := wsTestServer(t)
	rec, _ := server.bank.SaveMemory(MemoryRecord{Text: "ambiguous prose"})

	scripted := &scriptedProvider{
		respond: func(_ string) string { return "YES" },
	}
	sc := newSensitiveClassifierConst(server.bank, scripted, 0, 10)
	sc.pass(context.Background())

	flips := findEvents(getEvents(), "sensitive.flipped")
	if len(flips) != 1 {
		t.Fatalf("expected 1 sensitive.flipped from classifier, got %d", len(flips))
	}
	e := flips[0]
	if e.AdapterID != "sd-core-sensitive-ai" {
		t.Errorf("expected adapter_id=sd-core-sensitive-ai, got %q", e.AdapterID)
	}
	if e.Payload["source"] != "auto-ai" {
		t.Errorf("expected source=auto-ai, got %v", e.Payload["source"])
	}
	if e.Payload["trace_id"] != rec.ID {
		t.Errorf("trace_id mismatch")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// 3. sensitive.flipped on sticky-on rejection
// ──────────────────────────────────────────────────────────────────────────

func TestWS_SensitiveFlipped_StickyOnReject(t *testing.T) {
	server, getEvents := wsTestServer(t)
	// A memory whose CONTENT will trigger sticky-on regardless of intent.
	rec, _ := server.bank.SaveMemory(MemoryRecord{
		Text: "deployment uses sk-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789",
	})
	if !rec.Sensitive {
		t.Fatalf("test setup: regex auto-flag should have made this sensitive; got %+v", rec)
	}

	// Caller asks to clear, but the regex on the unchanged text keeps it on.
	// To force the sticky-on path on UpdateMemory, we re-run the regex check
	// against the patch text — pass the same key text in the patch so the
	// scanner re-detects it.
	keepText := "deployment uses sk-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	fa := false
	post, err := server.bank.UpdateMemory(rec.ID, MemoryUpdate{
		Text:      &keepText,
		Sensitive: &fa,
	})
	if err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	if !post.Sensitive {
		t.Fatalf("sticky-on should keep sensitive=true; got %+v", post)
	}

	flips := findEvents(getEvents(), "sensitive.flipped")
	if len(flips) == 0 {
		t.Fatalf("expected at least 1 sensitive.flipped, got 0")
	}
	// RingBuffer.Recent returns newest-first, so flips[0] is the most recent.
	last := flips[0]
	if last.Payload["source"] != "sticky-on" {
		t.Errorf("expected source=sticky-on for the rejection emit, got %v (full: %+v)",
			last.Payload["source"], last.Payload)
	}
	if last.Payload["sensitive"] != true {
		t.Errorf("sticky-on emit should report sensitive=true, got %v", last.Payload["sensitive"])
	}
}

// ──────────────────────────────────────────────────────────────────────────
// 4. map.updated on PATCH /maps/{id}
// ──────────────────────────────────────────────────────────────────────────

func TestWS_MapUpdated_OnPatch(t *testing.T) {
	server, getEvents := wsTestServer(t)
	saved, _ := server.bank.SaveMemoryMap(MemoryMap{
		Name: "Initial", Type: "project", AnchorTags: []string{"x"},
	})

	st := "An updated schema description"
	body, _ := json.Marshal(MemoryMapUpdate{SchemaText: &st})
	req := httptest.NewRequest(http.MethodPatch, "/maps/"+saved.ID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH /maps/{id} expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	updates := findEvents(getEvents(), "map.updated")
	if len(updates) == 0 {
		t.Fatalf("expected at least 1 map.updated event")
	}
	last := updates[0] // newest-first
	if last.AdapterID != "sd-core-maps" {
		t.Errorf("expected adapter_id=sd-core-maps, got %q", last.AdapterID)
	}
	if last.Payload["map_id"] != saved.ID {
		t.Errorf("map_id mismatch, got %v", last.Payload["map_id"])
	}
	// changed_fields should include schema_text. Note: when only one field
	// changed, changed_fields is a slice of length 1 in Go but JSON-decoded
	// as []interface{} with one string.
	cf, _ := last.Payload["changed_fields"].([]string)
	if len(cf) == 0 {
		// Try the []interface{} form too — depends on whether the payload
		// went through JSON marshal/unmarshal cycle.
		cfAny, _ := last.Payload["changed_fields"].([]interface{})
		if len(cfAny) == 0 {
			t.Errorf("expected changed_fields to be populated, got %v (raw payload: %+v)",
				last.Payload["changed_fields"], last.Payload)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────
// 5. audit.appended on every audit_log row
// ──────────────────────────────────────────────────────────────────────────

func TestWS_AuditAppended_OnAnyWrite(t *testing.T) {
	server, getEvents := wsTestServer(t)

	if err := server.bank.AppendAudit(AuditEntry{
		Operation: "create", EntityType: "trace", EntityID: "t1",
	}); err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}

	appends := findEvents(getEvents(), "audit.appended")
	if len(appends) != 1 {
		t.Fatalf("expected 1 audit.appended, got %d", len(appends))
	}
	e := appends[0]
	if e.AdapterID != "sd-core-bank" {
		t.Errorf("expected adapter_id=sd-core-bank, got %q", e.AdapterID)
	}
	if e.Payload["action"] != "create" {
		t.Errorf("payload.action should be 'create' (the dashboard's name for operation), got %v", e.Payload["action"])
	}
	if e.Payload["entity_type"] != "trace" {
		t.Errorf("entity_type mismatch")
	}
	if e.Payload["entity_id"] != "t1" {
		t.Errorf("entity_id mismatch")
	}
	if e.Payload["audit_id"] == nil || e.Payload["audit_id"] == "" {
		t.Errorf("audit_id should be populated, got %v", e.Payload["audit_id"])
	}
}

// ──────────────────────────────────────────────────────────────────────────
// 6. env.loaded at boot — one event per non-empty allow-listed key
// ──────────────────────────────────────────────────────────────────────────

func TestWS_EnvLoaded_AtBoot(t *testing.T) {
	// Clear all allow-listed keys, then set just two so we have a deterministic
	// expected fanout.
	for k := range envWriteAllowList {
		t.Setenv(k, "")
	}
	t.Setenv("SD_TIER1_API_KEY", "fake-tier1")
	t.Setenv("SD_TIER3_API_KEY", "fake-tier3")

	type emitted struct {
		Type, AdapterID string
		Payload         map[string]interface{}
	}
	var got []emitted
	emit := func(eventType, adapterID string, payload map[string]interface{}) {
		got = append(got, emitted{eventType, adapterID, payload})
	}
	emitEnvLoadedEvents(emit)

	if len(got) != 2 {
		t.Fatalf("expected 2 env.loaded events, got %d (%+v)", len(got), got)
	}
	for _, e := range got {
		if e.Type != "env.loaded" {
			t.Errorf("type should be env.loaded, got %q", e.Type)
		}
		if e.AdapterID != "sd-core-env" {
			t.Errorf("adapter_id should be sd-core-env, got %q", e.AdapterID)
		}
		key, _ := e.Payload["key"].(string)
		if key != "SD_TIER1_API_KEY" && key != "SD_TIER3_API_KEY" {
			t.Errorf("unexpected key in env.loaded: %q", key)
		}
		// Defense-in-depth: payload must NOT include the env var VALUE.
		for fname, fval := range e.Payload {
			if str, ok := fval.(string); ok {
				if strings.Contains(str, "fake-tier1") || strings.Contains(str, "fake-tier3") {
					t.Errorf("INVARIANT VIOLATION: env.loaded payload field %q contained the env value: %q", fname, str)
				}
			}
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────
// 7. research.cached on UpsertResearchCache
// ──────────────────────────────────────────────────────────────────────────

func TestWS_ResearchCached_OnUpsert(t *testing.T) {
	server, getEvents := wsTestServer(t)
	if err := server.bank.SaveResearchCache(ResearchCacheEntry{
		Topic: "research-test-1", Payload: `{"query":"what is OAuth","content":"answer"}`,
		FetchedAt: nowUTC(), ExpiresAt: "2099-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("SaveResearchCache: %v", err)
	}

	cached := findEvents(getEvents(), "research.cached")
	if len(cached) != 1 {
		t.Fatalf("expected 1 research.cached, got %d", len(cached))
	}
	e := cached[0]
	if e.AdapterID != "sd-core-research" {
		t.Errorf("expected adapter_id=sd-core-research, got %q", e.AdapterID)
	}
	if e.Payload["entry_id"] != "research-test-1" {
		t.Errorf("entry_id mismatch")
	}
	if e.Payload["query"] != "what is OAuth" {
		t.Errorf("expected query field extracted from payload JSON, got %v", e.Payload["query"])
	}
}

// ──────────────────────────────────────────────────────────────────────────
// 8. Dot-namespace convention — none of the new types use snake_case
// ──────────────────────────────────────────────────────────────────────────

func TestWS_DotNamespaceConvention(t *testing.T) {
	server, getEvents := wsTestServer(t)

	// Trigger one of each emit path.
	rec, _ := server.bank.SaveMemory(MemoryRecord{Text: "x"})
	tr := true
	_, _ = server.bank.UpdateMemory(rec.ID, MemoryUpdate{Sensitive: &tr})
	_ = server.bank.AppendAudit(AuditEntry{Operation: "create", EntityType: "x", EntityID: "y"})
	_ = server.bank.SaveResearchCache(ResearchCacheEntry{
		Topic: "r-1", Payload: `{"query":"q"}`, FetchedAt: nowUTC(), ExpiresAt: "2099-01-01T00:00:00Z",
	})
	mp, _ := server.bank.SaveMemoryMap(MemoryMap{Name: "n", Type: "project"})
	st := "x"
	body, _ := json.Marshal(MemoryMapUpdate{SchemaText: &st})
	req := httptest.NewRequest(http.MethodPatch, "/maps/"+mp.ID, bytes.NewReader(body))
	rec2 := httptest.NewRecorder()
	server.routes().ServeHTTP(rec2, req)

	// Among the events fired in this test, count control-channel types.
	wantedTypes := map[string]bool{
		"sensitive.flipped": true, "map.updated": true,
		"audit.appended": true, "research.cached": true,
	}
	sawAtLeastOne := false
	for _, e := range getEvents() {
		if !wantedTypes[e.Type] {
			continue
		}
		sawAtLeastOne = true
		if !strings.Contains(e.Type, ".") {
			t.Errorf("control event type %q must use dot-namespace, got snake_case", e.Type)
		}
		// Defense: explicit blacklist of snake_case forms that would be wrong.
		for _, bad := range []string{"sensitive_flipped", "map_updated", "audit_appended", "research_cached"} {
			if e.Type == bad {
				t.Errorf("control event %q is in snake_case form (would route through brain animation)", bad)
			}
		}
	}
	if !sawAtLeastOne {
		t.Fatalf("test setup didn't produce any control-channel events")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// 9. No PII / credential patterns in any payload
// ──────────────────────────────────────────────────────────────────────────

func TestWS_NoPiiInPayload(t *testing.T) {
	server, getEvents := wsTestServer(t)

	// Seed sensitive content that COULD leak if payloads weren't redacted.
	const sneakyEmail = "victim@example.com"
	const sneakyKey = "sk-DO-NOT-LEAK-THIS-zzzzzzzz"
	mem, _ := server.bank.SaveMemory(MemoryRecord{
		Text: "contact " + sneakyEmail + " using " + sneakyKey,
	})
	if !mem.Sensitive {
		t.Fatalf("test setup: regex should have flagged this; got %+v", mem)
	}
	_ = server.bank.SaveResearchCache(ResearchCacheEntry{
		Topic:   "research-with-secrets",
		Payload: `{"query":"safe query","content":"` + sneakyEmail + ` and ` + sneakyKey + `"}`,
		FetchedAt: nowUTC(), ExpiresAt: "2099-01-01T00:00:00Z",
	})

	// Patterns the dashboard's frontend test would also reject.
	emailRe := regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)
	skKeyRe := regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{8,}`)

	for _, e := range getEvents() {
		// Skip non-control channel events (synapse builder etc. that pre-date this).
		if !strings.Contains(e.Type, ".") {
			continue
		}
		raw, _ := json.Marshal(e.Payload)
		if emailRe.Match(raw) {
			t.Errorf("INVARIANT VIOLATION: %s payload contains email pattern: %s", e.Type, raw)
		}
		if skKeyRe.Match(raw) {
			t.Errorf("INVARIANT VIOLATION: %s payload contains sk- key pattern: %s", e.Type, raw)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────
// 10. adapter_id always starts with "sd-core-"
// ──────────────────────────────────────────────────────────────────────────

func TestWS_AdapterIdPrefix(t *testing.T) {
	server, getEvents := wsTestServer(t)

	// Trigger every control event type.
	rec, _ := server.bank.SaveMemory(MemoryRecord{Text: "x", Tags: []string{"private"}})
	_ = server.bank.AppendAudit(AuditEntry{
		Operation: "create", EntityType: "trace", EntityID: rec.ID,
	})
	_ = server.bank.SaveResearchCache(ResearchCacheEntry{
		Topic: "r-1", Payload: `{}`, FetchedAt: nowUTC(), ExpiresAt: "2099-01-01T00:00:00Z",
	})
	mp, _ := server.bank.SaveMemoryMap(MemoryMap{Name: "n", Type: "project"})
	st := "x"
	body, _ := json.Marshal(MemoryMapUpdate{SchemaText: &st})
	req := httptest.NewRequest(http.MethodPatch, "/maps/"+mp.ID, bytes.NewReader(body))
	srec := httptest.NewRecorder()
	server.routes().ServeHTTP(srec, req)

	for _, e := range getEvents() {
		if !strings.Contains(e.Type, ".") {
			continue
		}
		if !strings.HasPrefix(e.AdapterID, "sd-core-") {
			t.Errorf("control event %s has adapter_id=%q which doesn't start with sd-core-", e.Type, e.AdapterID)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────
// 11. Auth required on the WS upgrade path
// ──────────────────────────────────────────────────────────────────────────

func TestWS_AuthRequired(t *testing.T) {
	t.Setenv("SD_API_TOKEN", "ws-test-token")
	server, _ := wsTestServer(t)
	handler := newAuthMiddleware(server.routes())

	// No bearer + no ?token= query → 401 from middleware before WS upgrade.
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 on /ws without token, got %d", rec.Code)
	}

	// ?token= with the right value should pass the middleware (the WS
	// upgrade itself will fail in a non-WS request, but we just check the
	// middleware doesn't 401).
	req = httptest.NewRequest(http.MethodGet, "/ws?token=ws-test-token", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Errorf("correct ?token= should pass middleware, got 401")
	}
}
