// bank_p5.go — Phase 5 Bank methods.
//
// Schema for the tables touched here lives in bank.go's bankSchema const:
//   memory_maps, map_associations, map_traces, lexicon, audit_log,
//   research_cache, token_budget — plus the lifecycle/soft-delete columns
//   migrated onto the existing `memories` table.
//
// Conventions:
//   - All write methods take the bank's wmu mutex (single-writer SQLite policy).
//   - Public read methods do not lock (modernc.org/sqlite handles concurrent reads).
//   - "Save" upserts; "Update" partial-patches; "Delete" is hard-delete by default;
//     soft-delete on memories uses dedicated SoftDeleteMemory/UndeleteMemory pair.
//   - All write methods that mutate user-visible state should be paired with
//     an audit_log entry by the caller (typically the HTTP handler) — these
//     bank methods do NOT auto-audit so callers can choose granularity.
package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// ListContextMemoriesByRegion returns the most-recent context memory
// per region — keyed by context_region. Used by Phase 8.7 to decide
// whether a region's existing context memory is fresh enough to skip
// (within ContextRefreshDays) or stale enough to refresh.
// [R13] CITATIONS.md #18 (Johnson 2005).
func (b *Bank) ListContextMemoriesByRegion() (map[string]MemoryRecord, error) {
	if b == nil {
		return nil, errors.New("bank not enabled")
	}
	rows, err := b.db.Query(`SELECT ` + memoryColumns + `
		FROM memories
		WHERE memory_type = 'context' AND deleted_at = ''
		ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]MemoryRecord{}
	for rows.Next() {
		rec, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		// First (most-recent) wins per region; later rows are older.
		if _, seen := out[rec.ContextRegion]; !seen && rec.ContextRegion != "" {
			out[rec.ContextRegion] = rec
		}
	}
	return out, rows.Err()
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// idMu guards idLastNano, the monotonic source for all generated IDs.
var (
	idMu       sync.Mutex
	idLastNano int64
)

// nextMonotonicNano returns a strictly-increasing UnixNano value. On hosts
// with a coarse wall clock (notably Windows, where time.Now() can advance in
// ~100ns–15ms ticks) two calls in a tight loop would otherwise return the same
// nanosecond, so newID/inline ID builders produced DUPLICATE ids and collided
// on primary keys — silently dropping rows on bulk insert (import, map merge,
// nightly synthesis) and panicking/failing the bundle test suite. Forcing
// strict monotonicity makes every id unique regardless of clock granularity
// while preserving time-sortability and the exact `prefix-<int>` id format
// (no parsing breakage — audit confirmed no code splits an id on its numeric
// suffix and no SQL ORDER BY id relies on the timestamp).
func nextMonotonicNano() int64 {
	idMu.Lock()
	n := time.Now().UnixNano()
	if n <= idLastNano {
		n = idLastNano + 1
	}
	idLastNano = n
	idMu.Unlock()
	return n
}

func newID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, nextMonotonicNano())
}

// ──────────────────────────────────────────────────────────────────────────
// Memories: edit / soft-delete / lifecycle
// ──────────────────────────────────────────────────────────────────────────

// MemoryUpdate is a partial-patch payload. Nil fields mean "leave unchanged".
type MemoryUpdate struct {
	Text         *string   `json:"text,omitempty"`          // raw text — discouraged; prefer EnrichedText
	EnrichedText *string   `json:"enriched_text,omitempty"` // refined text from Tier 1 / sd_correct
	Tags         *[]string `json:"tags,omitempty"`
	RegionHint   *string   `json:"region_hint,omitempty"`
	Source       *string   `json:"source,omitempty"`
	MergedFrom   *[]string `json:"merged_from,omitempty"`
	// Sensitive is the only path that can EXPLICITLY clear the flag (passing
	// &false). Tag-driven auto-flags (in SaveMemory / UpdateMemory) can only
	// raise it. If the user PATCHes new tags that include a sensitive trigger,
	// Sensitive is auto-promoted to true regardless of what was passed in this
	// field, unless caller explicitly sets Sensitive=&false AND the resulting
	// tags do not contain a sensitive trigger.
	Sensitive *bool `json:"sensitive,omitempty"`
	// v2.7 Bundle L — Dormant Memories.
	//
	// ExpiresAt is the optional TTL trigger. ISO-8601 timestamp. When the
	// nightly dormancy pass sees expires_at <= now AND dormant_at == '',
	// it puts the memory to sleep (writes dormant_at + dormant_reason="ttl").
	// The memory stays in the bank — it's excluded from default /recall
	// but resurrectable via include_dormant=true or by PATCHing
	// dormant=&false. Pass `&""` to clear an existing TTL. Sensitive
	// memories can't have a TTL set (UpdateMemory returns an error) —
	// sticky-on policy.
	ExpiresAt *string `json:"expires_at,omitempty"`
	// Dormant is the manual put-to-sleep / wake-up toggle. Setting
	// &true marks the memory dormant (dormant_reason="manual"); &false
	// wakes a dormant memory back up (clears dormant_at). No effect if
	// the value is already the requested state.
	Dormant *bool `json:"dormant,omitempty"`
}

// UpdateMemory applies a partial patch and bumps updated_at. Returns the
// post-update record.
//
// Sensitive resolution rules:
//   - If patch.Tags introduces a sensitive trigger tag, sensitive is auto-set
//     to true regardless of patch.Sensitive.
//   - patch.Sensitive=&true forces sensitive=true unconditionally.
//   - patch.Sensitive=&false ONLY clears the flag if the resulting tag set
//     contains no sensitive triggers.
//   - If patch.Sensitive is nil, the existing value is preserved (modulo the
//     auto-promotion above).
func (b *Bank) UpdateMemory(id string, patch MemoryUpdate) (MemoryRecord, error) {
	if b == nil {
		return MemoryRecord{}, errors.New("bank not enabled")
	}
	if id == "" {
		return MemoryRecord{}, errors.New("id is required")
	}
	// We need the resulting tag set to compute the sensitive auto-promotion.
	current, err := b.GetMemory(id)
	if err != nil {
		return MemoryRecord{}, err
	}
	resultingTags := current.Tags
	if patch.Tags != nil {
		resultingTags = *patch.Tags
	}
	// Resolve sensitive:
	//   1. tag trigger always wins (force-on)
	//   2. regex scan of NEW text/enriched_text always wins (force-on)
	//   3. explicit patch.Sensitive=&true forces on
	//   4. patch.Sensitive=&false clears IFF no tag trigger AND scan clean
	//   5. otherwise keep the current value
	resolvedSensitive := current.Sensitive
	if patch.Sensitive != nil {
		resolvedSensitive = *patch.Sensitive
	}
	if tagsContainSensitive(resultingTags) {
		resolvedSensitive = true
	}
	if patch.Text != nil && len(scanSensitive(*patch.Text)) > 0 {
		resolvedSensitive = true
	}
	if patch.EnrichedText != nil && len(scanSensitive(*patch.EnrichedText)) > 0 {
		resolvedSensitive = true
	}

	sets := []string{"updated_at = ?"}
	args := []any{nowUTC()}
	if patch.Text != nil {
		sets = append(sets, "text = ?")
		args = append(args, *patch.Text)
	}
	if patch.EnrichedText != nil {
		sets = append(sets, "enriched_text = ?")
		args = append(args, *patch.EnrichedText)
	}
	if patch.Tags != nil {
		j, _ := json.Marshal(*patch.Tags)
		sets = append(sets, "tags = ?")
		args = append(args, string(j))
	}
	if patch.RegionHint != nil {
		sets = append(sets, "region_hint = ?")
		args = append(args, *patch.RegionHint)
	}
	if patch.Source != nil {
		sets = append(sets, "source = ?")
		args = append(args, *patch.Source)
	}
	if patch.MergedFrom != nil {
		j, _ := json.Marshal(*patch.MergedFrom)
		sets = append(sets, "merged_from = ?")
		args = append(args, string(j))
	}
	if patch.Sensitive != nil || (patch.Tags != nil && resolvedSensitive != current.Sensitive) {
		sets = append(sets, "sensitive = ?")
		args = append(args, boolToInt(resolvedSensitive))
	}
	// v2.7 Bundle L — TTL / dormancy patches.
	//
	// ExpiresAt: setting a non-empty TTL on a sensitive memory is rejected
	// (sticky-on policy — sensitive rows must stay reachable for audit and
	// can't be silently archived by a clock). Clearing (passing &"") is
	// always allowed.
	if patch.ExpiresAt != nil {
		if *patch.ExpiresAt != "" && resolvedSensitive {
			return MemoryRecord{}, errors.New("cannot set expires_at on a sensitive memory (sticky-on policy)")
		}
		sets = append(sets, "expires_at = ?")
		args = append(args, *patch.ExpiresAt)
	}
	// Dormant: manual wake / sleep. The nightly dormancy pass also writes
	// these columns when a TTL fires, but a caller can preempt or undo
	// that decision here. dormant_reason carries "manual" for caller
	// patches, "ttl" for the nightly pass.
	if patch.Dormant != nil {
		if *patch.Dormant && current.DormantAt == "" {
			sets = append(sets, "dormant_at = ?", "dormant_reason = ?")
			args = append(args, nowUTC(), "manual")
		} else if !*patch.Dormant && current.DormantAt != "" {
			sets = append(sets, "dormant_at = ?", "dormant_reason = ?")
			args = append(args, "", "")
		}
	}
	// Phase 0b dirty-flag trigger: text or tags changes mark the memory
	// for re-enrichment on the next nightly run. Sensitive-flag-only,
	// metadata-only, or other non-content edits don't dirty (they don't
	// change what Tier 2 would say about the memory). See handoff
	// "deep per-memory enrichment phase 2026-05-10" §Dirty-flag triggers.
	contentChanged := patch.Text != nil || patch.Tags != nil || patch.EnrichedText != nil
	if contentChanged {
		sets = append(sets, "marked_dirty_at = ?", "dirty_reason = ?")
		args = append(args, nowUTC(), "edited")
	}
	args = append(args, id)

	b.wmu.Lock()
	defer b.wmu.Unlock()
	res, err := b.db.Exec(
		`UPDATE memories SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return MemoryRecord{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return MemoryRecord{}, sql.ErrNoRows
	}

	// Emit a `sensitive.flipped` control event when:
	//   (a) the resolved value differs from the prior (genuine flip), OR
	//   (b) the caller asked for sensitive=false but the sticky-on policy
	//       kept it true (the dashboard shows "cannot clear — content still
	//       contains a sensitive pattern").
	// Source attribution priority: tag-trigger > auto-regex > manual > sticky-on.
	flipped := resolvedSensitive != current.Sensitive
	stickyOnReject := patch.Sensitive != nil && !*patch.Sensitive && resolvedSensitive
	if flipped || stickyOnReject {
		source := "manual"
		// `reason` carries the content-substance detail the dashboard uses to
		// explain WHY a memory got flagged. Falls back to the source label
		// when there's no specific pattern (manual / sticky-on flips).
		reason := source
		var hits []string
		switch {
		case stickyOnReject && resolvedSensitive == current.Sensitive:
			source = "sticky-on"
			reason = "sticky-on"
		case patch.Tags != nil && tagsContainSensitive(resultingTags) && !tagsContainSensitive(current.Tags):
			source = "tag-trigger"
			reason = "tag-trigger:" + firstSensitiveTag(resultingTags)
		case patch.Text != nil && len(scanSensitive(*patch.Text)) > 0:
			source = "auto-regex"
			hits = scanSensitive(*patch.Text)
			reason = "regex:" + strings.Join(hits, ",")
		case patch.EnrichedText != nil && len(scanSensitive(*patch.EnrichedText)) > 0:
			source = "auto-regex"
			hits = scanSensitive(*patch.EnrichedText)
			reason = "regex:" + strings.Join(hits, ",")
		case patch.Sensitive != nil:
			source = "manual"
			reason = "manual"
		}
		// before/after JSON: snapshot of the row pre- and post-flip, both
		// redacted via redactForAudit. The frontend's diff renderer uses
		// these to show "sensitive: false → true" + which fields changed.
		// We use `current` for before (what was on disk) and re-derive
		// after from current + the resolved fields, so we don't need
		// another GetMemory call inside the locked region.
		afterRow := current
		afterRow.Sensitive = resolvedSensitive
		// Apply the patch's user-visible field changes so the diff is
		// accurate (changes to text/tags also show up in the diff).
		if patch.Text != nil {
			afterRow.Text = *patch.Text
		}
		if patch.EnrichedText != nil {
			afterRow.EnrichedText = *patch.EnrichedText
		}
		if patch.Tags != nil {
			afterRow.Tags = *patch.Tags
		}
		beforeRedacted := redactForAudit(current, "auto_flag_sensitive")
		afterRedacted := redactForAudit(afterRow, "auto_flag_sensitive")
		bj, _ := json.Marshal(beforeRedacted)
		aj, _ := json.Marshal(afterRedacted)
		_ = b.appendAuditLocked(AuditEntry{
			Operation:  "auto_flag_sensitive",
			EntityType: "trace",
			EntityID:   id,
			BeforeJSON: string(bj),
			AfterJSON:  string(aj),
			Reason:     reason,
			AdapterID:  "sd-core-bank",
		})
		b.emit("sensitive.flipped", "sd-core-bank", map[string]interface{}{
			"trace_id":  id,
			"sensitive": resolvedSensitive,
			"source":    source,
		})
	}
	return b.GetMemory(id)
}

// updateMemoryContentChanged reports whether the most recent UpdateMemory
// patch to `id` would have flipped marked_dirty_at = now AND
// dirty_reason = 'edited'. Used by the HTTP handler so we know whether
// to call MarkSynthesisProductsDirty (cascading) post-update without
// duplicating the contentChanged predicate logic.
func updateMemoryContentChanged(patch MemoryUpdate) bool {
	return patch.Text != nil || patch.Tags != nil || patch.EnrichedText != nil
}

// ListUnscannedForSensitive returns memories that the AI sensitivity
// classifier has not yet processed (sensitive_checked_at = '' OR earlier
// than updated_at — meaning the row was edited after the last scan).
// Hard cap at 200 so a backlog flush doesn't lock the writer for long.
func (b *Bank) ListUnscannedForSensitive(limit int) ([]MemoryRecord, error) {
	if b == nil {
		return nil, errors.New("bank not enabled")
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := b.db.Query(
		`SELECT `+memoryColumns+` FROM memories
		 WHERE deleted_at = ''
		   AND (sensitive_checked_at = '' OR sensitive_checked_at < updated_at)
		 ORDER BY updated_at DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MemoryRecord{}
	for rows.Next() {
		rec, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// MarkSensitiveScan records that an AI sensitivity scan ran on a memory.
// If newlySensitive is true, also raises the sensitive flag (sticky-on).
//
// Emits `sensitive.flipped` with `source: "auto-ai"` ONLY when the row was
// previously not-sensitive and the AI just promoted it. Idempotent calls
// (already-sensitive rows reconfirmed as sensitive) don't emit.
func (b *Bank) MarkSensitiveScan(id string, newlySensitive bool) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	// Read prior value BEFORE the lock so we can detect a true flip
	// (not just an idempotent reconfirmation). One extra roundtrip on the
	// classifier path is cheap.
	priorSensitive := false
	if newlySensitive {
		var p int
		_ = b.db.QueryRow(`SELECT sensitive FROM memories WHERE id = ?`, id).Scan(&p)
		priorSensitive = p != 0
	}

	b.wmu.Lock()
	defer b.wmu.Unlock()
	if newlySensitive {
		_, err := b.db.Exec(
			`UPDATE memories SET sensitive = 1, sensitive_checked_at = ? WHERE id = ?`,
			nowUTC(), id,
		)
		if err == nil && !priorSensitive {
			b.emit("sensitive.flipped", "sd-core-sensitive-ai", map[string]interface{}{
				"trace_id":  id,
				"sensitive": true,
				"source":    "auto-ai",
			})
		}
		return err
	}
	_, err := b.db.Exec(
		`UPDATE memories SET sensitive_checked_at = ? WHERE id = ?`,
		nowUTC(), id,
	)
	return err
}

// MarkMemoryDirty stamps marked_dirty_at + dirty_reason on a single
// memory. Used by all R3 trigger hook sites (cascading, temporal_neighbor,
// surprise, emotional, tmr_user). Best-effort — idempotent in spirit:
// re-applying the same reason just bumps the timestamp.
//
// (CITATIONS.md #9 STC, #13 Ashton 2022, #14 Payne & Kensinger.)
func (b *Bank) MarkMemoryDirty(id, reason string) error {
	if b == nil || id == "" {
		return nil
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_, err := b.db.Exec(
		`UPDATE memories SET marked_dirty_at = ?, dirty_reason = ? WHERE id = ? AND deleted_at = ''`,
		nowUTC(), reason, id,
	)
	return err
}

// ──────────────────────────────────────────────────────────────────────────
// v2.7 Bundle L — Dormant Memories
// ──────────────────────────────────────────────────────────────────────────

// MemoriesInSession returns up to `limit` non-deleted, non-dormant
// memories sharing the given session_id, ordered by created_at ASC
// (chronological — the order the conversation actually happened in).
// Used by /recall's session co-retrieval (Bundle I lesson #4): when a
// top-K hit shares a session with other memories, promote those
// siblings into the candidate set so the synthesizer sees the full
// conversation context.
func (b *Bank) MemoriesInSession(sessionID string, limit int) ([]MemoryRecord, error) {
	if b == nil {
		return nil, errors.New("bank not enabled")
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := b.db.Query(`SELECT `+memoryColumns+`
		FROM memories
		WHERE session_id = ? AND deleted_at = '' AND dormant_at = ''
		ORDER BY created_at ASC
		LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MemoryRecord{}
	for rows.Next() {
		rec, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// DormantIDs returns the set of memory IDs currently marked dormant
// (dormant_at != '' and not soft-deleted). Used by /recall to filter
// the default current-view; structured as a set so the filter loop is
// a single map lookup per scored result.
func (b *Bank) DormantIDs() (map[string]struct{}, error) {
	if b == nil {
		return nil, errors.New("bank not enabled")
	}
	rows, err := b.db.Query(
		`SELECT id FROM memories WHERE dormant_at <> '' AND deleted_at = ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

// DormancyPassResult summarises one RunDormancyPass execution.
type DormancyPassResult struct {
	Scanned int `json:"scanned"`  // rows considered (expires_at <= now)
	Slept   int `json:"slept"`    // rows newly marked dormant
	Skipped int `json:"skipped"`  // already dormant / already deleted
}

// RunDormancyPass walks memories whose expires_at <= now and writes
// dormant_at + dormant_reason="ttl". Sensitive rows are skipped (sticky-on
// rejects TTL at the entrypoints, but legacy rows that pre-date the policy
// might still carry a TTL — better to defensively skip than archive a
// sensitive row by accident). Already-dormant and soft-deleted rows are
// no-ops. The pass is idempotent — re-running on the same minute does
// nothing new.
//
// Called by the nightly Phase 5b step and by the admin /admin/dormancy_pass
// endpoint (Bundle M will expose).
func (b *Bank) RunDormancyPass() (DormancyPassResult, error) {
	if b == nil {
		return DormancyPassResult{}, errors.New("bank not enabled")
	}
	now := nowUTC()
	rows, err := b.db.Query(
		`SELECT id, sensitive, dormant_at, deleted_at FROM memories
		 WHERE expires_at <> '' AND expires_at <= ?`, now)
	if err != nil {
		return DormancyPassResult{}, err
	}
	type cand struct {
		id     string
		sleeps bool // false = skip (already dormant / deleted / sensitive)
	}
	var cands []cand
	for rows.Next() {
		var id, dormantAt, deletedAt string
		var sensitive int
		if err := rows.Scan(&id, &sensitive, &dormantAt, &deletedAt); err != nil {
			rows.Close()
			return DormancyPassResult{}, err
		}
		c := cand{id: id, sleeps: dormantAt == "" && deletedAt == "" && sensitive == 0}
		cands = append(cands, c)
	}
	rows.Close()

	out := DormancyPassResult{Scanned: len(cands)}
	if len(cands) == 0 {
		return out, nil
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	for _, c := range cands {
		if !c.sleeps {
			out.Skipped++
			continue
		}
		_, err := b.db.Exec(
			`UPDATE memories SET dormant_at = ?, dormant_reason = ? WHERE id = ?`,
			now, "ttl", c.id)
		if err != nil {
			return out, fmt.Errorf("dormancy pass: %s: %w", c.id, err)
		}
		out.Slept++
		// Audit row so the dashboard can show "X memories went dormant
		// last night". Best-effort: don't fail the whole pass on a
		// missed audit row.
		_ = b.appendAuditLocked(AuditEntry{
			Operation:  "dormancy_set",
			EntityType: "memory",
			EntityID:   c.id,
			AfterJSON:  fmt.Sprintf(`{"dormant_at":%q,"dormant_reason":"ttl"}`, now),
			Reason:     "ttl fired",
		})
	}
	if out.Slept > 0 {
		log.Printf("dormancy pass: %d memories went dormant (scanned %d, skipped %d)",
			out.Slept, out.Scanned, out.Skipped)
	}
	return out, nil
}

// MarkSynthesisProductsDirty walks the bank for synthesis memories whose
// synthesis_source_ids JSON array contains sourceID, and marks each one
// dirty with reason='cascading'. Called from UpdateMemory when a memory
// that's a synthesis source has its text/tags edited — the synthesis
// product becomes stale and benefits from a fresh Tier 2 pass.
//
// [R3 cascading] CITATIONS.md #9 STC — when a tagged trace is touched,
// downstream products carrying its tag get re-considered.
func (b *Bank) MarkSynthesisProductsDirty(sourceID string) (int, error) {
	if b == nil || sourceID == "" {
		return 0, nil
	}
	// Cheap LIKE pre-filter; verify JSON membership in Go.
	rows, err := b.db.Query(
		`SELECT id, synthesis_source_ids FROM memories
		 WHERE deleted_at = ''
		   AND source IN ('nightly_synthesis', 'nightly_schema')
		   AND synthesis_source_ids LIKE '%' || ? || '%'`,
		sourceID,
	)
	if err != nil {
		return 0, err
	}
	hits := []string{}
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return 0, err
		}
		var ids []string
		if err := json.Unmarshal([]byte(raw), &ids); err != nil {
			continue
		}
		for _, x := range ids {
			if x == sourceID {
				hits = append(hits, id)
				break
			}
		}
	}
	rows.Close()
	if len(hits) == 0 {
		return 0, nil
	}
	now := nowUTC()
	b.wmu.Lock()
	defer b.wmu.Unlock()
	for _, id := range hits {
		_, _ = b.db.Exec(
			`UPDATE memories SET marked_dirty_at = ?, dirty_reason = 'cascading' WHERE id = ?`,
			now, id,
		)
	}
	return len(hits), nil
}

// MarkTemporalNeighborsDirty looks at memories created within ±30 minutes
// of the given memory (same region), and marks them dirty with
// reason='temporal_neighbor'. Triggered from SaveMemory when the new
// memory's salience > 0.7 — STC behavioral tagging.
//
// [R3 temporal_neighbor] CITATIONS.md #9 STC — a salient anchor tags
// nearby-in-time traces for preferential re-processing.
func (b *Bank) MarkTemporalNeighborsDirty(anchorID, region, createdAt string) (int, error) {
	if b == nil || anchorID == "" || createdAt == "" {
		return 0, nil
	}
	t, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		t, err = time.Parse(time.RFC3339, createdAt)
		if err != nil {
			return 0, nil
		}
	}
	earliest := t.Add(-30 * time.Minute).Format(time.RFC3339Nano)
	latest := t.Add(30 * time.Minute).Format(time.RFC3339Nano)

	b.wmu.Lock()
	defer b.wmu.Unlock()
	res, err := b.db.Exec(
		`UPDATE memories
		 SET marked_dirty_at = ?, dirty_reason = 'temporal_neighbor'
		 WHERE deleted_at = ''
		   AND id <> ?
		   AND region_hint = ?
		   AND created_at BETWEEN ? AND ?
		   AND deep_encoded_at <> ''`,
		nowUTC(), anchorID, region, earliest, latest,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// MarkPeriodicallyStale finds every memory whose deep_encoded_at is older
// than `cutoff` and re-marks it dirty with reason='periodic'. Called
// once per nightly run when settings.DeepEnrichRefreshDays > 0. Returns
// the affected row count.
//
// [R3 periodic] handoff §Dirty-flag triggers / 5 — bank drift means
// yesterday's enrichment isn't necessarily tomorrow's best summary.
// Default 90 days = quarterly refresh.
func (b *Bank) MarkPeriodicallyStale(cutoff string) (int, error) {
	if b == nil {
		return 0, nil
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	res, err := b.db.Exec(
		`UPDATE memories
		 SET marked_dirty_at = ?, dirty_reason = 'periodic'
		 WHERE deleted_at = ''
		   AND deep_encoded_at <> ''
		   AND deep_encoded_at < ?
		   AND (marked_dirty_at = '' OR marked_dirty_at < deep_encoded_at)`,
		nowUTC(), cutoff,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// MarkAllDeepEncodedDirty bulk-marks every already-deep-encoded memory as
// dirty with the given reason, EXCEPT memories whose deep_encoded_model
// matches `excludeModel` (used to skip rows that are already on the new
// model when reason="model_upgrade"). Returns the affected row count.
//
// Called from the settings handler when the user changes the Tier 2
// provider — the next nightly run starts catching the bank up to the
// new model. (Per handoff "deep per-memory enrichment phase 2026-05-10"
// §Dirty-flag triggers / Model upgrade.)
func (b *Bank) MarkAllDeepEncodedDirty(reason, excludeModel string) (int64, error) {
	if b == nil {
		return 0, errors.New("bank not enabled")
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	res, err := b.db.Exec(
		`UPDATE memories SET marked_dirty_at = ?, dirty_reason = ?
		 WHERE deleted_at = ''
		   AND deep_encoded_at <> ''
		   AND deep_encoded_model <> ?`,
		nowUTC(), reason, excludeModel,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PickNextDirtyMemory returns the next memory the Phase 0b queue should
// process. Priority: never-encoded > created > edited > model_upgrade >
// cascading > periodic. Within a priority class, oldest-dirty-first
// (fairness — no memory perpetually starved). Returns ID, ok=false when
// the queue is empty.
//
// (Per handoff "deep per-memory enrichment phase 2026-05-10"
// §Phase 0b pick-next query.)
func (b *Bank) PickNextDirtyMemory() (MemoryRecord, bool, error) {
	if b == nil {
		return MemoryRecord{}, false, errors.New("bank not enabled")
	}
	row := b.db.QueryRow(`SELECT ` + memoryColumns + ` FROM memories
WHERE deleted_at = ''
  AND (deep_encoded_at = '' OR marked_dirty_at > deep_encoded_at)
ORDER BY
  CASE
    -- Pinned memories (sd_pin_memory) outrank everything — user-curated
    -- ground truth gets deep-encoded first so its enriched_text + tags are
    -- ready immediately for retrieval. JSON tag scan is cheap because the
    -- subsequent LIMIT 1 means we only sort once per call.
    WHEN tags LIKE '%"pinned"%'               THEN -1
    -- [R11] User Targeted Memory Reactivation outranks everything else.
    WHEN dirty_reason   = 'tmr_user'          THEN 0
    -- Never-encoded next: foundational coverage gap.
    WHEN deep_encoded_at = ''                  THEN 1
    -- 'created' beats 'edited' so a fresh memory's first pass beats a
    -- re-think of an old one in the same window.
    WHEN dirty_reason   = 'created'           THEN 2
    WHEN dirty_reason   = 'edited'            THEN 3
    -- [R3] Surprise / emotional / temporal_neighbor are biologically
    -- elevated triggers — Ashton 2022 (#13), Payne & Kensinger (#14),
    -- STC (#9). They sit above background reasons.
    WHEN dirty_reason   = 'surprise'          THEN 4
    WHEN dirty_reason   = 'emotional'         THEN 5
    WHEN dirty_reason   = 'temporal_neighbor' THEN 6
    WHEN dirty_reason   = 'model_upgrade'     THEN 7
    WHEN dirty_reason   = 'cascading'         THEN 8
    WHEN dirty_reason   = 'periodic'          THEN 9
    ELSE 10
  END,
  marked_dirty_at ASC,
  created_at ASC
LIMIT 1`)
	rec, err := scanRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return MemoryRecord{}, false, nil
	}
	if err != nil {
		return MemoryRecord{}, false, err
	}
	return rec, true, nil
}

// CountDirtyMemories returns the total dirty-queue size + a breakdown by
// dirty_reason. Phase 0b reports both in stats.deep_encoding.queue.
func (b *Bank) CountDirtyMemories() (total int, byReason map[string]int, err error) {
	if b == nil {
		return 0, nil, errors.New("bank not enabled")
	}
	byReason = map[string]int{}
	rows, err := b.db.Query(`
SELECT
    CASE
        WHEN deep_encoded_at = '' AND dirty_reason = ''   THEN 'never_encoded'
        WHEN deep_encoded_at = ''                          THEN dirty_reason
        WHEN marked_dirty_at > deep_encoded_at             THEN dirty_reason
        ELSE NULL
    END AS reason,
    COUNT(*) AS n
FROM memories
WHERE deleted_at = ''
  AND (deep_encoded_at = '' OR marked_dirty_at > deep_encoded_at)
GROUP BY reason`)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var reason sql.NullString
		var n int
		if err := rows.Scan(&reason, &n); err != nil {
			return 0, nil, err
		}
		key := reason.String
		if !reason.Valid || key == "" {
			key = "unknown"
		}
		byReason[key] = n
		total += n
	}
	return total, byReason, rows.Err()
}

// CountDeepEncoded returns (live, deep_encoded) so the Phase 0b coverage
// percentage can be computed without a second query path.
func (b *Bank) CountDeepEncoded() (live, encoded int, err error) {
	if b == nil {
		return 0, 0, errors.New("bank not enabled")
	}
	if err := b.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE deleted_at = ''`).Scan(&live); err != nil {
		return 0, 0, err
	}
	if err := b.db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE deleted_at = '' AND deep_encoded_at <> ''`,
	).Scan(&encoded); err != nil {
		return 0, 0, err
	}
	return live, encoded, nil
}

// CountEmbeddedMemories returns how many live memories currently have a
// cached embedding row (matched by the synapseTextHash recipe). Used by
// the dashboard's "Phase 1 · Embeddings" progress bar so the ambient
// coverage reflects RIGHT-NOW state rather than the snapshot stamped on
// the last nightly run. O(N) over live memories; ~50ms at 4000 rows.
func (b *Bank) CountEmbeddedMemories() (live, embedded int, err error) {
	if b == nil {
		return 0, 0, errors.New("bank not enabled")
	}
	rows, queryErr := b.db.Query(`SELECT text, tags FROM memories WHERE deleted_at = ''`)
	if queryErr != nil {
		return 0, 0, queryErr
	}
	defer rows.Close()
	for rows.Next() {
		var text, tagsJSON string
		if err := rows.Scan(&text, &tagsJSON); err != nil {
			return 0, 0, err
		}
		live++
		var tags []string
		_ = json.Unmarshal([]byte(tagsJSON), &tags)
		tagsAny := make([]interface{}, len(tags))
		for i, t := range tags {
			tagsAny[i] = t
		}
		embedText := synapseEmbedText(map[string]interface{}{
			"text": text,
			"tags": tagsAny,
		})
		h := synapseTextHash(embedText)
		// b.GetEmbedding returns a nil vec + nil err when the row is
		// missing; we count "embedded" as a present-and-nonempty hit.
		vec, gerr := b.GetEmbedding(h)
		if gerr == nil && vec != nil {
			embedded++
		}
	}
	return live, embedded, rows.Err()
}

// PersistDeepEncoding writes the Tier 2 enrichment back to the memory
// row + clears the dirty marker + appends one row to deep_encoding_log.
// All in one writer-lock acquisition. The embedding refresh is handled
// by the caller (Phase 0b) after this returns.
func (b *Bank) PersistDeepEncoding(memID, summaryNew string, tagsNew []string,
	regionNew, model, reason string, tokensIn, tokensOut int, summaryDiff string,
	trigger string,
) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	if memID == "" {
		return errors.New("memID is required")
	}
	now := nowUTC()
	tagsJSON, _ := json.Marshal(tagsNew)

	// Read BEFORE state for the R3 'surprise' / 'emotional' triggers we
	// compute after the update lands. Read happens outside the writer
	// lock — the row is in a stable post-Phase-0b-Tier-2-call state by
	// the time we get here, so a concurrent UPDATE collision is
	// implausible. (CITATIONS.md #13 Ashton 2022 — surprise triggers
	// re-consolidation; #14 Payne & Kensinger — emotional weight.)
	var priorRegion, priorTagsRaw string
	_ = b.db.QueryRow(
		`SELECT region_hint, tags FROM memories WHERE id = ?`, memID,
	).Scan(&priorRegion, &priorTagsRaw)
	var priorTags []string
	_ = json.Unmarshal([]byte(priorTagsRaw), &priorTags)

	b.wmu.Lock()
	defer b.wmu.Unlock()

	// Apply only the fields Tier 2 actually changed. Empty strings mean
	// "Tier 2 left this alone"; we use COALESCE-style logic to preserve
	// the prior value rather than blanking it.
	sets := []string{
		"deep_encoded_at = ?",
		"deep_encoded_model = ?",
		"marked_dirty_at = ''",
		"dirty_reason = ''",
		"updated_at = ?",
	}
	args := []any{now, model, now}
	if summaryNew != "" {
		sets = append([]string{"enriched_text = ?"}, sets...)
		args = append([]any{summaryNew}, args...)
	}
	if tagsNew != nil {
		sets = append([]string{"tags = ?"}, sets...)
		args = append([]any{string(tagsJSON)}, args...)
	}
	if regionNew != "" {
		sets = append([]string{"region_hint = ?"}, sets...)
		args = append([]any{regionNew}, args...)
	}
	args = append(args, memID)

	if _, err := b.db.Exec(
		`UPDATE memories SET `+strings.Join(sets, ", ")+` WHERE id = ?`,
		args...,
	); err != nil {
		return fmt.Errorf("update memory: %w", err)
	}

	// Append the per-attempt log row. summaryDiff is a short human-readable
	// string ("text+tags refined", "added pkce tag, removed encoded") that
	// the trace-detail history panel renders verbatim.
	if _, err := b.db.Exec(`
INSERT INTO deep_encoding_log (memory_id, model, started_at, finished_at,
    tokens_in, tokens_out, summary_diff, trigger, error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, '')`,
		memID, model, now, now, tokensIn, tokensOut, summaryDiff, trigger,
	); err != nil {
		return fmt.Errorf("append deep_encoding_log: %w", err)
	}
	// [R1] Re-derive salience now that the memory has an updated
	// embedding + refined summary. Caller is expected to have already
	// written the new embedding via Phase 0b's loop, so the novelty
	// computation reflects current bank state. (CITATIONS.md #10 + #14.)
	// Best-effort: a salience-update failure shouldn't fail the whole
	// PersistDeepEncoding — it just leaves the column at its prior value
	// until the next refresh.
	textForScore := summaryNew
	if textForScore == "" {
		// Fallback: re-read the row's current enriched/raw text.
		var t, et string
		_ = b.db.QueryRow(`SELECT text, enriched_text FROM memories WHERE id = ?`, memID).Scan(&t, &et)
		textForScore = et
		if textForScore == "" {
			textForScore = t
		}
	}
	tagsForScore := tagsNew
	var lastRecalled string
	_ = b.db.QueryRow(`SELECT last_recalled_at FROM memories WHERE id = ?`, memID).Scan(&lastRecalled)
	tagsAny := make([]interface{}, len(tagsForScore))
	for i, t := range tagsForScore {
		tagsAny[i] = t
	}
	hash := synapseTextHash(synapseEmbedText(map[string]interface{}{
		"text": textForScore, "tags": tagsAny,
	}))
	novelty := computeNoveltyForMemory(b, memID, hash)
	salience := computeSalience(textForScore, tagsForScore, novelty, lastRecalled, time.Now().UTC())
	_, _ = b.db.Exec(`UPDATE memories SET salience = ? WHERE id = ?`, salience, memID)

	// [R3 surprise] CITATIONS.md #13 Ashton 2022. If Tier 2 changed the
	// region OR added/removed >50% of tags, the memory is "schematically
	// incongruent" enough to deserve a second pass on the next nightly
	// run. Re-mark dirty AFTER clearing the dirty flag above.
	regionChanged := regionNew != "" && !strings.EqualFold(regionNew, priorRegion)
	tagDelta := tagDeltaFraction(priorTags, tagsForScore)
	if regionChanged || tagDelta > 0.5 {
		_, _ = b.db.Exec(
			`UPDATE memories SET marked_dirty_at = ?, dirty_reason = 'surprise' WHERE id = ?`,
			nowUTC(), memID,
		)
	} else if salience > 0.8 {
		// [R3 emotional] CITATIONS.md #14 Payne & Kensinger. High-salience
		// memories get a "remembered for days" treatment — they re-enter
		// the queue automatically so successive nightly runs keep
		// thinking about them. Threshold 0.8 is conservative (only the
		// top of the salience curve qualifies).
		_, _ = b.db.Exec(
			`UPDATE memories SET marked_dirty_at = ?, dirty_reason = 'emotional' WHERE id = ?`,
			nowUTC(), memID,
		)
	}
	return nil
}

// tagDeltaFraction computes the Jaccard distance (1 - intersection/union)
// between two tag sets, case-insensitive. Returns 0 when both empty,
// 1 when fully disjoint. Used by the R3 'surprise' trigger to decide
// "did Tier 2 substantially relabel this memory?"
func tagDeltaFraction(before, after []string) float64 {
	if len(before) == 0 && len(after) == 0 {
		return 0
	}
	beforeSet := map[string]bool{}
	for _, t := range before {
		beforeSet[strings.ToLower(strings.TrimSpace(t))] = true
	}
	afterSet := map[string]bool{}
	for _, t := range after {
		afterSet[strings.ToLower(strings.TrimSpace(t))] = true
	}
	inter := 0
	union := 0
	seen := map[string]bool{}
	for k := range beforeSet {
		if afterSet[k] {
			inter++
		}
		seen[k] = true
		union++
	}
	for k := range afterSet {
		if !seen[k] {
			union++
		}
	}
	if union == 0 {
		return 0
	}
	return 1.0 - float64(inter)/float64(union)
}

// LogDeepEncodingFailure records a Tier 2 attempt that errored AND bumps
// the memory's marked_dirty_at so it rotates to the back of the dirty
// queue. Without the bump, PickNextDirtyMemory would keep returning the
// same doomed memory and the bulk-encode endpoint would infinite-loop on
// a fail-always Tier 2.
//
// The retry-budget logic (CountDeepEncodingFailures) reads back the log
// rows to decide whether to drop the memory entirely after N consecutive
// failures.
func (b *Bank) LogDeepEncodingFailure(memID, model, errMsg, trigger string) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	now := nowUTC()
	b.wmu.Lock()
	defer b.wmu.Unlock()
	if _, err := b.db.Exec(`
INSERT INTO deep_encoding_log (memory_id, model, started_at, finished_at,
    tokens_in, tokens_out, summary_diff, trigger, error)
VALUES (?, ?, ?, ?, 0, 0, '', ?, ?)`,
		memID, model, now, now, trigger, errMsg,
	); err != nil {
		return err
	}
	// Push to back of queue so other dirty memories get a turn.
	_, _ = b.db.Exec(
		`UPDATE memories SET marked_dirty_at = ? WHERE id = ?`,
		now, memID,
	)
	return nil
}

// CountDeepEncodingFailures returns how many CONSECUTIVE failed attempts
// the most recent log rows show for `memID`. After 3 consecutive, the
// Phase 0b queue should drop the memory (manual intervention needed).
func (b *Bank) CountDeepEncodingFailures(memID string) (int, error) {
	if b == nil {
		return 0, errors.New("bank not enabled")
	}
	rows, err := b.db.Query(`
SELECT error FROM deep_encoding_log
WHERE memory_id = ?
ORDER BY started_at DESC
LIMIT 5`, memID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	consecutive := 0
	for rows.Next() {
		var errMsg string
		if err := rows.Scan(&errMsg); err != nil {
			return 0, err
		}
		if errMsg == "" {
			break // most recent attempt succeeded — reset
		}
		consecutive++
	}
	return consecutive, nil
}

// BumpLastRecalled stamps last_recalled_at on a memory. Best-effort: errors
// are swallowed by the caller — a failed bump shouldn't block the /recall
// response. Phase 9 (reinforcement) reads this to decide which memories
// got hit since the previous Dream Cycle.
func (b *Bank) BumpLastRecalled(id string) error {
	if b == nil || id == "" {
		return nil
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_, err := b.db.Exec(
		`UPDATE memories SET last_recalled_at = ? WHERE id = ?`,
		nowUTC(), id,
	)
	return err
}

// SetMemoryLifecycle flips one of the lifecycle bool flags. flag must be one
// of: "light_encoded" | "nightly_consolidated" | "oracle_augmented".
//
// [R7] Side effect on light_encoded → true: bumps consolidation_stage from
// 'episodic' to 'consolidating' (CITATIONS.md #2 — Stickgold & Walker 2010
// systems consolidation). Idempotent: a row already at 'consolidating' or
// 'semantic' stays there — we never regress the stage.
func (b *Bank) SetMemoryLifecycle(id, flag string, value bool) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	allowed := map[string]bool{
		"light_encoded": true, "nightly_consolidated": true, "oracle_augmented": true,
	}
	if !allowed[flag] {
		return fmt.Errorf("invalid lifecycle flag %q", flag)
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	if _, err := b.db.Exec(
		`UPDATE memories SET `+flag+` = ?, updated_at = ? WHERE id = ?`,
		boolToInt(value), nowUTC(), id,
	); err != nil {
		return err
	}
	if flag == "light_encoded" && value {
		_, _ = b.db.Exec(
			`UPDATE memories SET consolidation_stage = 'consolidating'
			 WHERE id = ? AND consolidation_stage = 'episodic'`,
			id,
		)
	}
	return nil
}

// SoftDeleteMemory marks a memory as deleted but preserves the row for the
// SD_TRASH_DAYS grace period. Subsequent reads via ListMemories exclude it
// unless OnlyDeleted/IncludeDeleted are set.
func (b *Bank) SoftDeleteMemory(id string) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	now := nowUTC()
	res, err := b.db.Exec(
		`UPDATE memories SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at = ''`,
		now, now, id,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Either missing or already soft-deleted.
		var existed int
		_ = b.db.QueryRow(`SELECT 1 FROM memories WHERE id = ?`, id).Scan(&existed)
		if existed == 0 {
			return sql.ErrNoRows
		}
	}
	return nil
}

// UndeleteMemory clears deleted_at, restoring the row to live state.
//
// It ALSO clears deleted_reason. This enforces the invariant "no live row
// carries a deletion reason": a restored row whose deleted_at is '' must not
// keep a stale "merged into <id>" / "ttl" reason from a prior tombstone.
// Leaving deleted_reason set was the resurrection-orphan bug — a bulk restore
// (POST /bank/memories/{id}/restore) cleared deleted_at but left the reason,
// producing 52% of the bank as live-but-merged rows. dormant_at / dormant_reason
// are an orthogonal lifecycle axis (a row can be dormant independent of
// deletion) so restore intentionally leaves them untouched — un-deleting is
// not un-sleeping.
func (b *Bank) UndeleteMemory(id string) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	res, err := b.db.Exec(
		`UPDATE memories SET deleted_at = '', deleted_reason = '', updated_at = ? WHERE id = ?`,
		nowUTC(), id,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// HardDeleteMemory permanently removes a row. Cascades to map_traces.
// Caller is responsible for the audit_log entry (so before_json is captured).
func (b *Bank) HardDeleteMemory(id string) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	res, err := b.db.Exec(`DELETE FROM memories WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// PurgeOldDeletes hard-deletes any memory whose deleted_at is older than
// `olderThan` (RFC3339). Returns the number purged. Suitable for nightly cron.
func (b *Bank) PurgeOldDeletes(olderThan string) (int64, error) {
	if b == nil {
		return 0, errors.New("bank not enabled")
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	res, err := b.db.Exec(
		`DELETE FROM memories WHERE deleted_at <> '' AND deleted_at < ?`, olderThan,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ──────────────────────────────────────────────────────────────────────────
// Memory Maps
// ──────────────────────────────────────────────────────────────────────────

// MemoryMap is a profile entity (project/person/technology/organization/concept).
type MemoryMap struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Type       string   `json:"type"` // project|person|technology|organization|concept
	SchemaText string   `json:"schema_text,omitempty"`
	AnchorTags []string `json:"anchor_tags"`
	Source     string   `json:"source,omitempty"` // inferred|mcp_direct|oracle_context|web_research
	// Dream pipeline (Phase 4) — derived metadata refreshed each nightly run.
	Archived      bool   `json:"archived"`
	TopRegion     string `json:"top_region,omitempty"`
	LastTouchedAt string `json:"last_touched_at,omitempty"`
	// [R13 redesign — v2.4.0b1] Map-proposal lifecycle. Status is the
	// load-bearing field; the other two are provenance for the audit trail.
	// Default-friendly: empty Status from a legacy caller is coerced to
	// 'accepted' on save so older write paths keep working.
	Status          string `json:"status,omitempty"`           // proposed|accepted|dismissed
	GeneratedBy     string `json:"generated_by,omitempty"`     // user|r13_redesign|phase_4|legacy
	GenerationPhase string `json:"generation_phase,omitempty"` // '4' | '8.7' | 'user_triggered' | ''
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
	// TraceCount is the live count of map_traces rows linking memories to
	// this map. Populated by ListMemoryMapsWith via a correlated subquery
	// so the dashboard list view doesn't need 587 separate /traces probes.
	// 0-valued on single-map GETs (omitempty would hide a real "0 traces"
	// signal, so we keep it explicit).
	TraceCount int `json:"trace_count"`
}

// validMapStatuses gates writes through SaveMemoryMap + the propose/accept/
// dismiss handlers. Anything not in this set is rejected at the API edge.
var validMapStatuses = map[string]bool{
	"proposed":  true,
	"accepted":  true,
	"dismissed": true,
}

var validMapTypes = map[string]bool{
	"project": true, "person": true, "technology": true,
	"organization": true, "concept": true,
}

// SaveMemoryMap upserts a MemoryMap. If id is empty, generates one.
func (b *Bank) SaveMemoryMap(m MemoryMap) (MemoryMap, error) {
	if b == nil {
		return m, errors.New("bank not enabled")
	}
	if m.Name == "" {
		return m, errors.New("name is required")
	}
	if !validMapTypes[m.Type] {
		return m, fmt.Errorf("invalid map type %q", m.Type)
	}
	if m.ID == "" {
		m.ID = newID("map")
	}
	if m.AnchorTags == nil {
		m.AnchorTags = []string{}
	}
	if m.Source == "" {
		m.Source = "inferred"
	}
	// [R13 redesign] Default empty Status to 'accepted' so legacy SaveMemoryMap
	// callers (Phase 4, /maps POST handler) keep producing user-confirmed maps
	// without explicit status arguments. Propose-path callers always set
	// Status='proposed' explicitly.
	if m.Status == "" {
		m.Status = "accepted"
	}
	if !validMapStatuses[m.Status] {
		return m, fmt.Errorf("invalid map status %q", m.Status)
	}
	if m.GeneratedBy == "" {
		m.GeneratedBy = "user"
	}
	now := nowUTC()
	if m.CreatedAt == "" {
		m.CreatedAt = now
	}
	m.UpdatedAt = now
	tagsJSON, _ := json.Marshal(m.AnchorTags)

	b.wmu.Lock()
	defer b.wmu.Unlock()
	_, err := b.db.Exec(`
INSERT INTO memory_maps (id, name, type, schema_text, anchor_tags, source,
    archived, top_region, last_touched_at, status, generated_by, generation_phase,
    created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    name             = excluded.name,
    type             = excluded.type,
    schema_text      = excluded.schema_text,
    anchor_tags      = excluded.anchor_tags,
    source           = excluded.source,
    status           = excluded.status,
    generated_by     = excluded.generated_by,
    generation_phase = excluded.generation_phase,
    updated_at       = excluded.updated_at`,
		m.ID, m.Name, m.Type, m.SchemaText, string(tagsJSON), m.Source,
		boolToInt(m.Archived), m.TopRegion, m.LastTouchedAt,
		m.Status, m.GeneratedBy, m.GenerationPhase,
		m.CreatedAt, m.UpdatedAt,
	)
	if err != nil {
		return m, err
	}
	return m, nil
}

// MemoryMapUpdate is a partial-patch payload for UpdateMemoryMap.
type MemoryMapUpdate struct {
	Name       *string   `json:"name,omitempty"`
	Type       *string   `json:"type,omitempty"`
	SchemaText *string   `json:"schema_text,omitempty"`
	AnchorTags *[]string `json:"anchor_tags,omitempty"`
	Source     *string   `json:"source,omitempty"`
	// Status — R13 redesign. proposed|accepted|dismissed. Accept/dismiss
	// endpoints pass this; user PATCH should not unless they really know
	// what they're doing (validMapStatuses enforces).
	Status *string `json:"status,omitempty"`
}

// UpdateMemoryMap applies a partial patch.
func (b *Bank) UpdateMemoryMap(id string, patch MemoryMapUpdate) (MemoryMap, error) {
	if b == nil {
		return MemoryMap{}, errors.New("bank not enabled")
	}
	sets := []string{"updated_at = ?"}
	args := []any{nowUTC()}
	if patch.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *patch.Name)
	}
	if patch.Type != nil {
		if !validMapTypes[*patch.Type] {
			return MemoryMap{}, fmt.Errorf("invalid map type %q", *patch.Type)
		}
		sets = append(sets, "type = ?")
		args = append(args, *patch.Type)
	}
	if patch.SchemaText != nil {
		sets = append(sets, "schema_text = ?")
		args = append(args, *patch.SchemaText)
	}
	if patch.AnchorTags != nil {
		j, _ := json.Marshal(*patch.AnchorTags)
		sets = append(sets, "anchor_tags = ?")
		args = append(args, string(j))
	}
	if patch.Source != nil {
		sets = append(sets, "source = ?")
		args = append(args, *patch.Source)
	}
	if patch.Status != nil {
		if !validMapStatuses[*patch.Status] {
			return MemoryMap{}, fmt.Errorf("invalid map status %q (expected proposed|accepted|dismissed)", *patch.Status)
		}
		sets = append(sets, "status = ?")
		args = append(args, *patch.Status)
	}
	args = append(args, id)

	b.wmu.Lock()
	defer b.wmu.Unlock()
	res, err := b.db.Exec(
		`UPDATE memory_maps SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...,
	)
	if err != nil {
		return MemoryMap{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return MemoryMap{}, sql.ErrNoRows
	}
	return b.GetMemoryMap(id)
}

const memoryMapColumns = `id, name, type, schema_text, anchor_tags, source,
    archived, top_region, last_touched_at, status, generated_by, generation_phase,
    created_at, updated_at`

func scanMemoryMap(s scanner) (MemoryMap, error) {
	var m MemoryMap
	var tagsJSON string
	var archived int
	if err := s.Scan(&m.ID, &m.Name, &m.Type, &m.SchemaText, &tagsJSON, &m.Source,
		&archived, &m.TopRegion, &m.LastTouchedAt,
		&m.Status, &m.GeneratedBy, &m.GenerationPhase,
		&m.CreatedAt, &m.UpdatedAt); err != nil {
		return m, err
	}
	m.Archived = archived != 0
	if tagsJSON != "" {
		_ = json.Unmarshal([]byte(tagsJSON), &m.AnchorTags)
	}
	if m.AnchorTags == nil {
		m.AnchorTags = []string{}
	}
	return m, nil
}

// GetMemoryMap returns a single map by id.
func (b *Bank) GetMemoryMap(id string) (MemoryMap, error) {
	if b == nil {
		return MemoryMap{}, errors.New("bank not enabled")
	}
	row := b.db.QueryRow(`SELECT `+memoryMapColumns+` FROM memory_maps WHERE id = ?`, id)
	return scanMemoryMap(row)
}

// GetMemoryMapByName returns a single map by (name, type).
func (b *Bank) GetMemoryMapByName(name, mapType string) (MemoryMap, error) {
	if b == nil {
		return MemoryMap{}, errors.New("bank not enabled")
	}
	// 2026-05-26 — when mapType is empty, callers (Phase 4 at
	// nightly_pipeline.go:1744) expect "first live map with this name
	// regardless of type". The legacy query `type = ?` matched only rows
	// where type was the empty string, which never happens — so empty
	// mapType reliably returned ErrNoRows and Phase 4 re-proposed concepts
	// that already existed as an accepted Project/Entity/Technology under
	// the same name, producing perpetual Proposed regrowth. Archived rows
	// are excluded so a stale archive doesn't satisfy the lookup and starve
	// a legitimate re-propose.
	var (
		row *sql.Row
	)
	if mapType == "" {
		row = b.db.QueryRow(
			`SELECT `+memoryMapColumns+` FROM memory_maps
			 WHERE name = ? AND archived = 0
			 ORDER BY CASE status WHEN 'accepted' THEN 0 WHEN 'proposed' THEN 1 ELSE 2 END,
			          updated_at DESC
			 LIMIT 1`,
			name,
		)
	} else {
		row = b.db.QueryRow(
			`SELECT `+memoryMapColumns+` FROM memory_maps
			 WHERE name = ? AND type = ? AND archived = 0
			 LIMIT 1`,
			name, mapType,
		)
	}
	return scanMemoryMap(row)
}

// ListMemoryMaps returns up to limit maps; filter by type if non-empty.
// Back-compat shape: every caller pre-v2.4.0b1 expects all rows regardless of
// proposal status. Use ListMemoryMapsWith for status filtering.
func (b *Bank) ListMemoryMaps(mapType string, limit int) ([]MemoryMap, error) {
	return b.ListMemoryMapsWith(MapListOpts{Type: mapType, Limit: limit})
}

// MapListOpts narrows ListMemoryMapsWith. Empty Status returns rows of every
// status (the legacy shape — Phase 4 + nightly pipeline rely on this).
type MapListOpts struct {
	Type   string
	Limit  int
	Status string // 'proposed' | 'accepted' | 'dismissed' | '' (any)
}

// ListMemoryMapsWith is the rich-options variant of ListMemoryMaps.
func (b *Bank) ListMemoryMapsWith(opts MapListOpts) ([]MemoryMap, error) {
	if b == nil {
		return nil, errors.New("bank not enabled")
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	conds := []string{}
	args := []any{}
	if opts.Type != "" {
		conds = append(conds, "type = ?")
		args = append(args, opts.Type)
	}
	if opts.Status != "" {
		conds = append(conds, "status = ?")
		args = append(args, opts.Status)
	}
	// Include a live trace_count via a correlated subquery — saves the
	// dashboard from issuing one /maps/{id}/traces probe per map just to
	// show the trace badge on the list view (was ~587 extra round-trips
	// before this, so list-view badges all rendered as 0).
	q := `SELECT ` + memoryMapColumns + `,
	      COALESCE((SELECT COUNT(*) FROM map_traces mt WHERE mt.map_id = memory_maps.id), 0) AS trace_count
	      FROM memory_maps`
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, " AND ")
	}
	q += ` ORDER BY updated_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := b.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MemoryMap{}
	for rows.Next() {
		var m MemoryMap
		var tagsJSON string
		var archived int
		var tc int
		if err := rows.Scan(&m.ID, &m.Name, &m.Type, &m.SchemaText, &tagsJSON, &m.Source,
			&archived, &m.TopRegion, &m.LastTouchedAt,
			&m.Status, &m.GeneratedBy, &m.GenerationPhase,
			&m.CreatedAt, &m.UpdatedAt, &tc); err != nil {
			return nil, err
		}
		m.Archived = archived != 0
		if tagsJSON != "" {
			_ = json.Unmarshal([]byte(tagsJSON), &m.AnchorTags)
		}
		if m.AnchorTags == nil {
			m.AnchorTags = []string{}
		}
		m.TraceCount = tc
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteMemoryMap removes a map. Cascades to map_associations + map_traces.
func (b *Bank) DeleteMemoryMap(id string) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	res, err := b.db.Exec(`DELETE FROM memory_maps WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	// Best-effort orphan sweep — see CleanupMemoryMaps for the rationale.
	// map_overrides has no FK to memory_maps because the override key may
	// be either a backend UUID OR a raw tag string for synthesized maps.
	if _, err := b.db.Exec(`DELETE FROM map_overrides WHERE map_key = ?`, id); err != nil {
		log.Printf("delete_map: dropping orphan override %s failed (non-fatal): %v", id, err)
	}
	return nil
}

// MapCleanupOpts controls which maps are eligible for bulk cleanup. All
// filters are AND-combined; a map must match every set option to be touched.
type MapCleanupOpts struct {
	DeleteEmpty      bool   // delete maps with 0 linked memories
	DeleteDismissed  bool   // delete maps with status='dismissed'
	DeleteAllMatching bool  // delete EVERY map that passes OnlyType / OnlyGeneratedBy
	                       // regardless of trace count / status. Use with care; intended
	                       // for one-shot "clear all concept maps" type operations
	                       // before the propose-flow takeover.
	OnlyType         string // restrict to a specific type (concept|project|...)
	OnlyGeneratedBy  string // restrict by generation source (user|legacy|phase_4|r13_redesign)
	DryRun           bool   // count only, no deletes
}

// MapCleanupResult is the report returned to the caller.
type MapCleanupResult struct {
	Eligible       int      `json:"eligible"`        // matched the filter
	Deleted        int      `json:"deleted"`         // actually removed (0 when DryRun)
	DryRun         bool     `json:"dry_run"`
	SampleNames    []string `json:"sample_names"`    // first ~20 names of eligible maps
	ByType         map[string]int `json:"by_type"`
	ByGeneratedBy  map[string]int `json:"by_generated_by"`
}

// CleanupMemoryMaps walks memory_maps applying the OR-set of cleanup filters
// and either reports (DryRun) or deletes matching rows. Associations and
// trace links cascade via FK ON DELETE CASCADE on the join tables.
func (b *Bank) CleanupMemoryMaps(opts MapCleanupOpts) (MapCleanupResult, error) {
	if b == nil {
		return MapCleanupResult{}, errors.New("bank not enabled")
	}
	if !opts.DeleteEmpty && !opts.DeleteDismissed && !opts.DeleteAllMatching {
		return MapCleanupResult{}, errors.New("at least one of delete_empty / delete_dismissed / delete_all_matching must be true")
	}
	// DeleteAllMatching requires a type or generated_by filter — without one,
	// the call would nuke every map in the bank. Defensive guard.
	if opts.DeleteAllMatching && opts.OnlyType == "" && opts.OnlyGeneratedBy == "" {
		return MapCleanupResult{}, errors.New("delete_all_matching requires only_type or only_generated_by to scope the wipe")
	}
	// Build the WHERE clause. trace_count is computed live via a LEFT JOIN
	// onto map_traces so we don't rely on a stored counter (which can drift).
	cond := []string{}
	args := []interface{}{}
	// When DeleteAllMatching is set, skip the empty/dismissed OR-group
	// entirely — the only_type/only_generated_by filters below carry the
	// full constraint.
	if !opts.DeleteAllMatching {
		or := []string{}
		if opts.DeleteEmpty {
			or = append(or, "(SELECT COUNT(1) FROM map_traces mt WHERE mt.map_id = m.id) = 0")
		}
		if opts.DeleteDismissed {
			or = append(or, "m.status = 'dismissed'")
		}
		cond = append(cond, "("+strings.Join(or, " OR ")+")")
	}
	if opts.OnlyType != "" {
		cond = append(cond, "m.type = ?")
		args = append(args, opts.OnlyType)
	}
	if opts.OnlyGeneratedBy != "" {
		cond = append(cond, "m.generated_by = ?")
		args = append(args, opts.OnlyGeneratedBy)
	}
	where := "WHERE " + strings.Join(cond, " AND ")

	// First pass: count + sample names + breakdown.
	rows, err := b.db.Query(`SELECT m.id, m.name, m.type, m.generated_by FROM memory_maps m `+where, args...)
	if err != nil {
		return MapCleanupResult{}, fmt.Errorf("scan eligible: %w", err)
	}
	defer rows.Close()
	result := MapCleanupResult{
		DryRun:        opts.DryRun,
		ByType:        map[string]int{},
		ByGeneratedBy: map[string]int{},
	}
	var eligibleIDs []string
	for rows.Next() {
		var id, name, typ, gen string
		if err := rows.Scan(&id, &name, &typ, &gen); err != nil {
			return MapCleanupResult{}, err
		}
		result.Eligible++
		result.ByType[typ]++
		result.ByGeneratedBy[gen]++
		eligibleIDs = append(eligibleIDs, id)
		if len(result.SampleNames) < 20 {
			result.SampleNames = append(result.SampleNames, name)
		}
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	if opts.DryRun || result.Eligible == 0 {
		return result, nil
	}

	// Second pass: delete. Done one-by-one so a failing row doesn't kill the
	// whole batch. SQLite is fast enough; ~600 maps deletes in <100ms.
	//
	// map_overrides has NO FK to memory_maps (intentional — overrides also
	// apply to synthesized tag-derived maps where map_key is the raw tag),
	// so we MUST manually delete the matching override row whenever we
	// drop a backend-persisted map. Otherwise the orphan override leaks
	// back into the UI's _buildConceptMaps() forcedExtra step and shows
	// up as a ghost concept map keyed by the deleted UUID. Bug seen in
	// the wild on map-1778395048525070281 + two siblings (2026-05-12).
	b.wmu.Lock()
	defer b.wmu.Unlock()
	for _, id := range eligibleIDs {
		res, err := b.db.Exec(`DELETE FROM memory_maps WHERE id = ?`, id)
		if err != nil {
			return result, fmt.Errorf("delete %s: %w", id, err)
		}
		n, _ := res.RowsAffected()
		if n > 0 {
			result.Deleted++
			// Best-effort orphan override sweep. Failure here doesn't roll
			// back the map delete — losing the override row is recoverable
			// (next /maps/overrides reload reconciles), losing the map
			// delete would force the user to retry the whole cleanup.
			if _, err := b.db.Exec(`DELETE FROM map_overrides WHERE map_key = ?`, id); err != nil {
				log.Printf("cleanup: dropping orphan override %s failed (non-fatal): %v", id, err)
			}
		}
	}
	return result, nil
}

// ──────────────────────────────────────────────────────────────────────────
// Associations
// ──────────────────────────────────────────────────────────────────────────

// MapAssociation is a typed edge between two MemoryMaps.
type MapAssociation struct {
	ID          string  `json:"id"`
	FromMapID   string  `json:"from_map_id"`
	ToMapID     string  `json:"to_map_id"`
	Association string  `json:"association"` // uses_technology|worked_on|collaborated_with|related_to|depends_on|...
	Weight      float64 `json:"weight"`
	CreatedAt   string  `json:"created_at"`
}

// AddAssociation upserts an edge. Idempotent on (from, to, association).
func (b *Bank) AddAssociation(a MapAssociation) (MapAssociation, error) {
	if b == nil {
		return a, errors.New("bank not enabled")
	}
	if a.FromMapID == "" || a.ToMapID == "" {
		return a, errors.New("from_map_id and to_map_id are required")
	}
	if a.Association == "" {
		return a, errors.New("association is required")
	}
	if a.Weight == 0 {
		a.Weight = 1.0
	}
	if a.ID == "" {
		a.ID = newID("assoc")
	}
	if a.CreatedAt == "" {
		a.CreatedAt = nowUTC()
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_, err := b.db.Exec(`
INSERT INTO map_associations (id, from_map_id, to_map_id, association, weight, created_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(from_map_id, to_map_id, association) DO UPDATE SET
    weight = excluded.weight`,
		a.ID, a.FromMapID, a.ToMapID, a.Association, a.Weight, a.CreatedAt,
	)
	return a, err
}

// RemoveAssociation deletes by (from, to, association).
func (b *Bank) RemoveAssociation(fromID, toID, association string) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	res, err := b.db.Exec(
		`DELETE FROM map_associations WHERE from_map_id = ? AND to_map_id = ? AND association = ?`,
		fromID, toID, association,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListAssociations returns all edges where the given map appears as either
// `from` (outgoing) or `to` (incoming). direction = "from" | "to" | "both".
func (b *Bank) ListAssociations(mapID, direction string) ([]MapAssociation, error) {
	if b == nil {
		return nil, errors.New("bank not enabled")
	}
	var q string
	switch direction {
	case "from":
		q = `SELECT id, from_map_id, to_map_id, association, weight, created_at
		     FROM map_associations WHERE from_map_id = ? ORDER BY weight DESC`
	case "to":
		q = `SELECT id, from_map_id, to_map_id, association, weight, created_at
		     FROM map_associations WHERE to_map_id = ? ORDER BY weight DESC`
	default:
		q = `SELECT id, from_map_id, to_map_id, association, weight, created_at
		     FROM map_associations WHERE from_map_id = ? OR to_map_id = ?
		     ORDER BY weight DESC`
	}
	var rows *sql.Rows
	var err error
	if direction == "both" || direction == "" {
		rows, err = b.db.Query(q, mapID, mapID)
	} else {
		rows, err = b.db.Query(q, mapID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MapAssociation{}
	for rows.Next() {
		var a MapAssociation
		if err := rows.Scan(&a.ID, &a.FromMapID, &a.ToMapID,
			&a.Association, &a.Weight, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ──────────────────────────────────────────────────────────────────────────
// Map ↔ Trace pivot
// ──────────────────────────────────────────────────────────────────────────

// LinkMapTrace adds a (map_id, trace_id) pair. Idempotent.
func (b *Bank) LinkMapTrace(mapID, traceID string) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_, err := b.db.Exec(
		`INSERT INTO map_traces (map_id, trace_id, created_at) VALUES (?, ?, ?)
		 ON CONFLICT(map_id, trace_id) DO NOTHING`,
		mapID, traceID, nowUTC(),
	)
	return err
}

// UnlinkMapTrace removes a (map_id, trace_id) pair.
func (b *Bank) UnlinkMapTrace(mapID, traceID string) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	res, err := b.db.Exec(
		`DELETE FROM map_traces WHERE map_id = ? AND trace_id = ?`, mapID, traceID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListTracesForMap returns trace IDs linked to the given map.
func (b *Bank) ListTracesForMap(mapID string, limit int) ([]string, error) {
	if b == nil {
		return nil, errors.New("bank not enabled")
	}
	if limit <= 0 {
		limit = 200
	}
	rows, err := b.db.Query(
		`SELECT trace_id FROM map_traces WHERE map_id = ? ORDER BY created_at DESC LIMIT ?`,
		mapID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListMapsForTrace returns map IDs the given trace is associated with.
func (b *Bank) ListMapsForTrace(traceID string) ([]string, error) {
	if b == nil {
		return nil, errors.New("bank not enabled")
	}
	rows, err := b.db.Query(
		`SELECT map_id FROM map_traces WHERE trace_id = ?`, traceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ──────────────────────────────────────────────────────────────────────────
// Lexicon
// ──────────────────────────────────────────────────────────────────────────

// LexiconRow is a tag-co-occurrence count + weight.
type LexiconRow struct {
	TagA         string  `json:"tag_a"`
	TagB         string  `json:"tag_b"`
	Cooccurrence int     `json:"cooccurrence"`
	Weight       float64 `json:"weight"`
	UpdatedAt    string  `json:"updated_at"`
}

// canonicalLexKey orders (a,b) so (a,b) and (b,a) collapse to the same row.
func canonicalLexKey(a, b string) (string, string) {
	if a <= b {
		return a, b
	}
	return b, a
}

// UpsertLexicon writes a row. (tag_a, tag_b) are auto-sorted so callers do
// not need to canonicalise.
func (b *Bank) UpsertLexicon(row LexiconRow) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	if row.TagA == "" || row.TagB == "" {
		return errors.New("tag_a and tag_b are required")
	}
	a, b2 := canonicalLexKey(row.TagA, row.TagB)
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_, err := b.db.Exec(`
INSERT INTO lexicon (tag_a, tag_b, cooccurrence, weight, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(tag_a, tag_b) DO UPDATE SET
    cooccurrence = excluded.cooccurrence,
    weight       = excluded.weight,
    updated_at   = excluded.updated_at`,
		a, b2, row.Cooccurrence, row.Weight, nowUTC(),
	)
	return err
}

// IncrementLexicon bumps cooccurrence by 1 (creating the row if missing).
// Used by NightlyRunner during lexicon rebuild.
func (b *Bank) IncrementLexicon(tagA, tagB string) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	if tagA == tagB || tagA == "" || tagB == "" {
		return nil // self-pairs and empties are noise
	}
	a, b2 := canonicalLexKey(tagA, tagB)
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_, err := b.db.Exec(`
INSERT INTO lexicon (tag_a, tag_b, cooccurrence, weight, updated_at)
VALUES (?, ?, 1, 0.0, ?)
ON CONFLICT(tag_a, tag_b) DO UPDATE SET
    cooccurrence = cooccurrence + 1,
    updated_at   = excluded.updated_at`,
		a, b2, nowUTC(),
	)
	return err
}

// BulkIncrementLexicon batches many IncrementLexicon calls into a single
// transaction with a prepared statement, ~100x faster than per-pair
// IncrementLexicon when rebuilding the table during Phase 3.
// pairs: each entry is a 2-element [tagA, tagB] slice.
func (b *Bank) BulkIncrementLexicon(pairs [][2]string) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	if len(pairs) == 0 {
		return nil
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	tx, err := b.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`
INSERT INTO lexicon (tag_a, tag_b, cooccurrence, weight, updated_at)
VALUES (?, ?, 1, 0.0, ?)
ON CONFLICT(tag_a, tag_b) DO UPDATE SET
    cooccurrence = cooccurrence + 1,
    updated_at   = excluded.updated_at`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	now := nowUTC()
	for _, p := range pairs {
		if p[0] == p[1] || p[0] == "" || p[1] == "" {
			continue
		}
		a, b2 := canonicalLexKey(p[0], p[1])
		if _, err := stmt.Exec(a, b2, now); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// ListLexicon returns top-N rows by weight desc; if tag != "", only rows
// where tag_a = tag OR tag_b = tag.
func (b *Bank) ListLexicon(tag string, limit int) ([]LexiconRow, error) {
	if b == nil {
		return nil, errors.New("bank not enabled")
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 5000 {
		limit = 5000
	}
	var rows *sql.Rows
	var err error
	if tag == "" {
		rows, err = b.db.Query(
			`SELECT tag_a, tag_b, cooccurrence, weight, updated_at FROM lexicon
			 ORDER BY weight DESC, cooccurrence DESC LIMIT ?`, limit)
	} else {
		rows, err = b.db.Query(
			`SELECT tag_a, tag_b, cooccurrence, weight, updated_at FROM lexicon
			 WHERE tag_a = ? OR tag_b = ?
			 ORDER BY weight DESC, cooccurrence DESC LIMIT ?`,
			tag, tag, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LexiconRow{}
	for rows.Next() {
		var r LexiconRow
		if err := rows.Scan(&r.TagA, &r.TagB, &r.Cooccurrence, &r.Weight, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TagMergeSummary is the result of MergeTags — counts only, no per-row detail.
// Suitable for the audit row's after_json and the bulk.completed WS payload.
type TagMergeSummary struct {
	From            []string `json:"from"`
	To              string   `json:"to"`
	MemoriesUpdated int      `json:"memories_updated"`
	OverridesMerged int      `json:"overrides_merged"`
	LexiconRebuilt  bool     `json:"lexicon_rebuilt"`
	Preview         []string `json:"preview,omitempty"` // first 10 affected memory ids (dry-run)
	// Wave 8a — per-source memory count keyed by the original source-tag
	// string. Surfaces as the `source_breakdown` field in the /maps/merge
	// response when the new-spec body shape is used (primary_id +
	// source_ids[]). A memory that carried two source tags counts toward
	// both — sum can exceed MemoriesUpdated.
	SourceBreakdown map[string]int `json:"source_breakdown,omitempty"`
}

// MergeTags rewrites every live memory's tag list, replacing any value in
// `from` with `to` (case-insensitive match on `from`; deduplicated on the
// resulting list). Lexicon co-occurrence pairs are recomputed for affected
// memories; map_overrides keyed by any `from` value are deleted (their
// category/alias info is folded into the `to` override if one exists, or
// the row is just removed when `to` has no override).
//
// dryRun=true returns the summary + a preview of the first 10 affected
// memory IDs without writing anything.
//
// Suppresses per-row WS events: a single bulk.completed event is emitted
// by the HTTP handler after this returns successfully. Per-row audit rows
// are NOT written under this scheme — one summary audit row covers the
// whole bulk op (see handoff Option A). If anyone needs per-memory grain
// later, they can scan memory.tags and infer affected memories from the
// audit row's timestamp.
func (b *Bank) MergeTags(from []string, to string, dryRun bool) (TagMergeSummary, error) {
	summary := TagMergeSummary{From: from, To: to}
	if b == nil {
		return summary, errors.New("bank not enabled")
	}
	if len(from) == 0 {
		return summary, errors.New("from is required (non-empty list)")
	}
	if to == "" {
		return summary, errors.New("to is required")
	}
	// Normalise from/to once. Match is case-insensitive (lower-cased) since
	// tags are stored as the user typed them.
	// Wave 8a — also remember the original (un-lowercased) form per key so
	// SourceBreakdown reports the input string the caller used, not the
	// normalised form.
	fromSet := map[string]bool{}
	fromOriginal := map[string]string{} // lower → first-seen original
	for _, f := range from {
		key := strings.ToLower(strings.TrimSpace(f))
		if key == "" {
			continue
		}
		fromSet[key] = true
		if _, ok := fromOriginal[key]; !ok {
			fromOriginal[key] = f
		}
	}
	toLower := strings.ToLower(strings.TrimSpace(to))
	if fromSet[toLower] {
		// Asking to merge a tag into itself — no-op but still valid input.
		delete(fromSet, toLower)
		if len(fromSet) == 0 {
			return summary, nil
		}
	}

	// Walk live memories. The candidate set is anything whose tags column
	// even mentions one of the `from` strings (LIKE pre-filter), then we
	// verify in Go.
	live, err := b.ListMemoriesWith(MemoryListOpts{Limit: 10000})
	if err != nil {
		return summary, err
	}

	type rewrite struct {
		id      string
		newTags []string
	}
	rewrites := []rewrite{}
	breakdown := map[string]int{} // original source string → memories that carried it
	for _, m := range live {
		hit := false
		// Track which source tags this memory carried (a memory can carry
		// multiple sources at once; each contributes to its own bucket).
		hitsHere := map[string]bool{}
		for _, t := range m.Tags {
			key := strings.ToLower(strings.TrimSpace(t))
			if fromSet[key] {
				hit = true
				hitsHere[key] = true
			}
		}
		if !hit {
			continue
		}
		for key := range hitsHere {
			breakdown[fromOriginal[key]]++
		}
		// Replace + dedupe.
		seen := map[string]bool{}
		newTags := make([]string, 0, len(m.Tags))
		for _, t := range m.Tags {
			final := t
			if fromSet[strings.ToLower(strings.TrimSpace(t))] {
				final = to
			}
			lc := strings.ToLower(strings.TrimSpace(final))
			if seen[lc] {
				continue
			}
			seen[lc] = true
			newTags = append(newTags, final)
		}
		rewrites = append(rewrites, rewrite{id: m.ID, newTags: newTags})
	}

	summary.MemoriesUpdated = len(rewrites)
	if len(breakdown) > 0 {
		summary.SourceBreakdown = breakdown
	}
	if dryRun {
		// Preview first 10 affected ids.
		preview := []string{}
		for i, r := range rewrites {
			if i >= 10 {
				break
			}
			preview = append(preview, r.id)
		}
		summary.Preview = preview
		return summary, nil
	}

	// One transaction so a partial write rolls back cleanly.
	b.wmu.Lock()
	defer b.wmu.Unlock()
	tx, err := b.db.Begin()
	if err != nil {
		return summary, err
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()

	now := nowUTC()
	stmt, err := tx.Prepare(`UPDATE memories SET tags = ?, updated_at = ? WHERE id = ?`)
	if err != nil {
		return summary, err
	}
	defer stmt.Close()
	for _, r := range rewrites {
		j, _ := json.Marshal(r.newTags)
		if _, err := stmt.Exec(string(j), now, r.id); err != nil {
			return summary, fmt.Errorf("update memory %s: %w", r.id, err)
		}
	}

	// Map overrides: collapse any from-key override into the to-key. If
	// to-key already has an override row, leave it alone (user's existing
	// classification wins). Just delete the from rows.
	for f := range fromSet {
		if _, err := tx.Exec(`DELETE FROM map_overrides WHERE LOWER(map_key) = ?`, f); err != nil {
			return summary, fmt.Errorf("delete override %s: %w", f, err)
		}
		summary.OverridesMerged++
	}

	// Rebuild lexicon for affected memories: cheapest path is a full
	// DELETE + reinsert. The bulk endpoint runs are infrequent enough that
	// the cost is acceptable, and it keeps lexicon canonical.
	if _, err := tx.Exec(`DELETE FROM lexicon`); err != nil {
		return summary, fmt.Errorf("clear lexicon: %w", err)
	}
	// Re-tally from the in-memory rewrites + the unchanged memories.
	updatedTags := map[string][]string{}
	for _, r := range rewrites {
		updatedTags[r.id] = r.newTags
	}
	lexInsert, err := tx.Prepare(`
INSERT INTO lexicon (tag_a, tag_b, cooccurrence, weight, updated_at)
VALUES (?, ?, 1, 0.0, ?)
ON CONFLICT(tag_a, tag_b) DO UPDATE SET
    cooccurrence = lexicon.cooccurrence + 1,
    updated_at   = excluded.updated_at`)
	if err != nil {
		return summary, err
	}
	defer lexInsert.Close()
	for _, m := range live {
		tags := m.Tags
		if rewritten, ok := updatedTags[m.ID]; ok {
			tags = rewritten
		}
		for i := 0; i < len(tags); i++ {
			for j := i + 1; j < len(tags); j++ {
				a, b2 := canonicalLexKey(tags[i], tags[j])
				if a == b2 || a == "" || b2 == "" {
					continue
				}
				if _, err := lexInsert.Exec(a, b2, now); err != nil {
					return summary, fmt.Errorf("lexicon insert: %w", err)
				}
			}
		}
	}
	summary.LexiconRebuilt = true

	if err := tx.Commit(); err != nil {
		return summary, err
	}
	rollback = false
	return summary, nil
}

// DeleteLexicon removes a single row.
func (b *Bank) DeleteLexicon(tagA, tagB string) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	a, b2 := canonicalLexKey(tagA, tagB)
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_, err := b.db.Exec(`DELETE FROM lexicon WHERE tag_a = ? AND tag_b = ?`, a, b2)
	return err
}

// ──────────────────────────────────────────────────────────────────────────
// Audit log
// ──────────────────────────────────────────────────────────────────────────

// AuditEntry is one row in the audit_log.
type AuditEntry struct {
	ID         string `json:"id"`
	Operation  string `json:"operation"`   // create|update|delete|merge|oracle_call|research|nightly_step
	EntityType string `json:"entity_type"` // trace|memory_map|association|lexicon|research_cache|...
	EntityID   string `json:"entity_id,omitempty"`
	BeforeJSON string `json:"before_json,omitempty"`
	AfterJSON  string `json:"after_json,omitempty"`
	Reason     string `json:"reason,omitempty"`
	AdapterID  string `json:"adapter_id,omitempty"`
	CreatedAt  string `json:"created_at"`
}

// AppendAudit writes one audit row. Generates id + timestamp if missing.
// Acquires the bank's writer lock; callers that already hold it (SaveMemory,
// UpdateMemory, etc.) should call appendAuditLocked instead.
func (b *Bank) AppendAudit(e AuditEntry) error {
	if b == nil {
		return nil // best-effort: silently skip when bank disabled
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	return b.appendAuditLocked(e)
}

// AppendAuditSilent writes the audit row to disk WITHOUT emitting an
// `audit.appended` WS event. Callers that perform a bulk operation (tag
// merge, nightly Phase 1/5/9, future bulk-soft-delete) use this for the
// per-row writes — the wire stays quiet, and the bulk operation emits one
// summary event (e.g. `bulk.completed` or `nightly.completed`) at the end.
//
// The audit row itself is still queryable via /audit?op=... — this
// endpoint suppresses noise on the WS channel only, never the disk record.
// See `CODE_HANDOFF — bulk-op WS event suppression (2026-05-10).md`.
func (b *Bank) AppendAuditSilent(e AuditEntry) error {
	if b == nil {
		return nil
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	return b.appendAuditLockedSilent(e)
}

// appendAuditLockedSilent is the lock-held variant of AppendAuditSilent.
// Same as appendAuditLocked but skips the b.emit("audit.appended", ...)
// call. Callers MUST already hold b.wmu.
func (b *Bank) appendAuditLockedSilent(e AuditEntry) error {
	if e.Operation == "" || e.EntityType == "" {
		return errors.New("operation and entity_type are required")
	}
	if e.ID == "" {
		e.ID = newID("audit")
	}
	if e.CreatedAt == "" {
		e.CreatedAt = nowUTC()
	}
	_, err := b.db.Exec(
		`INSERT INTO audit_log (id, operation, entity_type, entity_id,
		    before_json, after_json, reason, adapter_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.Operation, e.EntityType, e.EntityID,
		e.BeforeJSON, e.AfterJSON, e.Reason, e.AdapterID, e.CreatedAt,
	)
	return err
}

// appendAuditLocked is the lock-held variant of AppendAudit. Callers MUST
// already hold b.wmu. Exists so SaveMemory / UpdateMemory can write a
// `regex:<pattern_name>` audit row inline with the memory mutation without
// re-entering the (non-reentrant) wmu mutex and deadlocking.
func (b *Bank) appendAuditLocked(e AuditEntry) error {
	if e.Operation == "" || e.EntityType == "" {
		return errors.New("operation and entity_type are required")
	}
	if e.ID == "" {
		e.ID = newID("audit")
	}
	if e.CreatedAt == "" {
		e.CreatedAt = nowUTC()
	}
	_, err := b.db.Exec(
		`INSERT INTO audit_log (id, operation, entity_type, entity_id,
		    before_json, after_json, reason, adapter_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.Operation, e.EntityType, e.EntityID,
		e.BeforeJSON, e.AfterJSON, e.Reason, e.AdapterID, e.CreatedAt,
	)
	if err == nil {
		// Emit the audit.appended control-channel event. Payload is the
		// structured shape the dashboard's Recent Activity feed needs —
		// never the before/after JSON itself (those can be large and may
		// contain redacted-but-still-suspicious fragments). emit() is
		// safe to call while holding wmu — Hub.Fanout doesn't reacquire
		// the bank's lock.
		// TODO: if NightlyRunner ships and bursts thousands of audit rows
		// per minute, add a coalesce / token-bucket here.
		b.emit("audit.appended", "sd-core-bank", map[string]interface{}{
			"action":      e.Operation,
			"entity_type": e.EntityType,
			"entity_id":   e.EntityID,
			"audit_id":    e.ID,
		})
	}
	return err
}

// AuditFilter narrows ListAuditLog.
type AuditFilter struct {
	EntityType string
	EntityID   string
	Operation  string
	Since      string // created_at > since
	Limit      int
}

// ListAuditLog returns most-recent-first entries matching the filter.
func (b *Bank) ListAuditLog(f AuditFilter) ([]AuditEntry, error) {
	if b == nil {
		return nil, errors.New("bank not enabled")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	conds := []string{"1=1"}
	args := []any{}
	if f.EntityType != "" {
		conds = append(conds, "entity_type = ?")
		args = append(args, f.EntityType)
	}
	if f.EntityID != "" {
		conds = append(conds, "entity_id = ?")
		args = append(args, f.EntityID)
	}
	if f.Operation != "" {
		conds = append(conds, "operation = ?")
		args = append(args, f.Operation)
	}
	if f.Since != "" {
		conds = append(conds, "created_at > ?")
		args = append(args, f.Since)
	}
	args = append(args, limit)
	q := `SELECT id, operation, entity_type, entity_id, before_json, after_json,
	             reason, adapter_id, created_at
	      FROM audit_log WHERE ` + strings.Join(conds, " AND ") + `
	      ORDER BY created_at DESC LIMIT ?`
	rows, err := b.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.Operation, &e.EntityType, &e.EntityID,
			&e.BeforeJSON, &e.AfterJSON, &e.Reason, &e.AdapterID, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ──────────────────────────────────────────────────────────────────────────
// Research cache
// ──────────────────────────────────────────────────────────────────────────

// ResearchCacheEntry is one cached topic lookup.
type ResearchCacheEntry struct {
	Topic     string `json:"topic"`
	MapID     string `json:"map_id,omitempty"`
	Payload   string `json:"payload,omitempty"` // JSON blob
	FetchedAt string `json:"fetched_at"`
	ExpiresAt string `json:"expires_at"`
	// FetchedBy distinguishes user-triggered ("manual") from
	// NightlyRunner / system-triggered ("auto") cache entries.
	// Default: "manual" — only POST /research currently writes
	// these, and that's user-initiated.
	FetchedBy string `json:"fetched_by,omitempty"`
}

// SaveResearchCache upserts an entry by topic.
func (b *Bank) SaveResearchCache(e ResearchCacheEntry) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	if e.Topic == "" {
		return errors.New("topic is required")
	}
	if e.FetchedAt == "" {
		e.FetchedAt = nowUTC()
	}
	if e.ExpiresAt == "" {
		return errors.New("expires_at is required")
	}
	if e.FetchedBy == "" {
		e.FetchedBy = "manual"
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_, err := b.db.Exec(`
INSERT INTO research_cache (topic, map_id, payload, fetched_at, expires_at, fetched_by)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(topic) DO UPDATE SET
    map_id     = excluded.map_id,
    payload    = excluded.payload,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    fetched_by = excluded.fetched_by`,
		e.Topic, e.MapID, e.Payload, e.FetchedAt, e.ExpiresAt, e.FetchedBy,
	)
	if err == nil {
		// Try to extract a `query` field from the payload for the
		// dashboard's live-update display. Optional — the dashboard can
		// always re-fetch via GET /research/{id} for the full record.
		query := ""
		if e.Payload != "" {
			var probe struct {
				Query string `json:"query"`
			}
			_ = json.Unmarshal([]byte(e.Payload), &probe)
			query = probe.Query
		}
		payload := map[string]interface{}{"entry_id": e.Topic}
		if query != "" {
			payload["query"] = query
		}
		b.emit("research.cached", "sd-core-research", payload)
	}
	return err
}

// GetResearchCache returns a single entry. Returns sql.ErrNoRows if missing.
func (b *Bank) GetResearchCache(topic string) (ResearchCacheEntry, error) {
	if b == nil {
		return ResearchCacheEntry{}, errors.New("bank not enabled")
	}
	row := b.db.QueryRow(
		`SELECT topic, map_id, payload, fetched_at, expires_at, fetched_by FROM research_cache
		 WHERE topic = ?`, topic,
	)
	var e ResearchCacheEntry
	err := row.Scan(&e.Topic, &e.MapID, &e.Payload, &e.FetchedAt, &e.ExpiresAt, &e.FetchedBy)
	return e, err
}

// ListResearchCache returns up to limit entries, newest first. When
// includeExpired is false, rows whose expires_at is in the past are
// filtered out (the cache lookup path already treats those as misses,
// so surfacing them in the list view was just visual noise).
func (b *Bank) ListResearchCache(limit int, includeExpired bool) ([]ResearchCacheEntry, error) {
	if b == nil {
		return nil, errors.New("bank not enabled")
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	q := `SELECT topic, map_id, payload, fetched_at, expires_at, fetched_by FROM research_cache`
	var args []any
	if !includeExpired {
		// expires_at='' was historically used as "never expires"; keep
		// those visible. Only filter rows with a concrete past timestamp.
		q += ` WHERE expires_at = '' OR expires_at >= ?`
		args = append(args, nowUTC())
	}
	q += ` ORDER BY fetched_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := b.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ResearchCacheEntry{}
	for rows.Next() {
		var e ResearchCacheEntry
		if err := rows.Scan(&e.Topic, &e.MapID, &e.Payload, &e.FetchedAt, &e.ExpiresAt, &e.FetchedBy); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteResearchCache removes a single entry.
func (b *Bank) DeleteResearchCache(topic string) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	res, err := b.db.Exec(`DELETE FROM research_cache WHERE topic = ?`, topic)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// PurgeExpiredResearchCache removes entries whose expires_at is in the past.
// Returns the number of rows removed.
func (b *Bank) PurgeExpiredResearchCache() (int64, error) {
	if b == nil {
		return 0, errors.New("bank not enabled")
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	res, err := b.db.Exec(`DELETE FROM research_cache WHERE expires_at < ?`, nowUTC())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ──────────────────────────────────────────────────────────────────────────
// Token budget (Oracle daily spend)
// ──────────────────────────────────────────────────────────────────────────

// validBudgetTiers is the closed set of tier names accepted by V2.
// Anything else is normalised to "unknown" so a caller bug doesn't silently
// pollute the by_tier breakdown with arbitrary strings.
var validBudgetTiers = map[string]bool{
	string(TierEmbedding): true,
	string(Tier1):         true,
	string(Tier2):         true,
	string(Tier3):         true,
	"unknown":             true,
}

// validBudgetProviders is the closed set of provider names. Same reasoning
// as validBudgetTiers — sanitise on write so the `external` derivation in
// providerIsExternal() is sound.
var validBudgetProviders = map[string]bool{
	string(ProviderOllama):    true,
	string(ProviderOpenAI):    true,
	string(ProviderAnthropic): true,
	string(ProviderCustom):    true,
	string(ProviderDisabled):  true,
	string(ProviderAirLLM):    true, // local; cost_usd: 0
	"unknown":                 true,
}

// providerIsExternal reports whether spend on this provider left the box
// (i.e., cost real money). Local Ollama, AirLLM, and disabled all stay home.
// Source-of-truth for the `external` column on token_budget_lines.
func providerIsExternal(provider string) bool {
	switch provider {
	case "ollama", "airllm", "disabled", "unknown", "":
		return false
	default:
		return true // openai, anthropic, custom — all paid
	}
}

// AddTokensUsedV2 records token spend with full per-tier / per-provider /
// per-model attribution. The `external` parameter is reserved (matches the
// handoff signature) but is overridden by the provider-derived value as a
// safety net — we never trust caller-supplied externality.
//
// `model` is the specific model name within the provider (e.g. "llama3.1:8b"
// for ollama, "meta-llama/Llama-3.1-70B-Instruct" for airllm, "gpt-4o-mini"
// for openai). When non-empty, lets the dashboard's token chip break down
// spend by model on Tier 2/3 — necessary now that hot-swap means a single
// day can have multiple models contributing under the same provider kind.
// Empty string is the back-compat bucket for legacy callers.
//
// Tier and provider are sanitised against fixed enums; unknowns are coerced
// to "unknown" rather than rejected, so a misconfigured call site still
// gets the spend recorded (just without precise attribution) instead of
// silently failing. Model has no allow-list — anything the caller passes is
// stored verbatim (so future provider kinds we haven't enumerated yet still
// get per-model rows).
//
// Two writes per call:
//   1. token_budget_lines (the new per-tier row, upsert-on-conflict).
//   2. token_budget (legacy aggregate, so the dashboard's daily-bar chart
//      keeps working without changes).
func (b *Bank) AddTokensUsedV2(date, tier, provider, model string, external bool, tokensIn, tokensOut int) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}
	if tier == "" || !validBudgetTiers[tier] {
		tier = "unknown"
	}
	if provider == "" || !validBudgetProviders[provider] {
		provider = "unknown"
	}
	// Override `external` with the provider-derived truth — caller can ask
	// for anything but we record what's actually externally billable.
	external = providerIsExternal(provider)
	if tokensIn < 0 {
		tokensIn = 0
	}
	if tokensOut < 0 {
		tokensOut = 0
	}
	total := tokensIn + tokensOut
	if total == 0 {
		return nil // nothing to record — saves write on no-op narratives
	}
	now := nowUTC()
	b.wmu.Lock()
	defer b.wmu.Unlock()
	if _, err := b.db.Exec(`
INSERT INTO token_budget_lines (date, tier, provider, model, external, tokens_in, tokens_out, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(date, tier, provider, model) DO UPDATE SET
    external   = excluded.external,
    tokens_in  = token_budget_lines.tokens_in  + excluded.tokens_in,
    tokens_out = token_budget_lines.tokens_out + excluded.tokens_out,
    updated_at = excluded.updated_at`,
		date, tier, provider, model, boolToInt(external), tokensIn, tokensOut, now,
	); err != nil {
		return err
	}
	// Back-compat: keep token_budget.tokens_used summed across all tiers so
	// the existing dashboard chart needs zero changes.
	_, err := b.db.Exec(`
INSERT INTO token_budget (date, tokens_used, updated_at)
VALUES (?, ?, ?)
ON CONFLICT(date) DO UPDATE SET
    tokens_used = tokens_used + ?,
    updated_at  = excluded.updated_at`,
		date, total, now, total,
	)
	return err
}

// AddTokensUsed is the legacy single-aggregate entry point. Delegates to
// AddTokensUsedV2 with tier="unknown" and provider="unknown" so older code
// paths keep working — the spend lands in both token_budget (as before)
// and a `tier=unknown,provider=unknown` row in token_budget_lines.
//
// New call sites should prefer AddTokensUsedV2 for proper attribution.
func (b *Bank) AddTokensUsed(date string, n int) error {
	return b.AddTokensUsedV2(date, "unknown", "unknown", "", false, n, 0)
}

// GetTokensUsed returns the count for a given date (0 if missing).
func (b *Bank) GetTokensUsed(date string) (int, error) {
	if b == nil {
		return 0, errors.New("bank not enabled")
	}
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}
	var n int
	err := b.db.QueryRow(
		`SELECT tokens_used FROM token_budget WHERE date = ?`, date,
	).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// TokenBudgetRow is the per-day spend.
type TokenBudgetRow struct {
	Date       string `json:"date"`
	TokensUsed int    `json:"tokens_used"`
	UpdatedAt  string `json:"updated_at"`
}

// ListTokenBudget returns the most recent N days, newest first.
func (b *Bank) ListTokenBudget(days int) ([]TokenBudgetRow, error) {
	if b == nil {
		return nil, errors.New("bank not enabled")
	}
	if days <= 0 {
		days = 30
	}
	rows, err := b.db.Query(
		`SELECT date, tokens_used, updated_at FROM token_budget
		 ORDER BY date DESC LIMIT ?`, days,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TokenBudgetRow{}
	for rows.Next() {
		var r TokenBudgetRow
		if err := rows.Scan(&r.Date, &r.TokensUsed, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TokenSpendByTier is one row in the by_tier breakdown returned to the
// dashboard. Provider/External reflect the most-recent attribution within
// the requested window — so when a user swaps Tier 3 from openai to
// anthropic, the next refresh shows anthropic even if older openai-spend
// is still in the totals.
//
// `Models` carries the per-model sub-aggregation under this tier — a
// post-hot-swap world where Tier 2 might have spend on both Ollama 8B and
// AirLLM 70B in the same window. Sorted descending by tokens_total for
// natural UI rendering. Empty when only legacy (pre-2026-05-12) rows
// exist for this tier (those have model=='').
type TokenSpendByTier struct {
	Provider    string             `json:"provider"`
	External    bool               `json:"external"`
	TokensIn    int                `json:"tokens_in"`
	TokensOut   int                `json:"tokens_out"`
	TokensTotal int                `json:"tokens_total"`
	Models      []TokenSpendByModel `json:"models,omitempty"`
}

// TokenSpendByModel is one model's contribution to a tier's total spend.
// `Name` is the provider's Name() (e.g. "ollama:llama3.1:8b") — empty
// string for legacy rows pre-dating the model column.
type TokenSpendByModel struct {
	Name        string `json:"name"`
	Provider    string `json:"provider"`
	External    bool   `json:"external"`
	TokensIn    int    `json:"tokens_in"`
	TokensOut   int    `json:"tokens_out"`
	TokensTotal int    `json:"tokens_total"`
}

// ListTokenBudgetByTier aggregates spend across the last `days` calendar
// days, grouped by tier. Returns all four canonical tiers (plus "unknown"
// when legacy AddTokensUsed wrote to it) — zero-spend tiers come back with
// zero totals and the most-recent provider attribution from settings.
//
// externalOnly=true filters lines to provider-derived external=1 so the
// "paid only" chip view aggregates cleanly without local Ollama noise.
func (b *Bank) ListTokenBudgetByTier(days int, externalOnly bool) (map[string]TokenSpendByTier, error) {
	if b == nil {
		return nil, errors.New("bank not enabled")
	}
	if days <= 0 {
		days = 30
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days+1).Format("2006-01-02")

	conds := "date >= ?"
	args := []any{cutoff}
	if externalOnly {
		conds += " AND external = 1"
	}

	// Aggregate sums per tier.
	rows, err := b.db.Query(
		`SELECT tier, COALESCE(SUM(tokens_in), 0), COALESCE(SUM(tokens_out), 0)
		 FROM token_budget_lines WHERE `+conds+`
		 GROUP BY tier`, args...)
	if err != nil {
		return nil, err
	}
	out := map[string]TokenSpendByTier{}
	for rows.Next() {
		var tier string
		var in, outTokens int
		if err := rows.Scan(&tier, &in, &outTokens); err != nil {
			rows.Close()
			return nil, err
		}
		out[tier] = TokenSpendByTier{
			TokensIn: in, TokensOut: outTokens, TokensTotal: in + outTokens,
		}
	}
	rows.Close()

	// For each tier, set Provider/External to the most-recent line within
	// the window. Tiers with no spend get whatever's configured in settings
	// (so the dashboard's per-tier row still has a sensible label).
	for _, tier := range []string{
		string(TierEmbedding), string(Tier1), string(Tier2), string(Tier3),
	} {
		entry := out[tier] // zero value if not yet present
		var provider string
		var ext int
		err := b.db.QueryRow(
			`SELECT provider, external FROM token_budget_lines
			 WHERE tier = ? AND date >= ?
			 ORDER BY updated_at DESC LIMIT 1`, tier, cutoff,
		).Scan(&provider, &ext)
		if errors.Is(err, sql.ErrNoRows) {
			// No spend in window — fall back to the configured provider.
			cfg, _ := b.GetProviderConfig(TierKey(tier))
			provider = string(cfg.Kind)
			if provider == "" {
				provider = "disabled"
			}
		} else if err != nil {
			return nil, err
		}
		// `ext` (1/0) is the persisted external bit from the most-recent
		// row — kept as documentation, but the response's External field
		// derives from provider so the closed-form rule is the source of
		// truth (defends against a stale row written before a provider
		// kind got reclassified).
		_ = ext
		entry.Provider = provider
		entry.External = providerIsExternal(provider)
		out[tier] = entry
	}

	// Per-model sub-aggregation. The bubble's expand-on-click feature shows
	// every model that contributed under a tier so users can audit their
	// hot-swap history at a glance ("how much did the 70B run cost vs the
	// 8B fallback?"). Models with empty name '' are legacy pre-migration
	// rows — keep them in the response as the "(unknown model)" bucket so
	// older totals still reconcile to the tier-level number.
	modelRows, err := b.db.Query(
		`SELECT tier, model, provider, COALESCE(SUM(tokens_in), 0), COALESCE(SUM(tokens_out), 0)
		 FROM token_budget_lines WHERE `+conds+`
		 GROUP BY tier, model, provider
		 ORDER BY tier, (COALESCE(SUM(tokens_in), 0) + COALESCE(SUM(tokens_out), 0)) DESC`,
		args...)
	if err != nil {
		return nil, err
	}
	defer modelRows.Close()
	for modelRows.Next() {
		var tier, model, provider string
		var in, outTokens int
		if err := modelRows.Scan(&tier, &model, &provider, &in, &outTokens); err != nil {
			return nil, err
		}
		entry := out[tier]
		entry.Models = append(entry.Models, TokenSpendByModel{
			Name:        model,
			Provider:    provider,
			External:    providerIsExternal(provider),
			TokensIn:    in,
			TokensOut:   outTokens,
			TokensTotal: in + outTokens,
		})
		out[tier] = entry
	}
	return out, nil
}

// GetExternalTokenTotal sums tokens across the window for provider-derived
// external=1 lines only. The dashboard's chip headline reads from this so
// the user sees "what cost real money" without local Ollama noise.
func (b *Bank) GetExternalTokenTotal(days int) (in, out, total int, err error) {
	if b == nil {
		return 0, 0, 0, errors.New("bank not enabled")
	}
	if days <= 0 {
		days = 30
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days+1).Format("2006-01-02")
	err = b.db.QueryRow(
		`SELECT COALESCE(SUM(tokens_in), 0), COALESCE(SUM(tokens_out), 0)
		 FROM token_budget_lines WHERE date >= ? AND external = 1`, cutoff,
	).Scan(&in, &out)
	if err != nil {
		return 0, 0, 0, err
	}
	return in, out, in + out, nil
}

// ──────────────────────────────────────────────────────────────────────────
// Nightly runs (Dream Journal data source)
// ──────────────────────────────────────────────────────────────────────────

// NightlyRun is one row in `nightly_runs` — the storage for one Dream Cycle.
type NightlyRun struct {
	ID         string `json:"id"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
	Status     string `json:"status"`
	StatsJSON  string `json:"-"`                   // raw column; the handler unwraps to `stats`
	Narrative  string `json:"narrative,omitempty"` // omitempty so dashboard doesn't render `null`
	Model      string `json:"model,omitempty"`
	Error      string `json:"error,omitempty"`
	// Dream pipeline Phase 12: literary prose entry, distinct from the
	// observational `narrative`. Frontend renders this as the lead content
	// in the Dream Journal card.
	DreamEntry          string `json:"dream_entry,omitempty"`
	DreamEntryArchetype string `json:"dream_entry_archetype,omitempty"`
	// Crash recovery (handoff "dream cycle resumption + crash recovery
	// 2026-05-10"). HeartbeatAt is bumped every 30s during a live run; if
	// it goes stale beyond ReconcileStaleInProgress's threshold, the row
	// is flipped to status='interrupted' with InterruptReason recording why.
	HeartbeatAt     string `json:"heartbeat_at,omitempty"`
	InterruptReason string `json:"interrupt_reason,omitempty"`
	CreatedAt       string `json:"created_at"`
}

// NightlyRunListOpts filters ListNightlyRuns.
type NightlyRunListOpts struct {
	Limit  int
	Since  string // started_at >= since (caller passes the cutoff timestamp)
	Status string // empty = all; otherwise exact match on status column
}

// InsertNightlyRun creates an `in_progress` row at the start of a Dream Cycle.
// Caller is responsible for the id format ("nightly-YYYY-MM-DD-HH-MM"); see
// NightlyRunner.runID().
func (b *Bank) InsertNightlyRun(rec NightlyRun) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	if rec.ID == "" || rec.StartedAt == "" || rec.Status == "" {
		return errors.New("id, started_at, and status are required")
	}
	if rec.CreatedAt == "" {
		rec.CreatedAt = nowUTC()
	}
	if rec.StatsJSON == "" {
		rec.StatsJSON = "{}"
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_, err := b.db.Exec(`
INSERT INTO nightly_runs (id, started_at, finished_at, status, stats_json, narrative, model, error,
    heartbeat_at, interrupt_reason, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.StartedAt, rec.FinishedAt, rec.Status,
		rec.StatsJSON, rec.Narrative, rec.Model, rec.Error,
		rec.HeartbeatAt, rec.InterruptReason, rec.CreatedAt,
	)
	return err
}

// UpdateNightlyRun completes the run row. Status transitions in_progress →
// completed | failed | partial. Pass empty errMsg for non-failed runs.
//
// Note: dream_entry / dream_entry_archetype are written by SetNightlyDreamEntry
// rather than this generic update so the pipeline's Phase 12 can land its
// output AFTER the run has already been marked completed (Phase 12 is one of
// the last things that runs, and a Tier 2 hiccup there should not flip the
// whole run to failed).
func (b *Bank) UpdateNightlyRun(id, finishedAt, status, statsJSON, narrative, model, errMsg string) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	if id == "" || status == "" {
		return errors.New("id and status are required")
	}
	if statsJSON == "" {
		statsJSON = "{}"
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	res, err := b.db.Exec(`
UPDATE nightly_runs SET
    finished_at = ?,
    status      = ?,
    stats_json  = ?,
    narrative   = ?,
    model       = ?,
    error       = ?
WHERE id = ?`,
		finishedAt, status, statsJSON, narrative, model, errMsg, id,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteNightlyRun hard-deletes a nightly_runs row by id. Returns
// sql.ErrNoRows when no such id existed. Caller is responsible for the
// audit row (so before_json can capture the deleted state with redaction
// applied where appropriate) and for the WS event emission.
//
// IMPORTANT: this only removes the journal row. Memories created during
// the run (synthesis, schema, replay, augmented) are kept; cross_region_links
// rows referencing run_id are kept; per-phase audit rows are kept. The
// Dream Journal × is purely a UX cleanup, not a destructive data op.
// See `CODE_HANDOFF — DELETE nightly runs (2026-05-10).md`.
func (b *Bank) DeleteNightlyRun(id string) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	if id == "" {
		return errors.New("id is required")
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	res, err := b.db.Exec(`DELETE FROM nightly_runs WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// BumpNightlyHeartbeat stamps heartbeat_at = now on the given run.
// Called every 30s from the runner's heartbeat goroutine + once at run
// start. Best-effort: failure here doesn't crash the runner.
//
// (Per handoff "dream cycle resumption + crash recovery 2026-05-10".)
func (b *Bank) BumpNightlyHeartbeat(id string) error {
	if b == nil || id == "" {
		return nil
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_, err := b.db.Exec(
		`UPDATE nightly_runs SET heartbeat_at = ? WHERE id = ? AND status = 'in_progress'`,
		nowUTC(), id,
	)
	return err
}

// ReconcileStaleInProgress walks every nightly_runs row with
// status='in_progress' and flips stale ones to status='interrupted'.
// Stale = heartbeat_at is empty (pre-heartbeat row from before this
// feature shipped) OR older than now - staleAfter.
//
// Returns the number of rows reconciled. Called once at SD Core boot
// and once at the start of each new DoRun (post-TryLock pre-INSERT) so
// orphan rows can't accumulate.
//
// (Per handoff "dream cycle resumption + crash recovery 2026-05-10".)
func (b *Bank) ReconcileStaleInProgress(staleAfter time.Duration) (int, error) {
	if b == nil {
		return 0, errors.New("bank not enabled")
	}
	cutoff := time.Now().UTC().Add(-staleAfter).Format(time.RFC3339Nano)
	now := nowUTC()
	b.wmu.Lock()
	defer b.wmu.Unlock()
	// heartbeat_at can be NULL (rows created before the heartbeat column
	// was populated, or rows where BumpNightlyHeartbeat hasn't fired
	// yet because the runner stopped between phase boundaries). SQLite
	// returns NULL — not true/false — for `NULL = ''` and `NULL < x`,
	// so the OR branch evaluates to NULL → false and the row is left
	// stuck as in_progress forever. COALESCE(heartbeat_at, '') folds
	// NULL into the empty-string case, which matches the intent: a
	// missing heartbeat is "infinitely stale, reconcile it."
	res, err := b.db.Exec(`
UPDATE nightly_runs
SET status = 'interrupted',
    finished_at = ?,
    interrupt_reason = 'process_restart',
    error = COALESCE(NULLIF(error, ''), 'interrupted before completion')
WHERE status = 'in_progress'
  AND (COALESCE(heartbeat_at, '') = '' OR heartbeat_at < ?)`,
		now, cutoff,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// SetNightlyDreamEntry writes the Phase 12 dream_entry + archetype on an
// already-completed run row. Called by NightlyRunner after the rest of the
// run has landed so a Tier 2 failure here doesn't taint the run status.
func (b *Bank) SetNightlyDreamEntry(id, entry, archetype string) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	if id == "" {
		return errors.New("id is required")
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_, err := b.db.Exec(`
UPDATE nightly_runs SET
    dream_entry           = ?,
    dream_entry_archetype = ?
WHERE id = ?`, entry, archetype, id,
	)
	return err
}

// GetNightlyRun fetches one run by id.
func (b *Bank) GetNightlyRun(id string) (NightlyRun, error) {
	if b == nil {
		return NightlyRun{}, errors.New("bank not enabled")
	}
	row := b.db.QueryRow(
		`SELECT id, started_at, finished_at, status, stats_json, narrative, model, error,
		        dream_entry, dream_entry_archetype, heartbeat_at, interrupt_reason, created_at
		 FROM nightly_runs WHERE id = ?`, id,
	)
	var r NightlyRun
	err := row.Scan(&r.ID, &r.StartedAt, &r.FinishedAt, &r.Status,
		&r.StatsJSON, &r.Narrative, &r.Model, &r.Error,
		&r.DreamEntry, &r.DreamEntryArchetype,
		&r.HeartbeatAt, &r.InterruptReason, &r.CreatedAt)
	return r, err
}

// ListNightlyRuns returns runs newest-first (by started_at), filtered by opts.
func (b *Bank) ListNightlyRuns(opts NightlyRunListOpts) ([]NightlyRun, error) {
	if b == nil {
		return nil, errors.New("bank not enabled")
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	conds := []string{"1=1"}
	args := []any{}
	if opts.Since != "" {
		conds = append(conds, "started_at >= ?")
		args = append(args, opts.Since)
	}
	if opts.Status != "" {
		conds = append(conds, "status = ?")
		args = append(args, opts.Status)
	}
	args = append(args, limit)
	q := `SELECT id, started_at, finished_at, status, stats_json, narrative, model, error,
	             dream_entry, dream_entry_archetype, heartbeat_at, interrupt_reason, created_at
	      FROM nightly_runs WHERE ` + strings.Join(conds, " AND ") + `
	      ORDER BY started_at DESC LIMIT ?`
	rows, err := b.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NightlyRun{}
	for rows.Next() {
		var r NightlyRun
		if err := rows.Scan(&r.ID, &r.StartedAt, &r.FinishedAt, &r.Status,
			&r.StatsJSON, &r.Narrative, &r.Model, &r.Error,
			&r.DreamEntry, &r.DreamEntryArchetype,
			&r.HeartbeatAt, &r.InterruptReason, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}


// ── Iter 25 (2026-05-14) — optimized_text + map embeddings ────────────
// Narrow accessors that update / read the new columns without forcing
// the columns into the MemoryRecord / MemoryMap structs or every
// INSERT/SELECT in the bank. Reduces blast radius of the change.

// SaveOptimizedText writes the fact-dense compression Phase 0b produced
// for a memory. Idempotent; safe to call on a missing memory_id (no-op).
func (b *Bank) SaveOptimizedText(memID, optimized string) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	if memID == "" {
		return nil
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_, err := b.db.Exec(
		`UPDATE memories SET optimized_text = ? WHERE id = ?`,
		optimized, memID,
	)
	return err
}

// GetOptimizedTextBatch returns a map[memID]optimized_text for the given
// IDs. Missing memories simply absent from the map. Used by /reflect to
// fold optimized texts alongside raw + enriched in the synthesis prompt.
func (b *Bank) GetOptimizedTextBatch(ids []string) (map[string]string, error) {
	out := map[string]string{}
	if b == nil || len(ids) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	q := `SELECT id, COALESCE(optimized_text, '') FROM memories WHERE id IN (` +
		strings.Join(placeholders, ",") + `)`
	rows, err := b.db.Query(q, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, ot string
		if err := rows.Scan(&id, &ot); err != nil {
			return out, err
		}
		if ot != "" {
			out[id] = ot
		}
	}
	return out, rows.Err()
}

// SaveMapEmbedding writes the (vector, model) pair for a memory_map.
// Vector is JSON-encoded the same way the `embeddings` table stores
// memory vectors. Empty vector clears the embedding (next Phase 4
// re-computes).
func (b *Bank) SaveMapEmbedding(mapID string, vec []float32, model string) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	if mapID == "" {
		return nil
	}
	encoded := ""
	if len(vec) > 0 {
		blob, err := json.Marshal(vec)
		if err != nil {
			return err
		}
		encoded = string(blob)
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_, err := b.db.Exec(
		`UPDATE memory_maps SET embedding = ?, embedding_model = ?, updated_at = ?
		 WHERE id = ?`,
		encoded, model, time.Now().UTC().Format(time.RFC3339Nano), mapID,
	)
	return err
}

// LoadMapEmbeddings returns map[map_id]vector for every non-archived map
// whose embedding_model matches the requested model. Used by
// rankCandidates to cosine-rank maps against the query.
//
// Pass `model=""` to load ALL embeddings regardless of model (legacy +
// migration cases). Returned map only contains entries that successfully
// decoded.
func (b *Bank) LoadMapEmbeddings(model string) (map[string][]float32, error) {
	out := map[string][]float32{}
	if b == nil {
		return out, errors.New("bank not enabled")
	}
	var rows *sql.Rows
	var err error
	if model == "" {
		rows, err = b.db.Query(
			`SELECT id, embedding FROM memory_maps WHERE archived = 0 AND embedding != ''`,
		)
	} else {
		rows, err = b.db.Query(
			`SELECT id, embedding FROM memory_maps WHERE archived = 0 AND embedding != '' AND embedding_model = ?`,
			model,
		)
	}
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, enc string
		if err := rows.Scan(&id, &enc); err != nil {
			return out, err
		}
		var v []float32
		if err := json.Unmarshal([]byte(enc), &v); err != nil {
			continue
		}
		out[id] = v
	}
	return out, rows.Err()
}
