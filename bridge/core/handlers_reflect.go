// handlers_reflect.go — POST /reflect.
//
// Synaptic's architecture is agent-driven: callers (Claude, GPT, your
// own client) retrieve ranked memories via /recall and run their own
// reasoning over the results. /reflect is the thin convenience sibling
// for callers without an agent in the loop — it runs the same retrieval
// pipeline as /recall, then hands the top-K (sensitive-filtered) set to
// Tier 3 with a minimal "answer from these memories" prompt and returns
// the model's response. No classifier, no abstention/contradiction
// detectors — keep that logic in the agent.
//
// Contract:
//
//   POST /reflect
//     Body: {
//       "query":       string,                 // required, <=1000 chars
//       "tags":        []string,               // optional; filter recall set
//       "tags_match":  "any"|"all"|"any_strict"|"all_strict",
//       "budget":      "low"|"mid"|"high",     // maps to top-K + max_tokens
//       "context":     string,                 // optional user-supplied
//                                              // framing for the synthesis
//       "limit":       int,                    // optional override of budget's
//                                              // top-K cap (clamped 1..50)
//     }
//     200: { synthesis, source_memory_ids, tokens_used:{in,out,total},
//            model, sensitive_excluded, local_concept_excluded,
//            recall_count, used_count }
//     400: empty / >1000-char query, query is a LocalConcept, or all
//          candidates were filtered (nothing safe to send)
//     401: missing bearer (handled by middleware)
//     403: Tier 3 not configured / SD_DISABLE_ORACLE=1
//     500: upstream Oracle error
//
// Pipeline:
//
//   1. Embed query and run the canonical retrieval pipeline (cosine +
//      BM25 + RRF fusion + cross-encoder rerank + keyword-rewrite
//      multi-probe). Same path /recall uses — the only difference
//      between /reflect and /recall is that /reflect templates the
//      result into a prompt and calls Tier 3.
//   2. Apply optional tag filter with tags_match semantics:
//        any           — at least one of the requested tags appears
//        all           — every requested tag appears
//        any_strict    — same as "any" but case-sensitive
//        all_strict    — same as "all" but case-sensitive
//   3. Trim to the budget's top-K cap.
//   4. Hand the candidate set to PrepareOracleCall — the canonical egress
//      chokepoint. Sensitive memories and LocalConcept-tagged memories
//      are dropped defensively even though the user explicitly initiated
//      the call (sensitive=true is sticky-on regardless of intent).
//   5. Build a minimal synthesis prompt (memories + question + today).
//   6. Call Tier 3 with the budget-mapped max_tokens.
//   7. Record token spend in token_budget_lines (Tier 3 attribution).
//   8. Audit log with redacted summary (counts, tokens, model — no content).
//   9. Broadcast reflect_done WS event so the dashboard can update.
//
// On budget→model mapping:
//   The acceptance criteria hint at "smaller Tier-3 model for low,
//   premium for high". SD Core currently exposes a single Tier 3 slot
//   (one provider/model at a time, hot-swappable). Per-request model
//   selection would require a multi-slot Tier 3 registry — outside this
//   feature's scope. Until that lands, budget maps to:
//       low  → top 5  memories, 512  max_tokens
//       mid  → top 15 memories, 1024 max_tokens   (default)
//       high → top 30 memories, 2048 max_tokens
//   The active Tier 3 model is returned in the response so the caller
//   can confirm what was used.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ── Types ──────────────────────────────────────────────────────────────────

type reflectRequest struct {
	Query     string   `json:"query"`
	Tags      []string `json:"tags,omitempty"`
	TagsMatch string   `json:"tags_match,omitempty"`
	Budget    string   `json:"budget,omitempty"`
	Context   string   `json:"context,omitempty"`
	Limit     int      `json:"limit,omitempty"`
	// Iter 35 — caller-supplied "today" anchor for relative-date questions
	// ("how many days ago did I meet Emma?"). Format: YYYY-MM-DD. If empty,
	// the prompt falls back to time.Now().UTC().
	AsOfDate string `json:"as_of_date,omitempty"`
	// IncludeSensitive opts the call in to forwarding sensitive memories
	// to Tier 3. Default false: sensitives are filtered server-side per
	// the standing privacy rule. When true, the response surfaces
	// `included_sensitive_count` so the user always sees what crossed.
	// LocalConcept filtering is non-overridable and still runs.
	IncludeSensitive bool `json:"include_sensitive,omitempty"`
	// v2.7 Bundle R — when true, persist the synthesis as a memory with
	// memory_type="reflection", source="reflect" so it surfaces in future
	// /recall calls. Defaults to false: agents that just want a one-shot
	// summary don't bloat the bank. The returned memory_id (when set)
	// lets the caller cross-reference / sd_forget / sd_set_dormant the
	// reflection later.
	SaveAsMemory bool `json:"save_as_memory,omitempty"`
}

type reflectTokensUsed struct {
	In    int `json:"in"`
	Out   int `json:"out"`
	Total int `json:"total"`
}

type reflectResponse struct {
	Synthesis       string            `json:"synthesis"`
	SourceMemoryIDs []string          `json:"source_memory_ids"`
	TokensUsed      reflectTokensUsed `json:"tokens_used"`
	Model           string            `json:"model"`
	// MemoriesExcludedByPrivacyFilter is the canonical user-facing privacy
	// count: union of sensitive-flag exclusions and LocalConcept exclusions.
	// Zero when the caller opted in via IncludeSensitive (sensitives DID
	// reach Tier 3) AND no LocalConcept tags were present.
	MemoriesExcludedByPrivacyFilter int `json:"memories_excluded_by_privacy_filter"`
	// IncludedSensitiveCount is non-zero ONLY when the caller set
	// IncludeSensitive=true AND sensitive memories actually reached the
	// Tier 3 prompt. Surfaces transparently to the user (never silent).
	IncludedSensitiveCount int `json:"included_sensitive_count"`
	// SensitiveExcluded / LocalConceptExcluded are the disaggregated counts
	// retained for diagnostics + audit dashboards.
	SensitiveExcluded    int `json:"sensitive_excluded"`
	LocalConceptExcluded int `json:"local_concept_excluded"`
	RecallCount          int `json:"recall_count"` // candidates after tag filter, before privacy filter
	UsedCount            int `json:"used_count"`   // memories actually forwarded to Tier 3
	// v2.7 Bundle R — id of the persisted reflection memory when
	// SaveAsMemory was true. Empty when save_as_memory was false or
	// when SaveMemory errored (the synthesis is still returned).
	MemoryID string `json:"memory_id,omitempty"`
}

// reflectBudget bundles top-K and max_tokens together so changes stay in one
// place. Defaults to "mid".
type reflectBudget struct {
	TopK      int
	MaxTokens int
	Label     string
}

func reflectBudgetForLabel(label string) reflectBudget {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "low":
		return reflectBudget{TopK: 5, MaxTokens: 512, Label: "low"}
	case "high":
		return reflectBudget{TopK: 30, MaxTokens: 2048, Label: "high"}
	default:
		return reflectBudget{TopK: 15, MaxTokens: 1024, Label: "mid"}
	}
}

// ── HTTP handler ───────────────────────────────────────────────────────────

func (s *Server) handleReflectPost(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST", "OPTIONS")
		return
	}
	if s.bank == nil {
		http.Error(w, "bank disabled — reflect requires SD_BANK_PATH", http.StatusServiceUnavailable)
		return
	}

	var body reflectRequest
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

	// Pre-check Tier 3 before doing retrieval work. Same shape as /research.
	if s.router == nil || s.router.Tier3() == nil {
		http.Error(w, "Tier 3 / Oracle is not configured", http.StatusForbidden)
		return
	}

	budget := reflectBudgetForLabel(body.Budget)
	// Optional explicit override (clamped). Lets the dashboard pass a
	// custom slider value without inventing new budget labels.
	if body.Limit > 0 {
		topK := body.Limit
		if topK > 50 {
			topK = 50
		}
		budget.TopK = topK
	}

	startedAt := time.Now()

	// 1+2. Shared retrieval pipeline — gets the Bundle I improvements
	// (query simplification, BM25 + RRF fusion, temporal awareness,
	// session co-retrieval) for free. Multi-session "total" questions
	// need sibling context from several sessions to see all the
	// trips/markets/sessions in scope, so /reflect expands the top-5
	// sessions (vs the default 3 used by /recall when no expansion is
	// requested). Agents calling /recall directly can pass their own
	// topSessionExpand value.
	scores, err := rankCandidates(r.Context(), s.bank, s.router, query, 5)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// 2a. Cross-encoder rerank — promotes/demotes candidates based on
	//     direct (query, candidate) relevance scoring rather than
	//     embedding-pooled cosine. Visibility (rerankTopK) and the
	//     min-keep floor scale dynamically with bank size so the same
	//     pipeline holds up on 140-memory test banks and 4000+-memory
	//     production banks. Logged for observability.
	rerankKept := 0
	if s.reranker != nil && rerankEnabled(s.bank) && len(scores) > 0 {
		rerankK := rerankTopK(s.bank)
		if rerankK > len(scores) {
			rerankK = len(scores)
		}
		cands := make([]rerankCandidate, 0, rerankK)
		for i := 0; i < rerankK; i++ {
			rec, gerr := s.bank.GetMemory(scores[i].ID)
			if gerr != nil {
				cands = nil
				break
			}
			cands = append(cands, rerankCandidate{
				ID:             scores[i].ID,
				CosineScore:    scores[i].Score,
				Salience:       rec.Salience,
				RecallStrength: rec.RecallStrength,
				Text:           rec.Text,
			})
		}
		if cands != nil {
			reranked, rerr := s.reranker.Rerank(r.Context(), query, cands)
			if rerr != nil {
				log.Printf("reflect: cross-encoder rerank failed, keeping fused order: %v", rerr)
			} else {
				keep := make(map[string]bool, len(reranked))
				for _, rc := range reranked {
					keep[rc.ID] = true
				}
				newScores := make([]Scored, 0, len(scores))
				for _, rc := range reranked {
					var origScore float64
					for _, sc := range scores {
						if sc.ID == rc.ID {
							origScore = sc.Score
							break
						}
					}
					newScores = append(newScores, Scored{ID: rc.ID, Score: origScore})
				}
				for i := rerankK; i < len(scores); i++ {
					if !keep[scores[i].ID] {
						newScores = append(newScores, scores[i])
					}
				}
				log.Printf("reflect: cross-encoder kept %d/%d in top-K (min-score=0.6 floor)",
					len(reranked), rerankK)
				scores = newScores
				rerankKept = len(reranked)
			}
		}
	}

	// 2b. Chronological sort — for "list the order / what came first /
	//     in sequence / before-after" queries, the relevance-ranked list
	//     misleads synthesis. Re-sort the top survivors by MemoryRecord
	//     created_at ascending, which is the actual ingest timestamp.
	//     Memories without created_at sink to the end. This is the ONE
	//     place where retrieval-rank order is deliberately discarded.
	if isChronologicalQuery(query) && len(scores) > 0 {
		sortLimit := budget.TopK * 2
		if sortLimit > len(scores) {
			sortLimit = len(scores)
		}
		createdByID := make(map[string]string, sortLimit)
		for i := 0; i < sortLimit; i++ {
			if rec, gerr := s.bank.GetMemory(scores[i].ID); gerr == nil {
				createdByID[scores[i].ID] = rec.CreatedAt
			}
		}
		head := scores[:sortLimit]
		sort.SliceStable(head, func(i, j int) bool {
			ci, cj := createdByID[head[i].ID], createdByID[head[j].ID]
			if ci == "" && cj == "" {
				return head[i].ID < head[j].ID // fallback: lexicographic ID
			}
			if ci == "" {
				return false
			}
			if cj == "" {
				return true
			}
			return ci < cj
		})
		log.Printf("reflect: chronological sort applied to top-%d (query matched order pattern)", sortLimit)
	}

	// 3. Walk the ranked list, materialising records and applying the tag
	//    filter, until we have budget.TopK survivors. Cap the scan at
	//    10x topK so an extreme tag filter doesn't pull the whole bank.
	scanCap := budget.TopK * 10
	if scanCap < 100 {
		scanCap = 100
	}
	if scanCap > len(scores) {
		scanCap = len(scores)
	}
	candidates := make([]MemoryRecord, 0, budget.TopK)
	for i := 0; i < scanCap && len(candidates) < budget.TopK; i++ {
		rec, err := s.bank.GetMemory(scores[i].ID)
		if err != nil {
			log.Printf("reflect: GetMemory(%s): %v", scores[i].ID, err)
			continue
		}
		if rec.DeletedAt != "" {
			continue
		}
		if !reflectTagsMatch(rec.Tags, body.Tags, body.TagsMatch) {
			continue
		}
		candidates = append(candidates, rec)
	}
	recallCount := len(candidates)

	if recallCount == 0 {
		http.Error(w, "no memories matched the query + tag filter", http.StatusBadRequest)
		return
	}

	// 4. Funnel through the canonical Tier 3 egress chokepoint. Sensitive
	//    records and LocalConcept-tagged records are dropped here.
	safe, err := s.router.PrepareOracleCall(OracleRequest{
		Topic:            query,
		Memories:         candidates,
		Prompt:           query,
		Reason:           "user-triggered reflect",
		IncludeSensitive: body.IncludeSensitive,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrOracleDisabled):
			http.Error(w, err.Error(), http.StatusForbidden)
		case errors.Is(err, ErrOracleTopicLocal):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, ErrOracleAllRedacted):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			http.Error(w, "oracle prep: "+err.Error(), http.StatusInternalServerError)
		}
		return
	}

	// 5. Build the synthesis prompt. Distinct from /research's "research the
	//    following query" framing — reflect's job is to weave the memory set
	//    into an answer, not to seek external knowledge.
	// Iter 25 — fetch optimized_text for each candidate so the prompt can
	// include the fact-dense compression alongside the raw body. Phase 0b
	// produces these; legacy / unprocessed memories return "" and
	// buildReflectPrompt skips the inline.
	var optimizedTexts map[string]string
	if s.bank != nil && len(safe.Memories) > 0 {
		ids := make([]string, 0, len(safe.Memories))
		for _, m := range safe.Memories {
			ids = append(ids, m.ID)
		}
		ot, oerr := s.bank.GetOptimizedTextBatch(ids)
		if oerr != nil {
			log.Printf("reflect: optimized-text batch lookup failed: %v", oerr)
		} else {
			optimizedTexts = ot
		}
	}
	// Synaptic is an indexed memory store; the agent in the loop
	// (Claude, GPT, your own client) is the question-answering engine.
	// /reflect is a thin convenience wrapper for callers without an
	// agent in the loop — it templates retrieved memories into a
	// minimal prompt and calls Tier 3. No classifier, no abstention
	// detector, no contradiction scanner — those are agent concerns.
	// Production callers should prefer /recall (returns ranked memories
	// without synthesis) and run their own reasoning over the results.
	finalPrompt := buildReflectPrompt(safe.Prompt, safe.Memories, body.Context, optimizedTexts, body.AsOfDate)
	_ = rerankKept // retained as a metric for observability

	// 6. Tier 3 call. 120s was tight even for OpenAI; with local Tier 3
	// (qwen2.5:14b, llama3.1:70b) a single synthesis routinely takes
	// 60-180s. Use 10 min so the provider's own timeout (set per-tier
	// in settings, default 60s for remote / 5 min for local Ollama)
	// drives the actual cutoff instead of this outer wrapper.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	// v2.7 Bundle O — procedural memory injection. Active procedures
	// prepend to the system role so the agent's reflections respect
	// the user's standing directives.
	procPrefix := BuildSystemPromptWithProcedures(s.bank, "" /* no scope filter */, "")
	msgs := []Message{}
	if procPrefix != "" {
		msgs = append(msgs, Message{Role: "system", Content: procPrefix})
	}
	msgs = append(msgs, Message{Role: "user", Content: finalPrompt})
	content, err := safe.Provider.Chat(ctx, msgs, budget.MaxTokens)
	if err != nil {
		http.Error(w, "oracle: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 7. Token accounting. RemoteProvider.Chat doesn't surface usage; use
	//    the same length heuristic /research uses.
	tokensIn := estimateTokens(finalPrompt)
	tokensOut := estimateTokens(content)
	tokensTotal := tokensIn + tokensOut

	today := time.Now().UTC().Format("2006-01-02")
	providerKind := providerKindFromName(safe.Provider.Name())
	if err := s.bank.AddTokensUsedV2(today, string(Tier3), providerKind, safe.Provider.Name(),
		!safe.Provider.IsLocal(), tokensIn, tokensOut); err != nil {
		log.Printf("reflect: budget update failed: %v", err)
	}

	// 8. Audit. Redacted — never the content, never the prompt body.
	auditAfter, _ := json.Marshal(map[string]interface{}{
		"query_len":              len(query),
		"budget":                 budget.Label,
		"top_k":                  budget.TopK,
		"recall_count":           recallCount,
		"used_count":             len(safe.Memories),
		"sensitive_excluded":     safe.SensitiveExcluded,
		"sensitive_included":     safe.SensitiveIncluded,
		"included_sensitive":     body.IncludeSensitive,
		"local_concept_excluded": safe.LocalConceptExcluded,
		"tokens_in":              tokensIn,
		"tokens_out":             tokensOut,
		"tokens_total":           tokensTotal,
		"model":                  safe.Provider.Name(),
		"elapsed_ms":             time.Since(startedAt).Milliseconds(),
	})
	s.auditWrite(AuditEntry{
		Operation:  "oracle_reflect",
		EntityType: "reflect",
		EntityID:   "", // reflect has no persisted entity; the audit row is the trail
		AfterJSON:  string(auditAfter),
		AdapterID:  adapterIDFromRequest(r),
		Reason:     "user-triggered reflect",
	})

	// 9. WS broadcast so the dashboard can update its activity feed + chip.
	if s.hub != nil {
		s.hub.Fanout(Event{
			SchemaVersion: SchemaVersion,
			Type:          "reflect_done",
			Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
			AdapterID:     "sd-core-reflect",
			Payload: map[string]interface{}{
				"tokens_used":  reflectTokensUsed{In: tokensIn, Out: tokensOut, Total: tokensTotal},
				"source_count": len(safe.Memories),
				"model":        safe.Provider.Name(),
				"budget":       budget.Label,
			},
		})
	}

	log.Printf("reflect: query_len=%d budget=%s used=%d/%d sensitive_excluded=%d local_excluded=%d tokens=%d model=%s elapsed=%s",
		len(query), budget.Label, len(safe.Memories), recallCount,
		safe.SensitiveExcluded, safe.LocalConceptExcluded,
		tokensTotal, safe.Provider.Name(), time.Since(startedAt).Round(time.Millisecond))

	// v2.7 Bundle R — optionally persist the synthesis as a reflection
	// memory. Sourcing it through SaveMemory means it picks up the
	// dirty-flag / sensitive-scan / dedup machinery for free. The
	// reflection inherits the source memory IDs as synthesis_source_ids
	// so the Phase 8 schema / dashboard lineage tooling can trace where
	// the takeaway came from.
	memoryID := ""
	if body.SaveAsMemory && strings.TrimSpace(content) != "" {
		rec := MemoryRecord{
			Text:               content,
			Tags:               []string{"reflection"},
			Source:             "reflect",
			MemoryType:         "reflection",
			SynthesisSourceIDs: memoryIDs(safe.Memories),
		}
		saved, serr := s.bank.SaveMemory(rec)
		if serr != nil {
			log.Printf("reflect: SaveMemory failed (returning synthesis only): %v", serr)
		} else {
			memoryID = saved.ID
		}
	}

	writeJSON(w, http.StatusOK, reflectResponse{
		Synthesis:                       content,
		SourceMemoryIDs:                 memoryIDs(safe.Memories),
		TokensUsed:                      reflectTokensUsed{In: tokensIn, Out: tokensOut, Total: tokensTotal},
		Model:                           safe.Provider.Name(),
		MemoriesExcludedByPrivacyFilter: safe.SensitiveExcluded + safe.LocalConceptExcluded,
		IncludedSensitiveCount:          safe.SensitiveIncluded,
		SensitiveExcluded:               safe.SensitiveExcluded,
		LocalConceptExcluded:            safe.LocalConceptExcluded,
		RecallCount:                     recallCount,
		UsedCount:                       len(safe.Memories),
		MemoryID:                        memoryID,
	})
}

// ── Helpers ────────────────────────────────────────────────────────────────

// reflectTagsMatch implements the tags_match semantics. Empty filter passes
// every record. Empty mode defaults to "any" (case-insensitive).
func reflectTagsMatch(memTags, want []string, mode string) bool {
	if len(want) == 0 {
		return true
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	strict := strings.HasSuffix(mode, "_strict")
	requireAll := strings.HasPrefix(mode, "all")

	// Build the haystack once.
	haystack := make(map[string]struct{}, len(memTags))
	for _, t := range memTags {
		if !strict {
			t = strings.ToLower(t)
		}
		haystack[strings.TrimSpace(t)] = struct{}{}
	}

	matched := 0
	for _, t := range want {
		key := strings.TrimSpace(t)
		if !strict {
			key = strings.ToLower(key)
		}
		if _, ok := haystack[key]; ok {
			matched++
			if !requireAll {
				return true // "any" — first hit wins
			}
		} else if requireAll {
			return false // "all" — first miss fails
		}
	}
	return requireAll && matched == len(want)
}

// buildReflectPrompt frames the synthesis ask. Distinct from /research's
// "go look this up" prompt — reflect asks the model to weave the user's
// own memory set into an answer.
//
// Iter 25 — when `optimizedTexts` is non-empty, the prompt embeds the
// fact-dense Phase 0b compression alongside raw text. Order in the
// prompt: raw body first (canonical source of truth), optimized text
// second (fact-dense crib), dream summary third (thematic context).
func buildReflectPrompt(query string, memories []MemoryRecord, userContext string, optimizedTexts map[string]string, asOfDate string) string {
	var b strings.Builder
	b.Grow(len(query) + len(userContext) + 80*len(memories) + 512)
	b.WriteString(`You are answering a question about the user's own past, grounded in the memory context below.

`)
	// Today's date — kept because relative-time arithmetic ("how many days ago")
	// genuinely benefits from a stated anchor. Generic, not benchmark-tuned.
	today := strings.TrimSpace(asOfDate)
	if today == "" {
		today = time.Now().UTC().Format("2006-01-02")
	}
	b.WriteString("Today's date is ")
	b.WriteString(today)
	b.WriteString(".\n\n")
	b.WriteString(`Answer the user's question using the memories below. If the memories don't contain enough information to answer, say so plainly rather than inventing facts. Cite memory IDs in [brackets] when you reference specific memories.

`)
	// v2.7 Bundle I lesson — "lost in the middle" mitigation. LLMs
	// reliably lose track of a question that appears BEFORE 20+ memory
	// blobs. Putting MEMORY CONTEXT first (so the model reads them
	// carefully) and the QUESTION at the end (so it's the freshest
	// instruction the model has) consistently lifts recall accuracy
	// on long-context QA. See Liu et al. 2023 "Lost in the Middle".
	if strings.TrimSpace(userContext) != "" {
		b.WriteString("ADDITIONAL CONTEXT FROM USER:\n")
		b.WriteString(strings.TrimSpace(userContext))
		b.WriteString("\n\n")
	}
	b.WriteString("MEMORY CONTEXT (top matches; sensitive entries already excluded):\n")
	for _, m := range memories {
		// Iter 23 (2026-05-14): send BOTH raw Text and EnrichedText to
		// synthesis. Phase 0b enrichment produces a 120-char "summary_new"
		// useful for thematic/preference questions ("what kind of
		// cocktails does the user prefer?") but strong Tier 2 models
		// (qwen2.5:14b+) compress aggressively and drop specific facts
		// ("10 times" → "perfecting Negroni game"). Raw Text preserves
		// the facts; EnrichedText preserves the abstract pattern. The
		// synthesis model is fully capable of using both — give it the
		// information and let it pick.
		text := m.Text
		// Cap per-memory text so one runaway record can't swamp the prompt.
		// 2000 chars covers virtually all conversational turns without
		// truncation; very long pasted-document memories get clipped here.
		if len(text) > 2000 {
			text = text[:2000] + "…"
		}
		// Tag the memory with its ID so the model can cite by reference;
		// the dashboard's source-memories expander uses the same IDs
		// returned in source_memory_ids.
		b.WriteString("[")
		b.WriteString(m.ID)
		b.WriteString("] ")
		b.WriteString(text)
		// Iter 25 — fact-dense Phase 0b compression. Sits between raw and
		// dream summary because it's the densest fact-preserving form:
		// short enough that the model parses every keyword, long enough
		// that no specific number/name/date is dropped.
		if opt, ok := optimizedTexts[m.ID]; ok && opt != "" && opt != m.Text {
			b.WriteString("  [facts: ")
			b.WriteString(opt)
			b.WriteString("]")
		}
		// Append the dream summary as a parenthetical hint — labeled so
		// the model knows it's the abstracted view, not the raw words.
		// Skipped when EnrichedText is empty (no dream yet) or equals
		// Text (some migration paths copy Text into EnrichedText).
		if m.EnrichedText != "" && m.EnrichedText != m.Text {
			enriched := m.EnrichedText
			if len(enriched) > 200 {
				enriched = enriched[:200] + "…"
			}
			b.WriteString("  [dream summary: ")
			b.WriteString(enriched)
			b.WriteString("]")
		}
		b.WriteString("\n")
	}
	b.WriteString("\nQUESTION:\n")
	b.WriteString(query)
	b.WriteString("\n")
	return b.String()
}

// isChronologicalQuery returns true when the question is asking about
// the ORDER / SEQUENCE of events — i.e. when retrieval-rank order is
// the wrong order to present results in. Used by /reflect to chrono-
// sort the candidate memories before they reach the agent / synthesis
// model. Generic helper; agents that prefer their own ordering can
// call /recall and ignore this signal.
func isChronologicalQuery(q string) bool {
	l := strings.ToLower(strings.TrimSpace(q))
	if l == "" {
		return false
	}
	triggers := []string{
		"list the order",
		"in what order",
		"in which order",
		"in what sequence",
		"what came first",
		"what did i bring up first",
		"order in which",
		"sequence in which",
		"timeline",
		"chronological",
		"chronologically",
		"in chronological order",
	}
	for _, t := range triggers {
		if strings.Contains(l, t) {
			return true
		}
	}
	return false
}
