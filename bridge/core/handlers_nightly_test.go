package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// nightlyTestServer wires a server with a fully-functional NightlyRunner
// (no-op steps by default; tests override fields directly). Returns the
// server + the runner so tests can inspect / inject.
func nightlyTestServer(t *testing.T) (*Server, *NightlyRunner) {
	t.Helper()
	bank := newTestBank(t)
	ring := NewRingBuffer(50)
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
	router := NewModelRouter(&fakeProvider{name: "t1", local: true}, nil, nil, nil)
	runner := NewNightlyRunner(bank, router, hub)
	server := &Server{bank: bank, hub: hub, router: router, nightly: runner}
	return server, runner
}

// seedRun is a tiny helper for tests that need pre-existing rows.
func seedRun(t *testing.T, bank *Bank, id, startedAt, status string) {
	t.Helper()
	if err := bank.InsertNightlyRun(NightlyRun{
		ID: id, StartedAt: startedAt, Status: status,
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func getNightlyRuns(t *testing.T, server *Server, query string) (int, map[string]interface{}, string) {
	t.Helper()
	url := "/nightly/runs"
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	var out map[string]interface{}
	if rec.Body.Len() > 0 && rec.Code == http.StatusOK {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out, rec.Body.String()
}

// ──────────────────────────────────────────────────────────────────────────

func TestNightlyRuns_EmptyList(t *testing.T) {
	server, _ := nightlyTestServer(t)
	code, body, raw := getNightlyRuns(t, server, "")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, raw)
	}
	if body["count"].(float64) != 0 {
		t.Errorf("expected count=0, got %v", body["count"])
	}
	runs, _ := body["runs"].([]interface{})
	if len(runs) != 0 {
		t.Errorf("expected empty runs, got %d", len(runs))
	}
}

func TestNightlyRuns_Pagination(t *testing.T) {
	server, _ := nightlyTestServer(t)
	// Seed 60 rows with monotonically increasing started_at so newest is row #59.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 60; i++ {
		seedRun(t, server.bank,
			"nightly-test-"+strings.ReplaceAll(base.Add(time.Duration(i)*time.Hour).Format("2006-01-02-15"), ":", "-"),
			base.Add(time.Duration(i)*time.Hour).Format(time.RFC3339Nano),
			"completed",
		)
	}
	code, body, _ := getNightlyRuns(t, server, "limit=20")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if body["count"].(float64) != 20 {
		t.Errorf("expected count=20, got %v", body["count"])
	}
	// Newest-first: first row's started_at should be the latest.
	runs, _ := body["runs"].([]interface{})
	first := runs[0].(map[string]interface{})
	wantNewest := base.Add(59 * time.Hour).Format(time.RFC3339Nano)
	if first["started_at"] != wantNewest {
		t.Errorf("expected newest first; want started_at=%s got %v", wantNewest, first["started_at"])
	}
}

func TestNightlyRuns_SinceFilter(t *testing.T) {
	server, _ := nightlyTestServer(t)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	mid := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	new := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	seedRun(t, server.bank, "old", old, "completed")
	seedRun(t, server.bank, "mid", mid, "completed")
	seedRun(t, server.bank, "new", new, "completed")

	code, body, _ := getNightlyRuns(t, server, "since="+mid)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	runs, _ := body["runs"].([]interface{})
	if len(runs) != 2 {
		t.Errorf("expected 2 (mid + new), got %d", len(runs))
	}
	for _, r := range runs {
		id := r.(map[string]interface{})["id"].(string)
		if id == "old" {
			t.Errorf("`old` row should have been filtered out")
		}
	}
}

func TestNightlyRuns_StatusFilter(t *testing.T) {
	server, _ := nightlyTestServer(t)
	now := nowUTC()
	seedRun(t, server.bank, "a", now, "completed")
	seedRun(t, server.bank, "b", now, "failed")
	seedRun(t, server.bank, "c", now, "in_progress")

	code, body, _ := getNightlyRuns(t, server, "status=failed")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	runs, _ := body["runs"].([]interface{})
	if len(runs) != 1 {
		t.Fatalf("expected 1 failed row, got %d", len(runs))
	}
	if runs[0].(map[string]interface{})["id"] != "b" {
		t.Errorf("expected the failed row, got %v", runs[0])
	}
}

func TestNightlyRuns_StatsJsonSerialized(t *testing.T) {
	server, _ := nightlyTestServer(t)
	stats := NightlyStats{Encoded: 47, Consolidated: 312, MapsUpdated: 8, SchemaVersion: 1}
	statsRaw, _ := json.Marshal(stats)
	if err := server.bank.InsertNightlyRun(NightlyRun{
		ID: "with-stats", StartedAt: nowUTC(), Status: "completed",
		StatsJSON: string(statsRaw), CreatedAt: nowUTC(),
	}); err != nil {
		t.Fatalf("InsertNightlyRun: %v", err)
	}

	_, body, _ := getNightlyRuns(t, server, "")
	runs, _ := body["runs"].([]interface{})
	if len(runs) != 1 {
		t.Fatalf("expected 1 run")
	}
	entry := runs[0].(map[string]interface{})
	gotStats, ok := entry["stats"].(map[string]interface{})
	if !ok {
		t.Fatalf("stats should be an object, got %T", entry["stats"])
	}
	if gotStats["encoded"].(float64) != 47 {
		t.Errorf("stats.encoded round-trip failed: got %v", gotStats["encoded"])
	}
	if gotStats["maps_updated"].(float64) != 8 {
		t.Errorf("stats.maps_updated round-trip failed: got %v", gotStats["maps_updated"])
	}
}

func TestNightlyRuns_NarrativeOptional(t *testing.T) {
	server, _ := nightlyTestServer(t)
	// Two rows: one with narrative, one without.
	if err := server.bank.InsertNightlyRun(NightlyRun{
		ID: "with-narr", StartedAt: nowUTC(), Status: "completed",
		Narrative: "Tonight your brain consolidated 47 traces.", CreatedAt: nowUTC(),
	}); err != nil {
		t.Fatalf("seed with narrative: %v", err)
	}
	if err := server.bank.InsertNightlyRun(NightlyRun{
		ID: "no-narr", StartedAt: nowUTC(), Status: "completed",
		Narrative: "", CreatedAt: nowUTC(),
	}); err != nil {
		t.Fatalf("seed without narrative: %v", err)
	}

	_, body, raw := getNightlyRuns(t, server, "")
	runs, _ := body["runs"].([]interface{})
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(runs))
	}
	for _, r := range runs {
		entry := r.(map[string]interface{})
		id := entry["id"].(string)
		if id == "no-narr" {
			if _, present := entry["narrative"]; present {
				t.Errorf("empty narrative should be OMITTED from response (not null), got %s", raw)
			}
		}
		if id == "with-narr" {
			if entry["narrative"] != "Tonight your brain consolidated 47 traces." {
				t.Errorf("narrative round-trip failed: %v", entry["narrative"])
			}
		}
	}
	// Sanity: response should never contain the literal "null".
	if strings.Contains(raw, `"narrative":null`) || strings.Contains(raw, `"narrative": null`) {
		t.Errorf("response must omit narrative when empty, never emit null: %s", raw)
	}
}

func TestNightlyRuns_AuthRequired(t *testing.T) {
	t.Setenv("SD_API_TOKEN", "nightly-test-token")
	server, _ := nightlyTestServer(t)
	handler := newAuthMiddleware(server.routes())

	req := httptest.NewRequest(http.MethodGet, "/nightly/runs", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// NightlyRunner framework
// ──────────────────────────────────────────────────────────────────────────

func TestNightlyRunner_WritesRow(t *testing.T) {
	server, runner := nightlyTestServer(t)

	rec, err := runner.DoRun(context.Background())
	if err != nil {
		t.Fatalf("DoRun: %v", err)
	}
	if rec.ID == "" || !strings.HasPrefix(rec.ID, "nightly-") {
		t.Errorf("expected id like nightly-..., got %q", rec.ID)
	}
	if rec.Status != "completed" {
		t.Errorf("expected status=completed, got %q", rec.Status)
	}

	// Exactly one row should exist.
	rows, _ := server.bank.ListNightlyRuns(NightlyRunListOpts{})
	if len(rows) != 1 {
		t.Errorf("expected exactly 1 row, got %d", len(rows))
	}
}

func TestNightlyRunner_FailureWritesError(t *testing.T) {
	_, runner := nightlyTestServer(t)
	const errMsg = "consolidation step blew up"
	runner.runStepsFn = func(_ context.Context, _ string) error {
		return errors.New(errMsg)
	}

	rec, _ := runner.DoRun(context.Background())
	if rec.Status != "failed" {
		t.Errorf("expected status=failed, got %q", rec.Status)
	}
	if !strings.Contains(rec.Error, errMsg) {
		t.Errorf("expected error column to contain %q, got %q", errMsg, rec.Error)
	}
}

func TestNightlyRunner_PanicCaughtAndReported(t *testing.T) {
	_, runner := nightlyTestServer(t)
	runner.runStepsFn = func(_ context.Context, _ string) error {
		panic("boom")
	}
	rec, err := runner.DoRun(context.Background())
	if err != nil {
		t.Fatalf("DoRun should NOT propagate panic; got err=%v", err)
	}
	if rec.Status != "failed" {
		t.Errorf("panic should set status=failed, got %q", rec.Status)
	}
	if !strings.Contains(rec.Error, "panic") {
		t.Errorf("panic should be captured in error column, got %q", rec.Error)
	}
}

func TestNightlyRunner_AuditEntry(t *testing.T) {
	server, runner := nightlyTestServer(t)
	rec, _ := runner.DoRun(context.Background())

	entries, err := server.bank.ListAuditLog(AuditFilter{
		EntityType: "nightly_run", Operation: "nightly_runner",
	})
	if err != nil {
		t.Fatalf("ListAuditLog: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 nightly_runner audit entry, got %d", len(entries))
	}
	e := entries[0]
	if e.EntityID != rec.ID {
		t.Errorf("audit entity_id mismatch: got %q want %q", e.EntityID, rec.ID)
	}
	// Should NOT contain the narrative (per handoff: keep summary small).
	if strings.Contains(e.AfterJSON, "narrative") {
		t.Errorf("audit after_json should NOT include narrative field, got %q", e.AfterJSON)
	}
}

func TestNightlyRunner_NoConcurrent(t *testing.T) {
	server, runner := nightlyTestServer(t)

	// Block runStepsFn until we release a gate, simulating a long-running pass.
	gate := make(chan struct{})
	runner.runStepsFn = func(ctx context.Context, _ string) error {
		<-gate
		return nil
	}

	// Kick off the first DoRun in a goroutine.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = runner.DoRun(context.Background())
	}()
	// Wait until the runner is in flight.
	for i := 0; i < 200; i++ {
		if running, _ := runner.IsRunning(); running {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Second trigger via HTTP must return 409.
	req := httptest.NewRequest(http.MethodPost, "/nightly/trigger", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "current_run_id") {
		t.Errorf("409 body should include current_run_id, got %s", rec.Body.String())
	}

	// Direct DoRun call also returns ErrNightlyAlreadyRunning.
	_, err := runner.DoRun(context.Background())
	if !errors.Is(err, ErrNightlyAlreadyRunning) {
		t.Errorf("expected ErrNightlyAlreadyRunning, got %v", err)
	}

	close(gate)
	wg.Wait()
}

func TestNightlyRunner_TokensCharged(t *testing.T) {
	server, runner := nightlyTestServer(t)

	// Inject a narrative function that "consumes" 100/200 tokens but does
	// not call AddTokensUsed — the runner should top up the budget from
	// the (tokensTotal − consumedDuringRun) delta.
	runner.narrativeFn = func(_ context.Context, _ NightlyStats) (string, int, int, string, error) {
		return "Tonight your brain consolidated nothing.", 100, 200, "fake-tier2", nil
	}

	today := time.Now().UTC().Format("2006-01-02")
	before, _ := server.bank.GetTokensUsed(today)

	rec, err := runner.DoRun(context.Background())
	if err != nil {
		t.Fatalf("DoRun: %v", err)
	}
	if rec.Status != "completed" {
		t.Fatalf("expected completed, got %q (err=%q)", rec.Status, rec.Error)
	}

	after, _ := server.bank.GetTokensUsed(today)
	delta := after - before
	if delta != 300 {
		t.Errorf("expected token budget +300 (100+200), got delta=%d (before=%d after=%d)",
			delta, before, after)
	}

	// Verify stats reflect the same numbers.
	row, _ := server.bank.GetNightlyRun(rec.ID)
	var stats NightlyStats
	_ = json.Unmarshal([]byte(row.StatsJSON), &stats)
	if stats.TokensTotal != 300 || stats.TokensIn != 100 || stats.TokensOut != 200 {
		t.Errorf("stats mismatch: %+v", stats)
	}
	if row.Narrative == "" {
		t.Errorf("expected narrative to be persisted when narrativeFn returned text")
	}
	if row.Model != "fake-tier2" {
		t.Errorf("expected model=fake-tier2, got %q", row.Model)
	}
}

// TestNightlyRunner_TriggerHTTP_HappyPath — POST /nightly/trigger when no run
// is active returns 200 with the new id.
func TestNightlyRunner_TriggerHTTP_HappyPath(t *testing.T) {
	server, _ := nightlyTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/nightly/trigger", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	id, _ := body["id"].(string)
	if !strings.HasPrefix(id, "nightly-") {
		t.Errorf("expected nightly-... id, got %q", id)
	}
	if body["status"] != "completed" {
		t.Errorf("expected status=completed, got %v", body["status"])
	}
}

// TestNightlyRunner_EmitsCompletedEvent — runs emit a `nightly.completed`
// control-channel event with the right shape.
func TestNightlyRunner_EmitsCompletedEvent(t *testing.T) {
	_, runner := nightlyTestServer(t)
	rec, _ := runner.DoRun(context.Background())

	// Inspect ring buffer.
	events := runner.hub.ring.Recent(50)
	found := false
	for _, e := range events {
		if e.Type == "nightly.completed" {
			found = true
			if e.AdapterID != "sd-core-nightly" {
				t.Errorf("expected adapter_id=sd-core-nightly, got %q", e.AdapterID)
			}
			if e.Payload["run_id"] != rec.ID {
				t.Errorf("payload run_id mismatch")
			}
			if e.Payload["status"] != "completed" {
				t.Errorf("payload status mismatch")
			}
		}
	}
	if !found {
		t.Errorf("expected nightly.completed event in ring, got %d total events", len(events))
	}
}
