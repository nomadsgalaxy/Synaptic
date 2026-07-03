// recall.go — POST /recall semantic-search endpoint (P5-1).
//
// Embeds the query via the router's embedding provider (same provider used
// by SynapseBuilder, embed-all, and the nightly pipeline phases), computes
// cosine similarity against all bank memories with cached embeddings, and
// returns top-N results sorted by score descending.
//
// map_hint is accepted but not acted on — that boost requires the memory_maps
// schema from P5-2. A debug log line is emitted when map_hint is non-empty.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ── Types ──────────────────────────────────────────────────────────────────

type recallRequest struct {
	Query   string `json:"query"`
	Limit   int    `json:"limit"`
	MapHint string `json:"map_hint"`
	// R10 bi-temporal supersede knobs (v2.4.0b1). Both default to current-truth.
	//   IncludeSuperseded=true       → recall returns superseded memories too
	//   AsOf="2025-01-01T00:00:00Z"  → recall returns the truth as of that ISO timestamp
	IncludeSuperseded bool   `json:"include_superseded,omitempty"`
	AsOf              string `json:"as_of,omitempty"`
	// v2.7 Bundle L — Dormant Memories. Default /recall excludes dormant
	// rows (same shape as supersede filter); flip this to true to surface
	// them. Used by the dashboard's "Show dormant" toggle and by callers
	// that explicitly want to wake old memories up.
	IncludeDormant bool `json:"include_dormant,omitempty"`
}

type recallResult struct {
	Memory MemoryRecord `json:"memory"`
	Score  float64      `json:"score"`
}

type recallResponse struct {
	Query   string         `json:"query"`
	Limit   int            `json:"limit"`
	Count   int            `json:"count"`
	Results []recallResult `json:"results"`
	// SupersededExcluded surfaces how many results were filtered out by the
	// R10 current-truth view. Lets the UI offer an "X superseded memories
	// hidden — include them?" toggle inline.
	SupersededExcluded int `json:"superseded_excluded,omitempty"`
	// DormantExcluded — Bundle L counterpart. Same UX pattern: surface the
	// count so the UI can offer "X dormant memories hidden — wake them?"
	DormantExcluded int `json:"dormant_excluded,omitempty"`
}

// ── HTTP handler ───────────────────────────────────────────────────────────

func (s *Server) postRecall(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.bank == nil {
		http.Error(w, "bank disabled — recall requires SD_BANK_PATH", http.StatusServiceUnavailable)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req recallRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Query == "" {
		http.Error(w, `"query" is required`, http.StatusBadRequest)
		return
	}
	if req.Limit <= 0 {
		req.Limit = 10
	}
	if req.Limit > 100 {
		req.Limit = 100
	}
	if req.MapHint != "" {
		log.Printf("recall: map_hint=%q received but not yet applied (requires P5-2)", req.MapHint)
	}

	// Route embedding through the router rather than re-constructing an
	// OllamaProvider from env vars (which broke whenever SD_OLLAMA_URL
	// wasn't set — e.g. when Tier 1 is an openai_local provider).
	// providerModelLabel() returns the bare model string ("llama3.2:3b")
	// so AllEmbeddingsForMemories can match rows already stored under
	// that key.
	embedProvider := s.router.ForEmbedding()
	if embedProvider == nil {
		http.Error(w, "embedding provider not configured", http.StatusServiceUnavailable)
		return
	}
	model := providerModelLabel(embedProvider)

	// v2.7 Bundle I lesson #1 — query simplification now lives inside
	// recallEmbedQuery so /reflect benefits too. embedQuery used to
	// be the post-simplification text; we still need it for BM25 fusion
	// below, so compute it here for that use only.
	embedQuery := req.Query
	if querySimplifyEnabled(s.bank) {
		if simplified := simplifyQueryForEmbedding(req.Query); simplified != req.Query {
			embedQuery = simplified
		}
	}
	queryVec, err := recallEmbedQuery(r.Context(), s.bank, req.Query, embedProvider)
	if err != nil {
		http.Error(w, "embed query: "+err.Error(), http.StatusInternalServerError)
		return
	}

	memVecs, err := s.bank.AllEmbeddingsForMemories(model)
	if err != nil {
		http.Error(w, "load embeddings: "+err.Error(), http.StatusInternalServerError)
		return
	}

	type scored struct {
		id    string
		score float64
	}
	scores := make([]scored, 0, len(memVecs))
	for id, vec := range memVecs {
		scores = append(scores, scored{id: id, score: recallScore(queryVec, vec)})
	}
	sort.Slice(scores, func(i, j int) bool { return scores[i].score > scores[j].score })

	// v2.7 Bundle J — hybrid retrieval. Fuse cosine + BM25 via
	// Reciprocal Rank Fusion. The cosine list is already sorted above;
	// the BM25 list comes from the FTS5 virtual table. RRF re-ranks
	// the union — IDs in both lists win biggest; pure-keyword and
	// pure-semantic hits get partial credit.
	if hybridEnabled(s.bank) {
		// v2.7 Bundle I lesson #2 — apply the same interrogative strip
		// to the BM25 query. Without this, FTS5 ranks documents heavy in
		// the question-word stop-set (what / does / in / the / a) above
		// keyword-relevant matches, which dominates RRF fusion. Use the
		// already-simplified embedQuery so cosine + BM25 see the same
		// content tokens.
		bm25Query := embedQuery
		// v2.7 Bundle N — alias expansion. If any query token resolves to
		// a canonical entity name, OR it into the BM25 query so e.g.
		// "what does AC pay?" matches memories tagged with "Anthropic".
		if expansions, eerr := s.bank.ExpandQueryViaAliases(req.Query); eerr == nil && len(expansions) > 0 {
			bm25Query = bm25Query + " " + strings.Join(expansions, " ")
		} else if eerr != nil {
			log.Printf("recall: alias expansion failed: %v (continuing with raw query)", eerr)
		}
		bm25IDs, berr := s.bank.SearchBM25(bm25Query, 200)
		if berr != nil {
			// Non-fatal: degrade to cosine-only with a log.
			log.Printf("recall: BM25 search failed, falling back to cosine-only: %v", berr)
		} else if len(bm25IDs) > 0 {
			cosineIDs := make([]string, len(scores))
			for i, sc := range scores {
				cosineIDs[i] = sc.id
			}
			rrfK := hybridRRFK(s.bank)
			fused := ReciprocalRankFusion([][]string{cosineIDs, bm25IDs}, rrfK, 0)
			// Replace scores with the fused order. Items unique to BM25
			// (not in the cosine list) join with whatever score RRF
			// computed; cosine-similarity is lost for those — that's
			// fine since we're sorting by the fused score, not raw
			// cosine.
			newScores := make([]scored, 0, len(fused))
			cosineScoreByID := make(map[string]float64, len(scores))
			for _, sc := range scores {
				cosineScoreByID[sc.id] = sc.score
			}
			for _, f := range fused {
				// Use fused score for ordering; keep the original cosine
				// score in the response (more interpretable for callers).
				cs := cosineScoreByID[f.ID]
				newScores = append(newScores, scored{id: f.ID, score: cs})
			}
			scores = newScores
		}
	}

	// v2.7 Bundle I lesson #3 — temporal-aware retrieval. When the
	// query carries an unambiguous temporal marker, fuse a third list
	// into the candidates: the topical pre-filter sorted by created_at
	// in the direction the marker implies. Genuine user benefit on
	// queries like "when did I first start running?" — without this,
	// cosine returns the most-similar running memory, which is usually
	// the most-DETAILED, not the OLDEST.
	if temporalRecallEnabled(s.bank) && len(scores) > 0 {
		if dir := detectTemporalIntent(req.Query); dir != "" {
			// Pre-filter to memories the topical retrieval already
			// surfaced — temporal alone over 9k memories would return
			// arbitrary old/new rows. Top-50 from fusion is enough
			// headroom for the temporal sort to surface the right one.
			topN := len(scores)
			if topN > 50 {
				topN = 50
			}
			candidateIDs := make([]string, topN)
			for i := 0; i < topN; i++ {
				candidateIDs[i] = scores[i].id
			}
			temporalIDs, terr := recallTemporalIDs(s.bank, candidateIDs, dir, topN)
			if terr != nil {
				log.Printf("recall: temporal sort failed (%s): %v", dir, terr)
			} else if len(temporalIDs) > 0 {
				existing := make([]string, len(scores))
				cosineScoreByID := make(map[string]float64, len(scores))
				for i, sc := range scores {
					existing[i] = sc.id
					cosineScoreByID[sc.id] = sc.score
				}
				fused := ReciprocalRankFusion([][]string{existing, temporalIDs},
					hybridRRFK(s.bank), 0)
				newScores := make([]scored, 0, len(fused))
				for _, f := range fused {
					newScores = append(newScores, scored{
						id: f.ID, score: cosineScoreByID[f.ID],
					})
				}
				scores = newScores
				log.Printf("recall: temporal-fused %d candidates (dir=%s)", len(temporalIDs), dir)
			}
		}
	}

	// v2.7 Bundle I lesson #4 — session co-retrieval. Multi-turn
	// conversations live as multiple memories with the same session_id.
	// When the user asks a question whose answer needs MULTIPLE turns
	// from the same conversation ("how many days did I spend camping?"
	// needs the 5-day + 3-day turns added together), our top-K pulls
	// only the highest-cosine turn. Promote the other turns from the
	// same session so the synthesizer sees full conversation context.
	// Conservative: only expand the top-3 sessions, and only add up to
	// 3 siblings per session. Caps the candidate count growth.
	if sessionExpandEnabled(s.bank) && len(scores) > 0 {
		topN := 3
		if topN > len(scores) {
			topN = len(scores)
		}
		seen := make(map[string]bool, len(scores))
		for _, sc := range scores {
			seen[sc.id] = true
		}
		seenSessions := make(map[string]bool, topN)
		extras := []string{}
		// Look up each top-N memory to read its session_id.
		for i := 0; i < topN; i++ {
			rec, gerr := s.bank.GetMemory(scores[i].id)
			if gerr != nil || rec.SessionID == "" || seenSessions[rec.SessionID] {
				continue
			}
			seenSessions[rec.SessionID] = true
			siblings, serr := s.bank.MemoriesInSession(rec.SessionID, 10)
			if serr != nil {
				continue
			}
			added := 0
			for _, sib := range siblings {
				if seen[sib.ID] || added >= 3 {
					continue
				}
				extras = append(extras, sib.ID)
				seen[sib.ID] = true
				added++
			}
		}
		if len(extras) > 0 {
			// Append as low-rank entries so they ride into the response
			// without displacing the top-K hits. Score them at 0 — the
			// rerank (Bundle Q) can promote them based on salience if
			// they're genuinely relevant; otherwise the synthesizer
			// just gets richer context.
			for _, id := range extras {
				scores = append(scores, scored{id: id, score: 0})
			}
			log.Printf("recall: session-expanded +%d sibling memories from top-%d sessions",
				len(extras), len(seenSessions))
		}
	}

	// R10 supersede filter: default current-truth view excludes superseded
	// ids. Compute once over the bank; the map is O(K) for K = total edges,
	// typically tiny. We filter BEFORE slicing to req.Limit so the user
	// always gets up to Limit *current-truth* results, not a truncated set
	// with most slots filled by old memories.
	supersededExcluded := 0
	if !req.IncludeSuperseded {
		hidden, err := s.bank.SupersededIDsAt(req.AsOf)
		if err != nil {
			log.Printf("recall: SupersededIDsAt: %v (continuing without supersede filter)", err)
		} else if len(hidden) > 0 {
			kept := scores[:0]
			for _, sc := range scores {
				if _, drop := hidden[sc.id]; drop {
					supersededExcluded++
					continue
				}
				kept = append(kept, sc)
			}
			scores = kept
		}
	}

	// v2.7 Bundle Q — second-pass rerank on the post-fusion top-K. Off by
	// default; opt-in via `recall_rerank_enabled`. The default reranker is
	// a no-I/O salience/cosine/recall_strength blend; a cross-encoder can
	// be wired in by setting s.reranker before this runs.
	if rerankEnabled(s.bank) && len(scores) > 0 {
		k := rerankTopK(s.bank)
		if k > len(scores) {
			k = len(scores)
		}
		// Build candidates from the top-K of the current scores.
		cands := make([]rerankCandidate, k)
		for i := 0; i < k; i++ {
			rec, gerr := s.bank.GetMemory(scores[i].id)
			if gerr != nil {
				// Bail on rerank entirely — fall through to the unmodified
				// scores. We don't want a single bad GET to drop results.
				cands = nil
				break
			}
			cands[i] = rerankCandidate{
				ID:             scores[i].id,
				CosineScore:    scores[i].score,
				Salience:       rec.Salience,
				RecallStrength: rec.RecallStrength,
				Text:           rec.Text,
			}
		}
		if cands != nil {
			rr := s.reranker
			if rr == nil {
				rr = salienceReranker{}
			}
			reranked, rerr := rr.Rerank(r.Context(), req.Query, cands)
			if rerr != nil {
				log.Printf("recall: reranker failed, keeping fused order: %v", rerr)
			} else if len(reranked) == len(cands) {
				// Splice the reranked top-K back into scores. Items beyond
				// the top-K keep their original relative order — they're
				// unlikely to make the response window anyway since the
				// rerank typically promotes a few from positions 5–20 into
				// the top-N.
				cosByID := make(map[string]float64, len(scores))
				for _, sc := range scores {
					cosByID[sc.id] = sc.score
				}
				newScores := make([]scored, 0, len(scores))
				for _, rc := range reranked {
					newScores = append(newScores, scored{id: rc.ID, score: cosByID[rc.ID]})
				}
				// Append the tail (items beyond K) in their original order.
				for i := k; i < len(scores); i++ {
					newScores = append(newScores, scores[i])
				}
				scores = newScores
			}
		}
	}

	// v2.7 Bundle L — dormancy filter. Default view hides dormant_at != ''
	// rows; opt-in via include_dormant=true. Filter on the metadata column
	// (cheap lookup) BEFORE slicing to req.Limit so callers get up to Limit
	// awake memories, not a truncated set padded with sleepers. Best-effort:
	// a failed lookup logs and continues (recall shouldn't fail because of
	// a dormancy index hiccup).
	dormantExcluded := 0
	if !req.IncludeDormant {
		dormant, err := s.bank.DormantIDs()
		if err != nil {
			log.Printf("recall: DormantIDs: %v (continuing without dormant filter)", err)
		} else if len(dormant) > 0 {
			kept := scores[:0]
			for _, sc := range scores {
				if _, asleep := dormant[sc.id]; asleep {
					dormantExcluded++
					continue
				}
				kept = append(kept, sc)
			}
			scores = kept
		}
	}

	if req.Limit < len(scores) {
		scores = scores[:req.Limit]
	}
	results := make([]recallResult, 0, len(scores))
	for _, sc := range scores {
		rec, err := s.bank.GetMemory(sc.id)
		if err != nil {
			log.Printf("recall: GetMemory(%s): %v", sc.id, err)
			continue
		}
		// Phase 9 reinforcement reads last_recalled_at to decide which
		// memories were hit since the previous Dream Cycle. Best-effort:
		// a failed UPDATE shouldn't fail the user's recall.
		_ = s.bank.BumpLastRecalled(sc.id)
		results = append(results, recallResult{Memory: rec, Score: sc.score})
	}

	resp := recallResponse{
		Query:              req.Query,
		Limit:              req.Limit,
		Count:              len(results),
		Results:            results,
		SupersededExcluded: supersededExcluded,
		DormantExcluded:    dormantExcluded,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// ── v2.7 Bundle I shared retrieval helper ───────────────────────────────

// Scored is one candidate memory with the cosine score that earned it
// its spot in the ranked list. Promoted to a package-level type so the
// /reflect handler can use the same ranker /recall uses.
type Scored struct {
	ID    string
	Score float64
}

// rankCandidates is the shared retrieval pipeline used by /recall AND
// /reflect. Lesson learned the hard way: the Bundle I retrieval
// improvements (query simplification, BM25+RRF fusion, temporal
// awareness, session co-retrieval) were initially wired into postRecall
// only — /reflect bypassed them through its own cosine-only path. The
// benchmark exposed this: iter 2 showed no movement vs iter 1 because
// the work the prompt fix surfaced ("retrieval isn't finding the
// answer") never reached /reflect.
//
// This helper is now the canonical place to add new retrieval logic.
// Returns candidates sorted by fused score (cosine + bm25 + temporal
// rank fusion); the caller applies its own filtering (supersede,
// dormant, tag, limit). Pass `topSessionExpand` > 0 to fold sibling
// memories from the top-K sessions in (Bundle I lesson #4 — multi-
// session answers).
func rankCandidates(
	ctx context.Context, bank *Bank, router *ModelRouter,
	rawQuery string, topSessionExpand int,
) ([]Scored, error) {
	if bank == nil {
		return nil, errors.New("rankCandidates: bank disabled")
	}
	if router == nil {
		return nil, errors.New("rankCandidates: router not configured")
	}
	embedProvider := router.ForEmbedding()
	if embedProvider == nil {
		return nil, errors.New("rankCandidates: no embedding provider")
	}
	model := providerModelLabel(embedProvider)

	// Simplification used for BM25 query — recallEmbedQuery applies
	// it internally to the embedding text already.
	bm25Query := rawQuery
	if querySimplifyEnabled(bank) {
		if simplified := simplifyQueryForEmbedding(rawQuery); simplified != rawQuery {
			bm25Query = simplified
		}
	}

	queryVec, err := recallEmbedQuery(ctx, bank, rawQuery, embedProvider)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	memVecs, err := bank.AllEmbeddingsForMemories(model)
	if err != nil {
		return nil, fmt.Errorf("load embeddings: %w", err)
	}
	scored := make([]Scored, 0, len(memVecs))
	for id, vec := range memVecs {
		scored = append(scored, Scored{ID: id, Score: recallScore(queryVec, vec)})
	}
	sort.Slice(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })

	// Iter 35 — multi-event query expansion. Questions like "How many days
	// between Walk for Hunger and Coastal Cleanup?" only embed well for
	// ONE of the two named events; the second event's memory falls below
	// the rerank threshold and the synthesis model never sees both dates.
	// Detect dual-event patterns, embed each sub-query separately, and
	// RRF-fuse the top hits from each sub-query into the candidate pool.
	subQueries := extractMultiEventSubqueries(rawQuery)
	if len(subQueries) > 0 {
		cosineIDs := make([]string, len(scored))
		cosineScoreByID := make(map[string]float64, len(scored))
		for i, sc := range scored {
			cosineIDs[i] = sc.ID
			cosineScoreByID[sc.ID] = sc.Score
		}
		rankLists := [][]string{cosineIDs}
		for _, sub := range subQueries {
			subVec, sverr := recallEmbedQuery(ctx, bank, sub, embedProvider)
			if sverr != nil {
				log.Printf("rank: multi-event sub-query embed failed for %q: %v", sub, sverr)
				continue
			}
			subScored := make([]Scored, 0, len(memVecs))
			for id, vec := range memVecs {
				subScored = append(subScored, Scored{ID: id, Score: recallScore(subVec, vec)})
			}
			sort.Slice(subScored, func(i, j int) bool { return subScored[i].Score > subScored[j].Score })
			topN := len(subScored)
			if topN > 50 {
				topN = 50
			}
			subIDs := make([]string, topN)
			for i := 0; i < topN; i++ {
				subIDs[i] = subScored[i].ID
			}
			rankLists = append(rankLists, subIDs)
		}
		if len(rankLists) > 1 {
			fused := ReciprocalRankFusion(rankLists, hybridRRFK(bank), 0)
			scored = scored[:0]
			for _, f := range fused {
				scored = append(scored, Scored{ID: f.ID, Score: cosineScoreByID[f.ID]})
			}
			log.Printf("rank: multi-event-fused %d sub-queries (%v)", len(subQueries), subQueries)
		}
	}

	// Iter66 — keyword-rewrite query expansion. Long natural-language
	// questions often score lower against short, declarative memory text
	// than a stopword-stripped variant would. Generate keyword-focused
	// rewrites and fuse them in via RRF. Generic technique that helps
	// any verbose question shape, not benchmark-specific.
	rewrites := extractKeywordRewrites(rawQuery)
	if len(rewrites) > 0 {
		cosineIDs := make([]string, len(scored))
		cosineScoreByID := make(map[string]float64, len(scored))
		for i, sc := range scored {
			cosineIDs[i] = sc.ID
			cosineScoreByID[sc.ID] = sc.Score
		}
		rankLists := [][]string{cosineIDs}
		for _, rw := range rewrites {
			rwVec, rverr := recallEmbedQuery(ctx, bank, rw, embedProvider)
			if rverr != nil {
				log.Printf("rank: keyword-rewrite embed failed for %q: %v", rw, rverr)
				continue
			}
			rwScored := make([]Scored, 0, len(memVecs))
			for id, vec := range memVecs {
				rwScored = append(rwScored, Scored{ID: id, Score: recallScore(rwVec, vec)})
			}
			sort.Slice(rwScored, func(i, j int) bool { return rwScored[i].Score > rwScored[j].Score })
			topN := len(rwScored)
			if topN > 50 {
				topN = 50
			}
			rwIDs := make([]string, topN)
			for i := 0; i < topN; i++ {
				rwIDs[i] = rwScored[i].ID
			}
			rankLists = append(rankLists, rwIDs)
		}
		if len(rankLists) > 1 {
			fused := ReciprocalRankFusion(rankLists, hybridRRFK(bank), 0)
			scored = scored[:0]
			for _, f := range fused {
				scored = append(scored, Scored{ID: f.ID, Score: cosineScoreByID[f.ID]})
			}
			log.Printf("rank: keyword-rewrite-fused %d variants (%v)", len(rewrites), rewrites)
		}
	}

	// Bundle J — BM25 + RRF fusion.
	if hybridEnabled(bank) {
		bm25Q := bm25Query
		if expansions, eerr := bank.ExpandQueryViaAliases(rawQuery); eerr == nil && len(expansions) > 0 {
			bm25Q = bm25Q + " " + strings.Join(expansions, " ")
		}
		bm25IDs, berr := bank.SearchBM25(bm25Q, 200)
		if berr == nil && len(bm25IDs) > 0 {
			cosineIDs := make([]string, len(scored))
			cosineScoreByID := make(map[string]float64, len(scored))
			for i, sc := range scored {
				cosineIDs[i] = sc.ID
				cosineScoreByID[sc.ID] = sc.Score
			}
			fused := ReciprocalRankFusion([][]string{cosineIDs, bm25IDs}, hybridRRFK(bank), 0)
			scored = scored[:0]
			for _, f := range fused {
				scored = append(scored, Scored{ID: f.ID, Score: cosineScoreByID[f.ID]})
			}
		}
	}

	// Bundle I lesson #3 — temporal-aware retrieval.
	if temporalRecallEnabled(bank) && len(scored) > 0 {
		if dir := detectTemporalIntent(rawQuery); dir != "" {
			topN := len(scored)
			if topN > 50 {
				topN = 50
			}
			candidateIDs := make([]string, topN)
			for i := 0; i < topN; i++ {
				candidateIDs[i] = scored[i].ID
			}
			if temporalIDs, terr := recallTemporalIDs(bank, candidateIDs, dir, topN); terr == nil && len(temporalIDs) > 0 {
				existing := make([]string, len(scored))
				cosineScoreByID := make(map[string]float64, len(scored))
				for i, sc := range scored {
					existing[i] = sc.ID
					cosineScoreByID[sc.ID] = sc.Score
				}
				fused := ReciprocalRankFusion([][]string{existing, temporalIDs}, hybridRRFK(bank), 0)
				scored = scored[:0]
				for _, f := range fused {
					scored = append(scored, Scored{ID: f.ID, Score: cosineScoreByID[f.ID]})
				}
				log.Printf("rank: temporal-fused %d (dir=%s)", len(temporalIDs), dir)
			}
		}
	}

	// Iter 25 (2026-05-14) — hierarchical recall: maps as a SECOND
	// retrieval index. The cosine pass above operates on per-memory
	// embeddings; this pass operates at the CONCEPT-MAP level (cosine
	// against the map's composite-text embedding). Each top-K map's
	// trace list is expanded; the resulting ID stream is RRF-fused
	// with the existing cosine-ranked memory IDs. Off by default
	// (key `map_augmented_recall`); on for the bench iter 25+ ladder.
	//
	// Cost: O(|maps|) cosine + O(|members|) trace lookup per top-K map.
	// Cheap because maps usually number in the hundreds and traces in
	// the tens. Falls through silently when map_traces is empty
	// (legacy banks where Phase 4 didn't link traces — see
	// nightly_pipeline.go runPhase4Maps backlink loop).
	if mapAugmentEnabled(bank) {
		mapExpandIDs, matchedN := mapAugmentRanked(bank, queryVec, model, 5, 200)
		if len(mapExpandIDs) > 0 {
			cosineIDs := make([]string, len(scored))
			cosineScoreByID := make(map[string]float64, len(scored))
			for i, sc := range scored {
				cosineIDs[i] = sc.ID
				cosineScoreByID[sc.ID] = sc.Score
			}
			fused := ReciprocalRankFusion([][]string{cosineIDs, mapExpandIDs}, hybridRRFK(bank), 0)
			scored = scored[:0]
			for _, f := range fused {
				cs := cosineScoreByID[f.ID]
				scored = append(scored, Scored{ID: f.ID, Score: cs})
			}
			log.Printf("rank: map-augmented fused (%d cosine + %d map-expanded across %d matched map(s))",
				len(cosineIDs), len(mapExpandIDs), matchedN)
		}
	}

	// Bundle I lesson #4 — session co-retrieval.
	if topSessionExpand > 0 && sessionExpandEnabled(bank) && len(scored) > 0 {
		if topSessionExpand > len(scored) {
			topSessionExpand = len(scored)
		}
		seen := make(map[string]bool, len(scored))
		for _, sc := range scored {
			seen[sc.ID] = true
		}
		seenSessions := make(map[string]bool, topSessionExpand)
		extras := []string{}
		for i := 0; i < topSessionExpand; i++ {
			rec, gerr := bank.GetMemory(scored[i].ID)
			if gerr != nil || rec.SessionID == "" || seenSessions[rec.SessionID] {
				continue
			}
			seenSessions[rec.SessionID] = true
			siblings, serr := bank.MemoriesInSession(rec.SessionID, 10)
			if serr != nil {
				continue
			}
			added := 0
			for _, sib := range siblings {
				if seen[sib.ID] || added >= 3 {
					continue
				}
				extras = append(extras, sib.ID)
				seen[sib.ID] = true
				added++
			}
		}
		for _, id := range extras {
			scored = append(scored, Scored{ID: id, Score: 0})
		}
		if len(extras) > 0 {
			log.Printf("rank: session-expanded +%d from top-%d sessions", len(extras), len(seenSessions))
		}
	}

	return scored, nil
}

// ── Iter 35 — multi-event query expansion ───────────────────────────────

// extractMultiEventSubqueries detects "between X and Y" / "from X to Y" /
// "X vs Y" / "which … X or Y" patterns in a question and returns the named
// events as separate sub-queries. The caller embeds each sub-query and
// RRF-fuses the results into the main candidate pool, ensuring memories
// about both events surface above the rerank threshold.
//
// Returns empty slice when no multi-event pattern is detected.
// extractKeywordRewrites produces 1-2 short keyword-focused variants of a
// long natural-language question. Generic NLP technique: long questions
// often have low cosine similarity to short, declarative memories whose
// text uses different filler words. Stripping stopwords + dropping
// auxiliary verbs yields a more retrieval-friendly probe.
//
// Returns:
//   - variant 1: stopword-stripped form (e.g. "replacement names assign
//     seed-data 3D printer model labels previously called Prusa")
//   - variant 2: top-N content tokens by length (most distinctive nouns
//     and named entities; e.g. "replacement Prusa printer labels Vector")
//
// Returns nil for queries that are already short (<8 words) — short
// queries don't benefit from rewriting.
//
// This complements extractMultiEventSubqueries, which targets two-anchor
// "between X and Y" questions. The two extractors run independently;
// rankCandidates fuses all variants via RRF.
func extractKeywordRewrites(q string) []string {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil
	}
	words := strings.Fields(q)
	if len(words) < 8 {
		return nil // short queries don't need rewriting
	}
	// Generic English stopword set — auxiliaries, determiners,
	// prepositions, pronouns, question-words. Not topic-tokens.
	stop := map[string]bool{
		"the": true, "a": true, "an": true, "and": true, "or": true, "but": true,
		"is": true, "are": true, "was": true, "were": true, "be": true, "been": true, "being": true,
		"do": true, "does": true, "did": true, "have": true, "has": true, "had": true,
		"will": true, "would": true, "could": true, "should": true, "may": true, "might": true, "can": true,
		"what": true, "which": true, "who": true, "whom": true, "whose": true, "when": true, "where": true, "why": true, "how": true,
		"this": true, "that": true, "these": true, "those": true, "there": true, "here": true,
		"of": true, "in": true, "on": true, "at": true, "by": true, "for": true, "to": true, "from": true,
		"with": true, "as": true, "into": true, "onto": true, "upon": true, "over": true, "under": true,
		"about": true, "above": true, "after": true, "before": true, "between": true, "during": true, "while": true,
		"i": true, "me": true, "my": true, "mine": true, "myself": true,
		"you": true, "your": true, "yours": true, "yourself": true, "yourselves": true,
		"we": true, "us": true, "our": true, "ours": true, "ourselves": true,
		"he": true, "him": true, "his": true, "himself": true,
		"she": true, "her": true, "hers": true, "herself": true,
		"it": true, "its": true, "itself": true, "they": true, "them": true, "their": true, "theirs": true,
		"not": true, "no": true, "yes": true, "if": true, "then": true, "than": true, "so": true,
		"any": true, "all": true, "some": true, "most": true, "more": true, "less": true,
		"one": true, "ones": true, "another": true, "other": true, "such": true, "same": true,
		"please": true, "kindly": true, "just": true, "only": true, "also": true, "too": true,
		"show": true, "tell": true, "give": true, "help": true, "let": true,
		"me?": true, "us?": true, "snippet": true, "snippet?": true,
	}
	// Trim per-word punctuation (BUT keep slashes for paths like /reflect)
	clean := func(w string) string {
		return strings.Trim(strings.ToLower(w), ".,?!:;\"'()[]")
	}

	// Variant 1: stopword-stripped, preserves order. Drop pure-punctuation
	// tokens and very short tokens (<3 chars).
	stripped := make([]string, 0, len(words))
	for _, w := range words {
		c := clean(w)
		if len(c) < 3 || stop[c] {
			continue
		}
		stripped = append(stripped, c)
	}
	if len(stripped) == 0 {
		return nil
	}
	v1 := strings.Join(stripped, " ")

	// Variant 2: top-5 most-distinctive content tokens (sort by length
	// desc as a proxy for distinctiveness; ties broken by original order).
	// Skip generic content words like "really" / "actually" / "thing".
	type tok struct {
		w   string
		pos int
	}
	toks := make([]tok, 0, len(stripped))
	seen := map[string]bool{}
	for i, w := range stripped {
		if seen[w] {
			continue
		}
		seen[w] = true
		toks = append(toks, tok{w: w, pos: i})
	}
	sort.SliceStable(toks, func(i, j int) bool {
		if len(toks[i].w) != len(toks[j].w) {
			return len(toks[i].w) > len(toks[j].w)
		}
		return toks[i].pos < toks[j].pos
	})
	keepN := 5
	if keepN > len(toks) {
		keepN = len(toks)
	}
	picked := make([]tok, keepN)
	copy(picked, toks[:keepN])
	sort.SliceStable(picked, func(i, j int) bool { return picked[i].pos < picked[j].pos })
	parts := make([]string, len(picked))
	for i, t := range picked {
		parts[i] = t.w
	}
	v2 := strings.Join(parts, " ")

	if v2 == v1 {
		return []string{v1}
	}
	return []string{v1, v2}
}

func extractMultiEventSubqueries(q string) []string {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil
	}
	lower := strings.ToLower(q)

	// Pattern 1: "between X and Y" / "days between X and Y"
	if i := strings.Index(lower, "between "); i >= 0 {
		rest := q[i+len("between "):]
		// Find " and " (case-insensitive) in the tail.
		restLower := strings.ToLower(rest)
		if j := strings.Index(restLower, " and "); j > 0 {
			left := strings.TrimSpace(rest[:j])
			right := strings.TrimSpace(rest[j+len(" and "):])
			// Stop right at first '?' or final clause boundary.
			right = trimAtBoundary(right, "?", ".", ",", ";", " when ", " on the ")
			if left != "" && right != "" && len(left) < 80 && len(right) < 80 {
				return []string{left, right}
			}
		}
	}

	// Pattern 2: "from X to Y"
	if i := strings.Index(lower, "from "); i >= 0 {
		rest := q[i+len("from "):]
		restLower := strings.ToLower(rest)
		if j := strings.Index(restLower, " to "); j > 0 {
			left := strings.TrimSpace(rest[:j])
			right := strings.TrimSpace(rest[j+len(" to "):])
			right = trimAtBoundary(right, "?", ".", ",", ";", " when ")
			if left != "" && right != "" && len(left) < 80 && len(right) < 80 {
				return []string{left, right}
			}
		}
	}

	// Pattern 3: "Which … X or Y" — comparative questions naming two
	// candidates ("Who became a parent first, Rachel or Alex?", "Which
	// did I buy first, the necklace or the photo album?"). The trigger
	// is "X or Y" near the
	// END of the question with a leading "which" / "who" / "what".
	if strings.HasPrefix(lower, "which ") || strings.HasPrefix(lower, "who ") ||
		strings.HasPrefix(lower, "what ") {
		if i := strings.LastIndex(lower, " or "); i > 0 {
			// Everything between the last comma and "or" is the left candidate.
			head := q[:i]
			right := strings.TrimSpace(q[i+len(" or "):])
			right = trimAtBoundary(right, "?", ".", ",", ";")
			// Try to find a comma in head to bracket the left candidate.
			leftStart := strings.LastIndexAny(head, ",")
			var left string
			if leftStart >= 0 {
				left = strings.TrimSpace(head[leftStart+1:])
			}
			// Otherwise take the last 8 words of head as a heuristic.
			if left == "" {
				words := strings.Fields(head)
				if len(words) > 8 {
					words = words[len(words)-8:]
				}
				left = strings.TrimSpace(strings.Join(words, " "))
			}
			if left != "" && right != "" && len(left) < 80 && len(right) < 80 {
				return []string{left, right}
			}
		}
	}

	return nil
}

// trimAtBoundary truncates s at the earliest occurrence of any boundary
// substring (case-sensitive). Used to lop off question marks, trailing
// clauses, etc. from extracted event names.
func trimAtBoundary(s string, boundaries ...string) string {
	cut := len(s)
	for _, b := range boundaries {
		if idx := strings.Index(s, b); idx >= 0 && idx < cut {
			cut = idx
		}
	}
	return strings.TrimSpace(s[:cut])
}

// ── v2.7 Bundle I lesson #1 — query simplification for embedding ─────────

// QuerySimplifyEnabledKey controls whether /recall strips interrogative
// scaffolding ("what does", "how do I", "where is", etc.) from the
// query before computing the embedding. Lesson from the Bundle I smoke
// benchmark: a chat-style question like "What does TTL do in Bundle L?"
// produces an embedding clustered near profile-identity memories and
// completely misses a topically-relevant row that surfaces immediately
// for keyword query "Bundle L TTL". Stripping scaffolding before
// embedding brings cosine back in line with the answer content. Stored
// values: "1"|"true"|"on"|"yes" = ON (default), anything else = OFF.
const QuerySimplifyEnabledKey = "recall_query_simplify"

func querySimplifyEnabled(b *Bank) bool {
	if b == nil {
		return true // default ON; nil-bank tests shouldn't disable
	}
	raw, ok, err := b.GetSetting(QuerySimplifyEnabledKey)
	if err != nil || !ok {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// simplifyQueryForEmbedding strips question-word scaffolding and a
// minimal stop-word set so the resulting tokens are the content the
// caller is actually asking about. The original query is still used
// for BM25 (FTS5 handles stop-words itself) — this transformation
// only affects the cosine path. Returns the trimmed string; empty
// result falls back to the raw query so a query that's ALL stop-words
// ("what does it do?") still hits the embedder rather than 400'ing.
func simplifyQueryForEmbedding(q string) string {
	tokens := strings.Fields(q)
	if len(tokens) == 0 {
		return q
	}
	// Trim trailing '?' / '.' / ',' / ';' / ':' before classification.
	stripPunct := func(s string) string {
		return strings.TrimRight(s, "?.,;:!")
	}
	// Stop-list optimised for "agent asks a question" phrasing. Kept
	// short on purpose — we don't want to nuke meaningful content
	// tokens. Single-letter tokens like "L" (as in "Bundle L") are
	// preserved.
	stop := map[string]bool{
		"a": true, "an": true, "the": true, "is": true, "are": true,
		"was": true, "were": true, "be": true, "been": true, "being": true,
		"do": true, "does": true, "did": true, "doing": true,
		"have": true, "has": true, "had": true, "having": true,
		"what": true, "which": true, "who": true, "whom": true, "whose": true,
		"where": true, "when": true, "why": true, "how": true,
		"can": true, "could": true, "should": true, "would": true,
		"will": true, "shall": true, "may": true, "might": true,
		"this": true, "that": true, "these": true, "those": true,
		"it": true, "its": true, "i": true, "me": true, "my": true,
		"you": true, "your": true, "we": true, "our": true, "us": true,
		"in": true, "on": true, "at": true, "by": true, "for": true,
		"to": true, "of": true, "with": true, "about": true, "from": true,
		"and": true, "or": true, "but": true, "if": true, "so": true,
		"as": true, "than": true, "then": true,
	}
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		trim := stripPunct(t)
		lower := strings.ToLower(trim)
		if lower == "" || stop[lower] {
			continue
		}
		out = append(out, trim)
	}
	simplified := strings.Join(out, " ")
	if strings.TrimSpace(simplified) == "" {
		return q
	}
	return simplified
}

// ── Bundle Q — Rerank top-K (salience + cosine + recall_strength) ─────────

// RecallRerankEnabledKey toggles the v2.7 Bundle Q second-pass rerank.
// Default OFF — opt-in until either the built-in salience/cosine blend is
// shown to win on the user's bank or a cross-encoder is wired in. Same
// values as the other recall toggles ("1"|"true"|"on"|"yes" = ON).
// ── v2.7 Bundle I lesson #3 — temporal-aware retrieval ───────────────────

// TemporalRecallEnabledKey toggles a third RRF input: top-N memories
// sorted by created_at when the query carries a temporal marker
// ("first", "last", "earliest", "most recent", "when did", etc.).
// Genuine user benefit: "what was my first meeting with X" no longer
// gets the most-cosine-similar X memory — it gets the OLDEST one.
// Default ON; same toggle semantics as the rest of the recall path.
const TemporalRecallEnabledKey = "recall_temporal_enabled"

// SessionExpandEnabledKey toggles the v2.7 Bundle I lesson #4 session
// co-retrieval (multi-session). Default ON; expands top-3 sessions
// with up to 3 siblings each (so candidate set grows by at most 9
// extra memories). Same toggle semantics as the other recall flags.
const SessionExpandEnabledKey = "recall_session_expand_enabled"

func sessionExpandEnabled(b *Bank) bool {
	if b == nil {
		return true
	}
	raw, ok, err := b.GetSetting(SessionExpandEnabledKey)
	if err != nil || !ok {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// detectTemporalIntent classifies a query into one of:
//   "asc"  — caller wants the EARLIEST matching memory ("first",
//            "originally", "initially", "when did I start", ...)
//   "desc" — caller wants the LATEST matching memory ("last",
//            "most recent", "latest", "yesterday", "lately", ...)
//   ""     — no temporal cue; no temporal augmentation runs.
//
// Word-boundary matching on a curated stem list. Conservative on
// purpose — false positives boost order-dependent rankings for
// non-temporal queries, which can drag the cosine path's best hit
// out of the top-K. "Last" is intentionally omitted from desc
// because it shadows "last year I bought a car" (=desc) AND
// "the last digit is 7" (=non-temporal); we require "last time"
// or "last week" / "last month" instead.
func detectTemporalIntent(query string) string {
	q := strings.ToLower(query)
	// Whole-word presence — a tiny tokenizer is enough.
	words := map[string]bool{}
	for _, t := range strings.FieldsFunc(q, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	}) {
		words[t] = true
	}
	// "asc" stems (treat each as a whole word).
	ascWords := []string{
		"first", "originally", "initially", "initial", "earliest",
		"beginning", "began", "begin", "started",
	}
	// "desc" stems + multi-word desc phrases handled below.
	descWords := []string{
		"recently", "lately", "latest", "newest", "yesterday",
		"today",
	}
	for _, w := range ascWords {
		if words[w] {
			return "asc"
		}
	}
	for _, w := range descWords {
		if words[w] {
			return "desc"
		}
	}
	// Multi-word desc phrases — substring match is OK here because
	// each phrase is unambiguous temporal English.
	descPhrases := []string{
		"most recent", "last time", "last week", "last month",
		"this week", "this month",
	}
	for _, p := range descPhrases {
		if strings.Contains(q, p) {
			return "desc"
		}
	}
	return ""
}

func temporalRecallEnabled(b *Bank) bool {
	if b == nil {
		return true
	}
	raw, ok, err := b.GetSetting(TemporalRecallEnabledKey)
	if err != nil || !ok {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// recallTemporalIDs returns up to `limit` memory IDs ordered by
// created_at for the given direction ("asc" = oldest first, "desc" =
// newest first). Filters out soft-deleted and dormant memories so the
// temporal fusion path matches the other recall lists. Caller passes
// candidateIDs to restrict to a topical pre-filter — only memories
// already in that set will be sorted. nil candidateIDs = scan whole
// bank (used by tests; production always restricts).
func recallTemporalIDs(b *Bank, candidateIDs []string, direction string, limit int) ([]string, error) {
	if b == nil || limit <= 0 || len(candidateIDs) == 0 {
		return nil, nil
	}
	if direction != "asc" && direction != "desc" {
		return nil, nil
	}
	// Build a placeholder list (?,?,?,...) sized to the candidate slice.
	placeholders := make([]string, len(candidateIDs))
	args := make([]any, len(candidateIDs))
	for i, id := range candidateIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	args = append(args, limit)
	q := `SELECT id FROM memories WHERE id IN (` +
		strings.Join(placeholders, ",") +
		`) AND deleted_at = '' AND dormant_at = ''
		  ORDER BY created_at ` + direction + ` LIMIT ?`
	rows, err := b.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

const RecallRerankEnabledKey = "recall_rerank_enabled"

// RecallRerankTopKKey is how many candidates from the post-fusion list
// flow into the reranker. Default 20. The reranker writes back the top-N
// (req.Limit) in its own order. Bumping K trades latency for recall.
const RecallRerankTopKKey = "recall_rerank_top_k"

func rerankEnabled(b *Bank) bool {
	if b == nil {
		return false
	}
	raw, ok, err := b.GetSetting(RecallRerankEnabledKey)
	if err != nil {
		return false
	}
	// Iter 33 (2026-05-15) — default ON when unset. Pairs with the
	// LLM-cross-encoder reranker becoming the system default at the
	// router level. The user can still flip this OFF by setting
	// `recall_rerank_enabled` = "0" / "false" / "off" / "no".
	if !ok {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// rerankTopK returns the number of candidates the cross-encoder
// should score per /reflect call.
//
// Dynamic by bank size — small banks (≤200 memories) can use 20
// because the right answer is almost certainly in cosine top-20.
// At scale (e.g. 800+ memories), cosine top-20 is only ~2-3% of the
// bank and frequently buries the right candidate below rank 20.
// We scale the visibility window linearly with bank size so the
// cross-encoder sees enough candidates to actually rerank.
//
// User-configured override via the RecallRerankTopK setting is
// honored when present (1-200 range). Default behavior scales with
// bank.Count(): bank ≤200 → 20, 200-2000 → 20-60, >2000 → 60-100.
func rerankTopK(b *Bank) int {
	if b == nil {
		return 20
	}
	if raw, ok, err := b.GetSetting(RecallRerankTopKKey); err == nil && ok {
		if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && n > 0 && n <= 200 {
			return n
		}
	}
	// Auto-scale by bank size.
	count, err := b.Count()
	if err != nil || count <= 0 {
		return 20
	}
	const baseTopK = 20
	const maxTopK = 100
	if count <= 200 {
		return baseTopK
	}
	if count >= 2000 {
		// Scale once more for very large banks but cap at maxTopK so
		// per-call rerank cost stays bounded.
		return maxTopK
	}
	// Linear ramp 200 → 2000 maps to 20 → 100.
	frac := float64(count-200) / float64(2000-200)
	topK := baseTopK + int(frac*float64(maxTopK-baseTopK))
	if topK < baseTopK {
		topK = baseTopK
	}
	if topK > maxTopK {
		topK = maxTopK
	}
	return topK
}

// rerankCandidate is the input/output shape for Reranker.Rerank. Holds the
// id, the cosine score from Bundle J, the bank's stored salience, and
// recall_strength so a reranker can score on a richer feature set than
// pure semantic similarity.
type rerankCandidate struct {
	ID             string
	CosineScore    float64
	Salience       float64
	RecallStrength float64
	// Text is the candidate memory's body. Optional — populated when a
	// downstream reranker (e.g. cross-encoder / LLM-based) needs to see
	// the actual content, not just the metadata scores.
	Text string
}

// Reranker is the swap-in interface. The built-in default
// (`salienceReranker`) is a no-LLM blend that combines cosine, salience,
// and recall_strength. A future cross-encoder implementation can replace
// it by wiring a sidecar reranker into the Server and gating on the same
// setting key.
type Reranker interface {
	Rerank(ctx context.Context, query string, candidates []rerankCandidate) ([]rerankCandidate, error)
}

// salienceReranker is the built-in default. It scores each candidate as a
// weighted blend:
//
//   score = 0.6 * cosine + 0.3 * salience + 0.1 * (recall_strength / 5)
//
// then sorts descending. Stable: candidates with equal scores keep their
// pre-rerank order (matters when salience defaults are flat). No I/O —
// safe to wire ON by default once we're confident the blend is at-least-
// neutral on bench data.
type salienceReranker struct{}

func (salienceReranker) Rerank(_ context.Context, _ string, in []rerankCandidate) ([]rerankCandidate, error) {
	out := make([]rerankCandidate, len(in))
	copy(out, in)
	type scored struct {
		c rerankCandidate
		s float64
	}
	scoredList := make([]scored, len(out))
	for i, c := range out {
		recallNorm := c.RecallStrength / 5.0
		if recallNorm > 1.0 {
			recallNorm = 1.0
		}
		if recallNorm < 0 {
			recallNorm = 0
		}
		scoredList[i] = scored{
			c: c,
			s: 0.6*c.CosineScore + 0.3*c.Salience + 0.1*recallNorm,
		}
	}
	sort.SliceStable(scoredList, func(i, j int) bool {
		return scoredList[i].s > scoredList[j].s
	})
	for i, s := range scoredList {
		out[i] = s.c
	}
	return out, nil
}

// ── Hybrid retrieval (BM25 + cosine fusion, Bundle J) helpers ─────────────

// HybridRecallEnabledKey is the well-known setting name for the v2.7
// Bundle J hybrid-retrieval toggle. Stored values: "1"|"true"|"on"|"yes"
// = ON (default), anything else = OFF (cosine-only legacy behaviour).
const HybridRecallEnabledKey = "recall_hybrid_enabled"

// HybridRecallRRFKKey is the per-bank override for the RRF constant.
// Default 60 (Cormack et al.). Higher k = flatter rank curve (item 1
// only slightly outweighs item 10); lower k = sharper top-of-list bias.
const HybridRecallRRFKKey = "recall_hybrid_rrf_k"

func hybridEnabled(b *Bank) bool {
	if b == nil {
		return true // default ON; nil-bank tests shouldn't disable
	}
	raw, ok, err := b.GetSetting(HybridRecallEnabledKey)
	if err != nil || !ok {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

func hybridRRFK(b *Bank) int {
	if b == nil {
		return 60
	}
	raw, ok, err := b.GetSetting(HybridRecallRRFKKey)
	if err != nil || !ok {
		return 60
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return 60
	}
	return n
}

// ── Embed helper ───────────────────────────────────────────────────────────

// recallEmbedQuery returns a normalised embedding for query text.
// Checks the bank embeddings table first; calls the provided embedding
// provider only on a cache miss. The saved label uses providerModelLabel()
// (bare model string, no "kind:" prefix) so AllEmbeddingsForMemories can
// find the row alongside historical entries written before the v2.6
// router refactor.
//
// v2.7 Bundle I — strips question-word scaffolding BEFORE embedding
// when the bank's `recall_query_simplify` setting is ON (default).
// Every caller (postRecall + handleReflectPost + future retrieval
// surfaces) gets the lesson-learned simplification for free.
func recallEmbedQuery(ctx context.Context, bank *Bank, query string, provider LLMProvider) ([]float32, error) {
	if provider == nil {
		return nil, errors.New("recall: no embedding provider")
	}
	embedText := query
	if querySimplifyEnabled(bank) {
		if simplified := simplifyQueryForEmbedding(query); simplified != query {
			embedText = simplified
		}
	}
	h := synapseTextHash(embedText)

	if vec, err := bank.GetEmbedding(h); err == nil && vec != nil {
		return vec, nil
	}

	vec, err := provider.Embed(ctx, embedText)
	if err != nil {
		return nil, err
	}

	label := providerModelLabel(provider)
	if perr := bank.SaveEmbedding(h, vec, label); perr != nil {
		log.Printf("recall: SaveEmbedding: %v (non-fatal)", perr)
	}
	return vec, nil
}

// recallScore returns cosine similarity between two unit vectors.
// Both vectors must already be normalised (vecNormalize was called on them).
func recallScore(a, b []float32) float64 {
	return float64(vecDot(a, b))
}

// ── iter 25 — map-augmented recall ───────────────────────────────────

// mapAugmentEnabledKey gates the hierarchical map-expansion in
// rankCandidates. Default OFF for prod (safest — existing banks may have
// dirty map_traces from old proposal flows). Switch ON for the bench:
//
//	curl -X PUT /settings/map_augmented_recall -d '{"value":"1"}'
const mapAugmentEnabledKey = "map_augmented_recall"

func mapAugmentEnabled(b *Bank) bool {
	if b == nil {
		return false
	}
	if v, ok, _ := b.GetSetting(mapAugmentEnabledKey); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "on", "yes":
			return true
		}
	}
	return false
}

// mapAugmentRanked is the iter-25 hierarchical-recall expansion. It:
//  1. Loads every map's stored embedding (filtered by current model)
//  2. Cosine-ranks them against the already-computed query vector
//  3. Takes the top-K matches and expands their member memories via
//     map_traces, returning a single ordered ID list suitable for RRF
//     fusion with the cosine memory ranking
//
// The trace-IDs from a higher-scoring map come earlier in the returned
// slice so RRF naturally favours members of more-relevant maps.
//
// Returns (memoryIDs, numMatchedMaps). When there are no maps or no
// embeddings under the current model, both are zero-valued and the
// caller skips the fusion.
func mapAugmentRanked(bank *Bank, queryVec []float32, model string, topK, maxOverall int) ([]string, int) {
	if bank == nil || len(queryVec) == 0 {
		return nil, 0
	}
	mapVecs, err := bank.LoadMapEmbeddings(model)
	if err != nil || len(mapVecs) == 0 {
		return nil, 0
	}
	type ms struct {
		id    string
		score float64
	}
	rankings := make([]ms, 0, len(mapVecs))
	for id, v := range mapVecs {
		if len(v) == 0 {
			continue
		}
		rankings = append(rankings, ms{id: id, score: recallScore(queryVec, v)})
	}
	sort.Slice(rankings, func(i, j int) bool { return rankings[i].score > rankings[j].score })
	if topK > 0 && len(rankings) > topK {
		rankings = rankings[:topK]
	}
	// Lower-bound the cosine score so spurious matches don't drag in
	// irrelevant traces. The cutoff is empirical; nomic-embed-text
	// produces unit vectors so cosine is in [-1, 1], and matches
	// below ~0.45 tend to be loose. Tune by setting
	// `map_augment_min_score` if needed (left as a future setting).
	const cutoff = 0.45
	out := make([]string, 0, maxOverall)
	seen := make(map[string]bool, maxOverall*2)
	matched := 0
	for _, r := range rankings {
		if r.score < cutoff {
			break
		}
		matched++
		traceIDs, err := bank.ListTracesForMap(r.id, 200)
		if err != nil {
			continue
		}
		for _, tid := range traceIDs {
			if seen[tid] {
				continue
			}
			out = append(out, tid)
			seen[tid] = true
			if len(out) >= maxOverall {
				return out, matched
			}
		}
	}
	return out, matched
}
