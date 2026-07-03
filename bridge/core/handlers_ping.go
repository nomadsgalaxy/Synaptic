// handlers_ping.go — `GET /settings/providers/{tier}/ping` connection check.
//
// Reports whether the configured provider for a tier is reachable AND that
// the API key (if any) is currently accepted, so the dashboard can render
// the green/red/hollow status dot.
//
// Design choices:
//
//   - **List-models / tags ONLY.** Never chat completions. Pings happen on
//     every dashboard render; using a billable endpoint would silently spend
//     the user's quota. We verify this with `TestProviderPing_NoTokensConsumed`.
//
//   - **Aggressive 4s timeout.** A red dot is more useful than a 30s spinner.
//     If the upstream is just slow, the dashboard can re-fetch in 30s.
//
//   - **30s cache keyed by (tier, kind, base_url, api_key_env).** The
//     dashboard re-renders the providers panel on every Atlas open; without
//     caching, opening the panel five times in a minute would five-times-bill
//     OpenAI's models endpoint. Cache TTL is short enough that "I just rotated
//     the key" feedback isn't noticeably stale (one cache miss → fresh probe).
//
//   - **Singleflight dedup.** Two parallel pings for the same key share one
//     upstream call; followers wait on a `done` channel and inherit the
//     result. Prevents the "user clicks open three times rapidly" thundering
//     herd against a flaky API.
//
//   - **Read api_key_env via os.Getenv at probe time, not from settings.**
//     The whole point is verifying what the running process actually has —
//     the post-Save-pre-restart state must produce a clear "restart needed"
//     error so the user knows the value isn't loaded yet.
//
//   - **Never log key values.** Log key NAME + length + result code only.
//     Verified by `TestProviderPing_NoKeyInLogs`.
//
//   - **No audit log writes.** Pings are read-only, high-frequency. Logging
//     each one would bloat audit_log and provide no investigative value.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// pingResult is the JSON response shape. Fields tolerate both
// `{"ok":true}` (handoff contract) and absent error/model when not relevant.
type pingResult struct {
	OK     bool   `json:"ok"`
	Kind   string `json:"kind"`
	Model  string `json:"model,omitempty"`
	Error  string `json:"error,omitempty"`
	Cached bool   `json:"cached,omitempty"`
}

// ──────────────────────────────────────────────────────────────────────────
// Cache + singleflight
// ──────────────────────────────────────────────────────────────────────────

const pingCacheTTL = 30 * time.Second

// pingHTTPTimeout is the per-probe ceiling. Slightly larger than the handoff's
// 3-5s suggestion to give TCP/TLS handshake some slack on slow networks.
const pingHTTPTimeout = 5 * time.Second

type providerPingCache struct {
	mu       sync.Mutex
	entries  map[string]cachedPing
	inFlight map[string]*pingFlight
}

type cachedPing struct {
	result pingResult
	at     time.Time
}

type pingFlight struct {
	done   chan struct{}
	result pingResult
}

func newProviderPingCache() *providerPingCache {
	return &providerPingCache{
		entries:  map[string]cachedPing{},
		inFlight: map[string]*pingFlight{},
	}
}

// do runs `fn` if no recent cached result exists and no other caller is
// already running it. Concurrent callers for the same key share one probe.
// Cache hits set result.Cached=true; followers inherit the leader's result
// without the cached flag (their probe was as fresh as it could be).
func (c *providerPingCache) do(key string, fn func() pingResult) pingResult {
	c.mu.Lock()
	if entry, ok := c.entries[key]; ok && time.Since(entry.at) < pingCacheTTL {
		r := entry.result
		r.Cached = true
		c.mu.Unlock()
		return r
	}
	if f, ok := c.inFlight[key]; ok {
		c.mu.Unlock()
		<-f.done
		return f.result
	}
	f := &pingFlight{done: make(chan struct{})}
	c.inFlight[key] = f
	c.mu.Unlock()

	result := fn()

	c.mu.Lock()
	f.result = result
	c.entries[key] = cachedPing{result: result, at: time.Now()}
	delete(c.inFlight, key)
	c.mu.Unlock()
	close(f.done)

	return result
}

// ensurePingCache lazily initialises the per-server cache. Avoids forcing
// every test that constructs `&Server{bank: bank}` to add the field.
func (s *Server) ensurePingCache() *providerPingCache {
	s.pingCacheOnce.Do(func() {
		s.pingCache = newProviderPingCache()
	})
	return s.pingCache
}

// pingCacheKey is what the cache + dedup map is keyed on. Note we DO NOT
// include the resolved API key value — the env var name is what matters
// for cache validity. If the user changes the env var value, they need
// to restart sd-core anyway, which clears the cache.
func pingCacheKey(tier TierKey, cfg ProviderConfig) string {
	return string(tier) + "|" + string(cfg.Kind) + "|" + cfg.BaseURL + "|" + cfg.APIKeyEnv
}

// ──────────────────────────────────────────────────────────────────────────
// HTTP handler
// ──────────────────────────────────────────────────────────────────────────

func (s *Server) settingsProvidersPing(w http.ResponseWriter, r *http.Request, tierStr string) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET", "OPTIONS")
		return
	}

	tier, ok := parseTierKey(tierStr)
	if !ok {
		http.Error(w,
			"unknown tier; expected one of: "+strings.Join(allTierStrings(), ", "),
			http.StatusBadRequest)
		return
	}

	cfg, err := s.bank.GetProviderConfig(tier)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	key := pingCacheKey(tier, cfg)
	result := s.ensurePingCache().do(key, func() pingResult {
		return probeProvider(r.Context(), cfg)
	})
	writeJSON(w, http.StatusOK, result)
}

// ──────────────────────────────────────────────────────────────────────────
// Probe
// ──────────────────────────────────────────────────────────────────────────

// probeProvider runs a single connection check against the configured tier.
// Returns a pingResult with HTTP-200 semantics — even on probe failure the
// outer handler still returns 200 with `ok:false`. The dashboard uses
// `r.ok === true` to decide green vs red.
func probeProvider(ctx context.Context, cfg ProviderConfig) pingResult {
	kind := string(cfg.Kind)

	// Disabled tier — short-circuit, no network call.
	if cfg.Kind == ProviderDisabled {
		return pingResult{Kind: "disabled", OK: false, Error: "disabled"}
	}

	// Resolve base URL.
	baseURL := cfg.BaseURL
	if cfg.Kind == ProviderOllama && baseURL == "" {
		baseURL = envOr("SD_OLLAMA_URL", "http://localhost:11434")
	}

	// Empty api_key_env on openai/anthropic — short-circuit. Custom is
	// allowed to ping without auth (self-hosted vLLM etc.) so we don't
	// short-circuit for kind=custom; the actual probe just omits the
	// auth header.
	switch cfg.Kind {
	case ProviderOpenAI, ProviderAnthropic:
		if cfg.APIKeyEnv == "" {
			return pingResult{Kind: kind, Model: cfg.Model, OK: false,
				Error: "no api_key_env configured"}
		}
	}

	// Resolve API key from env (if any). This is the critical
	// post-Save-pre-restart check — the env var name might be set in the
	// settings row but the running process doesn't see the value yet.
	var apiKey string
	if cfg.APIKeyEnv != "" {
		apiKey = os.Getenv(cfg.APIKeyEnv)
		if apiKey == "" && cfg.Kind != ProviderOllama && cfg.Kind != ProviderCustom {
			return pingResult{
				Kind: kind, Model: cfg.Model, OK: false,
				Error: fmt.Sprintf("env var %s not loaded — restart sd-core", cfg.APIKeyEnv),
			}
		}
	}

	// Build the probe URL + headers.
	var url string
	headers := map[string]string{}
	switch cfg.Kind {
	case ProviderOllama:
		url = strings.TrimRight(baseURL, "/") + "/api/tags"
	case ProviderOpenAI:
		if baseURL == "" {
			return pingResult{Kind: kind, Model: cfg.Model, OK: false,
				Error: "no base_url configured"}
		}
		url = strings.TrimRight(baseURL, "/") + "/models"
		headers["Authorization"] = "Bearer " + apiKey
	case ProviderAnthropic:
		if baseURL == "" {
			return pingResult{Kind: kind, Model: cfg.Model, OK: false,
				Error: "no base_url configured"}
		}
		// Anthropic models endpoint is /v1/models. We assume base_url already
		// includes the /v1 segment (matches our preset and our chat code's
		// `{base_url}/chat/completions` convention).
		url = strings.TrimRight(baseURL, "/") + "/models"
		scheme := cfg.AuthScheme
		if scheme == "" {
			scheme = "x-api-key"
		}
		if scheme == "x-api-key" {
			headers["x-api-key"] = apiKey
		} else {
			headers["Authorization"] = "Bearer " + apiKey
		}
		headers["anthropic-version"] = "2023-06-01"
	case ProviderCustom:
		if baseURL == "" {
			return pingResult{Kind: kind, Model: cfg.Model, OK: false,
				Error: "no base_url configured"}
		}
		url = strings.TrimRight(baseURL, "/") + "/models"
		// Auth scheme honoured only when an API key is present; empty key
		// = no auth header (self-hosted vLLM convention).
		if apiKey != "" {
			scheme := cfg.AuthScheme
			if scheme == "" || scheme == "bearer" {
				headers["Authorization"] = "Bearer " + apiKey
			} else if scheme == "x-api-key" {
				headers["x-api-key"] = apiKey
			}
		}
	case ProviderAirLLM:
		// AirLLM sidecar exposes /healthz returning
		//   {"status":"ok","loaded":<bool>}
		// `loaded:false` means the sidecar is up but the model isn't
		// resident yet (first start can take 5-30 min for 70B weights).
		// Don't auth — the sidecar is intended for localhost / compose-
		// network use only; the existing `apiKey` (if any, from
		// SD_TIER*_API_KEY) is allowed-empty and ignored.
		if baseURL == "" {
			return pingResult{Kind: kind, Model: cfg.Model, OK: false,
				Error: "no base_url configured"}
		}
		// Hand off to AirLLMProvider.Ping which already encodes the
		// loaded-status check and produces useful error strings (vs the
		// generic 200/non-200 fallback below). Short-circuit return.
		probeCtx, cancel := context.WithTimeout(ctx, pingHTTPTimeout)
		defer cancel()
		probe := &AirLLMProvider{BaseURL: strings.TrimRight(baseURL, "/"), Model: cfg.Model}
		if err := probe.Ping(probeCtx); err != nil {
			log.Printf("ping: kind=%s base_url=%s unreachable_or_unhealthy: %v",
				kind, baseURL, err)
			return pingResult{Kind: kind, Model: cfg.Model, OK: false, Error: err.Error()}
		}
		log.Printf("ping: kind=%s base_url=%s ok (model loaded)", kind, baseURL)
		return pingResult{Kind: kind, Model: cfg.Model, OK: true}
	default:
		return pingResult{Kind: kind, OK: false,
			Error: fmt.Sprintf("unknown provider kind: %q", kind)}
	}

	// Make the request with an aggressive timeout.
	probeCtx, cancel := context.WithTimeout(ctx, pingHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
	if err != nil {
		return pingResult{Kind: kind, Model: cfg.Model, OK: false, Error: err.Error()}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: pingHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		// Log result + key NAME, never the value.
		log.Printf("ping: tier=%s kind=%s key_env=%s key_len=%d unreachable: %v",
			"-", kind, cfg.APIKeyEnv, len(apiKey), err)
		return pingResult{Kind: kind, Model: cfg.Model, OK: false,
			Error: "unreachable: " + err.Error()}
	}
	defer resp.Body.Close()
	// Drain so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode == http.StatusOK {
		log.Printf("ping: kind=%s key_env=%s key_len=%d ok",
			kind, cfg.APIKeyEnv, len(apiKey))
		return pingResult{Kind: kind, Model: cfg.Model, OK: true}
	}

	// Specific 401 handling — point the user at the env var.
	errMsg := fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	if resp.StatusCode == http.StatusUnauthorized && cfg.APIKeyEnv != "" {
		errMsg = fmt.Sprintf("%d unauthorized — check %s", resp.StatusCode, cfg.APIKeyEnv)
	}
	log.Printf("ping: kind=%s key_env=%s key_len=%d status=%d",
		kind, cfg.APIKeyEnv, len(apiKey), resp.StatusCode)
	return pingResult{Kind: kind, Model: cfg.Model, OK: false, Error: errMsg}
}

// pingResultJSON is exported for tests that want to assert on the wire shape.
func pingResultJSON(r pingResult) string {
	b, _ := json.Marshal(r)
	return string(b)
}
