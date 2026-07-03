// handlers_admin_deep_encode.go — POST /admin/deep-encode-all bulk
// Tier 2 enrichment endpoint. Mirrors /admin/embed-all's shape:
// synchronous, capped error array, 424 when Tier 2 unconfigured, single
// silent audit row.
//
// Purpose: Phase 0b processes ~100 memories per nightly run; a fresh
// bank of 3611 memories needs a month of nightlies to fully deep-encode.
// This endpoint backfills in one shot — the dashboard's "Backfill deep
// encoding (3611 memories · ~70 min)" button POSTs here when the user
// just turned the feature on.
//
// See handoff "deep per-memory enrichment phase 2026-05-10" §Initial run / cold start.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type deepEncodeAllRequest struct {
	SkipExisting     bool `json:"skip_existing"`     // skip memories already deep-encoded
	MaxConcurrent    int  `json:"max_concurrent"`    // reserved for future async impl; sync ignores
	TokenBudget      int  `json:"token_budget"`      // hard cap; defaults to settings.deep_enrich_token_budget × 20
	IncludeDirtyOnly bool `json:"include_dirty_only"` // when true, only re-enrich memories with marked_dirty_at set
}

type deepEncodeAllError struct {
	MemoryID string `json:"memory_id"`
	Error    string `json:"error"`
	Trigger  string `json:"trigger,omitempty"`
}

// deepEncodeAllInFlight is a package-level single-flight guard so only one
// /admin/deep-encode-all loop runs at a time. Without this, a user clicking
// "Backfill" twice (or the browser auto-retrying after a timeout) spawns
// two long-lived HTTP goroutines that each hold the bank's wmu for write
// turns and each hammer Tier 2 in parallel — leading to goroutine
// accumulation, healthz timeouts, and an unhealthy container.
//
// The atomic.Bool starts false; CompareAndSwap(false,true) gates entry,
// the handler clears it via defer. Returns 409 Conflict to subsequent
// callers with the start timestamp of the in-flight run.
var (
	deepEncodeAllInFlight    deepEncodeAllInFlightFlag
)

type deepEncodeAllInFlightFlag struct {
	mu      sync.Mutex
	running bool
	startAt time.Time
}

func (f *deepEncodeAllInFlightFlag) acquire() (acquired bool, startedAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running {
		return false, f.startAt
	}
	f.running = true
	f.startAt = time.Now()
	return true, f.startAt
}

func (f *deepEncodeAllInFlightFlag) release() {
	f.mu.Lock()
	f.running = false
	f.mu.Unlock()
}

// IsRunning reports whether a backfill goroutine currently holds the
// single-flight flag. Exposed for test polling so the async-202 path
// can be exercised end-to-end without requiring a WS subscriber.
func (f *deepEncodeAllInFlightFlag) IsRunning() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running
}

// adoptIntoBackground is a no-op marker that documents the lifecycle
// transition: the HTTP handler has acquired the flag and is about to spawn
// a goroutine that takes ownership. The goroutine calls release() on exit.
// We don't actually do anything here (the flag is already set), but having
// the call site makes the ownership transfer obvious in code review and
// gives us a hook if we later need to record "running in background since".
func (f *deepEncodeAllInFlightFlag) adoptIntoBackground() {}

type deepEncodeAllResponse struct {
	Queued           int                  `json:"queued"`
	Enriched         int                  `json:"enriched"`
	SkippedExisting  int                  `json:"skipped_existing"`
	Errors           []deepEncodeAllError `json:"errors"`
	ErrorsTruncated  int                  `json:"errors_truncated,omitempty"`
	TokensTotal      int                  `json:"tokens_total"`
	DurationMs       int64                `json:"duration_ms"`
}

func (s *Server) handleAdminDeepEncodeAll(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST", "OPTIONS")
		return
	}
	if s.bank == nil {
		http.Error(w, "bank disabled", http.StatusServiceUnavailable)
		return
	}
	// Single-flight gate. The deep-encode loop can run for tens of minutes
	// (up to ~70 min for 3000 memories on Ollama, hours on AirLLM) and
	// holds an HTTP goroutine the whole time. Two parallel invocations
	// blow through Tier 2 token budgets, double-write the same memory
	// rows, and stack goroutines until /healthz starts timing out. Reject
	// concurrent runs with 409 + the in-flight start time so the UI can
	// surface a useful message.
	if acquired, startedAt := deepEncodeAllInFlight.acquire(); !acquired {
		http.Error(w,
			"deep-encode-all already running (started "+startedAt.Format(time.RFC3339)+
				"). Wait for it to finish or restart the core container.",
			http.StatusConflict)
		return
	}
	// The flag is held by the handler until the background goroutine
	// takes over (via adoptIntoBackground + its own deferred release).
	// Any handler-side early-return path (validation failure, probe
	// failure, etc.) must release the flag — otherwise the next call
	// would 409 forever. flagReleasedByGoroutine flips to true once we
	// hand ownership off; the defer below skips its release in that case.
	flagReleasedByGoroutine := false
	defer func() {
		if !flagReleasedByGoroutine {
			deepEncodeAllInFlight.release()
		}
	}()
	// Require Tier 2 explicitly — ForNightly() silently falls back to Tier 1
	// when Tier 2 isn't set, which would let this endpoint write Tier 1 output
	// into deep_encoded_at columns and pretend it's "real" enrichment. We
	// want a hard 424 instead so the user fixes the config.
	tier2 := s.router.Tier2()
	if tier2 == nil {
		http.Error(w,
			"Tier 2 (Nightly) provider unconfigured. Configure under Config → Connection → LLM Providers.",
			http.StatusFailedDependency)
		return
	}
	// Boot the active Tier 2 sidecar before the loop runs. The lifecycle
	// manager stops idle sidecars after their policy timeout (5 min Ollama,
	// 10 min AirLLM), so by the time the user clicks "Backfill deep encoding"
	// the service is usually down — and without this we'd loop Chat() against
	// a stopped sidecar, eat the consecutiveFailureLimit, and bail with
	// "couldn't start the model." Mirrors the nightly_runner.go pattern:
	// reuse the "nightly_run" workflow policy (both ollama-tier2 and
	// airllm-tier2 list it in WarmForWorkflow) and pass an activeFilter so
	// we don't accidentally boot the inactive Tier 2 candidate.
	if s.lifecycle != nil {
		var activeFilter map[string]bool
		if s.router != nil {
			activeFilter = map[string]bool{}
			if name := sidecarNameForTier2Provider(s.router.Tier2()); name != "" {
				activeFilter[name] = true
			}
		}
		for _, lerr := range s.lifecycle.EnsureForWorkflow(r.Context(), "nightly_run", activeFilter) {
			// Non-fatal — if the sidecar truly can't start, the pre-flight
			// probe below catches it and we 503 fast rather than letting
			// the loop burn 10 min of goroutine time.
			log.Printf("admin_deep_encode_all: lifecycle ensure: %v", lerr)
		}
	}
	// Pre-flight reachability probe. Without this, a misconfigured Tier 2
	// (host not in compose, base_url typo, etc.) means the loop below
	// burns through 5 × llmCallTimeout (default 120s = 10 min) of held
	// HTTP goroutines — eventually pushing /healthz past Docker's
	// healthcheck window. The probe targets the provider's lightweight
	// health endpoint (Ollama /api/tags, AirLLM /healthz) rather than
	// Chat() so cold-but-starting providers don't get falsely 503'd
	// while their model loads.
	{
		probeCtx, probeCancel := context.WithTimeout(r.Context(), 8*time.Second)
		probeErr := probeTier2Reachable(probeCtx, tier2)
		probeCancel()
		if probeErr != nil {
			log.Printf("admin_deep_encode_all: Tier 2 (%s) pre-flight probe failed: %v", tier2.Name(), probeErr)
			http.Error(w,
				"Tier 2 ("+tier2.Name()+") is configured but unreachable: "+probeErr.Error()+
					". Check Config → Models → LLM Providers, or start the sidecar service.",
				http.StatusServiceUnavailable)
			return
		}
	}
	embedder := s.router.ForEmbedding()
	embedModel := envOr("SD_SYNAPSE_MODEL", envOr("SD_CLASSIFY_MODEL", "llama3.2:3b"))

	var body deepEncodeAllRequest
	if r.ContentLength > 0 {
		if err := readJSON(r, &body); err != nil {
			http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	settings, _ := s.bank.GetDreamPipelineSettings()
	tokenBudget := body.TokenBudget
	if tokenBudget <= 0 {
		// Default: 20× the per-run nightly budget. Reasonable for a
		// "click and walk away" backfill.
		tokenBudget = settings.DeepEnrichTokenBudget * 20
		if tokenBudget < 1_000_000 {
			tokenBudget = 1_000_000
		}
	}

	// Snapshot the dirty-queue size up-front so the response + WS events
	// both report against the same total. Best-effort — if the count fails,
	// we still proceed with total=0 (UI renders indeterminate bar).
	totalAtStart, _, _ := s.bank.CountDirtyMemories()
	startedAt := time.Now()
	adapterID := adapterIDFromRequest(r)

	// 202-Accepted: the loop runs to completion in a background goroutine
	// using context.Background(), NOT r.Context(). The browser closing its
	// fetch (network blip, tab refresh, sleep, dev tools reload, etc.)
	// no longer cancels the loop — the backfill keeps running until it
	// drains the queue, hits the token budget, or trips
	// consecutiveFailureLimit. Progress + completion are observed via WS
	// events (deep_encode_backfill_progress / _done) which the dashboard
	// already subscribes to. Single-flight guard above stays acquired
	// for the duration of the goroutine; the deferred release moves into
	// the goroutine via stealing the flag from the parent.
	deepEncodeAllInFlight.adoptIntoBackground()
	flagReleasedByGoroutine = true
	go s.runDeepEncodeAllBackground(deepEncodeAllBackgroundArgs{
		tier2:        tier2,
		embedder:     embedder,
		embedModel:   embedModel,
		body:         body,
		settings:     settings,
		tokenBudget:  tokenBudget,
		totalAtStart: totalAtStart,
		startedAt:    startedAt,
		adapterID:    adapterID,
	})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"started":            true,
		"started_at":         startedAt.UTC().Format(time.RFC3339),
		"total_at_start":     totalAtStart,
		"token_budget":       tokenBudget,
		"tier2":              tier2.Name(),
		"message":            "Backfill running in background. Progress streams via WebSocket (deep_encode_backfill_progress / _done events). Closing this page will not abort the run.",
	})
}

// deepEncodeAllBackgroundArgs is the snapshot of state captured at handler
// entry. The goroutine owns these — no shared state with the http.Request
// (which the runtime recycles after the handler returns).
type deepEncodeAllBackgroundArgs struct {
	tier2        LLMProvider
	embedder     LLMProvider
	embedModel   string
	body         deepEncodeAllRequest
	settings     DreamPipelineSettings
	tokenBudget  int
	totalAtStart int
	startedAt    time.Time
	adapterID    string
}

// runDeepEncodeAllBackground runs the deep-encoding loop detached from the
// originating HTTP request. Releases the single-flight flag on exit so the
// next /admin/deep-encode-all can fire (subject to the same probe/lifecycle
// gates). Cancellation is via the lifecycle manager's shutdown path or
// container restart — the loop intentionally uses context.Background() so
// browser disconnects don't abort it mid-stream.
func (s *Server) runDeepEncodeAllBackground(a deepEncodeAllBackgroundArgs) {
	defer deepEncodeAllInFlight.release()
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("admin_deep_encode_all: panic in background goroutine: %v", rec)
		}
	}()

	bgCtx := context.Background()
	const errorCap = 50
	// 20 in a row is a "Tier 2 is genuinely dead" signal — not "this
	// memory's prompt happens to confuse the model." Was 5 originally,
	// which bailed mid-backfill on transient parser/hallucination
	// failures after only 9 successes (see admin_deep_encode_all log
	// for the v2.5.1b1-cut backfill: enriched=9/2831 errors=5 then quit).
	// The pre-flight /api/tags probe + lifecycle.EnsureForWorkflow above
	// already catch the "Tier 2 is dead at start" case fast; the limit
	// here is just the runtime safety net for a Tier 2 that goes down
	// MID-run (sidecar crash, host network blip), so it can afford to
	// be more generous.
	const consecutiveFailureLimit = 20

	resp := deepEncodeAllResponse{Errors: []deepEncodeAllError{}}
	consecutiveFailures := 0
	tier2 := a.tier2
	body := a.body
	embedder := a.embedder
	embedModel := a.embedModel
	tokenBudget := a.tokenBudget
	totalAtStart := a.totalAtStart
	startedAt := a.startedAt

	// Build a one-shot pipelineContext so we can reuse Phase 0b's helpers
	// (enrichMemoryWithTier2, applyTagSuggestions, buildSummaryDiff).
	// CRITICAL: pc.lifecycle must be set so signalProviderUse("nightly_run")
	// — called inside enrichMemoryWithTier2 before every Tier 2 Chat() —
	// actually bumps the lifecycle manager's last_used_at. Without this,
	// the 5-min idle reaper stops the tier2 sidecar mid-backfill and the
	// loop dies on consecutiveFailureLimit a moment later. Mirrors
	// `nightly_pipeline.go` line ~546: `lifecycle: n.lifecycle`.
	pc := &pipelineContext{
		ctx:       bgCtx,
		runID:     "admin-deepencode-" + startedAt.Format("150405"),
		bank:      s.bank,
		router:    s.router,
		hub:       s.hub,
		now:       time.Now().UTC(),
		settings:  a.settings,
		stats:     &NightlyStats{},
		lifecycle: s.lifecycle,
	}
	if s.router != nil {
		pc.localConcepts = s.router.LocalConcepts
	}

	var exitReason string
	for {
		if bgCtx.Err() != nil {
			exitReason = "bgCtx cancelled (container shutdown)"
			break
		}
		if resp.TokensTotal >= tokenBudget {
			exitReason = fmt.Sprintf("token budget reached (%d ≥ %d)", resp.TokensTotal, tokenBudget)
			break
		}
		if consecutiveFailures >= consecutiveFailureLimit {
			exitReason = fmt.Sprintf("consecutive failures (%d) hit limit (%d) — Tier 2 looks unhealthy",
				consecutiveFailures, consecutiveFailureLimit)
			break
		}
		mem, ok, err := s.bank.PickNextDirtyMemory()
		if err != nil {
			log.Printf("admin_deep_encode_all: pick-next failed: %v", err)
			// Tell the dashboard the loop crashed so it stops waiting.
			if s.hub != nil {
				s.hub.Fanout(Event{
					SchemaVersion: SchemaVersion,
					Type:          "deep_encode_backfill_done",
					Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
					AdapterID:     "sd-core-admin",
					Payload: map[string]any{
						"completed": resp.Enriched, "total": totalAtStart,
						"errors": len(resp.Errors) + 1, "tokens_total": resp.TokensTotal,
						"duration_ms": time.Since(startedAt).Milliseconds(),
						"error":       "pick-next: " + err.Error(),
					},
				})
			}
			return
		}
		if !ok {
			exitReason = "queue drained (no more dirty memories)"
			break
		}
		resp.Queued++

		// IncludeDirtyOnly: skip never-encoded memories, only refresh
		// already-flagged ones.
		if body.IncludeDirtyOnly && mem.DeepEncodedAt == "" && mem.DirtyReason != "edited" && mem.DirtyReason != "model_upgrade" {
			// Mark "clean" so the queue moves on, but don't enrich.
			s.bank.wmu.Lock()
			_, _ = s.bank.db.Exec(
				`UPDATE memories SET marked_dirty_at = '', dirty_reason = '' WHERE id = ?`,
				mem.ID,
			)
			s.bank.wmu.Unlock()
			resp.SkippedExisting++
			continue
		}
		if body.SkipExisting && mem.DeepEncodedAt != "" && mem.MarkedDirtyAt <= mem.DeepEncodedAt {
			// Shouldn't normally happen — PickNextDirtyMemory only
			// returns dirty rows — but be defensive.
			resp.SkippedExisting++
			continue
		}

		result, err := pc.enrichMemoryWithTier2(tier2, mem)
		if err != nil {
			consecutiveFailures++
			if len(resp.Errors) < errorCap {
				resp.Errors = append(resp.Errors, deepEncodeAllError{
					MemoryID: mem.ID, Error: err.Error(), Trigger: mem.DirtyReason,
				})
			} else {
				resp.ErrorsTruncated++
			}
			// Log + push to back of queue so the loop advances even when
			// every Tier 2 call fails. Without this we'd infinite-loop on
			// the oldest dirty memory.
			_ = s.bank.LogDeepEncodingFailure(mem.ID, tier2.Name(), err.Error(), mem.DirtyReason)
			continue
		}
		consecutiveFailures = 0 // reset on a real success
		newTags := applyTagSuggestions(mem.Tags, result.TagsAdded, result.TagsRemoved)
		summaryDiff := buildSummaryDiff(mem, result, newTags)
		auditReason := result.Reasoning
		if mem.Sensitive {
			auditReason = "[REDACTED — sensitive memory; Tier 2 reasoning omitted]"
		}
		if perr := s.bank.PersistDeepEncoding(
			mem.ID, result.SummaryNew, newTags, result.RegionNew,
			tier2.Name(), auditReason,
			result.TokensIn, result.TokensOut, summaryDiff, mem.DirtyReason,
		); perr != nil {
			if len(resp.Errors) < errorCap {
				resp.Errors = append(resp.Errors, deepEncodeAllError{
					MemoryID: mem.ID, Error: "persist: " + perr.Error(),
				})
			} else {
				resp.ErrorsTruncated++
			}
			continue
		}

		if embedder != nil {
			// CRITICAL: embed against raw `mem.Text`, NOT `result.SummaryNew`.
			// Embeddings are keyed by `hash(text + tags)` per the embed-all
			// + CountEmbeddedMemories convention (see handlers_admin_embed.go
			// line 139 and bank_p5.go CountEmbeddedMemories). If we hash
			// the enriched summary here instead, the embedding lands under
			// a key the coverage query never looks for — the memory then
			// appears un-embedded even though a fresh vector exists. That's
			// what produced the "embeddings going DOWN as Phase 2 runs"
			// regression. Tags may have changed via applyTagSuggestions, so
			// we still re-embed with `newTags` — but the text source must
			// stay the raw immutable `mem.Text` to match the hash convention.
			tagsAny := make([]interface{}, len(newTags))
			for i, t := range newTags {
				tagsAny[i] = t
			}
			embedText := synapseEmbedText(map[string]interface{}{
				"text": mem.Text, "tags": tagsAny,
			})
			ectx, ecancel := context.WithTimeout(bgCtx, 30*time.Second)
			vec, eerr := embedder.Embed(ectx, embedText)
			ecancel()
			if eerr == nil {
				_ = s.bank.SaveEmbedding(synapseTextHash(embedText), vec, embedModel)
			}
		}

		resp.Enriched++
		resp.TokensTotal += result.TokensIn + result.TokensOut
		// Charge token budget under tier2 so the daily/weekly chips reflect
		// the spend.
		if s.bank != nil {
			external := !tier2.IsLocal()
			_ = s.bank.AddTokensUsedV2(time.Now().UTC().Format("2006-01-02"),
				string(Tier2), providerKindFromName(tier2.Name()), tier2.Name(),
				external, result.TokensIn, result.TokensOut)
		}
		// Per-memory progress event for the Dream Journal's backfill bar.
		// Long-running endpoint (~70 min for 3000 memories with Ollama,
		// many hours with AirLLM) — the dashboard subscribes to this
		// event while a backfill is in flight so the user has visual
		// feedback instead of a frozen "Backfilling…" status.
		if s.hub != nil {
			elapsedMs := time.Since(startedAt).Milliseconds()
			var etaMs int64
			if resp.Enriched > 0 && totalAtStart > resp.Enriched {
				remaining := int64(totalAtStart - resp.Enriched)
				perMem := elapsedMs / int64(resp.Enriched)
				etaMs = remaining * perMem
			}
			preview := mem.Text
			if len(preview) > 80 {
				preview = preview[:80]
			}
			if mem.Sensitive {
				preview = "[REDACTED — sensitive memory]"
			}
			s.hub.Fanout(Event{
				SchemaVersion: SchemaVersion,
				Type:          "deep_encode_backfill_progress",
				Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
				AdapterID:     "sd-core-admin",
				Payload: map[string]any{
					"completed":      resp.Enriched,
					"total":          totalAtStart,
					"errors":         len(resp.Errors),
					"tokens_total":   resp.TokensTotal,
					"elapsed_ms":     elapsedMs,
					"eta_ms":         etaMs,
					"current_memory": mem.ID,
					"memory_preview": preview,
					"model":          tier2.Name(),
				},
			})
		}
	}
	// Final "done" event so the dashboard can flip the bar to 100% +
	// auto-hide. Fires whether the loop exited normally, hit the
	// consecutive-failure threshold, or ran the budget out.
	if s.hub != nil {
		s.hub.Fanout(Event{
			SchemaVersion: SchemaVersion,
			Type:          "deep_encode_backfill_done",
			Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
			AdapterID:     "sd-core-admin",
			Payload: map[string]any{
				"completed":    resp.Enriched,
				"total":        totalAtStart,
				"errors":       len(resp.Errors),
				"tokens_total": resp.TokensTotal,
				"duration_ms":  time.Since(startedAt).Milliseconds(),
			},
		})
	}

	resp.DurationMs = time.Since(startedAt).Milliseconds()

	// Trim to errorCap if we went past somehow (defensive).
	if len(resp.Errors) > errorCap {
		resp.ErrorsTruncated += len(resp.Errors) - errorCap
		resp.Errors = resp.Errors[:errorCap]
	}

	// Single silent audit row. Per-memory grain lives in deep_encoding_log.
	auditAfter := strings.Builder{}
	auditAfter.WriteString(`{"queued":`)
	auditAfter.WriteString(itoa(resp.Queued))
	auditAfter.WriteString(`,"enriched":`)
	auditAfter.WriteString(itoa(resp.Enriched))
	auditAfter.WriteString(`,"errors":`)
	auditAfter.WriteString(itoa(len(resp.Errors)))
	auditAfter.WriteString(`,"errors_truncated":`)
	auditAfter.WriteString(itoa(resp.ErrorsTruncated))
	auditAfter.WriteString(`,"tokens_total":`)
	auditAfter.WriteString(itoa(resp.TokensTotal))
	auditAfter.WriteString(`,"duration_ms":`)
	auditAfter.WriteString(itoa(int(resp.DurationMs)))
	auditAfter.WriteString(`}`)
	_ = s.bank.AppendAuditSilent(AuditEntry{
		Operation:  "admin_deep_encode_all",
		EntityType: "memories",
		EntityID:   "deep-encode-all",
		AfterJSON:  auditAfter.String(),
		Reason:     "manual deep-encoding backfill",
		AdapterID:  a.adapterID,
	})
	log.Printf("admin_deep_encode_all: background run done — enriched=%d/%d errors=%d tokens=%d duration=%dms · exit: %s",
		resp.Enriched, totalAtStart, len(resp.Errors), resp.TokensTotal, resp.DurationMs, exitReason)
}

// probeTier2Reachable checks whether the Tier 2 provider's host endpoint
// responds to a lightweight health probe within the caller's deadline.
// Targets /api/tags for Ollama (returns even when the model isn't yet
// loaded into VRAM) and AirLLMProvider.Ping for AirLLM (which knows about
// the loaded/unloaded distinction). Returns nil when the endpoint is
// reachable enough to start the loop; the loop's per-Chat timeouts and
// consecutiveFailureLimit handle deeper failures.
//
// Unknown provider types fall through to a Chat() probe — keeps the
// safety net while not blocking new provider implementations.
func probeTier2Reachable(ctx context.Context, p LLMProvider) error {
	switch prov := p.(type) {
	case *OllamaProvider:
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			strings.TrimRight(prov.BaseURL, "/")+"/api/tags", nil)
		if err != nil {
			return err
		}
		resp, err := prov.client().Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 500 {
			return fmt.Errorf("ollama /api/tags returned %d", resp.StatusCode)
		}
		return nil
	case *AirLLMProvider:
		return prov.Ping(ctx)
	default:
		// Remote providers (OpenAI/Anthropic/Custom) — unusual for Tier 2
		// but possible. Issue a minimal Chat() probe; these endpoints
		// respond fast when reachable, so the 8s deadline is plenty.
		_, err := p.Chat(ctx, []Message{{Role: "user", Content: "ping"}}, 4)
		return err
	}
}

// itoa is a tiny no-allocation int → string formatter for the audit
// JSON snippet above. Avoids pulling in strconv just for one call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
