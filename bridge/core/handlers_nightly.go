// handlers_nightly.go — Dream Journal data endpoints.
//
//   GET    /nightly/runs?limit&since&status   list cached runs newest-first
//   DELETE /nightly/runs/{id}                 delete one journal record
//   POST   /nightly/trigger                   manually start a run
//
// All share the existing /nightly apiPrefix; the dispatcher distinguishes
// by path suffix to avoid the catchall trap that bit /admin/env_write and
// /settings/providers/{tier}/ping.
package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// nightlyRoot is the single entry point. Dispatches /nightly/runs vs
// /nightly/trigger before falling through to 404.
func (s *Server) nightlyRoot(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if s.bank == nil {
		http.Error(w, "bank disabled", http.StatusServiceUnavailable)
		return
	}
	// /nightly/runs/{id} — item-level routes. Path-precedence rule
	// applies: match the prefix BEFORE falling through to the
	// "/nightly/runs" collection case below, otherwise the trailing
	// /{id} would 404. Same convention as /admin/env_write and
	// /settings/providers/{tier}/ping.
	if strings.HasPrefix(r.URL.Path, "/nightly/runs/") {
		id := strings.TrimPrefix(r.URL.Path, "/nightly/runs/")
		if id == "" || strings.Contains(id, "/") {
			http.Error(w, "invalid run id", http.StatusBadRequest)
			return
		}
		s.handleNightlyRunByID(w, r, id)
		return
	}
	switch r.URL.Path {
	case "/nightly/runs":
		s.handleNightlyRunsGet(w, r)
	case "/nightly/trigger", "/nightly/run":
		// /nightly/run is the dashboard's preferred name (handoff
		// 2026-05-09); /nightly/trigger remains as the original route
		// from when the runner first shipped. Both are accepted aliases.
		s.handleNightlyTrigger(w, r)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// handleNightlyRunByID handles per-row operations on /nightly/runs/{id}.
// Currently DELETE only — GET-by-id can be added later if the dashboard
// ever needs it (the list endpoint already returns the full row shape).
func (s *Server) handleNightlyRunByID(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodDelete:
		s.handleNightlyRunDelete(w, r, id)
	default:
		methodNotAllowed(w, "DELETE", "OPTIONS")
	}
}

// handleNightlyRunDelete removes a Dream Journal record. Memories created
// during the run are preserved; cross_region_links rows referencing run_id
// are kept; per-phase audit rows are kept. The deletion itself appends one
// `nightly_run_deleted` audit row (with redacted before_json so a sensitive
// dream_entry can't leak through the audit channel) and emits one
// `nightly.deleted` WS event for cross-tab refresh.
//
// Response codes:
//   200 — deleted ok
//   404 — no such run
//   409 — status='in_progress' (would deadlock the runner's lock)
//   401 — handled by middleware
//
// See: CODE_HANDOFF — DELETE nightly runs (2026-05-10).md
func (s *Server) handleNightlyRunDelete(w http.ResponseWriter, r *http.Request, id string) {
	row, err := s.bank.GetNightlyRun(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]interface{}{"error": "no such run"})
			return
		}
		http.Error(w, "load: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if row.Status == "in_progress" {
		writeJSON(w, http.StatusConflict, map[string]interface{}{
			"error": "run still in progress",
			"id":    row.ID,
		})
		return
	}

	if err := s.bank.DeleteNightlyRun(id); err != nil {
		http.Error(w, "delete: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Audit row. before_json captures the deleted state for forensics
	// but redacts the dream_entry — Phase 12 already filters bubbles
	// whose source is sensitive, but defensive redaction here is cheap
	// insurance against future Phase-12 prompt regressions.
	auditBefore := redactNightlyRunForAudit(row)
	s.auditWrite(AuditEntry{
		Operation:  "nightly_run_deleted",
		EntityType: "nightly_run",
		EntityID:   id,
		BeforeJSON: mustJSON(auditBefore),
		AfterJSON:  "{}",
		Reason:     "user_deleted_via_dashboard",
		AdapterID:  adapterIDFromRequest(r),
	})

	// WS event so other dashboard tabs refresh.
	if s.hub != nil {
		s.hub.Fanout(Event{
			SchemaVersion: SchemaVersion,
			Type:          "nightly.deleted",
			Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
			AdapterID:     "sd-core",
			Payload: map[string]interface{}{
				"run_id":         id,
				"deleted_status": row.Status,
			},
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"deleted": true,
		"id":      id,
	})
}

// redactNightlyRunForAudit returns a copy of the run with the dream_entry
// stripped — the prose may quote thought bubbles whose source memory is
// sensitive, and we don't want that leaking through the audit channel
// even though Phase 12 already filters at write time. Defensive redaction.
func redactNightlyRunForAudit(r NightlyRun) map[string]interface{} {
	out := map[string]interface{}{
		"id":          r.ID,
		"started_at":  r.StartedAt,
		"finished_at": r.FinishedAt,
		"status":      r.Status,
		"model":       r.Model,
		"created_at":  r.CreatedAt,
	}
	if r.Error != "" {
		out["error"] = r.Error
	}
	// stats_json is structured counts/refs only — no memory bodies; safe.
	if r.StatsJSON != "" {
		out["stats_json"] = r.StatsJSON
	}
	// Narrative is observational voice without verbatim memory quotes; safe.
	if r.Narrative != "" {
		out["narrative"] = r.Narrative
	}
	if r.DreamEntryArchetype != "" {
		out["dream_entry_archetype"] = r.DreamEntryArchetype
	}
	// Dream entry is the redaction target. Note that it was redacted rather
	// than omitted so future forensics can see "yes, this run had a dream
	// entry, but its content is not in the audit log."
	if r.DreamEntry != "" {
		out["dream_entry"] = "[REDACTED — dream prose, body omitted from audit]"
	}
	return out
}

// handleNightlyRunsGet returns the rich run shape per the handoff. Each row's
// stats_json column is parsed into a structured `stats` field so the
// dashboard doesn't have to JSON-parse a JSON-string.
func (s *Server) handleNightlyRunsGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET", "OPTIONS")
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	// Wave 7c2 — accept `since_cursor` as the canonical cursor param;
	// `since` stays as a legacy alias.
	rows, err := s.bank.ListNightlyRuns(NightlyRunListOpts{
		Limit:  limit,
		Since:  pickSinceCursor(r),
		Status: q.Get("status"),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 2026-05-23 (Feature #6) — for the in-flight run only, pull the
	// current phase + start time from the runner's atomic fields. Empty
	// strings when no run is active or the runner is nil. We do this
	// outside the loop so multiple in_progress rows (shouldn't happen,
	// but defensive) all see the same snapshot.
	var inflightPhase, inflightPhaseStarted string
	if s.nightly != nil {
		inflightPhase, inflightPhaseStarted = s.nightly.CurrentPhase()
	}
	out := make([]map[string]interface{}, 0, len(rows))
	for _, r := range rows {
		entry := map[string]interface{}{
			"id":         r.ID,
			"started_at": r.StartedAt,
			"status":     r.Status,
			"created_at": r.CreatedAt,
		}
		if r.FinishedAt != "" {
			entry["finished_at"] = r.FinishedAt
		}
		// Surface current_phase / phase_started_at / phase_elapsed_ms on
		// the in_progress row so the Dream Journal card can show
		// "phase dedup · 4m12s elapsed" instead of just spinning.
		// Polling clients (no WS) see the same data the WS subscribers do.
		if r.Status == "in_progress" && inflightPhase != "" {
			entry["current_phase"] = inflightPhase
			if inflightPhaseStarted != "" {
				entry["phase_started_at"] = inflightPhaseStarted
				if t, terr := time.Parse(time.RFC3339Nano, inflightPhaseStarted); terr == nil {
					entry["phase_elapsed_ms"] = time.Since(t).Milliseconds()
				}
			}
		}
		// Parse stats_json into structured form. Defensive: empty / bad JSON
		// degrades to an empty stats object so the dashboard renders zeros.
		var stats map[string]interface{}
		if r.StatsJSON != "" {
			_ = json.Unmarshal([]byte(r.StatsJSON), &stats)
		}
		if stats == nil {
			stats = map[string]interface{}{}
		}
		entry["stats"] = stats
		// Optional fields — omitted when empty so the dashboard's
		// presence-checks behave as documented in the handoff.
		if r.Narrative != "" {
			entry["narrative"] = r.Narrative
		}
		if r.Model != "" {
			entry["model"] = r.Model
		}
		if r.Error != "" {
			entry["error"] = r.Error
		}
		out = append(out, entry)
	}
	resp := map[string]interface{}{
		"runs":  out,
		"count": len(out),
	}
	// Wave 7c2 — surface `next_cursor` + `section_clean_at` when the
	// lazy-fetch flag is enabled. Runs are returned newest-first; the
	// most-recent row's started_at is the cursor for the next fetch.
	if lazyFetchEnabled() {
		nextCursor := ""
		if len(rows) > 0 {
			nextCursor = rows[0].StartedAt
		}
		resp["next_cursor"] = nextCursor
		resp["section_clean_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleNightlyTrigger manually starts a Dream Cycle (or returns 409 if one
// is already in flight). Synchronous — the handler waits for completion.
// For long-running real consolidation passes the dashboard should treat
// this as a fire-and-poll: send the POST, immediately re-fetch /nightly/runs
// to see the in_progress card, then poll every few seconds for completion.
func (s *Server) handleNightlyTrigger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST", "OPTIONS")
		return
	}
	if s.nightly == nil {
		http.Error(w, "nightly runner not configured", http.StatusServiceUnavailable)
		return
	}
	// Pre-check + race-free 409: DoRun's TryLock is the source of truth.
	if running, currentID := s.nightly.IsRunning(); running {
		writeJSON(w, http.StatusConflict, map[string]interface{}{
			"error":          "nightly run in progress",
			"current_run_id": currentID,
		})
		return
	}
	run, err := s.nightly.DoRun(r.Context())
	if err != nil {
		if errors.Is(err, ErrNightlyAlreadyRunning) {
			// Lost the TryLock race between the IsRunning check above and
			// DoRun's lock attempt. Surface the same 409 shape.
			_, currentID := s.nightly.IsRunning()
			writeJSON(w, http.StatusConflict, map[string]interface{}{
				"error":          "nightly run in progress",
				"current_run_id": currentID,
			})
			return
		}
		http.Error(w, "trigger: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Audit was written by DoRun. Echo the row for the dashboard.
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":     run.ID,
		"status": run.Status,
		"note":   "Refresh /nightly/runs to see the full row + stats.",
	})
}

// handleNightlyReconcile force-marks any in_progress nightly row as
// interrupted, regardless of heartbeat freshness. Genuine ops UX win:
// when Tier 2 OOMs mid-cycle, the single-instance lock stays held and
// the dashboard's "trigger nightly" button gets stuck on 409 forever.
// Without this endpoint, recovery requires either a container restart
// or waiting for the next DoRun (which can't fire because of the
// lock). POST-only; no body.
func (s *Server) handleNightlyReconcile(w http.ResponseWriter, r *http.Request) {
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
	// Pass a zero duration so EVERY in_progress row is treated as stale —
	// the staleness check inside Bank.ReconcileStaleInProgress uses
	// time.Now().Add(-staleAfter), and a zero duration means "anything
	// older than now" which is every row.
	n, err := s.bank.ReconcileStaleInProgress(0)
	if err != nil {
		http.Error(w, "reconcile: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Best-effort: also clear the in-memory currentID atomic on the
	// runner so a fresh DoRun can fire immediately. Safe even when n=0
	// (no-op if currentID was already empty).
	if s.nightly != nil {
		s.nightly.currentID.Store("")
	}
	s.auditWrite(AuditEntry{
		Operation:  "nightly_reconcile",
		EntityType: "nightly_run",
		Reason:     fmt.Sprintf("force-marked %d in_progress run(s) as interrupted", n),
		AdapterID:  adapterIDFromRequest(r),
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"reconciled": n,
		"note":       "Stale in_progress rows force-marked as interrupted.",
	})
}
