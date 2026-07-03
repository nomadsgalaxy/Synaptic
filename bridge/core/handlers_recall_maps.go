// handlers_recall_maps.go — POST /recall/maps.
//
// Map-level recall. Where POST /recall returns the N best-matching
// individual memories, this returns the N best-matching MemoryMaps —
// the higher-level abstractions a query touches. Useful for prompts
// like "Can you recall the project Synaptic-Disorder" where the user
// is asking about a topic, not for a specific experience.
//
// Three signals combine to rank a map:
//
//   1. nameScore   — direct match of the map's name (or any anchor
//                    tag) against the query string. Normalised
//                    (lowercase, strip non-alphanumeric, drop common
//                    filler words like "project", "the", "map"). A
//                    full-substring match scores 1.0; a token-subset
//                    match scores by overlap fraction. This is the
//                    "I asked for it by name" path.
//
//   2. schemaScore — cosine similarity between the query embedding
//                    and an embedding of the map's name + schema_text.
//                    Map embeddings are cached in the existing
//                    embeddings table keyed by sha1 of the embed-text
//                    so subsequent recalls are O(1) lookup per map.
//
//   3. memberScore — aggregate of the map's member memories' cosine
//                    scores. Re-uses the /recall pipeline: rank all
//                    embedded memories, sum the top-K scores of those
//                    that are linked to this map (via map_traces) OR
//                    carry one of its anchor tags.
//
// Hybrid mode (default) blends them:
//   final = max(nameScore, 0.45 * schemaScore + 0.45 * memberScore + 0.10 * nameScore)
//
// The `max` clamp ensures a clean name match always beats a fuzzy
// semantic match — "recall the project X" returns X even if there are
// schema-near misses with higher cosine.
package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

type recallMapsRequest struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
	Mode  string `json:"mode"`              // hybrid|schema|members|name (default: hybrid)
	Type  string `json:"type,omitempty"`    // optional: restrict to a map type
	IncludeProposed bool `json:"include_proposed,omitempty"` // default false — hide proposed unless asked
	// IncludeFullMembers opts the caller into receiving full MemoryRecord
	// rows for the top members of each returned map (in addition to the
	// 160-char excerpts). Off by default — hooks / MCP tools that just
	// want a name + relevance preview shouldn't pay the hydration cost.
	// When on, each result populates TopMembers with up to MembersLimit
	// records (default 5, capped at 20). This is the "dig deeper" path:
	// recall the map, then walk into the actual memories without a
	// second round-trip through /bank/memories/{id}.
	IncludeFullMembers bool `json:"include_full_members,omitempty"`
	MembersLimit       int  `json:"members_limit,omitempty"` // default 5, clamped 1..20
}

type recallMapsResult struct {
	MapID            string   `json:"map_id"`
	Name             string   `json:"name"`
	Type             string   `json:"type"`
	Status           string   `json:"status,omitempty"`
	SchemaText       string   `json:"schema_text,omitempty"`
	AnchorTags       []string `json:"anchor_tags,omitempty"`
	Score            float64  `json:"score"`
	NameScore        float64  `json:"name_score"`
	SchemaScore      float64  `json:"schema_score"`
	MemberScore      float64  `json:"member_score"`
	MemberCount      int      `json:"member_count"`
	TopMemberIDs     []string `json:"top_member_ids,omitempty"`
	TopMemberExcerpts []string `json:"top_member_excerpts,omitempty"`
	// TopMembers carries full MemoryRecord rows when the request set
	// include_full_members=true. Aligned with TopMemberIDs by index.
	// Empty when the opt-in was not requested.
	TopMembers       []MemoryRecord `json:"top_members,omitempty"`
}

type recallMapsResponse struct {
	Query     string             `json:"query"`
	Limit     int                `json:"limit"`
	Count     int                `json:"count"`
	Mode      string             `json:"mode"`
	Results   []recallMapsResult `json:"results"`
}

// Stop-words / filler we strip when normalising a query against map names.
// Keep this tight — too aggressive and "recall the parietal lobe" stops
// matching "parietal lobe."
var recallMapFiller = map[string]bool{
	"recall": true, "remember": true, "the": true, "a": true, "an": true,
	"map": true, "maps": true, "memorymap": true,
	"project": true, "concept": true, "entity": true, "person": true,
	"technology": true, "tech": true, "topic": true,
	"can": true, "could": true, "please": true, "you": true,
	"about": true, "regarding": true, "on": true, "of": true, "for": true,
}

var recallMapNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// normaliseForMapMatch lowercases, strips non-alphanumeric, drops filler
// words, and returns the remaining tokens. "Can you recall the project
// Synaptic-Disorder?" → ["synaptic", "disorder"].
func normaliseForMapMatch(s string) []string {
	lc := strings.ToLower(s)
	parts := recallMapNonAlnum.Split(lc, -1)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" || recallMapFiller[p] {
			continue
		}
		out = append(out, p)
	}
	return out
}

// nameScoreFor computes a 0..1 score for how well the query mentions
// this map's name or any of its anchor tags. Concretely:
//
//   - 1.00 — every query token (after filtering) matches a name token
//            AND every name token has a query token. Exact name match.
//   - 0.90 — every name token appears in the query (query may contain
//            extra context like "tell me about ___"). The natural
//            "recall the project X" case.
//   - 0.50..0.85 — fractional overlap, weighted by name-token coverage.
//   - 0.00 — no tokens overlap.
func nameScoreFor(queryTokens []string, name string, anchorTags []string) float64 {
	candidates := []string{name}
	candidates = append(candidates, anchorTags...)
	best := 0.0
	qset := map[string]bool{}
	for _, t := range queryTokens {
		qset[t] = true
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		nameTokens := normaliseForMapMatch(c)
		if len(nameTokens) == 0 {
			continue
		}
		matched := 0
		for _, nt := range nameTokens {
			if qset[nt] {
				matched++
			}
		}
		if matched == 0 {
			continue
		}
		coverage := float64(matched) / float64(len(nameTokens))
		// Bonus when ALL name tokens are present in the query.
		var score float64
		if matched == len(nameTokens) {
			// Full-name hit. If the query is JUST the name (no extra
			// tokens), score 1.0. If the query has extra context,
			// score 0.9 — still a strong "I'm asking about this map"
			// signal.
			extraQueryTokens := 0
			nameSet := map[string]bool{}
			for _, nt := range nameTokens {
				nameSet[nt] = true
			}
			for qt := range qset {
				if !nameSet[qt] {
					extraQueryTokens++
				}
			}
			if extraQueryTokens == 0 {
				score = 1.0
			} else {
				score = 0.9
			}
		} else {
			// Partial match — coverage × 0.85 caps below the full-name floor.
			score = coverage * 0.85
		}
		if score > best {
			best = score
		}
	}
	return best
}

// embedMapText returns an embedding for the map's "searchable text" —
// name + schema_text concatenated. Lazily caches via the existing
// embeddings table keyed by sha1 hash of the embed text. Returns nil
// without error when the map has no schema (caller treats schemaScore
// as 0 in that case). The label persisted into bank.embeddings.model
// is the bare model string (no "kind:" prefix) so AllEmbeddingsForMemories
// can find these rows alongside synapse-builder + /recall entries.
func (s *Server) embedMapText(m MemoryMap, provider LLMProvider) []float32 {
	if provider == nil {
		return nil
	}
	corpus := strings.TrimSpace(m.Name)
	if m.SchemaText != "" {
		if corpus != "" {
			corpus += " — "
		}
		corpus += strings.TrimSpace(m.SchemaText)
	}
	if corpus == "" {
		return nil
	}
	h := synapseTextHash(corpus)
	if vec, err := s.bank.GetEmbedding(h); err == nil && vec != nil {
		return vec
	}
	// Cache miss — embed via the router's provider and persist.
	// Use a fresh background context; the per-request context belongs
	// to the recall handler but caching shouldn't block the request
	// timeout.  Cap text length so a huge schema doesn't tank a recall.
	if len(corpus) > 4000 {
		corpus = corpus[:4000]
	}
	vec, err := provider.Embed(context.Background(), corpus)
	if err != nil {
		log.Printf("recall_maps: embed map %s failed (non-fatal): %v", m.ID, err)
		return nil
	}
	label := providerModelLabel(provider)
	if perr := s.bank.SaveEmbedding(h, vec, label); perr != nil {
		log.Printf("recall_maps: SaveEmbedding map %s failed (non-fatal): %v", m.ID, perr)
	}
	return vec
}

// handleRecallMaps serves POST /recall/maps.
func (s *Server) handleRecallMaps(w http.ResponseWriter, r *http.Request) {
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

	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req recallMapsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		http.Error(w, `"query" is required`, http.StatusBadRequest)
		return
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = "hybrid"
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	// Route through the router for provider-agnostic embedding (Ollama,
	// openai_local, etc.). Falling back to name-only mode if no provider
	// is configured preserves the "Ollama down" behaviour from before.
	var embedProvider LLMProvider
	var model string
	if s.router != nil {
		embedProvider = s.router.ForEmbedding()
	}
	if embedProvider != nil {
		model = providerModelLabel(embedProvider)
	}

	// 1. Query embedding (only needed for schema/hybrid/members modes).
	var queryVec []float32
	if mode != "name" {
		if embedProvider == nil {
			log.Printf("recall_maps: no embedding provider configured, falling back to name-only")
			mode = "name"
		} else {
			qv, err := recallEmbedQuery(r.Context(), s.bank, req.Query, embedProvider)
			if err != nil {
				// Fall back to name-only mode if embeddings are unavailable. Lets
				// "recall the project X" still work even with the embed provider down.
				log.Printf("recall_maps: query embed failed, falling back to name-only: %v", err)
				mode = "name"
			} else {
				queryVec = qv
			}
		}
	}

	// 2. Pull eligible maps. Default: accepted only; the user can flip
	//    include_proposed=true to also rank proposed maps.
	listOpts := MapListOpts{Limit: 1000, Type: req.Type}
	if !req.IncludeProposed {
		listOpts.Status = "accepted"
	}
	persisted, err := s.bank.ListMemoryMapsWith(listOpts)
	if err != nil {
		http.Error(w, "list maps: "+err.Error(), http.StatusInternalServerError)
		return
	}
	maps := append([]MemoryMap{}, persisted...)

	// 2b. Synthesize tag-cluster maps. The frontend's _buildConceptMaps()
	//     does this at runtime — every tag with ≥3 memories is treated as
	//     an implicit "concept" map (id=tag, name=tag). The user expects
	//     "recall the project Synaptic-Disorder" to match these synthesized
	//     maps too, not just persisted memory_maps rows. We dedup against
	//     persisted maps by name (case-insensitive) so a tag with a real
	//     map row doesn't appear twice.
	if req.Type == "" || req.Type == "concept" {
		persistedNames := map[string]bool{}
		for _, m := range persisted {
			persistedNames[strings.ToLower(strings.TrimSpace(m.Name))] = true
		}
		// Cheap: walk memories once, tally tags.
		mems, err := s.bank.ListMemoriesWith(MemoryListOpts{Limit: 10000})
		if err == nil {
			tagFreq := map[string]int{}
			for _, m := range mems {
				for _, t := range m.Tags {
					tt := strings.TrimSpace(t)
					if tt == "" {
						continue
					}
					tagFreq[tt]++
				}
			}
			for tag, n := range tagFreq {
				if n < 3 {
					continue // too small to count as a concept map
				}
				lc := strings.ToLower(tag)
				if persistedNames[lc] {
					continue // already covered by a persisted map row
				}
				// Skip structural tags that aren't user-meaningful concepts.
				if strings.HasPrefix(lc, "memory_type:") ||
					strings.HasPrefix(lc, "imported_at:") ||
					lc == "manual_augment" || lc == "oracle_augmented" ||
					lc == "augment" {
					continue
				}
				maps = append(maps, MemoryMap{
					ID:         tag, // synthesized maps use the tag as their id
					Name:       tag,
					Type:       "concept",
					Status:     "accepted",
					AnchorTags: []string{tag},
				})
			}
		}
	}

	// 3. For the memberScore pass: rank all memories via the standard
	//    cosine path. We only need this once per request; we then walk
	//    each map's anchor tags + map_traces links to aggregate.
	var memScoreByID map[string]float64
	if mode == "members" || mode == "hybrid" {
		memVecs, err := s.bank.AllEmbeddingsForMemories(model)
		if err != nil {
			log.Printf("recall_maps: load embeddings failed (members pass skipped): %v", err)
		} else if queryVec != nil {
			memScoreByID = make(map[string]float64, len(memVecs))
			for id, vec := range memVecs {
				memScoreByID[id] = recallScore(queryVec, vec)
			}
		}
	}

	// Pre-load every map's trace links (only the persisted ones have them;
	// tag-derived synthesized maps don't, so we'll fall back to anchor-tag
	// scan for those).
	traceIDsByMap := map[string][]string{}
	if mode == "members" || mode == "hybrid" {
		for _, m := range maps {
			if m.ID == "" {
				continue
			}
			ids, err := s.bank.ListTracesForMap(m.ID, 200)
			if err == nil && len(ids) > 0 {
				traceIDsByMap[m.ID] = ids
			}
		}
	}
	// Also pre-load anchor-tag → member-id index for maps with no
	// map_traces rows. Cheap because we already iterate memories in the
	// AllEmbeddingsForMemories step, but we need the tags too.
	var membersByTag map[string][]string
	if (mode == "members" || mode == "hybrid") && memScoreByID != nil {
		memsAll, err := s.bank.ListMemoriesWith(MemoryListOpts{Limit: 10000})
		if err == nil {
			membersByTag = map[string][]string{}
			for _, m := range memsAll {
				for _, t := range m.Tags {
					lc := strings.ToLower(strings.TrimSpace(t))
					if lc == "" {
						continue
					}
					membersByTag[lc] = append(membersByTag[lc], m.ID)
				}
			}
		}
	}

	queryTokens := normaliseForMapMatch(req.Query)
	results := make([]recallMapsResult, 0, len(maps))
	for _, m := range maps {
		nameScore := nameScoreFor(queryTokens, m.Name, m.AnchorTags)

		// Schema cosine — only when the map has a real schema_text. For
		// synthesized tag-cluster maps (schema_text=""), the cosine of
		// just-the-tag-name vs query is noisy AND embedding hundreds of
		// short strings on a cold cache makes the first request take
		// 30+ seconds. Synthesized maps lean on nameScore + memberScore
		// instead — both of which the user actually cares about for
		// "recall the project X" queries.
		schemaScore := 0.0
		if (mode == "schema" || mode == "hybrid") && queryVec != nil && strings.TrimSpace(m.SchemaText) != "" {
			if vec := s.embedMapText(m, embedProvider); vec != nil {
				schemaScore = recallScore(queryVec, vec)
				// Cosine is technically [-1, 1] for unit vectors. Clamp to
				// [0, 1] so it composes cleanly with name/member scores
				// (negative cosine = irrelevant, not anti-relevant).
				if schemaScore < 0 {
					schemaScore = 0
				}
			}
		}

		// Member aggregation — sum the top-K member scores. Members are
		// resolved from map_traces (persisted maps) OR anchor-tag match
		// (synthesized maps).
		memberScore := 0.0
		memberCount := 0
		var topMemberIDs []string
		if (mode == "members" || mode == "hybrid") && memScoreByID != nil {
			memberIDs := traceIDsByMap[m.ID]
			if len(memberIDs) == 0 && membersByTag != nil {
				// Synthesized / tag-derived map — collect by anchor tag.
				seen := map[string]bool{}
				for _, anchor := range m.AnchorTags {
					for _, mid := range membersByTag[strings.ToLower(strings.TrimSpace(anchor))] {
						if !seen[mid] {
							seen[mid] = true
							memberIDs = append(memberIDs, mid)
						}
					}
				}
				// Also try the map's name itself as an implicit anchor —
				// synthesized maps where id===name===tag.
				for _, mid := range membersByTag[strings.ToLower(strings.TrimSpace(m.Name))] {
					if !seen[mid] {
						seen[mid] = true
						memberIDs = append(memberIDs, mid)
					}
				}
			}
			memberCount = len(memberIDs)
			// Aggregate: sum the top-5 member scores (so a 50-member map
			// doesn't trivially outscore a 5-member map on count alone).
			scored := make([]float64, 0, len(memberIDs))
			scoredID := make([]string, 0, len(memberIDs))
			for _, mid := range memberIDs {
				if sc, ok := memScoreByID[mid]; ok && sc > 0 {
					scored = append(scored, sc)
					scoredID = append(scoredID, mid)
				}
			}
			// Sort indices by score desc; keep the IDs aligned.
			idx := make([]int, len(scored))
			for i := range idx {
				idx[i] = i
			}
			sort.Slice(idx, func(i, j int) bool { return scored[idx[i]] > scored[idx[j]] })
			topK := 5
			if len(idx) < topK {
				topK = len(idx)
			}
			for k := 0; k < topK; k++ {
				memberScore += scored[idx[k]]
				topMemberIDs = append(topMemberIDs, scoredID[idx[k]])
			}
			// Normalise: divide by topK so memberScore is a per-member
			// average in [0,1] rather than an unbounded sum.
			if topK > 0 {
				memberScore /= float64(topK)
			}
		}

		var finalScore float64
		switch mode {
		case "name":
			finalScore = nameScore
		case "schema":
			finalScore = schemaScore
		case "members":
			finalScore = memberScore
		default: // "hybrid"
			blend := 0.45*schemaScore + 0.45*memberScore + 0.10*nameScore
			// max(nameScore, blend) — strong name match always wins so
			// "recall the project X" returns X.
			if nameScore > blend {
				finalScore = nameScore
			} else {
				finalScore = blend
			}
		}

		if finalScore <= 0.001 {
			continue
		}

		results = append(results, recallMapsResult{
			MapID:        m.ID,
			Name:         m.Name,
			Type:         m.Type,
			Status:       m.Status,
			SchemaText:   m.SchemaText,
			AnchorTags:   m.AnchorTags,
			Score:        finalScore,
			NameScore:    nameScore,
			SchemaScore:  schemaScore,
			MemberScore:  memberScore,
			MemberCount:  memberCount,
			TopMemberIDs: topMemberIDs,
		})
	}

	// Sort + truncate.
	sort.Slice(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	if len(results) > limit {
		results = results[:limit]
	}

	// Hydrate top_member_excerpts for the returned set so the caller can
	// preview the map's relevance without a second round-trip.
	//
	// When req.IncludeFullMembers is set, ALSO hydrate full MemoryRecord
	// rows into TopMembers. This is the "dig deeper" entry point: hooks
	// and MCP tools that need to act on the actual memories (not just
	// know they exist) get them in one round-trip.
	fullMembersLimit := 0
	if req.IncludeFullMembers {
		fullMembersLimit = req.MembersLimit
		if fullMembersLimit <= 0 {
			fullMembersLimit = 5
		}
		if fullMembersLimit > 20 {
			fullMembersLimit = 20
		}
	}
	for i := range results {
		if len(results[i].TopMemberIDs) == 0 {
			continue
		}
		excerpts := make([]string, 0, len(results[i].TopMemberIDs))
		var fullMembers []MemoryRecord
		if fullMembersLimit > 0 {
			fullMembers = make([]MemoryRecord, 0, fullMembersLimit)
		}
		for idx, mid := range results[i].TopMemberIDs {
			rec, err := s.bank.GetMemory(mid)
			if err != nil {
				continue
			}
			txt := rec.EnrichedText
			if txt == "" {
				txt = rec.Text
			}
			if len(txt) > 160 {
				txt = txt[:160] + "…"
			}
			excerpts = append(excerpts, txt)
			if fullMembersLimit > 0 && idx < fullMembersLimit {
				fullMembers = append(fullMembers, rec)
			}
		}
		results[i].TopMemberExcerpts = excerpts
		results[i].TopMembers = fullMembers
	}

	writeJSON(w, http.StatusOK, recallMapsResponse{
		Query:   req.Query,
		Limit:   limit,
		Count:   len(results),
		Mode:    mode,
		Results: results,
	})
}
