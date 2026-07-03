package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// envCheckHelper builds a server, writes initial .env, and clears any
// existing process env for the four allow-listed keys (so previous tests
// or the user's real environment can't bleed into in_process_env).
func envCheckHelper(t *testing.T, initialContent string) (*Server, string) {
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
	// Clear all allow-listed keys so the test starts from a known
	// in_process_env=false baseline.
	for k := range envWriteAllowList {
		t.Setenv(k, "")
	}
	return &Server{bank: bank}, envPath
}

func getEnvCheck(t *testing.T, server *Server, key string) (int, envCheckResponse, string) {
	t.Helper()
	url := "/admin/env_check"
	if key != "" {
		url += "?key=" + key
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	var out envCheckResponse
	if rec.Code == http.StatusOK {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out, rec.Body.String()
}

// ──────────────────────────────────────────────────────────────────────────

func TestEnvCheck_AllowListReject(t *testing.T) {
	server, _ := envCheckHelper(t, "")
	code, _, body := getEnvCheck(t, server, "PATH")
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", code, body)
	}
	if !strings.Contains(body, "allow-list") {
		t.Errorf("expected 'allow-list' in error, got %q", body)
	}
	for _, want := range []string{"SD_TIER1_API_KEY", "SD_TIER3_API_KEY"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in allow-list error, got %q", want, body)
		}
	}
}

func TestEnvCheck_MissingKeyParam(t *testing.T) {
	server, _ := envCheckHelper(t, "")
	code, _, body := getEnvCheck(t, server, "")
	if code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing key, got %d: %s", code, body)
	}
}

func TestEnvCheck_KeyPresentNotLoaded(t *testing.T) {
	// Key in .env file, but the running process's env is empty.
	server, _ := envCheckHelper(t, "SD_TIER3_API_KEY=sk-from-env-file-xyz\n")
	code, res, _ := getEnvCheck(t, server, "SD_TIER3_API_KEY")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if !res.InEnvFile {
		t.Errorf("expected in_env_file=true")
	}
	if res.InProcessEnv {
		t.Errorf("expected in_process_env=false (helper clears env)")
	}
	if res.ValueLength != len("sk-from-env-file-xyz") {
		t.Errorf("expected length=%d, got %d", len("sk-from-env-file-xyz"), res.ValueLength)
	}
	if res.Key != "SD_TIER3_API_KEY" {
		t.Errorf("response should echo key, got %q", res.Key)
	}
}

func TestEnvCheck_KeyAbsentNotLoaded(t *testing.T) {
	server, _ := envCheckHelper(t, "FOO=bar\nBAZ=qux\n")
	code, res, _ := getEnvCheck(t, server, "SD_TIER1_API_KEY")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if res.InEnvFile {
		t.Errorf("expected in_env_file=false")
	}
	if res.ValueLength != 0 {
		t.Errorf("expected length=0, got %d", res.ValueLength)
	}
	if res.InProcessEnv {
		t.Errorf("expected in_process_env=false")
	}
}

func TestEnvCheck_KeyPresentAndLoaded(t *testing.T) {
	server, _ := envCheckHelper(t, "SD_TIER1_API_KEY=sk-loaded-into-env\n")
	t.Setenv("SD_TIER1_API_KEY", "sk-loaded-into-env")
	code, res, _ := getEnvCheck(t, server, "SD_TIER1_API_KEY")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if !res.InEnvFile || !res.InProcessEnv {
		t.Errorf("expected both true, got %+v", res)
	}
	if res.ValueLength != len("sk-loaded-into-env") {
		t.Errorf("expected length=%d, got %d", len("sk-loaded-into-env"), res.ValueLength)
	}
}

func TestEnvCheck_QuotedValueLength(t *testing.T) {
	// Quoted value: "abc" → length 3, not 5.
	server, _ := envCheckHelper(t, `SD_TIER1_API_KEY="abc"`+"\n")
	code, res, _ := getEnvCheck(t, server, "SD_TIER1_API_KEY")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if res.ValueLength != 3 {
		t.Errorf("expected length=3 for quoted abc, got %d", res.ValueLength)
	}
}

func TestEnvCheck_QuotedSingleQuotedValue(t *testing.T) {
	server, _ := envCheckHelper(t, "SD_TIER1_API_KEY='hello'\n")
	code, res, _ := getEnvCheck(t, server, "SD_TIER1_API_KEY")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if res.ValueLength != 5 {
		t.Errorf("expected length=5 for 'hello', got %d", res.ValueLength)
	}
}

func TestEnvCheck_DuplicateLines_LastWins(t *testing.T) {
	// First short, second long — last must win.
	content := "SD_TIER1_API_KEY=a\nSD_TIER1_API_KEY=bcd\n"
	server, _ := envCheckHelper(t, content)
	code, res, _ := getEnvCheck(t, server, "SD_TIER1_API_KEY")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if res.ValueLength != 3 {
		t.Errorf("expected last-wins length=3 (bcd), got %d", res.ValueLength)
	}
}

func TestEnvCheck_CommentsIgnored(t *testing.T) {
	// Commented assignment must NOT match; the live one must.
	content := "# SD_TIER1_API_KEY=fake-archived\nSD_TIER1_API_KEY=real\n"
	server, _ := envCheckHelper(t, content)
	code, res, _ := getEnvCheck(t, server, "SD_TIER1_API_KEY")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if !res.InEnvFile {
		t.Errorf("expected in_env_file=true (real line)")
	}
	if res.ValueLength != 4 {
		t.Errorf("expected length=4 (real), got %d", res.ValueLength)
	}
}

func TestEnvCheck_IndentedLineIgnored(t *testing.T) {
	// Per handoff: "Match is case-sensitive and at line start
	// (ignore comments, indented lines)". A leading-whitespace line
	// should NOT match.
	content := "  SD_TIER1_API_KEY=indented-should-not-match\n"
	server, _ := envCheckHelper(t, content)
	code, res, _ := getEnvCheck(t, server, "SD_TIER1_API_KEY")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if res.InEnvFile {
		t.Errorf("indented line should not count; got in_env_file=true")
	}
}

func TestEnvCheck_NoValueLeakInResponse(t *testing.T) {
	// Sneaky-value scan: response JSON must not contain the value or any
	// substring of it.
	const sneaky = "sk-super-sensitive-must-not-appear-anywhere-zzz9999"
	content := "SD_TIER1_API_KEY=" + sneaky + "\n"
	server, _ := envCheckHelper(t, content)

	// Use a sentinel substring (e.g., 8-char unique fragment) to detect
	// any leak, including hex hashes that happen to match (vanishingly
	// unlikely but let's check).
	for _, fragment := range []string{
		sneaky,
		sneaky[:8],
		sneaky[len(sneaky)-8:],
		"sensitive",
		"zzz9999",
	} {
		code, _, body := getEnvCheck(t, server, "SD_TIER1_API_KEY")
		if code != http.StatusOK {
			t.Fatalf("expected 200, got %d", code)
		}
		if strings.Contains(body, fragment) {
			t.Errorf("INVARIANT VIOLATION: response contained value fragment %q: %s", fragment, body)
		}
	}

	// Also verify the response shape has only the documented fields.
	_, res, raw := getEnvCheck(t, server, "SD_TIER1_API_KEY")
	if res.ValueLength != len(sneaky) {
		t.Errorf("length should match raw value length=%d, got %d", len(sneaky), res.ValueLength)
	}
	// Decode generically and check field set.
	var generic map[string]interface{}
	_ = json.Unmarshal([]byte(raw), &generic)
	for k := range generic {
		switch k {
		case "key", "in_env_file", "value_length", "in_process_env":
			// allowed
		default:
			t.Errorf("response contains unexpected field %q which could leak data", k)
		}
	}
}

func TestEnvCheck_NoValueInLogs(t *testing.T) {
	const sneaky = "sk-must-not-be-in-logs-uuuuu"
	content := "SD_TIER1_API_KEY=" + sneaky + "\n"
	server, _ := envCheckHelper(t, content)

	// Capture log output.
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	code, _, _ := getEnvCheck(t, server, "SD_TIER1_API_KEY")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	logged := buf.String()
	if strings.Contains(logged, sneaky) {
		t.Errorf("INVARIANT VIOLATION: log contained value: %q", logged)
	}
	// Per handoff: do NOT log the length either.
	if strings.Contains(logged, "value_length") {
		t.Errorf("logs should not include value_length, got: %q", logged)
	}
	if strings.Contains(logged, "len=") || strings.Contains(logged, "length=") {
		t.Errorf("logs should not include any length field, got: %q", logged)
	}
}

func TestEnvCheck_AuthRequired(t *testing.T) {
	t.Setenv("SD_API_TOKEN", "check-test-token")
	server, _ := envCheckHelper(t, "SD_TIER1_API_KEY=x\n")
	handler := newAuthMiddleware(server.routes())

	// No bearer token → 401
	req := httptest.NewRequest(http.MethodGet, "/admin/env_check?key=SD_TIER1_API_KEY", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without token, got %d", rec.Code)
	}

	// Correct token → 200
	req = httptest.NewRequest(http.MethodGet, "/admin/env_check?key=SD_TIER1_API_KEY", nil)
	req.Header.Set("Authorization", "Bearer check-test-token")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 with token, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestEnvCheck_FileMissing(t *testing.T) {
	bank := newTestBank(t)
	dir := t.TempDir()
	missingPath := filepath.Join(dir, "does-not-exist.env")
	t.Setenv("SD_ENV_FILE_PATH", missingPath)
	for k := range envWriteAllowList {
		t.Setenv(k, "")
	}
	server := &Server{bank: bank}
	code, res, body := getEnvCheck(t, server, "SD_TIER1_API_KEY")
	if code != http.StatusOK {
		t.Fatalf("missing .env should return 200, got %d: %s", code, body)
	}
	if res.InEnvFile {
		t.Errorf("expected in_env_file=false for missing file")
	}
	if res.ValueLength != 0 {
		t.Errorf("expected length=0 for missing file, got %d", res.ValueLength)
	}
}

// TestEnvCheck_NormalizesCRLFInValueLength — when the .env file uses CRLF
// terminators (Windows-host or buggy-pre-fix-env_write origin), the
// reported value_length must NOT include the trailing `\r`. Otherwise the
// dashboard would tell the user "your key is 184 chars" when it's
// actually 183, and the user would mistrust the diagnostic.
func TestEnvCheck_NormalizesCRLFInValueLength(t *testing.T) {
	// `value` is 5 chars; CRLF appends 2 bytes; without normalisation we'd
	// report 6 (value + \r). Must report 5.
	server, _ := envCheckHelper(t, "SD_TIER1_API_KEY=value\r\n")
	code, res, _ := getEnvCheck(t, server, "SD_TIER1_API_KEY")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if !res.InEnvFile {
		t.Errorf("expected in_env_file=true (line is present, just CRLF-terminated)")
	}
	if res.ValueLength != 5 {
		t.Errorf("expected value_length=5 excluding \\r, got %d", res.ValueLength)
	}
}

// TestEnvCheck_RoutePrecedence verifies that /admin/env_check is reached as
// its own handler, not absorbed by some other catchall (the env_write/ping
// route precedence trap).
func TestEnvCheck_RoutePrecedence(t *testing.T) {
	server, _ := envCheckHelper(t, "SD_TIER1_API_KEY=v\n")
	req := httptest.NewRequest(http.MethodGet, "/admin/env_check?key=SD_TIER1_API_KEY", nil)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from env_check handler, got %d body=%s",
			rec.Code, rec.Body.String())
	}
	// Sanity: a wrong-method (POST) on /admin/env_check should hit OUR
	// handler's 405, not be passed to env_write.
	req2 := httptest.NewRequest(http.MethodPost, "/admin/env_check?key=SD_TIER1_API_KEY", nil)
	rec2 := httptest.NewRecorder()
	server.routes().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST to env_check should 405, got %d (catchall absorbed it?)", rec2.Code)
	}
}
