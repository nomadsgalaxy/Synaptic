// bundle_b_guards_test.go — guards for v2.6 Bundle B:
//
//   - isRetryableErr classifies real Ollama and OpenAI-shape error strings
//   - withRateLimitRetry retries on 429/502/503, gives up on permanent
//     errors, respects context cancellation, and consumes the per-pipeline
//     RetryBudget when one is attached
//   - OllamaProvider.Embed end-to-end against a fake server that returns
//     429 once then 200 (proves the wrapper is wired correctly)
//
// To keep the suite fast the tests temporarily shrink the global
// retryBackoffSchedule to millisecond-scale waits via a helper that
// saves+restores the slice. This is the only way to exercise the full
// 5-retry path in <1 s without making the backoff schedule itself a
// per-call parameter (which would add API surface for a feature only
// tests care about).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// shrinkBackoff replaces the package-level retryBackoffSchedule with a
// fast schedule for the duration of the test, then restores it.
func shrinkBackoff(t *testing.T) {
	t.Helper()
	orig := retryBackoffSchedule
	retryBackoffSchedule = []time.Duration{
		1 * time.Millisecond,
		2 * time.Millisecond,
		4 * time.Millisecond,
		8 * time.Millisecond,
		16 * time.Millisecond,
	}
	t.Cleanup(func() { retryBackoffSchedule = orig })
}

// ── isRetryableErr classification ────────────────────────────────────────

func TestIsRetryableErr_RealOllamaShape(t *testing.T) {
	// Mirrors `provider.go:222` — `fmt.Errorf("ollama chat %d: %s", status, body)`.
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("ollama chat 200: ok"), false}, // status string only — but no retryable substring
		{errors.New("ollama chat 429: rate limited"), true},
		{errors.New("ollama chat 502: bad gateway"), true},
		{errors.New("ollama chat 503: service unavailable"), true},
		{errors.New("ollama embeddings 429: too many"), true},
		{errors.New("ollama chat 400: bad request"), false},
		{errors.New("ollama chat 500: internal"), false}, // 500 not in retryable set
		{errors.New("remote chat 429 Too Many Requests"), true},
		{errors.New("remote embed 503 Service Unavailable"), true},
		{errors.New(`{"error":{"type":"rate_limit_exceeded"}}`), true}, // rate_limit substring
		{errors.New("connection refused"), false},
		{context.Canceled, false},
		{context.DeadlineExceeded, false},
	}
	for _, tc := range cases {
		var label string
		if tc.err == nil {
			label = "nil"
		} else {
			label = tc.err.Error()
		}
		t.Run(label, func(t *testing.T) {
			if got := isRetryableErr(tc.err); got != tc.want {
				t.Errorf("isRetryableErr(%q) = %v, want %v", label, got, tc.want)
			}
		})
	}
}

// ── withRateLimitRetry behaviour ─────────────────────────────────────────

func TestWithRateLimitRetry_SuccessFirstTry(t *testing.T) {
	shrinkBackoff(t)
	calls := 0
	err := withRateLimitRetry(context.Background(), "test", func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

func TestWithRateLimitRetry_RetriesOn429ThenSucceeds(t *testing.T) {
	shrinkBackoff(t)
	calls := 0
	err := withRateLimitRetry(context.Background(), "test", func() error {
		calls++
		if calls == 1 {
			return errors.New("ollama chat 429: rate limited")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected success on retry, got %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 2 calls (1 + 1 retry), got %d", calls)
	}
}

func TestWithRateLimitRetry_RetriesOn502PatternThenSucceeds(t *testing.T) {
	shrinkBackoff(t)
	calls := 0
	err := withRateLimitRetry(context.Background(), "test", func() error {
		calls++
		if calls <= 4 {
			return errors.New("ollama chat 502: bad gateway")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected success after 4 retries, got %v", err)
	}
	if calls != 5 {
		t.Errorf("expected 5 calls (1 + 4 retries), got %d", calls)
	}
}

func TestWithRateLimitRetry_GivesUpAfterAllRetries(t *testing.T) {
	shrinkBackoff(t)
	calls := 0
	terminal := errors.New("ollama chat 429: forever")
	err := withRateLimitRetry(context.Background(), "test", func() error {
		calls++
		return terminal
	})
	if err != terminal {
		t.Errorf("expected terminal error returned unwrapped, got %v", err)
	}
	want := 1 + len(retryBackoffSchedule)
	if calls != want {
		t.Errorf("expected %d total calls, got %d", want, calls)
	}
}

func TestWithRateLimitRetry_NonRetryablePropagates(t *testing.T) {
	shrinkBackoff(t)
	calls := 0
	permanent := errors.New("ollama chat 400: bad request")
	err := withRateLimitRetry(context.Background(), "test", func() error {
		calls++
		return permanent
	})
	if err != permanent {
		t.Errorf("expected permanent error returned, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call (no retries for 400), got %d", calls)
	}
}

func TestWithRateLimitRetry_RespectsCtxCancel(t *testing.T) {
	shrinkBackoff(t)
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	go func() {
		// Cancel mid-backoff.
		time.Sleep(2 * time.Millisecond)
		cancel()
	}()
	err := withRateLimitRetry(ctx, "test", func() error {
		calls++
		return errors.New("503 Service Unavailable")
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	// Should have called once or twice before the cancel was observed.
	if calls < 1 || calls > 3 {
		t.Errorf("expected 1-3 calls before cancel, got %d", calls)
	}
}

// ── RetryBudget ──────────────────────────────────────────────────────────

func TestRetryBudget_NoBudgetIsUnlimited(t *testing.T) {
	if !tryConsumeRetry(context.Background()) {
		t.Errorf("no-budget ctx should always allow retries")
	}
	if got := RetryBudgetRemaining(context.Background()); got != -1 {
		t.Errorf("remaining without budget should be -1, got %d", got)
	}
}

func TestRetryBudget_DecrementsAndExhausts(t *testing.T) {
	ctx := WithRetryBudget(context.Background(), 2)
	if got := RetryBudgetRemaining(ctx); got != 2 {
		t.Errorf("initial remaining: want 2, got %d", got)
	}
	if !tryConsumeRetry(ctx) {
		t.Errorf("first consume should succeed")
	}
	if !tryConsumeRetry(ctx) {
		t.Errorf("second consume should succeed")
	}
	if tryConsumeRetry(ctx) {
		t.Errorf("third consume should fail (exhausted)")
	}
	if got := RetryBudgetRemaining(ctx); got != 0 {
		t.Errorf("remaining after exhaust: want 0, got %d", got)
	}
}

func TestRetryBudget_ZeroOrNegativeIsNoop(t *testing.T) {
	ctx := WithRetryBudget(context.Background(), 0)
	if !tryConsumeRetry(ctx) {
		t.Errorf("zero max should yield no-budget ctx (unlimited)")
	}
	if got := RetryBudgetRemaining(ctx); got != -1 {
		t.Errorf("zero-max ctx should report -1 remaining, got %d", got)
	}
}

// ── NightlyRetryBudget setting helper ────────────────────────────────────

func TestNightlyRetryBudget_DefaultIsZero(t *testing.T) {
	// Default convention is 0 = Infinite. Absent setting → 0.
	bank := newTestBank(t)
	if got := bank.NightlyRetryBudget(); got != 0 {
		t.Errorf("absent setting should return 0 (Infinite), got %d", got)
	}
	// Nil bank → also 0 (safe).
	var nilBank *Bank
	if got := nilBank.NightlyRetryBudget(); got != 0 {
		t.Errorf("nil bank should return 0, got %d", got)
	}
}

func TestNightlyRetryBudget_PositiveValueRoundtrips(t *testing.T) {
	bank := newTestBank(t)
	if err := bank.SetSetting(NightlyRetryBudgetKey, "20"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if got := bank.NightlyRetryBudget(); got != 20 {
		t.Errorf("expected 20, got %d", got)
	}
}

func TestNightlyRetryBudget_NegativeCoercedToZero(t *testing.T) {
	bank := newTestBank(t)
	// Negatives shouldn't brick the pipeline; coerce silently to 0.
	if err := bank.SetSetting(NightlyRetryBudgetKey, "-5"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if got := bank.NightlyRetryBudget(); got != 0 {
		t.Errorf("negative should coerce to 0, got %d", got)
	}
}

func TestNightlyRetryBudget_UnparseableIsZero(t *testing.T) {
	bank := newTestBank(t)
	if err := bank.SetSetting(NightlyRetryBudgetKey, "not-a-number"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if got := bank.NightlyRetryBudget(); got != 0 {
		t.Errorf("unparseable should coerce to 0, got %d", got)
	}
}

func TestWithRateLimitRetry_ExhaustsBudget(t *testing.T) {
	shrinkBackoff(t)
	ctx := WithRetryBudget(context.Background(), 2)
	// Three independent retryable calls. First two should each consume one
	// slot to retry once and then succeed. Third should run once (no budget
	// left), fail, and return immediately without further attempts.
	makeAttempt := func(attempts *int, succeedAt int) error {
		return withRateLimitRetry(ctx, "test", func() error {
			*attempts++
			if *attempts >= succeedAt {
				return nil
			}
			return errors.New("ollama chat 429: rate limited")
		})
	}

	var a, b, c int
	if err := makeAttempt(&a, 2); err != nil {
		t.Fatalf("first call expected to succeed after 1 retry, got %v", err)
	}
	if a != 2 {
		t.Errorf("first call attempts: want 2, got %d", a)
	}

	if err := makeAttempt(&b, 2); err != nil {
		t.Fatalf("second call expected to succeed after 1 retry, got %v", err)
	}
	if b != 2 {
		t.Errorf("second call attempts: want 2, got %d", b)
	}

	// Budget is now exhausted. Third call should run once, fail retryably,
	// see budget=0, and return the unwrapped error without retrying.
	err := makeAttempt(&c, 2)
	if err == nil {
		t.Fatalf("third call expected to fail (budget exhausted)")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("third call error should be the underlying 429, got %v", err)
	}
	if c != 1 {
		t.Errorf("third call attempts: want 1 (no retries), got %d", c)
	}
}

// ── End-to-end: OllamaProvider.Embed retries via the wrapper ─────────────

// embedFakeServer simulates an Ollama /api/embeddings endpoint that
// returns the configured statuses in order, then returns a real embedding
// for any subsequent request.
type embedFakeServer struct {
	mu       sync.Mutex
	hits     int32
	statuses []int
}

func (f *embedFakeServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		idx := atomic.AddInt32(&f.hits, 1) - 1
		f.mu.Unlock()
		if int(idx) < len(f.statuses) && f.statuses[idx] != 200 {
			w.WriteHeader(f.statuses[idx])
			io.WriteString(w, fmt.Sprintf(`{"error":"status %d"}`, f.statuses[idx]))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"embedding":[0.1,0.2,0.3,0.4]}`)
	}
}

func TestOllamaProvider_Embed_RetriesOn429(t *testing.T) {
	shrinkBackoff(t)
	f := &embedFakeServer{statuses: []int{429, 200}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	p := NewOllamaProvider(srv.URL, "test-model")
	vec, err := p.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if len(vec) != 4 {
		t.Errorf("expected 4-dim vector, got len=%d", len(vec))
	}
	if got := atomic.LoadInt32(&f.hits); got != 2 {
		t.Errorf("expected 2 HTTP hits (429 + 200), got %d", got)
	}
}

func TestOllamaProvider_Embed_FailsAfterTerminal429(t *testing.T) {
	shrinkBackoff(t)
	// 6 retries' worth of 429 (1 initial + 5 retry slots).
	f := &embedFakeServer{statuses: []int{429, 429, 429, 429, 429, 429, 429}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	p := NewOllamaProvider(srv.URL, "test-model")
	_, err := p.Embed(context.Background(), "hello")
	if err == nil {
		t.Fatalf("expected eventual failure, got nil")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error should mention 429, got %v", err)
	}
	want := int32(1 + len(retryBackoffSchedule))
	if got := atomic.LoadInt32(&f.hits); got != want {
		t.Errorf("expected %d HTTP hits, got %d", want, got)
	}
}
