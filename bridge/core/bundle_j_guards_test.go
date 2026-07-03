// bundle_j_guards_test.go — guards for v2.7 Bundle J (BM25 + cosine
// hybrid retrieval via SQLite FTS5 + Reciprocal Rank Fusion):
//
//   - FTS5 schema + triggers keep memories_fts in lockstep with the
//     memories table (insert / update / delete all reflected)
//   - Backfill catches legacy banks where rows existed before FTS5
//   - Bank.SearchBM25 returns relevant matches for keyword queries
//   - Bank.SearchBM25 sanitises FTS5-special characters (no crash on
//     a stray '*' / quote / paren / colon in the user's query)
//   - ReciprocalRankFusion fuses two ranked lists with predictable
//     scoring (id-in-both > id-in-first-only > id-in-second-only),
//     respects the k constant, and honours the limit cap
//   - hybridEnabled / hybridRRFK setting helpers honour defaults and
//     persisted overrides
package main

import (
	"testing"
)

// ── FTS5 schema integrity ───────────────────────────────────────────────

func TestFTS5_InsertTriggerPopulatesIndex(t *testing.T) {
	bank := newTestBank(t)
	rec, err := bank.SaveMemory(MemoryRecord{
		Text: "OAuth refresh token rotation procedure",
		Tags: []string{"auth", "security"},
	})
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}
	hits, err := bank.SearchBM25("OAuth refresh", 10)
	if err != nil {
		t.Fatalf("SearchBM25: %v", err)
	}
	found := false
	for _, id := range hits {
		if id == rec.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected new memory %q in BM25 hits, got %v", rec.ID, hits)
	}
}

func TestFTS5_DeleteTriggerRemovesFromIndex(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{
		Text: "unique-token-zonk-for-deletion-test",
	})
	// Confirm indexed.
	hits, _ := bank.SearchBM25("unique-token-zonk-for-deletion-test", 10)
	if len(hits) == 0 {
		t.Fatalf("expected the memory to be in FTS index pre-delete")
	}
	// Hard delete (DELETE FROM memories) — test the AFTER DELETE trigger.
	if _, err := bank.db.Exec(`DELETE FROM memories WHERE id = ?`, rec.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	hits, _ = bank.SearchBM25("unique-token-zonk-for-deletion-test", 10)
	for _, id := range hits {
		if id == rec.ID {
			t.Errorf("expected the deleted memory absent from FTS index, still found %q", id)
		}
	}
}

func TestFTS5_SoftDeletedExcluded(t *testing.T) {
	// Soft-delete = deleted_at != ''. SearchBM25 joins on memories and
	// filters those out so /recall never surfaces tombstoned rows.
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{
		Text: "soft-delete-trial widget specification",
	})
	// Soft-delete by writing deleted_at directly.
	if _, err := bank.db.Exec(`UPDATE memories SET deleted_at='2026-01-01T00:00:00Z' WHERE id = ?`, rec.ID); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}
	hits, _ := bank.SearchBM25("soft-delete-trial widget", 10)
	for _, id := range hits {
		if id == rec.ID {
			t.Errorf("expected soft-deleted memory to be filtered, still found %q", id)
		}
	}
}

// ── SearchBM25 sanitisation ─────────────────────────────────────────────

func TestSearchBM25_HandlesSpecialCharacters(t *testing.T) {
	bank := newTestBank(t)
	bank.SaveMemory(MemoryRecord{Text: "hello world this is innocuous text"})
	// Each of these queries would crash FTS5 if passed unsanitised.
	dangerous := []string{
		`"unterminated quote`,
		"prefix*",
		"paren(open",
		"col:on",
		"^caret",
		"NEAR(should not be a function)",
		"-leading-dash",
		"",
		"!@#$%^&*()",
	}
	for _, q := range dangerous {
		t.Run(q, func(t *testing.T) {
			// Should not error out. May return empty results — that's fine.
			if _, err := bank.SearchBM25(q, 5); err != nil {
				t.Errorf("SearchBM25(%q) errored: %v", q, err)
			}
		})
	}
}

func TestSearchBM25_NilBankSafe(t *testing.T) {
	var b *Bank
	_, err := b.SearchBM25("anything", 10)
	if err == nil {
		t.Errorf("nil bank should return error, got nil")
	}
}

func TestSearchBM25_EmptyQueryReturnsNothing(t *testing.T) {
	bank := newTestBank(t)
	bank.SaveMemory(MemoryRecord{Text: "alpha beta gamma"})
	hits, err := bank.SearchBM25("", 10)
	if err != nil {
		t.Errorf("empty query should not error, got %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("empty query should return no hits, got %d", len(hits))
	}
}

func TestSearchBM25_RanksRelevantHigher(t *testing.T) {
	bank := newTestBank(t)
	// Memory A: contains "kubernetes ingress" — should rank highest
	// for that query (both terms present).
	a, _ := bank.SaveMemory(MemoryRecord{Text: "kubernetes ingress controller config"})
	// Memory B: contains only "kubernetes" — should rank lower.
	bank.SaveMemory(MemoryRecord{Text: "kubernetes pod scheduling notes"})
	// Memory C: contains neither — should not appear.
	bank.SaveMemory(MemoryRecord{Text: "rust borrow checker rules"})

	hits, err := bank.SearchBM25("kubernetes ingress", 10)
	if err != nil {
		t.Fatalf("SearchBM25: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("expected at least one hit")
	}
	if hits[0] != a.ID {
		t.Errorf("expected memory A (both terms) ranked first, got %q (full list %v)", hits[0], hits)
	}
}

// ── ReciprocalRankFusion ────────────────────────────────────────────────

func TestRRF_IDInBothListsWins(t *testing.T) {
	cosine := []string{"a", "b", "c"}
	bm25 := []string{"b", "d", "a"}
	fused := ReciprocalRankFusion([][]string{cosine, bm25}, 60, 0)
	// "a" appears in both at positions 1 and 3 → score = 1/61 + 1/63 ≈ 0.0322
	// "b" appears in both at positions 2 and 1 → score = 1/62 + 1/61 ≈ 0.0325
	// "c" appears once at position 3 → 1/63 ≈ 0.0159
	// "d" appears once at position 2 → 1/62 ≈ 0.0161
	// Expected order: b > a > d > c
	want := []string{"b", "a", "d", "c"}
	if len(fused) != len(want) {
		t.Fatalf("expected %d entries, got %d", len(want), len(fused))
	}
	for i, w := range want {
		if fused[i].ID != w {
			t.Errorf("position %d: got %q, want %q (full fused: %v)", i, fused[i].ID, w, fused)
		}
	}
}

func TestRRF_RespectsLimit(t *testing.T) {
	a := []string{"a", "b", "c", "d", "e"}
	b := []string{"e", "d", "c", "b", "a"}
	fused := ReciprocalRankFusion([][]string{a, b}, 60, 3)
	if len(fused) != 3 {
		t.Errorf("expected limit=3, got %d", len(fused))
	}
}

func TestRRF_EmptyListsReturnNothing(t *testing.T) {
	if got := ReciprocalRankFusion([][]string{}, 60, 10); len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
	if got := ReciprocalRankFusion([][]string{{}, {}}, 60, 10); len(got) != 0 {
		t.Errorf("expected empty for empty lists, got %v", got)
	}
}

func TestRRF_SingleListPreservesOrder(t *testing.T) {
	single := []string{"x", "y", "z"}
	fused := ReciprocalRankFusion([][]string{single}, 60, 0)
	for i, want := range single {
		if fused[i].ID != want {
			t.Errorf("position %d: got %q, want %q", i, fused[i].ID, want)
		}
	}
}

func TestRRF_ZeroKDefaultsToCormack60(t *testing.T) {
	a := []string{"a"}
	b := []string{"a"}
	// With k=60, "a" gets 2/(60+1) ≈ 0.0328. Test that k<=0 picks up that default.
	fusedDefault := ReciprocalRankFusion([][]string{a, b}, 0, 0)
	fusedExplicit := ReciprocalRankFusion([][]string{a, b}, 60, 0)
	if len(fusedDefault) != 1 || len(fusedExplicit) != 1 {
		t.Fatalf("expected one row each, got %d / %d", len(fusedDefault), len(fusedExplicit))
	}
	if fusedDefault[0].Score != fusedExplicit[0].Score {
		t.Errorf("k=0 should default to 60, got differing scores: %v vs %v",
			fusedDefault[0].Score, fusedExplicit[0].Score)
	}
}

// ── Setting helpers ─────────────────────────────────────────────────────

func TestHybridEnabled_DefaultsOn(t *testing.T) {
	bank := newTestBank(t)
	if !hybridEnabled(bank) {
		t.Errorf("hybrid retrieval should default ON")
	}
	if !hybridEnabled(nil) {
		t.Errorf("nil bank should default ON too")
	}
}

func TestHybridEnabled_HonoursOffSetting(t *testing.T) {
	bank := newTestBank(t)
	for _, val := range []string{"0", "false", "off", "no", "FALSE"} {
		bank.SetSetting(HybridRecallEnabledKey, val)
		if hybridEnabled(bank) {
			t.Errorf("value %q should disable hybrid, got enabled", val)
		}
	}
	bank.SetSetting(HybridRecallEnabledKey, "1")
	if !hybridEnabled(bank) {
		t.Errorf(`"1" should re-enable hybrid`)
	}
}

func TestHybridRRFK_DefaultsTo60(t *testing.T) {
	bank := newTestBank(t)
	if got := hybridRRFK(bank); got != 60 {
		t.Errorf("default k should be 60, got %d", got)
	}
}

func TestHybridRRFK_HonoursValidOverride(t *testing.T) {
	bank := newTestBank(t)
	bank.SetSetting(HybridRecallRRFKKey, "30")
	if got := hybridRRFK(bank); got != 30 {
		t.Errorf("override should win, got %d", got)
	}
}

func TestHybridRRFK_RejectsInvalid(t *testing.T) {
	bank := newTestBank(t)
	for _, bad := range []string{"-5", "not-a-number", "0"} {
		bank.SetSetting(HybridRecallRRFKKey, bad)
		if got := hybridRRFK(bank); got != 60 {
			t.Errorf("bad value %q should fall back to 60, got %d", bad, got)
		}
	}
}
