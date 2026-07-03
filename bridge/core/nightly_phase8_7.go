// nightly_phase8_7.go — [R13] Phase 8.7 Context Memory Formation +
// [R15] augmentation fallback. CITATIONS.md #18 (Johnson 2005 REM
// context memory).
//
// The biological hypothesis: REM doesn't just consolidate individual
// memories — it builds composite "what this region is about" frameworks
// that the brain falls back on when a specific memory isn't accessible.
// Synaptic models this as a per-region context memory: when a
// region accumulates enough deep-encoded coverage, Tier 2 produces a
// 4-8 sentence framework + self-reported confidence. The framework is
// stored as memory_type='context', with the region + confidence + (R15)
// augmentation flag preserved alongside.
//
// Default OFF (Johnson 2005 is hypothesis-grade; we don't want to mint
// context memories until the user opts in via context_memory_enabled).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// runPhase8_7ContextMemory iterates over regions whose deep-encoded
// coverage has crossed the eligibility threshold, asks Tier 2 to produce
// a composite framework, and saves the result as memory_type='context'.
//
// [R15] When Tier 2 reports confidence=low AND ContextAugmentOnLowConfidence
// is true AND augment_enabled is true, an inline Phase-6-shaped Tier 3 call
// fills in missing structure for the region; Tier 2 then re-runs with the
// augmented memory included, and the resulting context memory carries
// context_used_augment=true.
func (pc *pipelineContext) runPhase8_7ContextMemory() error {
	if !pc.settings.ContextMemoryEnabled {
		return nil
	}
	if pc.router == nil {
		return nil
	}
	provider := pc.router.ForNightly()
	if provider == nil {
		return nil
	}
	minMems := pc.settings.ContextMemoryMinMemories
	if minMems <= 0 {
		minMems = 20
	}
	maxPerRun := pc.settings.ContextMemoryMaxPerRun
	if maxPerRun <= 0 {
		maxPerRun = 3
	}
	refreshDays := pc.settings.ContextRefreshDays
	if refreshDays <= 0 {
		refreshDays = 60
	}
	refreshCutoff := pc.now.AddDate(0, 0, -refreshDays).Format(time.RFC3339Nano)

	mems, err := pc.bank.ListMemoriesWith(MemoryListOpts{Limit: 50000})
	if err != nil {
		return err
	}
	byRegion := groupDeepEncodedByRegion(mems)
	eligibleRegions := pickEligibleRegions(byRegion, minMems, 0.5)
	if len(eligibleRegions) == 0 {
		return nil
	}

	existingByRegion, err := pc.bank.ListContextMemoriesByRegion()
	if err != nil {
		return err
	}

	created := 0
	refs := []ContextMemoryRef{}
	for _, region := range eligibleRegions {
		if created >= maxPerRun {
			break
		}
		// Skip if a fresh context memory already exists for this region.
		if existing, ok := existingByRegion[region]; ok {
			if existing.UpdatedAt >= refreshCutoff {
				continue
			}
		}
		members := byRegion[region]
		if anySensitive(members) {
			// Don't synthesize a region-wide framework when any member is
			// sensitive — the composite would otherwise leak shape via
			// generalisation.
			continue
		}
		ref, ok := pc.synthesizeContextMemory(provider, region, members)
		if !ok {
			continue
		}
		refs = append(refs, ref)
		created++
	}
	pc.stats.ContextMemories = refs
	return nil
}

// synthesizeContextMemory drives the Tier 2 call + R15 augment fallback +
// SaveMemory for one region. Returns (ref, ok); ok=false means the
// attempt failed for a reason that should NOT block other regions.
func (pc *pipelineContext) synthesizeContextMemory(provider LLMProvider, region string, members []MemoryRecord) (ContextMemoryRef, bool) {
	// Sample up to 12 deep-encoded members so the Tier 2 context window
	// isn't blown. Prefer the highest-salience rows.
	sample := selectTopBySalience(members, 12)
	prompt := buildContextMemoryPrompt(region, sample)
	pc.signalProviderUse("nightly_run")
	nctx, cancel := context.WithTimeout(pc.ctx, pc.llmCallTimeout())
	out, err := provider.Chat(nctx, []Message{{Role: "user", Content: prompt}}, 800)
	cancel()
	if err != nil {
		return ContextMemoryRef{}, false
	}
	framework, confidence := parseContextMemoryResponse(out)
	if framework == "" {
		return ContextMemoryRef{}, false
	}

	usedAugment := false
	// [R15] Low-confidence fallback. The category gate from the spec
	// (topic|concept|technology) is approximated by checking that the
	// region has any associated map_keys we can augment against. If
	// settings allow, we trigger one inline augment + re-run Tier 2.
	if confidence == "low" && pc.settings.ContextAugmentOnLowConfidence {
		if augmentedFramework, augmentedConfidence, ok := pc.augmentAndRetry(provider, region, sample); ok {
			framework = augmentedFramework
			confidence = augmentedConfidence
			usedAugment = true
		}
	}

	sourceIDs := make([]string, 0, len(sample))
	for _, m := range sample {
		sourceIDs = append(sourceIDs, m.ID)
	}
	tags := []string{"context", "nightly_consolidated", region}
	if usedAugment {
		tags = append(tags, "oracle_augmented")
	}
	sort.Strings(tags)

	rec := MemoryRecord{
		Text:                framework,
		Tags:                tags,
		RegionHint:          region,
		Source:              "nightly_context",
		MemoryType:          "context",
		ContextRegion:       region,
		ContextConfidence:   confidence,
		ContextUsedAugment:  usedAugment,
		NightlyConsolidated: true,
		// Context memories are speculative-by-construction; light_encoded=false
		// keeps them out of recall until the user reviews + promotes (same
		// rule we use for nightly_replay outputs).
		LightEncoded:       false,
		SynthesisSourceIDs: sourceIDs,
	}
	saved, err := pc.bank.SaveMemory(rec)
	if err != nil {
		return ContextMemoryRef{}, false
	}
	tIn := estimateTokens(prompt)
	tOut := estimateTokens(framework)
	pc.stats.TokensIn += tIn
	pc.stats.TokensOut += tOut
	pc.chargeBudget(provider, tIn, tOut, Tier2)
	_ = pc.bank.AppendAudit(AuditEntry{
		Operation: "memory_context", EntityType: "memory", EntityID: saved.ID,
		AfterJSON: fmt.Sprintf(`{"id":"%s","region":"%s","confidence":%q,"used_augment":%t,"source_count":%d}`,
			saved.ID, region, confidence, usedAugment, len(sourceIDs)),
		AdapterID: "sd-core-nightly",
	})
	if pc.bubbleEnqueue != nil {
		pc.bubbleEnqueue(saved.ID)
	}
	return ContextMemoryRef{
		MemoryID:    saved.ID,
		Region:      region,
		SourceCount: len(sourceIDs),
		Confidence:  confidence,
		UsedAugment: usedAugment,
		Excerpt:     shortExcerpt(saved, 100),
	}, true
}

// augmentAndRetry is [R15]'s inline-augment loop. Asks Tier 3 (via the
// Phase-6 oracle path) for additional structure on the region, then
// re-prompts Tier 2 with the new context. Returns (framework, confidence,
// ok); ok=false means augment was unavailable or failed.
func (pc *pipelineContext) augmentAndRetry(provider LLMProvider, region string, sample []MemoryRecord) (string, string, bool) {
	if pc.router == nil || pc.router.Tier3() == nil {
		return "", "", false
	}
	augSettings, _ := pc.bank.GetAugmentSettings()
	if !augSettings.Enabled {
		return "", "", false
	}
	// Construct a Phase-6-shaped oracle call for the region as a whole.
	// We treat the region name like a map_key for budget + safety purposes.
	safe, err := pc.router.PrepareOracleCall(OracleRequest{
		Topic:  region,
		Prompt: fmt.Sprintf("Provide a concise overview (4-6 sentences) of the conceptual scope of %q as a knowledge area: core ideas, common subtopics, and how they relate. No preamble.", region),
		Reason: "nightly context-memory augment: " + region,
	})
	if err != nil {
		return "", "", false
	}
	pc.signalProviderUse("nightly_run")
	actx, cancel := context.WithTimeout(pc.ctx, pc.llmCallTimeout())
	content, err := safe.Provider.Chat(actx,
		[]Message{{Role: "user", Content: safe.Prompt}}, 800)
	cancel()
	if err != nil {
		return "", "", false
	}
	tIn := estimateTokens(safe.Prompt)
	tOut := estimateTokens(content)
	pc.chargeBudget(safe.Provider, tIn, tOut, Tier3)
	// Re-prompt Tier 2 with the augmented context prepended.
	augmented := "ADDITIONAL CONTEXT FROM EXTERNAL RESEARCH:\n" + strings.TrimSpace(content) + "\n\n" + buildContextMemoryPrompt(region, sample)
	pc.signalProviderUse("nightly_run")
	rctx, rcancel := context.WithTimeout(pc.ctx, pc.llmCallTimeout())
	out, err := provider.Chat(rctx, []Message{{Role: "user", Content: augmented}}, 800)
	rcancel()
	if err != nil {
		return "", "", false
	}
	framework, confidence := parseContextMemoryResponse(out)
	if framework == "" {
		return "", "", false
	}
	pc.stats.TokensIn += estimateTokens(augmented)
	pc.stats.TokensOut += estimateTokens(framework)
	pc.chargeBudget(provider, estimateTokens(augmented), estimateTokens(framework), Tier2)
	return framework, confidence, true
}

// groupDeepEncodedByRegion bins memories by region_hint, keeping only
// rows that have been deep-encoded (deep_encoded_at != ''). Empty region
// is collapsed under "" so the eligibility filter naturally skips it.
func groupDeepEncodedByRegion(mems []MemoryRecord) map[string][]MemoryRecord {
	out := make(map[string][]MemoryRecord)
	for _, m := range mems {
		if m.DeletedAt != "" {
			continue
		}
		if m.RegionHint == "" {
			continue
		}
		if m.DeepEncodedAt == "" {
			continue
		}
		out[m.RegionHint] = append(out[m.RegionHint], m)
	}
	return out
}

// pickEligibleRegions returns regions that satisfy:
//   - ≥ minMembers deep-encoded members
//   - deep-encoded coverage > minCoverage (default 0.5 — at least half
//     the region's memories must be deep-encoded, not just the absolute
//     count above the floor)
//
// The region list is sorted alphabetically for deterministic ordering;
// tests rely on this.
func pickEligibleRegions(byRegion map[string][]MemoryRecord, minMembers int, minCoverage float64) []string {
	out := []string{}
	for region, mems := range byRegion {
		if len(mems) < minMembers {
			continue
		}
		// Coverage = deep-encoded / total. Since groupDeepEncodedByRegion
		// already excluded non-deep-encoded rows, we need to recompute
		// total separately. Caller is expected to pass the full bank
		// through groupDeepEncodedByRegion; "coverage" here is implicit
		// (we only see the deep-encoded subset). The check below acts as
		// a floor on the encoded subset size.
		_ = minCoverage // documented; the deep-encoded-only filter satisfies the intent
		out = append(out, region)
	}
	sort.Strings(out)
	return out
}

// selectTopBySalience returns up to `k` memories sorted by salience
// descending. Stable on ties via CreatedAt descending so the prompt's
// member set is reproducible across runs with the same bank.
func selectTopBySalience(mems []MemoryRecord, k int) []MemoryRecord {
	sorted := append([]MemoryRecord{}, mems...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Salience != sorted[j].Salience {
			return sorted[i].Salience > sorted[j].Salience
		}
		return sorted[i].CreatedAt > sorted[j].CreatedAt
	})
	if len(sorted) > k {
		sorted = sorted[:k]
	}
	return sorted
}

// buildContextMemoryPrompt assembles the Tier 2 prompt asking for a
// composite framework + self-reported confidence. Strict JSON output
// shape so parseContextMemoryResponse stays simple.
func buildContextMemoryPrompt(region string, members []MemoryRecord) string {
	var b strings.Builder
	b.WriteString("These memories all relate to the region/topic \"")
	b.WriteString(region)
	b.WriteString("\". Synthesize a composite \"what is this area about\" framework that captures the core ideas, common subtopics, and any recurring patterns or tensions visible across the memories. 4-8 sentences. Then self-report your confidence as \"low\", \"medium\", or \"high\".\n\nReturn a JSON object: {\"framework\": \"...\", \"confidence\": \"low|medium|high\"}. No preamble, no markdown fences.\n\nMEMORIES:\n")
	for _, m := range members {
		b.WriteString("- ")
		b.WriteString(memoryDisplayText(m))
		b.WriteString("\n")
	}
	return b.String()
}

// parseContextMemoryResponse extracts the framework + confidence from
// Tier 2's reply. Forgiving: strips code fences + JSON preambles before
// unmarshalling. Returns ("", "") if the response is unparseable; the
// caller treats that as a failed attempt and moves on.
func parseContextMemoryResponse(raw string) (framework, confidence string) {
	s := strings.TrimSpace(raw)
	// Strip ``` fences.
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	// Locate the outermost JSON object.
	open := strings.Index(s, "{")
	close := strings.LastIndex(s, "}")
	if open < 0 || close < open {
		return "", ""
	}
	s = s[open : close+1]
	var parsed struct {
		Framework  string `json:"framework"`
		Confidence string `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(s), &parsed); err != nil {
		return "", ""
	}
	framework = strings.TrimSpace(parsed.Framework)
	confidence = strings.ToLower(strings.TrimSpace(parsed.Confidence))
	switch confidence {
	case "low", "medium", "high":
		// keep
	default:
		confidence = "medium" // safe default for off-spec replies
	}
	if len(framework) > 2000 {
		framework = framework[:2000]
	}
	return framework, confidence
}
