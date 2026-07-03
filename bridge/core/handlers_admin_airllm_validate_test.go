// handlers_admin_airllm_validate_test.go — wave 8d HF token validation
// endpoint. Mocks the HuggingFace whoami-v2 API via httptest and points
// SD_HF_API_BASE_URL at it for each test.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// startMockHF returns a server whose whoami-v2 endpoint responds based
// on the supplied status + body, and records the Authorization header
// it received so tests can assert the token was forwarded.
func startMockHF(t *testing.T, status int, body string, gotAuth *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/whoami-v2" {
			http.NotFound(w, r)
			return
		}
		if gotAuth != nil {
			*gotAuth = r.Header.Get("Authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
}

func TestValidateHFToken_ValidReturnsUserBlob(t *testing.T) {
	bank := newTestBank(t)
	srv := newTestServer(t, bank)

	gotAuth := ""
	hf := startMockHF(t, http.StatusOK,
		`{"name":"anthony","type":"user","orgs":[{"name":"synaptic"},{"name":"prusa"}]}`,
		&gotAuth)
	defer hf.Close()
	t.Setenv("SD_HF_API_BASE_URL", hf.URL)

	req := httptest.NewRequest(http.MethodPost, "/admin/airllm/validate_hf_token",
		strings.NewReader(`{"token":"hf_validtoken1234567890"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAdminAirLLMValidateHFToken(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if gotAuth != "Bearer hf_validtoken1234567890" {
		t.Errorf("HF should have received the token in Authorization header; got %q", gotAuth)
	}
	var resp struct {
		Valid bool `json:"valid"`
		User  struct {
			Name string   `json:"name"`
			Type string   `json:"type"`
			Orgs []string `json:"orgs"`
		} `json:"user"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !resp.Valid {
		t.Errorf("valid: want true, got false")
	}
	if resp.User.Name != "anthony" {
		t.Errorf("user.name: got %q", resp.User.Name)
	}
	if resp.User.Type != "user" {
		t.Errorf("user.type: got %q", resp.User.Type)
	}
	if len(resp.User.Orgs) != 2 || resp.User.Orgs[0] != "synaptic" || resp.User.Orgs[1] != "prusa" {
		t.Errorf("user.orgs: got %v", resp.User.Orgs)
	}
}

func TestValidateHFToken_AuditRecordsUsernameNotToken(t *testing.T) {
	bank := newTestBank(t)
	srv := newTestServer(t, bank)

	hf := startMockHF(t, http.StatusOK,
		`{"name":"anthony","type":"user","orgs":[]}`, nil)
	defer hf.Close()
	t.Setenv("SD_HF_API_BASE_URL", hf.URL)

	const secret = "hf_supersecretvalue9999"
	req := httptest.NewRequest(http.MethodPost, "/admin/airllm/validate_hf_token",
		strings.NewReader(`{"token":"`+secret+`"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.handleAdminAirLLMValidateHFToken(httptest.NewRecorder(), req)

	rows, err := bank.ListAuditLog(AuditFilter{Operation: "airllm_token_validated", Limit: 5})
	if err != nil {
		t.Fatalf("audit query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 audit row, got %d", len(rows))
	}
	row := rows[0]
	if row.EntityID != "HUGGING_FACE_HUB_TOKEN" {
		t.Errorf("entity_id: got %q", row.EntityID)
	}
	// The token MUST NOT appear anywhere in the audit JSON.
	for _, field := range []string{row.AfterJSON, row.BeforeJSON, row.Reason} {
		if strings.Contains(field, secret) {
			t.Errorf("audit row leaked the raw token in field: %s", field)
		}
	}
	if !strings.Contains(row.AfterJSON, "anthony") {
		t.Errorf("after_json should carry the HF username; got %s", row.AfterJSON)
	}
}

func TestValidateHFToken_Invalid401(t *testing.T) {
	bank := newTestBank(t)
	srv := newTestServer(t, bank)
	hf := startMockHF(t, http.StatusUnauthorized, `{"error":"Invalid credentials"}`, nil)
	defer hf.Close()
	t.Setenv("SD_HF_API_BASE_URL", hf.URL)

	req := httptest.NewRequest(http.MethodPost, "/admin/airllm/validate_hf_token",
		strings.NewReader(`{"token":"hf_invalid_garbage"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAdminAirLLMValidateHFToken(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["valid"] != false {
		t.Errorf("valid: want false, got %v", resp["valid"])
	}
	if resp["error"] != "invalid_token" {
		t.Errorf("error tag: want invalid_token, got %v", resp["error"])
	}
	hint, _ := resp["hint"].(string)
	if !strings.Contains(hint, "huggingface.co/settings/tokens") {
		t.Errorf("hint should point at HF settings/tokens; got %q", hint)
	}

	// No audit row written on failure — we only record validated tokens.
	rows, _ := bank.ListAuditLog(AuditFilter{Operation: "airllm_token_validated", Limit: 5})
	if len(rows) != 0 {
		t.Errorf("failed validation must NOT write audit; got %d rows", len(rows))
	}
}

func TestValidateHFToken_Forbidden403(t *testing.T) {
	bank := newTestBank(t)
	srv := newTestServer(t, bank)
	hf := startMockHF(t, http.StatusForbidden, `{"error":"Account suspended"}`, nil)
	defer hf.Close()
	t.Setenv("SD_HF_API_BASE_URL", hf.URL)

	req := httptest.NewRequest(http.MethodPost, "/admin/airllm/validate_hf_token",
		strings.NewReader(`{"token":"hf_suspended"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAdminAirLLMValidateHFToken(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("want 403, got %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["error"] != "forbidden" {
		t.Errorf("error tag: want forbidden, got %v", resp["error"])
	}
}

func TestValidateHFToken_Unreachable(t *testing.T) {
	bank := newTestBank(t)
	srv := newTestServer(t, bank)
	// Point at a port nothing's listening on — net.Dial fails → handler
	// surfaces the unreachable hint.
	t.Setenv("SD_HF_API_BASE_URL", "http://127.0.0.1:1")

	req := httptest.NewRequest(http.MethodPost, "/admin/airllm/validate_hf_token",
		strings.NewReader(`{"token":"hf_x"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAdminAirLLMValidateHFToken(w, req)
	if w.Code != http.StatusBadGateway {
		t.Errorf("want 502 for network failure, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["error"] != "hf_unreachable" {
		t.Errorf("error tag: want hf_unreachable, got %v", resp["error"])
	}
	hint, _ := resp["hint"].(string)
	if !strings.Contains(hint, "network") {
		t.Errorf("hint should mention network; got %q", hint)
	}
}

func TestValidateHFToken_UnexpectedHFStatus(t *testing.T) {
	bank := newTestBank(t)
	srv := newTestServer(t, bank)
	// 503 from HF — handler should treat as unreachable-class, not a
	// "valid" failure with a different tag.
	hf := startMockHF(t, http.StatusServiceUnavailable, `service down`, nil)
	defer hf.Close()
	t.Setenv("SD_HF_API_BASE_URL", hf.URL)

	req := httptest.NewRequest(http.MethodPost, "/admin/airllm/validate_hf_token",
		strings.NewReader(`{"token":"hf_x"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAdminAirLLMValidateHFToken(w, req)
	if w.Code != http.StatusBadGateway {
		t.Errorf("HF 503 → want 502 from us, got %d", w.Code)
	}
}

func TestValidateHFToken_MissingTokenReturns400(t *testing.T) {
	srv := newTestServer(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/admin/airllm/validate_hf_token",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAdminAirLLMValidateHFToken(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing token: want 400, got %d", w.Code)
	}
}

func TestValidateHFToken_WhitespaceOnlyTokenReturns400(t *testing.T) {
	srv := newTestServer(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/admin/airllm/validate_hf_token",
		strings.NewReader(`{"token":"   "}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAdminAirLLMValidateHFToken(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("whitespace token: want 400, got %d", w.Code)
	}
}

func TestValidateHFToken_MethodGuard(t *testing.T) {
	srv := newTestServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/admin/airllm/validate_hf_token", nil)
	w := httptest.NewRecorder()
	srv.handleAdminAirLLMValidateHFToken(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: want 405, got %d", w.Code)
	}
}
