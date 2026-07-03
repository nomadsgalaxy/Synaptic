// Memory region classifier — background goroutine inside SD Core.
//
// Scans assets/memories.json for records with a missing or fallback `region`
// field and fills them in using the same layered cascade as tools/classifier.py:
//
//   Tier 1 — tag pattern matching  (deterministic; instant)
//   Tier 2 — keyword scan on text  (deterministic; instant)
//   Tier 3 — Ollama /api/chat      (semantic; ~2-4 s/call; 4 concurrent workers)
//
// Phase 1 (Tier 1+2) writes back immediately so the dashboard updates fast.
// Phase 2 (Tier 3) runs concurrently in the background, writing every 25
// completions. A content-hash cache in _meta/classify_cache.json prevents
// re-classifying the same text+tags pair across restarts.
//
// Env vars:
//   SD_CLASSIFY         "1" enable (default), "0" disable
//   SD_CLASSIFY_MODEL   Ollama model (default: llama3.2:3b)
//   SD_OLLAMA_URL       shared with the bubble generator
package main

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Region / tier data — ported 1-to-1 from tools/classifier.py
// ---------------------------------------------------------------------------

var regionKeys = []string{
	// Original 14 base anatomical regions (Allen Human Reference Atlas)
	"prefrontal_cortex", "frontal_lobe", "broca_area", "wernicke_area",
	"visual_cortex", "temporal_lobe_left", "temporal_lobe_right",
	"hippocampus", "parietal_lobe", "motor_cortex",
	"cerebellum", "amygdala", "corpus_callosum", "brain_stem",
	// 2026-05-17 real-anatomy gap-fill — subcortical and network-level
	// regions the dashboard renders via REGION_INFO + REGION_ANATOMICAL_CENTERS.
	// Listed here so Tier-2/3 LLM-emitted region names land in the canonical
	// set instead of getting dropped into "unknown".
	"insula", "thalamus", "basal_ganglia",
	"posterior_cingulate", "precuneus",
	"orbitofrontal_cortex", "ventromedial_prefrontal_cortex",
	"fusiform_gyrus", "angular_gyrus",
	"salience_network", "dorsal_attention_network",
	"frontoparietal_control_network",
}

var regionKeySet map[string]bool

func init() {
	regionKeySet = make(map[string]bool, len(regionKeys))
	for _, k := range regionKeys {
		regionKeySet[k] = true
	}
}

type tagRule struct {
	tags      map[string]bool
	prefix    []string
	contextEq map[string]bool
}

var tagPatterns = []struct {
	region string
	rule   tagRule
}{
	{"amygdala", tagRule{tags: strSet(
		"error", "gotcha", "incident", "warning", "bug", "bugfix", "rate-limiting", "broken", "crash",
	)}},
	{"hippocampus", tagRule{
		tags:      strSet("consolidated", "mental-model", "bank", "retrospective", "memory", "memory_type:fact", "memory_type:rule", "memory_type:procedure"),
		prefix:    []string{"memory_type:"},
		contextEq: strSet("user-profile"),
	}},
	{"broca_area", tagRule{tags: strSet(
		"documentation", "docs", "output", "response", "writing", "readme", "summary",
	)}},
	{"wernicke_area", tagRule{tags: strSet(
		"prompt", "intent", "parsing", "comprehension", "prompt-engineering",
	)}},
	{"visual_cortex", tagRule{tags: strSet(
		"ui", "ux", "visual", "screenshot", "chrome-mcp", "image", "obs-websocket",
		"vdo-ninja", "react", "html", "css", "browser-extension", "browser",
		"scene", "design", "radiation", "event-tracker",
	)}},
	{"temporal_lobe_left", tagRule{tags: strSet(
		"reference", "knowledge", "music", "audio", "language", "dictionary",
	)}},
	{"motor_cortex", tagRule{tags: strSet(
		"tool", "mcp", "bash", "exec", "execution", "deployment", "operations",
		"setup", "github", "cloudflare", "docker", "cli", "command", "shell",
		"powershell", "windows", "prusa-community-dashboard",
	)}},
	{"cerebellum", tagRule{tags: strSet(
		"testing", "ci", "test", "coordination", "auth", "automation", "fine-motor",
	)}},
	{"corpus_callosum", tagRule{tags: strSet(
		"slack", "discord-bridge", "discord", "bridge", "inter-agent", "mitmproxy",
		"phoenix-tracer", "observability", "ai-tooling", "community-live-obs", "ipc", "mcp-bridge",
	)}},
	{"parietal_lobe", tagRule{tags: strSet(
		"architecture", "tech-stack", "repo-layout", "code", "system-design",
		"config", "config-schema", "schema", "data", "database", "logic",
		"math", "gravity", "opencascade", "positron3d",
	)}},
	{"prefrontal_cortex", tagRule{tags: strSet(
		"decision", "design-decision", "planning", "ai-cascade", "agent",
		"subagent", "workflow", "orchestration", "synaptic", "synaptic-disorder",
	)}},
	{"frontal_lobe", tagRule{tags: strSet(
		"feedback", "directive", "user-profile", "user:anthony", "user:nomad",
		"preferences", "personality", "feature", "purpose", "overview", "project",
	)}},
	{"brain_stem", tagRule{tags: strSet(
		"creative", "generation", "imagination", "art", "dream", "concept", "vision",
	)}},
}

var kwPatterns = []struct {
	region   string
	keywords []string
}{
	{"amygdala", []string{"error ", "exception", "broke ", "broken", "crashed", " bug ", "warning", "incident", "rate limit"}},
	{"hippocampus", []string{"retrospective", "consolidat", "mental model", "recall", " memory ", " remember "}},
	{"broca_area", []string{"readme", "documentation", " docs ", "wrote a", "summary of"}},
	{"wernicke_area", []string{" prompt ", "user input", "comprehen", "parse"}},
	{"visual_cortex", []string{" ui ", " ux ", "visual", "screenshot", "browser", "obs ", "vdo", "react component", " css ", " html ", "design"}},
	{"temporal_lobe_left", []string{"fact:", "reference", "knowledge", " music ", " audio "}},
	{"motor_cortex", []string{"bash ", "deploy", " run ", "execut", "github", "cloudflare", "docker", " cli ", "command line", "powershell"}},
	{"cerebellum", []string{" test ", " tests ", " ci ", " auth ", "automation", "playwright"}},
	{"corpus_callosum", []string{" slack ", " discord ", "bridge", "tracing", " mcp ", "mitmproxy", "phoenix", "telemetry"}},
	{"parietal_lobe", []string{"architectur", "schema", "tech stack", "system design", " repo ", "config"}},
	{"prefrontal_cortex", []string{"plan ", "planning", "decision", "agent", "orchestrat", "subagent"}},
	{"frontal_lobe", []string{"preference", "personality", "feedback", "feature", "directive"}},
	{"brain_stem", []string{"creative", "imagin", "dream", " art ", "generat"}},
}

func strSet(ss ...string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

// classifySystemPrompt is the brain-region routing prompt. It is sent as
// the system message in tier3Ollama; for reasoning-capable models the
// caller can opt-in to prepending `[think:off]\n\n` via Bank.ThinkOffPrefix()
// (settings key `tier1_think_off`, default ON). The parser at
// strings.Contains() below still picks the region key correctly even if
// a leading `[think:off]` token echoes back into output.
const classifySystemPrompt = `You classify a memory into ONE brain region based on what cognitive function the memory is about. Output ONLY the region key from the list below — no explanation, no punctuation, no markdown.

Base regions:
  prefrontal_cortex   - agent orchestration, planning, architectural choices
  frontal_lobe        - identity, personality, work-style preferences, decisions
  broca_area          - response generation, documentation, READMEs, written output
  wernicke_area       - prompt parsing, user input comprehension
  visual_cortex       - UI/UX, web, screenshots, browser extensions, visual design
  temporal_lobe_left  - long-term factual knowledge, references, language, music
  temporal_lobe_right - long-term factual knowledge (right hemisphere variant)
  hippocampus         - memory systems themselves (consolidation, mental models)
  parietal_lobe       - code architecture, system design, schemas, data structures
  motor_cortex        - tool execution, MCP calls, bash, deployment, ops
  cerebellum          - testing, CI, coordination, auth flows
  amygdala            - errors, bugs, gotchas, warnings, incidents
  corpus_callosum     - inter-system bridges (Slack, MCP, telemetry, observability)
  brain_stem          - creative generation, imagination, art, dreams

Specialized regions (use when clearly applicable):
  insula                          - gut-level signals, "something feels off" intuitions
  thalamus                        - attention routing, what got through the filter
  basal_ganglia                   - habits, procedural memories, standing directives
  posterior_cingulate             - autobiographical recall, looking back at own work
  precuneus                       - mental-image / scenario reconstruction
  orbitofrontal_cortex            - value judgments, regret, "that was a bad idea"
  ventromedial_prefrontal_cortex  - emotionally-weighted choices, team-trust calls
  fusiform_gyrus                  - pattern recognition of specific recurring things
  angular_gyrus                   - cross-domain integration (TPJ, theory of mind)
  salience_network                - interrupt-worthy signals, triage decisions
  dorsal_attention_network        - deliberate focus, goal-directed search
  frontoparietal_control_network  - multi-step workflow context, sub-task switching

If multiple regions could fit, prefer a base region unless a specialized region is clearly the better match. Output only the region key.`

// ---------------------------------------------------------------------------
// MemoryClassifier
// ---------------------------------------------------------------------------

type MemoryClassifier struct {
	memPath   string
	cachePath string
	// providerFn resolves the realtime provider on each Tier 3 call.
	// main.go passes router.ForRealtime so PUT /settings/providers/tier1
	// hot-swaps take effect at the next classification without restart.
	providerFn func() LLMProvider
	hub        *Hub
	bank       *Bank         // optional; used to read tier1_think_off setting
	trigger    chan struct{} // buffered(1); non-blocking send to request a pass

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	Region string `json:"region"`
	Tier   string `json:"tier"`
}

// NewMemoryClassifier builds a classifier wired to a provider-resolver.
// v2.6 Bundle C: removed the hardcoded Ollama URL/model + httpClient
// fields. main.go passes router.ForRealtime (method value) so the
// classifier picks up Tier 1 model swaps without a Core restart. nil
// or always-nil providerFn is allowed — the pass falls back to
// tag/keyword routing.
func NewMemoryClassifier(memPath, cachePath string, providerFn func() LLMProvider, hub *Hub, bank *Bank) *MemoryClassifier {
	mc := &MemoryClassifier{
		memPath:    memPath,
		cachePath:  cachePath,
		providerFn: providerFn,
		hub:        hub,
		bank:       bank,
		trigger:    make(chan struct{}, 1),
		cache:      map[string]cacheEntry{},
	}
	mc.loadCache()
	return mc
}

// provider snapshots the current provider. Returns nil when no resolver
// is set or the resolver itself returns nil.
func (mc *MemoryClassifier) provider() LLMProvider {
	if mc == nil || mc.providerFn == nil {
		return nil
	}
	return mc.providerFn()
}

// thinkOffPrefix returns "[think:off]\n\n" or "" based on the bank's
// tier1_think_off setting (default ON when bank is nil — preserves
// the documented latency-win default for reasoning-capable models).
func (mc *MemoryClassifier) thinkOffPrefix() string {
	if mc == nil || mc.bank == nil {
		return "[think:off]\n\n"
	}
	return mc.bank.ThinkOffPrefix()
}

// Trigger requests a classification pass. Non-blocking; silently dropped if
// a pass is already pending.
func (mc *MemoryClassifier) Trigger() {
	select {
	case mc.trigger <- struct{}{}:
	default:
	}
}

// Run blocks until ctx is cancelled. It fires an initial pass after an 8-second
// startup delay (giving SD Core and Ollama time to settle), then re-runs
// whenever Trigger() is called or the 5-minute safety sweep fires.
func (mc *MemoryClassifier) Run(ctx context.Context) {
	providerName := "(none — tag fallback only)"
	if p := mc.provider(); p != nil {
		providerName = p.Name()
	}
	log.Printf("classifier: starting (provider=%s)", providerName)

	select {
	case <-ctx.Done():
		return
	case <-time.After(8 * time.Second):
	}
	mc.Trigger()

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-mc.trigger:
			mc.classifyPass(ctx)
		case <-ticker.C:
			mc.Trigger()
		}
	}
}

// ---------------------------------------------------------------------------
// Classification pass
// ---------------------------------------------------------------------------

type recItem struct {
	idx int
	rec map[string]interface{}
}

func (mc *MemoryClassifier) classifyPass(ctx context.Context) {
	envelope, rawRecords, recordsKey, err := mc.loadMemories()
	if err != nil || len(rawRecords) == 0 {
		return
	}

	// Decode all records; identify which need classification.
	allRecs := make([]map[string]interface{}, len(rawRecords))
	var unclassified []recItem
	for i, raw := range rawRecords {
		var rec map[string]interface{}
		if err := json.Unmarshal(raw, &rec); err != nil {
			continue
		}
		allRecs[i] = rec
		region, _ := rec["region"].(string)
		tier, _ := rec["region_tier"].(string)
		// Reclassify if missing OR if Ollama wasn't available last time (fallback tier).
		if region == "" || tier == "fallback" {
			unclassified = append(unclassified, recItem{i, rec})
		}
	}

	if len(unclassified) == 0 {
		return
	}
	log.Printf("classifier: %d memories need classification (of %d total)",
		len(unclassified), len(allRecs))

	// ------------------------------------------------------------------
	// Phase 1: Tier 1 (tags) + Tier 2 (keywords) — synchronous, instant.
	// ------------------------------------------------------------------
	var needTier3 []recItem
	phase1Count := 0
	for _, item := range unclassified {
		if ctx.Err() != nil {
			return
		}
		region, tier := mc.classifyTier1and2(item.rec)
		if region != "" {
			item.rec["region"] = region
			item.rec["region_tier"] = tier
			allRecs[item.idx] = item.rec
			phase1Count++
		} else {
			needTier3 = append(needTier3, item)
		}
	}

	if phase1Count > 0 {
		if err := mc.writeBack(envelope, allRecs, rawRecords, recordsKey); err != nil {
			log.Printf("classifier: phase 1 write: %v", err)
		} else {
			mc.fanout(allRecs, unclassified[:phase1Count])
			log.Printf("classifier: phase 1 done — %d memories via Tier 1/2", phase1Count)
		}
	}

	if len(needTier3) == 0 || ctx.Err() != nil {
		mc.saveCache()
		return
	}

	// ------------------------------------------------------------------
	// Phase 2: Tier 3 (LLM) — 4 concurrent workers, write every 25.
	// ------------------------------------------------------------------
	// Bundle C: provider-agnostic. The withRateLimitRetry wrap inside
	// provider.Chat() handles transient outages, so we don't need a
	// separate /api/tags probe anymore. Just check that the resolver
	// gives us a non-nil provider right now.
	providerOK := mc.provider() != nil
	if !providerOK {
		// Mark remaining as fallback so phase 1 results are persisted.
		// They will be retried on the next pass when a provider becomes available.
		for _, item := range needTier3 {
			item.rec["region"] = "frontal_lobe"
			item.rec["region_tier"] = "fallback"
			allRecs[item.idx] = item.rec
		}
		mc.writeBack(envelope, allRecs, rawRecords, recordsKey)
		mc.saveCache()
		log.Printf("classifier: Ollama not reachable — %d memories marked fallback (will retry)", len(needTier3))
		return
	}
	log.Printf("classifier: Ollama reachable — %d memories queued for Tier 3", len(needTier3))

	type t3Result struct {
		item   recItem
		region string
		tier   string
	}

	results := make(chan t3Result, len(needTier3))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup

	for _, item := range needTier3 {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		item := item
		go func() {
			defer func() { <-sem; wg.Done() }()
			r := mc.tier3Ollama(ctx, item.rec)
			t := "tier3_ollama"
			if r == "" {
				r, t = "frontal_lobe", "fallback"
			}
			results <- t3Result{item, r, t}
		}()
	}

	go func() { wg.Wait(); close(results) }()

	t3Done := 0
	var recentT3 []recItem
	for res := range results {
		if ctx.Err() != nil {
			break
		}
		res.item.rec["region"] = res.region
		res.item.rec["region_tier"] = res.tier
		allRecs[res.item.idx] = res.item.rec
		recentT3 = append(recentT3, res.item)
		t3Done++

		if t3Done%25 == 0 {
			mc.writeBack(envelope, allRecs, rawRecords, recordsKey)
			mc.fanout(allRecs, recentT3)
			log.Printf("classifier: Tier 3 progress %d/%d", t3Done, len(needTier3))
			recentT3 = recentT3[:0]
		}
	}

	// Final flush.
	mc.writeBack(envelope, allRecs, rawRecords, recordsKey)
	if len(recentT3) > 0 {
		mc.fanout(allRecs, recentT3)
	}
	mc.saveCache()
	log.Printf("classifier: phase 2 done — %d via Tier 3 Ollama", t3Done)
}

// ---------------------------------------------------------------------------
// Tier implementations
// ---------------------------------------------------------------------------

func (mc *MemoryClassifier) classifyTier1and2(rec map[string]interface{}) (region, tier string) {
	h := contentHash(rec)
	mc.mu.Lock()
	if ce, ok := mc.cache[h]; ok {
		mc.mu.Unlock()
		return ce.Region, ce.Tier
	}
	mc.mu.Unlock()

	if r := tier1Tags(rec); r != "" {
		mc.cacheSet(h, r, "tier1_tags")
		return r, "tier1_tags"
	}
	if r := tier2Keywords(rec); r != "" {
		mc.cacheSet(h, r, "tier2_keywords")
		return r, "tier2_keywords"
	}
	return "", ""
}

func tier1Tags(rec map[string]interface{}) string {
	tags := normalizeTags(rec)
	tagSet := make(map[string]bool, len(tags))
	for _, t := range tags {
		tagSet[t] = true
	}
	ctx := strings.ToLower(recStr(rec, "context"))

	for _, p := range tagPatterns {
		for t := range p.rule.tags {
			if tagSet[t] {
				return p.region
			}
		}
		for _, pfx := range p.rule.prefix {
			for t := range tagSet {
				if strings.HasPrefix(t, pfx) {
					return p.region
				}
			}
		}
		if len(p.rule.contextEq) > 0 && p.rule.contextEq[ctx] {
			return p.region
		}
	}
	return ""
}

func tier2Keywords(rec map[string]interface{}) string {
	text := strings.ToLower(recStr(rec, "text"))
	if text == "" {
		return ""
	}
	for _, p := range kwPatterns {
		for _, kw := range p.keywords {
			if strings.Contains(text, kw) {
				return p.region
			}
		}
	}
	return ""
}

// tier3Ollama returns the classifier's region key, or "" when the call
// produces no usable answer (transport error after retries via
// provider.Chat's internal withRateLimitRetry, or model output that
// doesn't match any region key).
//
// Bundle C: now delegates to provider.Chat(). The previous outer retry
// wrap is gone — provider.Chat() applies its own withRateLimitRetry
// internally, so wrapping again would double-retry to 5×5=25 attempts.
// Function name kept (`tier3Ollama`) to avoid churning callers; the
// "Ollama" suffix is historical only.
func (mc *MemoryClassifier) tier3Ollama(ctx context.Context, rec map[string]interface{}) string {
	out, _ := mc.tier3OllamaOnce(ctx, rec)
	return out
}

// tier3OllamaOnce performs one provider.Chat() call and returns the
// parsed region key. Errors are returned (caller currently discards
// them — tag-fallback is the contract for "AI unavailable").
// Provider is snapshotted per call so a mid-pass swap is consistent
// within this one classification.
func (mc *MemoryClassifier) tier3OllamaOnce(ctx context.Context, rec map[string]interface{}) (string, error) {
	p := mc.provider()
	if p == nil {
		return "", errors.New("classifier: no provider configured")
	}
	text := recStr(rec, "text")
	if len(text) > 1200 {
		text = text[:1200]
	}
	tags := strings.Join(normalizeTags(rec), ", ")
	if tags == "" {
		tags = "(none)"
	}
	userMsg := fmt.Sprintf("Memory text: %s\nTags: %s\nContext: %s\n\nRegion key:",
		text, tags, recStr(rec, "context"))

	// Budget 20 (was 12): small headroom for an echoed `[think:off]`
	// token or a leading "Region:" label before the actual region key,
	// since the parser scans across newlines as a fallback. Still well
	// under the 80-token threshold where output cost becomes noticeable.
	content, err := p.Chat(ctx, []Message{
		{Role: "system", Content: mc.thinkOffPrefix() + classifySystemPrompt},
		{Role: "user", Content: userMsg},
	}, 20)
	if err != nil {
		return "", err
	}

	raw := strings.ToLower(strings.TrimSpace(content))
	// Strip <think>...</think> blocks before parsing — reasoning-capable
	// models (qwen3-thinking, Llama 3.3-Think, etc.) wrap their hidden
	// trace in these tags when the `[think:off]` directive (settings.
	// tier1_think_off) is OFF or unsupported. Without this strip, the
	// first-newline split below would grab the trace's opening line
	// instead of the final region answer.
	for {
		start := strings.Index(raw, "<think>")
		if start < 0 {
			break
		}
		end := strings.Index(raw[start:], "</think>")
		if end < 0 {
			break
		}
		raw = raw[:start] + raw[start+end+len("</think>"):]
	}
	raw = strings.Trim(raw, "`'\"\n .,:;")
	// Try the first line first (legacy behaviour; matches "hippocampus"
	// and "[think:off] hippocampus" via the contains() loop below).
	firstLine := raw
	if i := strings.IndexByte(raw, '\n'); i >= 0 {
		firstLine = raw[:i]
	}
	firstLine = strings.TrimSpace(firstLine)
	if regionKeySet[firstLine] {
		return firstLine, nil
	}
	for _, k := range regionKeys {
		if strings.Contains(firstLine, k) {
			return k, nil
		}
	}
	// Fallback: scan the entire stripped response. Handles models that
	// emit the region key on a later line (e.g. after a "Region:" label
	// or after a stripped <think> block left whitespace at the start).
	for _, k := range regionKeys {
		if strings.Contains(raw, k) {
			return k, nil
		}
	}
	// Unrecognised content — return "" without error so the retry helper
	// doesn't burn its budget on a problem retries can't fix.
	return "", nil
}

// ---------------------------------------------------------------------------
// File I/O
// ---------------------------------------------------------------------------

func (mc *MemoryClassifier) loadMemories() (
	envelope map[string]json.RawMessage,
	records []json.RawMessage,
	recordsKey string,
	err error,
) {
	data, err := os.ReadFile(mc.memPath)
	if err != nil {
		return nil, nil, "", err
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, nil, "", fmt.Errorf("parse memories.json: %w", err)
	}

	// Prefer "memories" key; fall back to "items".
	for _, key := range []string{"memories", "items"} {
		if raw, ok := envelope[key]; ok {
			var recs []json.RawMessage
			if err := json.Unmarshal(raw, &recs); err == nil && len(recs) > 0 {
				return envelope, recs, key, nil
			}
		}
	}
	return envelope, nil, "", nil // empty file — nothing to do
}

// writeBack encodes allRecs back into the envelope and writes atomically.
// It syncs both "memories" and "items" keys if they exist.
func (mc *MemoryClassifier) writeBack(
	envelope map[string]json.RawMessage,
	allRecs []map[string]interface{},
	original []json.RawMessage,
	recordsKey string,
) error {
	encoded := make([]json.RawMessage, len(allRecs))
	for i, rec := range allRecs {
		if rec == nil {
			encoded[i] = original[i]
			continue
		}
		b, err := json.Marshal(rec)
		if err != nil {
			encoded[i] = original[i]
			continue
		}
		encoded[i] = json.RawMessage(b)
	}

	recsJSON, err := json.Marshal(encoded)
	if err != nil {
		return err
	}

	// Update both "memories" and "items" if present, keeping them in sync.
	if _, ok := envelope["memories"]; ok {
		envelope["memories"] = json.RawMessage(recsJSON)
	}
	if _, ok := envelope["items"]; ok {
		envelope["items"] = json.RawMessage(recsJSON)
	}
	// If only the recordsKey is present (edge case), update that.
	if _, ok := envelope["memories"]; !ok {
		if _, ok := envelope["items"]; !ok {
			envelope[recordsKey] = json.RawMessage(recsJSON)
		}
	}
	// Keep count in sync.
	if countRaw, err := json.Marshal(len(allRecs)); err == nil {
		envelope["count"] = json.RawMessage(countRaw)
	}

	out, err := json.Marshal(envelope)
	if err != nil {
		return err
	}

	tmp := mc.memPath + ".classify.tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, mc.memPath); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// fanout emits a memory_updated event for each item whose region was set
// during this pass, so connected dashboards update without a page reload.
func (mc *MemoryClassifier) fanout(allRecs []map[string]interface{}, items []recItem) {
	if mc.hub == nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, item := range items {
		rec := allRecs[item.idx]
		if rec == nil {
			continue
		}
		id, _ := rec["id"].(string)
		if id == "" {
			continue
		}
		mc.hub.Fanout(Event{
			SchemaVersion: SchemaVersion,
			Type:          "memory_updated",
			Timestamp:     now,
			AdapterID:     "sd-core-classifier",
			Payload: map[string]interface{}{
				"memory_id": id,
				"region":    rec["region"],
			},
		})
	}
}

// ---------------------------------------------------------------------------
// Cache
// ---------------------------------------------------------------------------

func (mc *MemoryClassifier) cacheSet(hash, region, tier string) {
	mc.mu.Lock()
	mc.cache[hash] = cacheEntry{Region: region, Tier: tier}
	mc.mu.Unlock()
}

func (mc *MemoryClassifier) loadCache() {
	data, err := os.ReadFile(mc.cachePath)
	if err != nil {
		return
	}
	var c map[string]cacheEntry
	if err := json.Unmarshal(data, &c); err != nil {
		log.Printf("classifier: cache parse error, starting fresh: %v", err)
		return
	}
	mc.cache = c
	log.Printf("classifier: loaded %d cache entries", len(c))
}

func (mc *MemoryClassifier) saveCache() {
	mc.mu.Lock()
	data, err := json.Marshal(mc.cache)
	mc.mu.Unlock()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(mc.cachePath), 0o755); err != nil {
		return
	}
	tmp := mc.cachePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	os.Rename(tmp, mc.cachePath)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func normalizeTags(rec map[string]interface{}) []string {
	switch v := rec["tags"].(type) {
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, t := range v {
			if s, ok := t.(string); ok && s != "" {
				out = append(out, strings.ToLower(s))
			}
		}
		return out
	case []string:
		out := make([]string, 0, len(v))
		for _, s := range v {
			if s != "" {
				out = append(out, strings.ToLower(s))
			}
		}
		return out
	case string:
		var out []string
		for _, t := range strings.Split(v, ",") {
			if t = strings.TrimSpace(t); t != "" {
				out = append(out, strings.ToLower(t))
			}
		}
		return out
	}
	return nil
}

func recStr(rec map[string]interface{}, key string) string {
	s, _ := rec[key].(string)
	return s
}

func contentHash(rec map[string]interface{}) string {
	text := recStr(rec, "text")
	if len(text) > 2000 {
		text = text[:2000]
	}
	tags := strings.Join(normalizeTags(rec), ",")
	ctx := recStr(rec, "context")
	h := sha1.Sum([]byte(text + "|" + tags + "|" + ctx))
	return fmt.Sprintf("%x", h[:8])
}
