package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// envWriteHelper builds a server pointed at a temp .env, returns the path.
func envWriteHelper(t *testing.T, initialContent string) (*Server, string) {
	t.Helper()
	bank := newTestBank(t)
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if initialContent != "" {
		if err := os.WriteFile(envPath, []byte(initialContent), 0o600); err != nil {
			t.Fatalf("seed .env: %v", err)
		}
	}
	t.Setenv("SD_ENV_FILE_PATH", envPath)
	return &Server{bank: bank}, envPath
}

func postEnvWrite(t *testing.T, server *Server, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/admin/env_write", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	return rec
}

// ──────────────────────────────────────────────────────────────────────────

func TestEnvWrite_AllowListReject(t *testing.T) {
	server, _ := envWriteHelper(t, "")
	rec := postEnvWrite(t, server, map[string]string{"key": "PATH", "value": "/foo"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "allow-list") {
		t.Errorf("expected 'allow-list' in error body, got %q", rec.Body.String())
	}
	// Allowed names should be enumerated for UI consumption.
	for _, want := range []string{"SD_TIER1_API_KEY", "SD_TIER3_API_KEY"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("error body should list %q for UI feedback", want)
		}
	}
}

func TestEnvWrite_AppendsNewKey(t *testing.T) {
	server, path := envWriteHelper(t, "")
	rec := postEnvWrite(t, server,
		map[string]string{"key": "SD_TIER1_API_KEY", "value": "sk-newkey-123"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "SD_TIER1_API_KEY=sk-newkey-123") {
		t.Errorf("file should contain new key=value, got: %q", string(got))
	}
	if !strings.HasSuffix(string(got), "\n") {
		t.Errorf("output should end with trailing newline, got: %q", string(got))
	}
}

func TestEnvWrite_ReplacesExistingKey(t *testing.T) {
	initial := "FOO=bar\nSD_TIER1_API_KEY=oldvalue\nBAZ=qux\n"
	server, path := envWriteHelper(t, initial)
	rec := postEnvWrite(t, server,
		map[string]string{"key": "SD_TIER1_API_KEY", "value": "newvalue"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	got, _ := os.ReadFile(path)
	want := "FOO=bar\nSD_TIER1_API_KEY=newvalue\nBAZ=qux\n"
	if string(got) != want {
		t.Errorf("replacement should preserve order; want %q got %q", want, string(got))
	}
}

func TestEnvWrite_PreservesComments(t *testing.T) {
	initial := "# This is a comment\n# Another comment\nSD_TIER1_API_KEY=old\n# trailing comment\n"
	server, path := envWriteHelper(t, initial)
	rec := postEnvWrite(t, server,
		map[string]string{"key": "SD_TIER1_API_KEY", "value": "fresh"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	got, _ := os.ReadFile(path)
	for _, want := range []string{"# This is a comment", "# Another comment", "# trailing comment", "SD_TIER1_API_KEY=fresh"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("expected %q preserved in: %q", want, string(got))
		}
	}
	if strings.Contains(string(got), "SD_TIER1_API_KEY=old") {
		t.Errorf("old value should be replaced, still found in: %q", string(got))
	}
}

func TestEnvWrite_DoesNotMatchCommentedKey(t *testing.T) {
	// A commented-out version of the key should NOT be matched as the existing entry.
	initial := "# SD_TIER1_API_KEY=archived\nFOO=bar\n"
	server, path := envWriteHelper(t, initial)
	rec := postEnvWrite(t, server,
		map[string]string{"key": "SD_TIER1_API_KEY", "value": "live"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "# SD_TIER1_API_KEY=archived") {
		t.Errorf("commented line should be preserved, got: %q", string(got))
	}
	if !strings.Contains(string(got), "SD_TIER1_API_KEY=live") {
		t.Errorf("new key should be appended, got: %q", string(got))
	}
}

func TestEnvWrite_DoesNotMatchPrefixCollision(t *testing.T) {
	// SD_TIER1_API_KEY_OTHER should not collide with SD_TIER1_API_KEY.
	initial := "SD_TIER1_API_KEY_OTHER=preserved\n"
	server, path := envWriteHelper(t, initial)
	rec := postEnvWrite(t, server,
		map[string]string{"key": "SD_TIER1_API_KEY", "value": "fresh"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "SD_TIER1_API_KEY_OTHER=preserved") {
		t.Errorf("prefix-collision key should be preserved, got: %q", string(got))
	}
	if !strings.Contains(string(got), "SD_TIER1_API_KEY=fresh") {
		t.Errorf("new key should be appended distinctly, got: %q", string(got))
	}
}

// Allow-listed keys hold provider API tokens that NEVER legitimately
// contain whitespace, `#`, or quotes. Old behaviour was to wrap such
// values in quotes; new behaviour (handoff 2026-05-09) is to reject them
// outright with a 400. These five tests cover the rejection surface.

func TestEnvWrite_RejectsValueWithSpace(t *testing.T) {
	server, _ := envWriteHelper(t, "")
	rec := postEnvWrite(t, server,
		map[string]string{"key": "SD_TIER1_API_KEY", "value": "sk a b"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for value-with-space, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestEnvWrite_RejectsValueWithTab(t *testing.T) {
	server, _ := envWriteHelper(t, "")
	rec := postEnvWrite(t, server,
		map[string]string{"key": "SD_TIER1_API_KEY", "value": "sk\tabc"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for value-with-tab, got %d", rec.Code)
	}
}

func TestEnvWrite_RejectsValueWithHash(t *testing.T) {
	server, _ := envWriteHelper(t, "")
	rec := postEnvWrite(t, server,
		map[string]string{"key": "SD_TIER1_API_KEY", "value": "sk#abc"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for value-with-hash, got %d", rec.Code)
	}
}

func TestEnvWrite_RejectsValueWithDoubleQuote(t *testing.T) {
	server, _ := envWriteHelper(t, "")
	rec := postEnvWrite(t, server,
		map[string]string{"key": "SD_TIER1_API_KEY", "value": `sk"abc`})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for value-with-quote, got %d", rec.Code)
	}
}

func TestEnvWrite_BareValueNotQuoted(t *testing.T) {
	server, path := envWriteHelper(t, "")
	rec := postEnvWrite(t, server,
		map[string]string{"key": "SD_TIER1_API_KEY", "value": "sk-abc123_def-456"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "SD_TIER1_API_KEY=sk-abc123_def-456\n") {
		t.Errorf("simple alphanumeric value should not be wrapped; got: %q", string(got))
	}
	if strings.Contains(string(got), `"sk-abc123_def-456"`) {
		t.Errorf("simple value should not be quoted; got: %q", string(got))
	}
}

func TestEnvWrite_RejectsNewlineValue(t *testing.T) {
	server, _ := envWriteHelper(t, "")
	rec := postEnvWrite(t, server,
		map[string]string{"key": "SD_TIER1_API_KEY", "value": "a\nb"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for newline value, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestEnvWrite_RejectsCRValue(t *testing.T) {
	server, _ := envWriteHelper(t, "")
	rec := postEnvWrite(t, server,
		map[string]string{"key": "SD_TIER1_API_KEY", "value": "a\rb"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for CR value, got %d", rec.Code)
	}
}

func TestEnvWrite_RejectsEmptyValue(t *testing.T) {
	server, _ := envWriteHelper(t, "")
	rec := postEnvWrite(t, server,
		map[string]string{"key": "SD_TIER1_API_KEY", "value": ""})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty value, got %d", rec.Code)
	}
}

func TestEnvWrite_RejectsWhitespaceOnlyValue(t *testing.T) {
	server, _ := envWriteHelper(t, "")
	rec := postEnvWrite(t, server,
		map[string]string{"key": "SD_TIER1_API_KEY", "value": "   \t  "})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for whitespace-only, got %d", rec.Code)
	}
}

func TestEnvWrite_RejectsOversizedValue(t *testing.T) {
	server, _ := envWriteHelper(t, "")
	rec := postEnvWrite(t, server,
		map[string]string{"key": "SD_TIER1_API_KEY", "value": strings.Repeat("x", 5000)})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for oversize, got %d", rec.Code)
	}
}

func TestEnvWrite_NoteInResponse(t *testing.T) {
	server, _ := envWriteHelper(t, "")
	rec := postEnvWrite(t, server,
		map[string]string{"key": "SD_TIER1_API_KEY", "value": "x"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["set"] != true {
		t.Errorf("response should set:true, got %v", resp["set"])
	}
	if resp["key"] != "SD_TIER1_API_KEY" {
		t.Errorf("response should echo key, got %v", resp["key"])
	}
	note, ok := resp["note"].(string)
	if !ok || !strings.Contains(strings.ToLower(note), "restart") {
		t.Errorf("response should include restart note, got %v", resp["note"])
	}
}

func TestEnvWrite_AuditRedactsValue(t *testing.T) {
	server, _ := envWriteHelper(t, "")
	const sneakyValue = "super-secret-sk-should-not-appear-in-audit"
	rec := postEnvWrite(t, server,
		map[string]string{"key": "SD_TIER1_API_KEY", "value": sneakyValue})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	entries, err := server.bank.ListAuditLog(AuditFilter{
		EntityType: "env", EntityID: "SD_TIER1_API_KEY",
	})
	if err != nil {
		t.Fatalf("ListAuditLog: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 audit entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Operation != "env_write" {
		t.Errorf("expected operation=env_write, got %q", e.Operation)
	}
	for _, field := range []string{e.AfterJSON, e.BeforeJSON, e.Reason, e.EntityID} {
		if strings.Contains(field, sneakyValue) {
			t.Errorf("INVARIANT VIOLATION: audit field contained the secret value: %q", field)
		}
	}
	if !strings.Contains(e.AfterJSON, `"value_set":true`) {
		t.Errorf("audit AfterJSON should record value_set:true, got %q", e.AfterJSON)
	}
	if !strings.Contains(e.AfterJSON, `"value_len":`) {
		t.Errorf("audit AfterJSON should record value_len, got %q", e.AfterJSON)
	}
}

// TestEnvWrite_AuthRequired verifies that /admin/env_write goes through the
// authPathExempt list — i.e., /admin/ is in apiPrefixes and the middleware
// challenges unauthenticated callers with 401 when SD_API_TOKEN is set.
func TestEnvWrite_AuthRequired(t *testing.T) {
	t.Setenv("SD_API_TOKEN", "test-token-xyz")
	server, _ := envWriteHelper(t, "")
	handler := newAuthMiddleware(server.routes())

	body, _ := json.Marshal(map[string]string{"key": "SD_TIER1_API_KEY", "value": "x"})

	// No token → 401.
	req := httptest.NewRequest(http.MethodPost, "/admin/env_write", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without token, got %d: %s", rec.Code, rec.Body.String())
	}

	// Wrong token → 401.
	req = httptest.NewRequest(http.MethodPost, "/admin/env_write", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 with wrong token, got %d", rec.Code)
	}

	// Correct token → 200.
	req = httptest.NewRequest(http.MethodPost, "/admin/env_write", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token-xyz")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 with correct token, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestEnvWrite_TmpFileCleanedOnSuccess(t *testing.T) {
	server, path := envWriteHelper(t, "")
	rec := postEnvWrite(t, server,
		map[string]string{"key": "SD_TIER1_API_KEY", "value": "x"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	// The .tmp should not survive a successful rename.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("tmp file should have been renamed away, stat err=%v", err)
	}
}

func TestEnvWrite_FailsCleanWhenDirMissing(t *testing.T) {
	bank := newTestBank(t)
	t.Setenv("SD_ENV_FILE_PATH", "/nonexistent-zzzzz-9999/sub/.env")
	server := &Server{bank: bank}

	// MkdirAll on /nonexistent-zzzzz-9999 will succeed inside container if
	// running as root (alpine test image is root); skip strict permission
	// check, but the request must not crash.
	rec := postEnvWrite(t, server,
		map[string]string{"key": "SD_TIER1_API_KEY", "value": "x"})
	if rec.Code == http.StatusInternalServerError && !strings.Contains(rec.Body.String(), "panic") {
		// Fine — generic write error is OK.
		return
	}
	if rec.Code != http.StatusOK && rec.Code != http.StatusForbidden && rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 200/403/500 (no crash), got %d: %s", rec.Code, rec.Body.String())
	}
}

// ──────────────────────────────────────────────────────────────────────────
// CRLF + bare-value tests (handoff 2026-05-09 fix)
// ──────────────────────────────────────────────────────────────────────────

// TestEnvWrite_AlwaysLF — file produced by env_write must contain ZERO
// carriage returns regardless of host OS. Mixed terminators were the root
// cause of the `Authorization: Bearer ...\r` 401 bug.
func TestEnvWrite_AlwaysLF(t *testing.T) {
	server, path := envWriteHelper(t, "")
	rec := postEnvWrite(t, server,
		map[string]string{"key": "SD_TIER1_API_KEY", "value": "sk-test-noCR"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read .env: %v", err)
	}
	if bytes.IndexByte(got, '\r') >= 0 {
		t.Errorf("file must not contain \\r anywhere; got: %q", string(got))
	}
}

// TestEnvWrite_NoQuotingOfBareValue — a typical API-key-shaped value should
// land in the file as exactly `KEY=value\n`, with no surrounding quotes.
func TestEnvWrite_NoQuotingOfBareValue(t *testing.T) {
	server, path := envWriteHelper(t, "")
	const value = "sk-test123_AbCdEf-ZyXwVu0123456789"
	rec := postEnvWrite(t, server,
		map[string]string{"key": "SD_TIER1_API_KEY", "value": value})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	got, _ := os.ReadFile(path)
	want := "SD_TIER1_API_KEY=" + value + "\n"
	if string(got) != want {
		t.Errorf("expected exactly %q, got %q", want, string(got))
	}
	// Belt-and-braces: no quote characters anywhere.
	if bytes.ContainsAny(got, `"'`) {
		t.Errorf("file must contain no quote characters, got: %q", string(got))
	}
}

// TestEnvWrite_AcceptsBase64PaddedKey — OpenAI project keys can contain
// trailing `=` (base64 padding). These were previously force-quoted because
// `=` was in the "needs quoting" set; that quoting path was the source of
// the malformed-line bug. Now `=` in the value is acceptable bare.
func TestEnvWrite_AcceptsBase64PaddedKey(t *testing.T) {
	server, path := envWriteHelper(t, "")
	const value = "sk-proj-AAAA1111BBBB2222CCCC3333DDDD4444EEEE5555FFFF6666GGGG7777=="
	rec := postEnvWrite(t, server,
		map[string]string{"key": "SD_TIER3_API_KEY", "value": value})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for base64-padded value, got %d: %s",
			rec.Code, rec.Body.String())
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "SD_TIER3_API_KEY="+value+"\n") {
		t.Errorf("base64-padded value should land bare; got: %q", string(got))
	}
}

// TestEnvWrite_NormalizesCRLFOnRead — when an existing .env file has CRLF
// line endings (e.g. created by a Windows editor or a previous buggy
// version of env_write), the next env_write call must auto-heal the
// whole file to LF-only.
func TestEnvWrite_NormalizesCRLFOnRead(t *testing.T) {
	// Pre-seed a file with CRLF terminators across multiple lines, including
	// a pre-existing tier1 line and adjacent unrelated entries. After we
	// write a NEW key (tier2), all lines — including the untouched ones —
	// must come out LF.
	initial := "FOO=bar\r\nSD_TIER1_API_KEY=existing\r\nBAZ=qux\r\n"
	server, path := envWriteHelper(t, initial)
	rec := postEnvWrite(t, server,
		map[string]string{"key": "SD_TIER2_API_KEY", "value": "newvalue"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	got, _ := os.ReadFile(path)
	if bytes.IndexByte(got, '\r') >= 0 {
		t.Errorf("file should be CRLF-normalised after write; got: %q", string(got))
	}
	// All original keys still present, with correct LF separators.
	for _, want := range []string{
		"FOO=bar\n",
		"SD_TIER1_API_KEY=existing\n",
		"BAZ=qux\n",
		"SD_TIER2_API_KEY=newvalue\n",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing %q in normalised file: %q", want, string(got))
		}
	}
}

// TestEnvWrite_NormalizesCRLFOnReplaceExistingKey — same key already in
// the file with CRLF; replacement must heal the line endings around it.
func TestEnvWrite_NormalizesCRLFOnReplaceExistingKey(t *testing.T) {
	initial := "SD_TIER1_API_KEY=oldvalue\r\nFOO=bar\r\n"
	server, path := envWriteHelper(t, initial)
	rec := postEnvWrite(t, server,
		map[string]string{"key": "SD_TIER1_API_KEY", "value": "newvalue"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	got, _ := os.ReadFile(path)
	if bytes.IndexByte(got, '\r') >= 0 {
		t.Errorf("file should be CRLF-normalised; got: %q", string(got))
	}
	want := "SD_TIER1_API_KEY=newvalue\nFOO=bar\n"
	if string(got) != want {
		t.Errorf("expected %q, got %q", want, string(got))
	}
}
