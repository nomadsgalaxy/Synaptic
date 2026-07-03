// provider_remote.go — generic OpenAI-compatible LLMProvider.
//
// One implementation covers OpenAI, Anthropic, Mistral, Together, Fireworks,
// any vLLM/Ollama-compat endpoint, and any custom OpenAI-compatible service —
// users supply BaseURL + Model + an env var name that holds the API key.
//
// Why one provider instead of one-per-vendor?
//   - The OpenAI chat-completions wire format is the de-facto standard.
//     Every major vendor either ships a native /v1/chat/completions endpoint
//     (OpenAI itself, Mistral, Together, Fireworks, vLLM) or offers a thin
//     OpenAI-compat shim (Anthropic via the official anthropic-sdk-python's
//     openai-compat layer, Cloudflare Workers AI, etc.).
//   - Vendor-specific extensions (system role placement, tool format, etc.)
//     are handled with conservative defaults that work everywhere.
//   - Adding a new vendor in the future is one settings entry, not a code
//     change.
//
// API keys are NEVER stored in the bank or settings table. The user's
// onboarding UI captures the key, drops it in an env var, and the settings
// row records only the env var NAME (e.g. SD_TIER1_API_KEY). At Chat/Embed
// time the provider reads the env var. This keeps secrets out of the audit
// log, out of /settings GET responses, and out of any backup of the bank.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RemoteProvider implements LLMProvider against any OpenAI-compatible
// chat-completions endpoint.
type RemoteProvider struct {
	// BaseURL is the API root WITHOUT a trailing slash.
	// Examples:
	//   - https://api.openai.com/v1
	//   - https://api.anthropic.com/v1
	//   - https://api.mistral.ai/v1
	//   - https://api.together.xyz/v1
	//   - http://192.168.1.50:8000/v1   (self-hosted)
	BaseURL string
	// Model is the model identifier the endpoint expects (e.g. gpt-4o-mini,
	// claude-haiku-4-5, mistral-small-latest).
	Model string
	// APIKeyEnv is the name of the env var that holds the API key. Resolved
	// at call time, not at construction. Empty string is allowed for endpoints
	// that don't require auth (some self-hosted vLLM deployments).
	APIKeyEnv string
	// SystemPrompt is prepended to every Chat call. Defaults to
	// DefaultRemoteSystemPrompt if empty.
	SystemPrompt string
	// AuthScheme picks the Authorization header format:
	//   "bearer"     → "Authorization: Bearer <key>"   (OpenAI default)
	//   "x-api-key"  → "x-api-key: <key>"              (Anthropic legacy)
	// If empty, uses bearer.
	AuthScheme string
	// HTTPClient lets callers inject a custom client (mostly for tests).
	HTTPClient *http.Client
	// DisplayName is what Name() returns. Falls back to "remote:<model>"
	// or "local:<model>" depending on Local.
	DisplayName string
	// Local marks this provider as living on the local network (LM Studio,
	// llama.cpp, vLLM). When true:
	//   - IsLocal() returns true (router accepts at TierEmbedding,
	//     sensitive AI doesn't auto-disable, egress filter skipped).
	//   - Name() returns "local:<model>" instead of "remote:<model>"
	//     when DisplayName is empty.
	// Set by BuildProvider when kind=custom and BaseURL classifies as
	// local (loopback / RFC1918 / .local / .lan / .internal). v2.6 Bundle E.
	Local bool
	// Timeout overrides the default HTTP client timeout (60 s). Zero
	// means use the default. Plumbed through from
	// ProviderConfig.TimeoutSeconds via providerTimeout().
	Timeout time.Duration
}

// IsLocal returns the Local flag — set true by BuildProvider when the
// BaseURL classifies as local (LM Studio, llama.cpp, vLLM on the user's
// LAN). When false, every Chat/Embed call leaves the user's machine and
// callers MUST filter sensitive content first.
func (p *RemoteProvider) IsLocal() bool { return p.Local }

// Name returns DisplayName when set, else "local:<model>" if Local,
// else "remote:<model>".
func (p *RemoteProvider) Name() string {
	if p.DisplayName != "" {
		return p.DisplayName
	}
	if p.Local {
		return "local:" + p.Model
	}
	return "remote:" + p.Model
}

func (p *RemoteProvider) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &http.Client{Timeout: timeout}
}

// resolveKey reads ONE API key from the configured env var. Returns
// empty string when APIKeyEnv is unset (auth header is then omitted —
// suitable for self-hosted endpoints with no auth).
//
// Supports multi-key rotation: when the env value is comma-separated
// or there are numbered siblings (KEY_1, KEY_2, ...), the provider
// rotates through them on auth/rate-limit failures via resolveAllKeys.
// resolveKey returns the FIRST key for the simple/legacy code path.
func (p *RemoteProvider) resolveKey() string {
	keys := p.resolveAllKeys()
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

// resolveAllKeys returns every API key configured for this provider's
// env var slot — either comma-split inside the bare env var or set as
// numbered suffixes (KEY_1, KEY_2, ...). Empty slice when nothing is
// configured. Used by retry logic to rotate through alternate keys
// when the active one is rate-limited or revoked.
func (p *RemoteProvider) resolveAllKeys() []string {
	if p.APIKeyEnv == "" {
		return nil
	}
	return resolveAPIKeys(p.APIKeyEnv)
}

// authHeader returns the (header, value) pair to set on the request.
// Returns empty strings when no key is configured / available.
func (p *RemoteProvider) authHeader() (string, string) {
	key := p.resolveKey()
	if key == "" {
		return "", ""
	}
	switch strings.ToLower(p.AuthScheme) {
	case "x-api-key":
		return "x-api-key", key
	default:
		return "Authorization", "Bearer " + key
	}
}

// systemPrompt returns the configured prompt, falling back to the safe default.
func (p *RemoteProvider) systemPrompt() string {
	if p.SystemPrompt != "" {
		return p.SystemPrompt
	}
	return DefaultRemoteSystemPrompt
}

// Chat sends an OpenAI-format /v1/chat/completions request and returns the
// first choice's message.content. Wrapped in withRateLimitRetry — cloud
// APIs (OpenAI, Anthropic) DO return 429 under burst, and local
// vLLM/Ollama-proxy deployments return 502 during backend restarts.
func (p *RemoteProvider) Chat(ctx context.Context, messages []Message, budgetTokens int) (string, error) {
	var result string
	err := withRateLimitRetry(ctx, p.Name(), func() error {
		out, ierr := p.chatOnce(ctx, messages, budgetTokens)
		result = out
		return ierr
	})
	return result, err
}

func (p *RemoteProvider) chatOnce(ctx context.Context, messages []Message, budgetTokens int) (string, error) {
	if budgetTokens <= 0 {
		budgetTokens = 512
	}
	messages = prependSystem(p.systemPrompt(), messages)

	// Tier 3 synthesis defaults to temperature=0.2 — the OpenAI/Anthropic
	// API default of 1.0 produces meaningful run-to-run variance on the
	// SAME question over the SAME memories. Lowering to 0.2 keeps
	// natural-sounding answers but eliminates ~all of the noise from
	// non-deterministic decoding. Operators that want creative synthesis
	// can override via the SD_TIER3_TEMPERATURE env var.
	temperature := 0.2
	if raw := envOr("SD_TIER3_TEMPERATURE", ""); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && v >= 0 && v <= 2 {
			temperature = v
		}
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"model":       p.Model,
		"messages":    messages,
		"max_tokens":  budgetTokens,
		"temperature": temperature,
		"stream":      false,
	})
	url := strings.TrimRight(p.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if h, v := p.authHeader(); h != "" {
		req.Header.Set(h, v)
		// Anthropic requires an additional version header.
		if strings.Contains(p.BaseURL, "anthropic.com") {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	}

	resp, err := p.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("remote chat %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		Choices []struct {
			Message Message `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("parse remote chat: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", errors.New("remote chat returned no choices")
	}
	return out.Choices[0].Message.Content, nil
}

// Embed sends an OpenAI-format /v1/embeddings request. Result is normalised
// before return for consistency with OllamaProvider.
//
// IMPORTANT: mixing embedding spaces (Ollama's nomic vs OpenAI's
// text-embedding-3-small) silently breaks cosine similarity in /recall.
// Callers should NEVER use a remote provider for embeddings while Ollama-
// generated embeddings still exist in the bank — there is no automatic
// re-embed pipeline as of this build. The router rejects this case in
// ForEmbedding().
// Embed is wrapped in withRateLimitRetry — see Chat for rationale.
func (p *RemoteProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	var vec []float32
	err := withRateLimitRetry(ctx, p.Name(), func() error {
		out, ierr := p.embedOnce(ctx, text)
		vec = out
		return ierr
	})
	if err != nil {
		return nil, err
	}
	return vec, nil
}

func (p *RemoteProvider) embedOnce(ctx context.Context, text string) ([]float32, error) {
	payload, _ := json.Marshal(map[string]interface{}{
		"model": p.Model,
		"input": text,
	})
	url := strings.TrimRight(p.BaseURL, "/") + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if h, v := p.authHeader(); h != "" {
		req.Header.Set(h, v)
	}

	resp, err := p.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("remote embed %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse remote embed: %w", err)
	}
	if len(out.Data) == 0 || len(out.Data[0].Embedding) == 0 {
		return nil, errors.New("remote embed returned no vector")
	}
	vec := make([]float32, len(out.Data[0].Embedding))
	for i, v := range out.Data[0].Embedding {
		vec[i] = float32(v)
	}
	vecNormalize(vec)
	return vec, nil
}
