package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ──────────────────────────────────────────────────────────────────────────
// Layer 1: regex scanner
// ──────────────────────────────────────────────────────────────────────────

func TestScanSensitive_RegexHits(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []string // categories expected (subset match — text may match more)
	}{
		{"openai key", "my key is sk-abc123def456ghi789jklm and that's it", []string{"openai_api_key"}},
		{"github token", "use ghp_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789 here", []string{"github_token"}},
		{"aws access", "AKIAIOSFODNN7EXAMPLE is the access key", []string{"aws_access_key"}},
		{"jwt", "token: eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NSJ9.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c", []string{"jwt"}},
		{"bearer", "Authorization: Bearer abcdefghijklmnopqrstuvwxyz", []string{"bearer_token"}},
		{"pem", "-----BEGIN RSA PRIVATE KEY-----\nXXXX", []string{"pem_private_key"}},
		{"password assignment", `password = "hunter2_secret"`, []string{"password_assignment"}},
		{"secret assignment", "api_key=abc123def456ghi", []string{"secret_assignment"}},
		{"ssn", "SSN 123-45-6789 on file", []string{"ssn"}},
		{"credit card valid", "card 4111 1111 1111 1111 charge", []string{"credit_card"}},
		{"email", "contact me at jane.doe@example.com", []string{"email"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scanSensitive(tc.text)
			for _, want := range tc.want {
				found := false
				for _, g := range got {
					if g == want {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected category %q in %v", want, got)
				}
			}
		})
	}
}

func TestScanSensitive_NegativesAndLuhn(t *testing.T) {
	// Innocuous text should produce no hits.
	if hits := scanSensitive("This is a generic discussion of OAuth flows."); len(hits) > 0 {
		t.Errorf("expected no hits on innocuous text, got %v", hits)
	}
	// 13-digit number that fails Luhn shouldn't trigger credit_card category.
	if hits := scanSensitive("contact 1234567890123 fails luhn"); contains(hits, "credit_card") {
		t.Errorf("non-Luhn 13-digit number should not trigger credit_card, got %v", hits)
	}
	// "000-00-0000" SSN should NOT trigger (the regex excludes 000 prefix).
	if hits := scanSensitive("ssn 000-00-0000 placeholder"); contains(hits, "ssn") {
		t.Errorf("placeholder 000- SSN should not trigger, got %v", hits)
	}
}

func contains(s []string, want string) bool {
	for _, x := range s {
		if x == want {
			return true
		}
	}
	return false
}

// ──────────────────────────────────────────────────────────────────────────
// Auto-flag at SaveMemory + UpdateMemory
// ──────────────────────────────────────────────────────────────────────────

func TestSaveMemory_AutoFlagsFromSensitiveTag(t *testing.T) {
	bank := newTestBank(t)
	rec, err := bank.SaveMemory(MemoryRecord{
		Text: "personal note", Tags: []string{"private"},
	})
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}
	if !rec.Sensitive {
		t.Errorf("expected sensitive=true after tag-based auto-flag")
	}
}

func TestSaveMemory_AutoFlagsFromRegex(t *testing.T) {
	bank := newTestBank(t)
	// Realistic-looking OpenAI key in body, no sensitive tag.
	rec, err := bank.SaveMemory(MemoryRecord{
		Text: "deployment uses sk-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789",
		Tags: []string{"deploy"},
	})
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}
	if !rec.Sensitive {
		t.Errorf("expected sensitive=true from regex scanner detecting openai key")
	}
}

func TestSaveMemory_StickyOnReSave(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "x", Tags: []string{"private"}})
	// Re-save same id with non-sensitive tags — sensitive must remain TRUE
	// thanks to the MAX(sensitive, excluded.sensitive) UPSERT semantics.
	rec.Tags = []string{"now-public"}
	rec.Sensitive = false
	again, err := bank.SaveMemory(rec)
	if err != nil {
		t.Fatalf("re-SaveMemory: %v", err)
	}
	if !again.Sensitive {
		// SaveMemory returns the rec passed in (with auto-flag from tags). The
		// truth is in the DB — re-fetch.
		got, _ := bank.GetMemory(rec.ID)
		if !got.Sensitive {
			t.Errorf("sensitive must remain TRUE after re-save with new tags, got %+v", got)
		}
	}
}

func TestUpdateMemory_PatchTrueOrTagPromotes(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "neutral", Tags: []string{"work"}})
	if rec.Sensitive {
		t.Fatalf("baseline should be non-sensitive")
	}

	// Explicit flip via PATCH.
	tr := true
	post, err := bank.UpdateMemory(rec.ID, MemoryUpdate{Sensitive: &tr})
	if err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	if !post.Sensitive {
		t.Errorf("explicit Sensitive=true patch should set the flag")
	}

	// Try to clear with &false — must succeed because tags & text are clean.
	fa := false
	cleared, _ := bank.UpdateMemory(rec.ID, MemoryUpdate{Sensitive: &fa})
	if cleared.Sensitive {
		t.Errorf("explicit clear should drop the flag when content has no triggers")
	}

	// Add a sensitive tag — &false must NOT win, sensitive auto-promotes.
	tags := []string{"work", "private"}
	withTag, _ := bank.UpdateMemory(rec.ID, MemoryUpdate{Tags: &tags, Sensitive: &fa})
	if !withTag.Sensitive {
		t.Errorf("tag trigger must override Sensitive=false, got %+v", withTag)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// MemoryListOpts filters
// ──────────────────────────────────────────────────────────────────────────

func TestListMemories_SensitiveFilters(t *testing.T) {
	bank := newTestBank(t)
	pub, _ := bank.SaveMemory(MemoryRecord{Text: "public note", Tags: []string{"work"}})
	priv, _ := bank.SaveMemory(MemoryRecord{Text: "secret", Tags: []string{"private"}})
	if pub.Sensitive || !priv.Sensitive {
		t.Fatalf("test setup: pub=%v priv=%v", pub.Sensitive, priv.Sensitive)
	}
	// Default: includes both.
	all, _ := bank.ListMemoriesWith(MemoryListOpts{})
	if len(all) != 2 {
		t.Errorf("default list should show both, got %d", len(all))
	}
	// ExcludeSensitive: only public.
	safeOnly, _ := bank.ListMemoriesWith(MemoryListOpts{ExcludeSensitive: true})
	if len(safeOnly) != 1 || safeOnly[0].ID != pub.ID {
		t.Errorf("ExcludeSensitive should return only pub, got %v", ids(safeOnly))
	}
	// OnlySensitive: only private.
	priv2, _ := bank.ListMemoriesWith(MemoryListOpts{OnlySensitive: true})
	if len(priv2) != 1 || priv2[0].ID != priv.ID {
		t.Errorf("OnlySensitive should return only priv, got %v", ids(priv2))
	}
}

func ids(rs []MemoryRecord) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

// ──────────────────────────────────────────────────────────────────────────
// PrepareOracleCall (the egress chokepoint)
// ──────────────────────────────────────────────────────────────────────────

func TestPrepareOracleCall_FiltersSensitive(t *testing.T) {
	t1 := &fakeProvider{name: "t1"}
	t3 := &fakeProvider{name: "t3"}
	r := NewModelRouter(t1, nil, t3, []string{"GravityCAD"})

	pub := MemoryRecord{ID: "pub", Text: "OAuth flow", Tags: []string{"auth"}}
	priv := MemoryRecord{ID: "priv", Text: "secret journal", Tags: []string{"private"}, Sensitive: true}
	pub2 := MemoryRecord{ID: "pub2", Text: "another public memory"}

	out, err := r.PrepareOracleCall(OracleRequest{
		Topic:    "OAuth best practices",
		Memories: []MemoryRecord{pub, priv, pub2},
		Prompt:   "advice please",
		Reason:   "test",
	})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if out.SensitiveExcluded != 1 {
		t.Errorf("expected 1 sensitive excluded, got %d", out.SensitiveExcluded)
	}
	if len(out.Memories) != 2 {
		t.Errorf("expected 2 safe memories, got %d", len(out.Memories))
	}
	for _, m := range out.Memories {
		if m.Sensitive {
			t.Errorf("INVARIANT VIOLATION: sensitive memory %s in safe slice", m.ID)
		}
	}
	if assertNoSensitive(out.Memories) != nil {
		t.Errorf("assertNoSensitive failed")
	}
}

func TestPrepareOracleCall_BlocksLocalConceptTopic(t *testing.T) {
	t3 := &fakeProvider{name: "t3"}
	r := NewModelRouter(&fakeProvider{name: "t1"}, nil, t3, []string{"GravityCAD"})
	_, err := r.PrepareOracleCall(OracleRequest{
		Topic: "GravityCAD", Memories: nil, Prompt: "x",
	})
	if !errors.Is(err, ErrOracleTopicLocal) {
		t.Errorf("expected ErrOracleTopicLocal, got %v", err)
	}
}

func TestPrepareOracleCall_BlocksOnLocalConceptTagsAndContent(t *testing.T) {
	t3 := &fakeProvider{name: "t3"}
	r := NewModelRouter(&fakeProvider{name: "t1"}, nil, t3, []string{"GravityCAD"})

	tagged := MemoryRecord{ID: "t1", Text: "sample", Tags: []string{"GravityCAD"}}
	prose := MemoryRecord{ID: "t2", Text: "I use GravityCAD daily for prototypes", Tags: []string{"work"}}
	clean := MemoryRecord{ID: "t3", Text: "OAuth refresh", Tags: []string{"auth"}}

	out, err := r.PrepareOracleCall(OracleRequest{
		Topic: "design tools",
		Memories: []MemoryRecord{tagged, prose, clean},
		Prompt: "advice",
	})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if out.LocalConceptExcluded != 2 {
		t.Errorf("expected 2 LocalConcept exclusions (tag + prose), got %d", out.LocalConceptExcluded)
	}
	if len(out.Memories) != 1 || out.Memories[0].ID != "t3" {
		t.Errorf("expected only clean memory to pass, got %v", ids(out.Memories))
	}
}

func TestPrepareOracleCall_RedactsPrompt(t *testing.T) {
	r := NewModelRouter(&fakeProvider{name: "t1"}, nil, &fakeProvider{name: "t3"},
		[]string{"GravityCAD", "Gravity"})
	out, err := r.PrepareOracleCall(OracleRequest{
		Topic:  "design",
		Prompt: "Should I use GravityCAD or Gravity for this?",
	})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if strings.Contains(out.Prompt, "GravityCAD") || strings.Contains(out.Prompt, "Gravity") {
		// "GravityCAD" must redact as a unit (longest first), so "Gravity"
		// would not double-redact inside it.
		if !strings.Contains(out.Prompt, "[REDACTED:GravityCAD]") {
			t.Errorf("prompt not redacted: %q", out.Prompt)
		}
	}
	if !strings.Contains(out.Prompt, "[REDACTED:GravityCAD]") {
		t.Errorf("expected GravityCAD redaction marker, got %q", out.Prompt)
	}
	if !strings.Contains(out.Prompt, "[REDACTED:Gravity]") {
		t.Errorf("expected standalone Gravity redaction marker, got %q", out.Prompt)
	}
}

func TestPrepareOracleCall_KillSwitchAndUnconfigured(t *testing.T) {
	// SD_DISABLE_ORACLE wins even when Tier 3 is wired.
	t.Setenv("SD_DISABLE_ORACLE", "1")
	r := NewModelRouter(&fakeProvider{name: "t1"}, nil, &fakeProvider{name: "t3"}, nil)
	if _, err := r.PrepareOracleCall(OracleRequest{Topic: "x", Prompt: "y"}); !errors.Is(err, ErrOracleDisabled) {
		t.Errorf("expected ErrOracleDisabled with kill switch, got %v", err)
	}
	t.Setenv("SD_DISABLE_ORACLE", "0")

	// Unconfigured Tier 3.
	r2 := NewModelRouter(&fakeProvider{name: "t1"}, nil, nil, nil)
	if _, err := r2.PrepareOracleCall(OracleRequest{Topic: "x", Prompt: "y"}); !errors.Is(err, ErrOracleDisabled) {
		t.Errorf("expected ErrOracleDisabled when Tier 3 nil, got %v", err)
	}
}

func TestPrepareOracleCall_AllRedactedReturnsError(t *testing.T) {
	r := NewModelRouter(&fakeProvider{name: "t1"}, nil, &fakeProvider{name: "t3"}, nil)
	priv := MemoryRecord{ID: "p", Text: "x", Sensitive: true}
	_, err := r.PrepareOracleCall(OracleRequest{
		Topic: "ok", Memories: []MemoryRecord{priv},
		// No Prompt — when both are empty, ErrOracleAllRedacted.
	})
	if !errors.Is(err, ErrOracleAllRedacted) {
		t.Errorf("expected ErrOracleAllRedacted when nothing safe to send, got %v", err)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// HTTP: PATCH supports sensitive flip; ?only_sensitive list filter
// ──────────────────────────────────────────────────────────────────────────

func TestHTTP_PatchSensitiveAndListFilter(t *testing.T) {
	bank := newTestBank(t)
	server := &Server{bank: bank}

	rec, _ := bank.SaveMemory(MemoryRecord{Text: "neutral", Tags: []string{"work"}})

	// PATCH sensitive=true via HTTP.
	tr := true
	body, _ := json.Marshal(MemoryUpdate{Sensitive: &tr})
	req := httptest.NewRequest(http.MethodPatch, "/bank/memories/"+rec.ID, bytes.NewReader(body))
	rec1 := httptest.NewRecorder()
	server.routes().ServeHTTP(rec1, req)
	if rec1.Code != http.StatusOK {
		t.Fatalf("PATCH expected 200, got %d: %s", rec1.Code, rec1.Body.String())
	}
	var got MemoryRecord
	_ = json.Unmarshal(rec1.Body.Bytes(), &got)
	if !got.Sensitive {
		t.Errorf("expected sensitive=true after PATCH")
	}

	// ?only_sensitive=1 via HTTP.
	req2 := httptest.NewRequest(http.MethodGet, "/bank/memories?only_sensitive=1", nil)
	rec2 := httptest.NewRecorder()
	server.routes().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET only_sensitive expected 200, got %d", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), rec.ID) {
		t.Errorf("only_sensitive list should include %s, got %s", rec.ID, rec2.Body.String())
	}

	// ?exclude_sensitive=1 should hide it.
	req3 := httptest.NewRequest(http.MethodGet, "/bank/memories?exclude_sensitive=1", nil)
	rec3 := httptest.NewRecorder()
	server.routes().ServeHTTP(rec3, req3)
	if strings.Contains(rec3.Body.String(), rec.ID) {
		t.Errorf("exclude_sensitive list should NOT include %s, got %s", rec.ID, rec3.Body.String())
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Layer 2: AI classifier (unit-tested with a stub provider)
// ──────────────────────────────────────────────────────────────────────────

// scriptedProvider returns canned responses. Useful for testing without Ollama.
type scriptedProvider struct {
	respond func(text string) string
}

func (s *scriptedProvider) Name() string                                         { return "scripted" }
func (s *scriptedProvider) IsLocal() bool                                        { return true }
func (s *scriptedProvider) Embed(_ context.Context, _ string) ([]float32, error) { return nil, nil }
func (s *scriptedProvider) Chat(_ context.Context, msgs []Message, _ int) (string, error) {
	if len(msgs) == 0 {
		return "NO", nil
	}
	return s.respond(msgs[len(msgs)-1].Content), nil
}

func TestSensitiveClassifier_FlagsPositive(t *testing.T) {
	bank := newTestBank(t)
	// Sentinel string must not appear anywhere in the live prompt template
	// (which is included in the formatted user message). Using a real-world
	// name like "Dr. Chen" was previously brittle because the production
	// prompt now ships an example mentioning "Dr. Chen", so the scripted
	// match would fire on every classification regardless of memory text.
	const sensitiveSentinel = "QZK-MEDICAL-TEST-SENTINEL-42"
	scripted := &scriptedProvider{
		respond: func(text string) string {
			if strings.Contains(text, sensitiveSentinel) {
				return "YES"
			}
			return "NO"
		},
	}
	medical, _ := bank.SaveMemory(MemoryRecord{Text: "private journal entry " + sensitiveSentinel + " ongoing concerns"})
	publik, _ := bank.SaveMemory(MemoryRecord{Text: "OAuth callback URL design"})

	// Force re-scan path by clearing the timestamp (SaveMemory resets to '' on insert).
	sc := newSensitiveClassifierConst(bank, scripted, 0, 10)
	sc.pass(context.Background())

	got, _ := bank.GetMemory(medical.ID)
	if !got.Sensitive {
		t.Errorf("expected medical record auto-flagged sensitive, got %+v", got)
	}
	if got.SensitiveCheckedAt == "" {
		t.Errorf("sensitive_checked_at should be set after scan")
	}
	got2, _ := bank.GetMemory(publik.ID)
	if got2.Sensitive {
		t.Errorf("public record should remain non-sensitive, got %+v", got2)
	}
	if got2.SensitiveCheckedAt == "" {
		t.Errorf("public record should also be marked scanned")
	}
}

// TestSensitiveRegex_AuditNamesPattern — when SaveMemory auto-flags via
// regex, an audit row should land with reason="regex:<pattern_name>" so
// the dashboard's formatReason() can render "Matched <Name> pattern".
func TestSensitiveRegex_AuditNamesPattern(t *testing.T) {
	bank := newTestBank(t)
	const fakeKey = "sk-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	rec, err := bank.SaveMemory(MemoryRecord{
		Text: "ops: deploy uses " + fakeKey,
	})
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}
	if !rec.Sensitive {
		t.Fatalf("expected regex auto-flag to mark sensitive, got %+v", rec)
	}
	entries, err := bank.ListAuditLog(AuditFilter{
		EntityType: "trace", EntityID: rec.ID, Operation: "auto_flag_sensitive",
	})
	if err != nil {
		t.Fatalf("ListAuditLog: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 auto_flag_sensitive audit row, got %d", len(entries))
	}
	if !strings.HasPrefix(entries[0].Reason, "regex:") {
		t.Errorf("expected reason to start with regex:, got %q", entries[0].Reason)
	}
	if !strings.Contains(entries[0].Reason, "openai_api_key") {
		t.Errorf("expected openai_api_key in reason, got %q", entries[0].Reason)
	}
	// Belt-and-braces: the audit row must NOT contain the verbatim key.
	for _, field := range []string{entries[0].Reason, entries[0].AfterJSON, entries[0].BeforeJSON} {
		if strings.Contains(field, fakeKey) {
			t.Errorf("INVARIANT VIOLATION: audit field %q leaked the verbatim key", field)
		}
	}
}

func TestSensitiveAI_AuditIncludesJustification(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "spoke to Dr. Chen about my anxiety meds"})

	scripted := &scriptedProvider{
		respond: func(_ string) string {
			return "YES\nMentions specific personal medical history about a named individual."
		},
	}
	sc := newSensitiveClassifierConst(bank, scripted, 0, 10)
	sc.pass(context.Background())

	entries, _ := bank.ListAuditLog(AuditFilter{
		EntityType: "trace", EntityID: rec.ID, Operation: "auto_flag_sensitive",
	})
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(entries))
	}
	r := entries[0].Reason
	if !strings.HasPrefix(r, "ai_classifier:") {
		t.Errorf("expected ai_classifier: prefix, got %q", r)
	}
	if !strings.Contains(r, "personal medical history") {
		t.Errorf("expected justification text in reason, got %q", r)
	}
}

func TestSensitiveAI_NoVerbatimLeakInJustification(t *testing.T) {
	bank := newTestBank(t)
	const sneakyKey = "sk-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "deployment notes"}) // base record
	// MarkSensitiveScan would skip if already sensitive, so seed a non-
	// sensitive record then have the AI flag it. The "AI reply" we mock
	// echoes back the raw key — backend MUST scrub.
	scripted := &scriptedProvider{
		respond: func(_ string) string {
			return "YES\nContains an API key " + sneakyKey + " for production"
		},
	}
	sc := newSensitiveClassifierConst(bank, scripted, 0, 10)
	sc.pass(context.Background())

	entries, _ := bank.ListAuditLog(AuditFilter{
		EntityType: "trace", EntityID: rec.ID, Operation: "auto_flag_sensitive",
	})
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(entries))
	}
	r := entries[0].Reason
	if strings.Contains(r, sneakyKey) {
		t.Errorf("INVARIANT VIOLATION: AI reason leaked verbatim key: %q", r)
	}
	if !strings.Contains(r, "[openai_api_key]") {
		t.Errorf("expected scrubbed substitution [openai_api_key], got %q", r)
	}
}

func TestSensitiveAI_ReasonLengthCap(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "long-justification test memory"})

	longJustification := strings.Repeat("contains lots of personal stuff in this very long sentence ", 20)
	scripted := &scriptedProvider{
		respond: func(_ string) string { return "YES\n" + longJustification },
	}
	sc := newSensitiveClassifierConst(bank, scripted, 0, 10)
	sc.pass(context.Background())

	entries, _ := bank.ListAuditLog(AuditFilter{
		EntityType: "trace", EntityID: rec.ID, Operation: "auto_flag_sensitive",
	})
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(entries))
	}
	r := entries[0].Reason
	// Whole reason has prefix "ai_classifier:scripted:" + justification.
	// Justification portion must be capped to aiJustificationMaxBytes.
	prefix := "ai_classifier:scripted:"
	if !strings.HasPrefix(r, prefix) {
		t.Fatalf("unexpected reason format: %q", r)
	}
	just := strings.TrimPrefix(r, prefix)
	if len(just) > aiJustificationMaxBytes {
		t.Errorf("justification exceeds %d-byte cap: got %d chars (%q)",
			aiJustificationMaxBytes, len(just), just)
	}
}

func TestSensitiveAI_ParseFallback(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "needs scanning"})

	// Malformed reply: no clear YES/NO, no second line. Backend should
	// gracefully degrade to "not sensitive" without crashing.
	scripted := &scriptedProvider{
		respond: func(_ string) string { return "??? maybe ???" },
	}
	sc := newSensitiveClassifierConst(bank, scripted, 0, 10)
	sc.pass(context.Background())

	got, _ := bank.GetMemory(rec.ID)
	if got.Sensitive {
		t.Errorf("malformed AI reply should fail-closed (not sensitive), got sensitive=true")
	}
	// And no audit row was written for the auto-flag (since the flag didn't fire).
	entries, _ := bank.ListAuditLog(AuditFilter{
		EntityType: "trace", EntityID: rec.ID, Operation: "auto_flag_sensitive",
	})
	if len(entries) != 0 {
		t.Errorf("malformed AI reply should not produce auto_flag_sensitive audit, got %d", len(entries))
	}
}

// TestSensitiveAI_YESWithoutJustification — model said YES but no second
// line; reason should still be ai_classifier:<provider> (no trailing colon).
func TestSensitiveAI_YESWithoutJustification(t *testing.T) {
	bank := newTestBank(t)
	rec, _ := bank.SaveMemory(MemoryRecord{Text: "ambiguous content"})

	scripted := &scriptedProvider{
		respond: func(_ string) string { return "YES" }, // no LINE 2
	}
	sc := newSensitiveClassifierConst(bank, scripted, 0, 10)
	sc.pass(context.Background())

	entries, _ := bank.ListAuditLog(AuditFilter{
		EntityType: "trace", EntityID: rec.ID, Operation: "auto_flag_sensitive",
	})
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(entries))
	}
	r := entries[0].Reason
	if r != "ai_classifier:scripted" {
		t.Errorf("expected reason=ai_classifier:scripted (no trailing detail), got %q", r)
	}
}

func TestSensitiveClassifier_DoesNotRescanUnchanged(t *testing.T) {
	bank := newTestBank(t)
	calls := 0
	scripted := &scriptedProvider{
		respond: func(_ string) string { calls++; return "NO" },
	}
	_, _ = bank.SaveMemory(MemoryRecord{Text: "hello"})
	sc := newSensitiveClassifierConst(bank, scripted, 0, 10)

	sc.pass(context.Background())
	first := calls
	sc.pass(context.Background())
	if calls != first {
		t.Errorf("classifier should not re-scan unchanged records (calls: %d → %d)", first, calls)
	}
}
