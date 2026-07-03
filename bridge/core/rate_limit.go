// rate_limit.go — token-bucket rate limiter for memory-write endpoints.
//
// Why: bulk-import / mass-MCP-push scenarios can peg local Ollama Tier 1
// because every write triggers sensitive-classification + synapse builder
// + embedding queue. A configurable limiter gives the user a brake without
// the cost of editing code.
//
// Scope: applied to memory-write paths only — `/event` (memory_added) and
// `/bank/memory`. Reads, recall, dashboard, settings, admin endpoints are
// NOT rate-limited.
//
// Default: unlimited (limiter is a no-op when the bucket is uncapped).
// User flips a Setting → Performance → "Memory write rate limit (per min)"
// to a positive integer; that gets read at request time so changes apply
// without restart.
//
// Behaviour on overflow:
//   - 429 with Retry-After: <seconds-until-next-token>
//   - Body: small JSON {error, retry_after_ms, limit_per_min, bucket_size}
//   - Audit log entry per overflow (for diagnostic visibility, not per
//     request — only on the first drop in a window to avoid log spam).
//
// Limiter is a single shared bucket per process. Local single-user
// deployment doesn't need per-IP buckets; if/when Synaptic is multi-tenant
// this gets reshaped.
package main

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// MemoryWriteLimiterSettingKey is the Setting row name. Value is parsed as
// int; 0 / unset / non-numeric all mean "unlimited" (limiter disabled).
const MemoryWriteLimiterSettingKey = "memory_write_rate_limit_per_min"

// MemoryWriteLimiter is a single token-bucket guard for memory writes.
// Tokens refill linearly at limit_per_min / 60 per second; bucket
// capacity equals limit_per_min (so a burst of one minute's worth of
// writes passes without delay, then sustained traffic gates to the rate).
type MemoryWriteLimiter struct {
	bank *Bank

	mu          sync.Mutex
	tokens      float64
	lastRefill  time.Time
	cachedLimit int       // limit per min, 0 = disabled
	cachedAt    time.Time // bank-setting cache TTL
	lastDropLog time.Time // throttle audit logging
}

// NewMemoryWriteLimiter wires the limiter to the bank so it can re-read
// the setting periodically. Bank may be nil in tests; limiter becomes
// a permanent no-op in that case.
func NewMemoryWriteLimiter(bank *Bank) *MemoryWriteLimiter {
	return &MemoryWriteLimiter{bank: bank}
}

// settingTTL controls how often the limiter re-reads the user's setting.
// Short enough that a UI change feels immediate, long enough that we don't
// hammer the bank on every write.
const settingTTL = 2 * time.Second

// readLimit returns the current limit_per_min (cached). 0 = disabled.
func (l *MemoryWriteLimiter) readLimit() int {
	if l == nil || l.bank == nil {
		return 0
	}
	if time.Since(l.cachedAt) < settingTTL {
		return l.cachedLimit
	}
	v, ok, _ := l.bank.GetSetting(MemoryWriteLimiterSettingKey)
	limit := 0
	if ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	l.cachedLimit = limit
	l.cachedAt = time.Now()
	return limit
}

// Acquire attempts to take one token. Returns (true, 0) on success,
// (false, retryAfterMs) on overflow. When the limiter is disabled
// (limit=0), always returns success. Thread-safe.
func (l *MemoryWriteLimiter) Acquire() (ok bool, retryAfterMs int) {
	if l == nil {
		return true, 0
	}
	limit := l.readLimit()
	if limit <= 0 {
		return true, 0 // disabled
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.lastRefill.IsZero() {
		l.lastRefill = now
		l.tokens = float64(limit) // start full so first burst passes
	}
	// Refill: limit tokens per 60 seconds.
	elapsed := now.Sub(l.lastRefill).Seconds()
	l.tokens += elapsed * float64(limit) / 60.0
	if l.tokens > float64(limit) {
		l.tokens = float64(limit)
	}
	l.lastRefill = now
	if l.tokens >= 1 {
		l.tokens -= 1
		return true, 0
	}
	// Need (1 - tokens) more tokens. At limit/60 per sec, that's
	// (1 - tokens) * 60 / limit seconds.
	needed := 1.0 - l.tokens
	secs := needed * 60.0 / float64(limit)
	return false, int(secs*1000) + 1
}

// MemoryWriteRateLimited wraps a memory-write handler with the limiter.
// On overflow, returns 429 with Retry-After header + a structured JSON
// body so clients (MCP, SDK, dashboard) can implement exponential
// backoff cleanly.
func (s *Server) MemoryWriteRateLimited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// OPTIONS / GET / HEAD shouldn't pay the rate-limit tax. The
		// limiter only gates the write path.
		if r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodPatch {
			next(w, r)
			return
		}
		if s.memoryLimiter == nil {
			next(w, r)
			return
		}
		ok, retryAfterMs := s.memoryLimiter.Acquire()
		if ok {
			next(w, r)
			return
		}
		// Throttled.
		retryAfterSec := (retryAfterMs + 999) / 1000
		if retryAfterSec < 1 {
			retryAfterSec = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSec))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		limit := s.memoryLimiter.readLimit()
		fmt.Fprintf(w,
			`{"error":"memory_write_rate_limited","retry_after_ms":%d,"limit_per_min":%d}`,
			retryAfterMs, limit,
		)
		// Throttled audit: at most once per 30s to avoid log spam.
		s.memoryLimiter.mu.Lock()
		emitAudit := time.Since(s.memoryLimiter.lastDropLog) > 30*time.Second
		if emitAudit {
			s.memoryLimiter.lastDropLog = time.Now()
		}
		s.memoryLimiter.mu.Unlock()
		if emitAudit {
			s.auditWrite(AuditEntry{
				Operation:  "memory_write_rate_limited",
				EntityType: "rate_limit",
				EntityID:   r.URL.Path,
				AfterJSON:  fmt.Sprintf(`{"path":%q,"limit_per_min":%d,"retry_after_ms":%d}`, r.URL.Path, limit, retryAfterMs),
				AdapterID:  adapterIDFromRequest(r),
				Reason:     "memory write rate limit exceeded",
			})
		}
	}
}
