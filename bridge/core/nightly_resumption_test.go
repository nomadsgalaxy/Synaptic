// nightly_resumption_test.go — tests for crash recovery via heartbeat
// + ReconcileStaleInProgress.
//
// Handoff: `CODE_HANDOFF — dream cycle resumption + crash recovery (2026-05-10).md`
package main

import (
	"testing"
	"time"
)

func TestReconcile_StaleHeartbeatFlipsToInterrupted(t *testing.T) {
	bank := newTestBank(t)
	now := time.Now().UTC()
	stale := now.Add(-10 * time.Minute).Format(time.RFC3339Nano)

	// Seed an in_progress row with a heartbeat that's older than the
	// reconciliation cutoff.
	bank.InsertNightlyRun(NightlyRun{
		ID:          "nightly-stale",
		StartedAt:   stale,
		Status:      "in_progress",
		HeartbeatAt: stale,
		CreatedAt:   stale,
	})
	n, err := bank.ReconcileStaleInProgress(5 * time.Minute)
	if err != nil {
		t.Fatalf("ReconcileStaleInProgress: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 row reconciled, got %d", n)
	}
	got, _ := bank.GetNightlyRun("nightly-stale")
	if got.Status != "interrupted" {
		t.Errorf("expected status=interrupted, got %q", got.Status)
	}
	if got.InterruptReason != "process_restart" {
		t.Errorf("expected interrupt_reason=process_restart, got %q", got.InterruptReason)
	}
	if got.FinishedAt == "" {
		t.Errorf("expected finished_at populated")
	}
}

func TestReconcile_NullHeartbeatTreatedAsStale(t *testing.T) {
	// Pre-heartbeat rows (from before this feature shipped) have empty
	// heartbeat_at — ReconcileStaleInProgress should still flip them so
	// the upgrade-to-this-build doesn't strand them.
	bank := newTestBank(t)
	bank.InsertNightlyRun(NightlyRun{
		ID:        "nightly-no-heartbeat",
		StartedAt: time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339Nano),
		Status:    "in_progress",
		// HeartbeatAt deliberately omitted → empty string
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	n, err := bank.ReconcileStaleInProgress(5 * time.Minute)
	if err != nil {
		t.Fatalf("ReconcileStaleInProgress: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 reconciled, got %d", n)
	}
	got, _ := bank.GetNightlyRun("nightly-no-heartbeat")
	if got.Status != "interrupted" {
		t.Errorf("expected status=interrupted, got %q", got.Status)
	}
}

func TestReconcile_FreshHeartbeatNotTouched(t *testing.T) {
	// An active run with a recent heartbeat should NOT be flipped.
	bank := newTestBank(t)
	now := time.Now().UTC()
	fresh := now.Add(-30 * time.Second).Format(time.RFC3339Nano)
	bank.InsertNightlyRun(NightlyRun{
		ID:          "nightly-active",
		StartedAt:   fresh,
		Status:      "in_progress",
		HeartbeatAt: fresh,
		CreatedAt:   fresh,
	})
	n, err := bank.ReconcileStaleInProgress(5 * time.Minute)
	if err != nil {
		t.Fatalf("ReconcileStaleInProgress: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 reconciled (active run), got %d", n)
	}
	got, _ := bank.GetNightlyRun("nightly-active")
	if got.Status != "in_progress" {
		t.Errorf("active run should remain in_progress, got %q", got.Status)
	}
}

func TestReconcile_OnlyTouchesInProgress(t *testing.T) {
	// Completed / failed / partial / interrupted rows must NOT be touched.
	bank := newTestBank(t)
	old := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339Nano)
	for _, status := range []string{"completed", "failed", "partial", "interrupted"} {
		bank.InsertNightlyRun(NightlyRun{
			ID:        "nightly-" + status,
			StartedAt: old,
			Status:    status,
			CreatedAt: old,
		})
	}
	n, err := bank.ReconcileStaleInProgress(5 * time.Minute)
	if err != nil {
		t.Fatalf("ReconcileStaleInProgress: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 reconciled (no in_progress rows), got %d", n)
	}
}

func TestBumpHeartbeat_OnlyAffectsInProgress(t *testing.T) {
	// BumpNightlyHeartbeat should be a no-op against a finished row —
	// avoids weird "heartbeat at 17:00 on a row that finished at 15:30"
	// readouts after a late tick fires post-completion.
	bank := newTestBank(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	bank.InsertNightlyRun(NightlyRun{
		ID:        "nightly-done",
		StartedAt: now,
		Status:    "completed",
		CreatedAt: now,
	})
	if err := bank.BumpNightlyHeartbeat("nightly-done"); err != nil {
		t.Fatalf("BumpNightlyHeartbeat: %v", err)
	}
	got, _ := bank.GetNightlyRun("nightly-done")
	if got.HeartbeatAt != "" {
		t.Errorf("completed run shouldn't get a heartbeat bump, got %q", got.HeartbeatAt)
	}
}

func TestBumpHeartbeat_RefreshesInProgressTimestamp(t *testing.T) {
	bank := newTestBank(t)
	old := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339Nano)
	bank.InsertNightlyRun(NightlyRun{
		ID:          "nightly-active",
		StartedAt:   old,
		Status:      "in_progress",
		HeartbeatAt: old,
		CreatedAt:   old,
	})
	time.Sleep(2 * time.Millisecond) // ensure now > old by at least a tick
	if err := bank.BumpNightlyHeartbeat("nightly-active"); err != nil {
		t.Fatalf("BumpNightlyHeartbeat: %v", err)
	}
	got, _ := bank.GetNightlyRun("nightly-active")
	if got.HeartbeatAt <= old {
		t.Errorf("heartbeat should be refreshed; got %q, original %q", got.HeartbeatAt, old)
	}
}

func TestReconcile_PreservesExistingErrorMessage(t *testing.T) {
	// If a row already had an error message (e.g. a panic that was recovered
	// before the runner could mark status='failed' cleanly), don't clobber it.
	bank := newTestBank(t)
	old := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339Nano)
	bank.InsertNightlyRun(NightlyRun{
		ID:        "nightly-prior-err",
		StartedAt: old,
		Status:    "in_progress",
		Error:     "panic: runtime error: nil pointer",
		CreatedAt: old,
	})
	bank.ReconcileStaleInProgress(5 * time.Minute)
	got, _ := bank.GetNightlyRun("nightly-prior-err")
	if got.Error != "panic: runtime error: nil pointer" {
		t.Errorf("existing error message clobbered: got %q", got.Error)
	}
}
