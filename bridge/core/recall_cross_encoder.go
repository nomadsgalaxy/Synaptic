// recall_cross_encoder.go — LLM-based cross-encoder reranker.
//
// Background (2026-05-15): single-vector cosine retrieval misses on
// semantically-distant phrasings (query "artist last Friday" ↔ memory
// "bluegrass band features banjo player today"). The fix is a
// cross-encoder rerank step that scores (query, candidate) pairs directly
// instead of comparing pooled embeddings.
//
// Two ways to deploy a cross-encoder:
//   (a) A real ms-marco-MiniLM sidecar — best quality, requires Python
//       container, adds a deployment surface.
//   (b) Use Tier 2 as the cross-encoder — score N candidates per call via
//       structured JSON output. Slower per-pair than (a) but BATCH-aware
//       (all 20 candidates in one LLM call), reuses existing infra,
//       zero new containers.
//
// This file implements (b). When `recall_rerank_kind=llm` and Tier 2 is
// configured, the reranker asks Tier 2 to score each candidate 0.0-1.0
// for relevance to the query. The scored list is sorted descending.
//
// The reranker fails open: on parse error or LLM timeout, returns the
// input candidates unchanged (caller logs and keeps cosine order).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// Circuit-breaker tuning. After this many consecutive rerank failures, skip
// for the cooldown window. Empirically tuned: gpt-4o-mini contended-by-
// nightly produces 100% rerank failures for the full duration of phase=maps
// (often >10 minutes). 3 failures × 8s wasted = 24s burned before the
// breaker trips — small price; cooldown then saves every subsequent /recall
// the same 5-8s wait until contention clears.
const (
	rerankBreakerThreshold = 3
	rerankBreakerCooldown  = 60 * time.Second
)

// LLMReranker implements Reranker via a chat call. Pass it a model
// router so it can find Tier 2; if Tier 2 isn't configured it falls back
// to the input order.
//
// 2026-05-27 — added a simple atomic circuit breaker (consecutiveFailures
// + skipUntilNanos) so sustained Tier 2 contention (e.g. during a nightly
// Phase 4 maps/synthesis burst on gpt-4o-mini) doesn't make EVERY /recall
// pay the full timeout. After N consecutive failures the reranker
// short-circuits for a cooldown window — /recall returns the fused
// cosine order immediately, which is what the post-fail fallback does
// anyway. The breaker auto-resets on first success.
type LLMReranker struct {
	router              *ModelRouter
	consecutiveFailures atomic.Int32
	skipUntilNanos      atomic.Int64
}

// NewLLMReranker constructs the reranker. Returns nil if router is nil.
func NewLLMReranker(router *ModelRouter) *LLMReranker {
	if router == nil {
		return nil
	}
	return &LLMReranker{router: router}
}

// Rerank asks Tier 2 to score each candidate. Returns the candidates
// sorted by LLM-assigned relevance score (descending). Stable on ties.
func (r *LLMReranker) Rerank(ctx context.Context, query string, candidates []rerankCandidate) ([]rerankCandidate, error) {
	if len(candidates) == 0 {
		return candidates, nil
	}
	// Circuit-breaker: if recent calls have been failing, skip the
	// chat round-trip entirely until the cooldown expires. The fused
	// cosine order (caller's fallback) is good enough — and avoiding
	// the wait is what unwedges /recall during nightly contention.
	if skipUntil := r.skipUntilNanos.Load(); skipUntil > 0 && time.Now().UnixNano() < skipUntil {
		return candidates, fmt.Errorf("rerank skipped: circuit breaker open (consecutive failures %d)", r.consecutiveFailures.Load())
	}
	provider := r.router.ForNightly()
	if provider == nil {
		return candidates, nil
	}

	prompt := buildCrossEncoderPrompt(query, candidates)

	// Timeout + output-token budget scale with candidate count. Each
	// scored entry in the JSON output is roughly "id":n.n, ~12-16 tokens
	// per candidate. For 20 candidates 800 tokens is plenty; for 100
	// candidates we need ~1500 to avoid truncation. Truncated JSON fails
	// parsing and the rerank silently falls back to cosine order — the
	// exact failure mode that showed up on a 3955-memory bank where
	// rerankTopK scaled to 100.
	maxTokens := 800
	if want := 50 + len(candidates)*20; want > maxTokens {
		maxTokens = want
	}
	// 2026-05-27 — reranker timeout used to be 30s (small) / 60s (large).
	// In production with a nightly run hammering Tier 2 (Phase 4 maps +
	// synthesis on gpt-4o-mini), OpenAI throttled the rerank chat call so
	// hard that every /recall blocked the full 30–60s and the MCP client
	// (mcpo default 30s) saw HTTP 0. The reranker is a quality-boost on
	// top of a perfectly-usable cosine fallback — under contention it's
	// better to skip it than to keep callers waiting. New defaults:
	//   - small (≤50 candidates): 8s
	//   - large  (>50 candidates): 12s
	// AND we clip to the parent context's deadline so we never outlive
	// what the client is willing to wait for. Both the per-call budget
	// and the rerank floor stay tunable via recall_rerank_timeout_ms
	// (future setting; not wired today). When time is too short to make
	// a Tier 2 round-trip worthwhile, skip the call entirely.
	timeout := 5 * time.Second
	if len(candidates) > 50 {
		timeout = 8 * time.Second
	}
	if dl, ok := ctx.Deadline(); ok {
		// Leave at least 500ms for the parent to write the fallback
		// response after the reranker bails — that's the minimum cushion
		// recall.go needs to fall through to fused order and serialize.
		remaining := time.Until(dl) - 500*time.Millisecond
		if remaining < timeout {
			timeout = remaining
		}
		if timeout < 2*time.Second {
			// Parent context has too little headroom for a meaningful
			// rerank round-trip. Skip the call so /recall returns the
			// fused order immediately rather than burning the budget
			// on a request that will time out anyway.
			return candidates, fmt.Errorf("rerank skipped: parent ctx has %s, need ≥2s", remaining.Round(time.Millisecond))
		}
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out, err := provider.Chat(rctx, []Message{{Role: "user", Content: prompt}}, maxTokens)
	if err != nil {
		r.recordFailure()
		return candidates, fmt.Errorf("rerank chat: %w", err)
	}

	scores, err := parseCrossEncoderResponse(out, candidates)
	if err != nil {
		r.recordFailure()
		return candidates, fmt.Errorf("rerank parse: %w", err)
	}
	// Chat returned + parsed cleanly — Tier 2 is healthy. Reset the
	// circuit breaker so future calls go through immediately.
	r.recordSuccess()

	// Sort candidates by the new score map (desc). Candidates absent from
	// the map fall to the end keeping their input order.
	type scored struct {
		c rerankCandidate
		s float64
	}
	out2 := make([]scored, len(candidates))
	for i, c := range candidates {
		s, ok := scores[c.ID]
		if !ok {
			s = -1.0 // unscored → push to end
		}
		out2[i] = scored{c: c, s: s}
	}
	sort.SliceStable(out2, func(i, j int) bool {
		return out2[i].s > out2[j].s
	})
	// Minimum-score filter. Candidates below the threshold (mention the
	// topic but don't answer the question, per the prompt rubric) are
	// dropped so the synthesis model isn't tempted to grab a related-but-
	// wrong specific from them ($350k from a partial-quote memory when
	// the actual answer $400k lives in a higher-scored candidate).
	//
	// minScore raised 0.4 → 0.6 (2026-05-15): hallucination came from
	// "topic mentioned but not answered" candidates that scored ~0.45 —
	// synthesis bridged them into a confident-but-wrong claim. Raising
	// the floor culls those without sacrificing genuine second-event
	// matches (which typically score 0.6-0.8).
	//
	// minKeep is DYNAMIC: scales with the input candidate pool so the
	// rerank's filler floor stays a constant fraction of the visibility
	// window. With 20 candidates we keep 8 (40%); with 60 candidates
	// we keep 24; with 100 we keep 40. The proportion (~40%) is what
	// previously made small-bank rerank work — multi-event questions
	// need both anchors, comparative questions need date-witnessing
	// candidates, etc. — and we want the SAME guarantee at larger
	// scales where cosine puts the right candidate at rank 40 rather
	// than rank 8.
	const minScore = 0.6
	minKeep := len(out2) * 2 / 5 // 40%
	if minKeep < 8 {
		minKeep = 8 // absolute floor for tiny banks
	}
	if minKeep > len(out2) {
		minKeep = len(out2)
	}
	kept := out2[:0]
	for _, s := range out2 {
		if s.s >= minScore || len(kept) < minKeep {
			kept = append(kept, s)
		}
	}
	result := make([]rerankCandidate, len(kept))
	for i, s := range kept {
		result[i] = s.c
	}
	return result, nil
}

// recordFailure bumps the consecutive-failure count and opens the
// circuit breaker once the threshold is crossed. Idempotent for the
// "still in cooldown" case — repeatedly setting skipUntil to a future
// instant just extends nothing because we only set it on the threshold-
// crossing call.
func (r *LLMReranker) recordFailure() {
	n := r.consecutiveFailures.Add(1)
	if n >= rerankBreakerThreshold {
		r.skipUntilNanos.Store(time.Now().Add(rerankBreakerCooldown).UnixNano())
	}
}

// recordSuccess resets the breaker so the next call goes through.
func (r *LLMReranker) recordSuccess() {
	r.consecutiveFailures.Store(0)
	r.skipUntilNanos.Store(0)
}

// buildCrossEncoderPrompt frames the relevance-scoring ask. Output is a
// JSON map {id: score}; the parser is forgiving when the LLM wraps it in
// prose or code fences.
func buildCrossEncoderPrompt(query string, candidates []rerankCandidate) string {
	var b strings.Builder
	b.Grow(200 + 80*len(candidates))
	b.WriteString(`You are scoring how directly each memory below answers the user's question. Score each on a 0.0-1.0 scale where:
  1.0 = memory contains the EXACT fact needed to answer the question
  0.7 = memory contains a closely related fact (same entity / event / person)
  0.4 = memory mentions the topic but doesn't answer the specific question
  0.0 = memory is unrelated to the question

Be selective. The cosine retrieval that produced these candidates is keyword-based and noisy — your job is to recognize semantic match when the words don't overlap (e.g. "artist" matches "bluegrass band", "trip" matches "vacation", "meeting" matches "meetup").

QUESTION:
  `)
	b.WriteString(strings.TrimSpace(query))
	b.WriteString("\n\nCANDIDATES:\n")
	for _, c := range candidates {
		text := c.Text
		if len(text) > 400 {
			text = text[:400] + "…"
		}
		b.WriteString("  [")
		b.WriteString(c.ID)
		b.WriteString("] ")
		b.WriteString(text)
		b.WriteString("\n")
	}
	b.WriteString(`
Return ONLY a JSON object mapping each candidate ID to its score (no preamble, no code fences):
{"`)
	b.WriteString(candidates[0].ID)
	b.WriteString(`": 0.85, "`)
	if len(candidates) > 1 {
		b.WriteString(candidates[1].ID)
	} else {
		b.WriteString("...")
	}
	b.WriteString(`": 0.20, ...}`)
	return b.String()
}

// parseCrossEncoderResponse extracts the JSON {id: score} map from the
// LLM's reply, tolerating leading explanation, code fences, or trailing
// commentary. Returns an error only if no parseable object is found.
func parseCrossEncoderResponse(raw string, candidates []rerankCandidate) (map[string]float64, error) {
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no JSON object found in response")
	}
	chunk := raw[start : end+1]

	// First pass: assume well-formed.
	var scores map[string]json.Number
	if err := json.Unmarshal([]byte(chunk), &scores); err == nil {
		out := make(map[string]float64, len(scores))
		for id, n := range scores {
			f, ferr := n.Float64()
			if ferr == nil {
				out[id] = f
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}

	// Fallback: scan for "ID": NUMBER pairs.
	out := make(map[string]float64)
	for _, c := range candidates {
		key := "\"" + c.ID + "\""
		idx := strings.Index(chunk, key)
		if idx < 0 {
			continue
		}
		// Skip the key + ":"
		rest := chunk[idx+len(key):]
		colon := strings.Index(rest, ":")
		if colon < 0 {
			continue
		}
		rest = rest[colon+1:]
		// Read up to ',' or '}'
		stopIdx := strings.IndexAny(rest, ",}")
		if stopIdx < 0 {
			continue
		}
		numStr := strings.TrimSpace(rest[:stopIdx])
		f, ferr := json.Number(numStr).Float64()
		if ferr == nil {
			out[c.ID] = f
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no scorable pairs found")
	}
	return out, nil
}
