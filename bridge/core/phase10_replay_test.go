// phase10_replay_test.go — [R4] generative replay (Phase 10) +
// [R8] schema-from-replay clustering (Phase 8 R8 path).
// CITATIONS.md #6 (Buzsáki 2015), #11 (Lacaux 2021).
package main

import (
	"strings"
	"testing"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────
// [R4] partitionReplayPools — recent vs preexisting split
// ──────────────────────────────────────────────────────────────────────────

func TestPartitionReplayPools_SplitsByCreatedAt(t *testing.T) {
	now := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	pc := &pipelineContext{now: now}

	mems := []MemoryRecord{
		{ID: "a", CreatedAt: now.AddDate(0, 0, -1).Format(time.RFC3339Nano)},  // recent (1d ago)
		{ID: "b", CreatedAt: now.AddDate(0, 0, -5).Format(time.RFC3339Nano)},  // recent (5d)
		{ID: "c", CreatedAt: now.AddDate(0, 0, -15).Format(time.RFC3339Nano)}, // mid (15d — between 7 and 30, dropped)
		{ID: "d", CreatedAt: now.AddDate(0, 0, -45).Format(time.RFC3339Nano)}, // preexisting (45d)
		{ID: "e", CreatedAt: now.AddDate(0, 0, -90).Format(time.RFC3339Nano)}, // preexisting (90d)
	}
	recentCutoff := now.AddDate(0, 0, -7).Format(time.RFC3339Nano)
	preexistingCutoff := now.AddDate(0, 0, -30).Format(time.RFC3339Nano)

	recent, preexisting := pc.partitionReplayPools(mems, recentCutoff, preexistingCutoff)
	if len(recent) != 2 {
		t.Errorf("recent pool: want 2, got %d", len(recent))
	}
	if len(preexisting) != 2 {
		t.Errorf("preexisting pool: want 2, got %d", len(preexisting))
	}
	// Mid-window memory must land in neither pool — generative mix needs
	// a clear age gap, not contiguous coverage.
	for _, m := range append(recent, preexisting...) {
		if m.ID == "c" {
			t.Errorf("mid-window memory %s should not be in either pool", m.ID)
		}
	}
}

func TestPartitionReplayPools_SkipsDeletedAndPriorReplays(t *testing.T) {
	now := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	pc := &pipelineContext{now: now}
	mems := []MemoryRecord{
		{ID: "deleted", CreatedAt: now.AddDate(0, 0, -1).Format(time.RFC3339Nano), DeletedAt: now.Format(time.RFC3339Nano)},
		{ID: "prior_replay", CreatedAt: now.AddDate(0, 0, -1).Format(time.RFC3339Nano), Source: "nightly_replay"},
		{ID: "keep", CreatedAt: now.AddDate(0, 0, -1).Format(time.RFC3339Nano)},
	}
	recent, _ := pc.partitionReplayPools(mems,
		now.AddDate(0, 0, -7).Format(time.RFC3339Nano),
		now.AddDate(0, 0, -30).Format(time.RFC3339Nano))
	if len(recent) != 1 || recent[0].ID != "keep" {
		t.Errorf("recent pool: want [keep], got %+v", recent)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// [R4] buildReplayPrompt — chronological + map-named
// ──────────────────────────────────────────────────────────────────────────

func TestBuildReplayPrompt_NamesMapAndDates(t *testing.T) {
	mems := []MemoryRecord{
		{Text: "old observation", CreatedAt: "2026-01-15T10:00:00Z"},
		{Text: "recent observation", CreatedAt: "2026-05-09T10:00:00Z"},
	}
	out := buildReplayPrompt("synaptic-disorder", mems)
	if !strings.Contains(out, "synaptic-disorder") {
		t.Errorf("prompt missing map name")
	}
	if !strings.Contains(out, "2026-01-15") || !strings.Contains(out, "2026-05-09") {
		t.Errorf("prompt missing dates")
	}
	if !strings.Contains(out, "OBSERVATIONS") {
		t.Errorf("prompt missing OBSERVATIONS header")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// [R4] anySensitive
// ──────────────────────────────────────────────────────────────────────────

func TestAnySensitive_Detects(t *testing.T) {
	if !anySensitive([]MemoryRecord{{}, {Sensitive: true}, {}}) {
		t.Errorf("expected detection of sensitive member")
	}
	if anySensitive([]MemoryRecord{{}, {}, {}}) {
		t.Errorf("expected no detection in clean batch")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// [R8] clusterReplaysByTagOverlap — shared-tag clustering
// ──────────────────────────────────────────────────────────────────────────

func TestClusterReplaysByTagOverlap_GroupsAtThreeSharedTags(t *testing.T) {
	replays := []MemoryRecord{
		{ID: "r1", Tags: []string{"replay", "nightly_consolidated", "auth", "oauth", "pkce", "synaptic"}},
		{ID: "r2", Tags: []string{"replay", "nightly_consolidated", "auth", "oauth", "pkce", "other-project"}},
		{ID: "r3", Tags: []string{"replay", "nightly_consolidated", "unrelated", "shipping", "ux"}},
	}
	clusters := clusterReplaysByTagOverlap(replays, 3)
	if len(clusters) != 1 {
		t.Fatalf("want 1 cluster (r1+r2 share auth/oauth/pkce), got %d", len(clusters))
	}
	if len(clusters[0]) != 2 {
		t.Errorf("want 2 members in cluster, got %d", len(clusters[0]))
	}
}

func TestClusterReplaysByTagOverlap_NoiseTagsDontCountTowardOverlap(t *testing.T) {
	// All three share "replay" + "nightly_consolidated" — that's 2 tags
	// but both are noise and must be excluded from the overlap count.
	replays := []MemoryRecord{
		{ID: "r1", Tags: []string{"replay", "nightly_consolidated", "auth"}},
		{ID: "r2", Tags: []string{"replay", "nightly_consolidated", "ui"}},
		{ID: "r3", Tags: []string{"replay", "nightly_consolidated", "db"}},
	}
	clusters := clusterReplaysByTagOverlap(replays, 3)
	if len(clusters) != 0 {
		t.Errorf("noise-only overlap should not form a cluster; got %d", len(clusters))
	}
}

func TestClusterReplaysByTagOverlap_SkipsSingletonClusters(t *testing.T) {
	replays := []MemoryRecord{
		{ID: "lonely", Tags: []string{"a", "b", "c", "d"}},
	}
	if got := clusterReplaysByTagOverlap(replays, 3); len(got) != 0 {
		t.Errorf("singleton input should yield 0 clusters, got %d", len(got))
	}
}

func TestClusterReplaysByTagOverlap_ThreeWayCluster(t *testing.T) {
	replays := []MemoryRecord{
		{ID: "r1", Tags: []string{"replay", "auth", "oauth", "pkce"}},
		{ID: "r2", Tags: []string{"replay", "auth", "oauth", "pkce", "rotation"}},
		{ID: "r3", Tags: []string{"replay", "auth", "oauth", "pkce", "session"}},
	}
	clusters := clusterReplaysByTagOverlap(replays, 3)
	if len(clusters) != 1 {
		t.Fatalf("want 1 cluster, got %d", len(clusters))
	}
	if len(clusters[0]) != 3 {
		t.Errorf("want 3 members, got %d", len(clusters[0]))
	}
}

// ──────────────────────────────────────────────────────────────────────────
// [R8] clusterAlreadySchematizedByIDs — dedup against existing schemas
// ──────────────────────────────────────────────────────────────────────────

func TestClusterAlreadySchematizedByIDs_DetectsExistingSchema(t *testing.T) {
	bank := newTestBank(t)
	r1, _ := bank.SaveMemory(MemoryRecord{Text: "replay 1", Source: "nightly_replay"})
	r2, _ := bank.SaveMemory(MemoryRecord{Text: "replay 2", Source: "nightly_replay"})
	// Pre-existing schema covering both r1 and r2.
	bank.SaveMemory(MemoryRecord{
		Text: "schema over r1+r2", Source: "nightly_schema_from_replay",
		SchemaSourceIDs: []string{r1.ID, r2.ID},
	})
	pc := newPipelineContextForTest(t, bank)
	if !pc.clusterAlreadySchematizedByIDs([]MemoryRecord{r1, r2}) {
		t.Errorf("expected existing schema to be detected for {r1,r2}")
	}
}

func TestClusterAlreadySchematizedByIDs_NewClusterReturnsFalse(t *testing.T) {
	bank := newTestBank(t)
	r1, _ := bank.SaveMemory(MemoryRecord{Text: "replay 1", Source: "nightly_replay"})
	r2, _ := bank.SaveMemory(MemoryRecord{Text: "replay 2", Source: "nightly_replay"})
	pc := newPipelineContextForTest(t, bank)
	if pc.clusterAlreadySchematizedByIDs([]MemoryRecord{r1, r2}) {
		t.Errorf("expected no schema for unsupported cluster")
	}
}

// (Phase 10 disabled-by-default is covered by
// TestPhase10_DisabledByDefault in nightly_pipeline_test.go.)

