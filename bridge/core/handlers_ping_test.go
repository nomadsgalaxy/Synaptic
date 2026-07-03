package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// pingHelper builds a server + saves a tier provider config; returns server.
func pingHelper(t *testing.T, tier TierKey, cfg ProviderConfig) *Server {
	t.Helper()
	bank := newTestBank(t)
	if err := bank.SaveProviderConfig(tier, cfg); err != nil {
		t.Fatalf("SaveProviderConfig: %v", err)
	}
	return &Server{bank: bank}
}

// callPing fires `GET /settings/providers/{tier}/ping` against `server.routes()`.
func callPing(t *testing.T, server *Server, tier string) (int, pingResult) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/settings/providers/"+tier+"/ping", nil)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	var out pingResult
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

// ──────────────────────────────────────────────────────────────────────────
// Ollama
// ──────────────────────────────────────────────────────────────────────────

func TestProviderPing_Ollama_Reachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	server := pingHelper(t, Tier1, ProviderConfig{
		Kind: ProviderOllama, BaseURL: srv.URL, Model: "llama3.2:3b",
	})
	code, res := callPing(t, server, "tier1")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if !res.OK {
		t.Errorf("expected ok:true, got %+v", res)
	}
	if res.Kind != "ollama" {
		t.Errorf("expected kind=ollama, got %q", res.Kind)
	}
}

func TestProviderPing_Ollama_Down(t *testing.T) {
	server := pingHelper(t, Tier1, ProviderConfig{
		Kind: ProviderOllama, BaseURL: "http://127.0.0.1:1", // unreachable
		Model: "llama3.2:3b",
	})
	code, res := callPing(t, server, "tier1")
	if code != http.StatusOK {
		t.Fatalf("ping failure should return HTTP 200 with ok:false, got %d", code)
	}
	if res.OK {
		t.Errorf("expected ok:false, got %+v", res)
	}
	if !strings.Contains(res.Error, "unreachable") {
		t.Errorf("expected 'unreachable' in error, got %q", res.Error)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// OpenAI
// ──────────────────────────────────────────────────────────────────────────

func TestProviderPing_OpenAI_Success(t *testing.T) {
	var seenAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		seenAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	t.Setenv("FAKE_OAI_KEY", "sk-test-123")
	server := pingHelper(t, Tier1, ProviderConfig{
		Kind: ProviderOpenAI, BaseURL: srv.URL, Model: "gpt-4o-mini",
		APIKeyEnv: "FAKE_OAI_KEY",
	})
	code, res := callPing(t, server, "tier1")
	if code != http.StatusOK || !res.OK {
		t.Fatalf("expected ok:true, got code=%d res=%+v", code, res)
	}
	if seenAuth != "Bearer sk-test-123" {
		t.Errorf("expected Bearer auth header, got %q", seenAuth)
	}
}

func TestProviderPing_OpenAI_AuthFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	t.Setenv("FAKE_OAI_KEY", "sk-bad")
	server := pingHelper(t, Tier1, ProviderConfig{
		Kind: ProviderOpenAI, BaseURL: srv.URL, Model: "gpt-4o-mini",
		APIKeyEnv: "FAKE_OAI_KEY",
	})
	code, res := callPing(t, server, "tier1")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if res.OK {
		t.Errorf("expected ok:false on 401")
	}
	if !strings.Contains(res.Error, "401") {
		t.Errorf("expected '401' in error, got %q", res.Error)
	}
	if !strings.Contains(res.Error, "FAKE_OAI_KEY") {
		t.Errorf("expected env var name in 401 error for actionable feedback, got %q", res.Error)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Anthropic
// ──────────────────────────────────────────────────────────────────────────

func TestProviderPing_Anthropic_AuthFail(t *testing.T) {
	var seenXAPIKey, seenAnthVer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenXAPIKey = r.Header.Get("x-api-key")
		seenAnthVer = r.Header.Get("anthropic-version")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	t.Setenv("FAKE_ANT_KEY", "sk-ant-bad")
	server := pingHelper(t, Tier3, ProviderConfig{
		Kind: ProviderAnthropic, BaseURL: srv.URL, Model: "claude-haiku-4-5",
		APIKeyEnv: "FAKE_ANT_KEY",
	})
	code, res := callPing(t, server, "tier3")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if res.OK {
		t.Errorf("expected ok:false")
	}
	if seenXAPIKey != "sk-ant-bad" {
		t.Errorf("expected x-api-key=sk-ant-bad, got %q", seenXAPIKey)
	}
	if seenAnthVer != "2023-06-01" {
		t.Errorf("expected anthropic-version header, got %q", seenAnthVer)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Custom
// ──────────────────────────────────────────────────────────────────────────

func TestProviderPing_Custom_NoAuth(t *testing.T) {
	var seenAuth, seenXAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenXAPIKey = r.Header.Get("x-api-key")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	// Custom provider with empty api_key_env → no auth header.
	server := pingHelper(t, Tier1, ProviderConfig{
		Kind: ProviderCustom, BaseURL: srv.URL, Model: "local-model",
		// APIKeyEnv intentionally empty
	})
	code, res := callPing(t, server, "tier1")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if !res.OK {
		t.Errorf("expected ok:true (mock 200), got %+v", res)
	}
	if seenAuth != "" {
		t.Errorf("expected NO Authorization header for unauth'd custom, got %q", seenAuth)
	}
	if seenXAPIKey != "" {
		t.Errorf("expected NO x-api-key for unauth'd custom, got %q", seenXAPIKey)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Disabled / empty / not-loaded
// ──────────────────────────────────────────────────────────────────────────

func TestProviderPing_Disabled(t *testing.T) {
	// Use a httptest server we expect NEVER to be hit; if it is, fail loudly.
	hit := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit.Add(1)
		_, _ = w.Write([]byte("oops"))
	}))
	defer srv.Close()

	server := pingHelper(t, Tier3, ProviderConfig{
		Kind: ProviderDisabled, BaseURL: srv.URL, // would 200 if reached
	})
	code, res := callPing(t, server, "tier3")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if res.OK {
		t.Errorf("disabled tier should be ok:false, got %+v", res)
	}
	if res.Kind != "disabled" {
		t.Errorf("expected kind=disabled, got %q", res.Kind)
	}
	if !strings.Contains(strings.ToLower(res.Error), "disabled") {
		t.Errorf("expected error mentioning 'disabled', got %q", res.Error)
	}
	if hit.Load() != 0 {
		t.Errorf("disabled tier MUST NOT make a network call, got %d hits", hit.Load())
	}
}

func TestProviderPing_EmptyApiKeyEnv(t *testing.T) {
	hit := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit.Add(1)
	}))
	defer srv.Close()

	server := pingHelper(t, Tier1, ProviderConfig{
		Kind: ProviderOpenAI, BaseURL: srv.URL, Model: "gpt-4o-mini",
		// APIKeyEnv intentionally empty for an openai (remote) provider
	})
	code, res := callPing(t, server, "tier1")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if res.OK {
		t.Errorf("expected ok:false for openai with no api_key_env")
	}
	if !strings.Contains(strings.ToLower(res.Error), "no api_key_env") {
		t.Errorf("expected 'no api_key_env' in error, got %q", res.Error)
	}
	if hit.Load() != 0 {
		t.Errorf("must NOT make network call when api_key_env is empty, got %d hits", hit.Load())
	}
}

func TestProviderPing_EnvNotLoaded(t *testing.T) {
	hit := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit.Add(1)
	}))
	defer srv.Close()

	// Env var name is set in config but the running process doesn't have a value.
	t.Setenv("UNUSED_BUT_REGISTERED_KEY", "")
	server := pingHelper(t, Tier1, ProviderConfig{
		Kind: ProviderOpenAI, BaseURL: srv.URL, Model: "gpt-4o-mini",
		APIKeyEnv: "UNUSED_BUT_REGISTERED_KEY",
	})
	code, res := callPing(t, server, "tier1")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if res.OK {
		t.Errorf("expected ok:false")
	}
	if !strings.Contains(strings.ToLower(res.Error), "restart") {
		t.Errorf("error should tell user to restart, got %q", res.Error)
	}
	if !strings.Contains(res.Error, "UNUSED_BUT_REGISTERED_KEY") {
		t.Errorf("error should name the env var, got %q", res.Error)
	}
	if hit.Load() != 0 {
		t.Errorf("must NOT make a network call when env var resolves empty, got %d hits", hit.Load())
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Cache + dedup
// ──────────────────────────────────────────────────────────────────────────

func TestProviderPing_Cache(t *testing.T) {
	hit := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit.Add(1)
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	server := pingHelper(t, Tier1, ProviderConfig{
		Kind: ProviderOllama, BaseURL: srv.URL, Model: "llama3.2:3b",
	})

	_, res1 := callPing(t, server, "tier1")
	if !res1.OK {
		t.Fatalf("first ping should succeed, got %+v", res1)
	}
	if res1.Cached {
		t.Errorf("first ping should not be cached, got %+v", res1)
	}

	_, res2 := callPing(t, server, "tier1")
	if !res2.OK {
		t.Fatalf("second ping should succeed, got %+v", res2)
	}
	if !res2.Cached {
		t.Errorf("second ping within 30s window should be served from cache, got %+v", res2)
	}
	if hit.Load() != 1 {
		t.Errorf("expected exactly 1 upstream hit (cache), got %d", hit.Load())
	}
}

func TestProviderPing_ConcurrentDedup(t *testing.T) {
	// Slow upstream so concurrent callers race for the same in-flight probe.
	var hit atomic.Int32
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit.Add(1)
		<-gate // hold the upstream until we've launched all callers
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	server := pingHelper(t, Tier1, ProviderConfig{
		Kind: ProviderOllama, BaseURL: srv.URL,
	})

	const N = 5
	var wg sync.WaitGroup
	results := make(chan pingResult, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, r := callPing(t, server, "tier1")
			results <- r
		}()
	}
	// Give the goroutines a moment to all enter the cache.do() path.
	// (No clean signal for "all in flight" without instrumenting the cache.)
	for i := 0; i < 50 && hit.Load() == 0; i++ {
		// busy-wait until the leader hits upstream
	}
	close(gate)
	wg.Wait()
	close(results)

	if hit.Load() != 1 {
		t.Errorf("expected exactly 1 upstream hit (singleflight), got %d", hit.Load())
	}
	count := 0
	for r := range results {
		if !r.OK {
			t.Errorf("all callers should get ok:true, got %+v", r)
		}
		count++
	}
	if count != N {
		t.Errorf("expected %d results, got %d", N, count)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Token budget unchanged
// ──────────────────────────────────────────────────────────────────────────

func TestProviderPing_NoTokensConsumed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	t.Setenv("FAKE_KEY", "sk-x")
	server := pingHelper(t, Tier3, ProviderConfig{
		Kind: ProviderOpenAI, BaseURL: srv.URL, Model: "gpt-4o-mini",
		APIKeyEnv: "FAKE_KEY",
	})

	// Seed a known starting balance so we'd notice a stray bump.
	if err := server.bank.AddTokensUsed("2026-05-09", 100); err != nil {
		t.Fatalf("AddTokensUsed: %v", err)
	}
	before, _ := server.bank.GetTokensUsed("2026-05-09")

	for i := 0; i < 3; i++ {
		_, _ = callPing(t, server, "tier3")
	}

	after, _ := server.bank.GetTokensUsed("2026-05-09")
	if before != after {
		t.Errorf("token budget changed during pings: before=%d after=%d", before, after)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Logs do not contain the key
// ──────────────────────────────────────────────────────────────────────────

func TestProviderPing_NoKeyInLogs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "denied", http.StatusUnauthorized)
	}))
	defer srv.Close()

	const sneakyKey = "sk-sensitive-must-not-appear-in-logs-9876"
	t.Setenv("FAKE_KEY_FOR_LOG_TEST", sneakyKey)

	// Capture log output.
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	server := pingHelper(t, Tier1, ProviderConfig{
		Kind: ProviderOpenAI, BaseURL: srv.URL, Model: "gpt-4o-mini",
		APIKeyEnv: "FAKE_KEY_FOR_LOG_TEST",
	})
	_, res := callPing(t, server, "tier1")
	if res.OK {
		t.Fatalf("expected ok:false on 401")
	}
	logged := buf.String()
	if strings.Contains(logged, sneakyKey) {
		t.Errorf("INVARIANT VIOLATION: log contained key value: %q", logged)
	}
	// Sanity: ping IS logging *something* (key name + length, not value).
	if !strings.Contains(logged, "FAKE_KEY_FOR_LOG_TEST") {
		t.Logf("note: log did not mention the env var name: %q", logged)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Auth + invalid tier + route precedence
// ──────────────────────────────────────────────────────────────────────────

func TestProviderPing_AuthRequired(t *testing.T) {
	t.Setenv("SD_API_TOKEN", "ping-test-token")
	bank := newTestBank(t)
	_ = bank.SaveProviderConfig(Tier1, ProviderConfig{Kind: ProviderOllama, BaseURL: "x"})
	server := &Server{bank: bank}
	handler := newAuthMiddleware(server.routes())

	req := httptest.NewRequest(http.MethodGet, "/settings/providers/tier1/ping", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without token, got %d", rec.Code)
	}
}

func TestProviderPing_InvalidTier(t *testing.T) {
	server := pingHelper(t, Tier1, ProviderConfig{Kind: ProviderOllama})
	code, _ := callPing(t, server, "tier99")
	if code != http.StatusBadRequest {
		t.Errorf("expected 400 for unknown tier, got %d", code)
	}
}

// TestProviderPing_RoutePrecedence verifies that
// `/settings/providers/{tier}/ping` is served by the ping handler and is
// NOT silently caught by the generic /settings/{key} catchall (which would
// 405 for the GET method on a non-existent key).
//
// Mechanism: if the ping handler ran, kind is in the response body. If the
// catchall ran, body would say "not found" or similar.
func TestProviderPing_RoutePrecedence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	server := pingHelper(t, Tier1, ProviderConfig{
		Kind: ProviderOllama, BaseURL: srv.URL,
	})
	req := httptest.NewRequest(http.MethodGet, "/settings/providers/tier1/ping", nil)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ping handler should serve 200, not catchall; got %d body=%s",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"kind"`) {
		t.Errorf("response should be ping JSON shape (have 'kind' field); got %s",
			rec.Body.String())
	}
	// Sanity: a real /settings/{key} GET to /settings/providers (no /ping)
	// hits the providers list handler (which we wrote earlier).
	req2 := httptest.NewRequest(http.MethodGet, "/settings/providers", nil)
	rec2 := httptest.NewRecorder()
	server.routes().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Errorf("baseline /settings/providers should still 200, got %d", rec2.Code)
	}
}
