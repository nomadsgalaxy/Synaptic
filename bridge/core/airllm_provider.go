// airllm_provider.go — Tier 2/3 provider that speaks HTTP to a local
// Python AirLLM service (`bridge/airllm/server.py`).
//
// Per `CODE_HANDOFF — AirLLM as peer LLM provider not sidecar (2026-05-10).md`
// AirLLM is a first-class peer LLM provider alongside Ollama + remote
// APIs, NOT a "sidecar." The HTTP shape is unchanged; only the
// positioning (and the directory the Python server lives in) differs.
//
// AirLLM streams model layers from disk per token, letting 70B-405B models
// run on consumer hardware in exchange for ~1-3 tok/s throughput. The
// trade is wrong for Tier 1 (real-time ingest) and Tier 2 (Phase 0b's
// 100/run pace). It's right for Tier 3 (Oracle research) where the user
// already accepts seconds of latency for OpenAI/Anthropic round-trips
// and gets fully-local zero-egress operation in return.
//
// See `Dev/docs/dev/CODE_HANDOFF — AirLLM Tier 3 provider (2026-05-10).md`
// for the full architecture rationale.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// ErrAirLLMEmbedUnsupported is returned by AirLLMProvider.Embed. The router
// already prefers Tier 1 for embeddings (per ForEmbedding's local-only
// rule), so this error should never reach a user-facing path. It exists
// so any caller that does land here gets a clear shape rather than a
// silent zero-vector.
var ErrAirLLMEmbedUnsupported = errors.New("airllm: Embed not supported; use Tier 1 for embeddings")

// AirLLMProvider implements LLMProvider for the local AirLLM sidecar.
// Speaks HTTP /generate + /healthz; does NOT call the Python directly.
type AirLLMProvider struct {
	BaseURL     string        // e.g. http://127.0.0.1:9912
	Model       string        // HuggingFace id, for logging only
	APIKeyEnv   string        // optional bearer-token env var name
	DisplayName string        // for Name() — defaults to "airllm:<model>"
	HTTPClient  *http.Client  // injected for tests
	Timeout     time.Duration // default 600s — AirLLM is slow
}

// NewAirLLMProvider returns an AirLLMProvider with sensible defaults.
// Caller fills in (BaseURL, Model, APIKeyEnv, DisplayName) from the
// stored ProviderConfig.
func NewAirLLMProvider(baseURL, model, apiKeyEnv, displayName string) *AirLLMProvider {
	if baseURL == "" {
		baseURL = envOr("SD_AIRLLM_URL", "http://127.0.0.1:9912")
	}
	if displayName == "" {
		displayName = "airllm:" + model
	}
	return &AirLLMProvider{
		BaseURL:     strings.TrimRight(baseURL, "/"),
		Model:       model,
		APIKeyEnv:   apiKeyEnv,
		DisplayName: displayName,
		Timeout:     10 * time.Minute,
	}
}

// IsLocal — yes. AirLLM is by definition local; the sidecar binds to
// 127.0.0.1 by default. This makes the provider eligible for the
// ModelRouter.ForEmbedding() local check, BUT Embed is unsupported so
// ForEmbedding's caller still falls back to Tier 1 in practice.
func (p *AirLLMProvider) IsLocal() bool { return true }

// Name returns the canonical "airllm:<model>" label used by the
// per-tier budget breakdown and audit logs.
func (p *AirLLMProvider) Name() string {
	if p.DisplayName != "" {
		return p.DisplayName
	}
	return "airllm:" + p.Model
}

// Embed is intentionally unsupported. AirLLM-class models are wasteful
// for embeddings (they're chat-tuned), and SD Core's embedding cache
// requires a fixed-dim space — switching mid-stream would break recall.
// Callers should use Tier 1 (Ollama nomic-embed-text) for embeddings.
func (p *AirLLMProvider) Embed(_ context.Context, _ string) ([]float32, error) {
	return nil, ErrAirLLMEmbedUnsupported
}

// Chat dispatches a single completion via POST /generate. The sidecar
// runs the request synchronously (no streaming) — at AirLLM's throughput
// a 500-token completion takes ~30s on Llama-3.1-70B 4-bit, so the
// ergonomic shape matches Tier 3's existing expectation.
//
// budgetTokens maps to max_tokens. We pass through the full message
// history (no system-prompt magic — the sidecar's prompt format is
// model-aware on the Python side).
func (p *AirLLMProvider) Chat(ctx context.Context, messages []Message, budgetTokens int) (string, error) {
	if budgetTokens <= 0 {
		budgetTokens = 512
	}
	prompt := flattenMessagesToPrompt(messages)
	body := map[string]interface{}{
		"prompt":      prompt,
		"max_tokens":  budgetTokens,
		"temperature": 0.4, // tier-3 default — sidecar README documents
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/generate", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.APIKeyEnv != "" {
		if tok := os.Getenv(p.APIKeyEnv); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("airllm /generate: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("airllm /generate %d: %s", resp.StatusCode, string(respBody))
	}
	var parsed struct {
		Text     string `json:"text"`
		Error    string `json:"error,omitempty"`
		TokensIn int    `json:"tokens_in"`
		// TokensOut is read by the caller via a dedicated path if needed;
		// the LLMProvider interface itself doesn't surface it (the Tier 2
		// budget logic estimates from text length elsewhere). Unused but
		// kept for future parity with OpenAI's usage block.
		TokensOut int `json:"tokens_out"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("airllm parse: %w", err)
	}
	if parsed.Error != "" {
		return "", fmt.Errorf("airllm sidecar error: %s", parsed.Error)
	}
	return parsed.Text, nil
}

// Ping hits /healthz to verify the sidecar is reachable + has its model
// loaded. Used by /settings/providers/{tier}/ping. Returns nil on a 200
// response with `loaded: true`.
func (p *AirLLMProvider) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return fmt.Errorf("airllm /healthz: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("airllm /healthz %d: %s", resp.StatusCode, string(body))
	}
	var parsed struct {
		Status string `json:"status"`
		Loaded bool   `json:"loaded"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("airllm /healthz parse: %w", err)
	}
	if parsed.Status != "ok" {
		return fmt.Errorf("airllm sidecar status=%q (expected ok)", parsed.Status)
	}
	if !parsed.Loaded {
		return errors.New("airllm sidecar reachable but model is not loaded yet")
	}
	return nil
}

func (p *AirLLMProvider) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	t := p.Timeout
	if t <= 0 {
		t = 10 * time.Minute
	}
	return &http.Client{Timeout: t}
}

// flattenMessagesToPrompt joins a Message slice into a single prompt
// string the sidecar can hand to AirLLM directly. The sidecar's Python
// side is responsible for any model-specific chat-template formatting
// (Llama, Mistral, Qwen each have different turn delimiters); this
// helper just preserves the role + content with a simple delimiter so
// the sidecar gets enough structure to reconstruct turns if it wants.
func flattenMessagesToPrompt(messages []Message) string {
	if len(messages) == 0 {
		return ""
	}
	var b strings.Builder
	for i, m := range messages {
		if i > 0 {
			b.WriteString("\n\n")
		}
		role := m.Role
		if role == "" {
			role = "user"
		}
		b.WriteString(strings.ToUpper(role))
		b.WriteString(": ")
		b.WriteString(m.Content)
	}
	return b.String()
}
