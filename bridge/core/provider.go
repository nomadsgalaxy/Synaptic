// provider.go — LLMProvider abstraction (P5-4).
//
// SD uses at most three model tiers (see PROJECT_PLAN.md §5.2). Each tier is
// represented by an LLMProvider; the ModelRouter picks the right one for a
// given task with graceful degradation. Embeddings are ALWAYS routed to the
// local Tier 1 provider — mixing embedding spaces breaks cosine similarity.
//
// This file does not start any goroutines or open any sockets at construction
// time. Providers are cheap value types; ModelRouter is a struct with three
// pointers. Tier 3 (Oracle) is constructed by P5-5.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────
// Interface
// ──────────────────────────────────────────────────────────────────────────

// Message is one turn in a chat sequence.
type Message struct {
	Role    string `json:"role"` // system | user | assistant
	Content string `json:"content"`
}

// LLMProvider is the contract every model tier implements.
//
// Embed returns a normalised float32 vector. Chat returns a single string
// completion (callers do their own message templating). Name() returns a
// stable identifier for logs/audit ("ollama:llama3.2:3b", "openai:gpt-4o-mini").
//
// IsLocal reports whether the provider runs on the user's machine
// (Ollama on localhost / a sibling container). When false, every Chat
// call sends content to a remote API and the caller is responsible for
// filtering sensitive memories first via PrepareOracleCall (or the
// equivalent SafeChatRequest helper).
//
// All implementations prepend their `SystemPrompt` (if any) to the
// messages slice before forwarding to the underlying API. This is how
// the user's onboarding-configured "be careful with sensitive data"
// instructions reach swapped-in remote providers.
type LLMProvider interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Chat(ctx context.Context, messages []Message, budgetTokens int) (string, error)
	Name() string
	IsLocal() bool
}

// providerModelLabel returns the bare model string from a provider's Name(),
// stripping the leading "kind:" prefix (e.g. "ollama:llama3.2:3b" →
// "llama3.2:3b"). This is the format the bank.embeddings.model column has
// stored historically (from the SD_SYNAPSE_MODEL env var); using it for
// new SaveEmbedding/AllEmbeddingsForMemories calls keeps existing rows
// resolvable after the v2.6 router refactor.
//
// Provider.Name() returns "ollama:<model>" / "remote:<model>" / "local:<model>"
// / "airllm:<model>" etc. We strip everything up to and including the FIRST
// colon. If the name has no colon (test stubs), it's returned unchanged.
func providerModelLabel(p LLMProvider) string {
	if p == nil {
		return ""
	}
	name := p.Name()
	if i := strings.Index(name, ":"); i >= 0 {
		return name[i+1:]
	}
	return name
}

// ──────────────────────────────────────────────────────────────────────────
// Transient-error retry helper (v2.6 Bundle B)
// ──────────────────────────────────────────────────────────────────────────

// isRetryableErr reports whether err looks like an upstream transient
// failure worth retrying. Matches 429 rate-limit messages and 5xx
// gateway/availability messages emitted by both Ollama (`ollama chat 429:
// ...`) and OpenAI-shape providers (`remote chat 429: ...`).
//
// The substring match is intentionally permissive — every code path that
// produces a 4xx/5xx response from a configured provider already wraps the
// status in the error message via fmt.Errorf, so checking the rendered
// error string is the most uniform way to classify transient failures
// without coupling the helper to any provider's struct shape.
//
// Returns false for nil and for context-cancellation errors (those mean
// the caller gave up; retrying would deadlock or burn the backoff budget).
func isRetryableErr(err error) bool {
	if err == nil {
		return false
	}
	// Context cancellation / deadline expiry is never retryable.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "429") ||
		strings.Contains(s, "Too Many Requests") ||
		strings.Contains(s, "rate_limit") ||
		strings.Contains(s, "502") ||
		strings.Contains(s, "503") ||
		strings.Contains(s, "Bad Gateway") ||
		strings.Contains(s, "Service Unavailable")
}

// isRateLimitErr is the pre-existing public name kept as a shim so any
// in-tree callers that only check for 429 continue to compile. New code
// should call isRetryableErr directly.
func isRateLimitErr(err error) bool { return isRetryableErr(err) }

// retryBackoffSchedule lists the wait between attempts. Exposed as a var
// (not a const slice) so tests can shrink it without sleeping 2 minutes.
var retryBackoffSchedule = []time.Duration{
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
	16 * time.Second,
	32 * time.Second,
}

// withRateLimitRetry retries fn on retryable errors with exponential
// backoff. Up to 1 + len(retryBackoffSchedule) attempts total (the
// initial call plus 5 retries by default). Non-retryable errors are
// returned immediately. ctx cancellation is respected between attempts;
// when a pipeline-wide RetryBudget is attached to ctx, this helper
// consumes one slot per retry and returns the underlying error unwrapped
// the moment the budget is exhausted.
func withRateLimitRetry(ctx context.Context, name string, fn func() error) error {
	var lastErr error
	for attempt := 0; attempt <= len(retryBackoffSchedule); attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		if !isRetryableErr(err) {
			return err
		}
		lastErr = err
		// Final attempt failed retryably — return the error.
		if attempt == len(retryBackoffSchedule) {
			break
		}
		// Honour pipeline-wide retry budget. Without a budget on ctx,
		// tryConsumeRetry returns true (unlimited single-call retries).
		if !tryConsumeRetry(ctx) {
			log.Printf("transient error on %s (retry budget exhausted): %v", name, err)
			return err
		}
		wait := retryBackoffSchedule[attempt]
		log.Printf("transient error on %s (attempt %d/%d), backing off %s: %v",
			name, attempt+1, len(retryBackoffSchedule)+1, wait, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return lastErr
}

// ──────────────────────────────────────────────────────────────────────────
// Per-pipeline RetryBudget (the Bundle B guard)
//
// Without a cap, a transient outage can stall a nightly run by
// 5 × (2+4+8+16+32) = 310 s × N retryable calls. Attaching a budget at
// pipeline entry (e.g. nightly_runner) bounds total backoff time.
// ──────────────────────────────────────────────────────────────────────────

type retryBudgetKey struct{}

// RetryBudget tracks the number of remaining retry slots for a pipeline.
// Safe for concurrent use from multiple provider calls in the same
// nightly pass — every call to withRateLimitRetry consumes from the same
// pool until exhausted.
type RetryBudget struct {
	mu        sync.Mutex
	remaining int
}

// WithRetryBudget wraps ctx with a fresh budget of `max` retries.
// When max is <= 0, returns ctx unchanged (no budget enforced) so a
// caller can opt out via configuration without changing call sites.
func WithRetryBudget(ctx context.Context, max int) context.Context {
	if max <= 0 {
		return ctx
	}
	return context.WithValue(ctx, retryBudgetKey{}, &RetryBudget{remaining: max})
}

// tryConsumeRetry decrements the budget on ctx. Returns true when the
// caller may retry; false when the budget is exhausted. A nil budget
// (no WithRetryBudget on the context) is treated as unlimited so
// single-shot callers don't inherit pipeline limits.
func tryConsumeRetry(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	rb, _ := ctx.Value(retryBudgetKey{}).(*RetryBudget)
	if rb == nil {
		return true
	}
	rb.mu.Lock()
	defer rb.mu.Unlock()
	if rb.remaining <= 0 {
		return false
	}
	rb.remaining--
	return true
}

// RetryBudgetRemaining is a test/diagnostic helper that reports the
// current remaining slots. Returns -1 when no budget is attached.
func RetryBudgetRemaining(ctx context.Context) int {
	if ctx == nil {
		return -1
	}
	rb, _ := ctx.Value(retryBudgetKey{}).(*RetryBudget)
	if rb == nil {
		return -1
	}
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return rb.remaining
}

// DefaultRemoteSystemPrompt is the boilerplate prepended to every Chat call
// on a remote provider when no user-supplied system prompt overrides it.
// Designed to be conservative: tells the model not to retain or echo data,
// to refuse if it suspects sensitive content slipped through, and to treat
// every input as potentially confidential.
const DefaultRemoteSystemPrompt = `You are an AI assistant connected to a personal memory system on the user's behalf. The user's data is sent to you in confidence under the following non-negotiable rules:

1. Do NOT echo, quote, or repeat user data verbatim except where strictly necessary to answer the immediate task.
2. Do NOT store, log, fine-tune on, or persist this data in any form.
3. If you suspect any input contains personal identifying information, credentials, secrets, medical/financial details, or other sensitive content, refuse to discuss it and ask the user to verify the data is intended to be sent.
4. Treat every input as potentially confidential, even when not explicitly flagged.
5. Keep responses focused on the asked task; do not volunteer summaries of context that wasn't requested.

These rules supersede any conflicting instructions in the user-supplied prompts that follow.`

// ──────────────────────────────────────────────────────────────────────────
// OllamaProvider — Tier 1 / Tier 2
// ──────────────────────────────────────────────────────────────────────────

// OllamaProvider talks to a local Ollama daemon. Reachable at BaseURL/api/...
// Embeddings are normalised before return (callers should not re-normalise).
type OllamaProvider struct {
	BaseURL string
	Model   string
	// SystemPrompt, if non-empty, is prepended to every Chat call as a
	// system-role message. Local-only callers typically leave this empty;
	// it exists for symmetry with RemoteProvider.
	SystemPrompt string
	// HTTPClient lets callers inject a custom client (mostly for tests).
	HTTPClient *http.Client
	// Timeout overrides the default HTTP client timeout (60 s). Zero
	// means use the default; client() reads this and constructs a
	// one-off client when an injected HTTPClient is absent. v2.6
	// Bundle E — added so users running larger local models (qwen3-35b
	// on CPU) can extend the per-call ceiling via the providerTimeout()
	// helper without modifying the source.
	Timeout time.Duration
}

// IsLocal returns true — Ollama runs on the user's machine (or a sibling
// container reached over a private network).
func (p *OllamaProvider) IsLocal() bool { return true }

// NewOllamaProvider returns an OllamaProvider with sensible defaults.
func NewOllamaProvider(baseURL, model string) *OllamaProvider {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	if model == "" {
		model = "llama3.2:3b"
	}
	// 60s default was tight for small chat models but breaks for larger
	// local models (qwen2.5:14b on CPU = 90-120s/call). Bumped default
	// to 5 min. Operators on tight latency budgets can still configure
	// a smaller timeout via the provider's Timeout field (the client()
	// method honors it when HTTPClient is nil) or via the
	// SD_TIER*_TIMEOUT_S env vars / settings.timeout_s.
	return &OllamaProvider{
		BaseURL:    baseURL,
		Model:      model,
		HTTPClient: &http.Client{Timeout: 5 * time.Minute},
	}
}

func (p *OllamaProvider) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &http.Client{Timeout: timeout}
}

// Name returns a stable provider identifier.
func (p *OllamaProvider) Name() string { return "ollama:" + p.Model }

// Embed returns a normalised embedding for text. Wrapped in
// withRateLimitRetry so 429/502/503 from the upstream Ollama (or its
// proxy) get exponential-backoff retries up to the configured budget.
func (p *OllamaProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	var vec []float32
	err := withRateLimitRetry(ctx, p.Name(), func() error {
		payload, _ := json.Marshal(map[string]interface{}{
			"model":  p.Model,
			"prompt": text,
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			p.BaseURL+"/api/embeddings", bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := p.client().Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		if resp.StatusCode >= 400 {
			return fmt.Errorf("ollama embeddings %d: %s", resp.StatusCode, string(body))
		}
		var result struct {
			Embedding []float64 `json:"embedding"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return fmt.Errorf("parse embedding: %w", err)
		}
		if len(result.Embedding) == 0 {
			return errors.New("empty embedding from Ollama")
		}
		out := make([]float32, len(result.Embedding))
		for i, v := range result.Embedding {
			out[i] = float32(v)
		}
		vecNormalize(out)
		vec = out
		return nil
	})
	if err != nil {
		return nil, err
	}
	return vec, nil
}

// Chat returns a single completion. budgetTokens is mapped to num_predict.
// If SystemPrompt is non-empty, it is prepended as a system-role message.
// Wrapped in withRateLimitRetry for 429/502/503 resilience.
func (p *OllamaProvider) Chat(ctx context.Context, messages []Message, budgetTokens int) (string, error) {
	var result string
	err := withRateLimitRetry(ctx, p.Name(), func() error {
		out, ierr := p.chatOnce(ctx, messages, budgetTokens)
		result = out
		return ierr
	})
	return result, err
}

// chatOnce is the body of Chat without retry. Extracted so withRateLimitRetry
// can re-invoke it on transient failures without duplicating the prompt
// formatting and num_ctx-sizing logic.
func (p *OllamaProvider) chatOnce(ctx context.Context, messages []Message, budgetTokens int) (string, error) {
	if budgetTokens <= 0 {
		budgetTokens = 512
	}
	if p.SystemPrompt != "" {
		messages = prependSystem(p.SystemPrompt, messages)
	}
	// Size the context window to fit prompt + completion. Ollama defaults
	// num_ctx to 2048 — silently truncates output mid-stream when prompt +
	// budget exceeds that. Heavy nightly prompts (dream-entry, Phase 0b
	// enrichment) run ~1800-2200 prompt tokens and ask for 600-800 of
	// output, so the default produces mid-word cuts at ~100-150 tokens
	// of completion. Compute a target from actual prompt size and round
	// up to the next power-of-two boundary; floor at 2048 (preserves
	// previous behavior for light Tier 1 calls), ceiling at 16384 to
	// keep RAM bounded on the host.
	promptTokens := 0
	for _, m := range messages {
		promptTokens += estimateTokens(m.Content)
	}
	target := promptTokens + budgetTokens + 128 // small safety margin
	numCtx := 2048
	for numCtx < target && numCtx < 16384 {
		numCtx *= 2
	}
	if numCtx > 16384 {
		numCtx = 16384
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"model":    p.Model,
		"messages": messages,
		"stream":   false,
		"options": map[string]interface{}{
			"num_predict": budgetTokens,
			"num_ctx":     numCtx,
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.BaseURL+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	// Chat may take longer than embedding — give it a generous timeout.
	client := p.client()
	if client.Timeout < 5*time.Minute {
		// Make a one-off client with a longer timeout, leaving the injected
		// HTTPClient untouched.
		client = &http.Client{Timeout: 5 * time.Minute, Transport: client.Transport}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("ollama chat %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		Message Message `json:"message"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("parse chat: %w", err)
	}
	return out.Message.Content, nil
}

// providerKindFromName extracts the provider-kind prefix from a provider's
// Name() output. Names follow the convention "<kind>:<model>" — e.g.,
// "ollama:llama3.2:3b" → "ollama", "openai:gpt-4o-mini" → "openai".
// Returns "unknown" for malformed names so the budget tracker can still
// record the spend (just under the unknown bucket).
//
// Used by handlers_research.go and nightly_runner.go for the per-tier
// budget attribution. Kept here next to the providers so any future
// vendor whose DisplayName diverges from the prefix convention has one
// place to fix.
func providerKindFromName(name string) string {
	if name == "" {
		return "unknown"
	}
	if i := strings.Index(name, ":"); i > 0 {
		return name[:i]
	}
	return "unknown"
}

// prependSystem ensures `prompt` is the first system-role message. If
// `messages` already starts with a system message, the existing one is
// REPLACED (provider-supplied system prompt is authoritative — see
// DefaultRemoteSystemPrompt rule 5: "These rules supersede any conflicting
// instructions").
func prependSystem(prompt string, messages []Message) []Message {
	if len(messages) > 0 && messages[0].Role == "system" {
		out := make([]Message, len(messages))
		copy(out, messages)
		out[0] = Message{Role: "system", Content: prompt + "\n\n" + messages[0].Content}
		return out
	}
	out := make([]Message, 0, len(messages)+1)
	out = append(out, Message{Role: "system", Content: prompt})
	out = append(out, messages...)
	return out
}

// Alive does a quick GET /api/tags and reports reachability.
func (p *OllamaProvider) Alive(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+"/api/tags", nil)
	if err != nil {
		return false
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// ──────────────────────────────────────────────────────────────────────────
// ModelRouter
// ──────────────────────────────────────────────────────────────────────────

// ModelRouter picks the right provider for a given task. Construct via
// NewModelRouter at server startup; pass it down to anything that needs
// embeddings or chat. Nil provider slots represent unconfigured tiers.
//
//   ForEmbedding() — always Tier 1. Embeddings never leave the machine.
//   ForRealtime()  — Tier 1.
//   ForNightly()   — Tier 2 if non-nil; otherwise falls back to Tier 1.
//   ForOracle(term) — Tier 3 if configured AND term passes the groundedness
//                     gate; nil otherwise (callers must skip).
//
// Hot-swap safety (added in the no-reboot-LLM-swap work): provider slots
// are guarded by an RWMutex so a `ReplaceTier` from the settings PUT path
// or the admin reload endpoint never races a concurrent `ForNightly()` /
// `ForOracle()`. Callers ALWAYS go through the accessor methods (Tier1,
// Tier2, Tier3) — direct field access is impossible because the slots are
// private. In-flight Chat() calls hold their own reference to the old
// provider, so a swap mid-call doesn't yank the model out from under them.
type ModelRouter struct {
	mu        sync.RWMutex
	tier1     LLMProvider // local realtime; required
	tier2     LLMProvider // local nightly; optional
	tier3     LLMProvider // remote Oracle; optional
	embedTier LLMProvider // dedicated embedding provider; optional (v2.6 Bundle D)
	// LocalConcepts are terms that must NEVER be sent to Tier 3 (project
	// codenames, internal identifiers, etc.). See SD_LOCAL_CONCEPTS.
	// Immutable after construction — no lock needed for reads.
	LocalConcepts map[string]bool
}

// NewModelRouter creates a router. tier1 is required; pass nil for
// missing tier2/tier3. localConcepts may be nil. Embed tier defaults to
// nil (use SetEmbedTier or NewModelRouterWithEmbed during boot to
// configure) — when nil, ForEmbedding() falls back to Tier 1, preserving
// the v2.5 behaviour for users who never configure SD_SYNAPSE_MODEL or
// the TierEmbedding settings row.
func NewModelRouter(tier1, tier2, tier3 LLMProvider, localConcepts []string) *ModelRouter {
	concepts := make(map[string]bool, len(localConcepts))
	for _, c := range localConcepts {
		if c != "" {
			concepts[c] = true
		}
	}
	return &ModelRouter{
		tier1:         tier1,
		tier2:         tier2,
		tier3:         tier3,
		LocalConcepts: concepts,
	}
}

// SetEmbedTier installs (or clears, with nil) the dedicated embedding
// provider. Atomic with respect to concurrent Tier{1,2,3}/Embed reads.
// v2.6 Bundle D — used by main.go boot wiring and by hot-swap paths.
func (r *ModelRouter) SetEmbedTier(p LLMProvider) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.embedTier = p
	r.mu.Unlock()
}

// EmbedTier returns the configured embedding provider (may be nil).
func (r *ModelRouter) EmbedTier() LLMProvider {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.embedTier
}

// Tier1 returns the current Tier 1 provider (may be nil if unconfigured).
// Safe to call concurrently with ReplaceTier.
func (r *ModelRouter) Tier1() LLMProvider {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tier1
}

// Tier2 returns the current Tier 2 provider (may be nil).
func (r *ModelRouter) Tier2() LLMProvider {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tier2
}

// Tier3 returns the current Tier 3 provider (may be nil).
func (r *ModelRouter) Tier3() LLMProvider {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tier3
}

// ReplaceTier atomically swaps the provider for the given tier. Returns
// (oldProvider, true) on success or (nil, false) if the tier key isn't
// recognized. The old provider isn't shut down here — callers that started
// a Chat() against it keep their reference and finish normally.
//
// Used by:
//   - PUT /settings/providers/{tier} (auto-reload after DB save)
//   - POST /admin/router/reload     (full re-read from DB)
//
// Hot-swap is the new normal: "restart core to take effect" no longer
// applies to provider changes.
func (r *ModelRouter) ReplaceTier(tier TierKey, p LLMProvider) (LLMProvider, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var old LLMProvider
	switch tier {
	case Tier1:
		old = r.tier1
		r.tier1 = p
	case Tier2:
		old = r.tier2
		r.tier2 = p
	case Tier3:
		old = r.tier3
		r.tier3 = p
	case TierEmbedding:
		// v2.6 Bundle D — hot-swap for the dedicated embed tier. A
		// non-local provider is REJECTED (cosine compatibility for
		// existing rows is only preserved within a single embedding
		// space; pointing the router at a remote embed model would
		// silently break /recall against historical rows).
		if p != nil && !p.IsLocal() {
			return nil, false
		}
		old = r.embedTier
		r.embedTier = p
	default:
		return nil, false
	}
	return old, true
}

// ForEmbedding returns the dedicated embed provider when configured,
// otherwise falls back to Tier 1 (when local). Mixing embedding spaces
// (e.g. Ollama nomic vs OpenAI text-embedding-3-small) silently breaks
// cosine similarity for every row already in the embeddings table, so
// the router refuses to hand out a remote provider for this purpose.
// Callers should treat nil as "embeddings unavailable" (recall +
// synapse builder skip).
//
// v2.6 Bundle D: an explicit `embedTier` takes precedence — when set
// to a local provider (Ollama, openai_local), it's returned regardless
// of Tier 1's state. With no embed tier configured (v2.5 reality), the
// Tier 1 fallback preserves all prior behaviour.
func (r *ModelRouter) ForEmbedding() LLMProvider {
	if et := r.EmbedTier(); et != nil && et.IsLocal() {
		return et
	}
	t1 := r.Tier1()
	if t1 == nil || !t1.IsLocal() {
		return nil
	}
	return t1
}

// ForRealtime always returns Tier 1.
func (r *ModelRouter) ForRealtime() LLMProvider { return r.Tier1() }

// ForNightly returns Tier 2 if available; otherwise Tier 1.
func (r *ModelRouter) ForNightly() LLMProvider {
	if t2 := r.Tier2(); t2 != nil {
		return t2
	}
	return r.Tier1()
}

// ForOracle returns Tier 3 only if (a) Tier 3 is configured, and
// (b) `term` is not in LocalConcepts. Returns nil otherwise. The
// full groundedness gate (see §5.3) is implemented by the caller;
// this is the cheap pre-check.
func (r *ModelRouter) ForOracle(term string) LLMProvider {
	t3 := r.Tier3()
	if t3 == nil {
		return nil
	}
	if r.LocalConcepts[term] {
		return nil
	}
	return t3
}
