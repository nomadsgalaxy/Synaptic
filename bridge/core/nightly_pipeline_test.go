// nightly_pipeline_test.go — focused tests for the dream-pipeline phases
// that don't require a live Tier 2/3. Phases requiring Tier 2 (synthesis,
// schema, replay, narrative, dream entry) are exercised through their
// pure helpers (cluster detection, archetype hash, eligibility filters);
// the LLM round-trip itself is covered by mocking in handlers_research_test
// and providers_test.
package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// newPipelineContextForTest builds a pipelineContext with sane defaults
// against a fresh in-memory bank. Tests poke fields directly when they
// need a particular setting tweak.
func newPipelineContextForTest(t *testing.T, bank *Bank) *pipelineContext {
	t.Helper()
	return &pipelineContext{
		ctx:           context.Background(),
		runID:         "test-run-" + time.Now().Format("150405"),
		bank:          bank,
		now:           time.Date(2026, 5, 10, 3, 0, 0, 0, time.UTC),
		settings:      DefaultDreamPipelineSettings(),
		stats:         &NightlyStats{SchemaVersion: 2},
		localConcepts: map[string]bool{},
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 0 — encoding
// ──────────────────────────────────────────────────────────────────────────

func TestPhase0_FlipsLightEncoded(t *testing.T) {
	bank := newTestBank(t)
	for i := 0; i < 5; i++ {
		_, err := bank.SaveMemory(MemoryRecord{Text: "memory " + string(rune('a'+i))})
		if err != nil {
			t.Fatalf("SaveMemory: %v", err)
		}
	}
	// Phase 0 now gates the light_encoded flip on a successful embed (per
	// handoff "Phase 0 silent embedding failure 2026-05-10"). Wire a
	// scripted in-memory Tier 1 so this test still exercises the
	// flag-flipping path without a live Ollama. The richer embed-failure
	// scenarios are covered in handlers_admin_embed_test.go.
	pc := newPipelineCtxWithEmbedder(t, bank, newScriptedEmbedder())
	if err := pc.runPhase0Encoding(); err != nil {
		t.Fatalf("phase0: %v", err)
	}
	if pc.stats.Encoded != 5 {
		t.Errorf("expected 5 encoded, got %d", pc.stats.Encoded)
	}
	mems, _ := bank.ListMemoriesWith(MemoryListOpts{Limit: 100})
	for _, m := range mems {
		if !m.LightEncoded {
			t.Errorf("memory %s should be light_encoded", m.ID)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 1 — dedup
// ──────────────────────────────────────────────────────────────────────────

func TestPickCanonicalAndLoser_PrefersHigherStrength(t *testing.T) {
	a := MemoryRecord{ID: "a", Tags: []string{"x"}, Text: "short", RecallStrength: 1.0}
	b := MemoryRecord{ID: "b", Tags: []string{"x"}, Text: "short", RecallStrength: 2.0}
	canonical, loser := pickCanonicalAndLoser(a, b)
	if canonical.ID != "b" || loser.ID != "a" {
		t.Errorf("expected b canonical (higher strength), got %s/%s", canonical.ID, loser.ID)
	}
}

func TestPickCanonicalAndLoser_PrefersMoreTags(t *testing.T) {
	a := MemoryRecord{ID: "a", Tags: []string{"x", "y"}, Text: "short", RecallStrength: 1.0}
	b := MemoryRecord{ID: "b", Tags: []string{"x"}, Text: "short", RecallStrength: 1.0}
	canonical, _ := pickCanonicalAndLoser(a, b)
	if canonical.ID != "a" {
		t.Errorf("expected a canonical (more tags), got %s", canonical.ID)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 3 — lexicon rebuild
// ──────────────────────────────────────────────────────────────────────────

func TestPhase3_RebuildsFromMemories(t *testing.T) {
	bank := newTestBank(t)
	bank.SaveMemory(MemoryRecord{Text: "a", Tags: []string{"alpha", "beta", "gamma"}})
	bank.SaveMemory(MemoryRecord{Text: "b", Tags: []string{"alpha", "beta"}})
	// Insert a stale lexicon row that the rebuild should clear.
	bank.UpsertLexicon(LexiconRow{TagA: "stale", TagB: "row", Cooccurrence: 99})

	pc := newPipelineContextForTest(t, bank)
	if err := pc.runPhase3Lexicon(); err != nil {
		t.Fatalf("phase3: %v", err)
	}
	pairs, _ := bank.ListLexicon("", 100)
	for _, p := range pairs {
		if p.TagA == "stale" || p.TagB == "stale" {
			t.Errorf("stale row not cleared: %v", p)
		}
	}
	// alpha-beta should appear with cooccurrence >= 2 (both memories share it).
	found := false
	for _, p := range pairs {
		if (p.TagA == "alpha" && p.TagB == "beta") || (p.TagA == "beta" && p.TagB == "alpha") {
			found = true
			if p.Cooccurrence < 2 {
				t.Errorf("alpha-beta cooccurrence < 2: %d", p.Cooccurrence)
			}
		}
	}
	if !found {
		t.Errorf("expected alpha-beta pair in lexicon")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 4 — maps
// ──────────────────────────────────────────────────────────────────────────

func TestPhase4_NewMapEmerges(t *testing.T) {
	bank := newTestBank(t)
	bank.SaveMemory(MemoryRecord{Text: "first", Tags: []string{"new-topic"}})
	bank.SaveMemory(MemoryRecord{Text: "second", Tags: []string{"new-topic"}})

	pc := newPipelineContextForTest(t, bank)
	// v2.6: Phase 4 now emits proposals (status='proposed') instead of
	// auto-creating concept maps, and gates the proposal on a minimum
	// cluster size — `MapProposalMinTraces` (default 10). The test
	// seeds 2 memories sharing a tag, which would be skipped under the
	// default threshold. Lower it to 2 so the cluster crosses the bar
	// and Phase 4 emits the proposal. The auto-create flag stays at
	// its default (false) so we're exercising the proposal path.
	pc.settings.MapProposalMinTraces = 2
	if err := pc.runPhase4Maps(); err != nil {
		t.Fatalf("phase4: %v", err)
	}
	if len(pc.stats.MapsEmerged) == 0 {
		t.Errorf("expected at least one emerged map, got 0")
	}
	gotTag := false
	for _, m := range pc.stats.MapsEmerged {
		if m.MapKey == "new-topic" && m.MemoryCount == 2 {
			gotTag = true
		}
	}
	if !gotTag {
		t.Errorf("expected emerged map for 'new-topic' with count 2, got %+v", pc.stats.MapsEmerged)
	}
	// Belt-and-braces: confirm the emitted map landed with status='proposed'
	// (the new normal) and was scoped to type='concept'. This is what
	// guards against accidentally resurrecting the auto-create-concept-
	// maps path. GetMemoryMapByName does an exact type-match (the empty-
	// type form is reserved for legacy lookups that don't filter), so we
	// pass the concrete type "concept" here.
	mm, err := bank.GetMemoryMapByName("new-topic", "concept")
	if err != nil {
		t.Fatalf("GetMemoryMapByName: %v", err)
	}
	if mm.Status != "proposed" {
		t.Errorf("expected status=proposed, got %q", mm.Status)
	}
	if mm.GeneratedBy != "phase_4_proposal" {
		t.Errorf("expected GeneratedBy=phase_4_proposal, got %q", mm.GeneratedBy)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 5 — decay
// ──────────────────────────────────────────────────────────────────────────

func TestPhase5_NeverDecaysSensitive(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{
		Text: "private stuff", Tags: []string{"low_priority", "private"},
		LightEncoded: true,
	})
	// Force created_at to ancient and never-recalled.
	bank.db.Exec(`UPDATE memories SET created_at = ?, last_recalled_at = '' WHERE id = ?`,
		"2020-01-01T00:00:00Z", rec.ID)

	pc := newPipelineContextForTest(t, bank)
	if err := pc.runPhase5Decay(); err != nil {
		t.Fatalf("phase5: %v", err)
	}
	// The 'private' tag triggers the sensitive flag → never decayed.
	got, _ := bank.GetMemory(rec.ID)
	if got.DeletedAt != "" {
		t.Errorf("sensitive memory was decayed (deleted_at=%q)", got.DeletedAt)
	}
}

func TestPhase5_HonorsCap(t *testing.T) {
	// Updated for [R2] SHY weighted decay (handoff "R-refinement followups
	// 2026-05-10"): pruning now requires strength < strength_floor in
	// addition to the legacy eligibility predicates. Seed memories with
	// already-below-floor strength so the cap test stays meaningful.
	bank := newTestBank(t)
	for i := 0; i < 5; i++ {
		rec, _ := bank.SaveMemory(MemoryRecord{
			Text:         "old " + string(rune('a'+i)),
			Tags:         []string{"low_priority"},
			LightEncoded: true,
		})
		bank.db.Exec(`UPDATE memories SET
			created_at = ?, last_recalled_at = '',
			recall_strength = 0.04, salience = 0.0
			WHERE id = ?`,
			"2020-01-01T00:00:00Z", rec.ID)
	}
	pc := newPipelineContextForTest(t, bank)
	pc.settings.DecayMaxPerRun = 2
	if err := pc.runPhase5Decay(); err != nil {
		t.Fatalf("phase5: %v", err)
	}
	if pc.stats.Pruned != 2 {
		t.Errorf("expected pruned=2 (cap), got %d", pc.stats.Pruned)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 7 — cross-region (helper-level)
// ──────────────────────────────────────────────────────────────────────────

func TestTagJaccard(t *testing.T) {
	cases := []struct {
		a, b []string
		want float64
	}{
		{[]string{"x", "y"}, []string{"x", "y"}, 1.0},
		{[]string{"x"}, []string{"y"}, 0.0},
		{[]string{"x", "y"}, []string{"y", "z"}, 1.0 / 3.0},
		{nil, nil, 0.0},
	}
	for _, c := range cases {
		got := tagJaccard(c.a, c.b)
		if got != c.want {
			t.Errorf("tagJaccard(%v,%v) = %g, want %g", c.a, c.b, got, c.want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 9 — reinforcement
// ──────────────────────────────────────────────────────────────────────────

func TestPhase9_BumpsRecalledMemories(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "hot memory", LightEncoded: true})
	// Stamp last_recalled_at AFTER the prevFinish baseline.
	bank.db.Exec(`UPDATE memories SET last_recalled_at = ? WHERE id = ?`,
		"2026-05-09T12:00:00Z", rec.ID)

	pc := newPipelineContextForTest(t, bank)
	pc.lastRunFinish = time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC)
	if err := pc.runPhase9Reinforcement(); err != nil {
		t.Fatalf("phase9: %v", err)
	}
	got, _ := bank.GetMemory(rec.ID)
	if got.RecallStrength <= 1.0 {
		t.Errorf("expected recall_strength bumped > 1.0, got %g", got.RecallStrength)
	}
	if pc.stats.ReinforcedCount != 1 {
		t.Errorf("expected ReinforcedCount=1, got %d", pc.stats.ReinforcedCount)
	}
}

func TestPhase9_CapsAtMaxStrength(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "max already", LightEncoded: true, RecallStrength: 5.0})
	bank.db.Exec(`UPDATE memories SET recall_strength = 5.0, last_recalled_at = ? WHERE id = ?`,
		"2026-05-09T12:00:00Z", rec.ID)

	pc := newPipelineContextForTest(t, bank)
	pc.lastRunFinish = time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC)
	pc.runPhase9Reinforcement()
	got, _ := bank.GetMemory(rec.ID)
	if got.RecallStrength > 5.0+0.001 {
		t.Errorf("expected cap at 5.0, got %g", got.RecallStrength)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 10 — replay
// ──────────────────────────────────────────────────────────────────────────

func TestPhase10_DisabledByDefault(t *testing.T) {
	bank := newTestBank(t)
	pc := newPipelineContextForTest(t, bank)
	// Replay disabled by default.
	if err := pc.runPhase10Replay(); err != nil {
		t.Fatalf("phase10: %v", err)
	}
	if len(pc.stats.Replays) != 0 {
		t.Errorf("expected no replays when disabled, got %d", len(pc.stats.Replays))
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 12 — dream entry
// ──────────────────────────────────────────────────────────────────────────

func TestDreamEntry_ArchetypeStable(t *testing.T) {
	a := pickArchetype("nightly-2026-05-10-03-00")
	b := pickArchetype("nightly-2026-05-10-03-00")
	if a != b {
		t.Errorf("same runID should give same archetype: got %q vs %q", a, b)
	}
}

func TestDreamEntry_ArchetypeVaries(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id := "nightly-test-" + string(rune('a'+i%26)) + string(rune('0'+i%10))
		seen[pickArchetype(id)] = true
	}
	if len(seen) < 8 {
		t.Errorf("expected 8+ distinct archetypes across 50 ids, got %d", len(seen))
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Settings round-trip
// ──────────────────────────────────────────────────────────────────────────

func TestDreamPipelineSettings_RoundTrip(t *testing.T) {
	bank := newTestBank(t)
	in := DreamPipelineSettings{
		DedupThreshold:       0.92,
		SynthesisMaxPerRun:   8,
		CrossRegionThreshold: 0.80,
		ReplayEnabled:        true,
	}
	if err := bank.SaveDreamPipelineSettings(in); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, err := bank.GetDreamPipelineSettings()
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if out.DedupThreshold != 0.92 {
		t.Errorf("dedup_threshold roundtrip: got %g", out.DedupThreshold)
	}
	if out.SynthesisMaxPerRun != 8 {
		t.Errorf("synthesis_max_per_run roundtrip: got %d", out.SynthesisMaxPerRun)
	}
	if !out.ReplayEnabled {
		t.Errorf("replay_enabled roundtrip: got false")
	}
}

func TestDreamPipelineSettings_DefaultsWhenAbsent(t *testing.T) {
	bank := newTestBank(t)
	out, _ := bank.GetDreamPipelineSettings()
	def := DefaultDreamPipelineSettings()
	if out != def {
		t.Errorf("expected defaults when no settings stored, got %+v", out)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Bubble freshness invariant
// ──────────────────────────────────────────────────────────────────────────

func TestBubble_SourceHashStable(t *testing.T) {
	h1 := computeSourceHash("hello world", []string{"a", "b"})
	h2 := computeSourceHash("hello world", []string{"b", "a"})
	if h1 != h2 {
		t.Errorf("tag order shouldn't affect hash: %q vs %q", h1, h2)
	}
}

func TestBubble_HashDiffersOnTextChange(t *testing.T) {
	h1 := computeSourceHash("hello world", []string{"a"})
	h2 := computeSourceHash("hello world!", []string{"a"})
	if h1 == h2 {
		t.Errorf("text change should change hash")
	}
}

func TestBubble_HashDiffersOnTagChange(t *testing.T) {
	h1 := computeSourceHash("hello", []string{"a"})
	h2 := computeSourceHash("hello", []string{"a", "b"})
	if h1 == h2 {
		t.Errorf("tag set change should change hash")
	}
}

func TestSetBaseThought_FreshPoolReturnsTrue(t *testing.T) {
	g := &BubbleGenerator{
		pool:        map[string]*ThoughtEntry{},
		prioritySet: map[string]bool{},
	}
	if !g.setBaseThought("m1", "thought!", "hash1") {
		t.Errorf("expected true on first set")
	}
	if g.pool["m1"].SourceHash != "hash1" {
		t.Errorf("expected hash stored, got %q", g.pool["m1"].SourceHash)
	}
}

func TestSetBaseThought_SameHashReturnsFalse(t *testing.T) {
	g := &BubbleGenerator{
		pool:        map[string]*ThoughtEntry{},
		prioritySet: map[string]bool{},
	}
	g.setBaseThought("m1", "thought", "hash1")
	if g.setBaseThought("m1", "different thought", "hash1") {
		t.Errorf("same hash should skip — got true")
	}
	if g.pool["m1"].Text != "thought" {
		t.Errorf("expected thought to be unchanged on hash match, got %q", g.pool["m1"].Text)
	}
}

func TestSetBaseThought_DifferentHashOverwritesAndWipesModes(t *testing.T) {
	g := &BubbleGenerator{
		pool:        map[string]*ThoughtEntry{},
		prioritySet: map[string]bool{},
	}
	g.setBaseThought("m1", "old thought", "hash1")
	g.pool["m1"].Modes["adhd"] = "old mutation"
	if !g.setBaseThought("m1", "new thought", "hash2") {
		t.Errorf("different hash should overwrite — got false")
	}
	if g.pool["m1"].Text != "new thought" {
		t.Errorf("expected new thought, got %q", g.pool["m1"].Text)
	}
	if len(g.pool["m1"].Modes) != 0 {
		t.Errorf("expected modes wiped on regen, got %v", g.pool["m1"].Modes)
	}
}

func TestInvalidateAndEnqueue_WipesModes(t *testing.T) {
	g := &BubbleGenerator{
		pool:        map[string]*ThoughtEntry{},
		prioritySet: map[string]bool{},
		priorityCh:  make(chan genTask, 8),
	}
	g.pool["m1"] = &ThoughtEntry{
		MemoryID: "m1", Text: "stale", SourceHash: "h1",
		Modes: map[string]string{"adhd": "stale-adhd"},
	}
	g.InvalidateAndEnqueue("m1")
	if len(g.pool["m1"].Modes) != 0 {
		t.Errorf("expected modes wiped, got %v", g.pool["m1"].Modes)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Recall bumps last_recalled_at (Phase 9 baseline)
// ──────────────────────────────────────────────────────────────────────────

func TestBumpLastRecalled_StampsTimestamp(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "hot"})
	if err := bank.BumpLastRecalled(rec.ID); err != nil {
		t.Fatalf("bump: %v", err)
	}
	got, _ := bank.GetMemory(rec.ID)
	if got.LastRecalledAt == "" {
		t.Errorf("expected last_recalled_at populated")
	}
	// Should be a valid RFC3339Nano stamp.
	if _, err := time.Parse(time.RFC3339Nano, got.LastRecalledAt); err != nil {
		t.Errorf("expected RFC3339Nano stamp, got %q (err %v)", got.LastRecalledAt, err)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Stats v2 schema versioning
// ──────────────────────────────────────────────────────────────────────────

func TestStatsV2_OmitsEmptyArrays(t *testing.T) {
	stats := NightlyStats{Encoded: 3, SchemaVersion: 2}
	out := mustJSON(stats)
	// New v2 array fields should be omitted when empty.
	for _, field := range []string{"merge_pairs", "syntheses", "cross_region_links", "schemas", "replays", "augmented"} {
		if strings.Contains(out, `"`+field+`"`) {
			t.Errorf("expected %s omitted from empty stats: %s", field, out)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Bug fixes from handoff "dream pipeline tuning 2026-05-10"
// ──────────────────────────────────────────────────────────────────────────

// Bug 1 — Phase 3 must rebuild from ALL live memories, not just the first 1000.
// Seeds 1500 memories where the second half shares tag pairs the first half
// doesn't. If the cap silently truncates, those pairs go missing.
func TestPhase3_RebuildsFromAllMemories(t *testing.T) {
	bank := newTestBank(t)
	for i := 0; i < 750; i++ {
		bank.SaveMemory(MemoryRecord{Text: "early memory", Tags: []string{"alpha", "beta"}})
	}
	// Second batch with a UNIQUE pair the first half doesn't carry.
	for i := 0; i < 750; i++ {
		bank.SaveMemory(MemoryRecord{Text: "late memory", Tags: []string{"gamma", "delta"}})
	}
	pc := newPipelineContextForTest(t, bank)
	if err := pc.runPhase3Lexicon(); err != nil {
		t.Fatalf("phase3: %v", err)
	}
	pairs, _ := bank.ListLexicon("", 5000)
	hasGammaDelta := false
	for _, p := range pairs {
		if (p.TagA == "gamma" && p.TagB == "delta") || (p.TagA == "delta" && p.TagB == "gamma") {
			hasGammaDelta = true
		}
	}
	if !hasGammaDelta {
		t.Errorf("Phase 3 should pick up tag pairs in the LAST 750 memories — gamma/delta pair missing (cap silently truncated)")
	}
}

// Bug 5 — `maps_emerged` and `maps_archived` must not overlap on the same
// run for the same map_key. Previously a map could appear in BOTH lists
// because the archive loop's tally lookup didn't case-fold.
func TestPhase4_EmergeAndArchive_NoOverlap(t *testing.T) {
	bank := newTestBank(t)
	// Seed memories whose tag will emerge as a map (count >= MapMinMemories).
	bank.SaveMemory(MemoryRecord{Text: "a", Tags: []string{"NewTopic"}})
	bank.SaveMemory(MemoryRecord{Text: "b", Tags: []string{"NewTopic"}})

	pc := newPipelineContextForTest(t, bank)
	if err := pc.runPhase4Maps(); err != nil {
		t.Fatalf("phase4: %v", err)
	}
	emergedKeys := map[string]bool{}
	for _, e := range pc.stats.MapsEmerged {
		emergedKeys[e.MapKey] = true
	}
	for _, key := range pc.stats.MapsArchived {
		if emergedKeys[key] {
			t.Errorf("key %q appeared in BOTH MapsEmerged and MapsArchived (Bug 5)", key)
		}
	}
}

// Bug 3 — dream entry generates for background activity (not just Tier 2 phases).
// Tests the predicate directly to keep this test deterministic without an LLM.
func TestPhase12_GeneratesEntryForBackgroundActivity(t *testing.T) {
	cases := []struct {
		name  string
		stats NightlyStats
		want  bool
	}{
		{"only encoded", NightlyStats{Encoded: 1000}, true},
		{"only maps emerged", NightlyStats{MapsEmerged: []MapEmergenceRef{{MapKey: "x"}}}, true},
		{"only lexicon delta", NightlyStats{LexiconPairsAdded: 450}, true},
		{"only pruned", NightlyStats{Pruned: 5}, true},
		{"only maps archived", NightlyStats{MapsArchived: []string{"old"}}, true},
		{"only reinforced", NightlyStats{ReinforcedCount: 3}, true},
		{"only synthesis", NightlyStats{Syntheses: []SynthesisRef{{ID: "s1"}}}, true},
		{"only cross_region", NightlyStats{CrossRegionLinks: []CrossRegionRef{{AID: "a", BID: "b"}}}, true},
	}
	for _, c := range cases {
		got := dreamEntryHasActivity(c.stats)
		if got != c.want {
			t.Errorf("%s: dreamEntryHasActivity = %v, want %v", c.name, got, c.want)
		}
	}
}

// Bug 3 inverse — no entry on a genuinely empty run.
func TestPhase12_SkipsForGenuineQuietNight(t *testing.T) {
	stats := NightlyStats{SchemaVersion: 2} // all zero
	if dreamEntryHasActivity(stats) {
		t.Errorf("expected hasActivity=false for empty stats blob")
	}
}

// Bug 2 — LexiconPairs must NOT be backfilled from the snapshot. Phase 3's
// rebuild count is authoritative; the snapshot can race and overwrite a
// legitimate value. Test by simulating a Phase 3 run that produced a
// specific count and asserting the orchestrator didn't clobber it.
//
// Indirect: the test reads the LexiconPairs assignment in DoRun's merge
// block. Since we removed the fallback entirely, any non-zero LexiconPairs
// the pipeline writes survives. The strongest assertion is that the code
// path no longer references after.lexiconPairs for LexiconPairs — confirmed
// by visual review of nightly_runner.go. This unit test is a smoke test
// that the field round-trips.
func TestStatsV2_LexiconPairsRoundTrip(t *testing.T) {
	stats := NightlyStats{LexiconPairs: 42, SchemaVersion: 2}
	out := mustJSON(stats)
	if !strings.Contains(out, `"lexicon_pairs":42`) {
		t.Errorf("LexiconPairs should serialize even when 42, got %s", out)
	}
}

// Bug 4 — DedupRegionGroups maps motor_cortex and prefrontal_cortex to
// DIFFERENT groups (motor vs executive); but the handoff explicitly lists
// "Run-now button" as the cross-region case. Ensure that within a group,
// cross-region pairs DO get bucketed together.
func TestDedupGroupOf_FunctionalGroups(t *testing.T) {
	cases := []struct {
		region   string
		wantSame string // another region that should share its group
	}{
		{"hippocampus", "entorhinal_cortex"},
		{"prefrontal_cortex", "frontal_lobe"},
		{"motor_cortex", "cerebellum"},
		{"broca_area", "wernicke_area"},
	}
	for _, c := range cases {
		if dedupGroupOf(c.region) != dedupGroupOf(c.wantSame) {
			t.Errorf("expected %s and %s to share a functional group, got %q vs %q",
				c.region, c.wantSame, dedupGroupOf(c.region), dedupGroupOf(c.wantSame))
		}
	}
	// Unknown region should fall back to a singleton group keyed by name.
	if dedupGroupOf("totally_made_up") != "totally_made_up" {
		t.Errorf("unknown region should fall back to its own name as group")
	}
	// Empty region should collapse to the unknown bucket.
	if dedupGroupOf("") != "_unknown" {
		t.Errorf("empty region should map to _unknown")
	}
}

// Tuning — DefaultDreamPipelineSettings honors the handoff's lowered defaults.
func TestTunedDefaults_DecayAndDedup(t *testing.T) {
	d := DefaultDreamPipelineSettings()
	if d.DecayAgeDays != 180 {
		t.Errorf("decay_age_days default lowered to 180 in tuning handoff, got %d", d.DecayAgeDays)
	}
	if d.DedupThreshold != 0.92 {
		t.Errorf("dedup_threshold default lowered to 0.92 in tuning handoff, got %g", d.DedupThreshold)
	}
}

// Bug 4 — Phase 1 silent-skips with a phase_failures entry when embedding
// coverage gap exceeds 5%.
func TestPhase1_SkipsOnEmbeddingGap(t *testing.T) {
	bank := newTestBank(t)
	pc := newPipelineContextForTest(t, bank)
	pc.stats.EmbeddingCoverage = &EmbeddingCoverageStats{
		Live: 1000, Embedded: 500, Unembedded: 500, // 50% gap
	}
	if err := pc.runPhase1Dedup(); err != nil {
		t.Fatalf("phase1: %v", err)
	}
	if len(pc.stats.PhaseFailures) == 0 {
		t.Fatalf("expected PhaseFailures to capture the embedding-gap skip")
	}
	if !strings.Contains(pc.stats.PhaseFailures[0], "embedding_gap_too_large") {
		t.Errorf("expected embedding_gap_too_large in phase_failures, got %q", pc.stats.PhaseFailures[0])
	}
}

// Bug 4 — embedding coverage runs cleanly even on an empty bank.
func TestEmbeddingCoverage_EmptyBank(t *testing.T) {
	bank := newTestBank(t)
	pc := newPipelineContextForTest(t, bank)
	pc.captureEmbeddingCoverage()
	if pc.stats.EmbeddingCoverage == nil {
		t.Fatalf("expected EmbeddingCoverage populated even on empty bank")
	}
	if pc.stats.EmbeddingCoverage.Live != 0 {
		t.Errorf("expected Live=0 on empty bank, got %d", pc.stats.EmbeddingCoverage.Live)
	}
}
