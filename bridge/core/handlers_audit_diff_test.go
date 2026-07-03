package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// auditedRow walks the audit log for a memory's auto_flag_sensitive entry
// and parses its before/after JSON.
func sensitiveFlipAudit(t *testing.T, bank *Bank, traceID string) AuditEntry {
	t.Helper()
	entries, err := bank.ListAuditLog(AuditFilter{
		EntityType: "trace", EntityID: traceID, Operation: "auto_flag_sensitive",
	})
	if err != nil {
		t.Fatalf("ListAuditLog: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 auto_flag_sensitive audit, got %d", len(entries))
	}
	return entries[0]
}

// TestAudit_SensitiveFlip_BeforeAfterPopulated — when a memory is auto-
// flagged, the audit row must include both before and after JSON, and the
// diff must show sensitive: false→true.
func TestAudit_SensitiveFlip_BeforeAfterPopulated(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{
		Text: "deployment uses sk-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789",
	})
	if !rec.Sensitive {
		t.Fatalf("expected regex auto-flag to mark sensitive")
	}

	a := sensitiveFlipAudit(t, bank, rec.ID)
	if a.BeforeJSON == "" {
		t.Errorf("BeforeJSON should not be empty for auto_flag_sensitive")
	}
	if a.AfterJSON == "" {
		t.Errorf("AfterJSON should not be empty for auto_flag_sensitive")
	}

	var before, after MemoryRecord
	if err := json.Unmarshal([]byte(a.BeforeJSON), &before); err != nil {
		t.Fatalf("decode before: %v", err)
	}
	if err := json.Unmarshal([]byte(a.AfterJSON), &after); err != nil {
		t.Fatalf("decode after: %v", err)
	}
	if before.Sensitive {
		t.Errorf("before snapshot should have sensitive=false, got %+v", before)
	}
	if !after.Sensitive {
		t.Errorf("after snapshot should have sensitive=true, got %+v", after)
	}
}

// TestAudit_SensitiveBodyRedaction — auto_flag_sensitive's audit JSON must
// NOT contain the verbatim sensitive memory body. The redacted placeholder
// goes into the body field on both before and after snapshots.
func TestAudit_SensitiveBodyRedaction(t *testing.T) {
	bank := newTestBank(t)
	const sneakyKey = "sk-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "production " + sneakyKey})
	if !rec.Sensitive {
		t.Fatalf("test setup: memory should have been auto-flagged sensitive")
	}

	a := sensitiveFlipAudit(t, bank, rec.ID)
	for _, field := range []string{a.BeforeJSON, a.AfterJSON} {
		if strings.Contains(field, sneakyKey) {
			t.Errorf("INVARIANT VIOLATION: audit field leaked verbatim key: %q", field)
		}
		if !strings.Contains(field, "REDACTED") {
			t.Errorf("expected REDACTED placeholder, got %q", field)
		}
	}
}

// TestAudit_SensitiveFlipShowsFlipNotBody — even though the body is
// redacted, the audit's BeforeJSON.sensitive=false and AfterJSON.sensitive=
// true diff is still observable, so the dashboard's diff renderer can
// surface "sensitive: false → true".
func TestAudit_SensitiveFlipShowsFlipNotBody(t *testing.T) {
	bank := newTestBank(t)
	// Manually mark something sensitive via tag so we have a clean
	// regex-pattern-free body for assertion clarity.
	rec, _ := bank.SaveMemory(MemoryRecord{
		Text: "innocuous text", Tags: []string{"private"},
	})
	if !rec.Sensitive {
		t.Fatalf("tag-trigger should have flagged sensitive")
	}
	a := sensitiveFlipAudit(t, bank, rec.ID)

	var before, after MemoryRecord
	_ = json.Unmarshal([]byte(a.BeforeJSON), &before)
	_ = json.Unmarshal([]byte(a.AfterJSON), &after)
	// The interesting diff must be on the sensitive field.
	if before.Sensitive == after.Sensitive {
		t.Errorf("expected sensitive field to differ; got before=%v after=%v",
			before.Sensitive, after.Sensitive)
	}
	// Body in BOTH snapshots is redacted (per redactForAudit policy).
	if !strings.Contains(before.Text, "REDACTED") {
		t.Errorf("before body should be redacted, got %q", before.Text)
	}
	if !strings.Contains(after.Text, "REDACTED") {
		t.Errorf("after body should be redacted, got %q", after.Text)
	}
}

// TestAudit_NonSensitiveCreate_BeforeEmpty — regression test: a normal
// (non-sensitive) memory create should NOT produce an auto_flag_sensitive
// audit row at all (only sensitive flips do).
func TestAudit_NonSensitiveCreate_BeforeEmpty(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "totally normal text"})
	if rec.Sensitive {
		t.Fatalf("plain text should not auto-flag")
	}
	entries, _ := bank.ListAuditLog(AuditFilter{
		EntityType: "trace", EntityID: rec.ID, Operation: "auto_flag_sensitive",
	})
	if len(entries) != 0 {
		t.Errorf("non-sensitive create should produce no auto_flag_sensitive audit; got %d", len(entries))
	}
}

// TestAudit_AIClassifierFlip_BeforeAfter — AI-flipped memory has
// before/after populated too (analogous to regex path).
func TestAudit_AIClassifierFlip_BeforeAfter(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "ambiguous prose about Dr. Chen"})

	scripted := &scriptedProvider{
		respond: func(_ string) string { return "YES\nMentions personal medical context." },
	}
	sc := newSensitiveClassifierConst(bank, scripted, 0, 10)
	sc.pass(context.Background())

	a := sensitiveFlipAudit(t, bank, rec.ID)
	if a.BeforeJSON == "" || a.AfterJSON == "" {
		t.Fatalf("AI flip must populate both before and after; got before=%q after=%q",
			a.BeforeJSON, a.AfterJSON)
	}
	var before, after MemoryRecord
	_ = json.Unmarshal([]byte(a.BeforeJSON), &before)
	_ = json.Unmarshal([]byte(a.AfterJSON), &after)
	if before.Sensitive {
		t.Errorf("AI flip before should be sensitive=false, got %v", before.Sensitive)
	}
	if !after.Sensitive {
		t.Errorf("AI flip after should be sensitive=true, got %v", after.Sensitive)
	}
}

// TestAudit_UpdateMemory_PatchProducesBeforeAfter — explicit Sensitive=true
// PATCH writes audit with both snapshots, so the dashboard can render the
// flip.
func TestAudit_UpdateMemory_PatchProducesBeforeAfter(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "neutral", Tags: []string{"work"}})
	tr := true
	if _, err := bank.UpdateMemory(rec.ID, MemoryUpdate{Sensitive: &tr}); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	a := sensitiveFlipAudit(t, bank, rec.ID)
	if a.BeforeJSON == "" || a.AfterJSON == "" {
		t.Errorf("manual PATCH must populate before/after; got before=%q after=%q",
			a.BeforeJSON, a.AfterJSON)
	}
}
