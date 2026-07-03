// nightly_phase0b_progress.go — Live per-memory progress emitter for
// Phase 0b deep enrichment.
//
// Polls the Tier 2 provider's /progress endpoint (AirLLM today; future
// Ollama support pending an equivalent endpoint upstream) every 5s
// while a single memory is mid-enrichment, and broadcasts a
// `phase0b_memory_progress` WS event with:
//   - which memory we're on (id, preview, index/total)
//   - live token counter (emitted/max)
//   - per-memory ETA (derived from current rate)
//   - per-run ETA (extrapolated from per-memory rate × remaining)
//   - the model and tier producing the work
//
// Doubles as a lifecycle heartbeat: every successful poll calls
// pc.lifecycle.SignalProviderUse("nightly_run"). On a long generate (AirLLM
// 70B can run for 30+ minutes per memory) the idle reaper would
// otherwise stop the sidecar mid-token; the poll cadence makes that
// impossible.
//
// Architectural notes:
//   - The bootstrap event fires synchronously when the poller starts,
//     so the dashboard always sees "memory N of M started" within a
//     couple ms of enrichMemoryWithTier2 being called — even when the
//     provider doesn't support /progress (Ollama).
//   - The poller goroutine never outlives the enrichment call. The
//     `stop()` closure cancels its context AND waits for the goroutine
//     to drain, so a slow final poll can't overlap the next memory's
//     bootstrap event.
//   - Errors from /progress are swallowed silently. The endpoint is
//     best-effort decoration — the actual Chat call is the source of
//     truth, and we never want progress-poller failure to bubble into
//     the phase failure log.

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// airllmProgressSnapshot mirrors the JSON shape of GET /progress on
// the AirLLM sidecar. Only the fields the poller actually needs are
// declared — extras are tolerated.
type airllmProgressSnapshot struct {
	Busy          bool    `json:"busy"`
	TokensEmitted int     `json:"tokens_emitted"`
	MaxTokens     int     `json:"max_tokens"`
	ElapsedMs     int     `json:"elapsed_ms"`
	EtaSeconds    float64 `json:"eta_seconds"`
	Model         string  `json:"model"`
	PromptTokens  int     `json:"prompt_tokens"`
}

// phase0bPollInterval is the cadence at which the progress poller hits
// the Tier 2 provider's /progress endpoint. 5s balances perceived
// liveness (the dashboard's animation reads ~smooth) against polling
// overhead (each request crosses the compose network + a JSON encode).
const phase0bPollInterval = 5 * time.Second

// phase0bProgressHTTPTimeout caps each poll. /progress is cheap (just
// a dict-to-JSON round-trip on the sidecar) but a hung sidecar
// shouldn't wedge the ticker.
const phase0bProgressHTTPTimeout = 3 * time.Second

// startPhase0bProgressPoller spawns a goroutine that emits
// phase0b_memory_progress WS events at ~5s cadence for the duration
// of one memory's enrichment. Returns a `stop()` closure that:
//   1. cancels the goroutine's context, AND
//   2. waits for the goroutine to finish its current iteration before returning.
// Callers MUST call stop() (defer it) immediately around the
// enrichMemoryWithTier2 call so the poller doesn't outlive its memory.
//
// For Tier 2 providers without a /progress endpoint (every provider
// other than AirLLM today) the poller still emits ONE bootstrap event
// at start so the dashboard knows which memory is in flight, then
// idles until stop().
func (pc *pipelineContext) startPhase0bProgressPoller(
	tier2 LLMProvider, mem MemoryRecord, memIndex, memTotal int,
) func() {
	progressURL := ""
	if airllm, ok := tier2.(*AirLLMProvider); ok && airllm != nil && airllm.BaseURL != "" {
		progressURL = strings.TrimRight(airllm.BaseURL, "/") + "/progress"
	}

	// Bootstrap event — fires before any poll. Gives the dashboard a
	// "memory in flight" hook even when the provider can't report
	// token-level progress (Ollama Tier 2), so the UI doesn't sit
	// blank waiting for a first event.
	pc.emitPhase0bMemoryProgress(mem, memIndex, memTotal, 0, 0, 0, 0, 0, providerName(tier2))

	if progressURL == "" || pc.hub == nil {
		return func() {} // nothing to poll / nowhere to emit
	}

	pollerCtx, cancel := context.WithCancel(pc.ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(phase0bPollInterval)
		defer ticker.Stop()
		client := &http.Client{Timeout: phase0bProgressHTTPTimeout}
		// Per-memory start time anchors the run-ETA extrapolation. We
		// use the AirLLM-reported elapsed_ms when it's > 0, falling
		// back to our wall-clock if the snapshot looks stale.
		memoryStarted := time.Now()
		for {
			select {
			case <-pollerCtx.Done():
				return
			case <-ticker.C:
				snap, ok := fetchAirLLMProgress(pollerCtx, client, progressURL)
				if !ok || !snap.Busy {
					// /progress unreachable, decoded badly, or
					// between requests. Don't emit — stale data is
					// worse than no data for the UI.
					continue
				}
				// Lifecycle heartbeat. Belt-and-suspenders alongside
				// the SignalProviderUse call inside each .Chat — if
				// the .Chat path's signal somehow doesn't fire, the
				// poller's keep-alive still prevents idle-reap.
				if pc.lifecycle != nil {
					// "nightly_run" matches the workflow tag the rest of the
					// pipeline uses (see nightly_runner.go); the lifecycle
					// manager bumps last_used_at on any service whose
					// warm_for_workflow list contains this tag — that's
					// airllm-tier2 and ollama-tier2 today.
					pc.lifecycle.SignalProviderUse("nightly_run")
				}
				// Per-memory ETA: prefer the sidecar's own EtaSeconds
				// (it knows its rate); fall back to dividing remaining
				// tokens by the rate we derive from emitted/elapsed.
				etaMem := snap.EtaSeconds
				if etaMem <= 0 && snap.ElapsedMs > 0 && snap.TokensEmitted > 0 {
					rate := float64(snap.TokensEmitted) / (float64(snap.ElapsedMs) / 1000.0)
					if rate > 0 && snap.MaxTokens > snap.TokensEmitted {
						etaMem = float64(snap.MaxTokens-snap.TokensEmitted) / rate
					}
				}
				// Per-run ETA: extrapolate "this memory's projected
				// total seconds" × "memories remaining including this
				// one". memTotal-memIndex covers (this memory + N
				// queued after it).
				etaRun := 0.0
				if memTotal > 0 && memIndex < memTotal {
					perMemorySec := float64(snap.ElapsedMs)/1000.0 + etaMem
					remainingMems := memTotal - memIndex
					etaRun = perMemorySec * float64(remainingMems)
				}
				elapsedSec := time.Since(memoryStarted).Seconds()
				_ = elapsedSec // reserved for future use; rate-of-rate detection
				pc.emitPhase0bMemoryProgress(
					mem, memIndex, memTotal,
					snap.TokensEmitted, snap.MaxTokens,
					snap.ElapsedMs, int(etaMem), int(etaRun),
					snap.Model,
				)
			}
		}
	}()
	return func() {
		cancel()
		wg.Wait()
	}
}

// fetchAirLLMProgress hits GET /progress and decodes the body.
// Returns (snapshot, true) on success or (zero, false) on any error.
// All failure modes are silent — the poller treats them as "no data
// this tick" rather than surfacing in phase_failures.
func fetchAirLLMProgress(ctx context.Context, client *http.Client, url string) (airllmProgressSnapshot, bool) {
	var snap airllmProgressSnapshot
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return snap, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return snap, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return snap, false
	}
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return snap, false
	}
	return snap, true
}

// emitPhase0bMemoryProgress builds + broadcasts one event. Centralized
// so the bootstrap path and the ticker path share one payload shape.
// Memory preview is truncated to 80 chars so a sensitive body can't
// leak by accident through the WS broadcast — long memories stay safe.
func (pc *pipelineContext) emitPhase0bMemoryProgress(
	mem MemoryRecord, memIndex, memTotal, tokensEmitted, maxTokens, elapsedMs, etaSecMem, etaSecRun int,
	model string,
) {
	if pc.hub == nil {
		return
	}
	preview := mem.Text
	if len(preview) > 80 {
		preview = preview[:80]
	}
	// Sensitive memories: redact the preview so the WS broadcast
	// doesn't leak the body. The audit-log path already does this for
	// the Tier 2 reasoning; mirror that policy here.
	if mem.Sensitive {
		preview = "[REDACTED — sensitive memory]"
	}
	pc.hub.Fanout(Event{
		SchemaVersion: SchemaVersion,
		Type:          "phase0b_memory_progress",
		Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
		AdapterID:     "sd-core-nightly",
		Payload: map[string]any{
			"run_id":             pc.runID,
			"memory_id":          mem.ID,
			"memory_preview":     preview,
			"memory_index":       memIndex,
			"memory_total":       memTotal,
			"tokens_emitted":     tokensEmitted,
			"max_tokens":         maxTokens,
			"elapsed_ms":         elapsedMs,
			"eta_seconds_memory": etaSecMem,
			"eta_seconds_run":    etaSecRun,
			"model":              model,
			"tier":               "tier2",
		},
	})
}

// providerName returns the human-readable name of an LLMProvider,
// best-effort. Nil-safe; returns "" when the provider doesn't expose
// a Name() method or is nil. Used in progress events so the dashboard
// can display "via meta-llama/Llama-3.1-70B-Instruct (AirLLM)".
func providerName(p LLMProvider) string {
	if p == nil {
		return ""
	}
	return p.Name()
}
