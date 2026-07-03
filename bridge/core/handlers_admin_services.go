// handlers_admin_services.go — Wave 7b2 HTTP surface for the sidecar
// lifecycle manager.
//
// Endpoints:
//
//   GET  /admin/services/{name}/lifecycle      — current state + policy
//   POST /admin/services/{name}/ensure_running — idempotent start + wait-ready
//   POST /admin/services/{name}/use_signal     — fire-and-forget idle reset
//   PUT  /settings/services/{name}/lifecycle   — set policy
//   GET  /settings/services/{name}/lifecycle   — get policy
//
// All endpoints require the Docker gate (SD_ALLOW_DOCKER_SOCKET=1) since
// they manipulate container state under the hood. /lifecycle GET is the
// one read-only path; we still gate it because the underlying state is
// only meaningful when the Docker control plane is enabled.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// adminServicesRouter dispatches /admin/services/{name}/{action}.
// Registered in main.go.
func (s *Server) adminServicesRouter(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/services")
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		http.Error(w, "missing service name", http.StatusBadRequest)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	service := parts[0]
	if service == "" {
		http.Error(w, "missing service name", http.StatusBadRequest)
		return
	}
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	switch action {
	case "lifecycle":
		s.handleAdminServiceLifecycleGet(w, r, service)
	case "ensure_running":
		s.handleAdminServiceEnsureRunning(w, r, service)
	case "use_signal":
		s.handleAdminServiceUseSignal(w, r, service)
	case "":
		http.Error(w, "missing action; expected lifecycle|ensure_running|use_signal", http.StatusBadRequest)
	default:
		http.Error(w, "unknown action; expected lifecycle|ensure_running|use_signal", http.StatusNotFound)
	}
}

// handleAdminServiceLifecycleGet serves GET /admin/services/{name}/lifecycle.
// Returns the persisted state row + the configured policy. No 404 for
// unknown services — we return a stopped/default-policy shape so the
// frontend can render uniformly.
func (s *Server) handleAdminServiceLifecycleGet(w http.ResponseWriter, r *http.Request, service string) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET", "OPTIONS")
		return
	}
	if err := s.docker.gateOK(r.Context(), w); err != nil {
		return
	}
	if s.lifecycle == nil {
		http.Error(w, "lifecycle manager not initialised", http.StatusServiceUnavailable)
		return
	}
	row, err := s.lifecycle.GetState(service)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	policy, _ := s.bank.GetServiceLifecyclePolicy(service)
	writeJSON(w, http.StatusOK, map[string]any{
		"service": service,
		"state":   row,
		"policy":  policy,
	})
}

// handleAdminServiceEnsureRunning serves POST /admin/services/{name}/ensure_running.
// Body (optional): {"wait_ready": true, "timeout_sec": 120}. Idempotent.
func (s *Server) handleAdminServiceEnsureRunning(w http.ResponseWriter, r *http.Request, service string) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST", "OPTIONS")
		return
	}
	if err := s.docker.gateOK(r.Context(), w); err != nil {
		return
	}
	if s.lifecycle == nil {
		http.Error(w, "lifecycle manager not initialised", http.StatusServiceUnavailable)
		return
	}
	if !s.docker.nameAllowed("synaptic-" + service) {
		http.Error(w, "service not allowed by SD_DOCKER_SERVICE_ALLOWLIST", http.StatusForbidden)
		return
	}
	var body struct {
		WaitReady  bool `json:"wait_ready"`
		TimeoutSec int  `json:"timeout_sec"`
	}
	if r.ContentLength > 0 {
		_ = readJSON(r, &body)
	}
	policy, _ := s.bank.GetServiceLifecyclePolicy(service)
	timeout := time.Duration(body.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = time.Duration(policy.ColdStartGraceSec) * time.Second
	}
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	beforeRow, _ := s.lifecycle.GetState(service)
	startedJustNow := beforeRow.CurrentState != LifecycleStateRunning &&
		beforeRow.CurrentState != LifecycleStateIdle
	startedAt := time.Now().UTC()
	ensureCtx, cancel := context.WithTimeout(r.Context(), timeout+5*time.Second)
	defer cancel()
	if err := s.lifecycle.EnsureRunning(ensureCtx, service, timeout); err != nil {
		afterRow, _ := s.lifecycle.GetState(service)
		writeJSON(w, http.StatusGatewayTimeout, map[string]any{
			"service":           service,
			"state":             afterRow.CurrentState,
			"started_just_now":  startedJustNow,
			"elapsed_ms":        time.Since(startedAt).Milliseconds(),
			"error":             err.Error(),
		})
		return
	}
	afterRow, _ := s.lifecycle.GetState(service)
	s.auditWrite(AuditEntry{
		Operation:  "service_lifecycle_started",
		EntityType: "service",
		EntityID:   service,
		BeforeJSON: mustJSON(map[string]any{"state": beforeRow.CurrentState}),
		AfterJSON: mustJSON(map[string]any{
			"state":            afterRow.CurrentState,
			"started_just_now": startedJustNow,
		}),
		AdapterID: adapterIDFromRequest(r),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"service":          service,
		"state":            afterRow.CurrentState,
		"started_just_now": startedJustNow,
		"ready_at":         afterRow.StateChangedAt,
		"elapsed_ms":       time.Since(startedAt).Milliseconds(),
	})
}

// handleAdminServiceUseSignal serves POST /admin/services/{name}/use_signal.
// Optional body: {"workflow":"nightly_run|research_request|..."}. Always
// returns 200; lifecycle bookkeeping is best-effort.
func (s *Server) handleAdminServiceUseSignal(w http.ResponseWriter, r *http.Request, service string) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST", "OPTIONS")
		return
	}
	if err := s.docker.gateOK(r.Context(), w); err != nil {
		return
	}
	if s.lifecycle == nil {
		writeJSON(w, http.StatusOK, map[string]any{"acknowledged": false, "reason": "lifecycle manager not initialised"})
		return
	}
	var body struct {
		Workflow string `json:"workflow"`
	}
	if r.ContentLength > 0 {
		_ = readJSON(r, &body)
	}
	s.lifecycle.SignalUsed(service, body.Workflow)
	writeJSON(w, http.StatusOK, map[string]any{
		"acknowledged": true,
		"service":      service,
		"workflow":     body.Workflow,
	})
}

// ──────────────────────────────────────────────────────────────────────────
// Settings glue — /settings/services/{name}/lifecycle GET/PUT
// ──────────────────────────────────────────────────────────────────────────

// settingsServicesRouter handles the /settings/services/{name}/lifecycle
// path family. Routed inside settingsRoot when rest starts with
// "services/".
func (s *Server) settingsServicesRouter(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(strings.TrimPrefix(rest, "services/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		http.Error(w, "expected /settings/services/{name}/lifecycle", http.StatusBadRequest)
		return
	}
	service, sub := parts[0], parts[1]
	if sub != "lifecycle" {
		http.Error(w, "unknown sub-resource; only `lifecycle` is supported", http.StatusNotFound)
		return
	}
	s.handleSettingsServiceLifecycle(w, r, service)
}

func (s *Server) handleSettingsServiceLifecycle(w http.ResponseWriter, r *http.Request, service string) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if s.bank == nil {
		http.Error(w, "bank disabled", http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		policy, err := s.bank.GetServiceLifecyclePolicy(service)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"service": service,
			"policy":  policy,
		})
	case http.MethodPut:
		var body ServiceLifecyclePolicy
		if err := readJSON(r, &body); err != nil {
			http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
			return
		}
		// Refuse to put core/ollama on on-demand lifecycles — they're
		// essential. Frontend should disable the dropdown for those
		// services; this is the second line of defence.
		if (service == "core" || service == "ollama") && body.Lifecycle != "" && body.Lifecycle != LifecycleAlwaysOn {
			http.Error(w, "essential services (core, ollama) must be always_on", http.StatusBadRequest)
			return
		}
		if err := s.bank.SaveServiceLifecyclePolicy(service, body); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.auditWrite(AuditEntry{
			Operation:  "upsert",
			EntityType: "setting",
			EntityID:   lifecyclePolicyKey(service),
			AfterJSON:  mustJSON(body),
			AdapterID:  adapterIDFromRequest(r),
		})
		policy, _ := s.bank.GetServiceLifecyclePolicy(service)
		writeJSON(w, http.StatusOK, map[string]any{
			"service": service,
			"policy":  policy,
		})
	default:
		methodNotAllowed(w, "GET", "PUT", "OPTIONS")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Small helpers used by both /admin/services/* and lifecycle manager
// ──────────────────────────────────────────────────────────────────────────

// Compile-time silence for the imports when the package is built in
// tests that don't reach every path. Cheap insurance.
var _ = json.Marshal
var _ = errors.New
