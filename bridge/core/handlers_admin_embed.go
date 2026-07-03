// handlers_admin_embed.go — POST /admin/embed-all bulk-embed endpoint.
//
// Purpose: Phase 0's per-run embed pass only handles memories scanned in
// that run. After Phase 0 was retrofitted to actually embed (was a
// no-op metadata flip before; see handoff "Phase 0 silent embedding
// failure 2026-05-10"), users with thousands of pre-existing unembedded
// memories still need a one-shot backfill — waiting for nightly cycles
// is too slow.
//
// This endpoint walks every live memory, skips any whose hash is already
// present in the embeddings table, embeds the rest via Tier 1, and
// returns a summary { queued, skipped_existing, errors[], duration_ms }.
//
// Synchronous — the dashboard's "Backfill embeddings" button can render
// the response directly. For very large banks (10k+ memories) the caller
// should expect 5-30 minutes; we don't add a polling/run-id surface yet.
package main

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// embedAllInFlight is a package-level single-flight guard so only one
// /admin/embed-all loop runs at a time. Without this, a user clicking
// "Backfill" twice (or the browser auto-retrying after a 20s curl
// timeout while the server-side goroutine kept iterating) spawns two
// long-lived embed loops that BOTH emit `embed_backfill_progress`
// events. The dashboard's WS handler doesn't tag-filter by run, so the
// progress bar bounces between Run A's `resp.Embedded` and Run B's —
// looking like "100 new items keep adding and removing" when the two
// runs are ~100 memories apart.
//
// Returns 409 Conflict to subsequent callers with the in-flight start
// timestamp so the UI can surface a useful message.
var embedAllInFlight embedAllInFlightFlag

type embedAllInFlightFlag struct {
	mu      sync.Mutex
	running bool
	startAt time.Time
}

func (f *embedAllInFlightFlag) acquire() (acquired bool, startedAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running {
		return false, f.startAt
	}
	f.running = true
	f.startAt = time.Now()
	return true, f.startAt
}

func (f *embedAllInFlightFlag) release() {
	f.mu.Lock()
	f.running = false
	f.mu.Unlock()
}

// embedAllRequest is the POST body for /admin/embed-all.
type embedAllRequest struct {
	Model         string `json:"model"`           // optional override; default = SD_SYNAPSE_MODEL
	SkipExisting  bool   `json:"skip_existing"`   // when true, never re-embed an already-cached row
	MaxConcurrent int    `json:"max_concurrent"`  // currently unused (sync); reserved for a future async impl
}

// embedAllResponse is the wire shape returned to the caller.
type embedAllResponse struct {
	Queued          int      `json:"queued"`           // memories the endpoint considered
	Embedded        int      `json:"embedded"`         // count of new embeddings actually written
	SkippedExisting int      `json:"skipped_existing"` // count of cache hits when skip_existing=true
	Errors          []string `json:"errors"`           // per-memory error strings (capped at 50)
	DurationMs      int64    `json:"duration_ms"`
}

// handleAdminEmbedAll handles POST /admin/embed-all.
func (s *Server) handleAdminEmbedAll(w http.ResponseWriter, r *http.Request) {
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
	if s.router == nil || s.router.ForEmbedding() == nil {
		http.Error(w, "Tier 1 embedding provider not configured (check /settings/providers and that an embedding-capable model is pulled in Ollama)",
			http.StatusFailedDependency)
		return
	}
	// Single-flight gate. Two parallel embed-all goroutines emit
	// interleaved `embed_backfill_progress` WS events; the dashboard's
	// handler doesn't tag-filter by run, so the bar bounces between
	// each run's independent counter. 409 fast on the second caller.
	if acquired, startedAt := embedAllInFlight.acquire(); !acquired {
		http.Error(w,
			"embed-all already running (started "+startedAt.Format(time.RFC3339)+
				"). Wait for it to finish or restart the core container.",
			http.StatusConflict)
		return
	}
	defer embedAllInFlight.release()
	provider := s.router.ForEmbedding()

	var body embedAllRequest
	// Tolerate empty body — defaults are sensible (skip_existing=false,
	// model from env). Reject only on bad JSON shape.
	if r.ContentLength > 0 {
		if err := readJSON(r, &body); err != nil {
			http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	model := body.Model
	if model == "" {
		model = envOr("SD_SYNAPSE_MODEL", envOr("SD_CLASSIFY_MODEL", "llama3.2:3b"))
	}

	startedAt := time.Now()
	mems, err := s.bank.ListMemoriesWith(MemoryListOpts{Limit: 50000})
	if err != nil {
		http.Error(w, "list memories: "+err.Error(), http.StatusInternalServerError)
		return
	}

	resp := embedAllResponse{Queued: 0, Errors: []string{}}
	const errorCap = 50
	totalAtStart := len(mems)
	// Time-throttled progress emit: at most one event every progressMinGap
	// regardless of how fast the loop spins. Caps flood from a wall of
	// SkipExisting hits (cached embeddings resolve in <1ms each — without
	// a time gate we'd post 5000 events/sec). 250ms is fast enough to
	// look real-time in the UI's progress bar without saturating the WS.
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
		processed := resp.Embedded + resp.SkippedExisting + len(resp.Errors)
		if processed > 0 && totalAtStart > processed {
			remaining := int64(totalAtStart - processed)
			perMem := elapsedMs / int64(processed)
			etaMs = remaining * perMem
		}
		s.hub.Fanout(Event{
			SchemaVersion: SchemaVersion,
			Type:          "embed_backfill_progress",
			Timestamp:     now.UTC().Format(time.RFC3339Nano),
			AdapterID:     "sd-core-admin",
			Payload: map[string]any{
				"completed":      resp.Embedded,
				"skipped":        resp.SkippedExisting,
				"total":          totalAtStart,
				"errors":         len(resp.Errors),
				"elapsed_ms":     elapsedMs,
				"eta_ms":         etaMs,
				"current_memory": currentID,
				"memory_preview": preview,
				"model":          model,
			},
		})
	}

	for _, m := range mems {
		resp.Queued++
		// Build hash via the same recipe AllEmbeddingsForMemories uses, so
		// any embedding written here will be visible to every downstream
		// phase via the existing cache lookup.
		tagsAny := make([]interface{}, len(m.Tags))
		for i, t := range m.Tags {
			tagsAny[i] = t
		}
		embedText := synapseEmbedText(map[string]interface{}{
			"text": m.Text,
			"tags": tagsAny,
		})
		hash := synapseTextHash(embedText)

		didWork := true
		if body.SkipExisting {
			if existing, gerr := s.bank.GetEmbedding(hash); gerr == nil && existing != nil {
				resp.SkippedExisting++
				didWork = false
			}
		}
		if didWork {
			// ponytail: detach from r.Context() on purpose. This backfill
			// outlives the HTTP request — behind Cloudflare's ~100s edge
			// timeout the client disconnects (524) long before a large bank
			// finishes, and if the embed loop derived its context from the
			// request, that disconnect cascaded into `context canceled` on
			// every remaining memory (only the first ~100s of embeds landed).
			// Background context lets the single-flight loop run to completion
			// and report via the audit row + embed_backfill_done WS event.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			vec, eerr := provider.Embed(ctx, embedText)
			cancel()
			if eerr != nil {
				if len(resp.Errors) < errorCap {
					resp.Errors = append(resp.Errors, m.ID+": "+eerr.Error())
				}
			} else if serr := s.bank.SaveEmbedding(hash, vec, model); serr != nil {
				if len(resp.Errors) < errorCap {
					resp.Errors = append(resp.Errors, m.ID+": save "+serr.Error())
				}
			} else {
				resp.Embedded++
			}
		}
		// Per-memory progress (time-throttled). Fires for embed, skip, AND
		// error paths so the bar tracks the scan position through the
		// queue — not just newly-written embeddings. With ~half the queue
		// already cached, the prior "every 25 embedded" scheme made the
		// bar look stuck for long stretches; this is smoother.
		preview := m.Text
		if len(preview) > 80 {
			preview = preview[:80]
		}
		if m.Sensitive {
			preview = "[REDACTED — sensitive memory]"
		}
		emitProgress(m.ID, preview, false)
	}

	resp.DurationMs = time.Since(startedAt).Milliseconds()
	// Terminal "done" event so the dashboard can flip the bar to 100%
	// and either finish or hand off to phase 2 (deep-encode-all) when
	// the user used the combined backfill button.
	if s.hub != nil {
		s.hub.Fanout(Event{
			SchemaVersion: SchemaVersion,
			Type:          "embed_backfill_done",
			Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
			AdapterID:     "sd-core-admin",
			Payload: map[string]any{
				"completed":   resp.Embedded,
				"skipped":     resp.SkippedExisting,
				"total":       totalAtStart,
				"errors":      len(resp.Errors),
				"duration_ms": resp.DurationMs,
			},
		})
	}

	// One audit row for the whole bulk op (silent — we don't want to fire
	// audit.appended for what could be a multi-thousand-row backfill;
	// per the bulk-suppression convention from CODE_HANDOFF — bulk-op WS
	// event suppression 2026-05-10).
	_ = s.bank.AppendAuditSilent(AuditEntry{
		Operation:  "admin_embed_all",
		EntityType: "memories",
		EntityID:   "embed-all",
		AfterJSON:  mustJSON(resp),
		Reason:     "manual bulk embedding backfill",
		AdapterID:  adapterIDFromRequest(r),
	})

	writeJSON(w, http.StatusOK, resp)
}
