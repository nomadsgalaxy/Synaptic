// handlers_augmentation.go — research-augmentation engine for thin maps.
//
// Goal (per user's stated intent in the augmentation handoff):
//
//   "Use Tier 3 / Oracle to fill in publicly-knowable Concept / Technology
//    / Topic maps where the user's personal context is sparse — but never
//    research personal/private maps and never burn tokens on rich maps."
//
// Safety properties (load-bearing):
//
//   1. Strict category gate: maps with category in augmentableMapCategories
//      ARE eligible. project / entity / "" / unknown ARE NOT. The
//      `map_overrides` row is the only signal — we don't infer category
//      from tag names.
//
//   2. LocalConcept gate: any map with a memory tagged in SD_LOCAL_CONCEPTS
//      is HARD-skipped. The egress chokepoint already filters these per-
//      memory; the candidate filter is defense-in-depth so we don't even
//      queue them.
//
//   3. Sensitive gate: any map with a sensitive=true memory is skipped.
//      Same reasoning — egress would catch it, but no point queueing.
//
//   4. Coverage thresholds: a map must be sparse on BOTH count AND avg
//      chars to be a candidate (count < min OR avg < min).
//
//   5. Cooldown: skip maps augmented within the last N days. Prevents
//      re-burning tokens on the same map every nightly run.
//
//   6. Per-run caps: max candidates and max tokens enforced by the
//      caller (run-now and the future nightly phase) so a runaway
//      candidate set can't drain the budget.
//
//   7. Off by default: `augment_enabled=false` is the shipped default.
//      User must explicitly opt in via the dashboard before any auto-
//      egress to Tier 3 happens.
//
// What's NOT in this file (deferred — see frontend handoff for status):
//
//   - Recall integration (Piece 1 of the handoff): /recall doesn't yet
//     search research_cache. Augmented memories ARE created with the
//     map's tag so they show up as memory hits, but the research_cache
//     entry itself isn't yet vector-indexed for cross-search.
//
//   - Nightly auto-augmentation phase (Piece 3): NightlyRunner's
//     runStepsFn is still a no-op. Manual trigger via
//     POST /maps/augmentation/run-now is the user's path for now;
//     wiring the same logic into a nightly phase is a small follow-up
//     once the demo confirms the candidate gates behave correctly.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AugmentSettings is the typed view over the 6 augment_* keys in the
// generic settings table. Defaults match the augmentation handoff.
type AugmentSettings struct {
	Enabled            bool `json:"enabled"`
	MaxPerRun          int  `json:"max_per_run"`
	MaxTokensPerRun    int  `json:"max_tokens_per_run"`
	MinMemories        int  `json:"min_memories"`
	MinAvgChars        int  `json:"min_avg_chars"`
	CooldownDays       int  `json:"cooldown_days"`
}

// DefaultAugmentSettings returns the safe-by-default config: OFF, with
// thresholds calibrated to "thin Concept/Technology maps only." Anything
// the user wants tighter or looser they configure explicitly.
// idLooksLikeMapID matches the backend's memory_maps.id format:
// "map-" followed by digits. Used by findAugmentCandidates to detect
// when an override row is keyed by a backend map ID vs a tag, so we
// can resolve to the map's anchor_tag instead of treating the ID as
// a tag (which always returns zero memories).
func idLooksLikeMapID(s string) bool {
	if len(s) < 5 || s[:4] != "map-" {
		return false
	}
	for i := 4; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func DefaultAugmentSettings() AugmentSettings {
	return AugmentSettings{
		Enabled:         false, // off until user explicitly opts in
		MaxPerRun:       3,
		MaxTokensPerRun: 5000,
		MinMemories:     5,
		MinAvgChars:     200,
		CooldownDays:    30,
	}
}

// settings keys — closed enum
const (
	keyAugEnabled         = "augment_enabled"
	keyAugMaxPerRun       = "augment_max_per_run"
	keyAugMaxTokensPerRun = "augment_max_tokens_per_run"
	keyAugMinMemories     = "augment_min_memories"
	keyAugMinAvgChars     = "augment_min_avg_chars"
	keyAugCooldownDays    = "augment_cooldown_days"
)

// GetAugmentSettings reads all 6 augment_* keys, falling back to
// DefaultAugmentSettings for any missing field.
func (b *Bank) GetAugmentSettings() (AugmentSettings, error) {
	s := DefaultAugmentSettings()
	if b == nil {
		return s, nil
	}
	if v, ok, _ := b.GetSetting(keyAugEnabled); ok {
		s.Enabled = v == "true" || v == "1"
	}
	for _, pair := range []struct {
		key string
		dst *int
	}{
		{keyAugMaxPerRun, &s.MaxPerRun},
		{keyAugMaxTokensPerRun, &s.MaxTokensPerRun},
		{keyAugMinMemories, &s.MinMemories},
		{keyAugMinAvgChars, &s.MinAvgChars},
		{keyAugCooldownDays, &s.CooldownDays},
	} {
		if v, ok, _ := b.GetSetting(pair.key); ok {
			if n, err := strconv.Atoi(v); err == nil {
				*pair.dst = n
			}
		}
	}
	return s, nil
}

// SaveAugmentSettings persists the typed shape via the generic settings table.
func (b *Bank) SaveAugmentSettings(s AugmentSettings) error {
	pairs := map[string]string{
		keyAugEnabled:         fmt.Sprintf("%t", s.Enabled),
		keyAugMaxPerRun:       strconv.Itoa(s.MaxPerRun),
		keyAugMaxTokensPerRun: strconv.Itoa(s.MaxTokensPerRun),
		keyAugMinMemories:     strconv.Itoa(s.MinMemories),
		keyAugMinAvgChars:     strconv.Itoa(s.MinAvgChars),
		keyAugCooldownDays:    strconv.Itoa(s.CooldownDays),
	}
	for k, v := range pairs {
		if err := b.SetSetting(k, v); err != nil {
			return err
		}
	}
	return nil
}

// AugmentCandidate is one map the engine would research. Keep this small —
// the dashboard preview renders 5-10 of these at a time.
type AugmentCandidate struct {
	MapKey         string `json:"map_key"`
	Category       string `json:"category"`
	MemoryCount    int    `json:"memory_count"`
	AvgChars       int    `json:"avg_chars"`
	CanonicalQuery string `json:"canonical_query"`
}

// findAugmentCandidates walks the map_overrides table and returns maps
// that pass every gate (category, count/coverage, cooldown, sensitive,
// LocalConcept). Sorted by AvgChars ascending — thinnest maps first so
// per-run caps spend on the highest-leverage augmentations.
//
// Pure-data — no Tier 3 calls. Safe to call from a read endpoint for the
// dashboard's preview.
func findAugmentCandidates(ctx context.Context, bank *Bank, settings AugmentSettings, localConcepts map[string]bool) ([]AugmentCandidate, error) {
	if bank == nil {
		return nil, errors.New("bank not enabled")
	}
	overrides, err := bank.ListMapOverrides()
	if err != nil {
		return nil, err
	}
	if len(overrides) == 0 {
		return nil, nil
	}
	cooldownCutoff := time.Now().UTC().AddDate(0, 0, -settings.CooldownDays).Format(time.RFC3339Nano)

	out := []AugmentCandidate{}
	for _, ovr := range overrides {
		// Gate 1: category opt-in.
		if !augmentableMapCategories[ovr.Category] {
			continue
		}
		// Gate 1b: archived maps are off-limits — user signalled "stop touching this".
		if ovr.Archived {
			continue
		}
		// Gate 2: map_key in LocalConcepts → off-limits.
		if localConcepts[ovr.MapKey] {
			continue
		}
		// Gather memories tagged with this map key. Tags are JSON arrays
		// in the column — pull the candidate set then walk.
		//
		// 2026-05-21 — for backend-persisted maps, the override row's
		// MapKey is the backend map ID (e.g. "map-1778725083848674410"),
		// NOT a real tag. Looking up memories by that ID always returned
		// zero — which made every accepted backend map look like a "thin
		// map" needing research, and showed "oauth · 0 mem · 0 avg" in
		// the augmentation preview while the actual map had 21 traces.
		// Resolve to the map's first anchor_tag when MapKey looks like an ID.
		lookupTag := ovr.MapKey
		if idLooksLikeMapID(ovr.MapKey) {
			m, err := bank.GetMemoryMap(ovr.MapKey)
			if err == nil && len(m.AnchorTags) > 0 {
				lookupTag = m.AnchorTags[0]
			}
		}
		mems, err := bank.listMemoriesByTag(lookupTag)
		if err != nil {
			return nil, err
		}
		// Gate 3: sensitive memory → off-limits.
		// Gate 3b: any LocalConcept tag in the map's memories → off-limits.
		hasSensitive := false
		hasLocalConcept := false
		totalChars := 0
		for _, m := range mems {
			if m.Sensitive {
				hasSensitive = true
				break
			}
			for _, t := range m.Tags {
				if localConcepts[t] {
					hasLocalConcept = true
					break
				}
			}
			if hasLocalConcept {
				break
			}
			totalChars += len(m.Text) + len(m.EnrichedText)
		}
		if hasSensitive || hasLocalConcept {
			continue
		}
		// Gate 4: coverage thresholds. Eligible if count < min OR avg < min.
		count := len(mems)
		avg := 0
		if count > 0 {
			avg = totalChars / count
		}
		eligibleByCount := count < settings.MinMemories
		eligibleByAvg := avg < settings.MinAvgChars
		if !eligibleByCount && !eligibleByAvg {
			continue
		}
		// Gate 5: cooldown — skip if we augmented within the window.
		recentlyAugmented := false
		// Look at audit_log for nightly_augment / manual_augment with
		// after_json containing this map_key, since cooldownCutoff.
		entries, _ := bank.ListAuditLog(AuditFilter{
			Operation: "nightly_augment", Since: cooldownCutoff, Limit: 50,
		})
		for _, e := range entries {
			if strings.Contains(e.AfterJSON, `"map_key":"`+ovr.MapKey+`"`) {
				recentlyAugmented = true
				break
			}
		}
		if !recentlyAugmented {
			entries2, _ := bank.ListAuditLog(AuditFilter{
				Operation: "manual_augment", Since: cooldownCutoff, Limit: 50,
			})
			for _, e := range entries2 {
				if strings.Contains(e.AfterJSON, `"map_key":"`+ovr.MapKey+`"`) {
					recentlyAugmented = true
					break
				}
			}
		}
		if recentlyAugmented {
			continue
		}

		out = append(out, AugmentCandidate{
			MapKey:         ovr.MapKey,
			Category:       ovr.Category,
			MemoryCount:    count,
			AvgChars:       avg,
			CanonicalQuery: canonicalAugmentQuery(ovr),
		})
	}
	// Sort thinnest first.
	sort.Slice(out, func(i, j int) bool {
		return out[i].AvgChars < out[j].AvgChars
	})
	return out, nil
}

// canonicalAugmentQuery builds the deterministic Oracle prompt for a
// candidate. Templates per category — keeps spend predictable and
// avoids an extra LLM call to generate the prompt.
func canonicalAugmentQuery(ovr MapOverride) string {
	display := ovr.Alias
	if display == "" {
		display = ovr.MapKey
	}
	switch ovr.Category {
	case "concept":
		return fmt.Sprintf(
			"Explain the concept of %q, including key terminology, common algorithms or approaches, and typical use cases. Be concise, factual, and avoid speculation.",
			display)
	case "technology":
		return fmt.Sprintf(
			"Provide a structured overview of the %q technology covering its architecture, key features, common use cases, and how it differs from comparable alternatives. Be concise and factual.",
			display)
	default: // topic
		return fmt.Sprintf(
			"Explain %q, including what it is, why it matters, and the most important things to know about it. Be concise, factual, and avoid speculation.",
			display)
	}
}

// listMemoriesByTag is a small helper that walks the memories table for
// rows whose tags JSON array contains the given map key. SQLite lacks
// native JSON-array containment without the json1 extension being
// reliably available, so we filter in Go after a coarse LIKE pre-filter
// to avoid a full scan on big banks.
func (b *Bank) listMemoriesByTag(tag string) ([]MemoryRecord, error) {
	if b == nil {
		return nil, errors.New("bank not enabled")
	}
	// LIKE pre-filter narrows the candidate set to rows whose tags column
	// even mentions the tag string. We then verify in Go.
	rows, err := b.db.Query(`SELECT `+memoryColumns+`
		FROM memories
		WHERE deleted_at = '' AND tags LIKE '%' || ? || '%'`, tag)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MemoryRecord{}
	for rows.Next() {
		rec, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		// Strict check: the tag must be in the array as an exact match.
		// LIKE can false-positive on substring (e.g., "X" matches "X-Y").
		matched := false
		for _, t := range rec.Tags {
			if t == tag {
				matched = true
				break
			}
		}
		if matched {
			out = append(out, rec)
		}
	}
	return out, rows.Err()
}

// ──────────────────────────────────────────────────────────────────────────
// HTTP
// ──────────────────────────────────────────────────────────────────────────

// getAugmentationCandidates handles GET /maps/augmentation/candidates.
// Read-only preview — lists what would be augmented next run. The
// dashboard's Maps tab renders this as a "Tonight's run will augment N
// maps:" panel.
func (s *Server) getAugmentationCandidates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET", "OPTIONS")
		return
	}
	settings, err := s.bank.GetAugmentSettings()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	localConcepts := map[string]bool{}
	if s.router != nil {
		localConcepts = s.router.LocalConcepts
	}
	cands, err := findAugmentCandidates(r.Context(), s.bank, settings, localConcepts)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"settings":   settings,
		"count":      len(cands),
		"candidates": cands,
	})
}

// postAugmentationRunNow handles POST /maps/augmentation/run-now. Body:
//
//   { "map_keys": ["concept:vector-embeddings"], "dry_run": false }
//
// When map_keys is empty/absent, runs all eligible candidates. dry_run
// returns the candidate list + estimated token cost without spending.
//
// Synchronous (returns when done) so the dashboard can show the result
// without polling. Caps from settings still apply.
func (s *Server) postAugmentationRunNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST", "OPTIONS")
		return
	}
	if s.router == nil || s.router.Tier3() == nil {
		http.Error(w, "Tier 3 / Oracle is not configured", http.StatusForbidden)
		return
	}
	settings, _ := s.bank.GetAugmentSettings()
	// Master switch: required for run-now too. Without an explicit
	// `augment_enabled=true` setting, the user hasn't opted in to any
	// auto-egress — surface a 403 they can fix.
	if !settings.Enabled {
		http.Error(w, "augmentation disabled (set augment_enabled=true to enable)", http.StatusForbidden)
		return
	}

	var body struct {
		MapKeys []string `json:"map_keys"`
		DryRun  bool     `json:"dry_run"`
	}
	if err := readJSON(r, &body); err != nil {
		// Tolerate empty body.
		body.MapKeys = nil
		body.DryRun = false
	}

	localConcepts := map[string]bool{}
	if s.router != nil {
		localConcepts = s.router.LocalConcepts
	}
	cands, err := findAugmentCandidates(r.Context(), s.bank, settings, localConcepts)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Filter to user-specified map_keys when present.
	if len(body.MapKeys) > 0 {
		want := map[string]bool{}
		for _, k := range body.MapKeys {
			want[k] = true
		}
		filtered := cands[:0]
		for _, c := range cands {
			if want[c.MapKey] {
				filtered = append(filtered, c)
			}
		}
		cands = filtered
	}

	if body.DryRun {
		// Estimate tokens at ~prompt_chars/4 + 1024 (rough output budget)
		// per candidate.
		estTotal := 0
		for _, c := range cands {
			estTotal += estimateTokens(c.CanonicalQuery) + 1024
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"dry_run":               true,
			"candidates":            cands,
			"estimated_tokens":      estTotal,
			"max_tokens_per_run":    settings.MaxTokensPerRun,
			"max_candidates_per_run": settings.MaxPerRun,
		})
		return
	}

	runID := fmt.Sprintf("aug-%d", nextMonotonicNano())
	results := []map[string]interface{}{}
	tokensSpent := 0
	for i, c := range cands {
		if i >= settings.MaxPerRun {
			break
		}
		if tokensSpent >= settings.MaxTokensPerRun {
			break
		}
		// Reuse the research handler's chokepoint by calling
		// PrepareOracleCall + the provider directly. We don't fetch
		// memories as context (this is "fill the gap with public
		// knowledge", not "ground my memories").
		safe, err := s.router.PrepareOracleCall(OracleRequest{
			Topic:  c.CanonicalQuery,
			Prompt: c.CanonicalQuery,
			Reason: "manual augmentation: " + c.MapKey,
		})
		if err != nil {
			results = append(results, map[string]interface{}{
				"map_key": c.MapKey, "error": err.Error(),
			})
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		content, err := safe.Provider.Chat(ctx,
			[]Message{{Role: "user", Content: safe.Prompt}},
			1024,
		)
		cancel()
		if err != nil {
			results = append(results, map[string]interface{}{
				"map_key": c.MapKey, "error": err.Error(),
			})
			continue
		}
		tIn := estimateTokens(safe.Prompt)
		tOut := estimateTokens(content)
		tokensSpent += tIn + tOut

		// Save research_cache entry under the canonical query hash so
		// the GET /research list/detail endpoints surface it too.
		researchID := researchID(c.CanonicalQuery)
		richEntry := researchEntry{
			ID:          researchID,
			Query:       c.CanonicalQuery,
			Content:     content,
			CachedAt:    time.Now().UTC().Format(time.RFC3339Nano),
			ExpiresAt:   time.Now().UTC().AddDate(0, 0, 30).Format(time.RFC3339Nano),
			TokensIn:    tIn,
			TokensOut:   tOut,
			TokensTotal: tIn + tOut,
			Model:       safe.Provider.Name(),
		}
		richJSON, _ := json.Marshal(richEntry)
		_ = s.bank.SaveResearchCache(ResearchCacheEntry{
			Topic:     researchID,
			Payload:   string(richJSON),
			FetchedAt: richEntry.CachedAt,
			ExpiresAt: richEntry.ExpiresAt,
			FetchedBy: "auto", // augmentation is system-driven, not user-typed
		})

		// Charge the budget under tier3.
		today := time.Now().UTC().Format("2006-01-02")
		_ = s.bank.AddTokensUsedV2(today, string(Tier3),
			providerKindFromName(safe.Provider.Name()), safe.Provider.Name(),
			!safe.Provider.IsLocal(), tIn, tOut)

		// Create the augmented memory tagged with the map key.
		text := content
		if len(text) > 4000 {
			text = text[:4000]
		}
		mem, err := s.bank.SaveMemory(MemoryRecord{
			Text:            text,
			Tags:            []string{c.MapKey, "augment", "oracle_augmented"},
			Source:          "manual_augment",
			OracleAugmented: true,
			LightEncoded:    true,
		})
		if err != nil {
			results = append(results, map[string]interface{}{
				"map_key": c.MapKey, "error": "save memory: " + err.Error(),
			})
			continue
		}

		// Audit row — operation tagged so the cooldown check can find it.
		auditAfter := fmt.Sprintf(
			`{"map_key":"%s","memory_id":"%s","tokens":%d,"research_id":"%s","run_id":"%s"}`,
			c.MapKey, mem.ID, tIn+tOut, researchID, runID)
		_ = s.bank.AppendAudit(AuditEntry{
			Operation:  "manual_augment",
			EntityType: "memory",
			EntityID:   mem.ID,
			AfterJSON:  auditAfter,
			AdapterID:  "sd-core-augment",
			Reason: fmt.Sprintf("auto-augment: thin %s map (%d memories, %d avg chars)",
				c.Category, c.MemoryCount, c.AvgChars),
		})

		results = append(results, map[string]interface{}{
			"map_key":      c.MapKey,
			"memory_id":    mem.ID,
			"research_id":  researchID,
			"tokens_total": tIn + tOut,
		})
	}

	log.Printf("augment-run %s: %d candidates → %d augmented, %d tokens",
		runID, len(cands), len(results), tokensSpent)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"run_id":            runID,
		"candidates":        cands,
		"augmented_count":   len(results),
		"tokens_spent":      tokensSpent,
		"results":           results,
	})
}

// settingsAugment handles GET/PUT /settings/augment for the typed
// AugmentSettings shape. The 6 keys live in the generic settings store;
// this convenience endpoint round-trips them as a structured object.
func (s *Server) settingsAugment(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		st, err := s.bank.GetAugmentSettings()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, st)
	case http.MethodPut:
		var st AugmentSettings
		if err := readJSON(r, &st); err != nil {
			http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.bank.SaveAugmentSettings(st); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.auditWrite(AuditEntry{
			Operation: "upsert", EntityType: "setting", EntityID: "augment",
			AfterJSON: mustJSON(st), AdapterID: adapterIDFromRequest(r),
		})
		writeJSON(w, http.StatusOK, st)
	default:
		methodNotAllowed(w, "GET", "PUT", "OPTIONS")
	}
}
