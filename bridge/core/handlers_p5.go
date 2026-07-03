// handlers_p5.go — Phase 5 HTTP endpoints.
//
// Routes:
//   PATCH  /bank/memories/{id}            update fields
//   DELETE /bank/memories/{id}            soft-delete (?hard=1 hard-deletes)
//   POST   /bank/memories/{id}/restore    clear deleted_at
//   POST   /bank/memories/{id}/lifecycle  set lifecycle flag
//
//   GET    /maps                          list (?type=&limit=)
//   POST   /maps                          create
//   GET    /maps/{id}                     single
//   PATCH  /maps/{id}                     update
//   DELETE /maps/{id}                     delete (cascades)
//   GET    /maps/{id}/associations        list (?direction=from|to|both)
//   POST   /maps/{id}/associations        body: {to_map_id, association, weight}
//   DELETE /maps/{id}/associations/{to}/{type}
//   GET    /maps/{id}/traces              list trace IDs linked to this map
//   POST   /maps/{id}/traces              body: {trace_id}
//   DELETE /maps/{id}/traces/{trace_id}
//
//   GET    /lexicon                       list (?tag=&limit=)
//   POST   /lexicon                       upsert one row (admin/nightly)
//   DELETE /lexicon                       body: {tag_a, tag_b}
//
//   GET    /audit                         list (?entity_type=&entity_id=&operation=&since=&limit=)
//
//   GET    /research                      list (?limit=)
//   GET    /research/{topic}              single
//   POST   /research                      upsert
//   DELETE /research/{topic}
//
//   GET    /budget                        ?date=YYYY-MM-DD or ?days=N for history
//   POST   /budget                        body: {date?, tokens}    add tokens used
//
// Convention: every write op appends to audit_log. before_json is captured
// via a pre-read of the entity where applicable.
package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func methodNotAllowed(w http.ResponseWriter, allowed ...string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func readJSON(r *http.Request, dst interface{}) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 256*1024))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return errors.New("empty body")
	}
	return json.Unmarshal(body, dst)
}

func mustJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// adapterIDFromRequest reads X-Adapter-Id header for audit attribution.
func adapterIDFromRequest(r *http.Request) string {
	return r.Header.Get("X-Adapter-Id")
}

// emitMapUpdated publishes a `map.updated` control-channel event so the
// dashboard's Maps panel can re-render without polling. Nil-safe (skips
// silently if hub is unwired in tests). changedFields is optional — pass
// nil to omit it. The dashboard already handles absence gracefully.
func (s *Server) emitMapUpdated(mapID string, changedFields []string) {
	if s == nil || s.hub == nil || mapID == "" {
		return
	}
	payload := map[string]interface{}{"map_id": mapID}
	if len(changedFields) > 0 {
		payload["changed_fields"] = changedFields
	}
	s.hub.Fanout(Event{
		SchemaVersion: SchemaVersion,
		Type:          "map.updated",
		Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
		AdapterID:     "sd-core-maps",
		Payload:       payload,
	})
}

// auditWrite is a non-fatal helper — logs the audit failure but does not
// fail the user's request.
func (s *Server) auditWrite(e AuditEntry) {
	if s.bank == nil {
		return
	}
	if err := s.bank.AppendAudit(e); err != nil {
		// Never block the user's request on audit failure; just log.
		// (No log import here — caller can re-emit if needed.)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// /bank/memories/{id}[/{action}]  (extends existing bankByID)
// ──────────────────────────────────────────────────────────────────────────

// bankByIDExtended replaces the original GET-only bankByID. Handles
// GET / PATCH / DELETE on /bank/memories/{id} plus
// POST on /bank/memories/{id}/restore and /bank/memories/{id}/lifecycle.
func (s *Server) bankByIDExtended(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if s.bank == nil {
		http.Error(w, "bank disabled", http.StatusServiceUnavailable)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/bank/memories/")
	if rest == "" {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	parts := strings.Split(rest, "/")
	id := parts[0]
	if id == "" {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}

	if len(parts) >= 2 {
		switch parts[1] {
		case "restore":
			s.handleMemoryRestore(w, r, id)
			return
		case "lifecycle":
			s.handleMemoryLifecycle(w, r, id)
			return
		case "dirty", "redream":
			// /dirty kept as alias per CODE_HANDOFF — Rename dirty to
			// undreamt (2026-05-10). /redream is the new canonical path.
			s.handleMemoryDirty(w, r, id)
			return
		case "supersede":
			// R10 supersede semantics:
			//   GET   /bank/memories/{id}/supersede                — list chain edges
			//   POST  /bank/memories/{id}/supersede                — body {superseded_id, valid_from?, reason?}
			//   DELETE /bank/memories/{id}/supersede/{other_id}    — remove one edge
			s.handleMemorySupersede(w, r, id, parts[2:])
			return
		default:
			http.Error(w, "unknown sub-resource", http.StatusNotFound)
			return
		}
	}

	switch r.Method {
	case http.MethodGet:
		rec, err := s.bank.GetMemory(id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, rec)
	case http.MethodPatch:
		s.handleMemoryPatch(w, r, id)
	case http.MethodDelete:
		s.handleMemoryDelete(w, r, id)
	default:
		methodNotAllowed(w, "GET", "PATCH", "DELETE", "OPTIONS")
	}
}

func (s *Server) handleMemoryPatch(w http.ResponseWriter, r *http.Request, id string) {
	before, err := s.bank.GetMemory(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var patch MemoryUpdate
	if err := readJSON(r, &patch); err != nil {
		http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
		return
	}
	after, err := s.bank.UpdateMemory(id, patch)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.auditWrite(AuditEntry{
		Operation: "update", EntityType: "trace", EntityID: id,
		BeforeJSON: mustJSON(before), AfterJSON: mustJSON(after),
		Reason: r.URL.Query().Get("reason"), AdapterID: adapterIDFromRequest(r),
	})
	// Bubble freshness: when text or tags change, the cached bubble for this
	// memory is stale (it was generated against the prior content). Wipe the
	// mode mutations and enqueue regeneration. The generator skips the actual
	// regeneration if the SourceHash didn't drift — but we always invalidate
	// modes here because they're a derivative.
	if (patch.Text != nil || patch.Tags != nil) && s.bubbleGen != nil {
		s.bubbleGen.InvalidateAndEnqueue(id)
	}
	// [R3 cascading] CITATIONS.md #9 STC. When the edited memory is itself
	// a source for any synthesis or schema product, those products are
	// stale — mark them dirty with reason='cascading' so the next nightly
	// run re-thinks them. Best-effort: failures here don't fail the user's
	// edit. Runs AFTER UpdateMemory's wmu is released.
	if updateMemoryContentChanged(patch) {
		if n, err := s.bank.MarkSynthesisProductsDirty(id); err == nil && n > 0 {
			s.auditWrite(AuditEntry{
				Operation:  "memory_cascading_dirty",
				EntityType: "memory",
				EntityID:   id,
				AfterJSON:  fmt.Sprintf(`{"products_marked":%d}`, n),
				Reason:     "edit cascaded to synthesis products",
				AdapterID:  adapterIDFromRequest(r),
			})
		}
	}
	writeJSON(w, http.StatusOK, after)
}

func (s *Server) handleMemoryDelete(w http.ResponseWriter, r *http.Request, id string) {
	hard := r.URL.Query().Get("hard") == "1"
	before, err := s.bank.GetMemory(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	op := "soft_delete"
	if hard {
		err = s.bank.HardDeleteMemory(id)
		op = "delete"
	} else {
		err = s.bank.SoftDeleteMemory(id)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditWrite(AuditEntry{
		Operation: op, EntityType: "trace", EntityID: id,
		BeforeJSON: mustJSON(before), Reason: r.URL.Query().Get("reason"),
		AdapterID: adapterIDFromRequest(r),
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMemoryRestore(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST", "OPTIONS")
		return
	}
	before, _ := s.bank.GetMemory(id) // best-effort for audit
	if err := s.bank.UndeleteMemory(id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	after, _ := s.bank.GetMemory(id)
	s.auditWrite(AuditEntry{
		Operation: "restore", EntityType: "trace", EntityID: id,
		BeforeJSON: mustJSON(before), AfterJSON: mustJSON(after),
		AdapterID: adapterIDFromRequest(r),
	})
	writeJSON(w, http.StatusOK, after)
}

func (s *Server) handleMemoryLifecycle(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST", "OPTIONS")
		return
	}
	var body struct {
		Flag  string `json:"flag"`
		Value bool   `json:"value"`
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
		return
	}
	before, _ := s.bank.GetMemory(id)
	if err := s.bank.SetMemoryLifecycle(id, body.Flag, body.Value); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	after, err := s.bank.GetMemory(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditWrite(AuditEntry{
		Operation: "lifecycle", EntityType: "trace", EntityID: id,
		BeforeJSON: mustJSON(before), AfterJSON: mustJSON(after),
		Reason: body.Flag, AdapterID: adapterIDFromRequest(r),
	})
	writeJSON(w, http.StatusOK, after)
}

// handleMemoryDirty implements [R11] manual Targeted Memory Reactivation
// (CITATIONS.md #3 — Paller, Creery & Schechtman 2021). The user marks a
// specific memory for re-prioritization in the next Phase 0b run. Body:
//
//   { "reason": "tmr_user" }   // optional; defaults to tmr_user
//
// reason is constrained to the allowed dirty-flag values to prevent the
// endpoint from poisoning the queue with arbitrary strings. tmr_user is
// the canonical value; "edited" / "cascading" / "surprise" / "emotional"
// are also accepted so the dashboard can re-mark with a richer cause.
//
// Side effect: the memory hops to the front of the Phase 0b dirty queue
// because PickNextDirtyMemory orders by reason precedence — tmr_user
// outranks every existing reason except 'created'. (See bank_p5.go
// PickNextDirtyMemory's CASE expression.)
func (s *Server) handleMemoryDirty(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST", "OPTIONS")
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	// Tolerate empty body — default to tmr_user.
	if r.ContentLength > 0 {
		if err := readJSON(r, &body); err != nil {
			http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	if body.Reason == "" {
		body.Reason = "tmr_user"
	}
	allowed := map[string]bool{
		"tmr_user":          true,
		"edited":            true,
		"cascading":         true,
		"surprise":          true,
		"emotional":         true,
		"temporal_neighbor": true,
	}
	if !allowed[body.Reason] {
		http.Error(w, "invalid reason; expected one of: tmr_user, edited, cascading, surprise, emotional, temporal_neighbor", http.StatusBadRequest)
		return
	}
	// 404 if memory doesn't exist (better UX than a silent no-op).
	if _, err := s.bank.GetMemory(id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.bank.MarkMemoryDirty(id, body.Reason); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	after, _ := s.bank.GetMemory(id)
	// Audit operation uses the new name per CODE_HANDOFF — Rename dirty
	// to undreamt; old audit rows from the prior /dirty endpoint stay
	// under `memory_marked_dirty` (history record).
	s.auditWrite(AuditEntry{
		Operation:  "memory_marked_for_redream",
		EntityType: "memory",
		EntityID:   id,
		AfterJSON: fmt.Sprintf(`{"reason":%q,"marked_for_redream_at":%q}`,
			after.DirtyReason, after.MarkedDirtyAt),
		Reason:    body.Reason,
		AdapterID: adapterIDFromRequest(r),
	})
	// Response carries both forms during the transition window.
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":                    id,
		"dirty_reason":          after.DirtyReason,
		"marked_dirty_at":       after.MarkedDirtyAt,
		"redream_reason":        after.DirtyReason,
		"marked_for_redream_at": after.MarkedDirtyAt,
	})
}

// handleMemorySupersede serves the R10 bi-temporal supersede endpoints:
//
//   GET    /bank/memories/{id}/supersede                   — list edges touching id
//   POST   /bank/memories/{id}/supersede                   — id is the superseder
//                                                            body: {superseded_id, valid_from?, reason?}
//   DELETE /bank/memories/{id}/supersede/{superseded_id}   — undo edge
//
// Raw memory text is NEVER mutated — supersede is recorded in a separate edge
// table. Reads default to current-truth view (excluding superseded ids); pass
// ?include_superseded=1 or ?as_of=RFC3339 to override.
func (s *Server) handleMemorySupersede(w http.ResponseWriter, r *http.Request, id string, tail []string) {
	// Memory must exist (catches typo'd ids early).
	if _, err := s.bank.GetMemory(id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "not found: "+id, http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	switch r.Method {
	case http.MethodGet:
		if len(tail) > 0 && tail[0] != "" {
			http.Error(w, "GET takes no sub-path", http.StatusBadRequest)
			return
		}
		edges, err := s.bank.SupersedeChainFor(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Split into "supersedes" (id is the superseder) and "superseded_by"
		// (id is the older memory) for the UI's benefit.
		out := struct {
			ID            string          `json:"id"`
			Supersedes    []SupersedeEdge `json:"supersedes"`
			SupersededBy  []SupersedeEdge `json:"superseded_by"`
		}{ID: id}
		for _, e := range edges {
			if e.SupersedingID == id {
				out.Supersedes = append(out.Supersedes, e)
			} else {
				out.SupersededBy = append(out.SupersededBy, e)
			}
		}
		writeJSON(w, http.StatusOK, out)

	case http.MethodPost:
		if len(tail) > 0 && tail[0] != "" {
			http.Error(w, "POST takes no sub-path", http.StatusBadRequest)
			return
		}
		var body struct {
			SupersededID string `json:"superseded_id"`
			ValidFrom    string `json:"valid_from"`
			Reason       string `json:"reason"`
		}
		if err := readJSON(r, &body); err != nil {
			http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
			return
		}
		if body.SupersededID == "" {
			http.Error(w, "superseded_id required", http.StatusBadRequest)
			return
		}
		if err := s.bank.AddSupersede(id, body.SupersededID, body.ValidFrom, body.Reason); err != nil {
			// "not found" / "cannot supersede itself" → 400; bank lookup failures → 500
			msg := err.Error()
			if strings.Contains(msg, "not found") || strings.Contains(msg, "itself") || strings.Contains(msg, "required") {
				http.Error(w, msg, http.StatusBadRequest)
				return
			}
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}
		s.auditWrite(AuditEntry{
			Operation:  "memory_superseded",
			EntityType: "memory",
			EntityID:   body.SupersededID,
			AfterJSON: fmt.Sprintf(`{"superseded_by":%q,"valid_from":%q,"reason":%q}`,
				id, body.ValidFrom, body.Reason),
			Reason:    body.Reason,
			AdapterID: adapterIDFromRequest(r),
		})
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"superseding_id": id,
			"superseded_id":  body.SupersededID,
			"valid_from":     body.ValidFrom,
			"reason":         body.Reason,
			"ok":             true,
		})

	case http.MethodDelete:
		if len(tail) == 0 || tail[0] == "" {
			http.Error(w, "DELETE requires .../supersede/{superseded_id}", http.StatusBadRequest)
			return
		}
		otherID := tail[0]
		if err := s.bank.RemoveSupersede(id, otherID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "edge not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.auditWrite(AuditEntry{
			Operation:  "memory_supersede_removed",
			EntityType: "memory",
			EntityID:   otherID,
			AfterJSON:  fmt.Sprintf(`{"unlinked_from":%q}`, id),
			AdapterID:  adapterIDFromRequest(r),
		})
		w.WriteHeader(http.StatusNoContent)

	default:
		methodNotAllowed(w, "GET", "POST", "DELETE", "OPTIONS")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// /maps and sub-resources
// ──────────────────────────────────────────────────────────────────────────

// mapsRoot handles /maps (no trailing-slash form) — list + create.
func (s *Server) mapsRoot(w http.ResponseWriter, r *http.Request) {
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
		typ := r.URL.Query().Get("type")
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		// [R13 redesign] Optional ?status=proposed|accepted|dismissed filter.
		// Empty (default) returns every status — back-compat with the dashboard's
		// existing Maps tab. New "Proposed Maps" tab will pass status=proposed.
		status := r.URL.Query().Get("status")
		if status != "" && !validMapStatuses[status] {
			http.Error(w, "invalid status filter", http.StatusBadRequest)
			return
		}
		out, err := s.bank.ListMemoryMapsWith(MapListOpts{
			Type: typ, Limit: limit, Status: status,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"count": len(out), "maps": out,
		})
	case http.MethodPost:
		var m MemoryMap
		if err := readJSON(r, &m); err != nil {
			http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
			return
		}
		saved, err := s.bank.SaveMemoryMap(m)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.auditWrite(AuditEntry{
			Operation: "create", EntityType: "memory_map", EntityID: saved.ID,
			AfterJSON: mustJSON(saved), AdapterID: adapterIDFromRequest(r),
		})
		writeJSON(w, http.StatusCreated, saved)
	default:
		methodNotAllowed(w, "GET", "POST", "OPTIONS")
	}
}

// mapsByID handles /maps/{id} and sub-resources.
func (s *Server) mapsByID(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if s.bank == nil {
		http.Error(w, "bank disabled", http.StatusServiceUnavailable)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/maps/")
	if rest == "" {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	parts := strings.Split(rest, "/")
	id := parts[0]
	if id == "" {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}

	// CRITICAL ordering: /maps/overrides is the server-side replacement for
	// the dashboard's localStorage map-overrides blob. Must dispatch BEFORE
	// the {id} interpretation below — otherwise "overrides" gets read as
	// a map id and the GET 404s. (Same route-precedence trap as
	// /admin/env_write and /settings/providers/{tier}/ping.)
	if id == "overrides" {
		s.mapsOverridesRoot(w, r, parts[1:])
		return
	}
	// /maps/merge — bulk tag merge with single bulk.completed WS event.
	// Same precedence rule as /maps/overrides (CODE_HANDOFF — bulk-op
	// WS event suppression + maps merge endpoint, 2026-05-10).
	if id == "merge" {
		s.handleMapsMerge(w, r)
		return
	}
	// /maps/propose — R13 redesign: create a new proposed map from a
	// tag/query/recall_id/auto trigger. Returns the new map with
	// status='proposed' so it doesn't show in the regular Maps tab
	// until the user accepts. Same precedence rule as /maps/overrides.
	if id == "propose" {
		s.handleMapsPropose(w, r)
		return
	}
	// /maps/cleanup — bulk-delete maps matching cleanup filters. Same
	// precedence rule as /maps/overrides.
	if id == "cleanup" {
		s.handleMapsCleanup(w, r)
		return
	}
	// /maps/augmentation/{candidates|run-now} — research-augmentation
	// engine endpoints. Same precedence rule as /maps/overrides.
	if id == "augmentation" {
		if len(parts) < 2 {
			http.Error(w, "missing augmentation sub-path", http.StatusBadRequest)
			return
		}
		switch parts[1] {
		case "candidates":
			s.getAugmentationCandidates(w, r)
			return
		case "run-now":
			s.postAugmentationRunNow(w, r)
			return
		default:
			http.Error(w, "unknown augmentation sub-path", http.StatusNotFound)
			return
		}
	}

	if len(parts) >= 2 {
		switch parts[1] {
		case "associations":
			s.mapAssociations(w, r, id, parts[2:])
			return
		case "traces":
			s.mapTraces(w, r, id, parts[2:])
			return
		case "accept", "dismiss":
			// R13 redesign — flip a proposed map's lifecycle status.
			s.handleMapAcceptDismiss(w, r, id, parts[1])
			return
		case "research":
			// User-triggered Tier 3 research scoped to this map. The Oracle
			// answer is cached AND ingested as a new memory tagged with the
			// map's anchors so the dream pipeline picks it up next nightly
			// run (Phase 0b → Phase 7/8/8.7). See handlers_map_research.go.
			s.handleMapResearch(w, r, id)
			return
		default:
			http.Error(w, "unknown sub-resource", http.StatusNotFound)
			return
		}
	}

	switch r.Method {
	case http.MethodGet:
		m, err := s.bank.GetMemoryMap(id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, m)
	case http.MethodPatch:
		before, err := s.bank.GetMemoryMap(id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var patch MemoryMapUpdate
		if err := readJSON(r, &patch); err != nil {
			http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
			return
		}
		after, err := s.bank.UpdateMemoryMap(id, patch)
		if err != nil {
			// 2026-05-26 — UNIQUE(name, type) collisions on PATCH used to
			// return a 400 with raw SQLite text. The frontend logged it to
			// console and "kept local override" — the user saw success
			// while the backend rejected. Translate the known collision
			// shape into a 409 Conflict with a human-friendly JSON body
			// (the dashboard's toast layer can read .error from JSON 4xx
			// responses) so the row can render an error chip and the
			// user understands why the move failed.
			msg := err.Error()
			if strings.Contains(msg, "UNIQUE constraint failed: memory_maps.name") {
				wantName := before.Name
				if patch.Name != nil {
					wantName = *patch.Name
				}
				wantType := before.Type
				if patch.Type != nil {
					wantType = *patch.Type
				}
				friendly := fmt.Sprintf(
					"A map named %q already exists with type %q. Archive or rename that one first.",
					strings.ToLower(wantName), strings.ToLower(wantType),
				)
				writeJSON(w, http.StatusConflict, map[string]any{
					"error":  friendly,
					"code":   "name_type_collision",
					"detail": msg,
				})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": msg})
			return
		}
		s.auditWrite(AuditEntry{
			Operation: "update", EntityType: "memory_map", EntityID: id,
			BeforeJSON: mustJSON(before), AfterJSON: mustJSON(after),
			Reason: r.URL.Query().Get("reason"), AdapterID: adapterIDFromRequest(r),
		})
		s.emitMapUpdated(id, mapDiffFields(before, after))
		writeJSON(w, http.StatusOK, after)
	case http.MethodDelete:
		before, err := s.bank.GetMemoryMap(id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.bank.DeleteMemoryMap(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.auditWrite(AuditEntry{
			Operation: "delete", EntityType: "memory_map", EntityID: id,
			BeforeJSON: mustJSON(before), Reason: r.URL.Query().Get("reason"),
			AdapterID: adapterIDFromRequest(r),
		})
		s.emitMapUpdated(id, []string{"deleted_at"})
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w, "GET", "PATCH", "DELETE", "OPTIONS")
	}
}

// mapDiffFields lists which top-level fields changed between two map
// snapshots. Used as the `changed_fields` payload of `map.updated` events
// so the dashboard can render a diff hint without re-fetching.
func mapDiffFields(before, after MemoryMap) []string {
	var fields []string
	if before.Name != after.Name {
		fields = append(fields, "name")
	}
	if before.Type != after.Type {
		fields = append(fields, "type")
	}
	if before.SchemaText != after.SchemaText {
		fields = append(fields, "schema_text")
	}
	if before.Source != after.Source {
		fields = append(fields, "source")
	}
	// Anchor tags compared as joined strings — order matters per the user's
	// editing intent.
	if strings.Join(before.AnchorTags, ",") != strings.Join(after.AnchorTags, ",") {
		fields = append(fields, "anchor_tags")
	}
	return fields
}

// mapAssociations handles /maps/{id}/associations[/{to}/{type}]
func (s *Server) mapAssociations(w http.ResponseWriter, r *http.Request, fromID string, tail []string) {
	switch {
	case len(tail) == 0:
		switch r.Method {
		case http.MethodGet:
			direction := r.URL.Query().Get("direction")
			out, err := s.bank.ListAssociations(fromID, direction)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"count": len(out), "associations": out,
			})
		case http.MethodPost:
			var a MapAssociation
			if err := readJSON(r, &a); err != nil {
				http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
				return
			}
			a.FromMapID = fromID
			saved, err := s.bank.AddAssociation(a)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			s.auditWrite(AuditEntry{
				Operation: "create", EntityType: "association", EntityID: saved.ID,
				AfterJSON: mustJSON(saved), AdapterID: adapterIDFromRequest(r),
			})
			s.emitMapUpdated(fromID, []string{"associations"})
			s.emitMapUpdated(saved.ToMapID, []string{"associations"})
			writeJSON(w, http.StatusCreated, saved)
		default:
			methodNotAllowed(w, "GET", "POST", "OPTIONS")
		}
	case len(tail) == 2:
		toID, assoc := tail[0], tail[1]
		if r.Method != http.MethodDelete {
			methodNotAllowed(w, "DELETE", "OPTIONS")
			return
		}
		if err := s.bank.RemoveAssociation(fromID, toID, assoc); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.auditWrite(AuditEntry{
			Operation: "delete", EntityType: "association",
			EntityID: fromID + "→" + toID + ":" + assoc,
			Reason:   r.URL.Query().Get("reason"), AdapterID: adapterIDFromRequest(r),
		})
		s.emitMapUpdated(fromID, []string{"associations"})
		s.emitMapUpdated(toID, []string{"associations"})
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "bad path", http.StatusBadRequest)
	}
}

// mapTraces handles /maps/{id}/traces[/{trace_id}]
func (s *Server) mapTraces(w http.ResponseWriter, r *http.Request, mapID string, tail []string) {
	switch {
	case len(tail) == 0:
		switch r.Method {
		case http.MethodGet:
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			ids, err := s.bank.ListTracesForMap(mapID, limit)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"count": len(ids), "trace_ids": ids,
			})
		case http.MethodPost:
			var body struct {
				TraceID string `json:"trace_id"`
			}
			if err := readJSON(r, &body); err != nil {
				http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
				return
			}
			if body.TraceID == "" {
				http.Error(w, "trace_id is required", http.StatusBadRequest)
				return
			}
			if err := s.bank.LinkMapTrace(mapID, body.TraceID); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			s.auditWrite(AuditEntry{
				Operation: "link", EntityType: "map_trace",
				EntityID: mapID + "↔" + body.TraceID, AdapterID: adapterIDFromRequest(r),
			})
			s.emitMapUpdated(mapID, []string{"traces"})
			w.WriteHeader(http.StatusCreated)
		default:
			methodNotAllowed(w, "GET", "POST", "OPTIONS")
		}
	case len(tail) == 1:
		traceID := tail[0]
		if r.Method != http.MethodDelete {
			methodNotAllowed(w, "DELETE", "OPTIONS")
			return
		}
		if err := s.bank.UnlinkMapTrace(mapID, traceID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.auditWrite(AuditEntry{
			Operation: "unlink", EntityType: "map_trace",
			EntityID: mapID + "↔" + traceID, AdapterID: adapterIDFromRequest(r),
		})
		s.emitMapUpdated(mapID, []string{"traces"})
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "bad path", http.StatusBadRequest)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// /lexicon, /audit, /research, /budget
// ──────────────────────────────────────────────────────────────────────────

// lexiconPairs serves /lexicon/pairs — the dedicated sub-resource for the
// dashboard's Pairs sub-tab. Returns `{count, pairs: [{tag_a, tag_b,
// cooccurrence, weight, updated_at}]}`. Shipped per
// `CODE_HANDOFF — Lexicon Pairs endpoint sync (2026-05-10).md` so the
// frontend can drop its fallback resolver.
//
// Each lexicon row IS a pair by construction, so the underlying query is
// identical to /lexicon — the response key differs to match the
// frontend's expectation. The `tag` query param scopes to pairs involving
// that tag (preserves the existing /lexicon filter semantics).
func (s *Server) lexiconPairs(w http.ResponseWriter, r *http.Request) {
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
	tag := r.URL.Query().Get("tag")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	out, err := s.bank.ListLexicon(tag, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"count": len(out), "pairs": out,
	})
}

func (s *Server) lexiconRoot(w http.ResponseWriter, r *http.Request) {
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
		tag := r.URL.Query().Get("tag")
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		out, err := s.bank.ListLexicon(tag, limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"count": len(out), "lexicon": out,
		})
	case http.MethodPost:
		var row LexiconRow
		if err := readJSON(r, &row); err != nil {
			http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.bank.UpsertLexicon(row); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.auditWrite(AuditEntry{
			Operation: "upsert", EntityType: "lexicon",
			EntityID:  row.TagA + "↔" + row.TagB,
			AfterJSON: mustJSON(row), AdapterID: adapterIDFromRequest(r),
		})
		writeJSON(w, http.StatusOK, row)
	case http.MethodDelete:
		var body struct {
			TagA string `json:"tag_a"`
			TagB string `json:"tag_b"`
		}
		if err := readJSON(r, &body); err != nil {
			http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.bank.DeleteLexicon(body.TagA, body.TagB); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.auditWrite(AuditEntry{
			Operation: "delete", EntityType: "lexicon",
			EntityID: body.TagA + "↔" + body.TagB, AdapterID: adapterIDFromRequest(r),
		})
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w, "GET", "POST", "DELETE", "OPTIONS")
	}
}

func (s *Server) auditRoot(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if s.bank == nil {
		http.Error(w, "bank disabled", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET", "OPTIONS")
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	// Wave 7c2 — accept `since_cursor` as the canonical cursor param;
	// `since` stays as a legacy alias.
	out, err := s.bank.ListAuditLog(AuditFilter{
		EntityType: q.Get("entity_type"),
		EntityID:   q.Get("entity_id"),
		Operation:  q.Get("operation"),
		Since:      pickSinceCursor(r),
		Limit:      limit,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp := map[string]interface{}{
		"count": len(out), "entries": out,
	}
	// Wave 7c2 — surface `next_cursor` + `section_clean_at` when the
	// lazy-fetch flag is enabled. Entries are sorted newest-first, so
	// the most-recent entry's created_at becomes the cursor for the
	// next "give me everything newer" call.
	if lazyFetchEnabled() {
		nextCursor := ""
		if len(out) > 0 {
			nextCursor = out[0].CreatedAt
		}
		resp["next_cursor"] = nextCursor
		resp["section_clean_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) researchRoot(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if s.bank == nil {
		http.Error(w, "bank disabled", http.StatusServiceUnavailable)
		return
	}

	// Path can be /research or /research/{topic}
	rest := strings.TrimPrefix(r.URL.Path, "/research")
	rest = strings.TrimPrefix(rest, "/")
	if rest != "" {
		s.researchByTopic(w, r, rest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		includeExpired := r.URL.Query().Get("include_expired") == "true"
		out, err := s.bank.ListResearchCache(limit, includeExpired)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"count": len(out), "entries": out,
		})
	case http.MethodPost:
		// User-triggered research: send a query through the Tier 3 chokepoint,
		// cache the result, charge the budget. Implementation lives in
		// handlers_research.go to keep this dispatcher tight.
		s.handleResearchPost(w, r)
	default:
		methodNotAllowed(w, "GET", "POST", "OPTIONS")
	}
}

func (s *Server) researchByTopic(w http.ResponseWriter, r *http.Request, topic string) {
	switch r.Method {
	case http.MethodGet:
		e, err := s.bank.GetResearchCache(topic)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Return the rich detail shape per the handoff: deserialise the
		// payload JSON (which holds the full Oracle answer + sources +
		// tokens + model from the time of POST /research) and merge in
		// the row-level metadata. The dashboard's detail view needs this
		// because the list endpoint returns a truncated form.
		out := map[string]interface{}{
			"topic":      e.Topic,
			"topic_id":   e.Topic, // stable id; same string serves both fields
			"map_id":     e.MapID,
			"fetched_at": e.FetchedAt,
			"expires_at": e.ExpiresAt,
			"fetched_by": e.FetchedBy,
		}
		// Parse the payload JSON to surface query/content/sources/tokens.
		// Failures are tolerated — a row with a malformed payload still
		// returns the row metadata, just without the rich fields.
		if e.Payload != "" {
			var rich map[string]interface{}
			if err := json.Unmarshal([]byte(e.Payload), &rich); err == nil {
				if v, ok := rich["query"]; ok {
					out["query"] = v
				}
				if v, ok := rich["content"]; ok {
					out["content"] = v
				}
				if v, ok := rich["sources"]; ok {
					out["sources"] = v
				}
				if v, ok := rich["model"]; ok {
					out["model"] = v
					// Derive provider from the Name() prefix convention.
					if name, ok := v.(string); ok {
						if i := strings.Index(name, ":"); i > 0 {
							out["provider"] = name[:i]
						}
					}
				}
				for _, k := range []string{"tokens_in", "tokens_out", "tokens_total"} {
					if v, ok := rich[k]; ok {
						out[k] = v
					}
				}
				// Always include the raw payload too, so the dashboard's
				// existing renderers that read entry.payload still work.
				out["payload"] = rich
			}
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodDelete:
		if err := s.bank.DeleteResearchCache(topic); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.auditWrite(AuditEntry{
			Operation: "delete", EntityType: "research_cache", EntityID: topic,
			AdapterID: adapterIDFromRequest(r),
		})
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w, "GET", "DELETE", "OPTIONS")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// /settings — generic key/value + typed nightly_schedule
// ──────────────────────────────────────────────────────────────────────────

func (s *Server) settingsRoot(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if s.bank == nil {
		http.Error(w, "bank disabled", http.StatusServiceUnavailable)
		return
	}

	// Path forms:
	//   /settings                            (collection)
	//   /settings/{key}                      (single — generic value)
	//   /settings/nightly_schedule           (single — typed accessor)
	//   /settings/providers                  (per-tier provider list)
	//   /settings/providers/{tier}           (single tier config — typed)
	rest := strings.TrimPrefix(r.URL.Path, "/settings")
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		s.settingsCollection(w, r)
		return
	}
	// Wave 6 — GET /settings/all returns every setting grouped by namespace
	// + the typed bundles in one round-trip. Per CODE_HANDOFF — server-side
	// prefs + lazy tab fetch (2026-05-10). Routed BEFORE settingsByKey so
	// the catchall doesn't try to look up a setting literally named "all".
	if rest == "all" {
		s.settingsAll(w, r)
		return
	}
	if rest == NightlyScheduleKey {
		s.settingsNightlySchedule(w, r)
		return
	}
	if rest == "augment" {
		s.settingsAugment(w, r)
		return
	}
	if rest == "dream_pipeline" {
		s.settingsDreamPipeline(w, r)
		return
	}
	if rest == "providers" {
		s.settingsProvidersAll(w, r)
		return
	}
	// Wave 7b2 — /settings/services/{name}/lifecycle. Routed BEFORE the
	// generic per-key catchall so the structured path doesn't fall
	// through and get looked up as a literal setting key.
	if strings.HasPrefix(rest, "services/") {
		s.settingsServicesRouter(w, r, rest)
		return
	}
	if strings.HasPrefix(rest, "providers/") {
		// CRITICAL ordering: match the /ping sub-resource BEFORE the bare
		// /providers/{tier} branch, otherwise `/providers/tier1/ping` would
		// be parsed as tier="tier1/ping" and fall to the generic settings
		// catchall with a 405. (Same trap as /admin/env_write — see handoff
		// 2026-05-09.)
		sub := strings.TrimPrefix(rest, "providers/")
		parts := strings.Split(sub, "/")
		if len(parts) == 2 && parts[1] == "ping" {
			s.settingsProvidersPing(w, r, parts[0])
			return
		}
		if len(parts) == 1 && parts[0] != "" {
			s.settingsProvidersByTier(w, r, parts[0])
			return
		}
		http.Error(w, "unknown provider sub-path", http.StatusNotFound)
		return
	}
	s.settingsByKey(w, r, rest)
}

// settingsProvidersAll handles GET /settings/providers — returns all four
// tier configs (embedding + tier1 + tier2 + tier3) with API keys redacted.
func (s *Server) settingsProvidersAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET", "OPTIONS")
		return
	}
	out := map[string]ProviderConfig{}
	for _, tier := range AllTierKeys {
		cfg, err := s.bank.GetProviderConfig(tier)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out[string(tier)] = cfg.SafeForResponse()
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"providers": out,
		// Hint to the UI: which API key env vars are currently populated?
		// Lets the onboarding wizard say "we see SD_TIER1_API_KEY is set"
		// without ever revealing the value.
		"api_key_env_set": apiKeyEnvSetMap(out),
	})
}

// settingsProvidersByTier handles GET / PUT for a single tier.
func (s *Server) settingsProvidersByTier(w http.ResponseWriter, r *http.Request, tierStr string) {
	tier, ok := parseTierKey(tierStr)
	if !ok {
		http.Error(w, "unknown tier; expected one of: "+strings.Join(allTierStrings(), ", "), http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		cfg, err := s.bank.GetProviderConfig(tier)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"tier":            string(tier),
			"config":          cfg.SafeForResponse(),
			"api_key_env_set": cfg.APIKeyEnv != "" && os.Getenv(cfg.APIKeyEnv) != "",
		})
	case http.MethodPut:
		var cfg ProviderConfig
		if err := readJSON(r, &cfg); err != nil {
			http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.bank.SaveProviderConfig(tier, cfg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Hot-swap: rebuild this tier's provider from the freshly-saved DB
		// config and atomically replace it on the live router. In-flight
		// Chat() calls hold their own reference to the old provider and
		// finish normally; the NEXT lookup via ForNightly()/ForOracle()
		// sees the new provider. Eliminates the prior "restart core to
		// take effect" requirement that surprised users on every Tier
		// change. Old provider returned so we can log + Close() if we
		// ever add Close() to the LLMProvider interface.
		hotSwapped := false
		var hotSwapName string
		var reencodeFlagged int64
		if s.router != nil {
			newProvider := buildOneTierFromSettingsOrEnv(s.bank, tier)
			old, ok := s.router.ReplaceTier(tier, newProvider)
			if ok {
				hotSwapped = true
				oldName := "<none>"
				if old != nil {
					oldName = old.Name()
				}
				newName := "<none>"
				if newProvider != nil {
					newName = newProvider.Name()
					hotSwapName = newName
				}
				log.Printf("router: %s hot-swapped %s → %s", tier, oldName, newName)

				// Phase C — optional re-process on Tier 2 model change.
				// When the user has flipped `deep_enrich_reencode_on_model_change`
				// on AND the Tier 2 provider name actually changed, bulk-mark
				// every previously-deep-encoded memory dirty with reason=
				// "model_upgrade" except rows whose `deep_encoded_model`
				// already matches the new provider. Phase 0b's existing
				// dirty queue picks them up on the next nightly run.
				//
				// Only fires for Tier 2 (the nightly enrichment tier);
				// Tier 1 swaps don't invalidate Phase 0b output, Tier 3 is
				// the Oracle path and isn't tied to memory encoding.
				if tier == Tier2 && newProvider != nil && oldName != newName {
					reencodeOn := false
					if v, ok, _ := s.bank.GetSetting("deep_enrich_reencode_on_model_change"); ok {
						reencodeOn = v == "true" || v == "1" || v == "on"
					}
					if reencodeOn {
						if n, err := s.bank.MarkAllDeepEncodedDirty("model_upgrade", newName); err != nil {
							log.Printf("router: re-encode flag failed: %v", err)
						} else {
							reencodeFlagged = n
							log.Printf("router: %d memories flagged for re-encode under new Tier 2 model %s", n, newName)
						}
					}
				}
			}
		}
		auditReason := "provider config updated; restart core to take effect"
		if hotSwapped {
			auditReason = "provider config updated; hot-swapped on live router (no restart needed)"
		}
		s.auditWrite(AuditEntry{
			Operation:  "upsert",
			EntityType: "setting",
			EntityID:   string(tier),
			AfterJSON:  mustJSON(cfg.SafeForResponse()),
			Reason:     auditReason,
			AdapterID:  adapterIDFromRequest(r),
		})
		note := "Restart sd-core for the new provider to take effect."
		if hotSwapped {
			note = "Provider hot-swapped on the live router — no restart needed."
		}
		resp := map[string]interface{}{
			"tier":        string(tier),
			"config":      cfg.SafeForResponse(),
			"note":        note,
			"hot_swapped": hotSwapped,
		}
		if hotSwapName != "" {
			resp["active_provider"] = hotSwapName
		}
		if reencodeFlagged > 0 {
			resp["reencode_flagged"] = reencodeFlagged
		}
		writeJSON(w, http.StatusOK, resp)
	default:
		methodNotAllowed(w, "GET", "PUT", "OPTIONS")
	}
}

// parseTierKey converts a path component to a TierKey, accepting either the
// canonical settings key ("tier1_provider") or the short form ("tier1").
func parseTierKey(s string) (TierKey, bool) {
	for _, t := range AllTierKeys {
		if string(t) == s {
			return t, true
		}
	}
	switch s {
	case "embedding":
		return TierEmbedding, true
	case "tier1":
		return Tier1, true
	case "tier2":
		return Tier2, true
	case "tier3":
		return Tier3, true
	}
	return "", false
}

func allTierStrings() []string {
	out := make([]string, 0, len(AllTierKeys)*2)
	for _, t := range AllTierKeys {
		out = append(out, string(t))
	}
	out = append(out, "embedding", "tier1", "tier2", "tier3")
	return out
}

// apiKeyEnvSetMap reports which configured api_key_env vars actually have
// a value set in the running process. UI uses this for the "your key is/isn't
// detected" affordance during onboarding.
func apiKeyEnvSetMap(configs map[string]ProviderConfig) map[string]bool {
	out := map[string]bool{}
	for tier, cfg := range configs {
		out[tier] = cfg.APIKeyEnv != "" && os.Getenv(cfg.APIKeyEnv) != ""
	}
	return out
}

func (s *Server) settingsCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET", "OPTIONS")
		return
	}
	out, err := s.bank.ListSettings()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"count": len(out), "settings": out,
	})
}

// settingsAll serves GET /settings/all per CODE_HANDOFF — server-side
// prefs + lazy tab fetch (2026-05-10). Single round-trip that returns:
//
//   - raw: every settings row grouped by dot-namespace (ui.theme lands as
//     {"ui": {"theme": "..."}}); keys without a dot land under "_root".
//   - dream_pipeline / augment: typed-bundle accessors (same shape as the
//     dedicated /settings/dream_pipeline + /settings/augment endpoints) so
//     callers don't follow up with N per-namespace fetches.
//   - providers: per-tier ProviderConfig, redacted via SafeForResponse.
//
// Values in "raw" are returned as strings (the storage type). Frontends
// that know the expected type for a key — booleans, JSON-encoded values —
// parse them client-side. This keeps the endpoint provenance-free.
func (s *Server) settingsAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET", "OPTIONS")
		return
	}
	rows, err := s.bank.ListSettings()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	raw := map[string]map[string]string{}
	for _, row := range rows {
		ns := "_root"
		key := row.Key
		if i := strings.Index(row.Key, "."); i > 0 {
			ns = row.Key[:i]
			key = row.Key[i+1:]
		}
		if _, ok := raw[ns]; !ok {
			raw[ns] = map[string]string{}
		}
		raw[ns][key] = row.Value
	}

	// Typed bundles. Tolerate per-bundle errors — partial response is still
	// useful for boot-time hydration.
	dp, _ := s.bank.GetDreamPipelineSettings()
	aug, _ := s.bank.GetAugmentSettings()
	providers := map[string]ProviderConfig{}
	for _, tier := range AllTierKeys {
		if cfg, err := s.bank.GetProviderConfig(tier); err == nil {
			providers[string(tier)] = cfg.SafeForResponse()
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"raw":             raw,
		"dream_pipeline":  dp,
		"augment":         aug,
		"providers":       providers,
		"api_key_env_set": apiKeyEnvSetMap(providers),
	})
}

func (s *Server) settingsByKey(w http.ResponseWriter, r *http.Request, key string) {
	switch r.Method {
	case http.MethodGet:
		v, ok, err := s.bank.GetSetting(key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, SettingRow{Key: key, Value: v})
	case http.MethodPut:
		var body struct {
			Value string `json:"value"`
		}
		if err := readJSON(r, &body); err != nil {
			http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.bank.SetSetting(key, body.Value); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Hot-swap hooks for settings that other subsystems cache. The user
		// expects changes here to take effect without a Core restart — same
		// invariant as provider hot-swap (PUT /settings/providers/{tier}).
		switch key {
		case SensitiveClassifierPromptKey:
			invalidateSensitivePromptCache()
		}
		s.auditWrite(AuditEntry{
			Operation: "upsert", EntityType: "setting", EntityID: key,
			AfterJSON: mustJSON(body), AdapterID: adapterIDFromRequest(r),
		})
		writeJSON(w, http.StatusOK, SettingRow{Key: key, Value: body.Value})
	case http.MethodDelete:
		if err := s.bank.DeleteSetting(key); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Same hot-swap hooks fire on delete (reset-to-default).
		switch key {
		case SensitiveClassifierPromptKey:
			invalidateSensitivePromptCache()
		}
		s.auditWrite(AuditEntry{
			Operation: "delete", EntityType: "setting", EntityID: key,
			AdapterID: adapterIDFromRequest(r),
		})
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w, "GET", "PUT", "DELETE", "OPTIONS")
	}
}

// settingsNightlySchedule provides a typed view over the nightly_schedule
// settings entry. UI should prefer this over /settings/{nightly_schedule}
// because GETs return DefaultNightlySchedule when nothing is stored (so the
// onboarding prompt can read configured=false), and PUTs validate the shape
// (rejecting bad HH:MM, unknown timezones, etc.) before persisting.
func (s *Server) settingsNightlySchedule(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		sched, err := s.bank.GetNightlySchedule()
		if err != nil {
			// Surface but still return defaults — the UI must always get a
			// usable shape.
			log := r.URL.Query().Get("debug") == "1"
			if log {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		writeJSON(w, http.StatusOK, sched)
	case http.MethodPut:
		var sched NightlySchedule
		if err := readJSON(r, &sched); err != nil {
			http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
			return
		}
		// PUT is the answer-the-onboarding-prompt path — mark configured so
		// the UI doesn't keep prompting on every load.
		sched.Configured = true
		if err := s.bank.SaveNightlySchedule(sched); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.auditWrite(AuditEntry{
			Operation: "upsert", EntityType: "setting", EntityID: NightlyScheduleKey,
			AfterJSON: mustJSON(sched), AdapterID: adapterIDFromRequest(r),
		})
		writeJSON(w, http.StatusOK, sched)
	default:
		methodNotAllowed(w, "GET", "PUT", "OPTIONS")
	}
}

// filterHistoryToExternal rewrites the legacy history rows (which sum ALL
// providers' spend per day) so each day's tokens_used reflects only
// external=1 lines. Used when ?external_only=1 is set so the dashboard's
// daily-bar chart aligns with the chip's headline.
//
// Returns a fresh slice (does NOT mutate the input). Days with zero
// external spend are KEPT (zeroed) rather than removed so the chart's
// x-axis stays continuous.
func filterHistoryToExternal(bank *Bank, rows []TokenBudgetRow, days int) []TokenBudgetRow {
	if len(rows) == 0 {
		return rows
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days+1).Format("2006-01-02")
	// Map date → sum of external in/out from token_budget_lines.
	q, err := bank.db.Query(
		`SELECT date, COALESCE(SUM(tokens_in + tokens_out), 0)
		 FROM token_budget_lines WHERE date >= ? AND external = 1
		 GROUP BY date`, cutoff,
	)
	if err != nil {
		return rows // best-effort: fall back to unfiltered on read failure
	}
	defer q.Close()
	external := map[string]int{}
	for q.Next() {
		var d string
		var n int
		if err := q.Scan(&d, &n); err == nil {
			external[d] = n
		}
	}
	out := make([]TokenBudgetRow, len(rows))
	for i, r := range rows {
		out[i] = r
		out[i].TokensUsed = external[r.Date] // zero if no external spend that day
	}
	return out
}

func (s *Server) budgetRoot(w http.ResponseWriter, r *http.Request) {
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
		q := r.URL.Query()
		date := q.Get("date")
		days, _ := strconv.Atoi(q.Get("days"))
		externalOnly := q.Get("external_only") == "1"
		if days > 0 {
			rows, err := s.bank.ListTokenBudget(days)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			byTier, err := s.bank.ListTokenBudgetByTier(days, externalOnly)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			extIn, extOut, extTotal, err := s.bank.GetExternalTokenTotal(days)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			// Always emit the four canonical tiers so the dashboard's
			// per-tier rows render uniformly even when a tier has zero spend.
			for _, k := range []TierKey{TierEmbedding, Tier1, Tier2, Tier3} {
				if _, ok := byTier[string(k)]; !ok {
					cfg, _ := s.bank.GetProviderConfig(k)
					provider := string(cfg.Kind)
					if provider == "" {
						provider = "disabled"
					}
					byTier[string(k)] = TokenSpendByTier{
						Provider: provider,
						External: providerIsExternal(provider),
					}
				}
			}
			// When external_only is set, also filter the legacy history rows
			// so the daily-bar chart matches the rest of the response.
			displayHistory := rows
			if externalOnly {
				displayHistory = filterHistoryToExternal(s.bank, rows, days)
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"count":   len(displayHistory),
				"history": displayHistory,
				"external_total": map[string]interface{}{
					"tokens_in":    extIn,
					"tokens_out":   extOut,
					"tokens_total": extTotal,
				},
				"by_tier": byTier,
			})
			return
		}
		used, err := s.bank.GetTokensUsed(date)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"date": date, "tokens_used": used,
		})
	case http.MethodPost:
		var body struct {
			Date   string `json:"date"`
			Tokens int    `json:"tokens"`
		}
		if err := readJSON(r, &body); err != nil {
			http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.bank.AddTokensUsed(body.Date, body.Tokens); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.auditWrite(AuditEntry{
			Operation: "add_tokens", EntityType: "token_budget", EntityID: body.Date,
			AfterJSON: mustJSON(body), AdapterID: adapterIDFromRequest(r),
		})
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w, "GET", "POST", "OPTIONS")
	}
}
