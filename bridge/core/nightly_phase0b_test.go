// nightly_phase0b_test.go — focused tests for Phase 0b deep-encoding +
// dirty-flag triggers + Stage 2 gating + /admin/deep-encode-all.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// scriptedTier2 is a deterministic Tier 2 mock for Phase 0b tests.
// fail returns nil = success; non-nil = error. response is the JSON-ish
// blob the mock returns when fail is nil. tokensIn/tokensOut are
// estimated by Phase 0b from the prompt + response automatically.
type scriptedTier2 struct {
	calls    int
	fail     func(call int) error
	response string
}

func (s *scriptedTier2) Name() string  { return "ollama:scripted-tier2" }
func (s *scriptedTier2) IsLocal() bool { return true }
func (s *scriptedTier2) Embed(_ context.Context, _ string) ([]float32, error) {
	return []float32{0.5, 0.5, 0.5, 0.5}, nil
}
func (s *scriptedTier2) Chat(_ context.Context, _ []Message, _ int) (string, error) {
	s.calls++
	if s.fail != nil {
		if err := s.fail(s.calls - 1); err != nil {
			return "", err
		}
	}
	return s.response, nil
}

// pcWithTier2 builds a pipelineContext where pc.router.ForNightly()
// returns the scripted Tier 2 (router.Tier2) and ForEmbedding returns
// a scripted embedder (router.Tier1).
func pcWithTier2(t *testing.T, bank *Bank, t2 LLMProvider) *pipelineContext {
	t.Helper()
	pc := newPipelineContextForTest(t, bank)
	embedder := newScriptedEmbedder()
	pc.router = NewModelRouter(embedder, t2, nil, nil)
	return pc
}

// ──────────────────────────────────────────────────────────────────────────
// Dirty-flag triggers
// ──────────────────────────────────────────────────────────────────────────

func TestDirtyFlag_OnCreate(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "fresh memory"})
	got, _ := bank.GetMemory(rec.ID)
	if got.MarkedDirtyAt == "" {
		t.Errorf("expected marked_dirty_at set on create")
	}
	if got.DirtyReason != "created" {
		t.Errorf("expected dirty_reason=created, got %q", got.DirtyReason)
	}
}

func TestDirtyFlag_OnTextEdit(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "v1"})
	// Simulate Phase 0b having cleaned the row.
	bank.db.Exec(`UPDATE memories SET deep_encoded_at = ?, marked_dirty_at = '', dirty_reason = '' WHERE id = ?`,
		"2026-05-09T00:00:00Z", rec.ID)

	newText := "v2 — substantively different"
	if _, err := bank.UpdateMemory(rec.ID, MemoryUpdate{Text: &newText}); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	got, _ := bank.GetMemory(rec.ID)
	if got.MarkedDirtyAt == "" {
		t.Errorf("expected marked_dirty_at after text edit")
	}
	if got.DirtyReason != "edited" {
		t.Errorf("expected dirty_reason=edited, got %q", got.DirtyReason)
	}
}

func TestDirtyFlag_NoOpOnSensitiveOnlyEdit(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "private"})
	bank.db.Exec(`UPDATE memories SET deep_encoded_at = ?, marked_dirty_at = '', dirty_reason = '' WHERE id = ?`,
		"2026-05-09T00:00:00Z", rec.ID)

	flag := true
	if _, err := bank.UpdateMemory(rec.ID, MemoryUpdate{Sensitive: &flag}); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	got, _ := bank.GetMemory(rec.ID)
	if got.MarkedDirtyAt != "" {
		t.Errorf("sensitive-only flip should NOT dirty the row, got marked_dirty_at=%q", got.MarkedDirtyAt)
	}
}

func TestDirtyFlag_OnTier2ModelUpgrade(t *testing.T) {
	bank := newTestBank(t)
	// Seed two already-deep-encoded memories.
	r1, _ := bank.SaveMemory(MemoryRecord{Text: "old1"})
	r2, _ := bank.SaveMemory(MemoryRecord{Text: "old2"})
	bank.db.Exec(`UPDATE memories SET deep_encoded_at = ?, deep_encoded_model = ?, marked_dirty_at = '', dirty_reason = '' WHERE id IN (?, ?)`,
		"2026-05-09T00:00:00Z", "ollama:llama3.1:8b", r1.ID, r2.ID)

	// Pre-set tier2 so Save sees a "previous" config.
	bank.SaveProviderConfig(Tier2, ProviderConfig{Kind: ProviderOllama, Model: "llama3.1:8b"})

	// Change tier2 model.
	if err := bank.SaveProviderConfig(Tier2, ProviderConfig{Kind: ProviderOllama, Model: "llama3.2:7b"}); err != nil {
		t.Fatalf("SaveProviderConfig: %v", err)
	}
	g1, _ := bank.GetMemory(r1.ID)
	g2, _ := bank.GetMemory(r2.ID)
	if g1.DirtyReason != "model_upgrade" || g2.DirtyReason != "model_upgrade" {
		t.Errorf("expected both memories dirty with reason=model_upgrade, got %q / %q", g1.DirtyReason, g2.DirtyReason)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// PickNextDirtyMemory priority
// ──────────────────────────────────────────────────────────────────────────

func TestPickNextDirtyMemory_PriorityOrdering(t *testing.T) {
	bank := newTestBank(t)
	now := time.Now().UTC()
	prior := now.Add(-1 * time.Hour).Format(time.RFC3339Nano)

	// Seed: a 'periodic' (lowest priority), a 'created' (highest), and an
	// 'edited' (middle). All share the same created_at so the priority
	// CASE drives the ordering.
	rPeriodic, _ := bank.SaveMemory(MemoryRecord{Text: "periodic"})
	rCreated, _ := bank.SaveMemory(MemoryRecord{Text: "created"})
	rEdited, _ := bank.SaveMemory(MemoryRecord{Text: "edited"})

	// Override the dirty markers manually so we can control priority.
	bank.db.Exec(`UPDATE memories SET deep_encoded_at = ?, marked_dirty_at = ?, dirty_reason = ? WHERE id = ?`,
		prior, now.Format(time.RFC3339Nano), "periodic", rPeriodic.ID)
	bank.db.Exec(`UPDATE memories SET deep_encoded_at = '', marked_dirty_at = ?, dirty_reason = 'created' WHERE id = ?`,
		now.Format(time.RFC3339Nano), rCreated.ID)
	bank.db.Exec(`UPDATE memories SET deep_encoded_at = ?, marked_dirty_at = ?, dirty_reason = ? WHERE id = ?`,
		prior, now.Format(time.RFC3339Nano), "edited", rEdited.ID)

	first, ok, err := bank.PickNextDirtyMemory()
	if err != nil || !ok {
		t.Fatalf("PickNextDirtyMemory: ok=%v err=%v", ok, err)
	}
	if first.ID != rCreated.ID {
		t.Errorf("expected 'created' to win priority, got %s (%s)", first.ID, first.DirtyReason)
	}
}

func TestPickNextDirtyMemory_EmptyQueue(t *testing.T) {
	bank := newTestBank(t)
	_, ok, err := bank.PickNextDirtyMemory()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok {
		t.Errorf("empty bank should produce no dirty memories")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 0b
// ──────────────────────────────────────────────────────────────────────────

const validEnrichJSON = `{
  "summary_new": "improved one-line summary",
  "tags_added": ["pkce"],
  "tags_removed": [],
  "region_new": "frontal_lobe",
  "reasoning": "Surface mention of OAuth PKCE warrants the tag."
}`

func TestPhase0b_EnrichesMemoryAndPersists(t *testing.T) {
	bank := newTestBank(t)
	// Seed with one starting tag so Tier 2's tag_added=["pkce"] gives a
	// jaccard distance of exactly 0.5 (intersect=1, union=2), which is
	// AT the surprise threshold — `tagDeltaFraction > 0.5` is strict, so
	// 0.5 does NOT trigger the R3 surprise re-mark. This keeps the test
	// asserting "Phase 0b enriched once" rather than fighting R3.
	// Match validEnrichJSON's `region_new: frontal_lobe` so Tier 2's
	// region suggestion isn't a "surprise" change either.
	rec, _ := bank.SaveMemory(MemoryRecord{
		Text: "OAuth flow notes", Tags: []string{"oauth"}, RegionHint: "frontal_lobe",
	})
	t2 := &scriptedTier2{response: validEnrichJSON}
	pc := pcWithTier2(t, bank, t2)

	if err := pc.runPhase0bDeepEncoding(); err != nil {
		t.Fatalf("phase0b: %v", err)
	}
	got, _ := bank.GetMemory(rec.ID)
	if got.DeepEncodedAt == "" {
		t.Errorf("expected deep_encoded_at populated")
	}
	if got.MarkedDirtyAt != "" {
		t.Errorf("expected dirty marker cleared after enrich, got %q", got.MarkedDirtyAt)
	}
	if got.EnrichedText != "improved one-line summary" {
		t.Errorf("expected enriched_text refreshed, got %q", got.EnrichedText)
	}
	hasPKCE := false
	for _, tag := range got.Tags {
		if strings.EqualFold(tag, "pkce") {
			hasPKCE = true
		}
	}
	if !hasPKCE {
		t.Errorf("expected tag 'pkce' added, got %v", got.Tags)
	}
	if pc.stats.DeepEncoding == nil || pc.stats.DeepEncoding.EnrichedThisRun != 1 {
		t.Errorf("expected DeepEncoding.EnrichedThisRun=1, got %+v", pc.stats.DeepEncoding)
	}
	if pc.stats.SchemaVersion != 3 {
		t.Errorf("expected schema_version bumped to 3, got %d", pc.stats.SchemaVersion)
	}
}

func TestPhase0b_TierTwoUnconfiguredCleanDegrade(t *testing.T) {
	bank := newTestBank(t)
	bank.SaveMemory(MemoryRecord{Text: "lonely"})
	pc := newPipelineContextForTest(t, bank) // pc.router == nil

	if err := pc.runPhase0bDeepEncoding(); err != nil {
		t.Fatalf("expected clean degrade, got error: %v", err)
	}
	if len(pc.stats.PhaseFailures) == 0 {
		t.Errorf("expected PhaseFailures populated when Tier 2 unconfigured")
	}
	if !strings.Contains(pc.stats.PhaseFailures[0], "tier2_unconfigured") {
		t.Errorf("expected tier2_unconfigured in PhaseFailures, got %q", pc.stats.PhaseFailures[0])
	}
}

func TestPhase0b_FailureLogsAndKeepsDirty(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "doomed"})
	t2 := &scriptedTier2{fail: func(int) error { return errors.New("ollama timeout") }}
	pc := pcWithTier2(t, bank, t2)
	pc.settings.DeepEnrichMaxPerRun = 1

	if err := pc.runPhase0bDeepEncoding(); err != nil {
		t.Fatalf("phase0b: %v", err)
	}
	got, _ := bank.GetMemory(rec.ID)
	if got.DeepEncodedAt != "" {
		t.Errorf("expected deep_encoded_at unset on failure, got %q", got.DeepEncodedAt)
	}
	if got.MarkedDirtyAt == "" {
		t.Errorf("expected dirty marker preserved after failure")
	}
	if len(pc.stats.PhaseFailures) == 0 {
		t.Errorf("expected enrich_failed in phase_failures")
	}
}

func TestPhase0b_DisabledByMasterSwitch(t *testing.T) {
	bank := newTestBank(t)
	bank.SaveMemory(MemoryRecord{Text: "x"})
	t2 := &scriptedTier2{response: validEnrichJSON}
	pc := pcWithTier2(t, bank, t2)
	pc.settings.DeepEnrichEnabled = false

	if err := pc.runPhase0bDeepEncoding(); err != nil {
		t.Fatalf("phase0b: %v", err)
	}
	if t2.calls != 0 {
		t.Errorf("expected 0 Tier 2 calls when master switch off, got %d", t2.calls)
	}
}

func TestPhase0b_BudgetCapsEnrichment(t *testing.T) {
	bank := newTestBank(t)
	for i := 0; i < 10; i++ {
		bank.SaveMemory(MemoryRecord{Text: "memory-" + string(rune('a'+i))})
	}
	t2 := &scriptedTier2{response: validEnrichJSON}
	pc := pcWithTier2(t, bank, t2)
	pc.settings.DeepEnrichMaxPerRun = 3

	if err := pc.runPhase0bDeepEncoding(); err != nil {
		t.Fatalf("phase0b: %v", err)
	}
	if pc.stats.DeepEncoding.EnrichedThisRun != 3 {
		t.Errorf("expected 3 enrichments (cap), got %d", pc.stats.DeepEncoding.EnrichedThisRun)
	}
	if pc.stats.DeepEncoding.Remaining < 7 {
		t.Errorf("expected at least 7 remaining in queue, got %d", pc.stats.DeepEncoding.Remaining)
	}
}

func TestPhase0b_CircuitBreakerOnConsecutiveFailures(t *testing.T) {
	bank := newTestBank(t)
	for i := 0; i < 10; i++ {
		bank.SaveMemory(MemoryRecord{Text: "memory-" + string(rune('a'+i))})
	}
	t2 := &scriptedTier2{fail: func(int) error { return errors.New("Ollama down") }}
	pc := pcWithTier2(t, bank, t2)

	pc.runPhase0bDeepEncoding()
	if t2.calls >= 10 {
		t.Errorf("expected circuit breaker to bail before all 10 attempts, got %d calls", t2.calls)
	}
	if t2.calls < 3 {
		t.Errorf("expected at least 3 attempts before breaker trips, got %d", t2.calls)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// JSON parsing forgiveness
// ──────────────────────────────────────────────────────────────────────────

func TestParseDeepEnrichResponse_PlainJSON(t *testing.T) {
	r := parseDeepEnrichResponse(validEnrichJSON)
	if r.SummaryNew != "improved one-line summary" {
		t.Errorf("summary_new not parsed, got %q", r.SummaryNew)
	}
}

func TestParseDeepEnrichResponse_StripsCodeFences(t *testing.T) {
	wrapped := "```json\n" + validEnrichJSON + "\n```"
	r := parseDeepEnrichResponse(wrapped)
	if r.SummaryNew == "" {
		t.Errorf("code-fenced JSON should still parse")
	}
}

func TestParseDeepEnrichResponse_StripsLeadingPreamble(t *testing.T) {
	chatty := "Here is the JSON you asked for:\n\n" + validEnrichJSON + "\n\nLet me know if you want changes."
	r := parseDeepEnrichResponse(chatty)
	if r.SummaryNew == "" {
		t.Errorf("preambled JSON should still parse")
	}
}

func TestParseDeepEnrichResponse_GracefulOnGarbage(t *testing.T) {
	r := parseDeepEnrichResponse("nope, no JSON here")
	if r.SummaryNew != "" || r.RegionNew != "" {
		t.Errorf("garbage input should return empty struct, got %+v", r)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Tag suggestion application
// ──────────────────────────────────────────────────────────────────────────

func TestApplyTagSuggestions(t *testing.T) {
	got := applyTagSuggestions(
		[]string{"a", "b", "remove-me"},
		[]string{"new", "B"},   // "B" should dedupe vs existing "b"
		[]string{"remove-me"}, // should be filtered
	)
	want := []string{"a", "b", "new"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("expected %v, got %v", want, got)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Stage 2 gating
// ──────────────────────────────────────────────────────────────────────────

func TestStage2Gating_LoadEmbeddedMemoriesFiltersUndeepEncoded(t *testing.T) {
	bank := newTestBank(t)
	r1, _ := bank.SaveMemory(MemoryRecord{Text: "raw"})
	r2, _ := bank.SaveMemory(MemoryRecord{Text: "enriched"})
	// Mark r2 deep-encoded; pre-seed embedding for both via the same hash
	// the loader expects.
	bank.db.Exec(`UPDATE memories SET deep_encoded_at = ? WHERE id = ?`, "2026-05-10T00:00:00Z", r2.ID)
	for _, id := range []string{r1.ID, r2.ID} {
		m, _ := bank.GetMemory(id)
		tagsAny := []interface{}{}
		hash := synapseTextHash(synapseEmbedText(map[string]interface{}{"text": m.Text, "tags": tagsAny}))
		bank.SaveEmbedding(hash, []float32{0.5, 0.5, 0.5, 0.5}, "fake")
	}
	pc := newPipelineContextForTest(t, bank)
	gated, err := pc.loadEmbeddedMemories()
	if err != nil {
		t.Fatalf("loadEmbeddedMemories: %v", err)
	}
	if len(gated) != 1 || gated[0].rec.ID != r2.ID {
		t.Errorf("expected only deep-encoded memory in gated set, got %d entries", len(gated))
	}
	all, _ := pc.loadEmbeddedMemoriesAll()
	if len(all) != 2 {
		t.Errorf("expected 2 entries from unfiltered loader, got %d", len(all))
	}
}

// ──────────────────────────────────────────────────────────────────────────
// /admin/deep-encode-all
// ──────────────────────────────────────────────────────────────────────────

func newDeepEncodeAdminServer(t *testing.T, t2 LLMProvider) (*Server, *Bank) {
	t.Helper()
	bank := newTestBank(t)
	embedder := newScriptedEmbedder()
	srv := &Server{
		bank:   bank,
		router: NewModelRouter(embedder, t2, nil, nil),
	}
	return srv, bank
}

// waitForDeepEncodeBackfill polls the in-flight flag until the background
// goroutine releases it, or until the timeout expires. Used by every
// deep-encode-all test that exercises the async 202 path. v2.6 — added
// when the endpoint was refactored from sync to async (handler now
// returns 202 + spawns a goroutine that runs to completion).
func waitForDeepEncodeBackfill(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !deepEncodeAllInFlight.IsRunning() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("deep-encode-all backfill did not complete within %s", timeout)
}

// readDeepEncodeAuditFinalCounts pulls the single
// `admin_deep_encode_all` audit row written by runDeepEncodeAllBackground
// and returns the parsed enriched/error/token counts. Tests assert
// against this rather than the (no-longer-present) sync response body.
type deepEncodeAuditCounts struct {
	Queued          int `json:"queued"`
	Enriched        int `json:"enriched"`
	Errors          int `json:"errors"`
	ErrorsTruncated int `json:"errors_truncated"`
	TokensTotal     int `json:"tokens_total"`
	DurationMs      int `json:"duration_ms"`
}

func readDeepEncodeAuditFinalCounts(t *testing.T, bank *Bank) deepEncodeAuditCounts {
	t.Helper()
	rows, err := bank.ListAuditLog(AuditFilter{
		Operation: "admin_deep_encode_all",
		Limit:     1,
	})
	if err != nil {
		t.Fatalf("ListAuditLog: %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("expected admin_deep_encode_all audit row, found none")
	}
	var c deepEncodeAuditCounts
	if err := json.Unmarshal([]byte(rows[0].AfterJSON), &c); err != nil {
		t.Fatalf("parse audit AfterJSON %q: %v", rows[0].AfterJSON, err)
	}
	return c
}

func TestDeepEncodeAll_HappyPath(t *testing.T) {
	t2 := &scriptedTier2{response: validEnrichJSON}
	srv, bank := newDeepEncodeAdminServer(t, t2)
	for i := 0; i < 3; i++ {
		// Seed with matching tag + region — see TestPhase0b_EnrichesMemoryAndPersists
		// for why (R3 surprise threshold tuning).
		bank.SaveMemory(MemoryRecord{
			Text:       "memory-" + string(rune('a'+i)),
			Tags:       []string{"oauth"},
			RegionHint: "frontal_lobe",
		})
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/deep-encode-all",
		strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	srv.handleAdminDeepEncodeAll(w, req)
	// v2.6: endpoint is async — returns 202 + body{"started":true}, then
	// runs the backfill in a goroutine. Wait for it to drain, then
	// assert against the audit row the background writer persists.
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d body=%s", w.Code, w.Body.String())
	}
	var accepted struct {
		Started      bool `json:"started"`
		TotalAtStart int  `json:"total_at_start"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("parse 202 body: %v", err)
	}
	if !accepted.Started {
		t.Errorf("expected started=true, got %+v", accepted)
	}
	waitForDeepEncodeBackfill(t, 10*time.Second)
	counts := readDeepEncodeAuditFinalCounts(t, bank)
	if counts.Enriched != 3 {
		t.Errorf("expected enriched=3, got %d (errors=%d)", counts.Enriched, counts.Errors)
	}
}

func TestDeepEncodeAll_NoTier2Returns424(t *testing.T) {
	srv := &Server{bank: newTestBank(t), router: NewModelRouter(newScriptedEmbedder(), nil, nil, nil)}
	req := httptest.NewRequest(http.MethodPost, "/admin/deep-encode-all",
		strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	srv.handleAdminDeepEncodeAll(w, req)
	if w.Code != http.StatusFailedDependency {
		t.Errorf("expected 424 when Tier 2 unconfigured, got %d", w.Code)
	}
}

func TestDeepEncodeAll_RejectsBadMethod(t *testing.T) {
	t2 := &scriptedTier2{response: validEnrichJSON}
	srv, _ := newDeepEncodeAdminServer(t, t2)
	req := httptest.NewRequest(http.MethodGet, "/admin/deep-encode-all", nil)
	w := httptest.NewRecorder()
	srv.handleAdminDeepEncodeAll(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 on GET, got %d", w.Code)
	}
}

func TestDeepEncodeAll_ErrorsListedAndCapped(t *testing.T) {
	// Mock lets the pre-flight reachability probe pass (call 0) so the
	// handler returns 202 instead of 503, then fails every subsequent
	// enrichment call so all per-memory work lands in Errors[]. The
	// pre-flight was added in v2.6 to fail fast when Tier 2 is genuinely
	// unreachable — for THIS test we want to exercise the per-memory
	// failure path, so we explicitly allow the probe through.
	t2 := &scriptedTier2{
		fail: func(call int) error {
			if call == 0 {
				return nil // pre-flight probe — allow
			}
			return errors.New("upstream timeout")
		},
		response: validEnrichJSON, // returned for the pre-flight probe
	}
	srv, bank := newDeepEncodeAdminServer(t, t2)
	for i := 0; i < 5; i++ {
		bank.SaveMemory(MemoryRecord{Text: "x"})
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/deep-encode-all", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	srv.handleAdminDeepEncodeAll(w, req)
	// v2.6: async — 202, then background goroutine. The errors land in
	// the audit row's `errors` count (the WS events carry the per-row
	// detail but the test has no hub subscriber).
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d body=%s", w.Code, w.Body.String())
	}
	waitForDeepEncodeBackfill(t, 30*time.Second)
	counts := readDeepEncodeAuditFinalCounts(t, bank)
	if counts.Enriched != 0 {
		t.Errorf("expected enriched=0 when all fail, got %d", counts.Enriched)
	}
	if counts.Errors == 0 {
		t.Errorf("expected errors > 0, got %d", counts.Errors)
	}
}
