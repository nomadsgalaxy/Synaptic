// synapse_builder.go — semantic synapse generator for SD Core.
//
// Computes embeddings (via the router's ForEmbedding() provider) for every
// memory, then connects memories whose cosine similarity exceeds a
// threshold. Writes assets/synapses.json and fans out a synapses_updated
// event so the dashboard reloads the graph.
//
// v2.6 Bundle C — provider-agnostic. The builder no longer constructs its
// own OllamaProvider from env vars; it accepts any LLMProvider (Ollama,
// openai_local, …) at construction. main.go passes router.ForEmbedding()
// so this stays in lockstep with /recall and the nightly pipeline phases.
// Per the v2.6 default-behaviour-preservation rule: a nil provider
// silently skips passes — same observable behaviour as the v2.5
// "Ollama not reachable" branch (just no longer Ollama-specific).
//
// Algorithm:
//   Phase 1 — Get embeddings for all memories (cached by text hash).
//   Phase 2 — Compute pairwise cosine similarity (O(N²·D) in-memory, fast).
//   Phase 3 — Build edge list: pairs above threshold, degree-capped.
//   Phase 4 — Write synapses.json atomically.
//   Phase 5 — Fanout synapses_updated so the dashboard reloads.
//
// Env vars:
//   SD_SYNAPSE            "1" enable (default), "0" disable
//   SD_SYNAPSE_THRESHOLD  cosine similarity cutoff 0.0–1.0 (default: "0.72")
//   SD_SYNAPSE_MAX_DEGREE max edges per memory (default: "8")
package main

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// SynapseBuilder
// ---------------------------------------------------------------------------

type SynapseBuilder struct {
	memPath   string
	synPath   string
	cachePath string
	threshold float64
	maxDegree int
	hub       *Hub
	// providerFn resolves the current embedding provider on each call.
	// Pass router.ForEmbedding (as a method value) so PUT /settings/
	// providers/{tier} hot-swaps via ReplaceTier take effect at the next
	// pass without a Core restart.
	providerFn func() LLMProvider
	trigger    chan struct{}
	mu         sync.Mutex
	embedCache map[string][]float32 // synapseTextHash → normalized embedding
}

// NewSynapseBuilder constructs a builder against any embedding provider.
// `providerFn` is invoked at each pass to resolve the current provider —
// pass `router.ForEmbedding` (method value) for hot-swap support. A nil
// or always-nil function is permitted; buildPass logs and skips that
// case the same way it used to handle "Ollama not reachable".
func NewSynapseBuilder(memPath, synPath, cachePath string, providerFn func() LLMProvider, hub *Hub) *SynapseBuilder {
	threshold := 0.72
	if v := os.Getenv("SD_SYNAPSE_THRESHOLD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			threshold = f
		}
	}
	maxDegree := 8
	if v := os.Getenv("SD_SYNAPSE_MAX_DEGREE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxDegree = n
		}
	}
	return &SynapseBuilder{
		memPath:    memPath,
		synPath:    synPath,
		cachePath:  cachePath,
		threshold:  threshold,
		maxDegree:  maxDegree,
		hub:        hub,
		providerFn: providerFn,
		trigger:    make(chan struct{}, 1),
		embedCache: make(map[string][]float32),
	}
}

// provider snapshots the current embedding provider. Returns nil when
// no resolver is set or the resolver itself returns nil.
func (sb *SynapseBuilder) provider() LLMProvider {
	if sb == nil || sb.providerFn == nil {
		return nil
	}
	return sb.providerFn()
}

// modelLabel returns the bare model name for log/comment/event use.
// Matches Bundle A's providerModelLabel — stays consistent with the
// SaveEmbedding label format used by /recall and embed-all.
func (sb *SynapseBuilder) modelLabel() string {
	p := sb.provider()
	if p == nil {
		return ""
	}
	return providerModelLabel(p)
}

func (sb *SynapseBuilder) Trigger() {
	select {
	case sb.trigger <- struct{}{}:
	default:
	}
}

func (sb *SynapseBuilder) Run(ctx context.Context) {
	providerName := "(none)"
	if p := sb.provider(); p != nil {
		providerName = p.Name()
	}
	log.Printf("synapse: starting (provider=%s, threshold=%.2f, maxDegree=%d)",
		providerName, sb.threshold, sb.maxDegree)
	sb.loadEmbedCache()
	sb.Trigger() // initial pass

	for {
		select {
		case <-ctx.Done():
			return
		case <-sb.trigger:
			sb.buildPass(ctx)
			// Drain any triggers that queued while we were running.
		drain:
			for {
				select {
				case <-sb.trigger:
				default:
					break drain
				}
			}
		}
	}
}

func (sb *SynapseBuilder) buildPass(ctx context.Context) {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	// Bundle C: no more Ollama-specific /api/tags probe. The
	// withRateLimitRetry wrap inside the provider's Embed() handles
	// transient outages; a nil provider means embedding is unconfigured
	// and we silently skip the pass — matches the v2.5 behaviour where
	// "Ollama not reachable" produced the same observable result.
	if sb.provider() == nil {
		log.Printf("synapse: no embedding provider configured, skipping")
		return
	}

	// Load memories.
	data, err := os.ReadFile(sb.memPath)
	if err != nil {
		log.Printf("synapse: read memories: %v", err)
		return
	}
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(data, &wrapper); err != nil {
		log.Printf("synapse: parse memories: %v", err)
		return
	}
	key := "items"
	if _, ok := wrapper["memories"]; ok {
		key = "memories"
	}
	var records []map[string]interface{}
	if raw, ok := wrapper[key]; ok {
		if err := json.Unmarshal(raw, &records); err != nil {
			log.Printf("synapse: parse records: %v", err)
			return
		}
	}
	if len(records) == 0 {
		return
	}

	log.Printf("synapse: embedding %d memories...", len(records))

	// Compute embeddings, using cache where possible.
	embeddings := make([][]float32, len(records))
	ids := make([]string, len(records))
	newEmbeds := 0
	const partialWriteEvery = 200 // write intermediate synapses while computing

	for i, rec := range records {
		if ctx.Err() != nil {
			return
		}
		id, _ := rec["id"].(string)
		ids[i] = id
		txt := synapseEmbedText(rec)
		h := synapseTextHash(txt)

		if cached, ok := sb.embedCache[h]; ok {
			embeddings[i] = cached
			continue
		}
		vec, err := sb.getEmbedding(ctx, txt)
		if err != nil {
			log.Printf("synapse: embed %s: %v", id, err)
			continue
		}
		vecNormalize(vec)
		embeddings[i] = vec
		sb.embedCache[h] = vec
		newEmbeds++

		if newEmbeds%100 == 0 {
			sb.saveEmbedCache()
			log.Printf("synapse: %d/%d embeddings computed...", i+1, len(records))
		}
		// Write partial synapses so the dashboard shows connections
		// as they're computed rather than waiting for the full pass.
		if newEmbeds > 0 && newEmbeds%partialWriteEvery == 0 {
			if edges := sb.buildEdges(ids[:i+1], embeddings[:i+1]); len(edges) > 0 {
				sb.writeEdges(edges, true)
				log.Printf("synapse: partial write — %d edges from %d/%d memories", len(edges), i+1, len(records))
			}
		}
	}
	if newEmbeds > 0 {
		sb.saveEmbedCache()
		log.Printf("synapse: %d new embeddings computed", newEmbeds)
	}

	// Final full-pass pairwise similarity and write.
	finalEdges := sb.buildEdges(ids, embeddings)
	log.Printf("synapse: %d final edges from %d memories", len(finalEdges), len(records))
	sb.writeEdges(finalEdges, false)
}

// buildEdges computes pairwise cosine similarity for the given id/embedding
// slices and returns degree-capped edges sorted strongest-first.
func (sb *SynapseBuilder) buildEdges(ids []string, embeddings [][]float32) []map[string]interface{} {
	type candidate struct {
		a, b string
		sim  float32
	}
	var cands []candidate
	for i := 0; i < len(embeddings); i++ {
		if embeddings[i] == nil || ids[i] == "" {
			continue
		}
		for j := i + 1; j < len(embeddings); j++ {
			if embeddings[j] == nil || ids[j] == "" {
				continue
			}
			sim := vecDot(embeddings[i], embeddings[j])
			if float64(sim) >= sb.threshold {
				cands = append(cands, candidate{ids[i], ids[j], sim})
			}
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].sim > cands[j].sim })

	degree := make(map[string]int, len(ids))
	var out []map[string]interface{}
	for _, e := range cands {
		if degree[e.a] >= sb.maxDegree || degree[e.b] >= sb.maxDegree {
			continue
		}
		w := int(math.Round(float64(e.sim) * 10))
		if w < 1 {
			w = 1
		}
		out = append(out, map[string]interface{}{"a": e.a, "b": e.b, "w": w})
		degree[e.a]++
		degree[e.b]++
	}
	return out
}

// writeEdges writes edges to synapses.json and fans out synapses_updated.
// partial=true adds a note that more edges are still being computed.
func (sb *SynapseBuilder) writeEdges(edges []map[string]interface{}, partial bool) {
	comment := fmt.Sprintf("Built by SD Core synapse builder — model=%s threshold=%.2f", sb.modelLabel(), sb.threshold)
	if partial {
		comment += " (partial — still computing)"
	}
	out := map[string]interface{}{
		"schema_version": "1.0",
		"edges":          edges,
		"comment":        comment,
	}
	outBytes, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		log.Printf("synapse: marshal: %v", err)
		return
	}
	tmp := sb.synPath + ".syn.tmp"
	if err := os.WriteFile(tmp, outBytes, 0o644); err != nil {
		log.Printf("synapse: write tmp: %v", err)
		return
	}
	if err := os.Rename(tmp, sb.synPath); err != nil {
		log.Printf("synapse: rename: %v", err)
		return
	}
	if !partial {
		log.Printf("synapse: wrote %s (%d edges)", sb.synPath, len(edges))
	}
	if sb.hub != nil {
		sb.hub.Fanout(Event{
			SchemaVersion: SchemaVersion,
			Type:          "synapses_updated",
			Timestamp:     time.Now().UTC().Format(time.RFC3339),
			AdapterID:     "sd-core-synapse",
			Payload: map[string]interface{}{
				"edge_count": len(edges),
				"partial":    partial,
				"model":      sb.modelLabel(),
			},
		})
	}
}

// ---------------------------------------------------------------------------
// Embedding helpers
// ---------------------------------------------------------------------------

// getEmbedding delegates to the currently-resolved LLMProvider. The
// vector returned is already normalised by the provider — callers
// should NOT call vecNormalize on it again. Resolving per call (via
// the providerFn closure) lets PUT /settings/providers/{tier} swaps
// be picked up mid-pass without a restart.
func (sb *SynapseBuilder) getEmbedding(ctx context.Context, text string) ([]float32, error) {
	p := sb.provider()
	if p == nil {
		return nil, errors.New("synapse: no embedding provider configured")
	}
	return p.Embed(ctx, text)
}

// synapseEmbedText builds the embedding input string for a memory.
// Combines text + tags for richer semantic signal.
func synapseEmbedText(rec map[string]interface{}) string {
	text, _ := rec["text"].(string)
	var tags []string
	switch t := rec["tags"].(type) {
	case []interface{}:
		for _, v := range t {
			if s, ok := v.(string); ok {
				tags = append(tags, s)
			}
		}
	case []string:
		tags = t
	}
	s := text
	if len(tags) > 0 {
		s += " [" + strings.Join(tags, " ") + "]"
	}
	if len(s) > 600 {
		s = s[:600]
	}
	return s
}

// synapseTextHash returns a short hash of the embed input string used as cache key.
func synapseTextHash(s string) string {
	h := sha1.Sum([]byte(s))
	return fmt.Sprintf("%x", h[:10]) // 20 hex chars
}

// vecNormalize normalizes a float32 slice to unit length in-place.
func vecNormalize(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return
	}
	inv := float32(1.0 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
}

// vecDot returns the dot product of two float32 slices (assumes unit vectors).
func vecDot(a, b []float32) float32 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var sum float64
	for i := 0; i < n; i++ {
		sum += float64(a[i]) * float64(b[i])
	}
	return float32(sum)
}

// ---------------------------------------------------------------------------
// Embedding cache (JSON: hash → []float32)
// ---------------------------------------------------------------------------

type embedCacheFile map[string][]float32

func (sb *SynapseBuilder) loadEmbedCache() {
	data, err := os.ReadFile(sb.cachePath)
	if err != nil {
		return
	}
	var raw embedCacheFile
	if err := json.Unmarshal(data, &raw); err != nil {
		return
	}
	sb.embedCache = raw
	log.Printf("synapse: loaded %d cached embeddings", len(sb.embedCache))
}

func (sb *SynapseBuilder) saveEmbedCache() {
	data, err := json.Marshal(embedCacheFile(sb.embedCache))
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(sb.cachePath), 0o755); err != nil {
		return
	}
	tmp := sb.cachePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	os.Rename(tmp, sb.cachePath)
}
