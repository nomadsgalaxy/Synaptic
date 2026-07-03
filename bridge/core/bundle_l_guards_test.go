// bundle_l_guards_test.go — guards for v2.7 Bundle L (Dormant Memories).
//
// Bundle L pivots the original "hard TTL" idea into a brain-style
// dormancy model: a memory whose expires_at fires moves to
// dormant_at != '' and stays in the bank. Default /recall hides
// dormant rows (same shape as the R10 supersede filter); the dashboard
// or an agent can pass include_dormant=true to wake them.
//
// Coverage:
//   - Schema additions: expires_at / dormant_at / dormant_reason columns
//     migrate on legacy banks
//   - UpdateMemory honours patch.ExpiresAt round-trip
//   - UpdateMemory rejects setting a TTL on a sensitive memory (sticky-on)
//   - UpdateMemory clearing TTL (passing &"") works even on sensitive rows
//   - UpdateMemory manual dormancy: patch.Dormant=&true / &false round-trips
//   - SaveMemory silently drops expires_at on sensitive rows (adapter
//     entrypoint policy)
//   - DormantIDs returns the set of dormant memory IDs (excludes deleted)
//   - RunDormancyPass writes dormant_at on expired rows, skips sensitive,
//     skips already-dormant, skips soft-deleted, is idempotent
//   - RunDormancyPass writes an audit row per memory put to sleep
package main

import (
	"testing"
	"time"
)

func ptrStr(s string) *string { return &s }
func ptrBool(b bool) *bool    { return &b }

// ── Schema migration ────────────────────────────────────────────────────

func TestBundleL_ColumnsExist(t *testing.T) {
	bank := newTestBank(t)
	// PRAGMA reflects the migrated table. The Read-side check (scanRow
	// not erroring on SELECT memoryColumns) is implicitly covered by
	// every other test that calls GetMemory/ListMemories.
	want := []string{"expires_at", "dormant_at", "dormant_reason"}
	for _, col := range want {
		var name string
		row := bank.db.QueryRow(
			`SELECT name FROM pragma_table_info('memories') WHERE name = ?`, col)
		if err := row.Scan(&name); err != nil {
			t.Errorf("expected column %q on memories, missing: %v", col, err)
		}
	}
}

// ── Update round-trip ───────────────────────────────────────────────────

func TestBundleL_UpdateMemory_ExpiresAt_Roundtrip(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "memory with future TTL"})
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	updated, err := bank.UpdateMemory(rec.ID, MemoryUpdate{ExpiresAt: ptrStr(future)})
	if err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	if updated.ExpiresAt != future {
		t.Errorf("expected expires_at=%q, got %q", future, updated.ExpiresAt)
	}
	// Clear it.
	cleared, err := bank.UpdateMemory(rec.ID, MemoryUpdate{ExpiresAt: ptrStr("")})
	if err != nil {
		t.Fatalf("clear TTL: %v", err)
	}
	if cleared.ExpiresAt != "" {
		t.Errorf("expected expires_at cleared, got %q", cleared.ExpiresAt)
	}
}

func TestBundleL_UpdateMemory_RejectsTTLOnSensitive(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "sensitive memory", Sensitive: true})
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	_, err := bank.UpdateMemory(rec.ID, MemoryUpdate{ExpiresAt: ptrStr(future)})
	if err == nil {
		t.Errorf("expected error setting TTL on sensitive memory, got nil")
	}
}

func TestBundleL_UpdateMemory_AllowsClearingTTLOnSensitive(t *testing.T) {
	bank := newTestBank(t)
	// Insert directly with TTL pre-set (simulating a legacy row that pre-
	// dates the sticky-on policy).
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "legacy with TTL"})
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	bank.db.Exec(`UPDATE memories SET expires_at = ?, sensitive = 1 WHERE id = ?`, future, rec.ID)
	// Clearing should succeed regardless of sensitive=1.
	cleared, err := bank.UpdateMemory(rec.ID, MemoryUpdate{ExpiresAt: ptrStr("")})
	if err != nil {
		t.Fatalf("clearing TTL on sensitive should succeed, got %v", err)
	}
	if cleared.ExpiresAt != "" {
		t.Errorf("expected cleared TTL, got %q", cleared.ExpiresAt)
	}
}

func TestBundleL_UpdateMemory_ManualDormancyToggle(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "wakeable memory"})

	// Put to sleep.
	asleep, err := bank.UpdateMemory(rec.ID, MemoryUpdate{Dormant: ptrBool(true)})
	if err != nil {
		t.Fatalf("manual dormant=true: %v", err)
	}
	if asleep.DormantAt == "" {
		t.Errorf("expected dormant_at populated after Dormant=&true, got empty")
	}
	if asleep.DormantReason != "manual" {
		t.Errorf("expected dormant_reason=manual, got %q", asleep.DormantReason)
	}

	// Wake up.
	awake, err := bank.UpdateMemory(rec.ID, MemoryUpdate{Dormant: ptrBool(false)})
	if err != nil {
		t.Fatalf("manual dormant=false: %v", err)
	}
	if awake.DormantAt != "" {
		t.Errorf("expected dormant_at cleared after Dormant=&false, got %q", awake.DormantAt)
	}
}

// ── SaveMemory drops TTL on sensitive ──────────────────────────────────

func TestBundleL_SaveMemory_DropsTTLOnSensitive(t *testing.T) {
	bank := newTestBank(t)
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	rec, err := bank.SaveMemory(MemoryRecord{
		Text:      "sensitive at insert",
		Sensitive: true,
		ExpiresAt: future,
	})
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}
	if rec.ExpiresAt != "" {
		t.Errorf("expected expires_at dropped on sensitive insert, got %q", rec.ExpiresAt)
	}
}

// ── DormantIDs ──────────────────────────────────────────────────────────

func TestBundleL_DormantIDs_ReturnsOnlyDormantRows(t *testing.T) {
	bank := newTestBank(t)
	a, _ := bank.SaveMemory(MemoryRecord{Text: "awake"})
	b, _ := bank.SaveMemory(MemoryRecord{Text: "sleeping"})
	c, _ := bank.SaveMemory(MemoryRecord{Text: "also sleeping but soft-deleted"})
	// Mark b dormant, c dormant + soft-deleted.
	bank.UpdateMemory(b.ID, MemoryUpdate{Dormant: ptrBool(true)})
	bank.UpdateMemory(c.ID, MemoryUpdate{Dormant: ptrBool(true)})
	bank.SoftDeleteMemory(c.ID)

	ids, err := bank.DormantIDs()
	if err != nil {
		t.Fatalf("DormantIDs: %v", err)
	}
	if _, present := ids[a.ID]; present {
		t.Errorf("awake memory should NOT be in dormant set")
	}
	if _, present := ids[b.ID]; !present {
		t.Errorf("expected dormant memory %q in set", b.ID)
	}
	if _, present := ids[c.ID]; present {
		t.Errorf("soft-deleted dormant memory should be filtered, got it in set")
	}
}

// ── RunDormancyPass ─────────────────────────────────────────────────────

func TestBundleL_RunDormancyPass_PutsExpiredToSleep(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "should sleep"})
	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	// Set TTL in the past via direct UPDATE (the patch path rejects past
	// timestamps only for sensitive — we want to test the pass itself).
	bank.db.Exec(`UPDATE memories SET expires_at = ? WHERE id = ?`, past, rec.ID)

	res, err := bank.RunDormancyPass()
	if err != nil {
		t.Fatalf("RunDormancyPass: %v", err)
	}
	if res.Slept != 1 {
		t.Errorf("expected 1 memory put to sleep, got %d (scanned %d, skipped %d)",
			res.Slept, res.Scanned, res.Skipped)
	}
	got, _ := bank.GetMemory(rec.ID)
	if got.DormantAt == "" {
		t.Errorf("expected dormant_at populated, got empty")
	}
	if got.DormantReason != "ttl" {
		t.Errorf("expected dormant_reason=ttl, got %q", got.DormantReason)
	}
}

func TestBundleL_RunDormancyPass_SkipsSensitive(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "sensitive legacy with TTL"})
	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	bank.db.Exec(`UPDATE memories SET expires_at = ?, sensitive = 1 WHERE id = ?`, past, rec.ID)

	res, err := bank.RunDormancyPass()
	if err != nil {
		t.Fatalf("RunDormancyPass: %v", err)
	}
	if res.Slept != 0 {
		t.Errorf("expected 0 newly dormant (sensitive must be skipped), got %d", res.Slept)
	}
	if res.Skipped != 1 {
		t.Errorf("expected 1 skipped, got %d", res.Skipped)
	}
	got, _ := bank.GetMemory(rec.ID)
	if got.DormantAt != "" {
		t.Errorf("sensitive memory should NOT be put to sleep, got dormant_at=%q", got.DormantAt)
	}
}

func TestBundleL_RunDormancyPass_SkipsAlreadyDormant(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "already dormant"})
	bank.UpdateMemory(rec.ID, MemoryUpdate{Dormant: ptrBool(true)})
	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	bank.db.Exec(`UPDATE memories SET expires_at = ? WHERE id = ?`, past, rec.ID)

	res, err := bank.RunDormancyPass()
	if err != nil {
		t.Fatalf("RunDormancyPass: %v", err)
	}
	if res.Slept != 0 {
		t.Errorf("expected 0 new sleeps (already dormant), got %d", res.Slept)
	}
}

func TestBundleL_RunDormancyPass_Idempotent(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "should sleep once"})
	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	bank.db.Exec(`UPDATE memories SET expires_at = ? WHERE id = ?`, past, rec.ID)

	res1, _ := bank.RunDormancyPass()
	res2, _ := bank.RunDormancyPass()
	if res1.Slept != 1 {
		t.Errorf("first run should sleep 1, got %d", res1.Slept)
	}
	if res2.Slept != 0 {
		t.Errorf("second run should sleep 0 (idempotent), got %d", res2.Slept)
	}
}

func TestBundleL_RunDormancyPass_WritesAuditRow(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "audit me"})
	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	bank.db.Exec(`UPDATE memories SET expires_at = ? WHERE id = ?`, past, rec.ID)

	bank.RunDormancyPass()
	rows, _ := bank.ListAuditLog(AuditFilter{Operation: "dormancy_set", Limit: 5})
	found := false
	for _, r := range rows {
		if r.EntityID == rec.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected dormancy_set audit row for %s, not found", rec.ID)
	}
}

func TestBundleL_RunDormancyPass_FutureExpiryNoOp(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "future TTL"})
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	bank.db.Exec(`UPDATE memories SET expires_at = ? WHERE id = ?`, future, rec.ID)

	res, _ := bank.RunDormancyPass()
	if res.Slept != 0 {
		t.Errorf("future TTL must not trigger sleep, got %d", res.Slept)
	}
	got, _ := bank.GetMemory(rec.ID)
	if got.DormantAt != "" {
		t.Errorf("future TTL should leave memory awake, got dormant_at=%q", got.DormantAt)
	}
}
