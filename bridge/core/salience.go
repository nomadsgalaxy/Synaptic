// salience.go — computes the per-memory salience score (0.0–1.0).
//
// [R1] Per CITATIONS.md #10 (Moncada et al. 2015 Behavioral Tagging) +
//      #14 (Payne & Kensinger 2018 Stress, sleep & emotional memory),
//      salience tags memories at encoding time for preferential
//      reactivation downstream. The score drives:
//        - Phase 0b queue priority (high-salience dirty memories first
//          when ties otherwise exist)
//        - Phase 5 decay protection (high salience → less likely to be
//          pruned even when recall-stale)
//        - Phase 9 reinforcement priority (R9 weak-but-salient class)
//
// The score is a simple blend, deliberately conservative:
//
//   emotional × 0.4   — surface markers in the text (! / ALL CAPS / lexicon)
//   novelty   × 0.4   — 1 − max cosine to existing embeddings (default 0.5
//                       when no embeddings exist yet)
//   recency   × 0.2   — last_recalled_at recency curve (0..1)
//
// This is NOT a sentiment classifier — we intentionally avoid Tier 1/2
// calls here so SaveMemory stays fast on the hot path. The lexicon is
// English-biased; that's a known limitation noted in CITATIONS.md.
//
// Both call sites (bank.SaveMemory + bank_p5.PersistDeepEncoding) pass
// the novelty score they have available; SaveMemory typically gets
// the default 0.5 (no embedding exists yet) while PersistDeepEncoding
// passes a real cosine-derived value via the Phase 0b orchestrator.

package main

import (
	"strings"
	"time"
	"unicode"
)

// salienceWeights captures the per-component weights — exposed as named
// constants so the rationale is grep-able from CITATIONS.md hits.
const (
	salienceEmotionalWeight = 0.4 // CITATIONS.md #14 — emotional encoding
	salienceNoveltyWeight   = 0.4 // CITATIONS.md #10 — novelty as STC tag
	salienceRecencyWeight   = 0.2 // CITATIONS.md #7  — recently-accessed bias
)

// emotionalLexicon — a small, English-biased sentiment-laden set used as
// a salience signal. Not intended to replace a real classifier; the goal
// is to give "URGENT", "panic", "must", "never" etc. a non-zero score
// without a model call. Each match contributes once (deduped per memory).
var emotionalLexicon = map[string]bool{
	// urgency / negation / superlatives
	"urgent": true, "must": true, "never": true, "always": true,
	"critical": true, "emergency": true, "panic": true, "fear": true,
	"hate": true, "love": true, "afraid": true, "worried": true,
	"important": true, "essential": true, "broken": true, "broke": true,
	"failed": true, "fail": true, "fails": true, "crash": true, "crashed": true,
	// emphatic affect
	"amazing": true, "awful": true, "terrible": true, "wonderful": true,
	"horrible": true, "fantastic": true, "disaster": true, "delight": true,
	// commitment / decision
	"decided": true, "committed": true, "promised": true, "rejected": true,
	"abandoned": true, "regretting": true, "regret": true,
}

// computeEmotionalScore returns a 0..1 score from surface emotional
// markers in text + tags. Not language-aware — purely lexical. Capped at
// 1.0. The breakdown:
//
//   exclamation marks: each adds 0.10 (max 0.30)
//   ALL-CAPS words (≥3 chars): each adds 0.05 (max 0.20)
//   lexicon hits: each adds 0.10 (max 0.40), deduped
//   tag hit on a sensitive/affect word: adds 0.10 (max 0.10)
//
// Total capped at 1.0.
func computeEmotionalScore(text string, tags []string) float64 {
	score := 0.0

	// Exclamation marks (capped).
	excl := 0
	for _, r := range text {
		if r == '!' {
			excl++
		}
	}
	if excl > 3 {
		excl = 3
	}
	score += 0.10 * float64(excl)

	// ALL-CAPS bursts (≥3 chars, all uppercase, not common acronyms).
	caps := 0
	for _, w := range strings.Fields(text) {
		if len(w) < 3 {
			continue
		}
		isCaps := true
		hasLetter := false
		for _, r := range w {
			if unicode.IsLetter(r) {
				hasLetter = true
				if !unicode.IsUpper(r) {
					isCaps = false
					break
				}
			}
		}
		if isCaps && hasLetter {
			caps++
		}
	}
	if caps > 4 {
		caps = 4
	}
	score += 0.05 * float64(caps)

	// Lexicon hits, deduped.
	seen := map[string]bool{}
	hits := 0
	for _, w := range strings.Fields(strings.ToLower(text)) {
		// Strip simple punctuation so "panic." matches "panic".
		w = strings.TrimFunc(w, func(r rune) bool {
			return !unicode.IsLetter(r)
		})
		if w == "" || seen[w] {
			continue
		}
		if emotionalLexicon[w] {
			seen[w] = true
			hits++
			if hits >= 4 {
				break
			}
		}
	}
	score += 0.10 * float64(hits)

	// Tag affect signal — lexicon hit on a tag adds a fixed 0.10 once.
	for _, t := range tags {
		if emotionalLexicon[strings.ToLower(strings.TrimSpace(t))] {
			score += 0.10
			break
		}
	}

	if score > 1.0 {
		score = 1.0
	}
	return score
}

// computeRecencyScore reads `last_recalled_at` (RFC3339-ish) and returns
// a recency curve in 0..1: 1.0 if recalled in the last 7 days, decays
// linearly to 0 at 90 days, hard zero beyond. Empty / unparseable → 0.
func computeRecencyScore(lastRecalledAt string, now time.Time) float64 {
	if lastRecalledAt == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, lastRecalledAt)
	if err != nil {
		t, err = time.Parse(time.RFC3339, lastRecalledAt)
		if err != nil {
			return 0
		}
	}
	days := now.Sub(t).Hours() / 24.0
	if days < 0 {
		return 1.0 // future timestamp shouldn't happen but defend against it
	}
	if days <= 7 {
		return 1.0
	}
	if days >= 90 {
		return 0
	}
	// linear decay between 7 and 90 days
	return 1.0 - (days-7)/83.0
}

// computeSalience blends the three components per the weights above.
// novelty must be supplied by the caller (0..1, default 0.5 when no
// embedding context available). lastRecalledAt is the column verbatim.
//
// Result clamped to [0, 1].
func computeSalience(text string, tags []string, novelty float64, lastRecalledAt string, now time.Time) float64 {
	if novelty < 0 {
		novelty = 0
	}
	if novelty > 1 {
		novelty = 1
	}
	emotional := computeEmotionalScore(text, tags)
	recency := computeRecencyScore(lastRecalledAt, now)
	s := salienceEmotionalWeight*emotional +
		salienceNoveltyWeight*novelty +
		salienceRecencyWeight*recency
	if s < 0 {
		s = 0
	}
	if s > 1 {
		s = 1
	}
	return s
}

// computeNoveltyForMemory returns 1 − max cosine to any cached embedding
// other than the memory's own. Suitable for the PersistDeepEncoding path
// where the memory has just been re-embedded and we want a fresh score.
//
// Returns the default 0.5 (neutral) when:
//   - bank has no other embeddings
//   - the memory's own embedding hash isn't in the cache yet
//   - any error occurs
//
// The compute cost is O(N) embedding-vector dot products — fine for the
// Phase 0b sequential loop where we're already paying that cost via
// findEnrichmentNeighbours; not appropriate to call from SaveMemory's
// hot path on a bursty ingestion stream.
func computeNoveltyForMemory(bank *Bank, memID, embedHash string) float64 {
	if bank == nil || embedHash == "" {
		return 0.5
	}
	target, err := bank.GetEmbedding(embedHash)
	if err != nil || target == nil {
		return 0.5
	}
	model := envOr("SD_SYNAPSE_MODEL", envOr("SD_CLASSIFY_MODEL", "llama3.2:3b"))
	all, err := bank.AllEmbeddingsForMemories(model)
	if err != nil || len(all) <= 1 {
		return 0.5
	}
	maxSim := float32(0.0)
	for id, vec := range all {
		if id == memID {
			continue
		}
		s := vecDot(target, vec)
		if s > maxSim {
			maxSim = s
		}
	}
	novelty := 1.0 - float64(maxSim)
	if novelty < 0 {
		novelty = 0
	}
	if novelty > 1 {
		novelty = 1
	}
	return novelty
}
