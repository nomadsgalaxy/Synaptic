// bank_migration_test.go — schema migration + verification tests.
//
// Per handoff "schema migration not applied 2026-05-10" — these tests
// exercise the boot path that the user's installation hit silently.
// Goal: any future migration regression fails LOUDLY at boot rather
// than letting Phase 1/5/7/9 silent-zero.
package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// TestMigrations_FreshDB starts from an empty file and asserts every
// expected column + table exists after NewBank returns.
func TestMigrations_FreshDB(t *testing.T) {
	dir := t.TempDir()
	bank, err := NewBank(filepath.Join(dir, "fresh.db"))
	if err != nil {
		t.Fatalf("NewBank fresh: %v", err)
	}
	t.Cleanup(func() { bank.Close() })

	for table, cols := range expectedColumns {
		for _, col := range cols {
			if !columnExists(t, bank.db, table, col) {
				t.Errorf("fresh DB missing %s.%s", table, col)
			}
		}
	}
	for _, table := range expectedTables {
		if !tableExists(t, bank.db, table) {
			t.Errorf("fresh DB missing table %s", table)
		}
	}
}

// TestMigrations_v1ToV2 simulates the user's exact failure mode: an
// existing DB with the OLD schema (memories table missing the dream-
// pipeline columns + no cross_region_links table). NewBank must detect
// the gap and migrate cleanly without data loss.
func TestMigrations_v1ToV2(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "v1.db")

	// Hand-write a v1-shaped DB. Mirrors the .schema output captured in
	// the user's situation report.
	raw, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open v1 db: %v", err)
	}
	v1Schema := `
CREATE TABLE memories (
    id TEXT PRIMARY KEY,
    text TEXT NOT NULL,
    tags TEXT NOT NULL DEFAULT '[]',
    region_hint TEXT NOT NULL DEFAULT '',
    adapter_id TEXT NOT NULL DEFAULT '',
    session_id TEXT NOT NULL DEFAULT '',
    enriched_text TEXT NOT NULL DEFAULT '',
    light_encoded INTEGER NOT NULL DEFAULT 0,
    nightly_consolidated INTEGER NOT NULL DEFAULT 0,
    oracle_augmented INTEGER NOT NULL DEFAULT 0,
    source TEXT NOT NULL DEFAULT 'adapter',
    merged_from TEXT NOT NULL DEFAULT '[]',
    deleted_at TEXT NOT NULL DEFAULT '',
    sensitive INTEGER NOT NULL DEFAULT 0,
    sensitive_checked_at TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE memory_maps (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    type TEXT NOT NULL,
    schema_text TEXT NOT NULL DEFAULT '',
    anchor_tags TEXT NOT NULL DEFAULT '[]',
    source TEXT NOT NULL DEFAULT 'inferred',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE nightly_runs (
    id TEXT PRIMARY KEY,
    started_at TEXT NOT NULL,
    finished_at TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    stats_json TEXT NOT NULL DEFAULT '{}',
    narrative TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
CREATE TABLE research_cache (
    topic TEXT PRIMARY KEY,
    map_id TEXT NOT NULL DEFAULT '',
    payload TEXT NOT NULL DEFAULT '',
    fetched_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);
INSERT INTO memories (id, text, created_at, updated_at) VALUES
    ('keep-me-1', 'live memory one', '2026-05-09T00:00:00Z', '2026-05-09T00:00:00Z'),
    ('keep-me-2', 'live memory two', '2026-05-09T00:00:00Z', '2026-05-09T00:00:00Z');
`
	if _, err := raw.Exec(v1Schema); err != nil {
		t.Fatalf("seed v1 schema: %v", err)
	}
	raw.Close()

	// Now open via NewBank — migrations must add the missing columns + table.
	bank, err := NewBank(dbPath)
	if err != nil {
		t.Fatalf("NewBank v1→v2: %v", err)
	}
	t.Cleanup(func() { bank.Close() })

	// Verify every expected column is present.
	for table, cols := range expectedColumns {
		for _, col := range cols {
			if !columnExists(t, bank.db, table, col) {
				t.Errorf("post-migration missing %s.%s", table, col)
			}
		}
	}
	if !tableExists(t, bank.db, "cross_region_links") {
		t.Errorf("post-migration missing cross_region_links table")
	}

	// Data preservation: the two seeded memories must still be there.
	mems, err := bank.ListMemoriesWith(MemoryListOpts{Limit: 100})
	if err != nil {
		t.Fatalf("list after migration: %v", err)
	}
	if len(mems) != 2 {
		t.Errorf("expected 2 memories preserved across migration, got %d", len(mems))
	}
	// recall_strength default should kick in — every old row reads as 1.0.
	for _, m := range mems {
		if m.RecallStrength != 1.0 {
			t.Errorf("memory %s: expected recall_strength=1.0 after migration, got %g", m.ID, m.RecallStrength)
		}
	}
}

// TestMigrations_Idempotent runs NewBank twice on the same file and
// asserts the second open still passes verification cleanly.
func TestMigrations_Idempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "idempotent.db")

	bank1, err := NewBank(dbPath)
	if err != nil {
		t.Fatalf("first NewBank: %v", err)
	}
	bank1.Close()

	bank2, err := NewBank(dbPath)
	if err != nil {
		t.Fatalf("second NewBank failed (migrations not idempotent): %v", err)
	}
	t.Cleanup(func() { bank2.Close() })

	// Verify schema is still complete after re-opening.
	for table, cols := range expectedColumns {
		for _, col := range cols {
			if !columnExists(t, bank2.db, table, col) {
				t.Errorf("after second open, missing %s.%s", table, col)
			}
		}
	}
}

// TestVerifySchema_DetectsMissingColumns synthesizes a deliberately
// incomplete DB (column dropped via a manual table rebuild) and asserts
// verifyExpectedSchema surfaces a clear error rather than letting
// NewBank succeed silently.
//
// This is the failure mode the user hit: a DB where the morning
// migrations claimed success but didn't actually persist for some
// path/timing reason. We need to fail LOUD when that happens so the
// pipeline isn't asked to run against a broken target.
func TestVerifySchema_DetectsMissingColumns(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "bad.db")

	// Create a memories table that's missing recall_strength.
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, err = raw.Exec(`
CREATE TABLE memories (
    id TEXT PRIMARY KEY, text TEXT, tags TEXT, region_hint TEXT,
    adapter_id TEXT, session_id TEXT, enriched_text TEXT,
    light_encoded INTEGER, nightly_consolidated INTEGER, oracle_augmented INTEGER,
    source TEXT, merged_from TEXT, deleted_at TEXT, sensitive INTEGER,
    sensitive_checked_at TEXT, synthesis_source_ids TEXT, schema_source_ids TEXT,
    last_recalled_at TEXT, /* NO recall_strength */ deleted_reason TEXT,
    created_at TEXT, updated_at TEXT
);
`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Directly invoke verifyExpectedSchema — we can't go through NewBank
	// because its migration step would re-add the column. The verifier
	// must catch a real gap on its own.
	err = verifyExpectedSchema(raw)
	raw.Close()
	if err == nil {
		t.Fatalf("expected verifyExpectedSchema to detect missing recall_strength")
	}
	if !strings.Contains(err.Error(), "recall_strength") {
		t.Errorf("error should name the missing column, got: %v", err)
	}
}

// TestVerifySchema_DetectsMissingTable checks the table-presence half.
func TestVerifySchema_DetectsMissingTable(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "no-xrl.db")

	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Empty DB — every table is missing. We just check that the verifier
	// reports cross_region_links explicitly.
	err = verifyExpectedSchema(raw)
	raw.Close()
	if err == nil {
		t.Fatalf("expected verifyExpectedSchema to detect missing tables")
	}
	if !strings.Contains(err.Error(), "cross_region_links") {
		t.Errorf("error should name cross_region_links specifically, got: %v", err)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// helpers
// ──────────────────────────────────────────────────────────────────────────

func columnExists(t *testing.T, db *sql.DB, table, col string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if name == col {
			return true
		}
	}
	return false
}

func tableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	var name string
	err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
	).Scan(&name)
	return err == nil && name == table
}
