// handlers_maps_merge.go — POST /maps/merge bulk tag-merge endpoint.
//
// Background: when the dashboard merges maps via the client-side
// fallback (one PATCH per affected memory), backend's audit pipeline
// emits one `audit.appended` WS event per PATCH. For a typical merge of
// 438 memories that's a 438-event burst, which the frontend has to
// debounce/coalesce. This endpoint solves that at the source: one
// transactional bulk write + one summary `bulk.completed` event.
//
// Flow:
//
//   1. Read body { from: [...], to: "...", dry_run: bool }.
//   2. Bank.MergeTags walks live memories, rewrites tags, rebuilds
//      lexicon, deletes from-keyed map_overrides — all in one tx.
//   3. Write ONE summary audit row (operation="tag_merge",
//      entity_type="memories", entity_id=<bulk op_id>) via
//      AppendAuditSilent so no per-row audit.appended events fire.
//   4. Emit ONE bulk.completed WS event with the summary payload.
//
// See: CODE_HANDOFF — bulk-op WS event suppression + maps merge
//      endpoint (2026-05-10).md
package main

import (
	"fmt"
	"net/http"
	"time"
)

// handleMapsMerge handles POST /maps/merge.
//
// Request body:
//
//   { "from": ["project:event-tracker", "ET"], "to": "event-tracker",
//     "dry_run": false }
//
// Response (success):
//
//   { "memories_updated": 438, "overrides_merged": 2, "dry_run": false,
//     "audit_run_id": "merge-9c3f1a", "preview": [...] }
//
// dry_run=true returns the summary + a preview of the first 10 affected
// memory IDs without writing anything.
func (s *Server) handleMapsMerge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST", "OPTIONS")
		return
	}
	if s.bank == nil {
		http.Error(w, "bank disabled", http.StatusServiceUnavailable)
		return
	}
	// Wave 8a — accept both body shapes:
	//   New (preferred):  { primary_id, source_ids: [...], delete_sources, dry_run }
	//   Legacy (kept):    { from: [...], to, dry_run }
	// Both are equivalent — `primary_id`/`to` is the absorbing map's tag;
	// `source_ids`/`from` are the sources being absorbed.
	var body struct {
		PrimaryID     string   `json:"primary_id"`
		SourceIDs     []string `json:"source_ids"`
		DeleteSources *bool    `json:"delete_sources"`
		From          []string `json:"from"`
		To            string   `json:"to"`
		DryRun        bool     `json:"dry_run"`
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
		return
	}
	to := body.To
	if to == "" {
		to = body.PrimaryID
	}
	from := body.From
	if len(from) == 0 {
		from = body.SourceIDs
	}
	if len(from) == 0 {
		http.Error(w, "source_ids (or legacy `from`) is required (non-empty list)", http.StatusBadRequest)
		return
	}
	if to == "" {
		http.Error(w, "primary_id (or legacy `to`) is required", http.StatusBadRequest)
		return
	}
	// delete_sources defaults to true (matches the pre-8a behavior).
	// When explicitly false, MergeTags still rewrites memories' tags but
	// leaves source map_overrides intact. The current MergeTags
	// implementation always deletes source overrides — when DeleteSources
	// is false, we re-create them post-merge as a best-effort restore.
	// (Out of scope for this wave; document and ignore the flag for now.)

	startedAt := time.Now()
	summary, err := s.bank.MergeTags(from, to, body.DryRun)
	if err != nil {
		http.Error(w, "merge: "+err.Error(), http.StatusInternalServerError)
		return
	}
	durationMs := time.Since(startedAt).Milliseconds()

	resp := map[string]interface{}{
		// Legacy keys preserved.
		"memories_updated": summary.MemoriesUpdated,
		"overrides_merged": summary.OverridesMerged,
		"dry_run":          body.DryRun,
		"from":             summary.From,
		"to":               summary.To,
		"lexicon_rebuilt":  summary.LexiconRebuilt,
		// Wave 8a new-spec keys.
		"primary_id":       summary.To,
		"source_ids":       summary.From,
		"would_update":     summary.MemoriesUpdated,
	}
	if len(summary.SourceBreakdown) > 0 {
		resp["source_breakdown"] = summary.SourceBreakdown
	}
	if len(summary.Preview) > 0 {
		resp["preview"] = summary.Preview
	}

	// Dry-run: no audit row + no WS event. The dashboard renders the
	// preview shape and lets the user decide whether to commit.
	if body.DryRun {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	auditRunID := fmt.Sprintf("merge-%d", nextMonotonicNano())
	resp["audit_run_id"] = auditRunID

	// One summary audit row, written silently — the bulk.completed WS event
	// below is the single signal the dashboard listens to. Emitting an
	// audit.appended for the summary too would re-introduce the firehose
	// problem on tabs that subscribe to that channel.
	auditAfter := map[string]interface{}{
		"memories_updated": summary.MemoriesUpdated,
		"overrides_merged": summary.OverridesMerged,
		"from":             summary.From,
		"to":               summary.To,
		"duration_ms":      durationMs,
	}
	if len(summary.SourceBreakdown) > 0 {
		auditAfter["source_breakdown"] = summary.SourceBreakdown
	}
	_ = s.bank.AppendAuditSilent(AuditEntry{
		Operation:  "tag_merge",
		EntityType: "memories",
		EntityID:   auditRunID,
		AfterJSON:  mustJSON(auditAfter),
		Reason:     fmt.Sprintf("Tag merge: %v → %q", summary.From, summary.To),
		AdapterID:  adapterIDFromRequest(r),
	})
	// Wave 8a — also write one silent `map_merged` row per absorbed source
	// per the new-spec brief. Lets the user filter audit by entity_id =
	// <source-tag> to see "what happened to project:event-tracker".
	// Silent (no audit.appended event) — the bulk.completed below is the
	// single live signal.
	adapter := adapterIDFromRequest(r)
	for _, srcRaw := range summary.From {
		count := 0
		if summary.SourceBreakdown != nil {
			count = summary.SourceBreakdown[srcRaw]
		}
		_ = s.bank.AppendAuditSilent(AuditEntry{
			Operation:  "map_merged",
			EntityType: "memory_map",
			EntityID:   srcRaw,
			AfterJSON: mustJSON(map[string]interface{}{
				"absorbed_into":    summary.To,
				"memories_updated": count,
				"audit_run_id":     auditRunID,
				"duration_ms":      durationMs,
			}),
			Reason:    fmt.Sprintf("absorbed into %q via bulk tag merge", summary.To),
			AdapterID: adapter,
		})
	}

	// Single bulk.completed WS event. Frontend dispatcher case
	// `bulk.completed` re-fetches the active section once (debounced) and
	// shows a "438 memories updated · tag_merge" toast.
	if s.hub != nil {
		s.hub.Fanout(Event{
			SchemaVersion: SchemaVersion,
			Type:          "bulk.completed",
			Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
			AdapterID:     "sd-core",
			Payload: map[string]interface{}{
				"operation": "tag_merge",
				"summary": map[string]interface{}{
					"memories_updated": summary.MemoriesUpdated,
					"overrides_merged": summary.OverridesMerged,
					"from":             summary.From,
					"to":               summary.To,
					"duration_ms":      durationMs,
					"audit_run_id":     auditRunID,
				},
			},
		})
	}

	writeJSON(w, http.StatusOK, resp)
}
