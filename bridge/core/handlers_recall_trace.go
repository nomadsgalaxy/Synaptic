// handlers_recall_trace.go — Iter 25 (2026-05-14) diagnostic endpoint.
//
// `/admin/recall-trace` runs the retrieval pipeline (rankCandidates) for
// a query and returns a structured trace of every intermediate decision
// without ever calling Tier 3. Used to debug why a memory wasn't
// retrieved, which maps matched, and how the fused ranking landed.
//
// No mutations. No Tier 3 calls (free). Bearer-token gated like the
// rest of /admin/*.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
)

type recallTraceRequest struct {
	Query  string `json:"query"`
	Limit  int    `json:"limit,omitempty"`
	Budget string `json:"budget,omitempty"`
	// Show the top-N candidates in the response. Defaults to 30.
	ShowTopN int `json:"show_top_n,omitempty"`
}

type recallTraceCandidate struct {
	ID            string   `json:"id"`
	Score         float64  `json:"score"`
	Tags          []string `json:"tags,omitempty"`
	SessionID     string   `json:"session_id,omitempty"`
	TextPreview   string   `json:"text_preview"`
	OptimizedText string   `json:"optimized_text,omitempty"`
	EnrichedText  string   `json:"enriched_text,omitempty"`
}

type recallTraceMapMatch struct {
	MapID       string   `json:"map_id"`
	Name        string   `json:"name"`
	AnchorTags  []string `json:"anchor_tags"`
	SchemaText  string   `json:"schema_text,omitempty"`
	CosineScore float64  `json:"cosine_score"`
	TraceCount  int      `json:"trace_count"`
	TraceIDs    []string `json:"trace_ids"`
}

type recallTraceResponse struct {
	Query             string                 `json:"query"`
	EmbeddingModel    string                 `json:"embedding_model"`
	QuerySimplified   string                 `json:"query_simplified,omitempty"`
	TotalMemories     int                    `json:"total_memories"`
	TotalMaps         int                    `json:"total_maps"`
	MapsWithEmbedding int                    `json:"maps_with_embedding"`
	CosineTopK        []recallTraceCandidate `json:"cosine_top_k"`
	MapAugment        struct {
		Enabled         bool                  `json:"enabled"`
		MatchedMaps     []recallTraceMapMatch `json:"matched_maps"`
		ExpandedAddedN  int                   `json:"expanded_added_n"`
		ExpandedFromN   int                   `json:"expanded_from_n"`
		CosineCutoffUsed float64              `json:"cosine_cutoff_used"`
	} `json:"map_augment"`
	FinalRanking []recallTraceCandidate `json:"final_ranking"`
	ElapsedMs    int64                  `json:"elapsed_ms"`
}

func (s *Server) postAdminRecallTrace(w http.ResponseWriter, r *http.Request) {
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
	if s.router == nil {
		http.Error(w, "router not configured", http.StatusServiceUnavailable)
		return
	}
	var body recallTraceRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Query) == "" {
		http.Error(w, "query is required", http.StatusBadRequest)
		return
	}
	if body.ShowTopN <= 0 {
		body.ShowTopN = 30
	}

	t0 := time.Now()
	out := recallTraceResponse{Query: body.Query}

	embedProvider := s.router.ForEmbedding()
	if embedProvider == nil {
		http.Error(w, "no embedding provider", http.StatusServiceUnavailable)
		return
	}
	out.EmbeddingModel = providerModelLabel(embedProvider)

	// Simplified query (Bundle I).
	if querySimplifyEnabled(s.bank) {
		if simplified := simplifyQueryForEmbedding(body.Query); simplified != body.Query {
			out.QuerySimplified = simplified
		}
	}

	// Counts.
	if vecs, err := s.bank.AllEmbeddingsForMemories(out.EmbeddingModel); err == nil {
		out.TotalMemories = len(vecs)
	}
	if maps, err := s.bank.ListMemoryMaps("", 1000); err == nil {
		out.TotalMaps = len(maps)
	}
	if mapVecs, err := s.bank.LoadMapEmbeddings(out.EmbeddingModel); err == nil {
		out.MapsWithEmbedding = len(mapVecs)
	}

	// Run the cosine pass directly (we re-do the work from
	// rankCandidates instead of capturing it via a side channel because
	// the existing function returns only the final fused list).
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	queryVec, err := recallEmbedQuery(ctx, s.bank, body.Query, embedProvider)
	if err != nil {
		http.Error(w, "embed query: "+err.Error(), http.StatusInternalServerError)
		return
	}
	memVecs, err := s.bank.AllEmbeddingsForMemories(out.EmbeddingModel)
	if err != nil {
		http.Error(w, "load embeddings: "+err.Error(), http.StatusInternalServerError)
		return
	}
	type sc struct {
		id    string
		score float64
	}
	cosScored := make([]sc, 0, len(memVecs))
	for id, v := range memVecs {
		cosScored = append(cosScored, sc{id: id, score: recallScore(queryVec, v)})
	}
	sort.Slice(cosScored, func(i, j int) bool { return cosScored[i].score > cosScored[j].score })

	// Build cosine top-K view with text previews + facts + dream summary.
	topN := body.ShowTopN
	if topN > len(cosScored) {
		topN = len(cosScored)
	}
	topIDs := make([]string, topN)
	for i := 0; i < topN; i++ {
		topIDs[i] = cosScored[i].id
	}
	optTexts, _ := s.bank.GetOptimizedTextBatch(topIDs)
	for i := 0; i < topN; i++ {
		mem, gerr := s.bank.GetMemory(cosScored[i].id)
		c := recallTraceCandidate{ID: cosScored[i].id, Score: cosScored[i].score}
		if gerr == nil {
			c.Tags = mem.Tags
			c.SessionID = mem.SessionID
			c.TextPreview = truncateForTrace(mem.Text, 200)
			c.EnrichedText = truncateForTrace(mem.EnrichedText, 200)
		}
		if ot, ok := optTexts[c.ID]; ok {
			c.OptimizedText = truncateForTrace(ot, 200)
		}
		out.CosineTopK = append(out.CosineTopK, c)
	}

	// Map augment trace.
	out.MapAugment.Enabled = mapAugmentEnabled(s.bank)
	out.MapAugment.CosineCutoffUsed = 0.45
	if out.MapAugment.Enabled {
		mapVecs, _ := s.bank.LoadMapEmbeddings(out.EmbeddingModel)
		type ms struct {
			id    string
			score float64
		}
		mapRanks := make([]ms, 0, len(mapVecs))
		for id, v := range mapVecs {
			mapRanks = append(mapRanks, ms{id: id, score: recallScore(queryVec, v)})
		}
		sort.Slice(mapRanks, func(i, j int) bool { return mapRanks[i].score > mapRanks[j].score })
		// Show top-10 maps in the trace regardless of cutoff so the user
		// can see borderline matches.
		showMaps := 10
		if showMaps > len(mapRanks) {
			showMaps = len(mapRanks)
		}
		expanded := map[string]bool{}
		for i := 0; i < showMaps; i++ {
			mp, gerr := s.bank.GetMemoryMap(mapRanks[i].id)
			match := recallTraceMapMatch{
				MapID:       mapRanks[i].id,
				CosineScore: mapRanks[i].score,
			}
			if gerr == nil {
				match.Name = mp.Name
				match.AnchorTags = mp.AnchorTags
				match.SchemaText = truncateForTrace(mp.SchemaText, 200)
			}
			tids, _ := s.bank.ListTracesForMap(mapRanks[i].id, 200)
			match.TraceCount = len(tids)
			match.TraceIDs = tids
			out.MapAugment.MatchedMaps = append(out.MapAugment.MatchedMaps, match)
			if mapRanks[i].score >= out.MapAugment.CosineCutoffUsed {
				for _, tid := range tids {
					expanded[tid] = true
				}
			}
		}
		// Count how many of the expanded are NEW vs already in cosine top-K.
		cosineSeen := map[string]bool{}
		for _, c := range out.CosineTopK {
			cosineSeen[c.ID] = true
		}
		newAdded := 0
		for id := range expanded {
			if !cosineSeen[id] {
				newAdded++
			}
		}
		out.MapAugment.ExpandedAddedN = newAdded
		out.MapAugment.ExpandedFromN = len(expanded)
	}

	// Compute the final fused ranking the way rankCandidates would
	// (post-BM25 + map-augment + temporal + session-expand). The easiest
	// way to get this consistent with prod is to actually call
	// rankCandidates and then collect the IDs.
	rcCtx, rcCancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer rcCancel()
	finalScored, rerr := rankCandidates(rcCtx, s.bank, s.router, body.Query, 3)
	if rerr == nil {
		showFinal := body.ShowTopN
		if showFinal > len(finalScored) {
			showFinal = len(finalScored)
		}
		ids := make([]string, showFinal)
		for i := 0; i < showFinal; i++ {
			ids[i] = finalScored[i].ID
		}
		optFinal, _ := s.bank.GetOptimizedTextBatch(ids)
		for i := 0; i < showFinal; i++ {
			mem, gerr := s.bank.GetMemory(finalScored[i].ID)
			c := recallTraceCandidate{ID: finalScored[i].ID, Score: finalScored[i].Score}
			if gerr == nil {
				c.Tags = mem.Tags
				c.SessionID = mem.SessionID
				c.TextPreview = truncateForTrace(mem.Text, 200)
				c.EnrichedText = truncateForTrace(mem.EnrichedText, 200)
			}
			if ot, ok := optFinal[c.ID]; ok {
				c.OptimizedText = truncateForTrace(ot, 200)
			}
			out.FinalRanking = append(out.FinalRanking, c)
		}
	}

	out.ElapsedMs = time.Since(t0).Milliseconds()

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		http.Error(w, "encode: "+err.Error(), http.StatusInternalServerError)
		return
	}
}

func truncateForTrace(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// silence unused-import lint when this file is the only consumer.
var _ = errors.New
