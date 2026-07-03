// bundle_n_guards_test.go — guards for v2.7 Bundle N (entity resolution
// + alias-driven /recall expansion).
//
// Coverage:
//   - Schema migration: entities + entity_aliases tables exist
//   - UpsertEntity round-trips, conflicts on canonical_name (case-insensitive)
//   - GetEntity / ListEntities / DeleteEntity behave
//   - AddAlias / RemoveAlias / ListAliases round-trip; AddAlias is idempotent
//   - ResolveAlias is case-insensitive
//   - ResolveAlias surfaces ALL canonicals for ambiguous aliases (Apple → fruit + company)
//   - ExpandQueryViaAliases generates n-grams up to length 4
//   - ExpandQueryViaAliases returns nil on empty input
//   - DeleteEntity cascades to its aliases (foreign-key ON DELETE CASCADE)
package main

import (
	"errors"
	"testing"

	"database/sql"
)

// ── Schema ──────────────────────────────────────────────────────────────

func TestBundleN_TablesExist(t *testing.T) {
	bank := newTestBank(t)
	for _, want := range []string{"entities", "entity_aliases"} {
		var name string
		row := bank.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name = ?`, want)
		if err := row.Scan(&name); err != nil {
			t.Errorf("expected table %q, got %v", want, err)
		}
	}
}

// ── UpsertEntity ────────────────────────────────────────────────────────

func TestBundleN_UpsertEntity_RoundTrips(t *testing.T) {
	bank := newTestBank(t)
	got, err := bank.UpsertEntity(Entity{CanonicalName: "Anthropic", Kind: "org"})
	if err != nil {
		t.Fatalf("UpsertEntity: %v", err)
	}
	if got.ID == "" {
		t.Errorf("expected auto-generated ID, got empty")
	}
	if got.CanonicalName != "Anthropic" || got.Kind != "org" {
		t.Errorf("roundtrip mismatch, got %+v", got)
	}
}

func TestBundleN_UpsertEntity_RejectsBlankName(t *testing.T) {
	bank := newTestBank(t)
	if _, err := bank.UpsertEntity(Entity{CanonicalName: "   "}); err == nil {
		t.Errorf("expected error on blank canonical_name, got nil")
	}
}

func TestBundleN_UpsertEntity_ConflictsOnCanonicalName(t *testing.T) {
	// Two upserts with the same name (different case) should resolve to
	// the same entity row.
	bank := newTestBank(t)
	a, _ := bank.UpsertEntity(Entity{CanonicalName: "Anthropic"})
	b, _ := bank.UpsertEntity(Entity{CanonicalName: "ANTHROPIC", Kind: "org"})
	if a.ID != b.ID {
		t.Errorf("case-insensitive conflict should keep same ID; got %q vs %q", a.ID, b.ID)
	}
}

func TestBundleN_GetEntity_ReturnsRowOrNoRows(t *testing.T) {
	bank := newTestBank(t)
	e, _ := bank.UpsertEntity(Entity{CanonicalName: "Sarah Chen"})
	got, err := bank.GetEntity(e.ID)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.CanonicalName != "Sarah Chen" {
		t.Errorf("expected name=Sarah Chen, got %q", got.CanonicalName)
	}
	if _, err := bank.GetEntity("does-not-exist"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("missing id should return sql.ErrNoRows, got %v", err)
	}
}

func TestBundleN_ListEntities(t *testing.T) {
	bank := newTestBank(t)
	bank.UpsertEntity(Entity{CanonicalName: "Charlie"})
	bank.UpsertEntity(Entity{CanonicalName: "alice"})
	bank.UpsertEntity(Entity{CanonicalName: "Bob"})
	got, err := bank.ListEntities(0)
	if err != nil {
		t.Fatalf("ListEntities: %v", err)
	}
	// Fatalf (not Errorf) so a short slice fails cleanly here instead of
	// panicking on the got[i] index below and aborting the rest of the suite.
	if len(got) != 3 {
		t.Fatalf("want 3 entities, got %d", len(got))
	}
	// Alphabetical (case-insensitive) order.
	want := []string{"alice", "Bob", "Charlie"}
	for i, w := range want {
		if got[i].CanonicalName != w {
			t.Errorf("position %d: want %q, got %q", i, w, got[i].CanonicalName)
		}
	}
}

// ── Aliases ─────────────────────────────────────────────────────────────

func TestBundleN_AddAlias_Idempotent(t *testing.T) {
	bank := newTestBank(t)
	e, _ := bank.UpsertEntity(Entity{CanonicalName: "Anthropic"})
	if err := bank.AddAlias(e.ID, "AC", "manual"); err != nil {
		t.Fatalf("AddAlias: %v", err)
	}
	if err := bank.AddAlias(e.ID, "AC", "manual"); err != nil {
		t.Errorf("idempotent AddAlias should not error, got %v", err)
	}
	got, _ := bank.ListAliases(e.ID)
	if len(got) != 1 {
		t.Errorf("expected 1 alias after idempotent inserts, got %d", len(got))
	}
}

func TestBundleN_AddAlias_RejectsEmpty(t *testing.T) {
	bank := newTestBank(t)
	e, _ := bank.UpsertEntity(Entity{CanonicalName: "X"})
	if err := bank.AddAlias(e.ID, "   ", "manual"); err == nil {
		t.Errorf("blank alias should error")
	}
}

func TestBundleN_RemoveAlias(t *testing.T) {
	bank := newTestBank(t)
	e, _ := bank.UpsertEntity(Entity{CanonicalName: "Anthropic"})
	bank.AddAlias(e.ID, "AC", "manual")
	bank.AddAlias(e.ID, "claude.ai", "manual")
	bank.RemoveAlias(e.ID, "AC")
	got, _ := bank.ListAliases(e.ID)
	if len(got) != 1 || got[0].Alias != "claude.ai" {
		t.Errorf("expected only 'claude.ai' left, got %v", got)
	}
}

func TestBundleN_DeleteEntity_CascadesAliases(t *testing.T) {
	bank := newTestBank(t)
	e, _ := bank.UpsertEntity(Entity{CanonicalName: "TempCorp"})
	bank.AddAlias(e.ID, "TC", "manual")
	if err := bank.DeleteEntity(e.ID); err != nil {
		t.Fatalf("DeleteEntity: %v", err)
	}
	// Cascade: aliases must be gone.
	aliases, _ := bank.ListAliases(e.ID)
	if len(aliases) != 0 {
		t.Errorf("expected aliases cascaded on entity delete, got %v", aliases)
	}
}

// ── ResolveAlias ────────────────────────────────────────────────────────

func TestBundleN_ResolveAlias_CaseInsensitive(t *testing.T) {
	bank := newTestBank(t)
	e, _ := bank.UpsertEntity(Entity{CanonicalName: "Anthropic"})
	bank.AddAlias(e.ID, "AC", "manual")
	for _, query := range []string{"AC", "ac", "Ac", "aC"} {
		got, err := bank.ResolveAlias(query)
		if err != nil {
			t.Fatalf("ResolveAlias(%q): %v", query, err)
		}
		if len(got) != 1 || got[0] != "Anthropic" {
			t.Errorf("ResolveAlias(%q): want [Anthropic], got %v", query, got)
		}
	}
}

func TestBundleN_ResolveAlias_AmbiguousReturnsAll(t *testing.T) {
	// "Apple" can mean the fruit OR the company. Both canonicals come back.
	bank := newTestBank(t)
	fruit, _ := bank.UpsertEntity(Entity{CanonicalName: "Apple (fruit)"})
	company, _ := bank.UpsertEntity(Entity{CanonicalName: "Apple Inc"})
	bank.AddAlias(fruit.ID, "Apple", "manual")
	bank.AddAlias(company.ID, "Apple", "manual")
	got, _ := bank.ResolveAlias("Apple")
	if len(got) != 2 {
		t.Errorf("expected 2 canonicals for ambiguous alias, got %d: %v", len(got), got)
	}
}

func TestBundleN_ResolveAlias_EmptyReturnsNothing(t *testing.T) {
	bank := newTestBank(t)
	got, _ := bank.ResolveAlias("")
	if len(got) != 0 {
		t.Errorf("empty alias should return nothing, got %v", got)
	}
}

// ── ExpandQueryViaAliases ───────────────────────────────────────────────

func TestBundleN_ExpandQuery_UnigramMatch(t *testing.T) {
	bank := newTestBank(t)
	e, _ := bank.UpsertEntity(Entity{CanonicalName: "Anthropic"})
	bank.AddAlias(e.ID, "AC", "manual")

	got, err := bank.ExpandQueryViaAliases("what does AC pay engineers?")
	if err != nil {
		t.Fatalf("ExpandQueryViaAliases: %v", err)
	}
	if len(got) != 1 || got[0] != "Anthropic" {
		t.Errorf("expected expansion to [Anthropic], got %v", got)
	}
}

func TestBundleN_ExpandQuery_MultiwordAlias(t *testing.T) {
	bank := newTestBank(t)
	e, _ := bank.UpsertEntity(Entity{CanonicalName: "Sarah Chen"})
	bank.AddAlias(e.ID, "the boss", "manual")

	got, _ := bank.ExpandQueryViaAliases("what did the boss say about the deadline?")
	if len(got) != 1 || got[0] != "Sarah Chen" {
		t.Errorf("expected expansion to [Sarah Chen] via multi-word alias, got %v", got)
	}
}

func TestBundleN_ExpandQuery_NoMatchReturnsNil(t *testing.T) {
	bank := newTestBank(t)
	bank.UpsertEntity(Entity{CanonicalName: "Acme"})
	got, _ := bank.ExpandQueryViaAliases("totally unrelated query")
	if len(got) != 0 {
		t.Errorf("expected nil expansion when no alias matches, got %v", got)
	}
}

func TestBundleN_ExpandQuery_EmptyInput(t *testing.T) {
	bank := newTestBank(t)
	got, _ := bank.ExpandQueryViaAliases("")
	if len(got) != 0 {
		t.Errorf("empty input should return nil, got %v", got)
	}
}

func TestBundleN_ExpandQuery_DedupsRepeats(t *testing.T) {
	// If an alias appears twice in the query, the canonical should only
	// surface once.
	bank := newTestBank(t)
	e, _ := bank.UpsertEntity(Entity{CanonicalName: "Anthropic"})
	bank.AddAlias(e.ID, "AC", "manual")

	got, _ := bank.ExpandQueryViaAliases("AC said AC will pay AC")
	if len(got) != 1 {
		t.Errorf("expected dedup'd expansion, got %v", got)
	}
}
