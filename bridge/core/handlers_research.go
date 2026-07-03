// handlers_research.go — POST /research (user-triggered Oracle research).
//
// Contract (per the 2026-05-09 frontend handoff):
//
//   POST /research
//     Body:    { "query": "...", "model"?: "...", "max_tokens"?: int, "ttl_hours"?: int }
//     Success: 200 with {entry, sensitive_excluded, local_concept_excluded, cache_hit}
//     400:     empty / >1000-char / malformed query, or topic in SD_LOCAL_CONCEPTS
//     401:     missing bearer (handled by middleware, not here)
//     403:     Tier 3 not configured / SD_DISABLE_ORACLE=1
//     500:     upstream Oracle error
//
// Pipeline:
//
//   1. Cache lookup by sha1(query) — return cached entry if not expired,
//      with cache_hit:true and zero new tokens charged.
//
//   2. Cache miss: pull live (non-deleted) memories as context. Send the
//      whole batch through PrepareOracleCall, which is the SOLE sanctioned
//      egress to Tier 3. It strips sensitive memories, strips memories with
//      LocalConcept tags or LocalConcept text, and redacts LocalConcept
//      terms from the prompt. The returned `safe.Memories` is what we send
//      to the model; `safe.SensitiveExcluded` + `safe.LocalConceptExcluded`
//      are surfaced in the response so the dashboard can show "filtered N
//      sensitive, M local-concept".
//
//   3. Build a prompt that combines the (filtered) query with the (filtered)
//      memory context. Hand to the Tier 3 provider's Chat method.
//
//   4. Save to research_cache. The natural key is the query hash so a
//      duplicate POST hits the cache. Charge token_budget. Append audit_log
//      with redacted after_json (no full content, no API key).
//
// Defense-in-depth notes:
//
//   - Per the sensitive-data invariant, we never call provider.Chat with
//     unfiltered memory text. Even though the user clicked Research, we
//     still filter — sensitive=true is sticky-on regardless of intent.
//
//   - We log only counts and the query hash, never the query body and never
//     the response content (Oracle responses can echo back parts of the
//     prompt — logging them risks leaking redacted-but-still-suspicious
//     fragments).
package main

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// researchEntry is the rich shape returned in the response. Stored as the
// research_cache row's `payload` JSON field.
type researchEntry struct {
	ID          string           `json:"id"`
	Query       string           `json:"query"`
	Content     string           `json:"content"`
	Sources     []researchSource `json:"sources,omitempty"`
	CachedAt    string           `json:"cached_at"`
	ExpiresAt   string           `json:"expires_at"`
	TokensIn    int              `json:"tokens_in"`
	TokensOut   int              `json:"tokens_out"`
	TokensTotal int              `json:"tokens_total"`
	Model       string           `json:"model"`
}

// researchSource is a citation / link the Oracle returned. Empty for
// providers that don't support citations natively (most chat-completions APIs).
type researchSource struct {
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
}

// researchRequest is the inbound POST body. Optional fields are tolerated
// gracefully — frontend may add them in future iterations.
type researchRequest struct {
	Query     string `json:"query"`
	Model     string `json:"model,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
	TTLHours  int    `json:"ttl_hours,omitempty"`
}

// handleResearchPost is the user-facing handler.
func (s *Server) handleResearchPost(w http.ResponseWriter, r *http.Request) {
	var body researchRequest
	if err := readJSON(r, &body); err != nil {
		http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
		return
	}
	query := strings.TrimSpace(body.Query)
	if query == "" {
		http.Error(w, "query is required", http.StatusBadRequest)
		return
	}
	if len(query) > 1000 {
		http.Error(w, "query too long (max 1000 chars)", http.StatusBadRequest)
		return
	}

	// Pre-check: tell the user we can't proceed before we touch the cache.
	// PrepareOracleCall would also catch this, but a 403 here is more
	// targeted (the dashboard maps it to the "configure Tier 3" prompt).
	if s.router == nil || s.router.Tier3() == nil {
		http.Error(w, "Tier 3 / Oracle is not configured", http.StatusForbidden)
		return
	}

	id := researchID(query)

	// 1. Cache lookup. A hit short-circuits the spend.
	if cached, err := s.bank.GetResearchCache(id); err == nil && !researchCacheExpired(cached) {
		entry, ok := decodeResearchEntry(cached)
		if ok {
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"entry":                  entry,
				"sensitive_excluded":     0,
				"local_concept_excluded": 0,
				"cache_hit":              true,
			})
			return
		}
		// If the cached row's payload is unparseable, fall through and
		// re-run the call. (Could happen if a future schema change lands
		// new fields and old rows lack them.)
	}

	// 2. Pull live memories as context. Cap at 50 most-recent so the prompt
	//    stays a reasonable size on any chat model.
	memList, err := s.bank.ListMemoriesWith(MemoryListOpts{Limit: 50})
	if err != nil {
		http.Error(w, "load memories: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 3. Route through the egress chokepoint. This is the sensitive-data
	//    invariant — never reach Tier 3 without this filter.
	safe, err := s.router.PrepareOracleCall(OracleRequest{
		Topic:    query,
		Memories: memList,
		Prompt:   query,
		Reason:   "user-triggered research",
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrOracleDisabled):
			http.Error(w, err.Error(), http.StatusForbidden)
		case errors.Is(err, ErrOracleTopicLocal):
			// The query itself is in SD_LOCAL_CONCEPTS — refuse with 400 so
			// the dashboard surfaces it as a query problem rather than an
			// "Oracle disabled" config issue.
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, ErrOracleAllRedacted):
			// Nothing safe to send. Vanishingly unlikely for a research
			// query (the prompt itself is non-empty), but handle anyway.
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			http.Error(w, "oracle prep: "+err.Error(), http.StatusInternalServerError)
		}
		return
	}

	// 4. Build the final prompt: the user's query plus the filtered memory
	//    context. We deliberately use safe.Memories (not memList) and
	//    safe.Prompt (not the raw query) — those are the redacted versions.
	finalPrompt := buildResearchPrompt(safe.Prompt, safe.Memories)

	// 5. Call Tier 3.
	maxTokens := body.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 1024
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	startedAt := time.Now()
	content, err := safe.Provider.Chat(ctx, []Message{
		{Role: "user", Content: finalPrompt},
	}, maxTokens)
	if err != nil {
		http.Error(w, "oracle: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 6. Estimate tokens. Real provider responses sometimes carry usage
	//    fields, but RemoteProvider.Chat doesn't currently surface them, so
	//    we approximate via length heuristic. Better to under-report than
	//    block the budget on a missing usage field.
	tokensIn := estimateTokens(finalPrompt)
	tokensOut := estimateTokens(content)
	tokensTotal := tokensIn + tokensOut

	// 7. Build + cache the entry.
	now := time.Now().UTC()
	ttlHours := body.TTLHours
	if ttlHours <= 0 {
		ttlHours = 24 * 7
	}
	expires := now.Add(time.Duration(ttlHours) * time.Hour)

	entry := researchEntry{
		ID:          id,
		Query:       query,
		Content:     content,
		Sources:     nil, // chat-completions providers don't return citations natively
		CachedAt:    now.Format(time.RFC3339Nano),
		ExpiresAt:   expires.Format(time.RFC3339Nano),
		TokensIn:    tokensIn,
		TokensOut:   tokensOut,
		TokensTotal: tokensTotal,
		Model:       safe.Provider.Name(),
	}
	payload, _ := json.Marshal(entry)
	if err := s.bank.SaveResearchCache(ResearchCacheEntry{
		Topic:     id,
		Payload:   string(payload),
		FetchedAt: entry.CachedAt,
		ExpiresAt: entry.ExpiresAt,
		FetchedBy: "manual", // POST /research is always user-triggered
	}); err != nil {
		// Non-fatal — return the result even if caching failed.
		log.Printf("research: cache save failed: %v", err)
	}

	// 8. Charge token budget with full per-tier attribution. Tier 3 (Oracle)
	//    is where the user's "show me what cost real money" headline comes
	//    from — providerKind drives the external/internal split that the
	//    dashboard's chip uses for its primary readout.
	today := now.Format("2006-01-02")
	providerKind := providerKindFromName(safe.Provider.Name())
	if err := s.bank.AddTokensUsedV2(today, string(Tier3), providerKind, safe.Provider.Name(),
		!safe.Provider.IsLocal(), tokensIn, tokensOut); err != nil {
		log.Printf("research: budget update failed: %v", err)
	}

	// 9. Audit log — redacted. Never the full content, never the prompt body.
	auditAfter, _ := json.Marshal(map[string]interface{}{
		"id":                     id,
		"query_len":              len(query),
		"sensitive_excluded":     safe.SensitiveExcluded,
		"local_concept_excluded": safe.LocalConceptExcluded,
		"tokens_in":              tokensIn,
		"tokens_out":             tokensOut,
		"tokens_total":           tokensTotal,
		"model":                  entry.Model,
		"elapsed_ms":             time.Since(startedAt).Milliseconds(),
	})
	s.auditWrite(AuditEntry{
		Operation:  "oracle_research",
		EntityType: "research_cache",
		EntityID:   id,
		AfterJSON:  string(auditAfter),
		AdapterID:  adapterIDFromRequest(r),
		Reason:     "user-triggered research",
	})

	// 10. Log only the safe summary. No query text, no content.
	log.Printf("research: id=%s sensitive_excluded=%d local_concept_excluded=%d tokens=%d model=%s elapsed=%s",
		id, safe.SensitiveExcluded, safe.LocalConceptExcluded, tokensTotal,
		entry.Model, time.Since(startedAt).Round(time.Millisecond))

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"entry":                  entry,
		"sensitive_excluded":     safe.SensitiveExcluded,
		"local_concept_excluded": safe.LocalConceptExcluded,
		"cache_hit":              false,
	})
}

// researchID is a deterministic short hash of the normalised query so a
// repeat POST of the same question hits the cache. We trim + lowercase for
// stability — minor wording differences ("foo" vs "Foo") still cache-hit.
func researchID(query string) string {
	norm := strings.TrimSpace(strings.ToLower(query))
	h := sha1.Sum([]byte(norm))
	return "research-" + fmt.Sprintf("%x", h[:8])
}

// researchCacheExpired returns true when the cached row's ExpiresAt is in
// the past (or unparseable, treated as expired).
func researchCacheExpired(e ResearchCacheEntry) bool {
	if e.ExpiresAt == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339Nano, e.ExpiresAt)
	if err != nil {
		t, err = time.Parse(time.RFC3339, e.ExpiresAt)
		if err != nil {
			return true
		}
	}
	return time.Now().After(t)
}

// decodeResearchEntry deserialises the JSON payload back into the rich
// shape. Returns ok=false if the row was created before this schema and
// therefore lacks the expected fields.
func decodeResearchEntry(row ResearchCacheEntry) (researchEntry, bool) {
	if row.Payload == "" {
		return researchEntry{}, false
	}
	var e researchEntry
	if err := json.Unmarshal([]byte(row.Payload), &e); err != nil {
		return researchEntry{}, false
	}
	if e.Query == "" || e.Content == "" {
		return researchEntry{}, false
	}
	// Refresh the timestamp fields from the row's columns in case the
	// payload's serialised values drifted (defensive).
	if e.CachedAt == "" {
		e.CachedAt = row.FetchedAt
	}
	if e.ExpiresAt == "" {
		e.ExpiresAt = row.ExpiresAt
	}
	return e, true
}

// buildResearchPrompt composes the final Oracle prompt from the redacted
// query + filtered memory list. Memory text is included verbatim because
// PrepareOracleCall has already vetted it.
func buildResearchPrompt(query string, memories []MemoryRecord) string {
	if len(memories) == 0 {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 64*len(memories))
	b.WriteString("Research the following query, drawing on the user's memory context where it is genuinely relevant. Be concise, factual, and cite uncertainty.\n\n")
	b.WriteString("QUERY:\n")
	b.WriteString(query)
	b.WriteString("\n\n")
	b.WriteString("USER'S MEMORY CONTEXT (most recent first; sensitive entries already excluded):\n")
	for _, m := range memories {
		text := m.EnrichedText
		if text == "" {
			text = m.Text
		}
		b.WriteString("- ")
		b.WriteString(text)
		b.WriteString("\n")
	}
	return b.String()
}

// estimateTokens is a fallback when the provider doesn't surface a usage
// field. ~4 chars per token is the standard rule of thumb for English text
// in BPE/SentencePiece tokenisers; close enough for budget tracking.
func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	n := len(text) / 4
	if n < 1 {
		return 1
	}
	return n
}
