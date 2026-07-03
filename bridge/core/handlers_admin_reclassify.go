// handlers_admin_reclassify.go — POST /admin/sensitive/reclassify
//
// Bulk-runs the Layer 2 AI sensitivity classifier on every currently-sensitive
// memory and clears the flag on any whose updated classifier verdict is NOT
// sensitive. Used after the user tightens the classifier prompt to remove
// false-positives from the bank in one pass.
//
// Sticky-on policy nuance: normally sensitive=true is sticky and only the
// user can clear it via the trace-detail lock icon. This endpoint provides
// the bulk equivalent — it ONLY clears flags where the (now-updated)
// classifier itself says the memory is no longer sensitive. It does NOT
// blanket-clear, and it can never raise a flag (memories that come back
// YES are left as-is; they were already sensitive).
//
// Audit: every cleared flag writes an `auto_unflag_sensitive` row so the
// user can review what changed.
//
// Body: { "max": 500, "dry_run": false }   (both optional)
// Returns: {
//   scanned: int,
//   unflagged: int,
//   still_sensitive: int,
//   errors: int,
//   duration_ms: int,
//   sample_unflagged: [{id, justification?}, ...],  // first ~20
// }
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// sensitiveReclassifyInFlight is a single-flight guard preventing two
// concurrent bulk-reclassify runs. Same shape as deepEncodeAllInFlight —
// a second caller gets 409 with the in-flight start time. Without this
// the WS progress events from two parallel runs would interleave and
// the dashboard's progress bar would oscillate.
var sensitiveReclassifyInFlight sensitiveReclassifyInFlightFlag

type sensitiveReclassifyInFlightFlag struct {
	mu      sync.Mutex
	running bool
	startAt time.Time
}

func (f *sensitiveReclassifyInFlightFlag) acquire() (acquired bool, startedAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running {
		return false, f.startAt
	}
	f.running = true
	f.startAt = time.Now()
	return true, f.startAt
}

func (f *sensitiveReclassifyInFlightFlag) release() {
	f.mu.Lock()
	f.running = false
	f.mu.Unlock()
}

type reclassifyRequest struct {
	Max    int  `json:"max"`
	DryRun bool `json:"dry_run"`
	// RatePerMin throttles the classifier loop to N classifications per
	// minute. 0 = unlimited (default, current behavior). Useful when the
	// user wants the re-scrubber to share Tier 2 capacity with nightly
	// or research workflows without monopolising the GPU/CPU. The throttle
	// is applied as a sleep AFTER each classify call, so the first call
	// fires immediately and subsequent calls space out to honour the rate.
	RatePerMin int `json:"rate_per_min"`
}

type reclassifySample struct {
	ID            string `json:"id"`
	Justification string `json:"justification,omitempty"`
}

type reclassifyResponse struct {
	Scanned         int                `json:"scanned"`
	Unflagged       int                `json:"unflagged"`
	StillSensitive  int                `json:"still_sensitive"`
	Errors          int                `json:"errors"`
	DurationMs      int64              `json:"duration_ms"`
	DryRun          bool               `json:"dry_run"`
	SampleUnflagged []reclassifySample `json:"sample_unflagged,omitempty"`
}

// handleAdminSensitiveDefaultPrompt returns the hardcoded default prompt
// so the Privacy UI's "Reset to default" button can re-populate the
// textarea without shipping a duplicate copy of the prompt in the
// frontend. GET-only.
func (s *Server) handleAdminSensitiveDefaultPrompt(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET", "OPTIONS")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"key":     SensitiveClassifierPromptKey,
		"default": classifyPrompt,
	})
}

// handleAdminSensitiveReclassifyStatus reports whether a bulk reclassify
// is currently in flight (single-flight flag held) and, if so, when it
// started. Used by the Privacy config UI to restore the progress card
// when the user navigates back mid-run — WS progress events keep
// flowing but a fresh DOM mount wouldn't know to reveal the card
// without this poll.
func (s *Server) handleAdminSensitiveReclassifyStatus(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET", "OPTIONS")
		return
	}
	sensitiveReclassifyInFlight.mu.Lock()
	running := sensitiveReclassifyInFlight.running
	startAt := sensitiveReclassifyInFlight.startAt
	sensitiveReclassifyInFlight.mu.Unlock()
	resp := map[string]any{"running": running}
	if running {
		resp["started_at"] = startAt.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAdminSensitiveCount returns the current number of memories
// flagged sensitive. Used by the Privacy config "Bulk re-classify"
// panel to cap the Max input at what actually exists (was previously a
// fixed 5000 ceiling regardless of bank state). Read-only; no auth
// changes from the rest of /admin/*.
func (s *Server) handleAdminSensitiveCount(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET", "OPTIONS")
		return
	}
	if s.bank == nil {
		http.Error(w, "bank disabled", http.StatusServiceUnavailable)
		return
	}
	// 50k cap matches the upstream ListMemoriesWith pull cap that
	// the bulk-reclassify handler uses — anything above this would be
	// truncated anyway, so reporting the real-real count is moot.
	recs, err := s.bank.ListMemoriesWith(MemoryListOpts{Limit: 50000, OnlySensitive: true})
	if err != nil {
		http.Error(w, "count: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"sensitive_count": len(recs),
	})
}

func (s *Server) handleAdminReclassifySensitive(w http.ResponseWriter, r *http.Request) {
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
	// Respect the LLM-classifier toggle: when the optional Tier 1/2 pass is
	// turned off (sensitive_llm_classifier_enabled=0), this bulk LLM reprocess
	// is a no-op — Tier 0 (deterministic) still flags keys/cards/SSNs on every
	// write. Enable the setting to reprocess prose-level sensitivity.
	if !sensitiveLLMClassifierEnabled(s.bank) {
		http.Error(w, "LLM sensitivity classifier is disabled (sensitive_llm_classifier_enabled=0); "+
			"Tier 0 deterministic scanning still runs on every write. Enable it to bulk-reprocess.",
			http.StatusConflict)
		return
	}
	// Resolve provider via the tier-preference setting. Tier 2 is the
	// user's recommended choice (better precision on nuanced "discussing
	// OAuth abstractly is NOT sensitive" carve-outs); Tier 1 is the
	// fast default. Falls back to Tier 1 when Tier 2 isn't configured.
	classifierProvider := resolveSensitiveClassifierProvider(s.bank, s.router)
	if classifierProvider == nil {
		http.Error(w, "no realtime/nightly provider configured for sensitive classification", http.StatusServiceUnavailable)
		return
	}

	var body reclassifyRequest
	if r.ContentLength > 0 {
		if err := readJSON(r, &body); err != nil {
			http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	// "Reprocess all" by default. Previously this defaulted to 500 and
	// capped at 5000 — both produced "mixed prompt-version" banks where
	// only a slice of flagged memories had been classified under the
	// current prompt. The user-stated goal (v2.5.1b1) is that running
	// bulk reclassify should normalise EVERY flagged memory in one
	// pass; 0 = unlimited (sweep the full bank up to the
	// ListMemoriesWith pull cap below).
	maxN := body.Max
	if maxN <= 0 {
		maxN = 50000 // matches ListMemoriesWith's practical ceiling
	}
	if maxN > 50000 {
		maxN = 50000
	}

	// Single-flight gate — ONLY for real (commit) runs. Dry-runs are
	// read-only previews; they don't write to the bank, so two parallel
	// dry-runs (or a dry-run alongside a real-run) are harmless. Gating
	// only the real-run path lets a user click "Preview (dry-run)" then
	// immediately click "Run" without hitting 409 while the dry-run is
	// still classifying.
	flagReleasedByGoroutine := false
	if !body.DryRun {
		if acquired, prevStartedAt := sensitiveReclassifyInFlight.acquire(); !acquired {
			http.Error(w,
				"sensitive reclassify already running (started "+prevStartedAt.Format(time.RFC3339)+
					"). Wait for it to finish or restart the core container.",
				http.StatusConflict)
			return
		}
		defer func() {
			if !flagReleasedByGoroutine {
				sensitiveReclassifyInFlight.release()
			}
		}()
	}

	startedAt := time.Now()

	// Pull currently-flagged memories. Cap at maxN.
	recs, err := s.bank.ListMemoriesWith(MemoryListOpts{Limit: maxN, OnlySensitive: true})
	if err != nil {
		http.Error(w, "list sensitive: "+err.Error(), http.StatusInternalServerError)
		return
	}
	totalAtStart := len(recs)

	// Build a one-shot classifier instance bound to the resolved tier
	// (Tier 1 OR Tier 2 per the sensitive_classifier_tier setting).
	// Re-uses the effectivePrompt path so the user's customized prompt
	// is what drives the bulk pass.
	classifier := newSensitiveClassifierConst(s.bank, classifierProvider, 0, 0)
	log.Printf("admin_reclassify: starting %s pass · provider=%s · total=%d",
		map[bool]string{true: "dry-run", false: "commit"}[body.DryRun],
		classifierProvider.Name(), len(recs))

	// 202-Accepted: the classification loop runs to completion in a
	// background goroutine using context.Background(), NOT r.Context().
	// Browser disconnect (tab close, refresh, network blip) no longer
	// aborts the run. Live progress + completion arrive via WS events:
	//   sensitive_reclassify_progress { scanned, unflagged, still_sensitive,
	//     errors, total, elapsed_ms, eta_ms, current_memory }
	//   sensitive_reclassify_done     { … final counts + duration_ms }
	// Mirrors the deep-encode-all async refactor pattern.
	flagReleasedByGoroutine = true
	adapterID := adapterIDFromRequest(r)
	go s.runSensitiveReclassifyBackground(recs, classifier, body, totalAtStart, startedAt, adapterID)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"started":        true,
		"started_at":     startedAt.UTC().Format(time.RFC3339),
		"total_at_start": totalAtStart,
		"dry_run":        body.DryRun,
		"message":        "Bulk reclassify running in background. Progress streams via WebSocket (sensitive_reclassify_progress / _done events). Closing this page will not abort the run.",
	})
}

// runSensitiveReclassifyBackground runs the bulk classification loop
// detached from the originating HTTP request. Releases the
// single-flight flag on exit. Emits WS progress events (throttled to
// one per 250ms) for the dashboard's bar to animate, plus a terminal
// _done event with final counts.
func (s *Server) runSensitiveReclassifyBackground(
	recs []MemoryRecord, classifier *SensitiveClassifier,
	body reclassifyRequest, totalAtStart int, startedAt time.Time, adapterID string,
) {
	// Single-flight flag only held for real (commit) runs — see the
	// handler-side guard for the rationale. Releasing a flag that
	// wasn't acquired is a no-op (release() unconditionally clears
	// the running bit), but skipping the call entirely on dry-runs
	// keeps the intent obvious in code review.
	if !body.DryRun {
		defer sensitiveReclassifyInFlight.release()
	}
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("admin_reclassify: panic in background goroutine: %v", rec)
		}
	}()

	bgCtx := context.Background()
	resp := reclassifyResponse{DryRun: body.DryRun}

	// Time-throttle WS events: at most one every 250ms regardless of
	// how fast the loop spins. Caps the firehose on a bank with many
	// short, fast-classifying memories.
	const progressMinGap = 250 * time.Millisecond
	var lastEmit time.Time
	emitProgress := func(currentID, preview string, force bool) {
		if s.hub == nil {
			return
		}
		now := time.Now()
		if !force && now.Sub(lastEmit) < progressMinGap {
			return
		}
		lastEmit = now
		elapsedMs := time.Since(startedAt).Milliseconds()
		var etaMs int64
		if resp.Scanned > 0 && totalAtStart > resp.Scanned {
			remaining := int64(totalAtStart - resp.Scanned)
			perMem := elapsedMs / int64(resp.Scanned)
			etaMs = remaining * perMem
		}
		s.hub.Fanout(Event{
			SchemaVersion: SchemaVersion,
			Type:          "sensitive_reclassify_progress",
			Timestamp:     now.UTC().Format(time.RFC3339Nano),
			AdapterID:     "sd-core-admin",
			Payload: map[string]any{
				"scanned":         resp.Scanned,
				"unflagged":       resp.Unflagged,
				"still_sensitive": resp.StillSensitive,
				"errors":          resp.Errors,
				"total":           totalAtStart,
				"elapsed_ms":      elapsedMs,
				"eta_ms":          etaMs,
				"current_memory":  currentID,
				"memory_preview":  preview,
				"dry_run":         body.DryRun,
			},
		})
	}

	// Rate-limit gap (sleep AFTER each classify) — 0 disables throttle.
	// Saved as a setting so the UI can pre-fill from the user's last value.
	rate := body.RatePerMin
	if rate < 0 {
		rate = 0
	}
	if rate > 0 && s.bank != nil {
		_ = s.bank.SetSetting("sensitive_reclassify_rate_per_min",
			fmt.Sprintf("%d", rate))
	}
	var perCallGap time.Duration
	if rate > 0 {
		perCallGap = time.Minute / time.Duration(rate)
	}

	for i, rec := range recs {
		resp.Scanned++
		// Server-side context timeout: each classification call gets up
		// to 130s so the in-classify 120s timeout (sensitive_classifier.go)
		// is the controlling deadline. Previously this outer wrapper was
		// 30s, which silently capped the inner 120s and caused every
		// CPU-bound Tier 2 call to abort.
		ctx, cancel := context.WithTimeout(bgCtx, 130*time.Second)
		bodyText := rec.EnrichedText
		if bodyText == "" {
			bodyText = rec.Text
		}
		callStart := time.Now()
		isSensitive, justification, err := classifier.classify(ctx, bodyText)
		cancel()
		if err != nil {
			resp.Errors++
			log.Printf("admin_reclassify: classify %s failed: %v", rec.ID, err)
			emitProgress(rec.ID, "", false)
			continue
		}
		if isSensitive {
			resp.StillSensitive++
			emitProgress(rec.ID, "", false)
			continue
		}
		// Classifier now says NOT sensitive — clear the flag (unless dry-run).
		resp.Unflagged++
		if len(resp.SampleUnflagged) < 20 {
			resp.SampleUnflagged = append(resp.SampleUnflagged, reclassifySample{
				ID:            rec.ID,
				Justification: justification,
			})
		}
		if !body.DryRun {
			falseVal := false
			if _, err := s.bank.UpdateMemory(rec.ID, MemoryUpdate{Sensitive: &falseVal}); err != nil {
				resp.Errors++
				log.Printf("admin_reclassify: UpdateMemory %s failed: %v", rec.ID, err)
				emitProgress(rec.ID, "", false)
				continue
			}
			_ = s.bank.AppendAudit(AuditEntry{
				Operation:  "auto_unflag_sensitive",
				EntityType: "trace",
				EntityID:   rec.ID,
				AfterJSON: fmt.Sprintf(
					`{"prior_sensitive":true,"new_sensitive":false,"source":"bulk_reclassify"}`,
				),
				Reason:    "bulk re-classify under updated classifier prompt",
				AdapterID: adapterID,
			})
		}
		// Preview: short, redacted (sensitive content stays out of the
		// WS payload — the user is reviewing flag results, not bodies).
		emitProgress(rec.ID, "[redacted — sensitive memory]", false)

		// Rate-limit: sleep the remaining gap (if any) so the loop honours
		// the requested classifications-per-minute ceiling. Skips the
		// sleep on the final iteration to avoid a trailing idle pause.
		if perCallGap > 0 && i+1 < len(recs) {
			used := time.Since(callStart)
			if remaining := perCallGap - used; remaining > 0 {
				time.Sleep(remaining)
			}
		}
	}
	resp.DurationMs = time.Since(startedAt).Milliseconds()

	// Single summary audit row so the user can see the bulk op in /audit.
	if !body.DryRun && resp.Scanned > 0 {
		_ = s.bank.AppendAudit(AuditEntry{
			Operation:  "bulk_reclassify_sensitive",
			EntityType: "memories",
			EntityID:   fmt.Sprintf("bulk-%d", nextMonotonicNano()),
			AfterJSON: fmt.Sprintf(
				`{"scanned":%d,"unflagged":%d,"still_sensitive":%d,"errors":%d,"duration_ms":%d}`,
				resp.Scanned, resp.Unflagged, resp.StillSensitive, resp.Errors, resp.DurationMs,
			),
			Reason:    "user-triggered bulk sensitivity re-classification",
			AdapterID: adapterID,
		})
	}

	// Terminal "done" event so the dashboard can flip the bar to 100%
	// and surface final counts.
	if s.hub != nil {
		s.hub.Fanout(Event{
			SchemaVersion: SchemaVersion,
			Type:          "sensitive_reclassify_done",
			Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
			AdapterID:     "sd-core-admin",
			Payload: map[string]any{
				"scanned":         resp.Scanned,
				"unflagged":       resp.Unflagged,
				"still_sensitive": resp.StillSensitive,
				"errors":          resp.Errors,
				"total":           totalAtStart,
				"duration_ms":     resp.DurationMs,
				"dry_run":         body.DryRun,
				"sample_unflagged": resp.SampleUnflagged,
			},
		})
	}

	log.Printf("admin_reclassify: scanned=%d unflagged=%d still=%d errors=%d duration=%dms dry_run=%t",
		resp.Scanned, resp.Unflagged, resp.StillSensitive, resp.Errors, resp.DurationMs, body.DryRun)
}
