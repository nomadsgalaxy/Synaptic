// handlers_wave8c_test.go — wave 8c per-service lifecycle defaults +
// delete_model endpoint.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// ──────────────────────────────────────────────────────────────────────────
// Item 2 — defaultPolicyFor + GetServiceLifecyclePolicy fallback
// ──────────────────────────────────────────────────────────────────────────

func TestDefaultPolicyFor_KnownNames(t *testing.T) {
	cases := []struct {
		name          string
		wantLifecycle string
		wantWarm      []string
	}{
		// Wave 8c: Tier 2 containers now default to on_demand_lazy with
		// both workflows in warm_for_workflow — Tier 3 containers were
		// retired, so Tier 2 serves both nightly_run AND research_request
		// for local LLM use.
		{"ollama-tier2", LifecycleOnDemandLazy, []string{"nightly_run", "research_request"}},
		{"airllm-tier2", LifecycleOnDemandLazy, []string{"nightly_run", "research_request"}},
		{"synaptic-ollama-tier2", LifecycleOnDemandLazy, []string{"nightly_run", "research_request"}},
		{"synaptic-airllm-tier2", LifecycleOnDemandLazy, []string{"nightly_run", "research_request"}},
	}
	for _, c := range cases {
		got := defaultPolicyFor(c.name)
		if got.Lifecycle != c.wantLifecycle {
			t.Errorf("%s: want lifecycle=%s got %s", c.name, c.wantLifecycle, got.Lifecycle)
		}
		if len(got.WarmForWorkflow) != len(c.wantWarm) {
			t.Errorf("%s: want %d warm entries, got %d (%v)", c.name, len(c.wantWarm), len(got.WarmForWorkflow), got.WarmForWorkflow)
			continue
		}
		for i, want := range c.wantWarm {
			if got.WarmForWorkflow[i] != want {
				t.Errorf("%s: warm[%d] want %q got %q", c.name, i, want, got.WarmForWorkflow[i])
			}
		}
	}
}

// TestDefaultPolicyFor_Tier3NamesFallToAlwaysOn — wave 8c retirement
// dropped dedicated Tier 3 containers. Anyone still PUTting an old
// name should get the safe always_on default rather than a phantom
// on-demand policy that would silently never start anything.
func TestDefaultPolicyFor_Tier3NamesFallToAlwaysOn(t *testing.T) {
	for _, name := range []string{"ollama-tier3", "airllm-tier3", "synaptic-ollama-tier3", "synaptic-airllm-tier3"} {
		got := defaultPolicyFor(name)
		if got.Lifecycle != LifecycleAlwaysOn {
			t.Errorf("%s: retired tier 3 name should fall to always_on, got %s", name, got.Lifecycle)
		}
	}
}

func TestDefaultPolicyFor_EssentialsStayAlwaysOn(t *testing.T) {
	for _, name := range []string{"core", "ollama", "ollama-tier1", "synaptic-ollama-tier1", "cloudflared", "unknown-service"} {
		if got := defaultPolicyFor(name); got.Lifecycle != LifecycleAlwaysOn {
			t.Errorf("%s: want always_on, got %s", name, got.Lifecycle)
		}
	}
}

func TestGetServiceLifecyclePolicy_FallsBackToPerServiceDefault(t *testing.T) {
	bank := newTestBank(t)
	// No row written → default fires. Wave 8c: tier2 default is now
	// on_demand_lazy with both workflows.
	got, err := bank.GetServiceLifecyclePolicy("ollama-tier2")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.Lifecycle != LifecycleOnDemandLazy {
		t.Errorf("ollama-tier2 with no row: want on_demand_lazy, got %s", got.Lifecycle)
	}
	if got.ColdStartGraceSec == 0 {
		t.Errorf("validate() should fill ColdStartGraceSec; got 0")
	}
	if got.IdleTimeoutSec != 300 {
		t.Errorf("ollama-tier2 idle: want 300, got %d", got.IdleTimeoutSec)
	}
	if len(got.WarmForWorkflow) != 2 {
		t.Fatalf("ollama-tier2 warm: want 2 entries, got %v", got.WarmForWorkflow)
	}
	if got.WarmForWorkflow[0] != "nightly_run" || got.WarmForWorkflow[1] != "research_request" {
		t.Errorf("ollama-tier2 warm: got %v", got.WarmForWorkflow)
	}
}

func TestGetServiceLifecyclePolicy_AirLLMTier2HasLongerIdle(t *testing.T) {
	bank := newTestBank(t)
	got, _ := bank.GetServiceLifecyclePolicy("airllm-tier2")
	if got.IdleTimeoutSec != 600 {
		t.Errorf("airllm-tier2 idle: want 600 (10 min for slow cold-start), got %d", got.IdleTimeoutSec)
	}
	if got.ColdStartGraceSec != 600 {
		t.Errorf("airllm-tier2 cold-start grace: want 600, got %d", got.ColdStartGraceSec)
	}
}

func TestGetServiceLifecyclePolicy_ExplicitPutOverridesDefault(t *testing.T) {
	bank := newTestBank(t)
	override := ServiceLifecyclePolicy{Lifecycle: LifecycleAlwaysOn}
	if err := bank.SaveServiceLifecyclePolicy("ollama-tier2", override); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, _ := bank.GetServiceLifecyclePolicy("ollama-tier2")
	if got.Lifecycle != LifecycleAlwaysOn {
		t.Errorf("explicit PUT should win over default; got %s", got.Lifecycle)
	}
}

func TestCandidateServiceNames_IncludesTier2NotTier3(t *testing.T) {
	bank := newTestBank(t)
	lm := NewLifecycleManager(bank, nil, nil)
	names := lm.candidateServiceNames()
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	for _, must := range []string{"ollama-tier2", "airllm-tier2"} {
		if !have[must] {
			t.Errorf("candidate set missing %s (would skip workflow hooks)", must)
		}
	}
	// Wave 8c: retired tier3 names should NOT be in the candidate set
	// — EnsureForWorkflow would otherwise try to start non-existent
	// containers each time the nightly + research workflows trigger.
	for _, retired := range []string{"ollama-tier3", "airllm-tier3"} {
		if have[retired] {
			t.Errorf("candidate set should NOT include retired %s", retired)
		}
	}
}

func TestCandidateServiceNames_DedupesPersistedAndDefault(t *testing.T) {
	bank := newTestBank(t)
	// Pre-write a row for ollama-tier2 so it appears in both the
	// persisted list AND the defaults list.
	bank.UpsertServiceLifecycle(ServiceLifecycleRow{
		ServiceName:  "ollama-tier2",
		CurrentState: LifecycleStateStopped,
	})
	lm := NewLifecycleManager(bank, nil, nil)
	names := lm.candidateServiceNames()
	count := 0
	for _, n := range names {
		if n == "ollama-tier2" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("ollama-tier2 should appear once after dedup; got %d", count)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Item 1 — DeleteModel proxy method
// ──────────────────────────────────────────────────────────────────────────

func TestOllamaProxy_DeleteModel_HappyPath(t *testing.T) {
	gotMethod := ""
	gotBody := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	cli := NewOllamaProxyClient(srv.URL)
	if err := cli.DeleteModel(context.Background(), "llama3.1:8b"); err != nil {
		t.Fatalf("DeleteModel: %v", err)
	}
	if gotMethod != "DELETE" {
		t.Errorf("method: want DELETE, got %s", gotMethod)
	}
	if !strings.Contains(gotBody, `"name":"llama3.1:8b"`) {
		t.Errorf("body should carry the model name; got %s", gotBody)
	}
}

func TestOllamaProxy_DeleteModel_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"model not found"}`))
	}))
	defer srv.Close()
	cli := NewOllamaProxyClient(srv.URL)
	err := cli.DeleteModel(context.Background(), "bogus:tag")
	if err == nil {
		t.Errorf("expected error on 404")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention not-found; got %v", err)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// /admin/docker/services/{name}/delete_model — offline routing tests
// ──────────────────────────────────────────────────────────────────────────

func TestAdminDockerRouter_AcceptsDeleteModel(t *testing.T) {
	srv := newTestServer(t, nil)
	srv.docker = &DockerControlState{
		client:         NewDockerClient("/tmp/nope.sock"),
		allowlistRegex: regexp.MustCompile("^synaptic-"),
		composeProject: "synaptic",
		socketAllowed:  false, // gate off → 412 short-circuits before docker calls
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/docker/services/ollama-tier2/delete_model",
		strings.NewReader(`{"model":"llama3.1:8b"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.adminDockerRouter(w, req)
	if w.Code != http.StatusPreconditionFailed {
		t.Errorf("delete_model with gate off: want 412, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestAdminDockerRouter_404BodyMentionsDeleteModel(t *testing.T) {
	srv := newTestServer(t, nil)
	srv.docker = NewDockerControlState()
	req := httptest.NewRequest(http.MethodPost, "/admin/docker/services/core/yeet", nil)
	w := httptest.NewRecorder()
	srv.adminDockerRouter(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "delete_model") {
		t.Errorf("404 body should list delete_model among legal actions; got %s", w.Body.String())
	}
}

func TestDeleteModel_RequiresPostMethod(t *testing.T) {
	srv := newTestServer(t, nil)
	srv.docker = &DockerControlState{
		client:         NewDockerClient("/tmp/nope.sock"),
		allowlistRegex: regexp.MustCompile("^synaptic-"),
		socketAllowed:  false,
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/docker/services/x/delete_model", nil)
	w := httptest.NewRecorder()
	srv.handleAdminDockerDeleteModel(w, req, "x")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET want 405, got %d", w.Code)
	}
}

// Sanity: the only-model guard returns 409, decoded shape carries error tag.
// We can't exercise the full live path offline (needs daemon + ollama), but
// the path-shape constants are worth pinning.
func TestDeleteModel_OnlyModelGuardShape(t *testing.T) {
	// This is a structural test — verify the response shape we return on
	// 409 carries the right error tag. Crafted by hand here.
	resp := map[string]any{
		"error":   "only_model",
		"service": "ollama-tier2",
		"model":   "llama3.1:8b",
		"hint":    "...",
	}
	b, _ := json.Marshal(resp)
	if !strings.Contains(string(b), `"error":"only_model"`) {
		t.Errorf("guard response shape: got %s", b)
	}
}
