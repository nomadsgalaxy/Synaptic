// SQLite "fallback bank" (B6) — durable storage for AI-reported memories.
//
// Synaptic is normally a *visualizer* of an external memory system
// (third-party stores, plain Markdown, etc.). The Bank gives it the
// option to also be the system itself: when the MCP adapter calls
// `report_memory_save`, or any adapter emits a `memory_added` event with
// text in the payload, we durably persist it here.
//
// Goals:
//   - Pure Go (CGO=0 stays; we use modernc.org/sqlite, not mattn's CGO driver).
//   - Cheap: a single file under SD_BANK_PATH; default lives next to the data dir.
//   - Safe to attach AFTER startup: SD Core works fine without the bank;
//     all writes are best-effort and never block the WS fanout.
//   - Readable: tags are stored as JSON arrays so the dashboard can use them
//     for region classification; ISO 8601 timestamps everywhere.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const bankSchema = `
CREATE TABLE IF NOT EXISTS memories (
    id                    TEXT PRIMARY KEY,
    text                  TEXT NOT NULL,                -- raw, immutable
    tags                  TEXT NOT NULL DEFAULT '[]',   -- JSON array
    region_hint           TEXT NOT NULL DEFAULT '',
    adapter_id            TEXT NOT NULL DEFAULT '',
    session_id            TEXT NOT NULL DEFAULT '',
    enriched_text         TEXT NOT NULL DEFAULT '',     -- Tier 1/sd_correct writes here; raw text untouched
    light_encoded         INTEGER NOT NULL DEFAULT 0,
    nightly_consolidated  INTEGER NOT NULL DEFAULT 0,
    oracle_augmented      INTEGER NOT NULL DEFAULT 0,
    source                TEXT NOT NULL DEFAULT 'adapter', -- adapter|mcp_direct|web_research|oracle_context
    merged_from           TEXT NOT NULL DEFAULT '[]',   -- JSON array of trace ids merged into this one
    deleted_at            TEXT NOT NULL DEFAULT '',     -- '' = live; ISO timestamp = soft-deleted
    sensitive             INTEGER NOT NULL DEFAULT 0,   -- 1 = NEVER send to Tier 3 / Oracle / research / internet
    sensitive_checked_at  TEXT NOT NULL DEFAULT '',     -- ISO timestamp of last AI sensitivity scan; '' = never scanned
    created_at            TEXT NOT NULL,
    updated_at            TEXT NOT NULL
);
-- NOTE: indices that reference Phase 5 columns (light_encoded, deleted_at,
-- sensitive, etc.) MUST live in bankIndices below — they run AFTER
-- migrateMemoriesColumns has added the columns to old databases.
CREATE INDEX IF NOT EXISTS idx_memories_created_at ON memories(created_at);
CREATE INDEX IF NOT EXISTS idx_memories_adapter    ON memories(adapter_id);
CREATE TABLE IF NOT EXISTS embeddings (
    text_hash   TEXT PRIMARY KEY,
    vector_json TEXT NOT NULL,
    model       TEXT NOT NULL,
    created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_embeddings_model ON embeddings(model);

-- ── Phase 5: MemoryMap system ────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS memory_maps (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    type        TEXT NOT NULL,                    -- project|person|technology|organization|concept
    schema_text TEXT NOT NULL DEFAULT '',
    anchor_tags TEXT NOT NULL DEFAULT '[]',       -- JSON array
    source      TEXT NOT NULL DEFAULT 'inferred', -- inferred|mcp_direct|oracle_context|web_research
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    -- [R13 redesign] map-proposal lifecycle. Fresh installs get these
    -- directly; legacy installs pick them up via migrateMemoryMapsColumns.
    status            TEXT NOT NULL DEFAULT 'accepted',  -- proposed|accepted|dismissed
    generated_by      TEXT NOT NULL DEFAULT 'user',      -- user|r13_redesign|phase_4|legacy
    generation_phase  TEXT NOT NULL DEFAULT ''           -- e.g. '4' | '8.7' | 'user_triggered'
);
CREATE INDEX IF NOT EXISTS idx_memory_maps_type   ON memory_maps(type);
-- idx_memory_maps_status + idx_memory_maps_name_type_live are created in
-- migrateMemoryMapsColumns AFTER the status + archived columns are present.
-- The legacy (non-partial) idx_memory_maps_name_type also lived here, but it
-- blocked re-creating an accepted map of the same (name, type) when an old
-- archived row still occupied the slot — see migration step in
-- migrateMemoryMapsColumns that swaps it for the partial-on-archived=0 index.

CREATE TABLE IF NOT EXISTS map_associations (
    id           TEXT PRIMARY KEY,
    from_map_id  TEXT NOT NULL REFERENCES memory_maps(id) ON DELETE CASCADE,
    to_map_id    TEXT NOT NULL REFERENCES memory_maps(id) ON DELETE CASCADE,
    association  TEXT NOT NULL,                   -- uses_technology|worked_on|collaborated_with|related_to|depends_on|...
    weight       REAL NOT NULL DEFAULT 1.0,
    created_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_assoc_from ON map_associations(from_map_id);
CREATE INDEX IF NOT EXISTS idx_assoc_to   ON map_associations(to_map_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_assoc_unique
    ON map_associations(from_map_id, to_map_id, association);

CREATE TABLE IF NOT EXISTS map_traces (
    map_id     TEXT NOT NULL REFERENCES memory_maps(id) ON DELETE CASCADE,
    trace_id   TEXT NOT NULL REFERENCES memories(id)    ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    PRIMARY KEY (map_id, trace_id)
);
CREATE INDEX IF NOT EXISTS idx_map_traces_trace ON map_traces(trace_id);

-- ── Phase 5: Lexicon (tag co-occurrence) ─────────────────────────────────
CREATE TABLE IF NOT EXISTS lexicon (
    tag_a        TEXT NOT NULL,
    tag_b        TEXT NOT NULL,
    cooccurrence INTEGER NOT NULL DEFAULT 0,
    weight       REAL    NOT NULL DEFAULT 0.0,
    updated_at   TEXT    NOT NULL,
    PRIMARY KEY (tag_a, tag_b)
);
CREATE INDEX IF NOT EXISTS idx_lexicon_weight ON lexicon(weight DESC);

-- ── Phase 5: Audit log (every write op) ──────────────────────────────────
CREATE TABLE IF NOT EXISTS audit_log (
    id          TEXT PRIMARY KEY,
    operation   TEXT NOT NULL,    -- create|update|delete|merge|oracle_call|research|nightly_step|...
    entity_type TEXT NOT NULL,    -- trace|memory_map|association|lexicon|research_cache|...
    entity_id   TEXT NOT NULL DEFAULT '',
    before_json TEXT NOT NULL DEFAULT '',
    after_json  TEXT NOT NULL DEFAULT '',
    reason      TEXT NOT NULL DEFAULT '',
    adapter_id  TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_log(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_entity  ON audit_log(entity_type, entity_id);
CREATE INDEX IF NOT EXISTS idx_audit_op      ON audit_log(operation);

-- ── Phase 5: Web research cache ──────────────────────────────────────────
CREATE TABLE IF NOT EXISTS research_cache (
    topic      TEXT PRIMARY KEY,
    map_id     TEXT NOT NULL DEFAULT '',
    payload    TEXT NOT NULL DEFAULT '',  -- JSON: source urls, summary, raw bullets, etc.
    fetched_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_research_expires ON research_cache(expires_at);

-- ── Phase 5: Daily Oracle token budget ───────────────────────────────────
CREATE TABLE IF NOT EXISTS token_budget (
    date        TEXT PRIMARY KEY,   -- YYYY-MM-DD (local)
    tokens_used INTEGER NOT NULL DEFAULT 0,
    updated_at  TEXT NOT NULL
);

-- Per-tier / per-provider token spend. Filled by AddTokensUsedV2; the
-- aggregate token_budget table above continues to be updated for
-- back-compat with the dashboard's daily-bar chart. The external column
-- is derived from provider at write time (never user-supplied).
CREATE TABLE IF NOT EXISTS token_budget_lines (
    date        TEXT NOT NULL,        -- YYYY-MM-DD
    tier        TEXT NOT NULL,        -- tier_embedding | tier1_provider | tier2_provider | tier3_provider | unknown
    provider    TEXT NOT NULL,        -- ollama | openai | anthropic | custom | disabled | unknown
    model       TEXT NOT NULL DEFAULT '', -- specific model name within the provider (e.g. "llama3.1:8b" / "gpt-4o-mini"); '' for legacy pre-2026-05-12 rows
    external    INTEGER NOT NULL DEFAULT 0,
    tokens_in   INTEGER NOT NULL DEFAULT 0,
    tokens_out  INTEGER NOT NULL DEFAULT 0,
    updated_at  TEXT NOT NULL,
    PRIMARY KEY (date, tier, provider, model)
);
CREATE INDEX IF NOT EXISTS idx_token_budget_lines_date     ON token_budget_lines(date);
CREATE INDEX IF NOT EXISTS idx_token_budget_lines_external ON token_budget_lines(external);
CREATE INDEX IF NOT EXISTS idx_token_budget_lines_tier     ON token_budget_lines(tier);

-- ── Generic settings (onboarding state, nightly schedule, future toggles) ──
CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,         -- arbitrary JSON or scalar string
    updated_at TEXT NOT NULL
);

-- ── Maps overrides (per-installation, server-side replacement for the
-- dashboard's localStorage map-overrides blob). Lets multi-device users
-- share Project / Entity / Topic categorisations of memory maps.
CREATE TABLE IF NOT EXISTS map_overrides (
    map_key      TEXT PRIMARY KEY COLLATE NOCASE,
    category     TEXT NOT NULL DEFAULT '', -- project | entity | topic | ''
    alias        TEXT NOT NULL DEFAULT '', -- display-name override; '' = use raw tag
    archived     INTEGER NOT NULL DEFAULT 0,
    updated_at   TEXT NOT NULL,
    updated_by   TEXT NOT NULL DEFAULT ''  -- adapter_id of the writer
);
CREATE INDEX IF NOT EXISTS idx_map_overrides_archived ON map_overrides(archived);
CREATE INDEX IF NOT EXISTS idx_map_overrides_category ON map_overrides(category);

-- ── Phase 5: NightlyRunner consolidation runs (one row per dream cycle) ──
CREATE TABLE IF NOT EXISTS nightly_runs (
    id          TEXT PRIMARY KEY,             -- "nightly-YYYY-MM-DD-HH-MM"
    started_at  TEXT NOT NULL,
    finished_at TEXT NOT NULL DEFAULT '',     -- empty = in_progress
    status      TEXT NOT NULL,                -- in_progress | completed | failed | partial
    stats_json  TEXT NOT NULL DEFAULT '{}',   -- schema-versioned NightlyStats blob
    narrative   TEXT NOT NULL DEFAULT '',     -- Tier 2 prose summary; optional
    model       TEXT NOT NULL DEFAULT '',     -- provider name used for narrative
    error       TEXT NOT NULL DEFAULT '',     -- populated when status='failed'
    created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_nightly_runs_started ON nightly_runs(started_at DESC);
CREATE INDEX IF NOT EXISTS idx_nightly_runs_status  ON nightly_runs(status);

-- ── Wave 7b2: Sidecar lifecycle tracking ────────────────────────────────
-- Per-managed-service state for the on-demand sidecar lifecycle (see
-- CODE_HANDOFF — On-demand Tier 2-3 sidecar lifecycle 2026-05-10). One
-- row per service that the lifecycle manager owns. State enum:
-- stopped | starting | running | idle | stopping | crashed.
CREATE TABLE IF NOT EXISTS service_lifecycle (
    service_name        TEXT PRIMARY KEY,
    current_state       TEXT NOT NULL DEFAULT 'stopped',
    state_changed_at    TEXT NOT NULL DEFAULT '',
    last_used_at        TEXT NOT NULL DEFAULT '',
    last_workflow       TEXT NOT NULL DEFAULT '',
    uptime_today_sec    INTEGER NOT NULL DEFAULT 0,
    cold_starts_today   INTEGER NOT NULL DEFAULT 0,
    crash_reason        TEXT NOT NULL DEFAULT '',
    updated_at          TEXT NOT NULL
);

-- ── Wave 8e: Provider benchmark cache ────────────────────────────────────
-- One row per (provider_kind, model, compression) tuple. Captures the
-- measured tokens-per-sec rate from a small benchmark Chat call so the
-- Dream Journal can render an honest "your next nightly will take ~Xh"
-- estimate, especially load-bearing for AirLLM (1-3 tok/s on CPU).
--
-- The composite key with REPLACE-on-conflict semantics means each
-- tuple has exactly one row — re-benchmarking the same (kind, model,
-- compression) overwrites the prior measurement rather than appending
-- history. We only care about the latest rate for estimation; a long
-- history would just bloat the table.
--
-- compression is "" for providers that don't have a compression flag
-- (Ollama, OpenAI, Anthropic). AirLLM stores 4bit/8bit/none.
CREATE TABLE IF NOT EXISTS provider_benchmark (
    provider_kind     TEXT NOT NULL,
    model             TEXT NOT NULL,
    compression       TEXT NOT NULL DEFAULT '',
    elapsed_ms        INTEGER NOT NULL,
    prompt_tokens     INTEGER NOT NULL,
    completion_tokens INTEGER NOT NULL,
    tokens_per_sec    REAL    NOT NULL,
    sample_excerpt    TEXT    NOT NULL DEFAULT '',
    benchmarked_at    TEXT    NOT NULL,
    PRIMARY KEY (provider_kind, model, compression)
);

-- ── Wave 5: Hook event log ───────────────────────────────────────────────
-- One row per forwarded hook event (Claude Code hooks, MCP server, OTel
-- collector, etc.). Distinct from audit_log: audit_log captures Synaptic's
-- own state changes; hook_events captures *upstream* adapter signals so
-- the dashboard can correlate "what was the agent doing when this memory
-- got saved." Payload is truncated to ~2KB at write time.
CREATE TABLE IF NOT EXISTS hook_events (
    id          TEXT PRIMARY KEY,
    adapter_id  TEXT NOT NULL DEFAULT '',
    session_id  TEXT NOT NULL DEFAULT '',
    event_type  TEXT NOT NULL,
    payload     TEXT NOT NULL DEFAULT '',  -- truncated JSON
    created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_hook_events_created  ON hook_events(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_hook_events_adapter  ON hook_events(adapter_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_hook_events_session  ON hook_events(session_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_hook_events_type     ON hook_events(event_type, created_at DESC);

-- ── Phase 0b deep-encoding history (2026-05-10 handoff) ──
-- One row per Tier 2 enrichment attempt. Lets the user audit "this got
-- re-enriched 3 times — here's what changed each time" via trace detail.
-- The error column captures Tier 2 failures so the pipeline can apply a
-- retry budget (3 strikes → memory marked failed, dropped from queue).
CREATE TABLE IF NOT EXISTS deep_encoding_log (
    memory_id    TEXT NOT NULL,
    model        TEXT NOT NULL,
    started_at   TEXT NOT NULL,
    finished_at  TEXT NOT NULL,
    tokens_in    INTEGER NOT NULL DEFAULT 0,
    tokens_out   INTEGER NOT NULL DEFAULT 0,
    summary_diff TEXT NOT NULL DEFAULT '',
    trigger      TEXT NOT NULL DEFAULT '',
    error        TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (memory_id, started_at)
);
CREATE INDEX IF NOT EXISTS idx_de_log_memory ON deep_encoding_log(memory_id);

-- ── R10 bi-temporal supersede: superseding_id REPLACES superseded_id ──
-- Append-only edge table. Raw memory text is NEVER mutated (R10 immutability).
-- Recall / list endpoints default to current-truth view: exclude any memory
-- whose id appears in superseded_id where valid_from <= now and no later row
-- has unsuperseded it. Time-travel reads pass an as_of timestamp.
--
-- Note: a memory can be superseded MULTIPLE times (chains: A→B, B→C). The
-- current-truth view walks the chain forward to the leaf at as_of.
CREATE TABLE IF NOT EXISTS memory_supersedes (
    superseding_id TEXT NOT NULL,
    superseded_id  TEXT NOT NULL,
    valid_from     TEXT NOT NULL,         -- RFC3339; defaults to now() at insert
    reason         TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    PRIMARY KEY (superseding_id, superseded_id)
);
CREATE INDEX IF NOT EXISTS idx_msup_superseded  ON memory_supersedes(superseded_id);
CREATE INDEX IF NOT EXISTS idx_msup_superseding ON memory_supersedes(superseding_id);
CREATE INDEX IF NOT EXISTS idx_msup_valid_from  ON memory_supersedes(valid_from);

-- ── Dream pipeline: cross-region creative-recombination links (Phase 7) ──
CREATE TABLE IF NOT EXISTS cross_region_links (
    memory_a_id   TEXT NOT NULL,
    memory_b_id   TEXT NOT NULL,
    similarity    REAL NOT NULL,
    tag_overlap   REAL NOT NULL,
    discovered_at TEXT NOT NULL,
    run_id        TEXT NOT NULL,
    PRIMARY KEY (memory_a_id, memory_b_id)
);
CREATE INDEX IF NOT EXISTS idx_xrl_a   ON cross_region_links(memory_a_id);
CREATE INDEX IF NOT EXISTS idx_xrl_b   ON cross_region_links(memory_b_id);
CREATE INDEX IF NOT EXISTS idx_xrl_run ON cross_region_links(run_id);
`

// bankIndices is applied AFTER migrateMemoriesColumns has run, so indices
// over Phase 5 columns succeed on databases that pre-date P5-3.
const bankIndices = `
CREATE INDEX IF NOT EXISTS idx_memories_lifecycle ON memories(light_encoded, nightly_consolidated);
CREATE INDEX IF NOT EXISTS idx_memories_deleted   ON memories(deleted_at);
CREATE INDEX IF NOT EXISTS idx_memories_sensitive ON memories(sensitive);

-- FTS5 virtual table for BM25 keyword search. Combined with the
-- existing cosine retrieval via Reciprocal Rank Fusion in recall.go to
-- give hybrid (semantic + lexical) search. Verbose natural-language
-- queries score well on cosine; literal keyword matches score well on
-- BM25; RRF lets a candidate that's strong on either pathway surface.
--
-- Standalone FTS5 table (not 'content=' linked) so writes are explicit
-- via triggers — keeps the FTS index decoupled from any modernc.org/
-- sqlite quirks around content-linked virtual tables.
CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
    memory_id UNINDEXED,
    text,
    enriched_text,
    tags
);
-- Sync triggers. AFTER INSERT / AFTER UPDATE / AFTER DELETE keep the
-- FTS table in lockstep with memories. Sensitive=true rows are still
-- indexed (egress filtering happens at /recall response time, not
-- at index time; this lets the user search their own sensitive
-- memories via the local UI).
CREATE TRIGGER IF NOT EXISTS memories_fts_ai AFTER INSERT ON memories BEGIN
    INSERT INTO memories_fts(memory_id, text, enriched_text, tags)
    VALUES (NEW.id, NEW.text, NEW.enriched_text, NEW.tags);
END;
CREATE TRIGGER IF NOT EXISTS memories_fts_ad AFTER DELETE ON memories BEGIN
    DELETE FROM memories_fts WHERE memory_id = OLD.id;
END;
CREATE TRIGGER IF NOT EXISTS memories_fts_au AFTER UPDATE ON memories BEGIN
    DELETE FROM memories_fts WHERE memory_id = OLD.id;
    INSERT INTO memories_fts(memory_id, text, enriched_text, tags)
    VALUES (NEW.id, NEW.text, NEW.enriched_text, NEW.tags);
END;

-- v2.7 Bundle N — Entity resolution. A small registry that maps
-- aliases ("the boss", "AC", "OAuth2") onto canonical names
-- ("Sarah Chen", "Anthropic", "OAuth 2.0"). Recall expands queries
-- through the alias table so "what does AC pay?" resolves the same
-- memories as "what does Anthropic pay?". Closes the
-- entity-graph parity gap with a deliberately minimal schema —
-- canonical name + alias edges, no embedding store, no co-reference
-- resolution. Phase-6-time LLM extraction can populate this later;
-- callers can also seed it directly via the bank methods.
CREATE TABLE IF NOT EXISTS entities (
    id              TEXT PRIMARY KEY,
    canonical_name  TEXT NOT NULL,
    kind            TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_entities_canonical
    ON entities(canonical_name COLLATE NOCASE);

CREATE TABLE IF NOT EXISTS entity_aliases (
    entity_id   TEXT NOT NULL,
    alias       TEXT NOT NULL,
    source      TEXT NOT NULL DEFAULT 'manual',
    created_at  TEXT NOT NULL,
    PRIMARY KEY (entity_id, alias),
    FOREIGN KEY (entity_id) REFERENCES entities(id) ON DELETE CASCADE
);
-- alias lookup hot path — recall expansion hits this on every query.
CREATE INDEX IF NOT EXISTS idx_entity_aliases_alias
    ON entity_aliases(alias COLLATE NOCASE);
`

// Bank wraps a SQLite file. Methods are safe for concurrent use.
type Bank struct {
	db   *sql.DB
	path string
	// Serialize writes through a mutex so we don't get SQLITE_BUSY under
	// bursty memory_added storms; modernc.org/sqlite tolerates concurrent
	// reads but a single writer per connection avoids surprises.
	wmu sync.Mutex
	// v2.7 Bundle K — provider-resolver closure for inline dedup at
	// SaveMemory time. main.go wires this to router.ForEmbedding so the
	// dedup path picks up the same provider as /recall and synapse. nil
	// means "no dedup" — SaveMemory short-circuits the duplicate check.
	// Hot-swappable: a router.ReplaceTier swap is observed at the next
	// SaveMemory call automatically.
	dedupProvider func() LLMProvider
	// Emit, when non-nil, is invoked by mutation methods (SaveMemory,
	// UpdateMemory, MarkSensitiveScan, AppendAudit, SaveResearchCache) to
	// publish dot-namespaced control-channel events. main.go wires this to
	// hub.Fanout. Tests typically leave it nil — set it from a recording
	// closure to inspect what would have been published.
	//
	// Convention: use snake_case-with-dots for type ("sensitive.flipped",
	// "audit.appended", "research.cached"); use "sd-core-<subsystem>" for
	// adapterID; payload contains opaque IDs only — no memory bodies, no
	// secrets, no env-var values.
	Emit func(eventType, adapterID string, payload map[string]interface{})
}

// emit is a nil-safe wrapper so callers don't have to repeat the guard.
func (b *Bank) emit(eventType, adapterID string, payload map[string]interface{}) {
	if b == nil || b.Emit == nil {
		return
	}
	b.Emit(eventType, adapterID, payload)
}

// MemoryRecord is the in-memory shape returned by Bank queries.
//
// `Text` is the immutable raw input. Tier 1 / sd_correct writes refinements
// to `EnrichedText`; the original is preserved forever. Use EnrichedText if
// non-empty when displaying; otherwise fall back to Text.
type MemoryRecord struct {
	ID                  string   `json:"id"`
	Text                string   `json:"text"`
	Tags                []string `json:"tags"`
	RegionHint          string   `json:"region_hint,omitempty"`
	AdapterID           string   `json:"adapter_id,omitempty"`
	SessionID           string   `json:"session_id,omitempty"`
	EnrichedText        string   `json:"enriched_text,omitempty"`
	LightEncoded        bool     `json:"light_encoded"`
	NightlyConsolidated bool     `json:"nightly_consolidated"`
	OracleAugmented     bool     `json:"oracle_augmented"`
	Source              string   `json:"source,omitempty"`
	MergedFrom          []string `json:"merged_from,omitempty"`
	DeletedAt           string   `json:"deleted_at,omitempty"`
	// Sensitive HARD-BLOCKS this memory from any outbound (Tier 3 / Oracle /
	// research) path. Local embeddings + /recall still work. Auto-set when a
	// tag matches SD_SENSITIVE_TAGS; sticky once true unless explicitly cleared.
	Sensitive bool `json:"sensitive"`
	// SensitiveCheckedAt is the most recent time the Layer 2 AI classifier ran
	// against this record. Empty = never scanned. The classifier filters
	// where this is '' OR earlier than `updated_at`.
	SensitiveCheckedAt string `json:"sensitive_checked_at,omitempty"`
	// Dream-pipeline lineage + reinforcement (added 2026-05-09 handoff):
	SynthesisSourceIDs []string `json:"synthesis_source_ids,omitempty"` // Phase 2 cluster members
	SchemaSourceIDs    []string `json:"schema_source_ids,omitempty"`    // Phase 8 source syntheses
	LastRecalledAt     string   `json:"last_recalled_at,omitempty"`     // bumped by /recall
	RecallStrength     float64  `json:"recall_strength,omitempty"`      // Phase 9 reinforcement; default 1.0
	DeletedReason      string   `json:"deleted_reason,omitempty"`       // why deleted_at was set
	// Phase 0b deep-encoding (added 2026-05-10 handoff):
	DeepEncodedAt    string  `json:"deep_encoded_at,omitempty"`    // '' = never; ISO timestamp of last successful enrichment
	DeepEncodedModel string  `json:"deep_encoded_model,omitempty"` // provider+model that wrote the enrichment
	MarkedDirtyAt    string  `json:"marked_dirty_at,omitempty"`    // '' = clean; ISO timestamp when row was queued
	DirtyReason      string  `json:"dirty_reason,omitempty"`       // created|edited|model_upgrade|cascading|periodic|temporal_neighbor|surprise|emotional|tmr_user
	Salience         float64 `json:"salience,omitempty"`           // [R1] 0..1 priority score (default 0.5)
	// [R7] CITATIONS.md #2 — Stickgold & Walker 2010. Tracks the
	// hippocampus→neocortex migration as a memory is reinforced and
	// integrated. Values: episodic (default, recently encoded),
	// consolidating (light_encoded=true, partially processed), semantic
	// (deep_encoded with strong recall — the memory has migrated to
	// "general knowledge" status). Phase 0b + Phase 9 drive transitions.
	ConsolidationStage string `json:"consolidation_stage,omitempty"`
	// [R13] Phase 8.7 Context Memory Formation. CITATIONS.md #18 (Johnson 2005
	// REM context memory). MemoryType categorises the row as
	// episodic|synthesis|schema|context. Context rows carry the additional
	// columns below; they are '' / 0 on non-context rows.
	MemoryType         string `json:"memory_type,omitempty"`          // '' (legacy) or episodic|synthesis|schema|context
	ContextRegion      string `json:"context_region,omitempty"`       // region this context memory covers
	ContextConfidence  string `json:"context_confidence,omitempty"`   // low|medium|high (self-reported by Tier 2)
	ContextUsedAugment bool   `json:"context_used_augment,omitempty"` // [R15] true when low-confidence triggered augment
	// v2.7 Bundle L — Dormant Memories. The brain doesn't time-out memories;
	// it lets them go dormant. A memory with dormant_at != '' is excluded
	// from default /recall (same shape as the R10 supersede filter) but
	// still lives in the bank — callers can resurrect it by passing
	// include_dormant=true or by PATCHing dormant=&false. ExpiresAt is
	// the optional TTL trigger; the nightly dormancy pass moves memories
	// past their expiry into dormant_at. Sensitive memories can't have
	// a TTL set (UpdateMemory rejects).
	ExpiresAt     string `json:"expires_at,omitempty"`
	DormantAt     string `json:"dormant_at,omitempty"`
	DormantReason string `json:"dormant_reason,omitempty"` // ttl|manual|<future-reasons>
	// Iter 25 — fact-dense key-value compression produced by Phase 0b.
	// 3-5× shorter than raw_text while preserving every fact (numbers,
	// dates, names, places, outcomes). Used by /reflect to give the
	// synthesis model a denser view of each candidate, and by Phase 4
	// when synthesizing concept-map schema_text.
	OptimizedText string `json:"optimized_text,omitempty"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

// MarshalJSON emits MemoryRecord with the wave-6 rename aliases per
// `CODE_HANDOFF — Rename dirty to undreamt (2026-05-10).md`. Two extra
// fields ride alongside the legacy ones:
//
//	marked_for_redream_at  — alias for marked_dirty_at
//	redream_reason          — alias for dirty_reason
//
// Internal DB columns + Go field names stay unchanged so the rename has
// zero migration risk; frontends that already read either form
// (transition-period clients) just see both. New clients should prefer
// the *_redream_* names; legacy clients keep working.
func (m MemoryRecord) MarshalJSON() ([]byte, error) {
	type rawMR MemoryRecord
	return json.Marshal(&struct {
		rawMR
		MarkedForRedreamAt string `json:"marked_for_redream_at,omitempty"`
		RedreamReason      string `json:"redream_reason,omitempty"`
	}{
		rawMR:              rawMR(m),
		MarkedForRedreamAt: m.MarkedDirtyAt,
		RedreamReason:      m.DirtyReason,
	})
}

// NewBank opens (or creates) the SQLite file and ensures the schema is present.
// Pass an empty path to disable persistence — returns nil, nil so callers can
// keep going without branching everywhere.
func NewBank(path string) (*Bank, error) {
	if path == "" {
		return nil, nil
	}
	// modernc.org/sqlite uses the driver name "sqlite". The pragmas keep
	// the bank cheap: WAL for concurrent readers, NORMAL sync since this is
	// telemetry-grade data (an OS crash losing the last few seconds of
	// memories is acceptable; the "real" memory system is upstream).
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open bank %s: %w", path, err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(time.Hour)
	if _, err := db.Exec(bankSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init bank schema: %w", err)
	}
	if err := migrateMemoriesColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate memories columns: %w", err)
	}
	if err := migrateResearchCacheColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate research_cache columns: %w", err)
	}
	if err := migrateMemoryMapsColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate memory_maps columns: %w", err)
	}
	if err := migrateNightlyRunsColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate nightly_runs columns: %w", err)
	}
	if err := migrateTokenBudgetLines(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate token_budget_lines: %w", err)
	}
	// Defensive post-migration check. Catches the class of bug where one of
	// the migrations silently no-op'd (e.g. column already existed in a
	// different shape, or a future ALTER got reordered). If anything the
	// pipeline depends on is still missing after migrations claim success,
	// we fail loud HERE rather than letting Phase 1/5/7/9 silent-zero on a
	// schema gap. (Per handoff "schema migration not applied 2026-05-10".)
	if err := verifyExpectedSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema verification: %w", err)
	}
	// Indices over Phase 5 columns must run AFTER migrateMemoriesColumns
	// (CREATE TABLE IF NOT EXISTS is a no-op on old DBs, leaving them
	// without the columns the indices reference).
	if _, err := db.Exec(bankIndices); err != nil {
		db.Close()
		return nil, fmt.Errorf("init bank indices: %w", err)
	}
	// v2.7 Bundle J — backfill the FTS5 table for legacy banks. Triggers
	// keep new writes in sync; this catches rows that existed before
	// memories_fts was created. Idempotent via the WHERE NOT IN clause.
	if _, err := db.Exec(`
		INSERT INTO memories_fts(memory_id, text, enriched_text, tags)
		SELECT id, text, enriched_text, tags FROM memories
		WHERE id NOT IN (SELECT memory_id FROM memories_fts);
	`); err != nil {
		// Non-fatal: log + continue. BM25 search degrades gracefully
		// (returns no hits for un-backfilled rows; cosine half of the
		// hybrid still works).
		log.Printf("bank: FTS5 backfill failed (continuing without keyword index): %v", err)
	} else {
		var ftsRows int
		_ = db.QueryRow(`SELECT COUNT(*) FROM memories_fts`).Scan(&ftsRows)
		log.Printf("bank: FTS5 index ready (%d rows)", ftsRows)
	}
	log.Printf("bank: opened SQLite store at %s", path)
	return &Bank{db: db, path: path}, nil
}

// expectedColumns lists the per-table columns the dream-pipeline depends on
// existing post-migration. Used by verifyExpectedSchema as a defense against
// silent migration failures. Add an entry whenever a phase reads/writes a
// new column so a stale DB gets caught at boot rather than at Phase N.
var expectedColumns = map[string][]string{
	"memories": {
		// dream-pipeline lineage + reinforcement (handoff 2026-05-09)
		"synthesis_source_ids", "schema_source_ids",
		"last_recalled_at", "recall_strength", "deleted_reason",
		// Phase 0b deep-encoding (handoff 2026-05-10)
		"deep_encoded_at", "deep_encoded_model",
		"marked_dirty_at", "dirty_reason", "salience",
		// R7 consolidation stage (handoff 2026-05-10c)
		"consolidation_stage",
		// R13 Phase 8.7 context memory (wave 5)
		"memory_type", "context_region", "context_confidence", "context_used_augment",
	},
	"memory_maps": {
		// Phase 4 metadata (handoff 2026-05-09)
		"archived", "top_region", "last_touched_at",
	},
	"nightly_runs": {
		// Phase 12 dream entry (handoff 2026-05-09)
		"dream_entry", "dream_entry_archetype",
		// Phase 0b resumability (handoff 2026-05-10)
		"deep_encoded_this_run", "deep_encoded_remaining", "deep_encoded_cursor",
		// Crash recovery (handoff "dream cycle resumption + crash recovery 2026-05-10")
		"heartbeat_at", "interrupt_reason",
	},
	"research_cache": {
		// research detail manual/auto attribution (handoff 2026-05-09)
		"fetched_by",
	},
}

// expectedTables lists tables the pipeline depends on. CREATE TABLE IF NOT
// EXISTS is part of bankSchema, but if that block ever silently no-op'd
// against an older DB this verifier surfaces the gap instead of letting
// Phase 7 try to write to a non-existent cross_region_links.
var expectedTables = []string{
	"memories", "embeddings", "memory_maps", "map_associations", "map_traces",
	"lexicon", "audit_log", "research_cache", "token_budget",
	"token_budget_lines", "settings", "map_overrides", "nightly_runs",
	"cross_region_links",
	// Phase 0b deep-encoding history (handoff 2026-05-10)
	"deep_encoding_log",
	// Wave 5: forwarded hook event log
	"hook_events",
	// Wave 7b2: sidecar lifecycle tracking
	"service_lifecycle",
	// Wave 8e: provider benchmark cache (tokens-per-sec per provider tuple)
	"provider_benchmark",
}

// verifyExpectedSchema runs after migrations and checks every column +
// table the pipeline relies on. Returns a descriptive error listing
// EVERY gap (not just the first), so the user fixes them in one pass.
func verifyExpectedSchema(db *sql.DB) error {
	gaps := []string{}

	// Column checks.
	for table, cols := range expectedColumns {
		have := map[string]bool{}
		rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
		if err != nil {
			gaps = append(gaps, fmt.Sprintf("PRAGMA table_info(%s): %v", table, err))
			continue
		}
		for rows.Next() {
			var cid int
			var name, ctype string
			var notnull, pk int
			var dflt sql.NullString
			if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
				continue
			}
			have[name] = true
		}
		rows.Close()
		for _, want := range cols {
			if !have[want] {
				gaps = append(gaps, fmt.Sprintf("missing column %s.%s", table, want))
			}
		}
	}

	// Table checks.
	for _, want := range expectedTables {
		var name string
		err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`,
			want,
		).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			gaps = append(gaps, fmt.Sprintf("missing table %s", want))
		}
	}

	if len(gaps) == 0 {
		return nil
	}
	return fmt.Errorf("schema gaps detected after migrations (run failed?): %s",
		strings.Join(gaps, "; "))
}

// LogSchemaState prints a one-line boot diagnostic so the user can verify
// at-a-glance that the running container is talking to the DB they think
// it's talking to. Lists path + memory count + the presence of each
// pipeline-critical column. Cheap (single COUNT + one PRAGMA per table).
//
// (Per handoff "schema migration not applied 2026-05-10" §C — the user
// inspecting a different DB than the backend uses was a real failure mode.)
func (b *Bank) LogSchemaState() {
	if b == nil || b.db == nil {
		log.Printf("bank: LogSchemaState skipped (bank nil)")
		return
	}
	var liveMems int
	_ = b.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE deleted_at = ''`).Scan(&liveMems)
	var allMems int
	_ = b.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&allMems)
	var nightlyCount int
	_ = b.db.QueryRow(`SELECT COUNT(*) FROM nightly_runs`).Scan(&nightlyCount)

	// Sample a few "did the migration land?" markers.
	checks := []struct {
		table, col string
	}{
		{"memories", "recall_strength"},
		{"memories", "synthesis_source_ids"},
		{"nightly_runs", "dream_entry"},
	}
	missing := []string{}
	for _, c := range checks {
		var found int
		_ = b.db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`,
			c.table, c.col,
		).Scan(&found)
		if found == 0 {
			missing = append(missing, c.table+"."+c.col)
		}
	}
	if len(missing) > 0 {
		log.Printf("bank: SCHEMA INCOMPLETE — missing %v at path %s", missing, b.path)
	} else {
		log.Printf("bank: schema OK (path=%s, live_memories=%d, total_memories=%d, nightly_runs=%d, dream-pipeline columns present)",
			b.path, liveMems, allMems, nightlyCount)
	}
}

// migrateResearchCacheColumns adds research_cache columns added after the
// initial Phase 5 ship. Currently:
//   - fetched_by (TEXT, default "manual"): distinguishes user-triggered vs
//     auto-triggered (NightlyRunner) research. Surfaced by the dashboard's
//     research-detail "Manual" / "Auto" badge.
func migrateResearchCacheColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(research_cache)`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	additions := []struct{ name, decl string }{
		{"fetched_by", `TEXT NOT NULL DEFAULT 'manual'`},
	}
	for _, c := range additions {
		if have[c.name] {
			continue
		}
		stmt := fmt.Sprintf(`ALTER TABLE research_cache ADD COLUMN %s %s`, c.name, c.decl)
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("add column %s: %w", c.name, err)
		}
		log.Printf("bank: migrated research_cache — added column %s", c.name)
	}
	return nil
}

// migrateMemoriesColumns adds Phase 5 lifecycle columns to existing `memories`
// tables that pre-date P5-3. CREATE TABLE IF NOT EXISTS is a no-op when the
// table already exists with the old schema, so we PRAGMA-detect missing
// columns and ALTER TABLE ADD COLUMN them. SQLite ADD COLUMN is fast (no
// table rewrite when DEFAULT is constant).
func migrateMemoriesColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(memories)`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()

	additions := []struct{ name, decl string }{
		{"enriched_text", `TEXT NOT NULL DEFAULT ''`},
		{"light_encoded", `INTEGER NOT NULL DEFAULT 0`},
		{"nightly_consolidated", `INTEGER NOT NULL DEFAULT 0`},
		{"oracle_augmented", `INTEGER NOT NULL DEFAULT 0`},
		{"source", `TEXT NOT NULL DEFAULT 'adapter'`},
		{"merged_from", `TEXT NOT NULL DEFAULT '[]'`},
		{"deleted_at", `TEXT NOT NULL DEFAULT ''`},
		{"sensitive", `INTEGER NOT NULL DEFAULT 0`},
		{"sensitive_checked_at", `TEXT NOT NULL DEFAULT ''`},
		// Dream pipeline additions (2026-05-09 handoff):
		// synthesis_source_ids: Phase 2 lineage (which cluster members produced this synthesis)
		// schema_source_ids:    Phase 8 lineage (which syntheses produced this schema)
		// last_recalled_at:     Phase 5 + 9 — bumped by /recall hits
		// recall_strength:      Phase 9 reinforcement weight (default 1.0, cap 5.0)
		// deleted_reason:       Phase 1/5 — why a soft-delete fired (dedup vs decay vs manual)
		{"synthesis_source_ids", `TEXT NOT NULL DEFAULT '[]'`},
		{"schema_source_ids", `TEXT NOT NULL DEFAULT '[]'`},
		{"last_recalled_at", `TEXT NOT NULL DEFAULT ''`},
		{"recall_strength", `REAL NOT NULL DEFAULT 1.0`},
		{"deleted_reason", `TEXT NOT NULL DEFAULT ''`},
		// Phase 0b deep-encoding additions (2026-05-10 handoff):
		// deep_encoded_at:    when Tier 2 last successfully enriched this row ('' = never)
		// deep_encoded_model: which provider+model wrote that enrichment
		// marked_dirty_at:    when the row was queued for re-enrichment
		// dirty_reason:       created|edited|model_upgrade|cascading|periodic|temporal_neighbor|surprise|emotional|tmr_user
		// salience:           [R1] 0..1 priority signal (Phase 5 decay protection + Phase 0b queue order)
		// consolidation_stage:[R7] CITATIONS.md #2 — episodic|consolidating|semantic; tracks the
		//                     hippocampus→neocortex migration each memory undergoes as it's
		//                     processed and reinforced.
		{"deep_encoded_at", `TEXT NOT NULL DEFAULT ''`},
		{"deep_encoded_model", `TEXT NOT NULL DEFAULT ''`},
		{"marked_dirty_at", `TEXT NOT NULL DEFAULT ''`},
		{"dirty_reason", `TEXT NOT NULL DEFAULT ''`},
		{"salience", `REAL NOT NULL DEFAULT 0.5`},
		{"consolidation_stage", `TEXT NOT NULL DEFAULT 'episodic'`},
		// [R13] Phase 8.7 Context Memory Formation columns. CITATIONS.md #18
		// (Johnson 2005). memory_type categorises the row; context_region +
		// context_confidence + context_used_augment apply only when
		// memory_type='context'.
		{"memory_type", `TEXT NOT NULL DEFAULT 'episodic'`},
		{"context_region", `TEXT NOT NULL DEFAULT ''`},
		{"context_confidence", `TEXT NOT NULL DEFAULT ''`},
		{"context_used_augment", `INTEGER NOT NULL DEFAULT 0`},
		// v2.7 Bundle L — Dormant Memories. The brain doesn't delete on
		// timeout; it lets unused memories fall dormant. They stay in
		// the bank (still queryable via include_dormant=true) but are
		// hidden from default /recall — same shape as R10's supersede
		// filter. expires_at is the optional TTL trigger (empty = no
		// TTL); the nightly dormancy pass writes dormant_at when an
		// expiry fires or when a caller manually puts a memory to sleep.
		// Sensitive memories can't have a TTL set (sticky-on policy).
		{"expires_at", `TEXT NOT NULL DEFAULT ''`},
		{"dormant_at", `TEXT NOT NULL DEFAULT ''`},
		{"dormant_reason", `TEXT NOT NULL DEFAULT ''`},
		// Iter 25 (2026-05-14) — fact-dense key-value compression of the
		// raw memory body, produced by Phase 0b alongside `enriched_text`.
		// Format: "Field: value, Field: value, ..." preserving numbers,
		// dates, names, places, and outcomes while stripping prose fluff.
		// 3-5× shorter than raw_text but lossless on the facts the
		// synthesis model needs. Used by /reflect (sent alongside raw)
		// and by Phase 4 to synthesize concept-map schema_text cheaply.
		{"optimized_text", `TEXT NOT NULL DEFAULT ''`},
	}
	for _, c := range additions {
		if have[c.name] {
			continue
		}
		stmt := fmt.Sprintf(`ALTER TABLE memories ADD COLUMN %s %s`, c.name, c.decl)
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("add column %s: %w", c.name, err)
		}
		log.Printf("bank: migrated memories — added column %s", c.name)
	}
	return nil
}

// migrateMemoryMapsColumns adds dream-pipeline columns to memory_maps:
//   - archived: Phase 4 marks maps whose last memory was merged/decayed away.
//     Preserves history vs deleting the row outright.
//   - top_region: Phase 4 caches the most-common region across map members.
//   - last_touched_at: Phase 4 timestamp of the most-recent memory tagged here;
//     drives the "recently active" filter in the dashboard's Maps tab.
func migrateMemoryMapsColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(memory_maps)`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	additions := []struct{ name, decl string }{
		{"archived", `INTEGER NOT NULL DEFAULT 0`},
		{"top_region", `TEXT NOT NULL DEFAULT ''`},
		{"last_touched_at", `TEXT NOT NULL DEFAULT ''`},
		// [R13 redesign — v2.4.0b1] Map-proposal lifecycle. Existing rows
		// default to status='accepted' so the legacy Phase 4 / mcp_direct /
		// inferred maps continue to render as user-confirmed without any
		// dashboard change. New rows from POST /maps/propose write
		// status='proposed'; user acceptance flips to 'accepted'; user
		// dismissal flips to 'dismissed' (audit-trail kept; not hard-deleted).
		{"status", `TEXT NOT NULL DEFAULT 'accepted'`},
		// generated_by records the trigger of the proposal:
		//   'legacy'        — pre-v2.4.0b1 rows (set by migration backfill).
		//   'user'          — POST /maps/propose with source=tag|query|recall_id.
		//   'r13_redesign'  — POST /maps/propose with source=auto (Phase 8.7).
		//   'phase_4'       — nightly pipeline's existing inferred maps.
		{"generated_by", `TEXT NOT NULL DEFAULT ''`},
		// generation_phase records which Phase produced it (e.g. '4', '8.7'),
		// or 'user_triggered' for synthesis the user initiated. Empty for
		// legacy rows.
		{"generation_phase", `TEXT NOT NULL DEFAULT ''`},
		// Iter 25 (2026-05-14) — semantic embedding of the map for
		// hierarchical recall (`map_augmented_recall`). Computed by
		// Phase 4 from `name + " " + schema_text` using the same
		// embedding model the memory bank uses. JSON-encoded float vector
		// (matches the `embeddings` table format). Empty = not yet
		// embedded (legacy rows OR maps created before Tier 2 was up).
		{"embedding", `TEXT NOT NULL DEFAULT ''`},
		// Iter 25 — model label that produced `embedding`. When the user
		// swaps embedding models, Phase 4 detects the mismatch and
		// recomputes. Stays in sync with the `embeddings.model` column.
		{"embedding_model", `TEXT NOT NULL DEFAULT ''`},
	}
	for _, c := range additions {
		if have[c.name] {
			continue
		}
		stmt := fmt.Sprintf(`ALTER TABLE memory_maps ADD COLUMN %s %s`, c.name, c.decl)
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("add column %s: %w", c.name, err)
		}
		log.Printf("bank: migrated memory_maps — added column %s", c.name)
	}
	// Backfill `generated_by='legacy'` exactly once for rows that survived the
	// schema migration with the default empty string. Idempotent — subsequent
	// boots see no '' rows and skip the UPDATE. Distinguishes pre-v2.4.0b1
	// rows from any future row that legitimately wants generated_by='' for
	// some reason we haven't anticipated.
	if _, err := db.Exec(`UPDATE memory_maps SET generated_by = 'legacy' WHERE generated_by = ''`); err != nil {
		return fmt.Errorf("backfill memory_maps.generated_by: %w", err)
	}
	// Create the status index AFTER the column is guaranteed to exist
	// (either via fresh-install CREATE TABLE or this migration's ADD COLUMN).
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_memory_maps_status ON memory_maps(status)`); err != nil {
		return fmt.Errorf("create idx_memory_maps_status: %w", err)
	}
	// 2026-05-26 — UNIQUE(name, type) was too tight. When a map was archived
	// (archived=1) the row still occupied the slot, so re-accepting a new
	// proposal with the same name+type as an archived row returned SQLite
	// error 2067 — and Phase 4 happily re-proposed concepts whose name
	// already lived in another type, producing perpetual Proposed regrowth.
	// Replace the global unique index with a partial one that only enforces
	// uniqueness across LIVE rows (archived=0), allowing archived ghosts to
	// keep their identity without blocking reclassification.
	if _, err := db.Exec(`DROP INDEX IF EXISTS idx_memory_maps_name_type`); err != nil {
		return fmt.Errorf("drop legacy idx_memory_maps_name_type: %w", err)
	}
	if _, err := db.Exec(
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_memory_maps_name_type_live
		   ON memory_maps(name, type) WHERE archived = 0`,
	); err != nil {
		return fmt.Errorf("create idx_memory_maps_name_type_live: %w", err)
	}
	return nil
}

// migrateNightlyRunsColumns adds dream_entry + dream_entry_archetype columns
// (Phase 12 of the dream pipeline). Distinct from the existing `narrative`
// column: narrative is observational, dream_entry is evocative prose.
func migrateNightlyRunsColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(nightly_runs)`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	additions := []struct{ name, decl string }{
		{"dream_entry", `TEXT NOT NULL DEFAULT ''`},
		{"dream_entry_archetype", `TEXT NOT NULL DEFAULT ''`},
		// Phase 0b resumability: each run reports how far through the dirty
		// queue it got + checkpoints the cursor so the next run resumes.
		{"deep_encoded_this_run", `INTEGER NOT NULL DEFAULT 0`},
		{"deep_encoded_remaining", `INTEGER NOT NULL DEFAULT 0`},
		{"deep_encoded_cursor", `TEXT NOT NULL DEFAULT ''`},
		// Crash recovery (handoff "dream cycle resumption + crash recovery 2026-05-10"):
		// heartbeat_at: bumped every 30s during a live run; absence after >5min
		//               means the runner died.
		// interrupt_reason: '' for normal runs; 'process_restart' / 'manual_kill'
		//                   when the row was reconciled by ReconcileStaleInProgress.
		{"heartbeat_at", `TEXT NOT NULL DEFAULT ''`},
		{"interrupt_reason", `TEXT NOT NULL DEFAULT ''`},
	}
	for _, c := range additions {
		if have[c.name] {
			continue
		}
		stmt := fmt.Sprintf(`ALTER TABLE nightly_runs ADD COLUMN %s %s`, c.name, c.decl)
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("add column %s: %w", c.name, err)
		}
		log.Printf("bank: migrated nightly_runs — added column %s", c.name)
	}
	return nil
}

// migrateTokenBudgetLines adds the `model` column and reshapes the primary
// key from (date, tier, provider) to (date, tier, provider, model) so the
// per-tier popover can break down spend by specific model (e.g. when the
// user hot-swaps from "ollama:llama3.1:8b" to "airllm:meta-llama/...").
//
// SQLite doesn't support altering a primary key in-place, so this is a
// full rebuild: copy rows into a freshly-shaped table, drop the original,
// rename. Old rows get model='' (the "legacy / unknown" bucket).
//
// Safe to run on fresh installs (idempotent — bails when the model column
// is already present, which the schema CREATE TABLE above declares for
// new banks).
func migrateTokenBudgetLines(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(token_budget_lines)`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	if have["model"] {
		return nil
	}
	// Full table rebuild: only path SQLite gives us for PK changes.
	stmts := []string{
		`ALTER TABLE token_budget_lines RENAME TO token_budget_lines_pre_model`,
		`CREATE TABLE token_budget_lines (
		    date        TEXT NOT NULL,
		    tier        TEXT NOT NULL,
		    provider    TEXT NOT NULL,
		    model       TEXT NOT NULL DEFAULT '',
		    external    INTEGER NOT NULL DEFAULT 0,
		    tokens_in   INTEGER NOT NULL DEFAULT 0,
		    tokens_out  INTEGER NOT NULL DEFAULT 0,
		    updated_at  TEXT NOT NULL,
		    PRIMARY KEY (date, tier, provider, model)
		)`,
		`INSERT INTO token_budget_lines (date, tier, provider, model, external, tokens_in, tokens_out, updated_at)
		 SELECT date, tier, provider, '', external, tokens_in, tokens_out, updated_at
		 FROM token_budget_lines_pre_model`,
		`DROP TABLE token_budget_lines_pre_model`,
		`CREATE INDEX IF NOT EXISTS idx_token_budget_lines_date     ON token_budget_lines(date)`,
		`CREATE INDEX IF NOT EXISTS idx_token_budget_lines_external ON token_budget_lines(external)`,
		`CREATE INDEX IF NOT EXISTS idx_token_budget_lines_tier     ON token_budget_lines(tier)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("token_budget_lines migration step: %w", err)
		}
	}
	log.Printf("bank: migrated token_budget_lines — added model column + reshaped PK")
	return nil
}

// Close releases the database handle. Safe to call on a nil Bank.
func (b *Bank) Close() error {
	if b == nil || b.db == nil {
		return nil
	}
	return b.db.Close()
}

// Reopen swaps the underlying SQLite file to a new path at runtime.
// Used by the Patient feature: switching active patient calls Reopen
// with the new patient's bank_path. The old *sql.DB is closed after a
// short grace delay so an in-flight handler holding a stale ref errors
// out cleanly ("database is closed") rather than panicking.
//
// Concurrency contract: callers MUST gate Reopen so that no long-running
// background pipeline (NightlyRunner phases, batch reclassify, embed-all)
// is in progress. The /patients/active endpoint enforces this by
// refusing the swap while nightly.IsRunning() reports true. Best-effort
// handlers (write a single memory, read a single memory) may briefly
// see "database is closed" during the swap window — this is treated as
// a transient error and surfaces as HTTP 500 to the caller, which is
// expected during an admin operation.
//
// Returns an error if the new path can't be opened (in which case the
// old db remains live and active — atomic from the caller's perspective).
func (b *Bank) Reopen(newPath string) error {
	if b == nil {
		return errors.New("bank: cannot reopen a nil bank")
	}
	if newPath == "" {
		return errors.New("bank: reopen requires a non-empty path")
	}
	if newPath == b.path {
		return nil // no-op — same file
	}
	// Open the new bank FIRST, before touching the old one. If migration
	// or schema verification fails, we want the caller to see the error
	// while the old bank is still live so the system keeps working.
	newBank, err := NewBank(newPath)
	if err != nil {
		return fmt.Errorf("bank: reopen %s: %w", newPath, err)
	}
	if newBank == nil {
		return fmt.Errorf("bank: NewBank(%s) returned nil unexpectedly", newPath)
	}
	// Acquire the write mutex to serialise with any in-flight SaveMemory.
	// Reads are not gated (sqlite handles concurrent readers); a read
	// landing during the brief swap window will error with "database is
	// closed" once the old handle is closed, which is acceptable for an
	// admin-triggered operation.
	b.wmu.Lock()
	oldDB := b.db
	b.db = newBank.db
	b.path = newBank.path
	b.wmu.Unlock()
	// Close the old handle after a short grace period so any in-flight
	// reads have a chance to finish. The newBank object itself becomes
	// garbage once we've stolen its db pointer.
	if oldDB != nil {
		go func() {
			time.Sleep(5 * time.Second)
			_ = oldDB.Close()
		}()
	}
	log.Printf("bank: reopened — new path %s", newPath)
	return nil
}

// Path returns the file path of the currently-active bank. Used by the
// /patients/active endpoint to confirm a swap took effect.
func (b *Bank) Path() string {
	if b == nil {
		return ""
	}
	return b.path
}

// SaveMemory upserts a memory record. id may be empty — caller should
// generate one upstream. Returns the resolved record (with timestamps).
func (b *Bank) SaveMemory(rec MemoryRecord) (MemoryRecord, error) {
	if b == nil {
		return rec, errors.New("bank not enabled")
	}
	if rec.Text == "" {
		return rec, errors.New("text is required")
	}
	// v2.7 Bundle K — inline dedup. Only fires when:
	//   - caller didn't pre-set an ID (an explicit ID means "upsert as
	//     this ID" — the existing ON CONFLICT path handles it; dedup
	//     would clobber the caller's intent)
	//   - memory_type isn't synthesis/schema/context (Phase 2/8/8.7
	//     manage their own merging via synthesis_source_ids)
	//   - the dedup-enabled setting is ON (default ON when an embed
	//     provider is configured; the helper short-circuits to false
	//     when no provider is wired so legacy banks aren't affected)
	if rec.ID == "" && shouldRunInlineDedup(rec, b) {
		if existing, err := b.findNearDuplicate(rec); err == nil && existing != nil {
			merged, mErr := b.mergeIntoExisting(*existing, rec)
			if mErr == nil {
				return merged, nil
			}
			// Merge failed — log and fall through to a normal insert.
			log.Printf("bank: dedup merge failed for %s, falling through to insert: %v", existing.ID, mErr)
		}
	}
	if rec.ID == "" {
		rec.ID = fmt.Sprintf("bank-%d", nextMonotonicNano())
	}
	if rec.Tags == nil {
		rec.Tags = []string{}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if rec.CreatedAt == "" {
		rec.CreatedAt = now
	}
	rec.UpdatedAt = now

	if rec.Source == "" {
		rec.Source = "adapter"
	}
	if rec.MergedFrom == nil {
		rec.MergedFrom = []string{}
	}
	// SAFETY (Layer 1): auto-flag sensitive when any tag matches the sensitive
	// list, OR when the regex scanner detects a high-confidence pattern in the
	// text (API keys, JWTs, SSN, credit cards, etc.). Sticky-on: never
	// auto-cleared. Callers can flip it back via UpdateMemory (passing &false),
	// but new tags / new content can never silently un-sensitise a record.
	priorSensitive := rec.Sensitive
	autoSource := ""
	autoReason := "" // populated with regex pattern names so the audit row
	// reads "regex:<pattern>" instead of just the source string. The
	// dashboard's formatReason() turns this into "Matched <Name> pattern".
	autoTagTrigger := ""
	if !rec.Sensitive && tagsContainSensitive(rec.Tags) {
		rec.Sensitive = true
		autoSource = "tag-trigger"
		autoTagTrigger = firstSensitiveTag(rec.Tags)
		autoReason = "tag-trigger:" + autoTagTrigger
	}
	if !rec.Sensitive {
		hits := scanSensitive(rec.Text)
		if len(hits) == 0 && rec.EnrichedText != "" {
			hits = scanSensitive(rec.EnrichedText)
		}
		if len(hits) > 0 {
			rec.Sensitive = true
			autoSource = "auto-regex"
			autoReason = "regex:" + strings.Join(hits, ",")
			log.Printf("bank: auto-flagged %s as sensitive (regex: %v)", rec.ID, hits)
		}
	}
	tagsJSON, _ := json.Marshal(rec.Tags)
	mergedJSON, _ := json.Marshal(rec.MergedFrom)
	if rec.SynthesisSourceIDs == nil {
		rec.SynthesisSourceIDs = []string{}
	}
	if rec.SchemaSourceIDs == nil {
		rec.SchemaSourceIDs = []string{}
	}
	if rec.RecallStrength == 0 {
		rec.RecallStrength = 1.0
	}
	if rec.Salience == 0 {
		// [R1] CITATIONS.md #10 (Moncada 2015 Behavioral Tagging) +
		// #14 (Payne & Kensinger 2018). Compute initial salience from
		// surface markers in text + tags + recall recency. Novelty defaults
		// to 0.5 here because the new row has no embedding yet — Phase 0b
		// re-derives a true novelty-aware score after enrichment.
		rec.Salience = computeSalience(rec.Text, rec.Tags, 0.5, rec.LastRecalledAt, time.Now().UTC())
	}
	synthesisJSON, _ := json.Marshal(rec.SynthesisSourceIDs)
	schemaJSON, _ := json.Marshal(rec.SchemaSourceIDs)

	// Phase 0b dirty-flag trigger: every freshly inserted memory is dirty
	// with reason="created" so the next nightly run picks it up for deep
	// encoding. Existing rows preserve their dirty state via the
	// MAX-semantics ON CONFLICT clause (we never silently un-dirty).
	if rec.MarkedDirtyAt == "" {
		rec.MarkedDirtyAt = rec.UpdatedAt
		rec.DirtyReason = "created"
	}
	// [R7] consolidation_stage defaults to 'episodic' for fresh memories.
	// Phase 0b transitions episodic → consolidating when light_encoded
	// flips; Phase 9 transitions to 'semantic' when recall_strength
	// crosses the threshold.
	if rec.ConsolidationStage == "" {
		rec.ConsolidationStage = "episodic"
	}

	b.wmu.Lock()
	defer b.wmu.Unlock()
	// [R13] memory_type defaults to "episodic" for legacy/raw inserts. Phase
	// 2/8/8.7 set it explicitly to synthesis/schema/context. Empty string in
	// the input means "use the legacy default" rather than "blank it out".
	memType := rec.MemoryType
	if memType == "" {
		switch rec.Source {
		case "nightly_synthesis":
			memType = "synthesis"
		case "nightly_schema", "nightly_schema_from_replay":
			memType = "schema"
		case "nightly_context":
			memType = "context"
		default:
			memType = "episodic"
		}
	}
	// v2.7 Bundle L — sticky-on sensitive rejects a TTL. SaveMemory is the
	// adapter-side entrypoint; we silently drop expires_at on sensitive rows
	// rather than 4xx-ing the upstream adapter (whose caller likely didn't
	// know the row would auto-flag). UpdateMemory hard-errors because there
	// the caller is the user explicitly asking for the TTL.
	if rec.Sensitive && rec.ExpiresAt != "" {
		log.Printf("bank: dropped expires_at on sensitive memory %s (sticky-on policy)", rec.ID)
		rec.ExpiresAt = ""
	}
	_, err := b.db.Exec(`
INSERT INTO memories (
    id, text, tags, region_hint, adapter_id, session_id,
    enriched_text, light_encoded, nightly_consolidated, oracle_augmented,
    source, merged_from, sensitive, sensitive_checked_at,
    synthesis_source_ids, schema_source_ids, last_recalled_at, recall_strength, deleted_reason,
    deep_encoded_at, deep_encoded_model, marked_dirty_at, dirty_reason, salience,
    consolidation_stage,
    memory_type, context_region, context_confidence, context_used_augment,
    expires_at, dormant_at, dormant_reason,
    created_at, updated_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    text                 = excluded.text,
    tags                 = excluded.tags,
    region_hint          = excluded.region_hint,
    adapter_id           = excluded.adapter_id,
    session_id           = excluded.session_id,
    enriched_text        = excluded.enriched_text,
    light_encoded        = excluded.light_encoded,
    nightly_consolidated = excluded.nightly_consolidated,
    oracle_augmented     = excluded.oracle_augmented,
    source               = excluded.source,
    merged_from          = excluded.merged_from,
    synthesis_source_ids = excluded.synthesis_source_ids,
    schema_source_ids    = excluded.schema_source_ids,
    -- sensitive uses "max" semantics: once 1, stays 1 unless explicitly cleared
    -- via UpdateMemory. Upserts can never silently un-sensitise.
    sensitive            = MAX(memories.sensitive, excluded.sensitive),
    -- An update to text/tags invalidates the previous AI scan; classifier
    -- will re-pick it up on next pass.
    sensitive_checked_at = '',
    updated_at           = excluded.updated_at`,
		rec.ID, rec.Text, string(tagsJSON), rec.RegionHint,
		rec.AdapterID, rec.SessionID,
		rec.EnrichedText, boolToInt(rec.LightEncoded),
		boolToInt(rec.NightlyConsolidated), boolToInt(rec.OracleAugmented),
		rec.Source, string(mergedJSON), boolToInt(rec.Sensitive),
		rec.SensitiveCheckedAt,
		string(synthesisJSON), string(schemaJSON), rec.LastRecalledAt, rec.RecallStrength, rec.DeletedReason,
		rec.DeepEncodedAt, rec.DeepEncodedModel, rec.MarkedDirtyAt, rec.DirtyReason, rec.Salience,
		rec.ConsolidationStage,
		memType, rec.ContextRegion, rec.ContextConfidence, boolToInt(rec.ContextUsedAugment),
		rec.ExpiresAt, rec.DormantAt, rec.DormantReason,
		rec.CreatedAt, rec.UpdatedAt,
	)
	if err != nil {
		return rec, err
	}
	// Audit + control-channel emission for auto-flagged inserts. Same pre-
	// condition for both: prior was false, current is true, an auto-source
	// (regex / tag-trigger) drove the flip. Held inside wmu — appendAuditLocked
	// is the lock-free variant for exactly this reason; Hub.Fanout doesn't
	// reacquire wmu so the inline emit is also safe.
	//
	// before/after JSON: the audit's purpose for sensitive flips is showing
	// the user "this changed from false to true" — minimal diff that's
	// non-revealing. Body is redacted both sides because the body is what
	// triggered the flip; we don't need to leak it in the audit row.
	if !priorSensitive && rec.Sensitive && autoSource != "" {
		// Synthesise a "before" snapshot — same row but pre-flip values.
		before := rec
		before.Sensitive = false
		before.SensitiveCheckedAt = ""
		beforeRedacted := redactForAudit(before, "auto_flag_sensitive")
		afterRedacted := redactForAudit(rec, "auto_flag_sensitive")
		bj, _ := json.Marshal(beforeRedacted)
		aj, _ := json.Marshal(afterRedacted)
		_ = b.appendAuditLocked(AuditEntry{
			Operation:  "auto_flag_sensitive",
			EntityType: "trace",
			EntityID:   rec.ID,
			BeforeJSON: string(bj),
			AfterJSON:  string(aj),
			Reason:     autoReason, // e.g. "regex:openai_api_key,phone" or "tag-trigger:private"
			AdapterID:  "sd-core-bank",
		})
		b.emit("sensitive.flipped", "sd-core-bank", map[string]interface{}{
			"trace_id":  rec.ID,
			"sensitive": true,
			"source":    autoSource,
		})
	}
	// [R3 temporal_neighbor] CITATIONS.md #9 STC. When a high-salience
	// memory arrives, mark its temporal-neighbour memories (same region,
	// ±30min window) dirty so the next nightly run revisits them as a
	// cluster. Threshold 0.7 on the freshly-computed salience.
	// Best-effort: runs OUTSIDE wmu via a goroutine so the bank's
	// returning-from-Save path isn't slowed.
	if rec.Salience > 0.7 && rec.RegionHint != "" {
		go func(id, region, created string) {
			if _, err := b.MarkTemporalNeighborsDirty(id, region, created); err != nil {
				log.Printf("bank: temporal-neighbor mark for %s: %v", id, err)
			}
		}(rec.ID, rec.RegionHint, rec.CreatedAt)
	}
	return rec, nil
}

// redactForAudit returns a copy of `m` with the body redacted in two cases:
//
//   1. m.Sensitive == true (regardless of operation). Without this, a
//      `trace_edit` audit on a sensitive memory would leak the body in
//      plain text via the audit log — a back-channel for reading
//      sensitive content even when the dashboard properly redacts it
//      in normal views.
//
//   2. The operation is a sensitive-flag flip (auto_flag_sensitive,
//      manual_flag_sensitive, clear_sensitive). The flip itself is the
//      interesting fact, not the body — the diff `sensitive: false → true`
//      carries the full information the dashboard needs. Crucially this
//      covers the BEFORE snapshot of an auto-flag flip, where sensitive
//      is still false at that timestamp but the body is the very thing
//      that triggered the flip.
//
// Both before and after snapshots of a sensitive flip get this redaction
// because the dashboard's diff renderer doesn't need the body to show
// "sensitive: false → true". Tags / region / source / lifecycle flags
// are kept — those are metadata the user's audit needs.
func redactForAudit(m MemoryRecord, op string) MemoryRecord {
	isSensitiveFlipOp := op == "auto_flag_sensitive" ||
		op == "manual_flag_sensitive" ||
		op == "clear_sensitive"
	if !m.Sensitive && !isSensitiveFlipOp {
		return m
	}
	const placeholder = "[REDACTED — sensitive trace, body omitted from audit]"
	m.Text = placeholder
	m.EnrichedText = ""
	return m
}

// firstSensitiveTag returns the first tag from `tags` that matches the
// runtime sensitive-tag set, lowercased. Empty if none match. Used to
// surface a specific tag in audit reasons (e.g. "tag-trigger:private")
// so the dashboard can render a meaningful row.
func firstSensitiveTag(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	set := sensitiveTagSet()
	for _, t := range tags {
		low := strings.ToLower(strings.TrimSpace(t))
		if set[low] {
			return low
		}
	}
	return ""
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// memoryColumns is the canonical SELECT list for `memories`. Keep aligned
// with scanRow. The dream-pipeline columns appear AFTER the legacy ones so
// pre-existing tests that rely on positional binding aren't broken.
const memoryColumns = `id, text, tags, region_hint, adapter_id, session_id,
    enriched_text, light_encoded, nightly_consolidated, oracle_augmented,
    source, merged_from, deleted_at, sensitive, sensitive_checked_at,
    synthesis_source_ids, schema_source_ids, last_recalled_at, recall_strength, deleted_reason,
    deep_encoded_at, deep_encoded_model, marked_dirty_at, dirty_reason, salience,
    consolidation_stage,
    memory_type, context_region, context_confidence, context_used_augment,
    expires_at, dormant_at, dormant_reason,
    created_at, updated_at`

// GetMemory returns a single record by id, or sql.ErrNoRows if missing.
func (b *Bank) GetMemory(id string) (MemoryRecord, error) {
	if b == nil {
		return MemoryRecord{}, errors.New("bank not enabled")
	}
	row := b.db.QueryRow(`SELECT `+memoryColumns+` FROM memories WHERE id = ?`, id)
	return scanRow(row)
}

// MemoryListOpts narrows ListMemories.
type MemoryListOpts struct {
	Limit          int
	Since          string // created_at > since
	AdapterID      string
	IncludeDeleted bool // when false, soft-deleted rows are excluded
	OnlyDeleted    bool // when true, returns ONLY soft-deleted rows (for trash UI)
	// ExcludeSensitive forces sensitive=0 in the WHERE clause. Use this on any
	// path that may eventually feed Tier 3 / Oracle / outbound research. The
	// canonical caller is ModelRouter.PrepareOracleCall.
	ExcludeSensitive bool
	// OnlySensitive returns ONLY sensitive=1 rows (for "what is hidden from
	// research" admin UI).
	OnlySensitive bool
}

// ListMemories returns up to `limit` records, optionally filtered.
//   - since: ISO 8601 timestamp; only records with created_at > since are returned
//   - adapterID: exact match on adapter_id; empty means any
//   - limit: defaults to 100, capped at 1000
//
// Soft-deleted rows are excluded by default. Use ListMemoriesWith for finer control.
func (b *Bank) ListMemories(limit int, since, adapterID string) ([]MemoryRecord, error) {
	return b.ListMemoriesWith(MemoryListOpts{
		Limit:     limit,
		Since:     since,
		AdapterID: adapterID,
	})
}

// ListMemoriesWith is the rich-options variant of ListMemories.
func (b *Bank) ListMemoriesWith(opts MemoryListOpts) ([]MemoryRecord, error) {
	if b == nil {
		return nil, errors.New("bank not enabled")
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	// Cap raised from 1000 → 50000 (handoff "dream pipeline tuning 2026-05-10").
	// 1000 was a development safety valve; in production it gave the dream
	// pipeline ~28% coverage of a 3608-memory bank. Phase 3 (lexicon rebuild)
	// is destructive on a partial scan — it deletes the lexicon table first,
	// so iterating only a subset wipes 70% of the user's tag co-occurrence
	// graph. Phases above 50k should switch to paginated cursors; until then
	// 50k is the hard cap.
	if limit > 50000 {
		limit = 50000
	}
	conds := []string{"1=1"}
	args := []any{}
	if opts.Since != "" {
		conds = append(conds, "created_at > ?")
		args = append(args, opts.Since)
	}
	if opts.AdapterID != "" {
		conds = append(conds, "adapter_id = ?")
		args = append(args, opts.AdapterID)
	}
	switch {
	case opts.OnlyDeleted:
		conds = append(conds, "deleted_at <> ''")
	case !opts.IncludeDeleted:
		conds = append(conds, "deleted_at = ''")
	}
	switch {
	case opts.OnlySensitive:
		conds = append(conds, "sensitive = 1")
	case opts.ExcludeSensitive:
		conds = append(conds, "sensitive = 0")
	}
	args = append(args, limit)
	q := `SELECT ` + memoryColumns + `
FROM memories WHERE ` + strings.Join(conds, " AND ") + `
ORDER BY created_at DESC
LIMIT ?`

	rows, err := b.db.Query(q, args...)
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

// Count returns the total number of memories in the bank.
func (b *Bank) Count() (int, error) {
	if b == nil {
		return 0, nil
	}
	var n int
	err := b.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&n)
	return n, err
}

// scanRow handles both *sql.Row and *sql.Rows since they share Scan().
type scanner interface {
	Scan(dest ...any) error
}

func scanRow(s scanner) (MemoryRecord, error) {
	var rec MemoryRecord
	var tagsJSON, mergedJSON, synthesisJSON, schemaJSON string
	var lightEnc, nightlyCon, oracleAug, sensitive, contextUsedAugment int
	if err := s.Scan(
		&rec.ID, &rec.Text, &tagsJSON,
		&rec.RegionHint, &rec.AdapterID, &rec.SessionID,
		&rec.EnrichedText, &lightEnc, &nightlyCon, &oracleAug,
		&rec.Source, &mergedJSON, &rec.DeletedAt, &sensitive,
		&rec.SensitiveCheckedAt,
		&synthesisJSON, &schemaJSON, &rec.LastRecalledAt, &rec.RecallStrength, &rec.DeletedReason,
		&rec.DeepEncodedAt, &rec.DeepEncodedModel, &rec.MarkedDirtyAt, &rec.DirtyReason, &rec.Salience,
		&rec.ConsolidationStage,
		&rec.MemoryType, &rec.ContextRegion, &rec.ContextConfidence, &contextUsedAugment,
		&rec.ExpiresAt, &rec.DormantAt, &rec.DormantReason,
		&rec.CreatedAt, &rec.UpdatedAt,
	); err != nil {
		return rec, err
	}
	rec.LightEncoded = lightEnc != 0
	rec.NightlyConsolidated = nightlyCon != 0
	rec.OracleAugmented = oracleAug != 0
	rec.Sensitive = sensitive != 0
	rec.ContextUsedAugment = contextUsedAugment != 0
	if tagsJSON == "" {
		rec.Tags = []string{}
	} else {
		_ = json.Unmarshal([]byte(tagsJSON), &rec.Tags)
		if rec.Tags == nil {
			rec.Tags = []string{}
		}
	}
	if mergedJSON == "" {
		rec.MergedFrom = []string{}
	} else {
		_ = json.Unmarshal([]byte(mergedJSON), &rec.MergedFrom)
		if rec.MergedFrom == nil {
			rec.MergedFrom = []string{}
		}
	}
	if synthesisJSON != "" {
		_ = json.Unmarshal([]byte(synthesisJSON), &rec.SynthesisSourceIDs)
	}
	if schemaJSON != "" {
		_ = json.Unmarshal([]byte(schemaJSON), &rec.SchemaSourceIDs)
	}
	if rec.RecallStrength == 0 {
		rec.RecallStrength = 1.0 // legacy rows get the default
	}
	return rec, nil
}

// ── R10 bi-temporal supersede ────────────────────────────────────────────────
//
// AddSupersede records that supersedingID replaces supersededID as of validFrom
// (RFC3339, defaults to now() when empty). Returns ErrConstraint-style errors
// on self-reference or unknown ID; idempotent on duplicate edge (last reason
// wins, valid_from stays original).
func (b *Bank) AddSupersede(supersedingID, supersededID, validFrom, reason string) error {
	if supersedingID == "" || supersededID == "" {
		return fmt.Errorf("supersede: both ids required")
	}
	if supersedingID == supersededID {
		return fmt.Errorf("supersede: a memory cannot supersede itself")
	}
	// Validate both memories exist.
	for _, id := range []string{supersedingID, supersededID} {
		var n int
		if err := b.db.QueryRow(`SELECT COUNT(1) FROM memories WHERE id=?`, id).Scan(&n); err != nil {
			return fmt.Errorf("supersede: id %s lookup: %w", id, err)
		}
		if n == 0 {
			return fmt.Errorf("supersede: memory not found: %s", id)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if validFrom == "" {
		validFrom = now
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_, err := b.db.Exec(`
		INSERT INTO memory_supersedes (superseding_id, superseded_id, valid_from, reason, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(superseding_id, superseded_id) DO UPDATE SET
			reason = excluded.reason
	`, supersedingID, supersededID, validFrom, reason, now)
	return err
}

// RemoveSupersede deletes a single edge (undo). Returns sql.ErrNoRows when
// the edge doesn't exist.
func (b *Bank) RemoveSupersede(supersedingID, supersededID string) error {
	b.wmu.Lock()
	defer b.wmu.Unlock()
	res, err := b.db.Exec(`DELETE FROM memory_supersedes WHERE superseding_id=? AND superseded_id=?`,
		supersedingID, supersededID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SupersedeEdge is one row of memory_supersedes (JSON-friendly).
type SupersedeEdge struct {
	SupersedingID string `json:"superseding_id"`
	SupersededID  string `json:"superseded_id"`
	ValidFrom     string `json:"valid_from"`
	Reason        string `json:"reason,omitempty"`
	CreatedAt     string `json:"created_at"`
}

// SupersedeChainFor returns all edges where memory id is either side. Lets
// the UI render "this memory was superseded by X" / "this memory supersedes Y".
func (b *Bank) SupersedeChainFor(id string) ([]SupersedeEdge, error) {
	rows, err := b.db.Query(`
		SELECT superseding_id, superseded_id, valid_from, reason, created_at
		FROM memory_supersedes
		WHERE superseding_id=? OR superseded_id=?
		ORDER BY valid_from ASC
	`, id, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SupersedeEdge
	for rows.Next() {
		var e SupersedeEdge
		if err := rows.Scan(&e.SupersedingID, &e.SupersededID, &e.ValidFrom, &e.Reason, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SupersededIDsAt returns the set of memory ids that have been superseded as
// of asOf (RFC3339; empty = now). A memory is "superseded" when there exists
// an edge superseded_id=id with valid_from <= asOf. Chains (A→B, B→C): both
// A and B are superseded at any time after the B→C edge.
func (b *Bank) SupersededIDsAt(asOf string) (map[string]struct{}, error) {
	if asOf == "" {
		asOf = time.Now().UTC().Format(time.RFC3339)
	}
	rows, err := b.db.Query(`
		SELECT DISTINCT superseded_id
		FROM memory_supersedes
		WHERE valid_from <= ?
	`, asOf)
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

// SaveEmbedding upserts an embedding row keyed by text_hash.
func (b *Bank) SaveEmbedding(textHash string, vec []float32, model string) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	data, err := json.Marshal(vec)
	if err != nil {
		return err
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_, err = b.db.Exec(`
INSERT INTO embeddings (text_hash, vector_json, model, created_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(text_hash) DO UPDATE SET
    vector_json = excluded.vector_json,
    model       = excluded.model`,
		textHash, string(data), model,
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	return err
}

// GetEmbedding returns the cached vector for textHash, or nil if not found.
func (b *Bank) GetEmbedding(textHash string) ([]float32, error) {
	if b == nil {
		return nil, nil
	}
	var raw string
	err := b.db.QueryRow(
		`SELECT vector_json FROM embeddings WHERE text_hash = ?`, textHash,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var vec []float32
	if err := json.Unmarshal([]byte(raw), &vec); err != nil {
		return nil, err
	}
	return vec, nil
}

// AllEmbeddingsForMemories returns a map of memoryID → cached embedding for
// every memory that has a row in the embeddings table. Memories without a
// cached embedding are silently skipped — populate them via /recall calls or
// the synapse builder before expecting full coverage.
func (b *Bank) AllEmbeddingsForMemories(model string) (map[string][]float32, error) {
	if b == nil {
		return nil, errors.New("bank not enabled")
	}
	// Soft-deleted rows are excluded here so every consumer (recall cosine
	// candidates, inline dedup near-duplicate scan, synapse builder, salience,
	// map recall) treats a tombstoned memory as gone. Without this filter,
	// soft-deleted memories whose embedding is still cached leaked back into
	// recall on the cosine path (BM25 already filters, but RRF unioned the
	// cosine hits). findNearDuplicate's doc-comment already assumed this
	// exclusion existed.
	rows, err := b.db.Query(`SELECT id, text, tags FROM memories WHERE deleted_at = ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type memRow struct {
		id   string
		text string
		tags []string
	}
	var mems []memRow
	for rows.Next() {
		var m memRow
		var tagsJSON string
		if err := rows.Scan(&m.id, &m.text, &tagsJSON); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(tagsJSON), &m.tags)
		mems = append(mems, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make(map[string][]float32, len(mems))
	for _, m := range mems {
		tagsAny := make([]interface{}, len(m.tags))
		for i, t := range m.tags {
			tagsAny[i] = t
		}
		embedText := synapseEmbedText(map[string]interface{}{
			"text": m.text,
			"tags": tagsAny,
		})
		h := synapseTextHash(embedText)
		vec, err := b.GetEmbedding(h)
		if err != nil || vec == nil {
			continue
		}
		out[m.id] = vec
	}
	return out, nil
}

// CountEmbeddingsByModel returns the number of rows in the embeddings
// table stored under the given bare model label. v2.6 Bundle D —
// powers the startup migration warning: if the user has rows under one
// label and we now point ForEmbedding() at a different one, /recall
// would return empty until they re-embed. Returns 0 when the bank or
// table is unavailable (we treat that as "no rows to worry about").
func (b *Bank) CountEmbeddingsByModel(model string) (int, error) {
	if b == nil || b.db == nil {
		return 0, nil
	}
	var n int
	row := b.db.QueryRow(`SELECT COUNT(*) FROM embeddings WHERE model = ?`, model)
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// SearchBM25 runs an FTS5 keyword search over the memories_fts virtual
// table and returns memory IDs in BM25-rank order (best match first).
// Used by hybrid retrieval (v2.7 Bundle J) to complement the cosine
// semantic-similarity path with a keyword-match score.
//
// The query string is passed through SQLite's FTS5 MATCH operator;
// callers should sanitize untrusted input first (FTS5 has a small
// query-grammar surface — `AND`, `OR`, `NOT`, quoted phrases,
// column-scoped matches via `text:foo`). For naive single-string
// queries we wrap each token in double-quotes to defang grammar.
//
// Returns at most `limit` rows; passing limit <= 0 falls back to 100.
// Soft-deleted memories (deleted_at != '') are excluded.
func (b *Bank) SearchBM25(query string, limit int) ([]string, error) {
	if b == nil || b.db == nil {
		return nil, errors.New("bank not enabled")
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 100
	}
	// Tokenize the query to defang FTS5 grammar surprises (a stray
	// `*` or `-` would otherwise change the meaning). Wrap each
	// non-empty token in double-quotes. FTS5 treats a quoted phrase
	// as an exact-token match; multiple quoted phrases without a
	// connector default to AND.
	ftsQuery := bm25SanitizeQuery(q)
	if ftsQuery == "" {
		return nil, nil
	}
	rows, err := b.db.Query(`
		SELECT memories_fts.memory_id
		FROM memories_fts
		JOIN memories ON memories.id = memories_fts.memory_id
		WHERE memories_fts MATCH ?
		  AND memories.deleted_at = ''
		ORDER BY rank
		LIMIT ?`, ftsQuery, limit)
	if err != nil {
		return nil, fmt.Errorf("bm25 search: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// bm25SanitizeQuery splits a free-form user string into FTS5-safe
// quoted-phrase tokens. Strips characters FTS5 treats specially
// (`"`, `*`, `(`, `)`, `:`, `^`, `-` at the start of a token, etc.)
// and re-emits each surviving token wrapped in double-quotes.
// Tokens shorter than 2 chars are dropped (FTS5 doesn't index them
// with the default unicode61 tokenizer anyway, and they're noise).
func bm25SanitizeQuery(raw string) string {
	if raw == "" {
		return ""
	}
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return ""
	}
	clean := make([]string, 0, len(fields))
	for _, f := range fields {
		var sb strings.Builder
		for _, r := range f {
			// Allow letters, digits, hyphen-inside-token. Drop punctuation
			// + FTS5 operators.
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
				sb.WriteRune(r)
				continue
			}
			// Allow a few in-word characters that appear in real tokens
			// (email/snake_case/etc.) — but break the token there so
			// FTS5 sees them as separate words.
			if r == '_' || r == '-' || r == '.' || r == '@' {
				sb.WriteRune(' ')
				continue
			}
			// Drop everything else.
		}
		for _, t := range strings.Fields(sb.String()) {
			if len(t) >= 2 {
				clean = append(clean, `"`+t+`"`)
			}
		}
	}
	return strings.Join(clean, " ")
}

// RRFScore is one row's contribution to a Reciprocal Rank Fusion run.
type RRFScore struct {
	ID    string
	Score float64
}

// ──────────────────────────────────────────────────────────────────────
// v2.7 Bundle K — inline auto-dedup at SaveMemory
// ──────────────────────────────────────────────────────────────────────

// AutoDedupEnabledKey gates whether SaveMemory runs the inline dedup
// check. Default ON; users (or operators) can flip OFF via PUT
// /settings/auto_dedup_enabled. Even when ON, the gate requires a
// non-nil dedup-provider closure to be wired (set by main.go from
// router.ForEmbedding) — so legacy installs with no embed tier
// configured see zero behaviour change.
const AutoDedupEnabledKey = "auto_dedup_enabled"

// AutoDedupThresholdKey is the cosine-similarity floor above which two
// memories are considered duplicates. Conservative default 0.97 — only
// fires on near-verbatim text. Stored as a string-encoded float so the
// generic settings path handles it.
const AutoDedupThresholdKey = "auto_dedup_threshold"

// AutoDedupTagOverlapPctKey is the minimum tag-overlap (Jaccard) required
// in addition to the cosine threshold. Defends against semantic-only
// near-duplicates that span different topical scopes (e.g. two memories
// about "OAuth" but one is tagged auth/security and the other is
// tagged tutorial/example — different intent, shouldn't merge).
// Default 75.
const AutoDedupTagOverlapPctKey = "auto_dedup_tag_overlap_pct"

// SetDedupProvider wires the embed-provider resolver into the bank for
// inline dedup. main.go calls this once on boot with router.ForEmbedding
// (as a method value, so subsequent router swaps are picked up). A nil
// resolver — or a resolver that returns nil — disables dedup entirely.
func (b *Bank) SetDedupProvider(fn func() LLMProvider) {
	if b == nil {
		return
	}
	b.dedupProvider = fn
}

// shouldRunInlineDedup is the gate: caller didn't supply an ID, memory
// isn't a synthesis/schema/context type, dedup is enabled, and an
// embed provider is wired.
func shouldRunInlineDedup(rec MemoryRecord, b *Bank) bool {
	if b == nil || b.dedupProvider == nil {
		return false
	}
	if b.dedupProvider() == nil {
		return false
	}
	if !autoDedupEnabled(b) {
		return false
	}
	switch strings.ToLower(rec.MemoryType) {
	case "synthesis", "schema", "context":
		return false // pipeline-managed types own their own merging
	}
	return true
}

func autoDedupEnabled(b *Bank) bool {
	if b == nil {
		return false
	}
	raw, ok, err := b.GetSetting(AutoDedupEnabledKey)
	if err != nil || !ok {
		return true // default ON
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

func autoDedupThreshold(b *Bank) float64 {
	if b == nil {
		return 0.97
	}
	raw, ok, err := b.GetSetting(AutoDedupThresholdKey)
	if err != nil || !ok {
		return 0.97
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || f <= 0 || f > 1.0 {
		return 0.97
	}
	return f
}

func autoDedupTagOverlapPct(b *Bank) int {
	if b == nil {
		return 75
	}
	raw, ok, err := b.GetSetting(AutoDedupTagOverlapPctKey)
	if err != nil || !ok {
		return 75
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 || n > 100 {
		return 75
	}
	return n
}

// findNearDuplicate embeds the candidate text + tags, runs cosine against
// the bank's existing embeddings under the current provider's label, and
// returns the matching MemoryRecord if (a) cosine >= threshold AND (b)
// tag-overlap (Jaccard) >= the configured percentage. Returns (nil, nil)
// when no duplicate is found.
//
// Soft-deleted rows are excluded by AllEmbeddingsForMemories' standard
// filter. Sensitive-tagged rows ARE candidates — if a duplicate is
// sensitive, the new memory's text gets merged into the existing
// sensitive row (preserving the sensitive flag is the safer default).
func (b *Bank) findNearDuplicate(candidate MemoryRecord) (*MemoryRecord, error) {
	if b == nil || b.dedupProvider == nil {
		return nil, nil
	}
	provider := b.dedupProvider()
	if provider == nil {
		return nil, nil
	}
	tagsAny := make([]interface{}, len(candidate.Tags))
	for i, t := range candidate.Tags {
		tagsAny[i] = t
	}
	embedText := synapseEmbedText(map[string]interface{}{
		"text": candidate.Text,
		"tags": tagsAny,
	})
	candHash := synapseTextHash(embedText)
	// Embed (or pull from cache) the candidate.
	candVec, err := b.GetEmbedding(candHash)
	if err != nil || candVec == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		candVec, err = provider.Embed(ctx, embedText)
		if err != nil {
			return nil, err
		}
		// Cache for future use (the inline dedup call also serves as a
		// pre-warm for the synapse builder).
		_ = b.SaveEmbedding(candHash, candVec, providerModelLabel(provider))
	}
	// Load all existing embeddings under the current label.
	label := providerModelLabel(provider)
	existing, err := b.AllEmbeddingsForMemories(label)
	if err != nil {
		return nil, err
	}
	threshold := autoDedupThreshold(b)
	tagPct := autoDedupTagOverlapPct(b)
	candTags := tagSet(candidate.Tags)
	// Linear scan — fine up to ~10k memories. Beyond that we'd want a
	// proper ANN index (Phase O ANN backlog).
	var bestID string
	var bestSim float64
	for id, vec := range existing {
		sim := float64(vecDot(candVec, vec))
		if sim < threshold {
			continue
		}
		if sim > bestSim {
			bestSim = sim
			bestID = id
		}
	}
	if bestID == "" {
		return nil, nil
	}
	// Pull the candidate's record + check tag overlap.
	rec, err := b.GetMemory(bestID)
	if err != nil {
		return nil, err
	}
	overlap := jaccardPct(candTags, tagSet(rec.Tags))
	if overlap < tagPct {
		return nil, nil
	}
	return &rec, nil
}

// mergeIntoExisting performs the actual dedup merge: appends the new
// memory's text to the existing row's MergedFrom list (so the lineage
// is preserved), bumps last_recalled_at + updated_at, writes an audit
// row, and returns the updated record.
//
// Conservative: text on the existing row is NOT replaced. The user
// can always inspect MergedFrom to see what was deduped into the row.
func (b *Bank) mergeIntoExisting(existing MemoryRecord, candidate MemoryRecord) (MemoryRecord, error) {
	if b == nil {
		return existing, errors.New("bank not enabled")
	}
	// Build the merged_from entry: snippet of the candidate text +
	// inherited tags. JSON-encoded so a future schema migration could
	// promote it into a proper child table.
	mergedEntry := struct {
		Text   string   `json:"text"`
		Tags   []string `json:"tags,omitempty"`
		Merged string   `json:"merged_at"`
		Source string   `json:"source,omitempty"`
	}{
		Text:   candidate.Text,
		Tags:   candidate.Tags,
		Merged: time.Now().UTC().Format(time.RFC3339Nano),
		Source: candidate.Source,
	}
	entryJSON, _ := json.Marshal(mergedEntry)
	existing.MergedFrom = append(existing.MergedFrom, string(entryJSON))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	mergedJSON, _ := json.Marshal(existing.MergedFrom)
	b.wmu.Lock()
	_, err := b.db.Exec(
		`UPDATE memories SET merged_from = ?, updated_at = ?, last_recalled_at = ? WHERE id = ?`,
		string(mergedJSON), now, now, existing.ID,
	)
	b.wmu.Unlock()
	if err != nil {
		return existing, err
	}
	_ = b.AppendAuditSilent(AuditEntry{
		Operation:  "auto_dedup_merge",
		EntityType: "trace",
		EntityID:   existing.ID,
		AfterJSON:  fmt.Sprintf(`{"merged_count":%d,"candidate_text_preview":%q}`, len(existing.MergedFrom), candidate.Text[:min(80, len(candidate.Text))]),
		Reason:     "v2.7 Bundle K inline auto-dedup",
		AdapterID:  "sd-core-dedup",
	})
	existing.UpdatedAt = now
	existing.LastRecalledAt = now
	return existing, nil
}

// tagSet builds a string-set from a tag slice, lower-cased.
func tagSet(tags []string) map[string]bool {
	out := make(map[string]bool, len(tags))
	for _, t := range tags {
		out[strings.ToLower(strings.TrimSpace(t))] = true
	}
	return out
}

// jaccardPct returns the Jaccard similarity of two tag sets, expressed
// as an integer percent (0..100). Two empty sets count as 100% (vacuous
// overlap) so dedup can still merge tagless memories whose text is
// near-identical.
func jaccardPct(a, b map[string]bool) int {
	if len(a) == 0 && len(b) == 0 {
		return 100
	}
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return (inter * 100) / union
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ReciprocalRankFusion combines multiple ranked lists into a single
// ranking via the classic Cormack RRF formula: for each list, each
// item's contribution is 1/(k + rank), where rank is 1-indexed.
// Ranks beyond the list length contribute zero. Items appearing in
// only one list still get their slice of credit.
//
// Returns up to `limit` IDs in descending fused-score order. When
// `limit <= 0` returns all unique IDs that appeared in any list.
//
// The RRF constant `k` defaults to 60 (Cormack et al.) when caller
// passes <=0 — empirically the sweet spot for hybrid lexical+semantic
// retrieval per the Microsoft AzureSearch and Anthropic Claude
// reference deployments.
func ReciprocalRankFusion(rankings [][]string, k int, limit int) []RRFScore {
	if k <= 0 {
		k = 60
	}
	scores := map[string]float64{}
	for _, list := range rankings {
		for i, id := range list {
			if id == "" {
				continue
			}
			scores[id] += 1.0 / float64(k+i+1) // +1 so rank is 1-indexed
		}
	}
	out := make([]RRFScore, 0, len(scores))
	for id, s := range scores {
		out = append(out, RRFScore{ID: id, Score: s})
	}
	// Sort descending by score. Stable order for ties (insertion order
	// is map-iteration order so we don't guarantee tie-break stability;
	// callers shouldn't rely on it).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Score > out[j-1].Score; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if limit > 0 && limit < len(out) {
		out = out[:limit]
	}
	return out
}
