// Bubble generator — server-side LLM thought-bubble loop, neuron-keyed.
//
// Runs as a goroutine inside SD Core. There are two kinds of generation:
//
//   1. BASE generation (memory_id only): every N seconds the generator
//      picks one memory that doesn't yet have a thought attached and asks
//      Ollama for a short inner-monologue thought rooted in that memory's
//      text. Each neuron in the dashboard ends up with at most one base
//      thought tied to its underlying memory.
//
//   2. MUTATION generation (memory_id + mode_key): when the dashboard
//      asks for a thought through the lens of one or more cognitive modes
//      (e.g. modeKey "adhd", or "adhd+tism" for combos), we look up the
//      base thought and ask Ollama to rewrite it through that mode's
//      lens. The result is cached under thoughts[memID].modes[modeKey],
//      so toggling the mode off and on again is free — no re-prompt.
//
// Both kinds share a priority queue that lets dashboards POST to the
// front when they want a specific slot filled now (instead of waiting
// for the next round-robin tick).
//
// Configuration: same env vars and config file as before — the JSON
// shape changed (top-level `thoughts` array of {memory_id, text, modes}
// objects, schema v3). Old `bubbles []string` files are treated as empty
// on load and replaced.

package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"
)

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------
type BubbleConfig struct {
	Model             string            `json:"model"`
	IntervalSeconds   int               `json:"interval_seconds"`
	Temperature       float64           `json:"temperature"`
	NumPredict        int               `json:"num_predict"`
	MaxPool           int               `json:"max_pool"`
	MemorySampleN     int               `json:"memory_sample_n"`
	SimilarityCheck   bool              `json:"similarity_check"`
	SimilaritySampleN int               `json:"similarity_sample_n"`
	Flavors           []string          `json:"flavors"`
	Prompts           map[string]string `json:"prompts"`
	// Mode mutators — the description for each cognitive mode that ends
	// up in the mutate prompt. Keyed by lowercase mode id (matches the
	// dashboard's modeMgr). Combo keys ("adhd+tism") are looked up by
	// splitting on "+"; each part contributes its description chained.
	ModeDescriptions map[string]string `json:"mode_descriptions"`
}

func defaultBubbleConfig() BubbleConfig {
	return BubbleConfig{
		Model:             "llama3.2:3b",
		IntervalSeconds:   30,
		Temperature:       0.85,
		NumPredict:        80,
		MaxPool:           5000,
		MemorySampleN:     1,
		SimilarityCheck:   true,
		SimilaritySampleN: 5,
		Flavors: []string{
			"a passing reflection on this memory",
			"a small worry about something in this memory",
			"a niche question this memory would surface",
			"\"wait, did I…?\" intrusive checking related to this memory",
			"a connection between this memory and something else",
			"a \"I should…\" / \"I keep meaning to…\" half-finished plan from this",
			"a hyperfixation flicker on a detail from this memory",
			"a quiet observation rooted in this memory",
			"an internal counter-argument with themselves about this",
			"a craving / preference that fits this memory's context",
			"a delight about something tiny that fits this memory's world",
			"an \"I forgot…\" surfacing of an old commitment in this memory",
		},
		Prompts: map[string]string{
			"generate":   defaultGeneratePrompt,
			"similarity": defaultSimilarityPrompt,
			"mutate":     defaultMutatePrompt,
		},
		ModeDescriptions: defaultModeDescriptions(),
	}
}

func defaultModeDescriptions() map[string]string {
	// Keys MUST match the dashboard's CognitiveMode `id` field (lowercase,
	// hyphenated where applicable). Keep this in sync with index.html's
	// mode classes — modeKey is a sorted+joined active-id string, so a
	// typo here means the lookup misses and the mode contributes no lens.
	return map[string]string{
		"adhd":             "Interrupt the original mid-thought with a tangent or quick pivot, parenthetical aside, or a 'wait, did I—' break.",
		"voices":           "Phrase as one voice in a chorus — contrarian or fragmented, possibly questioning itself.",
		"tism":             "Sharpen into a hyperfixation detail or factoid; more specific and technical, with a precise number, name, or term.",
		"time-blindness":   "Warp the time-sense; reference a moment hours or years off, or use vague time markers ('was that yesterday or last month').",
		"stim":             "Make it rhythmic or repetitive — a fragment that loops, mirrors, or has a stim cadence.",
		"hyperempathy":     "Frame as absorbing someone else's feelings; include a feeling someone else might be having about this.",
		"synesthesia":      "Add a cross-sensory element — color of a sound, taste of an idea, texture of a feeling.",
		"sensory-soothing": "Add comfort warmth, soft texture, or a soothing sensory anchor that fits the original.",
		"special-interest": "Go deep on a niche fact, lore, or technical detail tangentially related to the original.",
		"pattern-seeker":   "Frame as noticing a connection, recurrence, or pattern between this and something else.",
	}
}

const defaultGeneratePrompt = `You generate a single short inner-monologue thought (4-12 words) for a brain dashboard.
The thought MUST clearly stem from THIS specific memory belonging to the dashboard's owner:

  {{ .Memory }}
{{ if .Connected }}
This memory is connected to:
{{ range $i, $m := .Connected }}  - {{ $m }}
{{ end }}{{ end }}
The thought should sound like it could come from someone currently working on, worrying about, or noodling on THIS specific memory. Reference at least one concrete thing from THIS memory (a project name, a tool, a concept, a person, a pattern, a number).

Make this one: {{ .Flavor }}.
{{ if .RecentEvents }}Recent activity in their dashboard: {{ .RecentEvents }}.{{ end }}

Examples of GOOD thoughts (just to show shape):
  - "did I ever close that PR about the migration"
  - "the cascade only fires on tuesdays for some reason"
  - "should the classifier just check tags first"
Examples of BAD generic thoughts (do NOT produce):
  - "i wonder if clouds are sad sometimes" (vague, no anchor)
  - "processing boot sequence" (assistant voice)
  - "Initializing context variables for next session" (chatbot voice)

Respond with JUST the thought. No quotes. Lowercase is fine. Fragments are fine.`

const defaultSimilarityPrompt = `Is the NEW phrase a near-duplicate of any EXISTING phrase below?
Bar is HIGH: only reply yes if it's essentially the SAME thought (same subject, same point, paraphrased). Different topic = not a duplicate, even if the genre or vibe is similar.
Default to no when in doubt.

NEW: {{ .New }}

EXISTING:
{{ range $i, $p := .Existing }}{{ add $i 1 }}. {{ $p }}
{{ end }}
Respond with exactly one word: yes or no.`

const defaultMutatePrompt = `You rewrite an inner-monologue thought through a cognitive-mode lens.

ORIGINAL THOUGHT (don't change the underlying subject):
  {{ .Original }}

APPLY THIS LENS:
{{ range $i, $d := .Lenses }}  - {{ $d }}
{{ end }}
Constraints:
- Keep it short (4-14 words; under 20 absolute max).
- Stay rooted in the same subject — same project / tool / concept as the original.
- Apply the lens shape only; do NOT invent unrelated content.
- Lowercase fragments are fine.
- Do NOT echo the original verbatim; the result must read as a clearly mode-flavored variant of it.

Respond with JUST the mutated thought, nothing else.`

func loadBubbleConfig(path string) BubbleConfig {
	cfg := defaultBubbleConfig()
	if path == "" {
		return cfg
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("bubble: config read %s: %v (using defaults)", path, err)
		}
		return cfg
	}
	var override BubbleConfig
	if err := json.Unmarshal(data, &override); err != nil {
		log.Printf("bubble: config parse %s: %v (using defaults)", path, err)
		return cfg
	}
	if override.Model != "" {
		cfg.Model = override.Model
	}
	if override.IntervalSeconds > 0 {
		cfg.IntervalSeconds = override.IntervalSeconds
	}
	if override.Temperature > 0 {
		cfg.Temperature = override.Temperature
	}
	if override.NumPredict > 0 {
		cfg.NumPredict = override.NumPredict
	}
	if override.MaxPool > 0 {
		cfg.MaxPool = override.MaxPool
	}
	if override.MemorySampleN > 0 {
		cfg.MemorySampleN = override.MemorySampleN
	}
	if override.SimilaritySampleN > 0 {
		cfg.SimilaritySampleN = override.SimilaritySampleN
	}
	cfg.SimilarityCheck = override.SimilarityCheck || cfg.SimilarityCheck
	if len(override.Flavors) > 0 {
		cfg.Flavors = override.Flavors
	}
	for k, v := range override.Prompts {
		if v != "" {
			cfg.Prompts[k] = v
		}
	}
	for k, v := range override.ModeDescriptions {
		if v != "" {
			cfg.ModeDescriptions[k] = v
		}
	}
	log.Printf("bubble: loaded config from %s", path)
	return cfg
}

// ---------------------------------------------------------------------------
// On-disk schema (v3 — adds per-mode mutations under each thought)
// (v4 — adds SourceHash so edits to the underlying memory invalidate the
//  cached bubble; without this, bubbles stay frozen across memory edits.)
// ---------------------------------------------------------------------------
type ThoughtEntry struct {
	MemoryID    string            `json:"memory_id"`
	Text        string            `json:"text"`
	Modes       map[string]string `json:"modes,omitempty"` // modeKey ("adhd", "adhd+tism") → mutated text
	// SourceHash is sha1 of (memory.text + "\n" + sorted(memory.tags)).
	// When the underlying memory is edited (text or tags), the hash drifts
	// and setBaseThought regenerates instead of skipping. Empty on legacy
	// rows from before v4 — those get treated as stale on first comparison.
	SourceHash  string `json:"source_hash,omitempty"`
	GeneratedAt string `json:"generated_at,omitempty"`
}

// computeSourceHash returns the canonical hash for a memory's content.
// Stable: identical (text, tags) → identical hash. Tag order is normalised
// (sorted, lowercased) so a tag-reorder doesn't churn bubbles.
func computeSourceHash(text string, tags []string) string {
	normalised := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t != "" {
			normalised = append(normalised, t)
		}
	}
	sort.Strings(normalised)
	h := sha1.Sum([]byte(text + "\n" + strings.Join(normalised, ",")))
	return fmt.Sprintf("%x", h[:10])
}

type thoughtBubblesFile struct {
	SchemaVersion string         `json:"schema_version"`
	UpdatedAt     string         `json:"updated_at"`
	Thoughts      []ThoughtEntry `json:"thoughts"`
	Bubbles       []string       `json:"bubbles,omitempty"` // legacy v1 — ignored on load
}

// ---------------------------------------------------------------------------
// Generator
// ---------------------------------------------------------------------------

// genTask drives both kinds of generation through one queue/worker.
// ModeKey == "" means base generation; otherwise it's a mutation request
// for an existing thought.
type genTask struct {
	MemoryID string
	ModeKey  string
}

func (t genTask) dedupKey() string {
	if t.ModeKey == "" {
		return t.MemoryID
	}
	return t.MemoryID + "::" + t.ModeKey
}

type BubbleGenerator struct {
	cfg BubbleConfig
	// providerFn resolves the realtime provider on each call so a
	// PUT /settings/providers/tier1 hot-swap takes effect at the next
	// bubble without a Core restart. main.go passes router.ForRealtime.
	providerFn func() LLMProvider
	poolPath   string
	memPath    string
	synPath    string
	hub        *Hub
	ring       *RingBuffer
	bank       *Bank // optional; used to read tier1_think_off setting

	mu   sync.Mutex
	pool map[string]*ThoughtEntry // memoryID → entry (Text + Modes)

	priorityCh  chan genTask
	prioritySet map[string]bool // dedup keyed by genTask.dedupKey()

	tmpls map[string]*template.Template
}

// NewBubbleGenerator builds a generator wired to a provider-resolver.
// v2.6 Bundle C: removed ollamaURL + httpClient fields. main.go passes
// router.ForRealtime (method value) so bubble generation shares the
// same provider (and the same withRateLimitRetry behaviour) as the
// classifier and /recall. nil or always-nil providerFn is allowed —
// generateOnce no-ops in that case.
func NewBubbleGenerator(cfg BubbleConfig, providerFn func() LLMProvider, poolPath, memPath, synPath string, hub *Hub, ring *RingBuffer, bank *Bank) *BubbleGenerator {
	bg := &BubbleGenerator{
		cfg:         cfg,
		providerFn:  providerFn,
		poolPath:    poolPath,
		memPath:     memPath,
		synPath:     synPath,
		hub:         hub,
		ring:        ring,
		bank:        bank,
		pool:        map[string]*ThoughtEntry{},
		priorityCh:  make(chan genTask, 512),
		prioritySet: map[string]bool{},
		tmpls:       map[string]*template.Template{},
	}
	bg.loadPool()
	bg.compileTemplates()
	return bg
}

func (g *BubbleGenerator) compileTemplates() {
	funcs := template.FuncMap{
		"add": func(a, b int) int { return a + b },
	}
	for k, body := range g.cfg.Prompts {
		t, err := template.New(k).Funcs(funcs).Parse(body)
		if err != nil {
			log.Printf("bubble: template parse %q: %v", k, err)
			continue
		}
		g.tmpls[k] = t
	}
}

// Run blocks until ctx is cancelled.
func (g *BubbleGenerator) Run(ctx context.Context) {
	interval := time.Duration(g.cfg.IntervalSeconds) * time.Second
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	providerName := "(none)"
	if p := g.provider(); p != nil {
		providerName = p.Name()
	}
	log.Printf("bubble: starting per-neuron generator (provider=%s, every %s, pool=%s)",
		providerName, interval, g.poolPath)

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := g.generateOnce(ctx); err != nil {
				if !errors.Is(err, context.Canceled) {
					log.Printf("bubble: %v", err)
				}
			}
			next := interval
			if g.priorityLen() > 0 {
				next = 5 * time.Second
			}
			timer.Reset(next)
		}
	}
}

// ---------------------------------------------------------------------------
// Pool persistence
// ---------------------------------------------------------------------------

func (g *BubbleGenerator) loadPool() {
	g.mu.Lock()
	defer g.mu.Unlock()
	data, err := os.ReadFile(g.poolPath)
	if err != nil {
		return
	}
	var f thoughtBubblesFile
	if err := json.Unmarshal(data, &f); err != nil {
		log.Printf("bubble: pool parse %s: %v", g.poolPath, err)
		return
	}
	if len(f.Thoughts) == 0 && len(f.Bubbles) > 0 {
		log.Printf("bubble: legacy pool detected (%d unkeyed strings) — discarding, generator will refill keyed by memory_id", len(f.Bubbles))
		return
	}
	for i := range f.Thoughts {
		t := &f.Thoughts[i]
		if t.MemoryID == "" || t.Text == "" {
			continue
		}
		if t.Modes == nil {
			t.Modes = map[string]string{}
		}
		g.pool[t.MemoryID] = t
	}
	log.Printf("bubble: loaded %d keyed thoughts from %s", len(g.pool), g.poolPath)
}

// setBaseThought stores text for memID and persists. Returns true when the
// pool was modified (caller broadcasts on true).
//
// Replaces the legacy "already filled → skip" check with a "still fresh"
// check: if the existing entry's SourceHash matches the current memory's
// hash, we keep it; otherwise we overwrite the entry and clear stale mode
// mutations (mutations are derived from the base thought, so a base change
// invalidates them).
func (g *BubbleGenerator) setBaseThought(memID, text, sourceHash string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if existing, ok := g.pool[memID]; ok {
		if sourceHash != "" && existing.SourceHash == sourceHash {
			return false // matches current memory state — keep cached bubble
		}
		// Source drifted (memory edited) — overwrite. Mode mutations are
		// derived from the base text, so they get wiped too.
	}
	g.pool[memID] = &ThoughtEntry{
		MemoryID:    memID,
		Text:        text,
		SourceHash:  sourceHash,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Modes:       map[string]string{},
	}
	delete(g.prioritySet, memID)
	g.persistLocked()
	return true
}

// InvalidateAndEnqueue is called from handlers_p5.go after UpdateMemory
// touches a memory's text or tags. Wipes the cached mode mutations
// (those derive from base text and become stale on edit) and enqueues
// the base bubble for regeneration. The base itself only actually
// regenerates if the SourceHash drifted; the generator does the check
// when it picks up the priority task.
func (g *BubbleGenerator) InvalidateAndEnqueue(memID string) {
	if g == nil || memID == "" {
		return
	}
	g.mu.Lock()
	if entry, ok := g.pool[memID]; ok {
		entry.Modes = map[string]string{}
		g.persistLocked()
	}
	g.mu.Unlock()
	g.EnqueuePriority(memID)
}

// setMutation stores a mode-mutated text under thoughts[memID].modes[modeKey].
// Returns (true, baseText) if newly added; (false, baseText) if the slot was
// already filled or the base is missing.
func (g *BubbleGenerator) setMutation(memID, modeKey, text string) (bool, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	entry, ok := g.pool[memID]
	if !ok {
		return false, ""
	}
	if entry.Modes == nil {
		entry.Modes = map[string]string{}
	}
	if _, exists := entry.Modes[modeKey]; exists {
		return false, entry.Text
	}
	entry.Modes[modeKey] = text
	delete(g.prioritySet, memID+"::"+modeKey)
	g.persistLocked()
	return true, entry.Text
}

func (g *BubbleGenerator) persistLocked() {
	if g.poolPath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(g.poolPath), 0o755); err != nil {
		log.Printf("bubble: mkdir %s: %v", filepath.Dir(g.poolPath), err)
		return
	}
	thoughts := make([]ThoughtEntry, 0, len(g.pool))
	for _, t := range g.pool {
		thoughts = append(thoughts, *t)
	}
	out := thoughtBubblesFile{
		SchemaVersion: SchemaVersion,
		UpdatedAt:     time.Now().UTC().Format(time.RFC3339),
		Thoughts:      thoughts,
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		log.Printf("bubble: marshal: %v", err)
		return
	}
	tmp := g.poolPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Printf("bubble: write %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, g.poolPath); err != nil {
		log.Printf("bubble: rename %s -> %s: %v", tmp, g.poolPath, err)
	}
}

// ---------------------------------------------------------------------------
// Priority queue (lazy on-demand)
// ---------------------------------------------------------------------------

// EnqueuePriority asks the generator to fill memID's BASE slot next.
// Returns true if newly enqueued.
func (g *BubbleGenerator) EnqueuePriority(memID string) bool {
	return g.enqueueTask(genTask{MemoryID: memID, ModeKey: ""})
}

// EnqueueMutation asks the generator to fill memID's mode-mutation slot
// next. modeKey is the canonical sorted+joined active mode IDs (e.g.
// "adhd" or "adhd+tism"). Returns true if newly enqueued.
func (g *BubbleGenerator) EnqueueMutation(memID, modeKey string) bool {
	if modeKey == "" {
		return false
	}
	return g.enqueueTask(genTask{MemoryID: memID, ModeKey: modeKey})
}

func (g *BubbleGenerator) enqueueTask(t genTask) bool {
	if t.MemoryID == "" {
		return false
	}
	g.mu.Lock()
	// Already filled? Skip ONLY when the cached entry's SourceHash still
	// matches the underlying memory. Without this check, a tag/text edit
	// would never refresh the bubble — the very bug Phase 2 of the
	// freshness handoff fixes.
	if entry, ok := g.pool[t.MemoryID]; ok {
		if t.ModeKey == "" {
			// Compute current hash; if it matches, skip. We pay one disk
			// read here, but enqueue isn't on a hot path.
			g.mu.Unlock()
			_, _, currentHash := g.lookupMemoryWithHash(t.MemoryID)
			g.mu.Lock()
			entry, ok = g.pool[t.MemoryID]
			if !ok {
				// Pool entry vanished while we were reading from disk; fall
				// through to the enqueue path.
			} else if currentHash != "" && entry.SourceHash == currentHash {
				g.mu.Unlock()
				return false
			}
		}
		if t.ModeKey != "" && entry != nil && entry.Modes != nil {
			if _, has := entry.Modes[t.ModeKey]; has {
				g.mu.Unlock()
				return false
			}
		}
	} else if t.ModeKey != "" {
		// Mutation requested but no base yet — first enqueue the base, then
		// the mutation. The mutation will land once base is filled.
		// (We don't auto-chain server-side; the client's pickForNeuron
		// will re-request the mutation after seeing the base land.)
		g.mu.Unlock()
		g.EnqueuePriority(t.MemoryID)
		return false
	}
	if g.prioritySet[t.dedupKey()] {
		g.mu.Unlock()
		return false
	}
	g.prioritySet[t.dedupKey()] = true
	g.mu.Unlock()
	select {
	case g.priorityCh <- t:
	default:
		g.mu.Lock()
		delete(g.prioritySet, t.dedupKey())
		g.mu.Unlock()
		return false
	}
	return true
}

func (g *BubbleGenerator) priorityLen() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.prioritySet)
}

// nextPriorityTask drains one task from the queue, dropping any that
// were filled in the meantime.
func (g *BubbleGenerator) nextPriorityTask() (genTask, bool) {
	for {
		select {
		case t := <-g.priorityCh:
			g.mu.Lock()
			entry, ok := g.pool[t.MemoryID]
			if !ok && t.ModeKey != "" {
				// Need base first — re-queue via EnqueuePriority and skip.
				g.mu.Unlock()
				g.EnqueuePriority(t.MemoryID)
				continue
			}
			if ok && t.ModeKey == "" {
				// Base already filled — but only really skip if its
				// SourceHash matches the current memory. A drifted hash
				// means the cached entry is stale and we DO want to run
				// generateBase again to refresh.
				g.mu.Unlock()
				_, _, currentHash := g.lookupMemoryWithHash(t.MemoryID)
				if currentHash != "" && entry.SourceHash == currentHash {
					continue // fresh — skip
				}
				return t, true // stale — run generateBase
			}
			if ok && t.ModeKey != "" && entry.Modes != nil {
				if _, has := entry.Modes[t.ModeKey]; has {
					g.mu.Unlock()
					continue
				}
			}
			g.mu.Unlock()
			return t, true
		default:
			return genTask{}, false
		}
	}
}

// ---------------------------------------------------------------------------
// Generation
// ---------------------------------------------------------------------------

// provider snapshots the current realtime provider. nil when unset.
func (g *BubbleGenerator) provider() LLMProvider {
	if g == nil || g.providerFn == nil {
		return nil
	}
	return g.providerFn()
}

// generateOnce dispatches one cycle: priority queue (base or mutation)
// first, falling back to a random unfilled base slot.
func (g *BubbleGenerator) generateOnce(ctx context.Context) error {
	if g.provider() == nil {
		return errors.New("bubble: no provider configured")
	}
	if t, ok := g.nextPriorityTask(); ok {
		if t.ModeKey == "" {
			return g.generateBase(ctx, t.MemoryID)
		}
		return g.generateMutation(ctx, t.MemoryID, t.ModeKey)
	}
	// No priority work — pick a random unfilled base slot.
	memID, _, _ := g.pickRandomUnfilledBase()
	if memID == "" {
		return nil
	}
	return g.generateBase(ctx, memID)
}

// generateBase produces a base thought for memID and stores it.
func (g *BubbleGenerator) generateBase(ctx context.Context, memID string) error {
	tmpl, ok := g.tmpls["generate"]
	if !ok {
		return errors.New("generate template not loaded")
	}
	memText, connected, sourceHash := g.lookupMemoryWithHash(memID)
	if memText == "" {
		return nil
	}
	// Fast-path: if a fresh entry already exists with the current source
	// hash, skip the Ollama round-trip. Saves work on enqueue-storms when
	// the underlying memory hasn't actually changed.
	g.mu.Lock()
	if existing, ok := g.pool[memID]; ok && existing.SourceHash != "" && existing.SourceHash == sourceHash {
		g.mu.Unlock()
		return nil
	}
	g.mu.Unlock()
	flavor := g.pickFlavor()
	recent := g.recentEventsLine()

	var promptBuf bytes.Buffer
	if err := tmpl.Execute(&promptBuf, map[string]interface{}{
		"Memory":       memText,
		"Connected":    connected,
		"Flavor":       flavor,
		"RecentEvents": recent,
	}); err != nil {
		return fmt.Errorf("template execute: %w", err)
	}

	resp, err := g.callOllama(ctx, promptBuf.String())
	if err != nil {
		return fmt.Errorf("ollama: %w", err)
	}
	phrase := normalizePhrase(resp)
	if len(phrase) < 4 {
		return nil
	}
	if g.cfg.SimilarityCheck && g.isSimilarToPool(ctx, phrase) {
		return nil
	}
	if !g.setBaseThought(memID, phrase, sourceHash) {
		return nil
	}
	g.hub.Fanout(Event{
		SchemaVersion: SchemaVersion,
		Type:          "bubble_added",
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
		AdapterID:     "sd-core-bubble-gen",
		Payload: map[string]interface{}{
			"memory_id": memID,
			"text":      phrase,
			"flavor":    flavor,
		},
	})
	g.mu.Lock()
	poolLen := len(g.pool)
	g.mu.Unlock()
	log.Printf("bubble: + [%s] %q (pool=%d)", memID, phrase, poolLen)
	return nil
}

// generateMutation produces a mode-mutated version of memID's existing
// base thought and stores it under modes[modeKey].
func (g *BubbleGenerator) generateMutation(ctx context.Context, memID, modeKey string) error {
	g.mu.Lock()
	entry, ok := g.pool[memID]
	g.mu.Unlock()
	if !ok {
		// Base missing — re-queue it; mutation will be requested again later
		// by the dashboard once the base lands.
		g.EnqueuePriority(memID)
		return nil
	}
	tmpl, ok2 := g.tmpls["mutate"]
	if !ok2 {
		return errors.New("mutate template not loaded")
	}
	lenses := g.lensesForKey(modeKey)
	if len(lenses) == 0 {
		return nil
	}
	var promptBuf bytes.Buffer
	if err := tmpl.Execute(&promptBuf, map[string]interface{}{
		"Original": entry.Text,
		"Lenses":   lenses,
		"ModeKey":  modeKey,
	}); err != nil {
		return fmt.Errorf("mutate template: %w", err)
	}
	resp, err := g.callOllama(ctx, promptBuf.String())
	if err != nil {
		return fmt.Errorf("ollama: %w", err)
	}
	phrase := normalizePhrase(resp)
	if len(phrase) < 4 {
		return nil
	}
	added, base := g.setMutation(memID, modeKey, phrase)
	if !added {
		return nil
	}
	g.hub.Fanout(Event{
		SchemaVersion: SchemaVersion,
		Type:          "bubble_mutated",
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
		AdapterID:     "sd-core-bubble-gen",
		Payload: map[string]interface{}{
			"memory_id": memID,
			"mode_key":  modeKey,
			"text":      phrase,
			"base":      base,
		},
	})
	log.Printf("bubble: ~ [%s|%s] %q  (was: %q)", memID, modeKey, phrase, base)
	return nil
}

// lensesForKey turns "adhd" or "adhd+tism" into a slice of mode
// descriptions for the prompt. Unknown sub-keys are skipped.
func (g *BubbleGenerator) lensesForKey(modeKey string) []string {
	parts := strings.Split(modeKey, "+")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(strings.ToLower(p))
		if p == "" {
			continue
		}
		if d, ok := g.cfg.ModeDescriptions[p]; ok && d != "" {
			out = append(out, d)
		}
	}
	return out
}

// lookupMemory returns the (truncated) text for memID and up to 2
// synapse-connected neighbour-snippets.
func (g *BubbleGenerator) lookupMemory(memID string) (string, []string) {
	text, connected, _ := g.lookupMemoryWithHash(memID)
	return text, connected
}

// lookupMemoryWithHash extends lookupMemory with the canonical source hash
// (sha1 of full-fidelity text + sorted tags). Used by generateBase so the
// stored ThoughtEntry knows when its source memory has drifted.
//
// Hash is computed from the FULL memory text + tags (not the truncated
// display string fed to the prompt) — that way a tag-only edit invalidates
// the bubble even though the prompt input wouldn't change. Returns
// ("", nil, "") when the memory is missing.
func (g *BubbleGenerator) lookupMemoryWithHash(memID string) (string, []string, string) {
	if g.memPath == "" || memID == "" {
		return "", nil, ""
	}
	data, err := os.ReadFile(g.memPath)
	if err != nil {
		return "", nil, ""
	}
	var wrapper struct {
		Memories []memRecord `json:"memories"`
		Items    []memRecord `json:"items"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return "", nil, ""
	}
	all := wrapper.Memories
	if len(all) == 0 {
		all = wrapper.Items
	}
	idToText := make(map[string]string, len(all))
	var target *memRecord
	for i := range all {
		m := &all[i]
		t := strings.TrimSpace(m.Text)
		if len(t) < 8 || m.ID == "" {
			continue
		}
		if len(t) > 240 {
			idToText[m.ID] = strings.Join(strings.Fields(t[:237]+"…"), " ")
		} else {
			idToText[m.ID] = strings.Join(strings.Fields(t), " ")
		}
		if m.ID == memID {
			target = m
		}
	}
	memText, ok := idToText[memID]
	if !ok || target == nil {
		return "", nil, ""
	}
	connected := g.connectedTexts(memID, idToText, 2)

	// Coerce tags from interface{} to []string for the hash.
	var tags []string
	switch t := target.Tags.(type) {
	case []interface{}:
		for _, v := range t {
			if s, ok := v.(string); ok {
				tags = append(tags, s)
			}
		}
	case []string:
		tags = t
	}
	hash := computeSourceHash(target.Text, tags)
	return memText, connected, hash
}

// pickRandomUnfilledBase returns a memory_id whose base thought hasn't
// been generated yet.
func (g *BubbleGenerator) pickRandomUnfilledBase() (string, string, []string) {
	if g.memPath == "" {
		return "", "", nil
	}
	data, err := os.ReadFile(g.memPath)
	if err != nil {
		return "", "", nil
	}
	var wrapper struct {
		Memories []memRecord `json:"memories"`
		Items    []memRecord `json:"items"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return "", "", nil
	}
	all := wrapper.Memories
	if len(all) == 0 {
		all = wrapper.Items
	}
	idToText := map[string]string{}
	for _, m := range all {
		t := strings.TrimSpace(m.Text)
		if len(t) < 8 || m.ID == "" {
			continue
		}
		if len(t) > 240 {
			t = t[:237] + "…"
		}
		idToText[m.ID] = strings.Join(strings.Fields(t), " ")
	}
	if len(idToText) == 0 {
		return "", "", nil
	}
	g.mu.Lock()
	var unfilled []string
	for id := range idToText {
		if _, ok := g.pool[id]; !ok {
			unfilled = append(unfilled, id)
		}
	}
	g.mu.Unlock()
	if len(unfilled) == 0 {
		return "", "", nil
	}
	chosen := unfilled[rand.Intn(len(unfilled))]
	return chosen, idToText[chosen], g.connectedTexts(chosen, idToText, 2)
}

func (g *BubbleGenerator) connectedTexts(memID string, idToText map[string]string, max int) []string {
	if g.synPath == "" || max <= 0 {
		return nil
	}
	data, err := os.ReadFile(g.synPath)
	if err != nil {
		return nil
	}
	var synWrapper struct {
		Edges []struct {
			A string `json:"a"`
			B string `json:"b"`
		} `json:"edges"`
	}
	if json.Unmarshal(data, &synWrapper) != nil {
		return nil
	}
	var neighbours []string
	for _, e := range synWrapper.Edges {
		var other string
		if e.A == memID {
			other = e.B
		} else if e.B == memID {
			other = e.A
		} else {
			continue
		}
		if t, ok := idToText[other]; ok {
			snippet := t
			if len(snippet) > 100 {
				snippet = snippet[:97] + "…"
			}
			neighbours = append(neighbours, snippet)
		}
	}
	if len(neighbours) <= max {
		return neighbours
	}
	rand.Shuffle(len(neighbours), func(i, j int) {
		neighbours[i], neighbours[j] = neighbours[j], neighbours[i]
	})
	return neighbours[:max]
}

func (g *BubbleGenerator) isSimilarToPool(ctx context.Context, phrase string) bool {
	tmpl, ok := g.tmpls["similarity"]
	if !ok {
		return false
	}
	samples := g.randomPoolSample(g.cfg.SimilaritySampleN)
	if len(samples) == 0 {
		return false
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]interface{}{
		"New":      phrase,
		"Existing": samples,
	}); err != nil {
		return false
	}
	resp, err := g.callOllama(ctx, buf.String())
	if err != nil {
		return false
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(resp)), "yes")
}

// callOllama runs one Chat() against the currently-resolved provider.
// Bundle C: the outer withRateLimitRetry wrap is gone — provider.Chat()
// already applies its own retry helper internally, so wrapping again
// would double-retry to 5×5=25 attempts. Function name kept; the
// "Ollama" suffix is historical only.
func (g *BubbleGenerator) callOllama(ctx context.Context, prompt string) (string, error) {
	p := g.provider()
	if p == nil {
		return "", errors.New("bubble: no provider configured")
	}
	// `[think:off]` system message disables reasoning-trace emission on
	// reasoning-capable models. Bubble generation is latency-sensitive
	// (per-neuron, every 30s) and the thinking trace would blow the
	// NumPredict budget. No-op on non-reasoning models. Toggleable via
	// settings.tier1_think_off (default ON).
	systemMsg := "[think:off]"
	if g.bank != nil && !g.bank.ThinkOffEnabled() {
		systemMsg = ""
	}
	msgs := []Message{}
	if systemMsg != "" {
		msgs = append(msgs, Message{Role: "system", Content: systemMsg})
	}
	msgs = append(msgs, Message{Role: "user", Content: prompt})
	return p.Chat(ctx, msgs, g.cfg.NumPredict)
}

func (g *BubbleGenerator) randomPoolSample(n int) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.pool) == 0 || n <= 0 {
		return nil
	}
	all := make([]string, 0, len(g.pool))
	for _, t := range g.pool {
		all = append(all, t.Text)
	}
	if n > len(all) {
		n = len(all)
	}
	rand.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	return all[:n]
}

func (g *BubbleGenerator) pickFlavor() string {
	if len(g.cfg.Flavors) == 0 {
		return "an idle thought"
	}
	return g.cfg.Flavors[rand.Intn(len(g.cfg.Flavors))]
}

func (g *BubbleGenerator) recentEventsLine() string {
	if g.ring == nil {
		return ""
	}
	events := g.ring.Recent(8)
	if len(events) == 0 {
		return ""
	}
	parts := make([]string, 0, len(events))
	for _, e := range events {
		region := ""
		if rh, ok := e.Payload["region_hint"].(string); ok && rh != "" {
			region = "@" + rh
		}
		parts = append(parts, e.Type+region)
	}
	return strings.Join(parts, ", ")
}

// CanonicalModeKey turns a slice of active mode IDs into the cache key
// used by Modes[]. Sorts + lowercases + joins with "+". Empty slice → "".
// Exposed for tests / future server-side callers.
func CanonicalModeKey(active []string) string {
	if len(active) == 0 {
		return ""
	}
	parts := make([]string, 0, len(active))
	for _, m := range active {
		m = strings.TrimSpace(strings.ToLower(m))
		if m != "" {
			parts = append(parts, m)
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "+")
}

func normalizePhrase(raw string) string {
	first := strings.SplitN(strings.TrimSpace(raw), "\n", 2)[0]
	first = strings.Trim(first, "\"' \t`")
	first = strings.Join(strings.Fields(first), " ")
	if len(first) > 140 {
		first = first[:140]
	}
	return first
}

func envOrInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
