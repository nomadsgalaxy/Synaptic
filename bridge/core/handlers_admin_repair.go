// handlers_admin_repair.go — POST /admin/repair/orphaned-merges
//
// One-off data repair for the resurrection-orphan bug. A bulk restore
// (POST /bank/memories/{id}/restore) cleared deleted_at on ~2,601 dedup
// losers but left their deleted_reason="merged into <id>" intact, so 52%
// of the bank ended up live-but-merged: rows that are recallable yet still
// carry a stale "I was merged away" marker.
//
// The underlying code bug is fixed in UndeleteMemory (it now clears
// deleted_reason atomically with deleted_at). This endpoint cleans up the
// rows that were already orphaned before that fix landed.
//
// Contract (mirrors /admin/sensitive/reclassify):
//   - dry_run defaults to TRUE. A dry run only COUNTS and breaks down the
//     orphans; it writes nothing.
//   - dry_run=false performs ONE bulk UPDATE clearing deleted_reason on
//     every live orphan, writes a single summary audit row, and returns
//     rows-affected.
//   - Admin auth is enforced by the global authMiddleware on the /admin/
//     prefix; no per-handler token check is needed.
//
// dry_run can be supplied either in the JSON body ({"dry_run":false}) or
// as a query param (?dry_run=false). An empty/absent body is treated as a
// dry run — the safe default — so a bare POST never mutates data.
package main

import (
	"fmt"
	"net/http"
	"time"
)

type repairOrphanedMergesResponse struct {
	DryRun bool `json:"dry_run"`
	// Orphans is the number of live rows (deleted_at='') carrying a
	// non-empty deleted_reason — the rows this repair targets.
	Orphans int `json:"orphans"`
	// MergedInto is how many of the orphans have a "merged into ..." reason
	// (the dedup-loser resurrection class). AlsoMergeSurvivor is how many of
	// THOSE also carry a non-empty merged_from array (chains — rows that won
	// an earlier dedup then lost a later one).
	MergedInto       int `json:"merged_into"`
	AlsoMergeSurvivor int `json:"also_merge_survivor"`
	// OtherReason counts orphans whose reason is set but does NOT start with
	// "merged into" (e.g. a stale "ttl" reason from a restored TTL expiry).
	OtherReason int `json:"other_reason"`
	// Cleared is the rows-affected count from the UPDATE (0 on dry runs).
	Cleared      int    `json:"cleared"`
	AuditRunID   string `json:"audit_run_id,omitempty"`
	DurationMs   int64  `json:"duration_ms"`
	// MergedFromCleanupSkipped flags that the secondary survivor merged_from
	// cleanup was intentionally not performed (see note below).
	MergedFromCleanupSkipped bool `json:"merged_from_cleanup_skipped"`
}

func (s *Server) handleAdminRepairOrphanedMerges(w http.ResponseWriter, r *http.Request) {
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

	// dry_run defaults to TRUE. Body wins if present; query param is a
	// fallback. An empty/unparseable body leaves the default in place so a
	// bare POST is always a dry run.
	var body struct {
		DryRun *bool `json:"dry_run"`
	}
	_ = readJSON(r, &body) // ignore parse errors — empty body is fine
	dryRun := true
	if body.DryRun != nil {
		dryRun = *body.DryRun
	} else if q := r.URL.Query().Get("dry_run"); q != "" {
		dryRun = !(q == "false" || q == "0" || q == "off")
	}

	startedAt := time.Now()
	resp := repairOrphanedMergesResponse{
		DryRun:                   dryRun,
		MergedFromCleanupSkipped: true,
	}

	// Count + breakdown. The predicate for an orphan is the exact inverse of
	// the invariant UndeleteMemory now enforces: a LIVE row (deleted_at='')
	// must not carry a deleted_reason.
	if err := s.bank.db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE deleted_at = '' AND deleted_reason <> ''`,
	).Scan(&resp.Orphans); err != nil {
		http.Error(w, "count orphans: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_ = s.bank.db.QueryRow(
		`SELECT COUNT(*) FROM memories
		 WHERE deleted_at = '' AND deleted_reason LIKE 'merged into %'`,
	).Scan(&resp.MergedInto)
	_ = s.bank.db.QueryRow(
		`SELECT COUNT(*) FROM memories
		 WHERE deleted_at = '' AND deleted_reason LIKE 'merged into %'
		   AND merged_from <> '' AND merged_from <> '[]'`,
	).Scan(&resp.AlsoMergeSurvivor)
	resp.OtherReason = resp.Orphans - resp.MergedInto

	if dryRun {
		resp.DurationMs = time.Since(startedAt).Milliseconds()
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// Execute: clear deleted_reason on every live orphan in one statement.
	// updated_at is bumped so downstream consumers (and the dashboard) see
	// the rows as touched by this repair.
	now := nowUTC()
	s.bank.wmu.Lock()
	res, err := s.bank.db.Exec(
		`UPDATE memories SET deleted_reason = '', updated_at = ?
		 WHERE deleted_at = '' AND deleted_reason <> ''`,
		now,
	)
	s.bank.wmu.Unlock()
	if err != nil {
		http.Error(w, "repair update: "+err.Error(), http.StatusInternalServerError)
		return
	}
	affected, _ := res.RowsAffected()
	resp.Cleared = int(affected)

	// Single summary audit row.
	resp.AuditRunID = fmt.Sprintf("repair-%d", nextMonotonicNano())
	_ = s.bank.AppendAudit(AuditEntry{
		Operation:  "repair_orphaned_merges",
		EntityType: "memories",
		EntityID:   resp.AuditRunID,
		AfterJSON: fmt.Sprintf(
			`{"cleared":%d,"orphans":%d,"merged_into":%d,"also_merge_survivor":%d,"other_reason":%d}`,
			resp.Cleared, resp.Orphans, resp.MergedInto, resp.AlsoMergeSurvivor, resp.OtherReason,
		),
		Reason:    "cleared stale deleted_reason on live rows orphaned by bulk restore",
		AdapterID: adapterIDFromRequest(r),
	})

	// Survivor merged_from cleanup is INTENTIONALLY skipped here. Deciding
	// which merged_from entries are stale requires cross-referencing each id
	// against rows that were actually hard-purged vs. still present, plus
	// JSON array surgery per survivor — fiddly and easy to get wrong on a
	// 52%-affected bank. The stale merged_from arrays are harmless (they're
	// provenance metadata, not a recall/visibility gate), so we leave them.
	// MergedFromCleanupSkipped stays true to advertise that.

	resp.DurationMs = time.Since(startedAt).Milliseconds()
	writeJSON(w, http.StatusOK, resp)
}
