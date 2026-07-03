// nightly_phase0b.go — Phase 0b "deep encoding" / REM-style per-memory
// Tier 2 enrichment.
//
// Background (per handoff "deep per-memory enrichment phase 2026-05-10"):
//
// The user's mental model is that REM consolidation is "thinking about
// each memory harder" — each trace gets a chance to be re-summarised,
// re-tagged, re-located in the brain, in light of the rest of the bank.
// The original Phase 0 only embeds + flips a flag; nothing actually
// re-thinks the memory. Phase 0b fills that gap.
//
// Key invariants:
//   - Sequential per-memory by default (concurrency = 1). The user's
//     stated intent: "this is meant to take a long time, and we should
//     allow it to go one by one."
//   - Resumable. Token + memory budgets cap the per-run work; the
//     cursor + dirty-queue mechanism means the next run picks up where
//     the previous one stopped.
//   - Same patterns as Phase 0: clean degrade when Tier 2 unconfigured,
//     circuit breaker on 3 consecutive Tier 2 failures, errors land in
//     phase_failures (capped) rather than aborting the whole run.
//   - Per-memory persistence is transactional via Bank.PersistDeepEncoding.
//     Embedding is recomputed against the new summary so Stage 2 phases
//     work on the refined content rather than ingestion-time text.
//   - Sensitive memories ARE enriched locally (Tier 2 = local Ollama by
//     default), but the audit's `Reason` field is redacted so we don't
//     leak Tier 2's reasoning across the audit channel.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

// runPhase0bDeepEncoding drains the dirty queue using Tier 2 enrichment.
// Returns nil for "phase ran, possibly produced 0 enrichments cleanly"
// and only returns error when something fundamental went wrong (DB
// failure mid-loop). Per-memory failures land in phase_failures.
func (pc *pipelineContext) runPhase0bDeepEncoding() error {
	if !pc.settings.DeepEnrichEnabled {
		return nil // master switch off — phase is a no-op
	}
	// Clean-degrade: missing Tier 2 → one phase_failures entry, no work,
	// no error. Same shape as Phase 0's no-Tier-1 handling.
	if pc.router == nil {
		pc.stats.PhaseFailures = append(pc.stats.PhaseFailures, "deep_enrich:tier2_unconfigured")
		return nil
	}
	tier2 := pc.router.ForNightly()
	if tier2 == nil {
		pc.stats.PhaseFailures = append(pc.stats.PhaseFailures, "deep_enrich:tier2_unconfigured")
		return nil
	}
	embedder := pc.router.ForEmbedding() // optional — skip embedding refresh when nil
	embedModel := envOr("SD_SYNAPSE_MODEL", envOr("SD_CLASSIFY_MODEL", "llama3.2:3b"))

	// Initial coverage snapshot for the stats block.
	live, encoded, _ := pc.bank.CountDeepEncoded()
	totalDirty, byReason, _ := pc.bank.CountDirtyMemories()

	maxPerRun := pc.settings.DeepEnrichMaxPerRun
	if maxPerRun <= 0 {
		maxPerRun = 100
	}
	tokenBudget := pc.settings.DeepEnrichTokenBudget
	if tokenBudget <= 0 {
		tokenBudget = 50000
	}

	const consecutiveFailureLimit = 3
	const failedRetryBudget = 3 // skip memories that have failed >= this many times

	enrichedThisRun := 0
	tokensInTotal := 0
	tokensOutTotal := 0
	consecutiveFailures := 0
	cursor := ""
	failureSeen := map[string]bool{} // dedup phase_failures spam per memory

	for {
		if pc.ctx.Err() != nil {
			break
		}
		if enrichedThisRun >= maxPerRun {
			log.Printf("nightly: phase0b stopped at max_per_run=%d", maxPerRun)
			break
		}
		if tokensInTotal+tokensOutTotal >= tokenBudget {
			log.Printf("nightly: phase0b stopped at token_budget=%d (consumed=%d)",
				tokenBudget, tokensInTotal+tokensOutTotal)
			break
		}
		if consecutiveFailures >= consecutiveFailureLimit {
			pc.stats.PhaseFailures = append(pc.stats.PhaseFailures,
				fmt.Sprintf("deep_enrich:circuit_breaker (%d consecutive Tier 2 failures)", consecutiveFailures))
			log.Printf("nightly: phase0b circuit breaker tripped after %d consecutive failures", consecutiveFailures)
			break
		}

		mem, ok, err := pc.bank.PickNextDirtyMemory()
		if err != nil {
			return fmt.Errorf("phase0b pick-next: %w", err)
		}
		if !ok {
			break // queue drained
		}
		// Retry-budget guard — don't hammer a memory whose Tier 2 calls
		// always error. After 3 consecutive failures we drop it from the
		// queue and surface in phase_failures so the user can fix the
		// underlying content (e.g., bad characters that break the prompt).
		if fails, _ := pc.bank.CountDeepEncodingFailures(mem.ID); fails >= failedRetryBudget {
			if !failureSeen[mem.ID] {
				pc.stats.PhaseFailures = append(pc.stats.PhaseFailures,
					fmt.Sprintf("deep_enrich:retry_budget_exhausted (%s)", mem.ID))
				failureSeen[mem.ID] = true
			}
			// Mark "clean" so the queue doesn't hand it back forever.
			// Sets deep_encoded_at = now with no model — the dashboard
			// can show "enrich failed" by checking failure log. CRITICAL:
			// we MUST also stamp deep_encoded_at because PickNextDirtyMemory's
			// WHERE matches `deep_encoded_at = '' OR marked_dirty_at >
			// deep_encoded_at`. Clearing marked_dirty_at alone leaves a
			// never-encoded memory (deep_encoded_at='') in the queue and
			// the picker keeps handing back the same row → hot infinite
			// loop pinning CPU at 100% with zero log output.
			now := time.Now().UTC().Format(time.RFC3339Nano)
			pc.bank.wmu.Lock()
			_, _ = pc.bank.db.Exec(
				`UPDATE memories SET marked_dirty_at = '', dirty_reason = '', deep_encoded_at = ? WHERE id = ?`,
				now, mem.ID,
			)
			pc.bank.wmu.Unlock()
			consecutiveFailures = 0 // not a Tier 2 failure — don't trip breaker
			continue
		}

		// Live-progress polling for the Dream Journal UI + lifecycle
		// heartbeat. See nightly_phase0b_progress.go for the full
		// contract. Bracket the (potentially many-minute) Tier 2 call
		// so the poller's goroutine never outlives the enrichment.
		// memIndex = enrichedThisRun is the count of memories ALREADY
		// completed; the active one is the (enrichedThisRun+1)th. We
		// pass `enrichedThisRun` and the renderer adds 1 for display.
		// memTotal caps at maxPerRun — that's the real ceiling for
		// the user's "memories remaining" mental model in this run.
		stopPoller := pc.startPhase0bProgressPoller(tier2, mem, enrichedThisRun, maxPerRun)
		result, err := pc.enrichMemoryWithTier2(tier2, mem)
		stopPoller()
		if err != nil {
			consecutiveFailures++
			truncated := err.Error()
			if len(truncated) > 80 {
				truncated = truncated[:80]
			}
			pc.stats.PhaseFailures = append(pc.stats.PhaseFailures,
				fmt.Sprintf("deep_enrich:enrich_failed (%s: %s)", mem.ID, truncated))
			_ = pc.bank.LogDeepEncodingFailure(mem.ID, tier2.Name(), err.Error(), mem.DirtyReason)
			log.Printf("nightly: phase0b enrich %s failed: %v", mem.ID, err)
			continue
		}
		consecutiveFailures = 0

		// Apply suggested changes — drop tag suggestions that look like
		// noise (very short or matching existing tag verbatim).
		newTags := applyTagSuggestions(mem.Tags, result.TagsAdded, result.TagsRemoved)
		summaryDiff := buildSummaryDiff(mem, result, newTags)
		// Sensitive memories: redact the audit-attached reasoning so
		// Tier 2's interpretation of the body doesn't leak via audit_log.
		auditReason := result.Reasoning
		if mem.Sensitive {
			auditReason = "[REDACTED — sensitive memory; Tier 2 reasoning omitted]"
		}

		if err := pc.bank.PersistDeepEncoding(
			mem.ID, result.SummaryNew, newTags, result.RegionNew,
			tier2.Name(), auditReason,
			result.TokensIn, result.TokensOut, summaryDiff, mem.DirtyReason,
		); err != nil {
			pc.stats.PhaseFailures = append(pc.stats.PhaseFailures,
				fmt.Sprintf("deep_enrich:persist_failed (%s: %v)", mem.ID, err))
			continue
		}
		// Iter 25 — also persist the fact-dense compression. Separate
		// helper so the legacy PersistDeepEncoding signature doesn't
		// have to change. Best-effort; an OT failure doesn't block the
		// enrichment success path.
		if opt := strings.TrimSpace(result.OptimizedText); opt != "" {
			if err := pc.bank.SaveOptimizedText(mem.ID, opt); err != nil {
				log.Printf("nightly: phase0b SaveOptimizedText %s: %v", mem.ID, err)
			}
		}

		// Refresh the embedding so Stage 2 phases see the new content.
		// Best-effort: an embedding miss after enrichment isn't fatal —
		// the synapse builder will catch up async.
		//
		// CRITICAL: embed against raw `mem.Text`, NOT `result.SummaryNew`.
		// Coverage + embed-all both key embeddings by `hash(text + tags)`;
		// if we hash the enriched summary here, the embedding lands under
		// a key the coverage query never looks for — the memory appears
		// un-embedded even though a fresh vector exists. Tags may have
		// changed via applyTagSuggestions, so we re-embed with newTags
		// to update the (text, tags) hash entry — but the text source
		// must stay the raw immutable `mem.Text` to match the hash
		// convention used by /admin/embed-all and CountEmbeddedMemories.
		if embedder != nil {
			tagsAny := make([]interface{}, len(newTags))
			for i, t := range newTags {
				tagsAny[i] = t
			}
			embedText := synapseEmbedText(map[string]interface{}{
				"text": mem.Text, "tags": tagsAny,
			})
			ectx, cancel := context.WithTimeout(pc.ctx, 30*time.Second)
			vec, eerr := embedder.Embed(ectx, embedText)
			cancel()
			if eerr == nil {
				_ = pc.bank.SaveEmbedding(synapseTextHash(embedText), vec, embedModel)
			}
		}

		// Audit row for the change (silent — we don't want a per-memory
		// audit.appended firehose for what could be 100 enrichments).
		_ = pc.bank.AppendAuditSilent(AuditEntry{
			Operation:  "memory_deep_encoded",
			EntityType: "memory",
			EntityID:   mem.ID,
			AfterJSON: fmt.Sprintf(
				`{"model":%q,"summary_diff":%q,"tokens_in":%d,"tokens_out":%d,"trigger":%q}`,
				tier2.Name(), summaryDiff, result.TokensIn, result.TokensOut, mem.DirtyReason,
			),
			Reason:    auditReason,
			AdapterID: "sd-core-nightly",
		})

		enrichedThisRun++
		tokensInTotal += result.TokensIn
		tokensOutTotal += result.TokensOut
		cursor = mem.ID

		// Charge token budget under tier2.
		pc.chargeBudget(tier2, result.TokensIn, result.TokensOut, Tier2)

		// Success log — used to be silent (only failures logged), which
		// made it impossible to follow per-memory progress from
		// `docker logs synaptic-core` without polling the API. Memory
		// id deliberately truncated to first 8 chars to avoid bloating
		// logs at 100-memory-per-run scale while still being grep-able
		// against bank rows.
		log.Printf("nightly: phase0b enrich %s ok (memory %d/%d, tokens_in=%d tokens_out=%d)",
			mem.ID[:min(len(mem.ID), 12)],
			enrichedThisRun, maxPerRun,
			result.TokensIn, result.TokensOut,
		)
	}

	// Final coverage snapshot for the stats block.
	postLive, postEncoded, _ := pc.bank.CountDeepEncoded()
	postDirty, postByReason, _ := pc.bank.CountDirtyMemories()
	coverage := 0.0
	if postLive > 0 {
		coverage = float64(postEncoded) / float64(postLive) * 100.0
	}
	estRuns := 0
	if enrichedThisRun > 0 && postDirty > 0 {
		estRuns = (postDirty + enrichedThisRun - 1) / enrichedThisRun // ceiling div
	}
	pc.stats.DeepEncoding = &DeepEncodingStats{
		EnrichedThisRun:           enrichedThisRun,
		Remaining:                 postDirty,
		CoveragePercent:           coverage,
		Cursor:                    cursor,
		TokensIn:                  tokensInTotal,
		TokensOut:                 tokensOutTotal,
		EstimatedRunsToCompletion: estRuns,
		Queue: DeepEncodingQueueRef{
			TotalDirty:  postDirty,
			TotalQueued: postDirty, // wave-6 alias (CODE_HANDOFF rename dirty→undreamt)
			ByReason:    postByReason,
		},
	}
	pc.stats.SchemaVersion = 3

	// Suppress unused vars when no work happened (initial snapshot only).
	_ = live
	_ = encoded
	_ = totalDirty
	_ = byReason
	return nil
}

// deepEnrichResponse mirrors the JSON shape Phase 0b's Tier 2 prompt asks
// for. Forgiving parser handles slight deviations (extra prose, missing
// fields). All fields are optional from Tier 2's perspective; an empty
// SummaryNew/RegionNew means "Tier 2 chose to keep current."
type deepEnrichResponse struct {
	SummaryNew  string   `json:"summary_new"`
	TagsAdded   []string `json:"tags_added"`
	TagsRemoved []string `json:"tags_removed"`
	RegionNew   string   `json:"region_new"`
	Reasoning   string   `json:"reasoning"`
	// Iter 25 — fact-dense key-value compression of mem.Text, produced
	// alongside the summary. See buildDeepEnrichPrompt for format.
	OptimizedText string `json:"optimized_text"`
	TokensIn      int    `json:"-"`
	TokensOut     int    `json:"-"`
}

// enrichMemoryWithTier2 builds the prompt, calls Tier 2, parses the
// response. Returns the structured suggestion + token counts. Pure
// function modulo the Tier 2 call itself — easy to mock in tests by
// substituting a scriptedProvider for the router.
func (pc *pipelineContext) enrichMemoryWithTier2(provider LLMProvider, mem MemoryRecord) (deepEnrichResponse, error) {
	// 2026-05-23 — trace logs to diagnose the deep_encode hang. The user
	// observed that with Tier 2 on OpenAI, no API calls land but CPU
	// stays at 89% — meaning we never reach provider.Chat. These log
	// lines bracket each step so the offending one shows up clearly.
	log.Printf("phase0b/trace: enrichMemoryWithTier2 START mem=%s text_len=%d tags=%d", mem.ID, len(mem.Text), len(mem.Tags))
	t0 := time.Now()
	neighbours := pc.findEnrichmentNeighbours(mem, 5)
	log.Printf("phase0b/trace: findEnrichmentNeighbours DONE mem=%s neighbours=%d elapsed=%s", mem.ID, len(neighbours), time.Since(t0).Round(time.Millisecond))

	t1 := time.Now()
	prompt := buildDeepEnrichPrompt(mem, neighbours)
	log.Printf("phase0b/trace: buildDeepEnrichPrompt DONE mem=%s prompt_len=%d elapsed=%s", mem.ID, len(prompt), time.Since(t1).Round(time.Millisecond))

	pc.signalProviderUse("nightly_run")
	log.Printf("phase0b/trace: signalProviderUse DONE mem=%s", mem.ID)

	timeout := pc.llmCallTimeout()
	log.Printf("phase0b/trace: about to call provider.Chat mem=%s timeout=%s provider=%s", mem.ID, timeout, provider.Name())
	nctx, cancel := context.WithTimeout(pc.ctx, timeout)
	defer cancel()
	tChat := time.Now()
	out, err := provider.Chat(nctx, []Message{{Role: "user", Content: prompt}}, 800)
	log.Printf("phase0b/trace: provider.Chat RETURNED mem=%s elapsed=%s err=%v out_len=%d", mem.ID, time.Since(tChat).Round(time.Millisecond), err, len(out))
	if err != nil {
		return deepEnrichResponse{}, err
	}
	resp := parseDeepEnrichResponse(out)
	resp.TokensIn = estimateTokens(prompt)
	resp.TokensOut = estimateTokens(out)
	return resp, nil
}

// findEnrichmentNeighbours returns up to k other memories most similar to
// `mem` by embedding cosine. Used to give Tier 2 context about what the
// bank is "about" so suggested tags / region are coherent with siblings.
// Cheap when embeddings are cached; degrades gracefully when they aren't.
func (pc *pipelineContext) findEnrichmentNeighbours(mem MemoryRecord, k int) []MemoryRecord {
	if pc.bank == nil || k <= 0 {
		return nil
	}
	model := envOr("SD_SYNAPSE_MODEL", envOr("SD_CLASSIFY_MODEL", "llama3.2:3b"))
	t0 := time.Now()
	log.Printf("phase0b/trace: AllEmbeddingsForMemories CALL mem=%s model=%s", mem.ID, model)
	vecs, err := pc.bank.AllEmbeddingsForMemories(model)
	log.Printf("phase0b/trace: AllEmbeddingsForMemories RETURN mem=%s vecs=%d err=%v elapsed=%s", mem.ID, len(vecs), err, time.Since(t0).Round(time.Millisecond))
	if err != nil || len(vecs) == 0 {
		return nil
	}
	target, ok := vecs[mem.ID]
	if !ok {
		return nil
	}
	type scored struct {
		id  string
		sim float64
	}
	scoredList := make([]scored, 0, len(vecs))
	for id, v := range vecs {
		if id == mem.ID {
			continue
		}
		scoredList = append(scoredList, scored{id: id, sim: float64(vecDot(target, v))})
	}
	sort.Slice(scoredList, func(i, j int) bool { return scoredList[i].sim > scoredList[j].sim })
	if len(scoredList) > k {
		scoredList = scoredList[:k]
	}
	out := make([]MemoryRecord, 0, len(scoredList))
	for _, s := range scoredList {
		nb, err := pc.bank.GetMemory(s.id)
		if err == nil {
			out = append(out, nb)
		}
	}
	return out
}

// buildDeepEnrichPrompt assembles the Tier 2 prompt. The structure matches
// the handoff's spec; the response is requested as JSON, but the parser
// is forgiving when Tier 2 wraps it in prose.
func buildDeepEnrichPrompt(mem MemoryRecord, neighbours []MemoryRecord) string {
	var b strings.Builder
	b.WriteString(`You are reviewing one memory in a personal-knowledge brain to improve its summary, tags, and anatomical placement. Use the surrounding memories as context for what this brain is about, but the output is JUST about the target memory.

TARGET MEMORY:
  text: "`)
	b.WriteString(escapeForPrompt(mem.Text))
	b.WriteString("\"\n")
	// Write-time temporal anchoring: resolve relative dates ("today",
	// "last Friday") to absolute YYYY-MM-DD so retrieval/synthesis never
	// has to re-derive the date from context. Tier 2 will rewrite "today"
	// → the absolute date string in optimized_text.
	if mem.CreatedAt != "" {
		if t, terr := time.Parse(time.RFC3339Nano, mem.CreatedAt); terr == nil {
			b.WriteString("  recorded_at: ")
			b.WriteString(t.UTC().Format("2006-01-02 (Monday)"))
			b.WriteString("\n")
		} else if t, terr := time.Parse(time.RFC3339, mem.CreatedAt); terr == nil {
			b.WriteString("  recorded_at: ")
			b.WriteString(t.UTC().Format("2006-01-02 (Monday)"))
			b.WriteString("\n")
		}
	}
	if mem.EnrichedText != "" {
		b.WriteString("  current summary: \"")
		b.WriteString(escapeForPrompt(mem.EnrichedText))
		b.WriteString("\"\n")
	}
	if len(mem.Tags) > 0 {
		b.WriteString("  current tags: ")
		b.WriteString(strings.Join(mem.Tags, ", "))
		b.WriteString("\n")
	}
	if mem.RegionHint != "" {
		b.WriteString("  current region: ")
		b.WriteString(mem.RegionHint)
		b.WriteString("\n")
	}

	if len(neighbours) > 0 {
		b.WriteString("\nNEIGHBOURING MEMORIES (top similarity, for context):\n")
		for _, n := range neighbours {
			snippet := n.Text
			if len(snippet) > 200 {
				snippet = snippet[:200] + "…"
			}
			fmt.Fprintf(&b, "  - %q (region=%s)\n", escapeForPrompt(snippet), n.RegionHint)
		}
	}
	b.WriteString(`
Return ONLY a JSON object with this exact shape (no preamble, no code fences):
{
  "summary_new":    "...",            // improved one-line summary, max 120 chars; "" to keep current
  "optimized_text": "...",            // fact-dense key-value compression of the target memory body.
                                      // Format: "Field: value; Field: value; ..." preserving EVERY
                                      // number, date, name, place, brand, outcome. Strip filler
                                      // words ("really", "kind of", "I guess"), first-person
                                      // pronouns where implied, redundant adjectives. Example:
                                      //   raw  = "I went fishing in the Colorado River on May 14th and got me 10 large bass, but it wasn't really that good"
                                      //   opt  = "Activity: fishing; Location: Colorado River; Date: May 14; Catch: 10 large bass; Quality: poor"
                                      // Aim for 3-5x compression vs raw. Lossless on facts.
                                      //
                                      // CRITICAL — write-time normalization rules:
                                      //   (a) DECISIONS among multiple options. When the memory shows
                                      //       options being suggested AND one being picked ("decided",
                                      //       "went with", "settled on", "named it", "picked", "REALLY
                                      //       cool one", "I'll go with"), extract as:
                                      //         "Decision: <topic> = <chosen> | Rejected: <list>"
                                      //       Final choice ALWAYS first. Earlier suggestions go in
                                      //       Rejected list. Example:
                                      //         raw  = "Sure, how about 'Contaminated Colossus'? ...
                                      //                  4. Fissionator. ... Fissionator is a REALLY
                                      //                  cool one"
                                      //         opt  = "Decision: zombie name = Fissionator | Rejected:
                                      //                 Contaminated Colossus, Irradiated Behemoth"
                                      //   (b) RELATIVE dates. Use the recorded_at above as the anchor
                                      //       date for resolving "today", "yesterday", "last Friday",
                                      //       "two weeks ago", "this morning", etc. Store the
                                      //       resolved ABSOLUTE date in optimized_text. Example
                                      //       (recorded_at = 2023-05-24 Wednesday):
                                      //         raw  = "I joined Book Lovers Unite three weeks ago"
                                      //         opt  = "Action: joined Book Lovers Unite;
                                      //                 Joined: 2023-05-03 (three weeks before
                                      //                 2023-05-24)"
                                      //         raw  = "I attended a meetup last week"
                                      //         opt  = "Event: book club meetup; Attended: 2023-05-17
                                      //                 (last week of 2023-05-24)"
                                      //   (c) PREFERENCES / interests. When memory expresses a
                                      //       preference for a topic / artist / style / brand,
                                      //       extract as:
                                      //         "Preference: <topic> = <specific value>"
                                      //       Quote concrete terms (named brands, specific
                                      //       sub-genres). Example:
                                      //         raw  = "I recently discovered a bluegrass band that
                                      //                 features a banjo player and started enjoying
                                      //                 their music today"
                                      //         opt  = "Discovery: bluegrass band (banjo-featuring);
                                      //                 Listening since: 2023-03-31; Genre interest:
                                      //                 bluegrass"
  "tags_added":   ["..."],            // suggested new tags
  "tags_removed": ["..."],            // suggested tag removals
  "region_new":   "frontal_lobe",     // canonical region name, or "" to keep current
  "reasoning":    "..."               // 1-2 sentence explanation of changes
}`)
	return b.String()
}

// escapeForPrompt makes a string safe to embed in the prompt's JSON-ish
// surroundings. Just enough to avoid breaking the JSON structure when
// Tier 2 reads back its own prompt — not a full JSON escaper.
func escapeForPrompt(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 600 {
		s = s[:600] + "…"
	}
	return s
}

// parseDeepEnrichResponse extracts the JSON block from Tier 2's reply,
// tolerating a leading explanation, trailing whitespace, or code fences.
// On parse failure returns an empty response — the caller will save no
// changes (treat as "Tier 2 chose to keep everything as is"), preserving
// the dirty marker so the next run can retry.
func parseDeepEnrichResponse(raw string) deepEnrichResponse {
	raw = strings.TrimSpace(raw)
	// Strip code fences if present.
	if strings.HasPrefix(raw, "```") {
		// find first newline after the fence header
		if i := strings.Index(raw, "\n"); i >= 0 {
			raw = raw[i+1:]
		}
		if i := strings.LastIndex(raw, "```"); i >= 0 {
			raw = raw[:i]
		}
	}
	// Find the first { and the last } so a leading paragraph doesn't
	// trip the parser. (Some models like to over-explain.)
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end <= start {
		return deepEnrichResponse{}
	}
	jsonBlob := raw[start : end+1]
	var resp deepEnrichResponse
	if err := json.Unmarshal([]byte(jsonBlob), &resp); err != nil {
		return deepEnrichResponse{}
	}
	// Hard cap summary length so a chatty response can't blow out the
	// memory's display.
	if len(resp.SummaryNew) > 240 {
		resp.SummaryNew = resp.SummaryNew[:240]
	}
	if len(resp.Reasoning) > 400 {
		resp.Reasoning = resp.Reasoning[:400]
	}
	return resp
}

// applyTagSuggestions merges Tier 2's tag suggestions onto the existing
// list. Strips tags Tier 2 wants removed; appends added ones (deduped,
// case-insensitive); ignores empty / 1-character noise. Order preserved
// for tags Tier 2 didn't touch — easier for the user to recognise.
func applyTagSuggestions(existing, added, removed []string) []string {
	removeSet := map[string]bool{}
	for _, r := range removed {
		removeSet[strings.ToLower(strings.TrimSpace(r))] = true
	}
	out := make([]string, 0, len(existing)+len(added))
	have := map[string]bool{}
	for _, t := range existing {
		lc := strings.ToLower(strings.TrimSpace(t))
		if lc == "" || have[lc] || removeSet[lc] {
			continue
		}
		have[lc] = true
		out = append(out, t)
	}
	for _, a := range added {
		lc := strings.ToLower(strings.TrimSpace(a))
		if lc == "" || len(lc) < 2 || have[lc] {
			continue
		}
		have[lc] = true
		out = append(out, a)
	}
	return out
}

// buildSummaryDiff returns a short human-readable string describing what
// Tier 2 changed. Stored verbatim in deep_encoding_log.summary_diff for
// the trace-detail history panel ("Refined summary; added pkce tag").
func buildSummaryDiff(before MemoryRecord, after deepEnrichResponse, newTags []string) string {
	parts := []string{}
	if after.SummaryNew != "" && after.SummaryNew != before.EnrichedText {
		parts = append(parts, "refined summary")
	}
	beforeTags := map[string]bool{}
	for _, t := range before.Tags {
		beforeTags[strings.ToLower(t)] = true
	}
	addedNames := []string{}
	for _, t := range newTags {
		if !beforeTags[strings.ToLower(t)] {
			addedNames = append(addedNames, t)
		}
	}
	afterTags := map[string]bool{}
	for _, t := range newTags {
		afterTags[strings.ToLower(t)] = true
	}
	removedNames := []string{}
	for _, t := range before.Tags {
		if !afterTags[strings.ToLower(t)] {
			removedNames = append(removedNames, t)
		}
	}
	if len(addedNames) > 0 {
		parts = append(parts, "added: "+strings.Join(addedNames, ","))
	}
	if len(removedNames) > 0 {
		parts = append(parts, "removed: "+strings.Join(removedNames, ","))
	}
	if after.RegionNew != "" && after.RegionNew != before.RegionHint {
		parts = append(parts, "region→"+after.RegionNew)
	}
	if len(parts) == 0 {
		return "no changes"
	}
	out := strings.Join(parts, "; ")
	if len(out) > 200 {
		out = out[:200] + "…"
	}
	return out
}

// errPersistDeepEnrichmentMissing is unused but kept for symmetry with
// the handoff's error tree and to make the future per-memory failure
// reasons grep-able.
var errPersistDeepEnrichmentMissing = errors.New("persist_deep_enrichment: missing")

func init() { _ = errPersistDeepEnrichmentMissing }
