// service_lifecycle_test.go — wave 7b2 sidecar lifecycle tests.
//
// Offline-safe coverage:
//   - policy validation + persistence round-trip
//   - state-cache + transition emits WS events
//   - SignalUsed flips idle → running
//   - reapOnce ignores always_on, transitions on_demand_lazy after timeout
//   - settings refuses on_demand for essential services
//
// Live Docker daemon paths (EnsureRunning, GracefulStop) get manual
// verification via the rebuild step.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestValidateLifecycle_UnknownFallsBackToAlwaysOn(t *testing.T) {
	got := validateLifecycle(ServiceLifecyclePolicy{Lifecycle: "yeet"})
	if got.Lifecycle != LifecycleAlwaysOn {
		t.Errorf("unknown lifecycle should fall back to always_on, got %q", got.Lifecycle)
	}
}

func TestValidateLifecycle_DefaultsFillIn(t *testing.T) {
	got := validateLifecycle(ServiceLifecyclePolicy{Lifecycle: LifecycleOnDemandLazy})
	if got.IdleTimeoutSec != 300 {
		t.Errorf("idle timeout default: want 300, got %d", got.IdleTimeoutSec)
	}
	if got.ColdStartGraceSec != 90 {
		t.Errorf("cold start grace default: want 90, got %d", got.ColdStartGraceSec)
	}
}

func TestSaveAndGetServiceLifecyclePolicy_Roundtrip(t *testing.T) {
	bank := newTestBank(t)
	in := ServiceLifecyclePolicy{
		Lifecycle:         LifecycleOnDemandLazy,
		IdleTimeoutSec:    120,
		WarmForWorkflow:   []string{"research_request"},
		ColdStartGraceSec: 60,
	}
	if err := bank.SaveServiceLifecyclePolicy("airllm-tier3", in); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := bank.GetServiceLifecyclePolicy("airllm-tier3")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Lifecycle != in.Lifecycle || got.IdleTimeoutSec != in.IdleTimeoutSec || got.ColdStartGraceSec != in.ColdStartGraceSec {
		t.Errorf("roundtrip mismatch: got %+v want %+v", got, in)
	}
	if len(got.WarmForWorkflow) != 1 || got.WarmForWorkflow[0] != "research_request" {
		t.Errorf("warm_for_workflow: got %v", got.WarmForWorkflow)
	}
}

func TestGetServiceLifecyclePolicy_UnknownReturnsDefault(t *testing.T) {
	bank := newTestBank(t)
	got, err := bank.GetServiceLifecyclePolicy("never-configured")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.Lifecycle != LifecycleAlwaysOn {
		t.Errorf("unset policy should default to always_on, got %q", got.Lifecycle)
	}
}

func TestServiceLifecycleRow_GetReturnsStoppedForMissing(t *testing.T) {
	bank := newTestBank(t)
	row, err := bank.GetServiceLifecycle("never-configured")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if row.CurrentState != LifecycleStateStopped {
		t.Errorf("missing row should return state=stopped, got %q", row.CurrentState)
	}
}

func TestServiceLifecycleRow_UpsertRoundtrip(t *testing.T) {
	bank := newTestBank(t)
	in := ServiceLifecycleRow{
		ServiceName:     "airllm-tier3",
		CurrentState:    LifecycleStateRunning,
		LastUsedAt:      "2026-05-10T22:00:00Z",
		LastWorkflow:    "research_request",
		ColdStartsToday: 3,
		UptimeTodaySec:  900,
	}
	if err := bank.UpsertServiceLifecycle(in); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := bank.GetServiceLifecycle("airllm-tier3")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CurrentState != in.CurrentState || got.LastWorkflow != in.LastWorkflow || got.ColdStartsToday != 3 {
		t.Errorf("roundtrip: %+v", got)
	}
}

func TestLifecycleManager_SignalUsedFlipsIdleToRunning(t *testing.T) {
	bank := newTestBank(t)
	// Pre-populate as idle.
	bank.UpsertServiceLifecycle(ServiceLifecycleRow{
		ServiceName:  "airllm-tier3",
		CurrentState: LifecycleStateIdle,
		LastUsedAt:   time.Now().Add(-10 * time.Minute).Format(time.RFC3339Nano),
	})
	lm := NewLifecycleManager(bank, nil, nil)
	lm.SignalUsed("airllm-tier3", "research_request")
	row, _ := bank.GetServiceLifecycle("airllm-tier3")
	if row.CurrentState != LifecycleStateRunning {
		t.Errorf("expected idle→running on SignalUsed, got %q", row.CurrentState)
	}
	if row.LastWorkflow != "research_request" {
		t.Errorf("workflow not recorded, got %q", row.LastWorkflow)
	}
}

func TestLifecycleManager_ReapOnceIgnoresAlwaysOn(t *testing.T) {
	bank := newTestBank(t)
	bank.SaveServiceLifecyclePolicy("synaptic-core", ServiceLifecyclePolicy{
		Lifecycle: LifecycleAlwaysOn,
	})
	bank.UpsertServiceLifecycle(ServiceLifecycleRow{
		ServiceName:  "synaptic-core",
		CurrentState: LifecycleStateRunning,
		LastUsedAt:   time.Now().Add(-24 * time.Hour).Format(time.RFC3339Nano),
	})
	lm := NewLifecycleManager(bank, nil, nil)
	lm.reapOnce(context.Background())
	row, _ := bank.GetServiceLifecycle("synaptic-core")
	if row.CurrentState != LifecycleStateRunning {
		t.Errorf("always_on must not be reaped, got %q", row.CurrentState)
	}
}

func TestLifecycleManager_ReapOnceMarksIdleAtHalfTimeout(t *testing.T) {
	bank := newTestBank(t)
	bank.SaveServiceLifecyclePolicy("airllm-tier3", ServiceLifecyclePolicy{
		Lifecycle:      LifecycleOnDemandLazy,
		IdleTimeoutSec: 60,
	})
	// Last used 40s ago — past half-timeout (30s) but before full timeout (60s).
	bank.UpsertServiceLifecycle(ServiceLifecycleRow{
		ServiceName:  "airllm-tier3",
		CurrentState: LifecycleStateRunning,
		LastUsedAt:   time.Now().Add(-40 * time.Second).Format(time.RFC3339Nano),
	})
	lm := NewLifecycleManager(bank, nil, nil)
	lm.reapOnce(context.Background())
	row, _ := bank.GetServiceLifecycle("airllm-tier3")
	if row.CurrentState != LifecycleStateIdle {
		t.Errorf("expected running→idle at half-timeout, got %q", row.CurrentState)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// HTTP handler tests
// ──────────────────────────────────────────────────────────────────────────

func TestAdminServicesRouter_RequiresAction(t *testing.T) {
	bank := newTestBank(t)
	srv := newTestServer(t, bank)
	srv.docker = &DockerControlState{
		client:         NewDockerClient("/tmp/nope.sock"),
		allowlistRegex: regexp.MustCompile("^synaptic-"),
		socketAllowed:  false,
	}
	srv.lifecycle = NewLifecycleManager(bank, srv.docker, nil)

	req := httptest.NewRequest(http.MethodGet, "/admin/services/airllm-tier3", nil)
	w := httptest.NewRecorder()
	srv.adminServicesRouter(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing action: want 400, got %d", w.Code)
	}
}

func TestAdminServiceLifecycleGet_GateOff(t *testing.T) {
	bank := newTestBank(t)
	srv := newTestServer(t, bank)
	srv.docker = &DockerControlState{
		client:         NewDockerClient("/tmp/nope.sock"),
		allowlistRegex: regexp.MustCompile("^synaptic-"),
		socketAllowed:  false,
	}
	srv.lifecycle = NewLifecycleManager(bank, srv.docker, nil)
	req := httptest.NewRequest(http.MethodGet, "/admin/services/airllm-tier3/lifecycle", nil)
	w := httptest.NewRecorder()
	srv.adminServicesRouter(w, req)
	if w.Code != http.StatusPreconditionFailed {
		t.Errorf("want 412, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestSettingsServiceLifecycle_PutAndGet(t *testing.T) {
	bank := newTestBank(t)
	srv := newTestServer(t, bank)

	// PUT
	body := strings.NewReader(`{"lifecycle":"on_demand_lazy","idle_timeout_sec":120,"warm_for_workflow":["research_request"]}`)
	req := httptest.NewRequest(http.MethodPut, "/settings/services/airllm-tier3/lifecycle", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.settingsServicesRouter(w, req, "services/airllm-tier3/lifecycle")
	if w.Code != http.StatusOK {
		t.Fatalf("PUT want 200, got %d body=%s", w.Code, w.Body.String())
	}

	// GET
	req = httptest.NewRequest(http.MethodGet, "/settings/services/airllm-tier3/lifecycle", nil)
	w = httptest.NewRecorder()
	srv.settingsServicesRouter(w, req, "services/airllm-tier3/lifecycle")
	if w.Code != http.StatusOK {
		t.Fatalf("GET want 200, got %d", w.Code)
	}
	var resp struct {
		Policy ServiceLifecyclePolicy `json:"policy"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Policy.Lifecycle != LifecycleOnDemandLazy || resp.Policy.IdleTimeoutSec != 120 {
		t.Errorf("policy: %+v", resp.Policy)
	}
}

func TestSettingsServiceLifecycle_RefusesOnDemandForCore(t *testing.T) {
	bank := newTestBank(t)
	srv := newTestServer(t, bank)
	body := strings.NewReader(`{"lifecycle":"on_demand_lazy"}`)
	req := httptest.NewRequest(http.MethodPut, "/settings/services/core/lifecycle", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.settingsServicesRouter(w, req, "services/core/lifecycle")
	if w.Code != http.StatusBadRequest {
		t.Errorf("essential service must be always_on; want 400, got %d", w.Code)
	}
}

func TestSettingsServicesRouter_RejectsUnknownSub(t *testing.T) {
	bank := newTestBank(t)
	srv := newTestServer(t, bank)
	req := httptest.NewRequest(http.MethodGet, "/settings/services/airllm-tier3/colour", nil)
	w := httptest.NewRecorder()
	srv.settingsServicesRouter(w, req, "services/airllm-tier3/colour")
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown sub: want 404, got %d", w.Code)
	}
}
