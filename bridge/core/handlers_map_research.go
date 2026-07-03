// handlers_map_research.go — POST /maps/{id}/research
//
// User-initiated research scoped to a single MemoryMap. The point of
// scoping it to a map (vs the generic POST /research) is to close the
// feedback loop between Oracle research and the dream pipeline:
//
//   1. Frontend opens the Research… dialog from the map detail header.
//      Query is prefilled from map.name + anchor tags + trace excerpts.
//   2. Backend resolves the map's anchor tags, pulls live memories that
//      carry those tags as the Oracle's context (filtered through the
//      egress chokepoint — sensitive + LocalConcept rows excluded).
//   3. Tier 3 returns a synthesis.
//   4. Result lands in research_cache with map_id set, AND a NEW memory
//      is created carrying the map's anchor tags + source=manual_augment
//      + oracle_augmented=true. Because the new row is a normal memory
//      with the map's tags, the existing dream pipeline picks it up:
//        - Phase 0b deep-encodes it next nightly run (dirty=created)
//        - Phase 7 cross-region matches it against other tagged memories
//        - Phase 8 schema synthesis sees it when clustering by anchor tag
//        - Phase 8.7 context memory formation can promote it
//        - It appears under the map's "Recently augmented from research"
//          panel (existing affordance filters traces by
//          source==='manual_augment'|nightly_augment).
//
// This is the same shape Phase 6's nightly auto-augment uses (see
// runPhase6Augmentation in nightly_pipeline.go) — manual research just
// front-loads the same path for a single user-chosen map.
package main

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

type mapResearchRequest struct {
	Query string `json:"query"`
	// Optional knobs — frontend doesn't currently send these; defaults match
	// the generic /research handler.
	Model     string `json:"model,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
	TTLHours  int    `json:"ttl_hours,omitempty"`
}

type mapResearchResponse struct {
	Entry                researchEntry `json:"entry"`
	MemoryID             string        `json:"memory_id"`
	MemoryTags           []string      `json:"memory_tags"`
	SensitiveExcluded    int           `json:"sensitive_excluded"`
	LocalConceptExcluded int           `json:"local_concept_excluded"`
	CacheHit             bool          `json:"cache_hit"`
	MapID                string        `json:"map_id"`
	MapName              string        `json:"map_name"`
	AnchorTags           []string      `json:"anchor_tags"`
	MemoriesUsedAsContext int          `json:"memories_used_as_context"`
}

// handleMapResearch serves POST /maps/{id}/research.
//
// id can be either a backend-persisted map UUID (lookup memory_maps row to
// get anchor_tags) OR a raw tag name for synthesized concept maps (treat
// the id as the single anchor tag and pull memories carrying it).
func (s *Server) handleMapResearch(w http.ResponseWriter, r *http.Request, id string) {
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
		http.Error(w, "bank disabled", http.StatusServiceUnavailable)
		return
	}
	if s.router == nil || s.router.Tier3() == nil {
		http.Error(w, "Tier 3 / Oracle is not configured", http.StatusForbidden)
		return
	}

	var body mapResearchRequest
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

	// Resolve the map. Two shapes are supported:
	//   1. backend UUID — looks up memory_maps row, reads anchor_tags
	//   2. raw tag       — treats id as the single anchor tag (synthesized
	//                      concept maps where frontend's map.id === tag)
	var (
		mapID      string
		mapName    string
		anchorTags []string
	)
	if rec, err := s.bank.GetMemoryMap(id); err == nil {
		mapID = rec.ID
		mapName = rec.Name
		anchorTags = append(anchorTags, rec.AnchorTags...)
		if len(anchorTags) == 0 {
			// Persisted map with no anchors — fall back to the name as the tag.
			// Saves the user from a "research is empty" UX trap on freshly
			// created maps that haven't picked up anchors yet.
			anchorTags = []string{rec.Name}
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		// Synthesized / tag-derived map. Treat id as the anchor tag.
		mapID = id
		mapName = id
		anchorTags = []string{id}
	} else {
		http.Error(w, "map lookup: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Pull memories carrying ANY of the anchor tags as context. We grab up
	// to 1000 most-recent live memories and filter in Go because the bank
	// stores tags as a JSON array (no efficient SQL index). 1000 is a
	// generous cap — anchor matches usually drop us to <50 rows.
	all, err := s.bank.ListMemoriesWith(MemoryListOpts{Limit: 1000})
	if err != nil {
		http.Error(w, "load memories: "+err.Error(), http.StatusInternalServerError)
		return
	}
	anchorLower := map[string]bool{}
	for _, t := range anchorTags {
		anchorLower[strings.ToLower(strings.TrimSpace(t))] = true
	}
	var contextMems []MemoryRecord
	for _, m := range all {
		for _, t := range m.Tags {
			if anchorLower[strings.ToLower(strings.TrimSpace(t))] {
				contextMems = append(contextMems, m)
				break
			}
		}
		if len(contextMems) >= 60 {
			break // cap prompt size
		}
	}
	memoriesUsed := len(contextMems)

	// Cache lookup keyed by query+map so the same query on different maps
	// produces different cache entries (the prompts diverge — different
	// memory context produces different Tier 3 outputs).
	cacheID := researchIDForMap(mapID, query)
	if cached, err := s.bank.GetResearchCache(cacheID); err == nil && !researchCacheExpired(cached) {
		if entry, ok := decodeResearchEntry(cached); ok {
			// Cache hit short-circuits the spend AND the memory creation —
			// we already minted a memory the first time this query ran for
			// this map. Return the entry without re-firing.
			writeJSON(w, http.StatusOK, mapResearchResponse{
				Entry:    entry,
				CacheHit: true,
				MapID:    mapID,
				MapName:  mapName,
				AnchorTags: anchorTags,
				MemoriesUsedAsContext: memoriesUsed,
			})
			return
		}
	}

	// Egress chokepoint — drops sensitive + LocalConcept rows.
	safe, err := s.router.PrepareOracleCall(OracleRequest{
		Topic:    query,
		Memories: contextMems,
		Prompt:   query,
		Reason:   "user-triggered map research (" + mapName + ")",
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

	// Build the final prompt. Same shape as the generic /research path,
	// just with map context up top so the Oracle knows what it's helping
	// refine.
	finalPrompt := buildMapResearchPrompt(mapName, anchorTags, safe.Prompt, safe.Memories)

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

	tokensIn := estimateTokens(finalPrompt)
	tokensOut := estimateTokens(content)
	tokensTotal := tokensIn + tokensOut

	now := time.Now().UTC()
	ttlHours := body.TTLHours
	if ttlHours <= 0 {
		ttlHours = 24 * 7
	}
	expires := now.Add(time.Duration(ttlHours) * time.Hour)
	entry := researchEntry{
		ID:          cacheID,
		Query:       query,
		Content:     content,
		CachedAt:    now.Format(time.RFC3339Nano),
		ExpiresAt:   expires.Format(time.RFC3339Nano),
		TokensIn:    tokensIn,
		TokensOut:   tokensOut,
		TokensTotal: tokensTotal,
		Model:       safe.Provider.Name(),
	}
	payload, _ := json.Marshal(entry)
	if err := s.bank.SaveResearchCache(ResearchCacheEntry{
		Topic:     cacheID,
		MapID:     mapID,
		Payload:   string(payload),
		FetchedAt: entry.CachedAt,
		ExpiresAt: entry.ExpiresAt,
		FetchedBy: "manual_map",
	}); err != nil {
		log.Printf("map_research: cache save failed: %v", err)
	}

	// Charge Tier 3 budget — same accounting as /research.
	today := now.Format("2006-01-02")
	providerKind := providerKindFromName(safe.Provider.Name())
	if err := s.bank.AddTokensUsedV2(today, string(Tier3), providerKind, safe.Provider.Name(),
		!safe.Provider.IsLocal(), tokensIn, tokensOut); err != nil {
		log.Printf("map_research: budget update failed: %v", err)
	}

	// Mint the augmentation memory. Tags include every anchor tag plus
	// the system-tags that mark this as Oracle-derived. Source=manual_augment
	// matches Phase 6's nightly_augment naming so the map detail's
	// "Recently augmented from research" panel surfaces it automatically.
	// We DON'T set LightEncoded/NightlyConsolidated — leave them false so
	// Phase 0b deep-encodes the new memory next nightly run (the bank's
	// SaveMemory auto-marks dirty=created on insert).
	memTags := append([]string{}, anchorTags...)
	memTags = append(memTags, "manual_augment", "oracle_augmented")
	// Cap the stored text — Tier 3 outputs can run thousands of tokens; the
	// 4000-char cap matches Phase 6's auto-augment policy and keeps the
	// memory body indexable.
	storedText := content
	if len(storedText) > 4000 {
		storedText = storedText[:4000]
	}
	mem, err := s.bank.SaveMemory(MemoryRecord{
		Text:            storedText,
		Tags:            memTags,
		Source:          "manual_augment",
		OracleAugmented: true,
		RegionHint:      "neocortex_research", // research-derived memories cluster in the neocortex bucket
		AdapterID:       adapterIDFromRequest(r),
	})
	if err != nil {
		// Non-fatal — research already cached + budget charged. Return what
		// we have and surface the memory miss in the response so the UI can
		// tell the user the dream pipeline won't pick this up.
		log.Printf("map_research: memory mint failed: %v", err)
		writeJSON(w, http.StatusOK, mapResearchResponse{
			Entry:                 entry,
			SensitiveExcluded:     safe.SensitiveExcluded,
			LocalConceptExcluded:  safe.LocalConceptExcluded,
			MapID:                 mapID,
			MapName:               mapName,
			AnchorTags:            anchorTags,
			MemoriesUsedAsContext: memoriesUsed,
		})
		return
	}

	// Link the new memory to the map's persisted record (if any) so the
	// /maps/{id}/traces query surfaces it. For tag-synthesized maps, the
	// anchor-tag match already wires the memory into the synthesized view
	// — no LinkMapTrace needed.
	if rec, err := s.bank.GetMemoryMap(mapID); err == nil {
		if linkErr := s.bank.LinkMapTrace(rec.ID, mem.ID); linkErr != nil {
			log.Printf("map_research: LinkMapTrace failed (non-fatal): %v", linkErr)
		}
	}

	s.auditWrite(AuditEntry{
		Operation:  "map_research",
		EntityType: "memory_map",
		EntityID:   mapID,
		AfterJSON: fmt.Sprintf(
			`{"map_id":%q,"map_name":%q,"memory_id":%q,"research_id":%q,"query_len":%d,"tokens_in":%d,"tokens_out":%d,"sensitive_excluded":%d,"local_concept_excluded":%d,"memories_used":%d,"elapsed_ms":%d}`,
			mapID, mapName, mem.ID, cacheID, len(query), tokensIn, tokensOut,
			safe.SensitiveExcluded, safe.LocalConceptExcluded, memoriesUsed,
			time.Since(startedAt).Milliseconds(),
		),
		AdapterID: adapterIDFromRequest(r),
		Reason:    "user-triggered map research",
	})

	log.Printf("map_research: map=%s memory=%s tokens=%d model=%s elapsed=%s",
		mapID, mem.ID, tokensTotal, entry.Model, time.Since(startedAt).Round(time.Millisecond))

	writeJSON(w, http.StatusOK, mapResearchResponse{
		Entry:                 entry,
		MemoryID:              mem.ID,
		MemoryTags:            memTags,
		SensitiveExcluded:     safe.SensitiveExcluded,
		LocalConceptExcluded:  safe.LocalConceptExcluded,
		CacheHit:              false,
		MapID:                 mapID,
		MapName:               mapName,
		AnchorTags:            anchorTags,
		MemoriesUsedAsContext: memoriesUsed,
	})
}

// researchIDForMap returns a deterministic short hash combining map id +
// normalised query. Keying on both means re-researching the same prompt
// on a DIFFERENT map produces a fresh cache row (different memory context
// → different output worth caching separately).
func researchIDForMap(mapID, query string) string {
	norm := strings.TrimSpace(strings.ToLower(query))
	h := sha1.Sum([]byte(mapID + "\n" + norm))
	return "research-map-" + fmt.Sprintf("%x", h[:8])
}

// buildMapResearchPrompt composes the Oracle prompt with explicit map
// context up top so the model knows what knowledge gap it's filling.
func buildMapResearchPrompt(mapName string, anchorTags []string, query string, memories []MemoryRecord) string {
	var b strings.Builder
	b.Grow(len(query) + 80*len(memories))
	b.WriteString("You are helping the user refine a MemoryMap called \"")
	b.WriteString(mapName)
	b.WriteString("\" in their personal knowledge graph.\n")
	if len(anchorTags) > 0 {
		b.WriteString("The map is anchored on these tags: ")
		b.WriteString(strings.Join(anchorTags, ", "))
		b.WriteString(".\n")
	}
	b.WriteString("\nResearch the following query, drawing on the user's existing memory context where it is genuinely relevant. Be concise, factual, and cite uncertainty. Output should fill gaps in the user's understanding of this topic — do NOT just restate what's already in the memory context.\n\n")
	b.WriteString("QUERY:\n")
	b.WriteString(query)
	b.WriteString("\n\n")
	if len(memories) > 0 {
		b.WriteString("USER'S MEMORY CONTEXT for this map (sensitive entries already excluded):\n")
		for _, m := range memories {
			text := m.EnrichedText
			if text == "" {
				text = m.Text
			}
			b.WriteString("- ")
			b.WriteString(text)
			b.WriteString("\n")
		}
	} else {
		b.WriteString("(The map currently has no memory context. Provide a general synthesis suitable for seeding this map.)\n")
	}
	return b.String()
}
