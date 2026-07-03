// service_lifecycle.go — Wave 7b2 on-demand sidecar lifecycle.
//
// Tracks per-managed-service state (stopped / starting / running / idle /
// stopping / crashed) + applies one of three policies:
//
//   - always_on:        never auto-stops; idle reaper ignores it
//   - on_demand_lazy:   starts on ensure_running; idle reaper stops after
//                       idle_timeout_sec of no use
//   - on_demand_burst:  starts before a workflow (Phase 0b, research call);
//                       stopped explicitly when the workflow signals
//                       burst-complete
//
// State is the union of (Docker container state from the daemon, our
// own "is being used" tracking). The daemon owns running/stopped facts;
// we own "ready", "idle", and "stopping (intent)" — none of which the
// daemon exposes.
//
// Persistence: per-service rows in `service_lifecycle` survive restarts
// so the idle reaper can pick up where it left off. Policy itself lives
// in the settings table under `services.<name>.lifecycle`.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────
// Settings shape
// ──────────────────────────────────────────────────────────────────────────

// ServiceLifecyclePolicy is the per-service lifecycle setting. Stored in
// the settings table as JSON under key `services.<name>.lifecycle`.
type ServiceLifecyclePolicy struct {
	// Lifecycle is one of always_on | on_demand_lazy | on_demand_burst.
	// Validation happens at PUT time; unknown values fall back to
	// always_on for safety.
	Lifecycle string `json:"lifecycle"`
	// IdleTimeoutSec applies to on_demand_lazy only. 0 = use the default
	// 300 (5 minutes). Ignored for the other two policies.
	IdleTimeoutSec int `json:"idle_timeout_sec,omitempty"`
	// WarmForWorkflow lists workflow names that auto-start this service
	// when they begin. Currently advisory — the workflow code calls
	// EnsureRunning explicitly; this metadata is surfaced in the UI for
	// transparency.
	WarmForWorkflow []string `json:"warm_for_workflow,omitempty"`
	// ColdStartGraceSec bounds how long EnsureRunning blocks waiting for
	// the container's healthz to flip to OK. 0 = use the default 90.
	ColdStartGraceSec int `json:"cold_start_grace_sec,omitempty"`
}

// Lifecycle policy strings.
const (
	LifecycleAlwaysOn       = "always_on"
	LifecycleOnDemandLazy   = "on_demand_lazy"
	LifecycleOnDemandBurst  = "on_demand_burst"
)

// State strings — match the column values for round-tripping.
const (
	LifecycleStateStopped  = "stopped"
	LifecycleStateStarting = "starting"
	LifecycleStateRunning  = "running"
	LifecycleStateIdle     = "idle"
	LifecycleStateStopping = "stopping"
	LifecycleStateCrashed  = "crashed"
)

// DefaultLifecyclePolicy returns the safe default for a service that
// hasn't been configured: always_on. Means uninitialised services keep
// running like they always did before this wave.
func DefaultLifecyclePolicy() ServiceLifecyclePolicy {
	return ServiceLifecyclePolicy{
		Lifecycle:         LifecycleAlwaysOn,
		IdleTimeoutSec:    300,
		ColdStartGraceSec: 90,
	}
}

// defaultPolicyFor returns the initial-value policy for a known managed
// service. Users PUTting an explicit policy still wins; this only fills
// the gap when no settings row exists yet.
//
// Wave 8c update: dedicated Tier 3 containers were retired — a Tier 2
// model serves both nightly_run AND research_request workflows for the
// local case (remote Tier 3 still routes to OpenAI/Anthropic/etc. and
// doesn't touch lifecycle). The tier-2 default flipped from
// on_demand_burst to on_demand_lazy so research calls don't trigger an
// immediate stop the way burst-completion does.
//
// Name normalisation: accepts either the compose service form
// ("ollama-tier2") or the container-name form ("synaptic-ollama-tier2")
// so callers don't have to remember which surface uses which.
func defaultPolicyFor(name string) ServiceLifecyclePolicy {
	short := strings.TrimPrefix(name, "synaptic-")
	switch short {
	case "ollama-tier2":
		// Lazy with a 5-min idle: nightly_run warms it; research_request
		// keeps it warm via SignalUsed each time the user runs a query;
		// the idle reaper stops it when nothing's used it for 5 minutes.
		return ServiceLifecyclePolicy{
			Lifecycle:         LifecycleOnDemandLazy,
			IdleTimeoutSec:    300,
			WarmForWorkflow:   []string{"nightly_run", "research_request"},
			ColdStartGraceSec: 90,
		}
	case "airllm-tier2":
		// Same shape, longer idle window because AirLLM cold-starts are
		// multi-minute (layer streaming from disk). 10 min idle vs 5 for
		// Ollama balances "don't pin RAM forever" against "don't pay
		// cold-start tax twice in quick succession". cold_start_grace
		// also bumped to 600s to cover first-pass model download.
		return ServiceLifecyclePolicy{
			Lifecycle:         LifecycleOnDemandLazy,
			IdleTimeoutSec:    600,
			WarmForWorkflow:   []string{"nightly_run", "research_request"},
			ColdStartGraceSec: 600,
		}
	}
	// core, ollama, ollama-tier1, cloudflared, anything unrecognised:
	// always_on. Tier 1 specifically must stay always_on (realtime
	// ingest can't tolerate cold-start latency).
	return DefaultLifecyclePolicy()
}

func lifecyclePolicyKey(serviceName string) string {
	return "services." + serviceName + ".lifecycle"
}

// validateLifecycle returns the policy after normalisation. Unknown
// values fall through to LifecycleAlwaysOn so a typo never silently
// auto-stops a critical service.
func validateLifecycle(p ServiceLifecyclePolicy) ServiceLifecyclePolicy {
	switch p.Lifecycle {
	case LifecycleAlwaysOn, LifecycleOnDemandLazy, LifecycleOnDemandBurst:
		// known
	default:
		p.Lifecycle = LifecycleAlwaysOn
	}
	if p.IdleTimeoutSec <= 0 {
		p.IdleTimeoutSec = 300
	}
	if p.ColdStartGraceSec <= 0 {
		p.ColdStartGraceSec = 90
	}
	return p
}

// ──────────────────────────────────────────────────────────────────────────
// Bank methods — service_lifecycle table I/O
// ──────────────────────────────────────────────────────────────────────────

// ServiceLifecycleRow is the persisted per-service state.
type ServiceLifecycleRow struct {
	ServiceName     string `json:"service_name"`
	CurrentState    string `json:"current_state"`
	StateChangedAt  string `json:"state_changed_at,omitempty"`
	LastUsedAt      string `json:"last_used_at,omitempty"`
	LastWorkflow    string `json:"last_workflow,omitempty"`
	UptimeTodaySec  int    `json:"uptime_today_sec"`
	ColdStartsToday int    `json:"cold_starts_today"`
	CrashReason     string `json:"crash_reason,omitempty"`
	UpdatedAt       string `json:"updated_at"`
}

// GetServiceLifecycle returns the row, or a zero-value row with
// CurrentState=stopped when no row exists yet.
func (b *Bank) GetServiceLifecycle(name string) (ServiceLifecycleRow, error) {
	var row ServiceLifecycleRow
	if b == nil {
		return row, errors.New("bank not enabled")
	}
	err := b.db.QueryRow(
		`SELECT service_name, current_state, state_changed_at, last_used_at,
		        last_workflow, uptime_today_sec, cold_starts_today, crash_reason, updated_at
		 FROM service_lifecycle WHERE service_name = ?`, name,
	).Scan(&row.ServiceName, &row.CurrentState, &row.StateChangedAt, &row.LastUsedAt,
		&row.LastWorkflow, &row.UptimeTodaySec, &row.ColdStartsToday, &row.CrashReason, &row.UpdatedAt)
	if err != nil {
		// Treat ErrNoRows as "no entry yet — default state is stopped".
		if strings.Contains(err.Error(), "no rows") {
			return ServiceLifecycleRow{
				ServiceName:  name,
				CurrentState: LifecycleStateStopped,
			}, nil
		}
		return row, err
	}
	return row, nil
}

// UpsertServiceLifecycle persists the row. Callers set CurrentState +
// optionally LastUsedAt/LastWorkflow/etc. UpdatedAt is always bumped.
func (b *Bank) UpsertServiceLifecycle(row ServiceLifecycleRow) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	if row.ServiceName == "" {
		return errors.New("service_name required")
	}
	now := nowUTC()
	row.UpdatedAt = now
	if row.StateChangedAt == "" {
		row.StateChangedAt = now
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_, err := b.db.Exec(`
INSERT INTO service_lifecycle (
    service_name, current_state, state_changed_at, last_used_at,
    last_workflow, uptime_today_sec, cold_starts_today, crash_reason, updated_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(service_name) DO UPDATE SET
    current_state      = excluded.current_state,
    state_changed_at   = excluded.state_changed_at,
    last_used_at       = excluded.last_used_at,
    last_workflow      = excluded.last_workflow,
    uptime_today_sec   = excluded.uptime_today_sec,
    cold_starts_today  = excluded.cold_starts_today,
    crash_reason       = excluded.crash_reason,
    updated_at         = excluded.updated_at`,
		row.ServiceName, row.CurrentState, row.StateChangedAt, row.LastUsedAt,
		row.LastWorkflow, row.UptimeTodaySec, row.ColdStartsToday, row.CrashReason, row.UpdatedAt,
	)
	return err
}

// ListServiceLifecycle returns every row. Used by the idle reaper to
// scan all managed services + by the Services panel to render the
// lifecycle column.
func (b *Bank) ListServiceLifecycle() ([]ServiceLifecycleRow, error) {
	if b == nil {
		return nil, errors.New("bank not enabled")
	}
	rows, err := b.db.Query(
		`SELECT service_name, current_state, state_changed_at, last_used_at,
		        last_workflow, uptime_today_sec, cold_starts_today, crash_reason, updated_at
		 FROM service_lifecycle ORDER BY service_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ServiceLifecycleRow{}
	for rows.Next() {
		var r ServiceLifecycleRow
		if err := rows.Scan(&r.ServiceName, &r.CurrentState, &r.StateChangedAt, &r.LastUsedAt,
			&r.LastWorkflow, &r.UptimeTodaySec, &r.ColdStartsToday, &r.CrashReason, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ──────────────────────────────────────────────────────────────────────────
// Settings glue — per-service lifecycle policy
// ──────────────────────────────────────────────────────────────────────────

// GetServiceLifecyclePolicy reads the policy from settings. Returns the
// default when no row exists.
func (b *Bank) GetServiceLifecyclePolicy(name string) (ServiceLifecyclePolicy, error) {
	if b == nil {
		return defaultPolicyFor(name), nil
	}
	raw, ok, err := b.GetSetting(lifecyclePolicyKey(name))
	if err != nil {
		return defaultPolicyFor(name), err
	}
	if !ok || raw == "" {
		// No row written yet — use the per-service default rather than
		// always_on. Validate normalises the result so any default
		// missing IdleTimeoutSec/ColdStartGraceSec gets the safe values.
		return validateLifecycle(defaultPolicyFor(name)), nil
	}
	var p ServiceLifecyclePolicy
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return defaultPolicyFor(name), err
	}
	return validateLifecycle(p), nil
}

// SaveServiceLifecyclePolicy persists the policy (after normalisation).
func (b *Bank) SaveServiceLifecyclePolicy(name string, p ServiceLifecyclePolicy) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	p = validateLifecycle(p)
	buf, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return b.SetSetting(lifecyclePolicyKey(name), string(buf))
}

// ──────────────────────────────────────────────────────────────────────────
// Lifecycle manager — the state-machine driver
// ──────────────────────────────────────────────────────────────────────────

// LifecycleManager owns the in-memory state cache + the idle reaper
// goroutine. Constructed in main.go after the bank + docker control
// state are ready.
type LifecycleManager struct {
	bank   *Bank
	docker *DockerControlState
	hub    *Hub

	mu sync.Mutex
	// stateCache mirrors the persisted rows for read-only fast paths.
	stateCache map[string]ServiceLifecycleRow
	// stopReaper is closed to signal shutdown of the idle goroutine.
	stopReaper chan struct{}
}

// NewLifecycleManager wires the manager. Doesn't start the reaper —
// caller starts it via StartIdleReaper.
func NewLifecycleManager(bank *Bank, docker *DockerControlState, hub *Hub) *LifecycleManager {
	return &LifecycleManager{
		bank:       bank,
		docker:     docker,
		hub:        hub,
		stateCache: map[string]ServiceLifecycleRow{},
		stopReaper: make(chan struct{}),
	}
}

// GetState returns the latest persisted lifecycle row (defaults to
// stopped). Cheap read — first checks the in-memory cache.
func (lm *LifecycleManager) GetState(name string) (ServiceLifecycleRow, error) {
	if lm == nil || lm.bank == nil {
		return ServiceLifecycleRow{ServiceName: name, CurrentState: LifecycleStateStopped}, nil
	}
	lm.mu.Lock()
	if row, ok := lm.stateCache[name]; ok {
		lm.mu.Unlock()
		return row, nil
	}
	lm.mu.Unlock()
	row, err := lm.bank.GetServiceLifecycle(name)
	if err != nil {
		return row, err
	}
	lm.mu.Lock()
	lm.stateCache[name] = row
	lm.mu.Unlock()
	return row, nil
}

// transition writes the new state to bank + cache and fans out a WS
// event. emitType is the WS event type (e.g. "service_lifecycle.transitioned").
func (lm *LifecycleManager) transition(name string, newState string, mutator func(*ServiceLifecycleRow)) {
	if lm == nil || lm.bank == nil {
		return
	}
	row, _ := lm.bank.GetServiceLifecycle(name)
	if row.ServiceName == "" {
		row.ServiceName = name
	}
	prev := row.CurrentState
	if prev == "" {
		prev = LifecycleStateStopped
	}
	row.CurrentState = newState
	row.StateChangedAt = nowUTC()
	if mutator != nil {
		mutator(&row)
	}
	_ = lm.bank.UpsertServiceLifecycle(row)
	lm.mu.Lock()
	lm.stateCache[name] = row
	lm.mu.Unlock()
	if lm.hub != nil {
		lm.hub.Fanout(Event{
			SchemaVersion: SchemaVersion,
			Type:          "service_lifecycle_transitioned",
			Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
			AdapterID:     "sd-core-lifecycle",
			Payload: map[string]any{
				"service": name,
				"from":    prev,
				"to":      newState,
			},
		})
	}
}

// SignalProviderUse bumps last_used_at on every candidate non-always_on
// service. Called by the nightly pipeline (via pipelineContext) BEFORE
// each LLM Chat call, so the idle reaper doesn't stop an on-demand-lazy
// sidecar mid-inference. The lazy reaper computes idle from
// last_used_at, and a long AirLLM Chat call (~5-15 min for 70B at
// 1-3 tok/s) would otherwise blow past the 10-min idle window even
// though the container is actively serving the call.
//
// Fire-and-forget. Iterates `candidateServiceNames` so always_on
// services (core, ollama-tier1, anything unrecognised) get a no-op
// rather than a phantom row creation. Workflow is recorded on the
// signaled rows for retroactive "what was using Tier 2 at 03:14" audit
// queries.
func (lm *LifecycleManager) SignalProviderUse(workflow string) {
	if lm == nil || lm.bank == nil {
		return
	}
	for _, name := range lm.candidateServiceNames() {
		policy, _ := lm.bank.GetServiceLifecyclePolicy(name)
		if policy.Lifecycle == LifecycleAlwaysOn {
			continue
		}
		lm.SignalUsed(name, workflow)
	}
}

// SignalUsed bumps last_used_at + last_workflow. If the service was in
// idle state, transitions back to running. Fire-and-forget — used by
// providers after each successful call.
func (lm *LifecycleManager) SignalUsed(name, workflow string) {
	if lm == nil || lm.bank == nil {
		return
	}
	row, _ := lm.bank.GetServiceLifecycle(name)
	if row.ServiceName == "" {
		row.ServiceName = name
	}
	row.LastUsedAt = nowUTC()
	if workflow != "" {
		row.LastWorkflow = workflow
	}
	wasIdle := row.CurrentState == LifecycleStateIdle
	if wasIdle {
		row.CurrentState = LifecycleStateRunning
		row.StateChangedAt = nowUTC()
	}
	_ = lm.bank.UpsertServiceLifecycle(row)
	lm.mu.Lock()
	lm.stateCache[name] = row
	lm.mu.Unlock()
	if wasIdle && lm.hub != nil {
		lm.hub.Fanout(Event{
			SchemaVersion: SchemaVersion,
			Type:          "service_lifecycle_transitioned",
			Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
			AdapterID:     "sd-core-lifecycle",
			Payload: map[string]any{
				"service": name,
				"from":    LifecycleStateIdle,
				"to":      LifecycleStateRunning,
				"reason":  "use_signal",
			},
		})
	}
}

// SignalBurstComplete stops the service when its policy is on_demand_burst.
// No-op for the other policies — they manage their own lifecycle.
func (lm *LifecycleManager) SignalBurstComplete(ctx context.Context, name, workflow string) error {
	if lm == nil || lm.bank == nil {
		return nil
	}
	policy, _ := lm.bank.GetServiceLifecyclePolicy(name)
	if policy.Lifecycle != LifecycleOnDemandBurst {
		return nil
	}
	return lm.GracefulStop(ctx, name, "burst_complete:"+workflow)
}

// EnsureRunning is idempotent. Resolves the container, checks daemon
// state, starts if stopped, waits up to coldStartGrace for /healthz to
// flip ready (when the container declares a healthcheck), and updates
// the lifecycle row throughout.
//
// Returns nil + sets state=running on success. Returns an error +
// leaves state=stopped (or crashed) if the cold-start grace window
// expires.
func (lm *LifecycleManager) EnsureRunning(ctx context.Context, name string, timeout time.Duration) error {
	if lm == nil || lm.docker == nil || lm.docker.client == nil {
		return errors.New("lifecycle manager not initialised")
	}
	containerName := name
	if lm.docker != nil {
		// resolveContainerName lives on Server; here we go through Docker
		// directly. Try treating `name` as a compose service first; fall
		// back to using it as the literal container name.
		ctrs, _ := lm.docker.client.ListContainers(ctx, map[string]string{
			"com.docker.compose.project": lm.docker.composeProject,
			"com.docker.compose.service": name,
		})
		if len(ctrs) > 0 {
			containerName = primaryName(ctrs[0].Names)
		}
	}
	insp, err := lm.docker.client.InspectContainer(ctx, containerName)
	if err != nil {
		return fmt.Errorf("ensure_running %s: inspect: %w", name, err)
	}
	if insp.State.Running {
		// Already running. Sync our state to "running" if cache is stale.
		row, _ := lm.GetState(name)
		if row.CurrentState != LifecycleStateRunning && row.CurrentState != LifecycleStateIdle {
			lm.transition(name, LifecycleStateRunning, nil)
		}
		return nil
	}
	// Transition to starting + start the container.
	lm.transition(name, LifecycleStateStarting, func(r *ServiceLifecycleRow) {
		r.ColdStartsToday++
	})
	if err := lm.docker.client.StartContainer(ctx, containerName); err != nil {
		lm.transition(name, LifecycleStateCrashed, func(r *ServiceLifecycleRow) {
			r.CrashReason = err.Error()
		})
		return fmt.Errorf("ensure_running %s: start: %w", name, err)
	}
	// Wait for /healthz (or the container's healthcheck) to flip. We
	// poll the daemon's inspect every 2s.
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		insp, err = lm.docker.client.InspectContainer(ctx, containerName)
		if err == nil && insp.State.Running {
			// Healthy if no healthcheck declared OR explicit healthy status.
			if insp.State.Health.Status == "" || insp.State.Health.Status == "healthy" {
				lm.transition(name, LifecycleStateRunning, func(r *ServiceLifecycleRow) {
					r.LastUsedAt = nowUTC()
				})
				if lm.hub != nil {
					lm.hub.Fanout(Event{
						SchemaVersion: SchemaVersion,
						Type:          "service_lifecycle_ready",
						Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
						AdapterID:     "sd-core-lifecycle",
						Payload: map[string]any{
							"service": name,
						},
					})
				}
				return nil
			}
			if insp.State.Health.Status == "unhealthy" {
				lm.transition(name, LifecycleStateCrashed, func(r *ServiceLifecycleRow) {
					r.CrashReason = "healthcheck reported unhealthy"
				})
				return fmt.Errorf("ensure_running %s: unhealthy", name)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	lm.transition(name, LifecycleStateCrashed, func(r *ServiceLifecycleRow) {
		r.CrashReason = "cold start grace expired"
	})
	return fmt.Errorf("ensure_running %s: timed out after %s", name, timeout)
}

// GracefulStop transitions the service to stopping, calls the daemon
// stop with a 10s grace, then marks stopped. Reason is recorded in the
// audit + the lifecycle row for observability.
func (lm *LifecycleManager) GracefulStop(ctx context.Context, name, reason string) error {
	if lm == nil || lm.docker == nil || lm.docker.client == nil {
		return errors.New("lifecycle manager not initialised")
	}
	containerName := name
	ctrs, _ := lm.docker.client.ListContainers(ctx, map[string]string{
		"com.docker.compose.project": lm.docker.composeProject,
		"com.docker.compose.service": name,
	})
	if len(ctrs) > 0 {
		containerName = primaryName(ctrs[0].Names)
	}
	lm.transition(name, LifecycleStateStopping, func(r *ServiceLifecycleRow) {
		r.CrashReason = "" // explicit stop is not a crash
	})
	if err := lm.docker.client.StopContainer(ctx, containerName, 10); err != nil {
		return fmt.Errorf("graceful_stop %s: %w", name, err)
	}
	lm.transition(name, LifecycleStateStopped, func(r *ServiceLifecycleRow) {
		r.LastWorkflow = reason
	})
	if lm.hub != nil {
		lm.hub.Fanout(Event{
			SchemaVersion: SchemaVersion,
			Type:          "service_lifecycle_stopped",
			Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
			AdapterID:     "sd-core-lifecycle",
			Payload: map[string]any{
				"service": name,
				"reason":  reason,
			},
		})
	}
	return nil
}

// StartIdleReaper kicks off the goroutine that scans on_demand_lazy
// services every `interval` and transitions them to stopping when the
// idle window has passed. Returns immediately; runs until Stop is called.
func (lm *LifecycleManager) StartIdleReaper(interval time.Duration) {
	if lm == nil {
		return
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-lm.stopReaper:
				return
			case <-t.C:
				lm.reapOnce(context.Background())
			}
		}
	}()
}

// Stop signals the reaper goroutine to exit. Safe to call multiple times.
func (lm *LifecycleManager) Stop() {
	if lm == nil {
		return
	}
	select {
	case <-lm.stopReaper:
		// already closed
	default:
		close(lm.stopReaper)
	}
}

// EnsureForWorkflow ensures every service whose policy lists `workflow`
// in WarmForWorkflow is running before the workflow begins. Errors per
// service are logged + collected but don't short-circuit the rest of the
// workflow — a failed sidecar boot just means the corresponding phase
// runs with whatever provider is wired up. Returns a slice of per-service
// errors (nil when everything started cleanly).
//
// `activeFilter` (optional) restricts ensure-calls to the named sidecars.
// Pass nil for "ensure every matching policy" (legacy behaviour). Pass a
// non-nil set to ensure only the active tier providers' sidecars — this
// is what nightly_runner uses so a Tier 2 = ollama config doesn't also
// cold-start the airllm-tier2 sidecar.
func (lm *LifecycleManager) EnsureForWorkflow(ctx context.Context, workflow string, activeFilter map[string]bool) []error {
	if lm == nil || lm.bank == nil {
		return nil
	}
	var errs []error
	for _, name := range lm.candidateServiceNames() {
		if activeFilter != nil && !activeFilter[name] {
			continue
		}
		policy, _ := lm.bank.GetServiceLifecyclePolicy(name)
		if policy.Lifecycle == LifecycleAlwaysOn {
			continue
		}
		match := false
		for _, w := range policy.WarmForWorkflow {
			if w == workflow {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		timeout := time.Duration(policy.ColdStartGraceSec) * time.Second
		if timeout <= 0 {
			timeout = 90 * time.Second
		}
		log.Printf("lifecycle: ensure %s for workflow=%s (cold-start grace %s)", name, workflow, timeout)
		if err := lm.EnsureRunning(ctx, name, timeout); err != nil {
			errs = append(errs, fmt.Errorf("ensure_for_workflow %s: %w", name, err))
		} else {
			lm.SignalUsed(name, workflow)
		}
	}
	return errs
}

// sidecarNameForProvider maps an active tier provider's Name() (e.g.,
// "ollama:llama3.1:8b" / "airllm:meta-llama/Llama-3.1-70B-Instruct") to
// the matching managed-service short name, or "" when the provider
// doesn't have a sidecar (e.g., openai, anthropic — external APIs).
// Tier 1 (ollama-tier1) is always_on and not in this map.
func sidecarNameForTier2Provider(p LLMProvider) string {
	if p == nil {
		return ""
	}
	kind := providerKindFromName(p.Name())
	switch kind {
	case "ollama":
		return "ollama-tier2"
	case "airllm":
		return "airllm-tier2"
	}
	return ""
}

// SignalWorkflowComplete walks every service whose policy is
// on_demand_burst and whose WarmForWorkflow includes `workflow`,
// gracefully stopping each. No-op for on_demand_lazy (those get reaped
// on idle timeout) and always_on (those stay up).
func (lm *LifecycleManager) SignalWorkflowComplete(ctx context.Context, workflow string) {
	if lm == nil || lm.bank == nil {
		return
	}
	for _, name := range lm.candidateServiceNames() {
		policy, _ := lm.bank.GetServiceLifecyclePolicy(name)
		if policy.Lifecycle != LifecycleOnDemandBurst {
			continue
		}
		match := false
		for _, w := range policy.WarmForWorkflow {
			if w == workflow {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		if err := lm.GracefulStop(ctx, name, "burst_complete:"+workflow); err != nil {
			log.Printf("lifecycle: burst stop %s failed: %v", name, err)
		}
	}
}

// candidateServiceNames returns the union of (a) services with a
// persisted lifecycle row and (b) services with a non-always_on default
// policy. The default-policy set covers freshly-enabled containers that
// haven't been PUT-explicitly yet — without this, wave 8c's per-tier
// default policies would only kick in AFTER the user touched
// /settings/services/<name>/lifecycle, defeating the purpose.
func (lm *LifecycleManager) candidateServiceNames() []string {
	seen := map[string]bool{}
	out := []string{}
	if lm != nil && lm.bank != nil {
		if rows, err := lm.bank.ListServiceLifecycle(); err == nil {
			for _, row := range rows {
				if !seen[row.ServiceName] {
					seen[row.ServiceName] = true
					out = append(out, row.ServiceName)
				}
			}
		}
	}
	// Names with default-defined non-always-on policies. Keep in sync
	// with defaultPolicyFor's switch above. Wave 8c dropped tier3 — the
	// retired containers shouldn't be in the candidate set, otherwise
	// EnsureForWorkflow would try to start non-existent services.
	for _, name := range []string{"ollama-tier2", "airllm-tier2"} {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	// v2.6 Bundle G — user-managed containers attached via per-tier
	// ProviderConfig.Managed. Only included when LifecycleEnabled=true
	// AND the operator has flipped SD_ALLOW_DOCKER_SOCKET on (the
	// lifecycle manager itself gates the actual Docker calls; we just
	// surface the names here so the reaper + EnsureForWorkflow path
	// see them). The default-stack user with no Managed block set sees
	// zero change — the for-loop below short-circuits at the nil check.
	for _, name := range userManagedServiceNames(lm.bank) {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// userManagedServiceNames harvests container_name strings from per-tier
// ProviderConfig.Managed blocks (only when LifecycleEnabled is true).
// Best-effort: any per-tier lookup error is silently skipped so a
// malformed setting in one tier doesn't blank the whole list.
func userManagedServiceNames(bank *Bank) []string {
	if bank == nil {
		return nil
	}
	out := []string{}
	seen := map[string]bool{}
	for _, tier := range []TierKey{Tier1, Tier2, Tier3, TierEmbedding} {
		cfg, err := bank.GetProviderConfig(tier)
		if err != nil {
			continue
		}
		if cfg.Managed == nil || !cfg.Managed.LifecycleEnabled {
			continue
		}
		name := cfg.Managed.ContainerName
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// reapOnce scans all persisted lifecycle rows + transitions any
// on_demand_lazy service whose idle window has expired. Runs every tick.
func (lm *LifecycleManager) reapOnce(ctx context.Context) {
	if lm == nil || lm.bank == nil {
		return
	}
	rows, err := lm.bank.ListServiceLifecycle()
	if err != nil {
		return
	}
	for _, row := range rows {
		policy, _ := lm.bank.GetServiceLifecyclePolicy(row.ServiceName)
		if policy.Lifecycle != LifecycleOnDemandLazy {
			continue
		}
		if row.CurrentState != LifecycleStateRunning && row.CurrentState != LifecycleStateIdle {
			continue
		}
		if row.LastUsedAt == "" {
			continue
		}
		lastUsed, err := time.Parse(time.RFC3339Nano, row.LastUsedAt)
		if err != nil {
			continue
		}
		idleFor := time.Since(lastUsed)
		timeout := time.Duration(policy.IdleTimeoutSec) * time.Second
		if idleFor < timeout {
			// Not yet idle long enough. If we're currently "running" and
			// have been idle for more than half the timeout, flip the
			// state label to "idle" so the UI reflects it.
			if row.CurrentState == LifecycleStateRunning && idleFor > timeout/2 {
				lm.transition(row.ServiceName, LifecycleStateIdle, nil)
				if lm.hub != nil {
					lm.hub.Fanout(Event{
						SchemaVersion: SchemaVersion,
						Type:          "service_lifecycle_idled",
						Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
						AdapterID:     "sd-core-lifecycle",
						Payload: map[string]any{
							"service":  row.ServiceName,
							"idle_sec": int(idleFor.Seconds()),
						},
					})
				}
			}
			continue
		}
		// Idle window expired — stop.
		log.Printf("lifecycle: %s idle for %s; gracefully stopping", row.ServiceName, idleFor)
		if err := lm.GracefulStop(ctx, row.ServiceName, fmt.Sprintf("idle_timeout:%ds", policy.IdleTimeoutSec)); err != nil {
			log.Printf("lifecycle: graceful_stop %s failed: %v", row.ServiceName, err)
		}
	}
}
