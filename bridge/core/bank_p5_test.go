package main

import (
	"errors"
	"path/filepath"
	"testing"

	"database/sql"
)

func newTestBank(t *testing.T) *Bank {
	t.Helper()
	dir := t.TempDir()
	bank, err := NewBank(filepath.Join(dir, "p5.db"))
	if err != nil {
		t.Fatalf("NewBank: %v", err)
	}
	t.Cleanup(func() { bank.Close() })
	return bank
}

func TestMemoryLifecycleAndSoftDelete(t *testing.T) {
	bank := newTestBank(t)

	rec, err := bank.SaveMemory(MemoryRecord{Text: "hello world", Tags: []string{"greeting"}})
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}
	if rec.Source != "adapter" {
		t.Errorf("expected default source=adapter, got %q", rec.Source)
	}

	// Lifecycle flag flip.
	if err := bank.SetMemoryLifecycle(rec.ID, "light_encoded", true); err != nil {
		t.Fatalf("SetMemoryLifecycle: %v", err)
	}
	got, _ := bank.GetMemory(rec.ID)
	if !got.LightEncoded {
		t.Errorf("expected light_encoded=true after flip")
	}

	// PATCH a field.
	enriched := "hello, refined world"
	tags := []string{"greeting", "refined"}
	updated, err := bank.UpdateMemory(rec.ID, MemoryUpdate{
		EnrichedText: &enriched,
		Tags:         &tags,
	})
	if err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	if updated.EnrichedText != enriched {
		t.Errorf("enriched_text not updated: %q", updated.EnrichedText)
	}
	if len(updated.Tags) != 2 || updated.Tags[1] != "refined" {
		t.Errorf("tags not updated: %v", updated.Tags)
	}
	// Raw text must be untouched.
	if updated.Text != "hello world" {
		t.Errorf("raw text was mutated: %q", updated.Text)
	}

	// Soft delete; default list excludes.
	if err := bank.SoftDeleteMemory(rec.ID); err != nil {
		t.Fatalf("SoftDeleteMemory: %v", err)
	}
	live, _ := bank.ListMemoriesWith(MemoryListOpts{})
	for _, m := range live {
		if m.ID == rec.ID {
			t.Errorf("soft-deleted memory leaked into default list")
		}
	}
	deleted, _ := bank.ListMemoriesWith(MemoryListOpts{OnlyDeleted: true})
	if len(deleted) != 1 || deleted[0].ID != rec.ID {
		t.Errorf("OnlyDeleted should return the soft-deleted record, got %v", deleted)
	}

	// Undelete.
	if err := bank.UndeleteMemory(rec.ID); err != nil {
		t.Fatalf("UndeleteMemory: %v", err)
	}
	got, _ = bank.GetMemory(rec.ID)
	if got.DeletedAt != "" {
		t.Errorf("deleted_at should clear after Undelete; got %q", got.DeletedAt)
	}

	// Hard delete.
	if err := bank.HardDeleteMemory(rec.ID); err != nil {
		t.Fatalf("HardDeleteMemory: %v", err)
	}
	if _, err := bank.GetMemory(rec.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("expected ErrNoRows after hard delete, got %v", err)
	}
}

func TestMemoryMapsAndAssociations(t *testing.T) {
	bank := newTestBank(t)

	// Invalid type.
	if _, err := bank.SaveMemoryMap(MemoryMap{Name: "X", Type: "garbage"}); err == nil {
		t.Errorf("expected error for invalid type")
	}

	mapA, err := bank.SaveMemoryMap(MemoryMap{
		Name: "Event Tracker", Type: "project",
		AnchorTags: []string{"event-tracker", "qr"},
	})
	if err != nil {
		t.Fatalf("SaveMemoryMap A: %v", err)
	}
	mapB, err := bank.SaveMemoryMap(MemoryMap{
		Name: "OAuth", Type: "technology",
		AnchorTags: []string{"oauth", "auth"},
	})
	if err != nil {
		t.Fatalf("SaveMemoryMap B: %v", err)
	}

	// PATCH anchor_tags only.
	tags := []string{"event-tracker", "qr", "inventory"}
	patched, err := bank.UpdateMemoryMap(mapA.ID, MemoryMapUpdate{AnchorTags: &tags})
	if err != nil {
		t.Fatalf("UpdateMemoryMap: %v", err)
	}
	if len(patched.AnchorTags) != 3 {
		t.Errorf("anchor_tags not updated: %v", patched.AnchorTags)
	}

	// GetMemoryMapByName.
	got, err := bank.GetMemoryMapByName("OAuth", "technology")
	if err != nil || got.ID != mapB.ID {
		t.Errorf("GetMemoryMapByName: %v %s", err, got.ID)
	}

	// List with type filter.
	tech, _ := bank.ListMemoryMaps("technology", 0)
	if len(tech) != 1 {
		t.Errorf("expected 1 technology map, got %d", len(tech))
	}

	// Add an association A → B.
	a, err := bank.AddAssociation(MapAssociation{
		FromMapID: mapA.ID, ToMapID: mapB.ID,
		Association: "uses_technology", Weight: 0.9,
	})
	if err != nil {
		t.Fatalf("AddAssociation: %v", err)
	}
	if a.ID == "" {
		t.Errorf("association ID should be auto-assigned")
	}
	// Idempotent re-add (updates weight).
	if _, err := bank.AddAssociation(MapAssociation{
		FromMapID: mapA.ID, ToMapID: mapB.ID,
		Association: "uses_technology", Weight: 0.95,
	}); err != nil {
		t.Fatalf("AddAssociation idempotent: %v", err)
	}

	from, _ := bank.ListAssociations(mapA.ID, "from")
	if len(from) != 1 {
		t.Errorf("expected 1 outgoing association, got %d", len(from))
	}
	to, _ := bank.ListAssociations(mapB.ID, "to")
	if len(to) != 1 {
		t.Errorf("expected 1 incoming association, got %d", len(to))
	}

	if err := bank.RemoveAssociation(mapA.ID, mapB.ID, "uses_technology"); err != nil {
		t.Errorf("RemoveAssociation: %v", err)
	}

	// DELETE map cascades to associations and traces.
	mem, _ := bank.SaveMemory(MemoryRecord{Text: "trace1"})
	_ = bank.LinkMapTrace(mapA.ID, mem.ID)
	traces, _ := bank.ListTracesForMap(mapA.ID, 0)
	if len(traces) != 1 {
		t.Errorf("expected 1 linked trace, got %d", len(traces))
	}
	if err := bank.DeleteMemoryMap(mapA.ID); err != nil {
		t.Fatalf("DeleteMemoryMap: %v", err)
	}
	traces, _ = bank.ListTracesForMap(mapA.ID, 0)
	if len(traces) != 0 {
		t.Errorf("expected map_traces cascade-deleted, got %d", len(traces))
	}
}

func TestLexiconCanonicalKeyAndIncrement(t *testing.T) {
	bank := newTestBank(t)

	if err := bank.IncrementLexicon("oauth", "auth"); err != nil {
		t.Fatalf("IncrementLexicon: %v", err)
	}
	// Reverse-order increment must hit the same row.
	if err := bank.IncrementLexicon("auth", "oauth"); err != nil {
		t.Fatalf("IncrementLexicon reverse: %v", err)
	}
	rows, _ := bank.ListLexicon("", 0)
	if len(rows) != 1 {
		t.Fatalf("expected 1 lexicon row, got %d", len(rows))
	}
	if rows[0].Cooccurrence != 2 {
		t.Errorf("expected cooccurrence=2, got %d", rows[0].Cooccurrence)
	}
	if rows[0].TagA != "auth" || rows[0].TagB != "oauth" {
		t.Errorf("expected canonical (auth,oauth), got (%s,%s)", rows[0].TagA, rows[0].TagB)
	}

	// Self-pair and empty are no-ops.
	if err := bank.IncrementLexicon("auth", "auth"); err != nil {
		t.Errorf("self-pair should be no-op, got %v", err)
	}
	if err := bank.IncrementLexicon("", "anything"); err != nil {
		t.Errorf("empty should be no-op, got %v", err)
	}

	rowsForAuth, _ := bank.ListLexicon("auth", 10)
	if len(rowsForAuth) != 1 {
		t.Errorf("ListLexicon(auth) should return the (auth,oauth) row, got %d rows", len(rowsForAuth))
	}
}

func TestAuditLogAppendAndFilter(t *testing.T) {
	bank := newTestBank(t)
	if err := bank.AppendAudit(AuditEntry{
		Operation: "create", EntityType: "trace", EntityID: "t1",
		AfterJSON: `{"id":"t1"}`,
	}); err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}
	if err := bank.AppendAudit(AuditEntry{
		Operation: "update", EntityType: "memory_map", EntityID: "m1",
	}); err != nil {
		t.Fatalf("AppendAudit 2: %v", err)
	}

	// Filter by entity_type.
	traces, err := bank.ListAuditLog(AuditFilter{EntityType: "trace"})
	if err != nil {
		t.Fatalf("ListAuditLog: %v", err)
	}
	if len(traces) != 1 || traces[0].EntityID != "t1" {
		t.Errorf("expected 1 trace audit entry, got %v", traces)
	}

	// Empty filter returns all.
	all, _ := bank.ListAuditLog(AuditFilter{})
	if len(all) != 2 {
		t.Errorf("expected 2 audit entries, got %d", len(all))
	}

	// Validation.
	if err := bank.AppendAudit(AuditEntry{}); err == nil {
		t.Errorf("expected error for empty audit entry")
	}
}

func TestResearchCacheAndPurge(t *testing.T) {
	bank := newTestBank(t)
	now := nowUTC()
	past := "2020-01-01T00:00:00Z"
	future := "2099-01-01T00:00:00Z"

	if err := bank.SaveResearchCache(ResearchCacheEntry{
		Topic: "Gravity", Payload: `{"summary":"..."}`,
		FetchedAt: now, ExpiresAt: future,
	}); err != nil {
		t.Fatalf("SaveResearchCache: %v", err)
	}
	if err := bank.SaveResearchCache(ResearchCacheEntry{
		Topic: "Old", Payload: `{}`, FetchedAt: now, ExpiresAt: past,
	}); err != nil {
		t.Fatalf("SaveResearchCache expired: %v", err)
	}

	got, err := bank.GetResearchCache("Gravity")
	if err != nil || got.Topic != "Gravity" {
		t.Errorf("GetResearchCache: %v %v", err, got)
	}

	n, err := bank.PurgeExpiredResearchCache()
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 purged, got %d", n)
	}
	all, _ := bank.ListResearchCache(0, true)
	if len(all) != 1 {
		t.Errorf("expected 1 surviving entry, got %d", len(all))
	}
}

func TestTokenBudgetAccumulates(t *testing.T) {
	bank := newTestBank(t)
	if err := bank.AddTokensUsed("2026-05-08", 100); err != nil {
		t.Fatalf("AddTokensUsed: %v", err)
	}
	if err := bank.AddTokensUsed("2026-05-08", 250); err != nil {
		t.Fatalf("AddTokensUsed 2: %v", err)
	}
	got, err := bank.GetTokensUsed("2026-05-08")
	if err != nil {
		t.Fatalf("GetTokensUsed: %v", err)
	}
	if got != 350 {
		t.Errorf("expected 350 tokens, got %d", got)
	}

	// Different day = independent counter.
	_ = bank.AddTokensUsed("2026-05-09", 42)
	got9, _ := bank.GetTokensUsed("2026-05-09")
	if got9 != 42 {
		t.Errorf("day-9 should be 42, got %d", got9)
	}
	hist, _ := bank.ListTokenBudget(7)
	if len(hist) != 2 {
		t.Errorf("expected 2 day rows, got %d", len(hist))
	}
}
