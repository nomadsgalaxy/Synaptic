// settings.go — generic key/value settings store + the typed `nightly_schedule`
// the future NightlyRunner (P5-8) gates on.
//
// Why a generic settings table instead of one column per knob?
//
//   The HTML onboarding flow will introduce a stream of small toggles (theme,
//   default region, accent colour, "expand all on load", nightly window, ...)
//   most of which are user preferences with no schema impact. Bunching them
//   into JSON values keyed by string lets us add a knob from the UI without
//   touching the DB or the backend struct definitions every time. Typed
//   accessors (GetNightlySchedule, SaveNightlySchedule) give us type safety
//   for the load-bearing settings while the rest stay free-form.
//
// All reads go through GetSetting/ListSettings (no lock); all writes go
// through SetSetting/DeleteSetting (single-writer mutex).
package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────
// Generic settings CRUD
// ──────────────────────────────────────────────────────────────────────────

// SettingRow is one row in `settings`.
type SettingRow struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	UpdatedAt string `json:"updated_at"`
}

// SetSetting upserts a (key, value) pair.
func (b *Bank) SetSetting(key, value string) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	if key == "" {
		return errors.New("key is required")
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_, err := b.db.Exec(`
INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET
    value      = excluded.value,
    updated_at = excluded.updated_at`,
		key, value, nowUTC(),
	)
	return err
}

// GetSetting returns the value for key. Returns (default, false, nil) on
// missing key. Errors only on DB failures.
func (b *Bank) GetSetting(key string) (string, bool, error) {
	if b == nil {
		return "", false, errors.New("bank not enabled")
	}
	var v string
	err := b.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// ListSettings returns every (key, value, updated_at) row, ordered by key.
func (b *Bank) ListSettings() ([]SettingRow, error) {
	if b == nil {
		return nil, errors.New("bank not enabled")
	}
	rows, err := b.db.Query(`SELECT key, value, updated_at FROM settings ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SettingRow{}
	for rows.Next() {
		var s SettingRow
		if err := rows.Scan(&s.Key, &s.Value, &s.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DeleteSetting removes a key. Returns sql.ErrNoRows when the key was absent.
func (b *Bank) DeleteSetting(key string) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	res, err := b.db.Exec(`DELETE FROM settings WHERE key = ?`, key)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ──────────────────────────────────────────────────────────────────────────
// Tier 1 [think:off] toggle — controls whether the Tier 1 callers
// (classifier.go, sensitive_classifier.go, bubble_generator.go) prepend
// `[think:off]` to their system message. Reasoning-capable models
// (qwen3-thinking, Llama 3.3-Think, etc.) emit a hidden trace before the
// final answer; this trace adds 10–30 s of latency per call and eats
// into the response token budget. For latency-sensitive Tier 1 tasks
// (region classification, sensitivity YES/NO, thought bubbles) the
// trace adds no quality benefit, so we default the toggle to ON.
//
// Users running reasoning models who explicitly want the trace
// (sometimes it improves accuracy on edge-case inputs) can disable
// the prefix from Config → AI/API. The setting is read per-call so
// toggles take effect without a container restart. Non-reasoning models
// ignore the directive entirely, so leaving it ON is a no-op for the
// shipped llama3.2:3b default.
// ──────────────────────────────────────────────────────────────────────────

const ThinkOffEnabledKey = "tier1_think_off"

// ──────────────────────────────────────────────────────────────────────────
// Retry budgets — pipeline-wide caps on how many transient-error retries
// withRateLimitRetry may perform. Default convention is **0 = Infinite**
// (unlimited retries up to the per-call backoff schedule). Users who want
// to bound worst-case nightly stall time can set a positive integer; the
// pipeline ctx then carries that budget, decrementing one slot per retry
// across all provider calls until exhausted.
//
// The IMPLEMENTATION_PLAN's recommended bound was 20 (caps a transient
// outage at ~20 × 62 s = 20 minutes of backoff); shipping default-infinite
// preserves the v2.5 behaviour and makes the bound opt-in.
// ──────────────────────────────────────────────────────────────────────────

const NightlyRetryBudgetKey = "nightly_retry_budget"

// NightlyRetryBudget returns the max retries allowed across one nightly
// pipeline run, or 0 (unlimited) when the setting is absent/unparseable.
// Negative stored values are also coerced to 0 — the user shouldn't be
// able to brick the pipeline by setting -1 in the bank.
func (b *Bank) NightlyRetryBudget() int {
	if b == nil {
		return 0
	}
	raw, ok, err := b.GetSetting(NightlyRetryBudgetKey)
	if err != nil || !ok {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// ThinkOffEnabled reports whether Tier 1 callers should prepend
// `[think:off]` to their system message. Returns true (default ON) when
// the bank is unreachable, the setting row is absent, or the stored
// value is unparseable — preserving the previous behaviour for users
// who haven't touched the toggle.
func (b *Bank) ThinkOffEnabled() bool {
	if b == nil {
		return true
	}
	raw, ok, err := b.GetSetting(ThinkOffEnabledKey)
	if err != nil || !ok {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// ThinkOffPrefix returns the literal prefix to prepend to system
// messages — either `[think:off]\n\n` (when enabled) or "" (when
// disabled). Helper so call sites don't repeat the format string.
func (b *Bank) ThinkOffPrefix() string {
	if b == nil || b.ThinkOffEnabled() {
		return "[think:off]\n\n"
	}
	return ""
}

// ──────────────────────────────────────────────────────────────────────────
// Nightly schedule — the load-bearing setting NightlyRunner (P5-8) reads
// ──────────────────────────────────────────────────────────────────────────

// NightlyScheduleKey is the well-known settings key for the schedule blob.
const NightlyScheduleKey = "nightly_schedule"

// NightlySchedule controls when NightlyRunner is allowed to execute.
//
// JSON shape stored under key="nightly_schedule":
//
//	{
//	  "enabled":    true,
//	  "start_time": "23:00",          // 24h "HH:MM", local to Timezone
//	  "end_time":   "06:00",          // 24h "HH:MM"; if <= start_time the
//	                                  //   window is treated as crossing midnight
//	  "timezone":   "America/New_York", // IANA name; "" = server local time
//	  "configured": true,             // false until user has answered the
//	                                  //   onboarding prompt; UI uses this to
//	                                  //   decide whether to show the prompt
//	  "min_idle_minutes": 0           // (reserved) machine must be idle this
//	                                  //   long before nightly may start;
//	                                  //   0 = always run when in window.
//	                                  //   Idle detection is host-OS specific
//	                                  //   and currently unimplemented — left
//	                                  //   as a forward-compatible knob.
//	}
//
// Defaults when no row exists yet:
//   - enabled=true, start_time=02:00, end_time=06:00, timezone="" (server local),
//     configured=false. The runner can still execute on these defaults so a
//     freshly-deployed instance is functional out of the box, but the dashboard
//     should detect configured=false and prompt the user to set their window.
type NightlySchedule struct {
	Enabled        bool   `json:"enabled"`
	StartTime      string `json:"start_time"`
	EndTime        string `json:"end_time"`
	Timezone       string `json:"timezone"`
	Configured     bool   `json:"configured"`
	MinIdleMinutes int    `json:"min_idle_minutes,omitempty"`
}

// DefaultNightlySchedule returns the safe-default schedule for fresh installs.
func DefaultNightlySchedule() NightlySchedule {
	return NightlySchedule{
		Enabled:    true,
		StartTime:  "02:00",
		EndTime:    "06:00",
		Timezone:   "",
		Configured: false,
	}
}

// GetNightlySchedule reads the schedule from settings, falling back to
// DefaultNightlySchedule when absent or unparseable.
func (b *Bank) GetNightlySchedule() (NightlySchedule, error) {
	if b == nil {
		return DefaultNightlySchedule(), nil
	}
	raw, ok, err := b.GetSetting(NightlyScheduleKey)
	if err != nil {
		return DefaultNightlySchedule(), err
	}
	if !ok || raw == "" {
		return DefaultNightlySchedule(), nil
	}
	var s NightlySchedule
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		// Don't fail-fast — an unparseable blob shouldn't brick the runner.
		// Surface the error but degrade to defaults.
		return DefaultNightlySchedule(), fmt.Errorf("parse nightly_schedule: %w", err)
	}
	return s, nil
}

// SaveNightlySchedule validates and persists the schedule. Validation
// errors (bad time format, unknown timezone) are returned without touching
// storage — the existing setting is left intact.
func (b *Bank) SaveNightlySchedule(s NightlySchedule) error {
	if err := s.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return b.SetSetting(NightlyScheduleKey, string(raw))
}

// Validate checks StartTime/EndTime/Timezone format. Empty timezone is OK
// (server local). Disabled schedules skip start/end validation so a UI can
// flip "enabled=false" without the user filling in times.
func (s NightlySchedule) Validate() error {
	if !s.Enabled {
		return nil
	}
	if _, err := parseHHMM(s.StartTime); err != nil {
		return fmt.Errorf("start_time: %w", err)
	}
	if _, err := parseHHMM(s.EndTime); err != nil {
		return fmt.Errorf("end_time: %w", err)
	}
	if s.Timezone != "" {
		if _, err := time.LoadLocation(s.Timezone); err != nil {
			return fmt.Errorf("timezone %q: %w", s.Timezone, err)
		}
	}
	if s.MinIdleMinutes < 0 {
		return errors.New("min_idle_minutes cannot be negative")
	}
	return nil
}

// IsActive reports whether `now` falls inside the configured window.
// Disabled schedules always return false. Cross-midnight windows
// (e.g. start=23:00, end=06:00) are handled correctly. The schedule's
// Timezone overrides the timezone of `now`.
func (s NightlySchedule) IsActive(now time.Time) bool {
	if !s.Enabled {
		return false
	}
	loc := time.Local
	if s.Timezone != "" {
		if l, err := time.LoadLocation(s.Timezone); err == nil {
			loc = l
		}
	}
	now = now.In(loc)

	startMin, err := parseHHMM(s.StartTime)
	if err != nil {
		return false
	}
	endMin, err := parseHHMM(s.EndTime)
	if err != nil {
		return false
	}
	currentMin := now.Hour()*60 + now.Minute()

	if startMin == endMin {
		// Degenerate window — never active. (Avoid "always active" surprise.)
		return false
	}
	if startMin < endMin {
		// Same-day window: e.g. 02:00 → 06:00.
		return currentMin >= startMin && currentMin < endMin
	}
	// Cross-midnight window: e.g. 23:00 → 06:00. Active when current time is
	// >= start OR < end.
	return currentMin >= startMin || currentMin < endMin
}

// WindowEndAt returns when the currently-active window closes, in the
// schedule's timezone. Returns zero time when the schedule is disabled,
// misconfigured, or `now` falls outside an active window.
//
// Cross-midnight windows (e.g. start=23:00, end=06:00) are handled —
// the returned end-time is on the appropriate calendar day relative to
// `now`. Multi-dream-per-night uses this to compute "remaining time
// in the current window" so the auto-loop can decide whether the next
// cycle has time to finish.
func (s NightlySchedule) WindowEndAt(now time.Time) time.Time {
	if !s.IsActive(now) {
		return time.Time{}
	}
	loc := time.Local
	if s.Timezone != "" {
		if l, err := time.LoadLocation(s.Timezone); err == nil {
			loc = l
		}
	}
	now = now.In(loc)
	startMin, err := parseHHMM(s.StartTime)
	if err != nil {
		return time.Time{}
	}
	endMin, err := parseHHMM(s.EndTime)
	if err != nil {
		return time.Time{}
	}
	currentMin := now.Hour()*60 + now.Minute()
	// Same-day window (start < end). The end-time is today at endMin.
	if startMin < endMin {
		return time.Date(now.Year(), now.Month(), now.Day(), endMin/60, endMin%60, 0, 0, loc)
	}
	// Cross-midnight window (start > end). If we're past start (still on
	// the start-day), end is tomorrow at endMin. If we're before end
	// (on the end-day), end is today at endMin.
	if currentMin >= startMin {
		// We're on the start-day; end is tomorrow.
		t := time.Date(now.Year(), now.Month(), now.Day(), endMin/60, endMin%60, 0, 0, loc)
		return t.Add(24 * time.Hour)
	}
	return time.Date(now.Year(), now.Month(), now.Day(), endMin/60, endMin%60, 0, 0, loc)
}

// WindowStartAt returns when the currently-active window opened. The
// counterpart to WindowEndAt, used for "cycles completed since window
// open" tallies. Returns zero time when the schedule is disabled or
// `now` is outside an active window.
func (s NightlySchedule) WindowStartAt(now time.Time) time.Time {
	if !s.IsActive(now) {
		return time.Time{}
	}
	loc := time.Local
	if s.Timezone != "" {
		if l, err := time.LoadLocation(s.Timezone); err == nil {
			loc = l
		}
	}
	now = now.In(loc)
	startMin, err := parseHHMM(s.StartTime)
	if err != nil {
		return time.Time{}
	}
	endMin, err := parseHHMM(s.EndTime)
	if err != nil {
		return time.Time{}
	}
	currentMin := now.Hour()*60 + now.Minute()
	if startMin < endMin {
		return time.Date(now.Year(), now.Month(), now.Day(), startMin/60, startMin%60, 0, 0, loc)
	}
	// Cross-midnight. If currentMin < endMin we're on the end-day; the
	// window opened yesterday at startMin. Otherwise it opened today.
	if currentMin < endMin {
		t := time.Date(now.Year(), now.Month(), now.Day(), startMin/60, startMin%60, 0, 0, loc)
		return t.Add(-24 * time.Hour)
	}
	return time.Date(now.Year(), now.Month(), now.Day(), startMin/60, startMin%60, 0, 0, loc)
}

// NextActivationFrom returns the next time at which IsActive transitions
// from false to true, starting the search at `from`. Used by NightlyRunner
// to compute its sleep duration. Returns zero time when disabled.
func (s NightlySchedule) NextActivationFrom(from time.Time) time.Time {
	if !s.Enabled {
		return time.Time{}
	}
	loc := time.Local
	if s.Timezone != "" {
		if l, err := time.LoadLocation(s.Timezone); err == nil {
			loc = l
		}
	}
	from = from.In(loc)
	startMin, err := parseHHMM(s.StartTime)
	if err != nil {
		return time.Time{}
	}
	// Today at start_time, in the schedule's timezone.
	candidate := time.Date(from.Year(), from.Month(), from.Day(),
		startMin/60, startMin%60, 0, 0, loc)
	if !candidate.After(from) {
		// Already past today's start — push to tomorrow.
		candidate = candidate.Add(24 * time.Hour)
	}
	return candidate
}

// ──────────────────────────────────────────────────────────────────────────
// Per-tier provider configuration
// ──────────────────────────────────────────────────────────────────────────

// TierKey identifies one of the three model tiers in settings storage.
type TierKey string

const (
	TierEmbedding TierKey = "tier_embedding" // dedicated embedding provider; always local
	Tier1         TierKey = "tier1_provider" // real-time encode + bubble + classifier
	Tier2         TierKey = "tier2_provider" // NightlyRunner consolidation
	Tier3         TierKey = "tier3_provider" // Oracle / research
)

// AllTierKeys lists every tier key for iteration in /settings/providers.
var AllTierKeys = []TierKey{TierEmbedding, Tier1, Tier2, Tier3}

// ProviderKind enumerates the supported provider families.
type ProviderKind string

const (
	ProviderOllama    ProviderKind = "ollama"
	ProviderOpenAI    ProviderKind = "openai"
	ProviderAnthropic ProviderKind = "anthropic"
	ProviderCustom    ProviderKind = "custom" // any OpenAI-compatible endpoint
	ProviderDisabled  ProviderKind = "disabled"
	// AirLLM — local layered-streaming inference for 70B+ models (CITATIONS.md
	// not applicable; this is operational, not biological). Speed/quality
	// trade puts it strictly at Tier 3: ~1-3 tok/s makes it unsuitable for
	// per-ingest Tier 1 work or Phase 0b's 100/run pace at Tier 2. See
	// `CODE_HANDOFF — AirLLM Tier 3 provider (2026-05-10).md`.
	ProviderAirLLM ProviderKind = "airllm"
)

// v2.6 Bundle E — local OpenAI-compatible support is now folded into
// `kind=custom`. When the BaseURL classifies as local (loopback / RFC1918
// / .local / .lan / .internal — see isLocalURL), BuildProvider sets
// RemoteProvider.Local=true so the resulting provider is accepted at
// TierEmbedding and skips the sensitive-content egress filter. Cloud
// `custom` endpoints (HTTPS to public DNS) keep the original
// remote-treatment behaviour. The submitter's `SD_TIER{N}_KIND=openai_local`
// env-var spelling is accepted as a synonym for `custom` (see
// normalizeProviderKindString in main.go) so the CHANGES.md doc audience
// can use it without learning a new spelling.

// KindLabels maps provider kinds to user-facing labels rendered by the
// dashboard's provider dropdown. Adding a new entry here makes the
// option appear automatically in the UI; restriction to specific tiers
// is enforced by ProviderConfig.Validate.
var KindLabels = map[ProviderKind]string{
	ProviderOllama:    "Ollama",
	ProviderOpenAI:    "OpenAI",
	ProviderAnthropic: "Anthropic",
	// Custom covers both cloud OpenAI-compatible endpoints AND local
	// OpenAI-format servers (LM Studio, llama.cpp, vLLM). Local treatment
	// is auto-detected from the BaseURL (loopback / RFC1918 / .local / .lan
	// / .internal). v2.6 Bundle E.
	ProviderCustom:   "Custom (OpenAI-compatible — cloud or local)",
	ProviderDisabled: "Disabled",
	ProviderAirLLM:   "AirLLM (local large-model)",
}

// ProviderConfig is the per-tier swappable configuration captured by the
// onboarding UI and persisted in the settings table.
//
// Stored under settings.key = TierKey (e.g. "tier1_provider"), value =
// JSON of this struct.
//
// API KEYS ARE NOT STORED HERE. APIKeyEnv names an env var; the running
// process reads the actual key at call time. The HTML onboarding UI must
// explain this trade-off: keys go in the host environment / .env file,
// not the database.
type ProviderConfig struct {
	// Kind dictates which LLMProvider implementation backs this tier.
	Kind ProviderKind `json:"kind"`
	// BaseURL is required for openai / anthropic / custom; ignored for
	// ollama (which uses SD_OLLAMA_URL) and disabled.
	BaseURL string `json:"base_url,omitempty"`
	// Model is the model identifier the provider expects. Empty defaults
	// to the legacy SD_REALTIME_MODEL / SD_NIGHTLY_MODEL env var depending
	// on the tier.
	Model string `json:"model,omitempty"`
	// APIKeyEnv names the env var holding the API key. Validated when
	// Kind is non-local; never returned in GET responses.
	APIKeyEnv string `json:"api_key_env,omitempty"`
	// AuthScheme is "bearer" (default) or "x-api-key" (Anthropic legacy).
	AuthScheme string `json:"auth_scheme,omitempty"`
	// SystemPrompt overrides DefaultRemoteSystemPrompt for remote tiers.
	// Empty = use the default. Local tiers ignore this for now.
	SystemPrompt string `json:"system_prompt,omitempty"`
	// DisplayName shows up in logs and audit ("openai:gpt-4o-mini").
	DisplayName string `json:"display_name,omitempty"`
	// TimeoutSeconds overrides the per-call HTTP timeout. Zero = use the
	// provider-kind default (60s remote, 120s openai_local Tier 1/Embed,
	// 300s openai_local Tier 2, 600s openai_local Tier 3). Centralised
	// via providerTimeout() so default-shifts in one place. v2.6 Bundle E.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// Managed names a Docker container the user wants Synaptic to
	// know about / lifecycle-manage for this tier. Optional — leave
	// unset for tiers that don't need lifecycle wiring (cloud providers,
	// containers already managed by docker-compose with always-on policy,
	// etc.). v2.6 Bundle G. The block is purely informational unless
	// `LifecycleEnabled=true` AND the operator has set
	// `SD_ALLOW_DOCKER_SOCKET=1` on the SD Core container.
	Managed *ManagedContainer `json:"managed,omitempty"`
}

// ManagedContainer captures the metadata SD Core needs to lifecycle a
// user-registered Docker container alongside the shipped sidecars
// (`synaptic-ollama-tier2`, `synaptic-airllm-tier2`). All fields are
// user-facing; the operator opts in by setting LifecycleEnabled.
//
// Safety: the SD Core process never touches the Docker socket unless
// `SD_ALLOW_DOCKER_SOCKET=1`. With that gate held closed (the default),
// LifecycleEnabled=true just causes the container to APPEAR in the
// /admin/docker/services dashboard — it doesn't actually start/stop
// anything.
type ManagedContainer struct {
	// Image is the Docker image reference (e.g. "ghcr.io/ggerganov/llama.cpp:server"
	// or "lmstudio/lmstudio:latest"). Required when the block is present.
	Image string `json:"image"`
	// ContainerName is the docker container_name the user has set in
	// their compose file or `docker run --name`. Required. Used as the
	// lookup key for lifecycle ops. Must be unique across all tiers
	// (validated at the router level via userManagedContainerConflicts).
	ContainerName string `json:"container_name"`
	// HostPort + ContainerPort document the exposed port. Zero on either
	// side means "not exposed / port-only known to docker network".
	// Informational — SD Core doesn't open the port; that's the
	// container author's job.
	HostPort      int `json:"host_port,omitempty"`
	ContainerPort int `json:"container_port,omitempty"`
	// Env is an optional KEY=VALUE list for the user to document the
	// env vars their container needs. Informational only; SD Core
	// doesn't inject these.
	Env []string `json:"env,omitempty"`
	// Volumes is an optional host:container mount-spec list, same
	// shape as Env — informational, not injected.
	Volumes []string `json:"volumes,omitempty"`
	// LifecycleEnabled gates whether SD Core may invoke Docker start/
	// stop / restart on this container. Defaults false so registering
	// a container is a safe no-op until the user explicitly opts in.
	// Requires `SD_ALLOW_DOCKER_SOCKET=1` host-side; without that the
	// flag is honoured-but-inert (container appears in the dashboard
	// listing; SD Core never touches the socket).
	LifecycleEnabled bool `json:"lifecycle_enabled,omitempty"`
}

// IsZero reports whether the block carries no useful information. Used
// by Validate to skip the rules when the field is left empty.
func (m *ManagedContainer) IsZero() bool {
	if m == nil {
		return true
	}
	return m.Image == "" &&
		m.ContainerName == "" &&
		m.HostPort == 0 &&
		m.ContainerPort == 0 &&
		len(m.Env) == 0 &&
		len(m.Volumes) == 0 &&
		!m.LifecycleEnabled
}

// IsLocalKind reports whether Kind designates a local provider.
// `custom` is local when its BaseURL is loopback / RFC1918 / .local /
// .lan / .internal — see isLocalURL. v2.6 Bundle E.
func (c ProviderConfig) IsLocalKind() bool {
	if c.Kind == ProviderOllama || c.Kind == ProviderAirLLM {
		return true
	}
	if c.Kind == ProviderCustom && isLocalURL(c.BaseURL) {
		return true
	}
	return false
}

// providerTimeout returns the effective HTTP timeout for a provider
// config. Zero TimeoutSeconds falls back to a kind-appropriate default
// — local kinds get longer timeouts because local CPU-bound models
// regularly run 30–60 s per call. v2.6 Bundle E.
func providerTimeout(cfg ProviderConfig) time.Duration {
	if cfg.TimeoutSeconds > 0 {
		return time.Duration(cfg.TimeoutSeconds) * time.Second
	}
	if cfg.Kind == ProviderAirLLM {
		return 600 * time.Second
	}
	// `kind=custom` classified as local (LM Studio / llama.cpp / vLLM
	// over the local network) needs a longer default — local 35B
	// models on CPU regularly run 30–90 s per call.
	if cfg.Kind == ProviderCustom && isLocalURL(cfg.BaseURL) {
		return 120 * time.Second
	}
	return 60 * time.Second
}

// isLocalURL classifies a URL as local-network or not. Used by the
// openai_local validator (Bundle E.2) to keep a misconfigured user
// from setting BaseURL to a cloud endpoint with kind=openai_local —
// IsLocal()=true on that combination would bypass the sensitive-
// content egress gate. Returns true for loopback / RFC1918 / link-
// local / .local / .lan / .internal hostnames.
func isLocalURL(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	if host == "localhost" || host == "::1" {
		return true
	}
	lower := strings.ToLower(host)
	for _, suffix := range []string{".local", ".lan", ".internal"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
			return true
		}
	}
	return false
}

// validateManagedContainer enforces the v2.6 Bundle G rules for the
// optional ManagedContainer block. Called from Validate; returns nil
// when the block is empty/unset (the common case).
func (c ProviderConfig) validateManagedContainer() error {
	if c.Managed == nil || c.Managed.IsZero() {
		return nil
	}
	if c.Managed.Image == "" {
		return errors.New("managed.image is required when managed block is set")
	}
	if c.Managed.ContainerName == "" {
		return errors.New("managed.container_name is required when managed block is set")
	}
	if c.Managed.HostPort < 0 || c.Managed.HostPort > 65535 {
		return fmt.Errorf("managed.host_port out of range: %d", c.Managed.HostPort)
	}
	if c.Managed.ContainerPort < 0 || c.Managed.ContainerPort > 65535 {
		return fmt.Errorf("managed.container_port out of range: %d", c.Managed.ContainerPort)
	}
	// Reject obviously-bogus container names early (Docker permits
	// `[a-zA-Z0-9][a-zA-Z0-9_.-]*` per its docs; we mirror that loose
	// check rather than a full regex parse).
	for i, r := range c.Managed.ContainerName {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '_' || r == '.' || r == '-'
		if !ok {
			return fmt.Errorf("managed.container_name has illegal character %q at position %d", r, i)
		}
	}
	return nil
}

// Validate returns an error if the config is missing required fields for
// its Kind, or contains a hard-rejected pattern.
func (c ProviderConfig) Validate(tier TierKey) error {
	// Common rule: the ManagedContainer block is opt-in but, when
	// present, has its own correctness requirements.
	if err := c.validateManagedContainer(); err != nil {
		return err
	}
	switch c.Kind {
	case ProviderDisabled:
		return nil
	case ProviderOllama:
		// BaseURL & Model can be empty (we'll fall back to env).
		return nil
	case ProviderAirLLM:
		// Allowed at Tier 2 + Tier 3 (per user direction "for tier 2 and 3,
		// it can 100% be a bundle replacement for ollama"). Tier 1 stays
		// rejected — ingest needs sub-second classification + nomic-embed-text
		// is the right tool for embeddings, both incompatible with AirLLM's
		// per-token disk streaming. Tier 2 acceptance lets users opt into
		// 70B+-quality deep enrichment overnight; they should lower
		// `deep_enrich_max_per_run` to 5-20 to match the slower throughput.
		// Frontend gates the dropdown; this is defence-in-depth.
		if tier != Tier2 && tier != Tier3 {
			return fmt.Errorf("airllm is only available at Tier 2 or Tier 3; latency makes it unsuitable for tier=%s", tier)
		}
		if c.BaseURL == "" {
			return errors.New("base_url is required for kind=airllm (e.g. http://127.0.0.1:9912)")
		}
		if c.Model == "" {
			return errors.New("model is required for kind=airllm (HuggingFace id, e.g. meta-llama/Llama-3.1-70B-Instruct)")
		}
		// AirLLM sidecar auth is optional + localhost-bound; no api_key_env shape rejection.
		return nil
	case ProviderOpenAI, ProviderAnthropic, ProviderCustom:
		if c.BaseURL == "" {
			return fmt.Errorf("base_url is required for kind=%s", c.Kind)
		}
		if c.Model == "" {
			return fmt.Errorf("model is required for kind=%s", c.Kind)
		}
		// Hard reject: anything that smells like a literal API key. We never
		// want a key persisted into the DB — the user must drop it in an env
		// var named by api_key_env instead.
		if looksLikeAPIKey(c.APIKeyEnv) {
			return fmt.Errorf("api_key_env must be an env var NAME (e.g. SD_TIER1_API_KEY), not the key itself")
		}
		// v2.6 Bundle E — kind=custom is allowed at TierEmbedding when
		// the BaseURL classifies as local (loopback / RFC1918 / .local /
		// .lan / .internal). Cloud `custom` endpoints stay rejected
		// because mixing embedding spaces silently breaks /recall.
		if tier == TierEmbedding {
			if c.Kind == ProviderCustom && isLocalURL(c.BaseURL) {
				return nil
			}
			// SD_ALLOW_REMOTE_EMBEDDINGS=1 — escape hatch for ephemeral
			// benchmark/test containers where memories are wiped per
			// iteration. Real user banks would lose /recall on existing
			// rows; this gate is OFF by default.
			if v := strings.TrimSpace(os.Getenv("SD_ALLOW_REMOTE_EMBEDDINGS")); v == "1" || v == "true" || v == "TRUE" || v == "on" {
				return nil
			}
			return errors.New("embedding tier must be a local provider (ollama, or custom with a local BaseURL); mixing embedding spaces breaks /recall (set SD_ALLOW_REMOTE_EMBEDDINGS=1 to override for bench/test)")
		}
		return nil
	}
	return fmt.Errorf("unknown provider kind: %q", c.Kind)
}

// looksLikeAPIKey is a heuristic for the validator. We don't try to be
// exhaustive — we just stop the foot-guns: a string starting with
// sk-/sk-ant-/Bearer/etc. or longer than ~80 chars is almost certainly
// the key itself rather than an env var name.
func looksLikeAPIKey(s string) bool {
	if len(s) > 80 {
		return true
	}
	low := strings.ToLower(s)
	for _, p := range []string{"sk-", "sk_", "bearer ", "ghp_", "akia"} {
		if strings.HasPrefix(low, p) {
			return true
		}
	}
	return false
}

// DefaultProviderConfig returns the safe-default config for a tier when
// nothing is stored yet — preserves the legacy env-var-driven behaviour.
func DefaultProviderConfig(tier TierKey) ProviderConfig {
	switch tier {
	case TierEmbedding, Tier1:
		return ProviderConfig{Kind: ProviderOllama}
	case Tier2:
		// Tier 2 disabled by default — env-var path in main.go promotes it
		// to Ollama when SD_NIGHTLY_MODEL is set.
		return ProviderConfig{Kind: ProviderDisabled}
	case Tier3:
		return ProviderConfig{Kind: ProviderDisabled}
	}
	return ProviderConfig{Kind: ProviderDisabled}
}

// GetProviderConfig reads the per-tier config from settings, falling back
// to DefaultProviderConfig on missing/unparseable.
func (b *Bank) GetProviderConfig(tier TierKey) (ProviderConfig, error) {
	if b == nil {
		return DefaultProviderConfig(tier), nil
	}
	raw, ok, err := b.GetSetting(string(tier))
	if err != nil {
		return DefaultProviderConfig(tier), err
	}
	if !ok || raw == "" {
		return DefaultProviderConfig(tier), nil
	}
	var cfg ProviderConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return DefaultProviderConfig(tier), fmt.Errorf("parse %s: %w", tier, err)
	}
	return cfg, nil
}

// SaveProviderConfig validates and persists the per-tier config.
//
// Side effect for Tier 2 changes (handoff "deep per-memory enrichment
// 2026-05-10" §Dirty-flag triggers / Model upgrade): when the user
// switches tier2 to a different model, every already-deep-encoded memory
// gets bulk-marked dirty with reason="model_upgrade". The next nightly
// run starts catching the bank up to the new model.
func (b *Bank) SaveProviderConfig(tier TierKey, cfg ProviderConfig) error {
	if err := cfg.Validate(tier); err != nil {
		return err
	}
	prevModel := ""
	if tier == Tier2 {
		if prev, _ := b.GetProviderConfig(tier); prev.Kind != "" {
			prevModel = providerNameFromConfig(prev)
		}
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := b.SetSetting(string(tier), string(raw)); err != nil {
		return err
	}
	if tier == Tier2 {
		newModel := providerNameFromConfig(cfg)
		if newModel != "" && newModel != prevModel {
			if n, mErr := b.MarkAllDeepEncodedDirty("model_upgrade", newModel); mErr == nil && n > 0 {
				// Best-effort log — surfacing the bulk-mark via UI is the
				// frontend's job (per the handoff's confirmation-dialog spec).
				log.Printf("settings: tier2 changed %q → %q; marked %d memories dirty (model_upgrade)", prevModel, newModel, n)
			}
		}
	}
	return nil
}

// providerNameFromConfig builds the canonical "<kind>:<model>" string used
// for dirty-comparison. Empty when the tier is disabled.
func providerNameFromConfig(c ProviderConfig) string {
	if c.Kind == "" || c.Kind == ProviderDisabled {
		return ""
	}
	return string(c.Kind) + ":" + c.Model
}

// ──────────────────────────────────────────────────────────────────────────
// Provider factory
// ──────────────────────────────────────────────────────────────────────────

// BuildProvider constructs an LLMProvider from a config. Returns nil for
// kind=disabled. Falls back to SD_OLLAMA_URL for Ollama BaseURL when
// cfg.BaseURL is empty.
func BuildProvider(cfg ProviderConfig) LLMProvider {
	switch cfg.Kind {
	case ProviderDisabled:
		return nil
	case ProviderOllama:
		base := cfg.BaseURL
		if base == "" {
			// Default Ollama endpoint. After wave 8 this is the
			// `ollama-tier1` container (default-up, always-on).
			//
			// Inside the synaptic_default compose network the canonical
			// hostname is `synaptic-ollama-tier1` — that's what SD Core's
			// own compose stanza sets `SD_OLLAMA_URL` to. The
			// `localhost:11434` fallback below is for adapters running
			// OUTSIDE the SD Core container that reach the same Ollama
			// via the host port-mapping (ollama-tier1: 11434 → 11434).
			//
			// Either form works because the host-port binding makes both
			// reachable; users on Docker Desktop typically point native
			// clients (e.g. their MCP adapter) at localhost:11434 and
			// containerised code at `ollama-tier1:11434`.
			base = envOr("SD_OLLAMA_URL", "http://localhost:11434")
		}
		return &OllamaProvider{BaseURL: base, Model: cfg.Model, Timeout: providerTimeout(cfg)}
	case ProviderOpenAI, ProviderAnthropic, ProviderCustom:
		// Anthropic uses x-api-key by historical convention; allow override.
		auth := cfg.AuthScheme
		if auth == "" && cfg.Kind == ProviderAnthropic {
			auth = "x-api-key"
		}
		// v2.6 Bundle E — auto-detect local OpenAI-compatible servers
		// (LM Studio, llama.cpp, vLLM) by BaseURL classification. When
		// detected, the provider is treated as local (router accepts
		// it at TierEmbedding, sensitive AI doesn't auto-disable,
		// egress filter is skipped). Log loudly so an audit reader can
		// see the classification choice.
		local := cfg.Kind == ProviderCustom && isLocalURL(cfg.BaseURL)
		display := cfg.DisplayName
		if display == "" {
			if local {
				display = "local:" + cfg.Model
			} else {
				display = string(cfg.Kind) + ":" + cfg.Model
			}
		}
		if local {
			log.Printf("router: custom provider %q classified as local (BaseURL=%s) — accepted at TierEmbedding, egress filter skipped",
				cfg.Model, cfg.BaseURL)
		}
		return &RemoteProvider{
			BaseURL:      cfg.BaseURL,
			Model:        cfg.Model,
			APIKeyEnv:    cfg.APIKeyEnv,
			AuthScheme:   auth,
			SystemPrompt: cfg.SystemPrompt,
			DisplayName:  display,
			Local:        local,
			Timeout:      providerTimeout(cfg),
		}
	case ProviderAirLLM:
		display := cfg.DisplayName
		if display == "" {
			display = "airllm:" + cfg.Model
		}
		return NewAirLLMProvider(cfg.BaseURL, cfg.Model, cfg.APIKeyEnv, display)
	}
	return nil
}

// SafeProviderConfig returns a copy with no fields stripped. Currently
// ProviderConfig already excludes secrets (api_key_env is just a name),
// but this helper exists so future redaction (e.g. masking BaseURL when
// it embeds a token) has a single chokepoint.
func (c ProviderConfig) SafeForResponse() ProviderConfig {
	out := c
	// Belt-and-braces: if some caller stuffed a key inline, don't leak it.
	if looksLikeAPIKey(out.APIKeyEnv) {
		out.APIKeyEnv = "[REDACTED]"
	}
	return out
}

// parseHHMM parses "HH:MM" → minutes-since-midnight. Returns an error for
// bad format. Tolerates leading zeros ("02:00") and single-digit hours ("2:00").
func parseHHMM(s string) (int, error) {
	s = strings.TrimSpace(s)
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("expected HH:MM, got %q", s)
	}
	var h, m int
	if _, err := fmt.Sscanf(parts[0], "%d", &h); err != nil {
		return 0, fmt.Errorf("hours: %w", err)
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &m); err != nil {
		return 0, fmt.Errorf("minutes: %w", err)
	}
	if h < 0 || h > 23 {
		return 0, fmt.Errorf("hours out of range: %d", h)
	}
	if m < 0 || m > 59 {
		return 0, fmt.Errorf("minutes out of range: %d", m)
	}
	return h*60 + m, nil
}
