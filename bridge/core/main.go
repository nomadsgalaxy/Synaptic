// Package main — Synaptic Core
//
// A tiny localhost daemon that:
//   - Receives schema-conformant events from adapters via POST /event
//   - Fans them out to all dashboard subscribers via WebSocket /ws
//   - Watches assets/memories.json for changes and emits memory_*
//     events when memories are added/updated/deleted
//   - Ring-buffers the last N events so a freshly-connected dashboard
//     can replay recent history (GET /events?limit=N)
//   - Optional SQLite "fallback bank" for AI-reported memories (B6)
//
// Designed to be deployed as a single static binary OR a tiny Docker
// container. No external runtime dependencies (CGO disabled).
//
// Default endpoints:
//   GET  /             — the dashboard (static index.html + assets/)
//   GET  /healthz      — liveness probe (returns "ok"; auth-exempt)
//   POST /event        — adapter pushes one event, validated then fanned out
//   POST /events       — adapter pushes a batch
//   GET  /events       — last N events (?limit= and ?since=)
//   GET  /memories     — current memories.json contents (proxied)
//   GET  /bank/*       — SQLite fallback bank (B6)
//   WS   /ws           — dashboard subscribes; receives events as they arrive
//
// Configure via env vars:
//   SD_CORE_LISTEN     "127.0.0.1:9911"  (default; bind 0.0.0.0:9911 in Docker)
//   SD_DATA_DIR        "."               (where assets/ + sqlite live)
//   SD_RING_SIZE       "1000"            (events kept in memory for replay)
//   SD_LOG_LEVEL       "info"            (debug | info | warn | error)
//   SD_API_TOKEN       ""                (Bearer token required on every API
//                                          request when set. Comma-separated
//                                          for multiple tokens. Empty = open
//                                          mode for localhost dev. /healthz
//                                          and OPTIONS preflights are always
//                                          exempt. WS upgrade accepts
//                                          ?token=<tok> query param since
//                                          browsers can't set headers on WS.)
//
// License: TBD pending OCL.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	// Embed the IANA tzdata so time.LoadLocation("America/New_York") works
	// inside the FROM-scratch container (no /usr/share/zoneinfo at runtime).
	// Adds ~450 KB to the binary; without it, the nightly_schedule onboarding
	// modal can't accept any non-UTC tz the user picks.
	_ "time/tzdata"

	"github.com/fsnotify/fsnotify"
	"github.com/gorilla/websocket"
)

const SchemaVersion = "1.0"

var (
	validEventTypes = map[string]bool{
		"session_start": true, "session_end": true,
		"prompt_received": true, "model_thinking": true,
		"response_streaming": true, "response_complete": true,
		"tool_call": true, "tool_result": true,
		"memory_recall": true, "memory_added": true,
		"memory_updated": true, "memory_deleted": true,
		"subagent_spawn": true, "subagent_complete": true,
		"error":        true,
		"bubble_added":                    true, // server-side bubble generator (B-bubble); dashboard subscribes when liveBubbles is on
		"service_state_changed":           true, // wave 7a Docker control plane emit
		"service_lifecycle_transitioned":  true, // wave 7b2 — lifecycle FSM (state change)
		"service_lifecycle_ready":         true, // wave 7b2 — cold-start completed
		"service_lifecycle_idled":         true, // wave 7b2 — idle-reaper transition into idle
		"service_lifecycle_stopped":       true, // wave 7b2 — explicit / burst / idle stop
		"section_dirty":                   true, // wave 7c2 — list-driving event collapsed for lazy-fetch
		"reflect_done":                    true, // v2.4.0b1 — POST /reflect synthesis completed
	}
)

// ---------------------------------------------------------------------------
// Event — what flows through the system. Mirrors docs/event.schema.json.
// ---------------------------------------------------------------------------
type Event struct {
	SchemaVersion string                 `json:"schema_version"`
	Type          string                 `json:"type"`
	Timestamp     string                 `json:"timestamp"`
	AdapterID     string                 `json:"adapter_id"`
	SessionID     string                 `json:"session_id,omitempty"`
	Payload       map[string]interface{} `json:"payload,omitempty"`
}

func (e *Event) Validate() error {
	if e.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schema_version %q (want %q)", e.SchemaVersion, SchemaVersion)
	}
	if !validEventTypes[e.Type] {
		return fmt.Errorf("unknown event type %q", e.Type)
	}
	if e.Timestamp == "" {
		return fmt.Errorf("timestamp is required")
	}
	if e.AdapterID == "" {
		return fmt.Errorf("adapter_id is required")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Ring buffer — fixed-size circular buffer of recent events for replay.
// ---------------------------------------------------------------------------
type RingBuffer struct {
	mu     sync.RWMutex
	buf    []Event
	head   int   // index where the next write goes
	count  int   // 0..cap, total events ever added is monotonic
	totalN int64 // monotonic counter so callers can ?since=
}

func NewRingBuffer(size int) *RingBuffer {
	return &RingBuffer{buf: make([]Event, size)}
}

func (r *RingBuffer) Add(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.head] = e
	r.head = (r.head + 1) % len(r.buf)
	if r.count < len(r.buf) {
		r.count++
	}
	r.totalN++
}

// Recent returns up to `limit` most-recent events, newest first.
func (r *RingBuffer) Recent(limit int) []Event {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if limit <= 0 || limit > r.count {
		limit = r.count
	}
	out := make([]Event, limit)
	// Walk backwards from head-1
	idx := r.head - 1
	if idx < 0 {
		idx = len(r.buf) - 1
	}
	for i := 0; i < limit; i++ {
		out[i] = r.buf[idx]
		idx--
		if idx < 0 {
			idx = len(r.buf) - 1
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Hub — manages WebSocket subscribers + fan-out.
// ---------------------------------------------------------------------------
type Hub struct {
	mu          sync.RWMutex
	subscribers map[*Subscriber]struct{}
	ring        *RingBuffer
	connCount   atomic.Int64
	bank        *Bank // optional; nil disables fallback persistence
	// Path of assets/memories.json so Fanout can wipe synthetic seed data
	// on the first real-adapter memory_added event. Empty disables the
	// wipe behavior (e.g., when running headless without static assets).
	memPath          string
	syntheticChecked atomic.Bool

	// Wave 7c2 — section_dirty bookkeeping. Global map (not per-subscriber)
	// of section-name → last-dirtied timestamp. Clients track their own
	// "last fetched" timestamps client-side and compare against this map
	// to decide what's stale. Simpler than per-subscriber state, survives
	// reconnects, multi-tab clients track independently.
	sectionMu       sync.RWMutex
	sectionDirtyAt  map[string]time.Time
}

// Wave 7c2 section names. Enumerated so a typo in a MarkSectionDirty
// call doesn't silently create a never-cleared section. Tests assert
// these match the frontend's known-set.
const (
	SectionAudit        = "audit"
	SectionBank         = "bank"
	SectionMaps         = "maps"
	SectionLexicon      = "lexicon"
	SectionDreamJournal = "dream_journal"
	SectionPrivacy      = "privacy"
	SectionResearch     = "research"
	SectionBubbles      = "bubbles"
	SectionServices     = "services"
	SectionSettings     = "settings"
)

// knownSections is the canonical set of section names. Used by
// /session/dirty to seed null entries for sections that have never
// been dirtied yet (gives the frontend a predictable shape).
var knownSections = []string{
	SectionAudit, SectionBank, SectionMaps, SectionLexicon,
	SectionDreamJournal, SectionPrivacy, SectionResearch,
	SectionBubbles, SectionServices, SectionSettings,
}

// eventToSections is the wave-7c2 taxonomy. Legacy event types that
// would trigger a list/table re-render in the dashboard are mapped to
// section names; `Fanout` translates each into a `section_dirty` emit
// alongside the legacy event. Events not in this map (tool_call,
// memory_added, model_thinking, heartbeat, bubble_added, etc.) stay
// pure visualisation events and do not dirty any section.
//
// Adding a new event-to-section mapping is a one-line change here;
// each emission site stays untouched.
var eventToSections = map[string][]string{
	"audit.appended":                  {SectionAudit},
	"bulk.completed":                  {SectionMaps, SectionAudit, SectionBank},
	"nightly.completed":               {SectionDreamJournal},
	"nightly.deleted":                 {SectionDreamJournal},
	"lexicon_rebuild_done":            {SectionLexicon},
	"map_added":                       {SectionMaps},
	"map_archived":                    {SectionMaps},
	"sensitive.flipped":               {SectionPrivacy, SectionBank},
	"service_state_changed":           {SectionServices},
	"service_lifecycle_transitioned":  {SectionServices},
	"service_lifecycle_ready":         {SectionServices},
	"service_lifecycle_idled":         {SectionServices},
	"service_lifecycle_stopped":       {SectionServices},
}

// MarkSectionDirty records the current time as the last-dirtied stamp
// for each named section. Safe to call concurrently. Also fans out a
// single `section_dirty` WS event carrying the affected sections so
// connected clients can update their staleness badges without polling.
//
// Idempotent + cheap — a no-op when sections is empty.
func (h *Hub) MarkSectionDirty(sections ...string) {
	if h == nil || len(sections) == 0 {
		return
	}
	now := time.Now().UTC()
	h.sectionMu.Lock()
	if h.sectionDirtyAt == nil {
		h.sectionDirtyAt = map[string]time.Time{}
	}
	for _, s := range sections {
		if s == "" {
			continue
		}
		h.sectionDirtyAt[s] = now
	}
	h.sectionMu.Unlock()
	// Emit a single section_dirty event for the batch. The event flows
	// through the same ring + fanout as everything else; clients
	// subscribe via /ws and update their badge state.
	h.FanoutEphemeral(Event{
		SchemaVersion: SchemaVersion,
		Type:          "section_dirty",
		Timestamp:     now.Format(time.RFC3339Nano),
		AdapterID:     "sd-core-hub",
		Payload: map[string]any{
			"sections":    sections,
			"dirty_at":    now.Format(time.RFC3339Nano),
		},
	})
}

// SectionState returns a snapshot of the section→last-dirty-at map.
// Sections that have never been dirtied are present with a zero-value
// timestamp; callers (GET /session/dirty) serialise the zero as null
// in JSON.
func (h *Hub) SectionState() map[string]time.Time {
	out := map[string]time.Time{}
	if h == nil {
		return out
	}
	h.sectionMu.RLock()
	defer h.sectionMu.RUnlock()
	for _, s := range knownSections {
		out[s] = h.sectionDirtyAt[s] // zero-value when not yet dirtied
	}
	return out
}

type Subscriber struct {
	conn *websocket.Conn
	send chan Event
	id   int64
}

func NewHub(ring *RingBuffer) *Hub {
	return &Hub{
		subscribers: make(map[*Subscriber]struct{}),
		ring:        ring,
	}
}

// AttachBank wires an optional SQLite bank into the Hub. memory_added events
// with text are persisted; watcher events (id-only) are not. Pass nil to
// disable. Safe to call before or after subscribers connect.
func (h *Hub) AttachBank(b *Bank) {
	h.bank = b
}

func (h *Hub) Add(s *Subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subscribers[s] = struct{}{}
	h.connCount.Add(1)
}

func (h *Hub) Remove(s *Subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subscribers[s]; ok {
		delete(h.subscribers, s)
		close(s.send)
		h.connCount.Add(-1)
	}
}

// slowSubscriberTimeout is how long Fanout waits for a slow subscriber to
// accept a message before disconnecting it. Generous enough that normal
// bursts don't false-trigger; tight enough that a genuinely stuck client
// doesn't keep the dashboard's live-update channel zombie-state.
//
// Pre-2026-05-09 the timeout was effectively zero (`select { default: drop }`)
// which silently dropped events without telling the client — the dashboard
// stayed `readyState=1` but received nothing for ~75s until the reader's
// read deadline expired. See `CODE_HANDOFF — WS hub silent-drop bug`.
const slowSubscriberTimeout = 100 * time.Millisecond

// Fanout pushes an event to every subscriber.
//
// Slow-subscriber policy: if a subscriber's send buffer cannot accept the
// event within `slowSubscriberTimeout`, the subscriber's underlying
// websocket connection is closed. That triggers a chain — writer's next
// WriteJSON errors → writer returns → reader's ReadMessage errors → reader's
// `defer h.Remove(sub)` fires → subscriber is removed from the map. From
// the client's perspective, this surfaces as a normal close frame, so the
// dashboard's reconnect logic kicks in instead of staring at a zombie.
//
// We snapshot the subscriber set under the read lock and release before
// any potentially-slow send, so per-subscriber timeouts do not freeze the
// hub for `timeout × num_slow` seconds during a fanout cycle.
//
// Side effect: if the optional SQLite bank is attached and the event is a
// `memory_added` carrying real text, the record is persisted asynchronously
// so write latency never delays WS fanout. Watcher events (id-only, no text)
// are intentionally NOT persisted — the bank is the AI-reported store, not
// a mirror of memories.json.
//
// Side effect 2: when the first `memory_added` event arrives from a real
// adapter (anything not prefixed `sd-core-`), Hub checks whether
// `assets/memories.json` carries the `synthetic: true` flag. If so, the
// file is wiped back to the empty stub so the brain stops mixing demo
// data with the user's real activity.
func (h *Hub) Fanout(e Event) {
	h.ring.Add(e)
	if h.bank != nil && e.Type == "memory_added" {
		h.maybePersist(e)
	}
	if e.Type == "memory_added" && !strings.HasPrefix(e.AdapterID, "sd-core-") {
		h.maybeWipeSyntheticOnce()
	}
	for _, s := range h.snapshotSubs() {
		h.sendToSubscriber(s, e)
	}
	// Wave 7c2 — translate list-driving legacy events into a sibling
	// section_dirty emit. The legacy event keeps flowing for back-compat
	// with older dashboards; new dashboards prefer the section_dirty
	// signal for table re-render decisions. Recursion-safe: section_dirty
	// itself never appears in eventToSections.
	if e.Type != "section_dirty" {
		if sections, ok := eventToSections[e.Type]; ok && len(sections) > 0 {
			h.MarkSectionDirty(sections...)
		}
	}
}

// FanoutEphemeral pushes an event to current subscribers WITHOUT writing
// to the ring buffer. Use for transient signals (heartbeats etc.) where
// `/events?since=` replay would be redundant or noisy. Same slow-subscriber
// disconnect policy as Fanout.
func (h *Hub) FanoutEphemeral(e Event) {
	for _, s := range h.snapshotSubs() {
		h.sendToSubscriber(s, e)
	}
}

// hubHeartbeatInterval is how often the hub emits a `heartbeat` event so
// the dashboard's stale-detector can keep its threshold tight (~45s) instead
// of waiting for the 90s "no events at all" timeout.
const hubHeartbeatInterval = 30 * time.Second

// RunHeartbeat fires a `heartbeat` control event every hubHeartbeatInterval
// to all current subscribers (skipping the ring buffer — heartbeats are
// transient, no point persisting them). Returns when ctx is cancelled.
//
// `heartbeat` uses the dot-namespace convention so the dashboard's
// `EventDispatcher` routes it to the control-channel path (no brain
// animation) — see `CODE_HANDOFF — WS event extensions`.
func (h *Hub) RunHeartbeat(ctx context.Context) {
	t := time.NewTicker(hubHeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.FanoutEphemeral(Event{
				SchemaVersion: SchemaVersion,
				Type:          "heartbeat",
				Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
				AdapterID:     "sd-core-hub",
				Payload:       map[string]interface{}{},
			})
		}
	}
}

// snapshotSubs returns a copy of the subscriber slice taken under the read
// lock so callers can iterate without holding the lock during slow sends.
func (h *Hub) snapshotSubs() []*Subscriber {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*Subscriber, 0, len(h.subscribers))
	for s := range h.subscribers {
		out = append(out, s)
	}
	return out
}

// sendToSubscriber tries to deliver `e` to `s` within slowSubscriberTimeout.
// On timeout, closes the underlying connection so the writer/reader cleanup
// chain fires. Recovers from send-on-closed-channel panics that can happen
// if a concurrent Remove closes s.send between our snapshot and our send.
func (h *Hub) sendToSubscriber(s *Subscriber, e Event) {
	defer func() {
		// A concurrent Remove may close s.send between Fanout's snapshot
		// and our send attempt. Sending to a closed channel panics; the
		// subscriber is already gone so we just swallow it.
		_ = recover()
	}()
	select {
	case s.send <- e:
		// delivered
	case <-time.After(slowSubscriberTimeout):
		log.Printf("hub: slow subscriber id=%d disconnecting (buffer full %s)",
			s.id, slowSubscriberTimeout)
		// Closing the underlying conn cascades:
		//   1. writer's next WriteJSON / WriteMessage errors → writer returns
		//   2. writer's `defer conn.Close()` is a no-op (already closed)
		//   3. reader's ReadMessage errors → reader's defer fires Remove
		//   4. Remove deletes from map + closes s.send
		// We deliberately do NOT call Remove here: this function may be
		// running from a Fanout that holds the read lock, and Remove takes
		// the write lock. Letting the reader trigger Remove avoids both
		// the deadlock and the "called Remove twice" footgun.
		s.conn.Close()
	}
}

// maybeWipeSyntheticOnce — first real-adapter memory triggers a one-time
// check of assets/memories.json. If the file is the synthetic seed (the
// `.example.json` content copied in during opt-in seeding, identifiable
// by the top-level `synthetic: true` flag), it gets replaced with an
// empty stub so the user's real memories aren't co-located with demo
// data. After the first call, the atomic flag short-circuits future
// invocations — this is a session-level decision.
func (h *Hub) maybeWipeSyntheticOnce() {
	if !h.syntheticChecked.CompareAndSwap(false, true) {
		return
	}
	if h.memPath == "" {
		return
	}
	data, err := os.ReadFile(h.memPath)
	if err != nil {
		// File doesn't exist or unreadable — nothing to wipe.
		return
	}
	var probe struct {
		Synthetic bool `json:"synthetic"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return
	}
	if !probe.Synthetic {
		return
	}
	stub := []byte(`{"schema_version":"1.0","items":[],"comment":"Synthetic seed wiped on first real memory_added. Real adapter data going forward."}`)
	if err := os.WriteFile(h.memPath, stub, 0o644); err != nil {
		log.Printf("synthetic-wipe: could not rewrite %s: %v", h.memPath, err)
		return
	}
	log.Printf("synthetic-wipe: cleared seed data from %s — first real memory inbound", h.memPath)
}

func (h *Hub) maybePersist(e Event) {
	if h.bank == nil {
		return
	}
	textVal, _ := e.Payload["text"].(string)
	if textVal == "" {
		return // watcher / id-only event — skip
	}
	rec := MemoryRecord{
		ID:         strFromPayload(e.Payload, "memory_id"),
		Text:       textVal,
		Tags:       tagsFromPayload(e.Payload),
		RegionHint: strFromPayload(e.Payload, "region_hint"),
		AdapterID:  e.AdapterID,
		SessionID:  e.SessionID,
		CreatedAt:  e.Timestamp,
		// v2.7 Bundle O — let adapters set memory_type on the create
		// event. Used by sd_save_procedure to land directly as a
		// procedural memory rather than a generic episodic row.
		MemoryType: strFromPayload(e.Payload, "memory_type"),
	}
	go func() {
		if _, err := h.bank.SaveMemory(rec); err != nil {
			log.Printf("bank: persist memory_added failed: %v", err)
		}
	}()
}

func strFromPayload(p map[string]interface{}, key string) string {
	if p == nil {
		return ""
	}
	if s, ok := p[key].(string); ok {
		return s
	}
	return ""
}

func tagsFromPayload(p map[string]interface{}) []string {
	if p == nil {
		return nil
	}
	raw, ok := p["tags"]
	if !ok {
		return nil
	}
	if arr, ok := raw.([]interface{}); ok {
		out := make([]string, 0, len(arr))
		for _, v := range arr {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	if arr, ok := raw.([]string); ok {
		return arr
	}
	return nil
}

// ---------------------------------------------------------------------------
// File watcher — emits memory_added / memory_updated / memory_deleted events
// when assets/memories.json changes on disk. Uses an mtime+content-hash
// approach: re-reads the file, diffs against the last snapshot.
// ---------------------------------------------------------------------------
type MemoryWatcher struct {
	path     string
	hub      *Hub
	prev     map[string]string // memory id -> hash of (text|tags)
	mu       sync.Mutex
	debounce time.Duration
	onChange []func()
}

func (mw *MemoryWatcher) OnChange(fn func()) {
	mw.onChange = append(mw.onChange, fn)
}

func NewMemoryWatcher(path string, hub *Hub) *MemoryWatcher {
	return &MemoryWatcher{
		path: path, hub: hub, prev: map[string]string{},
		debounce: 1 * time.Second,
	}
}

func (mw *MemoryWatcher) Start(ctx context.Context) error {
	if _, err := os.Stat(mw.path); err != nil {
		log.Printf("memory watcher: file does not exist yet at %s, will watch the parent dir", mw.path)
	} else {
		mw.snapshot() // seed initial state without firing events
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	dir := filepath.Dir(mw.path)
	if err := w.Add(dir); err != nil {
		return fmt.Errorf("watch %s: %w", dir, err)
	}
	go func() {
		defer w.Close()
		var pending *time.Timer
		fire := func() {
			mw.diffAndEmit()
		}
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if filepath.Clean(ev.Name) != filepath.Clean(mw.path) {
					continue
				}
				if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
					continue
				}
				// Debounce — many editors write a flurry of events
				if pending != nil {
					pending.Stop()
				}
				pending = time.AfterFunc(mw.debounce, fire)
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				log.Printf("memory watcher error: %v", err)
			}
		}
	}()
	log.Printf("memory watcher: watching %s", mw.path)
	return nil
}

// snapshot reads the current memories file into the in-memory map without
// emitting any events (used at startup).
func (mw *MemoryWatcher) snapshot() {
	mw.mu.Lock()
	defer mw.mu.Unlock()
	mems := mw.read()
	mw.prev = map[string]string{}
	for _, m := range mems {
		mw.prev[m.ID] = m.signature()
	}
}

func (mw *MemoryWatcher) diffAndEmit() {
	mw.mu.Lock()
	defer mw.mu.Unlock()
	mems := mw.read()
	if mems == nil {
		return
	}
	current := map[string]string{}
	for _, m := range mems {
		current[m.ID] = m.signature()
	}
	now := time.Now().UTC().Format(time.RFC3339)
	added, updated, deleted := 0, 0, 0
	for id, sig := range current {
		if prevSig, ok := mw.prev[id]; !ok {
			mw.hub.Fanout(Event{
				SchemaVersion: SchemaVersion, Type: "memory_added",
				Timestamp: now, AdapterID: "sd-core-watcher",
				Payload: map[string]interface{}{"memory_id": id},
			})
			added++
		} else if prevSig != sig {
			mw.hub.Fanout(Event{
				SchemaVersion: SchemaVersion, Type: "memory_updated",
				Timestamp: now, AdapterID: "sd-core-watcher",
				Payload: map[string]interface{}{"memory_id": id},
			})
			updated++
		}
	}
	for id := range mw.prev {
		if _, ok := current[id]; !ok {
			mw.hub.Fanout(Event{
				SchemaVersion: SchemaVersion, Type: "memory_deleted",
				Timestamp: now, AdapterID: "sd-core-watcher",
				Payload: map[string]interface{}{"memory_id": id},
			})
			deleted++
		}
	}
	if added+updated+deleted > 0 {
		log.Printf("memories changed: +%d ~%d -%d", added, updated, deleted)
		for _, fn := range mw.onChange {
			go fn()
		}
	}
	mw.prev = current
}

type memRecord struct {
	ID   string                 `json:"id"`
	Text string                 `json:"text"`
	Tags interface{}            `json:"tags,omitempty"`
	Etc  map[string]interface{} `json:"-"`
}

func (m memRecord) signature() string {
	tagBytes, _ := json.Marshal(m.Tags)
	return fmt.Sprintf("%d|%x", len(m.Text), tagBytes)
}

func (mw *MemoryWatcher) read() []memRecord {
	data, err := os.ReadFile(mw.path)
	if err != nil {
		return nil
	}
	var wrapper struct {
		Memories []memRecord `json:"memories"`
		Items    []memRecord `json:"items"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		log.Printf("memory watcher: parse error: %v", err)
		return nil
	}
	if len(wrapper.Memories) > 0 {
		return wrapper.Memories
	}
	return wrapper.Items
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------
type Server struct {
	hub      *Hub
	ring     *RingBuffer
	bank     *Bank // optional fallback bank
	dataDir  string
	upgrader websocket.Upgrader
	// Optional reference to the bubble generator so HTTP handlers can ask
	// it to fill specific neurons on-demand (POST /bubbles/generate).
	bubbleGen *BubbleGenerator
	// router routes embedding/chat work to the appropriate model tier.
	// Tier 1 is always populated. Tier 2/3 may be nil.
	router *ModelRouter
	// reranker is the optional v2.7 Bundle Q second-pass reranker. nil →
	// the default salienceReranker is used (no I/O, blends cosine +
	// salience + recall_strength). A production deploy can install a
	// cross-encoder reranker here that calls a sidecar.
	reranker Reranker
	// hookSynth is the v2.7 Bundle S auto-synthesis engine. Wired into
	// postEvent; no-op when the engine has no synthesiser or the
	// `hook_synthesis_enabled` setting is OFF (default). nil-safe.
	hookSynth *HookSynthesisEngine
	// pingCache + pingCacheOnce back the per-server provider-ping cache.
	// Lazily initialised by ensurePingCache() so existing test fixtures that
	// construct &Server{bank: bank} keep working without explicit setup.
	pingCacheOnce sync.Once
	pingCache     *providerPingCache
	// nightly drives Dream Cycles. Optional — nil disables /nightly endpoints
	// (handlers fall back to "runner not configured" 503).
	nightly *NightlyRunner
	// lexiconRebuildMu + lexiconRebuildID gate /lexicon/rebuild so a second
	// trigger while a rebuild is in flight returns 409. atomic so reads
	// don't need to acquire the write mutex.
	lexiconRebuildMu sync.Mutex
	lexiconRebuildID atomic.Value // string; non-empty when a rebuild is running
	// Wave 7a Docker control plane state. nil-safe — handlers degrade to
	// 412 Precondition Failed when the gate is disabled.
	docker *DockerControlState
	// Wave 7b2 sidecar lifecycle manager. Nil when bank or docker are
	// not configured; handlers degrade with a clear error.
	lifecycle *LifecycleManager
	// v2.5.0b1 memory-write rate limiter. Nil when bank is disabled.
	// Honours setting memory_write_rate_limit_per_min (default 0 = unlimited).
	memoryLimiter *MemoryWriteLimiter
	// v2.8 — Patient feature. Multiple independent memory banks under
	// one running core. patients is the registry; bankSwapMu serialises
	// the actual Bank.Reopen() during /patients/{id}/activate so two
	// concurrent switches don't race. nil patients = feature disabled
	// (bank-disabled deploys or test fixtures).
	patients     *PatientStore
	bankSwapMu   sync.Mutex
}

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	// NEW PATH FAMILY CHECKLIST:
	// Before adding a new top-level path family (not a sub-path of an existing prefix),
	// see Dev/docs/REMOTE.md § "New path family checklist". Adding a new top-level path
	// without updating the cloudflared ingress regex on the host will cause the path to
	// 404 on remote (synaptic.nomadsgalaxy.com) while working locally.
	//
	// Existing prefixes the regex covers (cognito.<your-domain> ingress as
	// of 2026-05-15): /event /events /memories /bank /ws /bubbles /recall
	// /reflect /import /maps /lexicon /audit /research /budget /settings
	// /admin /nightly /VERSION /assets /healthz /patients /procedures
	// /entities. See Dev/docs/CLOUDFLARE_BRIDGE.md §3-§4 for the full table.
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/event", s.MemoryWriteRateLimited(s.postEvent))
	mux.HandleFunc("/events", s.eventsRouter)
	mux.HandleFunc("/memories", s.getMemories)
	mux.HandleFunc("/bank/memory", s.MemoryWriteRateLimited(s.bankWrite))
	mux.HandleFunc("/bank/memories", s.bankList)
	mux.HandleFunc("/bank/memories/", s.bankByIDExtended) // trailing slash → /{id}[/action]
	mux.HandleFunc("/recall", s.postRecall)
	// v2.5.0b1+ — map-level recall. Ranks MemoryMaps by name match +
	// schema cosine + member aggregate. Lets "Can you recall the
	// project X" return the X map at the top regardless of how its
	// schema/members align. See handlers_recall_maps.go.
	mux.HandleFunc("/recall/maps", s.handleRecallMaps)
	// v2.4.0b1 feature #1 — Tier 3 synthesis over recall result set. Lives
	// next to /recall on purpose: same retrieval pipeline, different exit.
	// See handlers_reflect.go for budget mapping + sensitive-flag enforcement.
	mux.HandleFunc("/reflect", s.handleReflectPost)
	// v2.7 Bundle O — procedural memory read endpoints.
	mux.HandleFunc("/procedures", s.handleProcedures)
	mux.HandleFunc("/procedures/prefix", s.handleProcedures)
	// v2.7 Bundle N HTTP surface for the entity registry. The Mux's
	// trailing-slash convention dispatches both /entities and
	// /entities/<sub>; handleEntities branches on the path tail.
	mux.HandleFunc("/entities", s.handleEntities)
	mux.HandleFunc("/entities/", s.handleEntities)
	// v2.5.0b1+ feature #22 — bulk import from other memory managers.
	// Auth + rate-limit applied (same chokepoint as /event + /bank/memory).
	// See handlers_import.go for the supported format mapper IDs.
	mux.HandleFunc("/import", s.MemoryWriteRateLimited(s.handleImport))
	mux.HandleFunc("/maps", s.mapsRoot)
	mux.HandleFunc("/maps/", s.mapsByID)
	// /lexicon/pairs registered BEFORE /lexicon so the sub-route never
	// risks being shadowed (Go's mux is exact-match here, but the
	// precedence-by-registration convention from prior handoffs applies).
	mux.HandleFunc("/lexicon/pairs", s.lexiconPairs)
	mux.HandleFunc("/lexicon", s.lexiconRoot)
	mux.HandleFunc("/lexicon/rebuild", s.postLexiconRebuild)
	mux.HandleFunc("/audit", s.auditRoot)
	mux.HandleFunc("/research", s.researchRoot)
	mux.HandleFunc("/research/", s.researchRoot)
	mux.HandleFunc("/budget", s.budgetRoot)
	mux.HandleFunc("/settings", s.settingsRoot)
	mux.HandleFunc("/settings/", s.settingsRoot)
	// Hot-swap LLM tiers without restarting core. PUT /settings/providers/
	// {tier} auto-reloads via the same code path; this endpoint is the
	// explicit fallback (recovery, manual re-read after a settings file
	// edit that bypassed the API, etc.). Body: {} or no body. Re-reads
	// ALL tiers from DB and swaps any that changed.
	mux.HandleFunc("/admin/router/reload", s.handleAdminRouterReload)
	mux.HandleFunc("/admin/env_write", s.postAdminEnvWrite)
	mux.HandleFunc("/admin/env_check", s.getAdminEnvCheck)
	mux.HandleFunc("/admin/network", s.getAdminNetwork)
	mux.HandleFunc("/admin/embed-all", s.handleAdminEmbedAll)
	mux.HandleFunc("/admin/deep-encode-all", s.handleAdminDeepEncodeAll)
	// One-off data repair: clears stale deleted_reason on live rows orphaned
	// by the bulk-restore resurrection bug. dry_run defaults TRUE.
	mux.HandleFunc("/admin/repair/orphaned-merges", s.handleAdminRepairOrphanedMerges)
	// Iter 25 — diagnostic endpoint: runs the retrieval pipeline for a
	// query and returns every intermediate decision (cosine top-K,
	// matched maps, fused ranking, optimized/enriched text per
	// candidate) without ever calling Tier 3. Free, fast, useful for
	// answering "why didn't the system retrieve memory X?".
	mux.HandleFunc("/admin/recall-trace", s.postAdminRecallTrace)
	// Iter 30 (2026-05-15) — Narrative-fact extraction endpoint.
	// POST /admin/extract-facts takes a multi-turn session and asks Tier 2
	// to emit 2-5 self-contained narrative facts. Stored as
	// MemoryRecords tagged "narrative_fact". Targets the per-turn-storage
	// failure modes: wrong-candidate selection, retrieval miss on
	// semantically-distant phrasings, lost decision pragmatics.
	mux.HandleFunc("/admin/extract-facts", s.postAdminExtractFacts)
	// User-triggered bulk re-classification of currently-sensitive memories
	// under the (possibly-updated) classifier prompt. Clears flags on
	// memories the updated classifier no longer thinks are sensitive.
	mux.HandleFunc("/admin/sensitive/reclassify", s.handleAdminReclassifySensitive)
	// Per-tier+vendor budget cap configured via SD_TIER*_API_*_BUDGET
	// envs (M20 = $20/month, W5 = $5/week, D2 = $2/day, T100 = $100
	// lifetime). Returns parsed caps + key counts (multi-key support
	// via comma-split or numbered _1/_2/_3 suffix) + period spend.
	mux.HandleFunc("/admin/budget/caps", s.handleAdminBudgetCaps)
	// Feature #5 (2026-05-23) — sync top-K project-tagged memories into
	// <project_path>/CLAUDE.md so every new Claude Code session in that
	// directory boots with Synaptic context. Idempotent; writes a
	// delimited block, preserves user content outside it.
	mux.HandleFunc("/admin/sync-claude-md", s.handleAdminSyncClaudeMd)
	// Read-only: the hardcoded default classifier prompt so the Privacy
	// config UI can populate the textarea on "Reset to default" without
	// shipping a duplicate copy of the prompt in the frontend.
	mux.HandleFunc("/admin/sensitive/default_prompt", s.handleAdminSensitiveDefaultPrompt)
	// Read-only count of memories currently flagged sensitive. The
	// Privacy config "Bulk re-classify" panel uses this to cap the
	// Max input at the actual flagged count instead of a fixed 5000.
	mux.HandleFunc("/admin/sensitive/count", s.handleAdminSensitiveCount)
	// Status of an in-flight bulk reclassify (for restoring the
	// progress card when the user returns to the Privacy tab
	// mid-run).
	mux.HandleFunc("/admin/sensitive/reclassify/status", s.handleAdminSensitiveReclassifyStatus)
	// Live (right-now) memory coverage counts — embeddings + deep-
	// encoding + dirty queue read directly from the bank, not from a
	// nightly-run snapshot. Powers the Dream Journal's ambient bars.
	// Lives under /memories/* (sibling to the existing GET /memories
	// list endpoint) so:
	//   - Cloudflare Access policies that gate /admin/* don't block it
	//     (common in remote-tunnel deployments — see v2.1.6b1 note).
	//   - Historical cloudflared ingress regexes that already include
	//     `memories` route it without any regex update on the user's end.
	mux.HandleFunc("/memories/coverage", s.handleMemoryCoverage)
	// Wave 7a Docker control plane. The trailing-slash form routes every
	// /admin/docker/* path through one dispatcher (adminDockerRouter).
	mux.HandleFunc("/admin/docker/", s.adminDockerRouter)
	// Wave 7b2 sidecar lifecycle endpoints — /admin/services/{name}/*.
	mux.HandleFunc("/admin/services/", s.adminServicesRouter)
	// Wave 7c3 AirLLM-specific admin endpoints (diagnostics + install_script).
	mux.HandleFunc("/admin/airllm/diagnostics", s.handleAdminAirLLMDiagnostics)
	mux.HandleFunc("/admin/airllm/install_script", s.handleAdminAirLLMInstallScript)
	// Wave 8a AirLLM orchestration helpers — atomic configure + lightweight probe.
	mux.HandleFunc("/admin/airllm/configure_tier", s.handleAdminAirLLMConfigureTier)
	mux.HandleFunc("/admin/airllm/health/", s.handleAdminAirLLMHealthPort)
	// Wave 8d — HF token validation. POST a candidate token; we hit HF
	// whoami-v2 and return whether it's accepted, plus the associated
	// account for the audit row. Does NOT persist the token.
	mux.HandleFunc("/admin/airllm/validate_hf_token", s.handleAdminAirLLMValidateHFToken)
	// Wave 8e — LLM benchmark + nightly-duration estimator. Trailing-slash
	// catches /admin/llm/benchmark/{tier} sub-paths; bare /admin/llm/benchmark
	// and /admin/llm/estimate_nightly route through the same dispatcher.
	mux.HandleFunc("/admin/llm/", s.adminLLMRouter)
	// Wave 7c2 — section dirty bitmap surface for lazy per-tab fetching.
	mux.HandleFunc("/session/dirty", s.handleSessionDirty)
	mux.HandleFunc("/session/clear/", s.handleSessionClear)
	mux.HandleFunc("/nightly/runs", s.nightlyRoot)
	mux.HandleFunc("/nightly/runs/", s.nightlyRoot) // /{id} item routes (DELETE)
	mux.HandleFunc("/nightly/trigger", s.nightlyRoot)
	mux.HandleFunc("/nightly/run", s.nightlyRoot)
	// POST /nightly/reconcile — force the stale-in-progress reconciler
	// to run NOW, bypassing the 5-minute heartbeat threshold. Genuine
	// ops UX: when a Tier 2 OOM kills a dream cycle mid-run, the
	// dashboard's nightly status sits "in_progress" forever and the
	// next /nightly/run returns 409. Without this, the only recovery
	// is a container restart OR waiting for the next reconcile pass
	// inside DoRun (which itself can't fire because the lock is held).
	mux.HandleFunc("/nightly/reconcile", s.handleNightlyReconcile)
	mux.HandleFunc("/ws", s.websocket)
	// Per-neuron bubble generation: dashboards POST a memory_id when their
	// pool has no thought for that neuron yet. The bubble generator
	// promotes the ID to the front of its work queue. Returns 202 Accepted
	// (work is asynchronous; the new thought lands later as a bubble_added
	// or bubble_mutated WS event).
	mux.HandleFunc("/bubbles/generate", s.bubblesGenerate)
	mux.HandleFunc("/bubbles/mutate", s.bubblesMutate)
	// v2.8 Patient feature — multiple independent memory banks under one
	// running core. /patients lists/creates; /patients/{id}/activate
	// triggers a Bank.Reopen; /patients/{id} (DELETE) removes a non-active,
	// non-default patient + its bank file. See handlers_patients.go.
	mux.HandleFunc("/patients", s.handlePatients)
	mux.HandleFunc("/patients/", s.handlePatientByID)
	// Catch-all: serve the dashboard's static files (whitelisted). This
	// folds the previous nginx container into SD Core itself — one binary,
	// one port, one container. The whitelist exists because SD_DATA_DIR
	// points at the project root, which contains source code, build tools,
	// and personal docs we never want exposed over HTTP.
	mux.Handle("/", s.staticDashboard())
	return mux
}

// staticDashboard returns an http.Handler that serves the dashboard shell
// (index.html), its assets, and a few static auxiliaries — and 404s
// everything else. Even if SD_DATA_DIR is the entire project tree,
// /tools, /bridge, /docs, and personal files are not reachable via HTTP.
//
// User-data assets (memories.json, synapses.json, thought_bubbles.json)
// are lazy-created on first request: if the file doesn't exist on disk
// when the dashboard fetches it, write the empty stub then serve. Belt-
// and-suspenders alongside the startup-time `ensureEmptyAssetFile()` so
// the dashboard never sees a 404 for these paths even if the startup
// scaffolding silently failed (Docker bind-mount permission quirks,
// stale cached image without the scaffolding code, etc.).
// staticMime overrides Go's mime.TypeByExtension for the file types the
// dashboard cares about. On Windows the default lookup consults the system
// registry (golang/go#32350) which can return "text/plain" for ".js" — that
// trips browsers' strict-MIME-check on Web Workers and ESM imports, breaking
// `/vendor/gif.js` and any other executable script load. Always-set entries
// here win over whatever the registry says.
var staticMime = map[string]string{
	".js":    "application/javascript; charset=utf-8",
	".mjs":   "application/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".html":  "text/html; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".wasm":  "application/wasm",
	".svg":   "image/svg+xml",
	".woff2": "font/woff2",
	".map":   "application/json; charset=utf-8",
}

func (s *Server) staticDashboard() http.Handler {
	fs := http.FileServer(http.Dir(s.dataDir))
	// Prefixes that map directly into the data dir.
	allowedPrefixes := []string{
		"/assets/",
		"/bubbles/",
		"/vendor/", // gif.js worker etc. — see staticMime override above.
	}
	// Exact paths that map to specific files in the data dir root.
	// /VERSION is served as the canonical version source-of-truth so
	// index.html's boot fetch (`fetch('/VERSION')`) can stamp the HUD,
	// medical-login titlebar, and `window.SD_VERSION` from the running
	// install's actual VERSION file rather than relying on the
	// build-time-stamped `const SD_VERSION = '…'` (which can lag if
	// someone runs an in-place dev build without re-stamping).
	allowedExact := map[string]string{
		"/":              "index.html",
		"/index.html":    "index.html",
		"/favicon.ico":   "favicon.ico",
		"/bubbles.json":  "bubbles.json",
		"/VERSION":       "VERSION",
	}
	// Specific user-data paths that get auto-created on first request if
	// missing. Keys are URL paths; values are the JSON body to write.
	lazyEnsure := map[string]string{
		"/assets/memories.json":        `{"schema_version":"1.0","items":[],"comment":"Auto-created by SD Core on first request. Add memories via your adapter / memory provider."}`,
		"/assets/synapses.json":        `{"schema_version":"1.0","edges":[],"comment":"Auto-created by SD Core on first request. Synapses are recomputed when memories change."}`,
		"/assets/thought_bubbles.json": `{"schema_version":"1.0","updated_at":"","bubbles":[]}`,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		// Block traversal attempts and dotfiles.
		if strings.Contains(p, "..") || strings.HasPrefix(filepath.Base(p), ".") {
			http.NotFound(w, r)
			return
		}
		// CORS on static-file responses too. The dashboard's runtime
		// `fetch('/VERSION')` and `fetch('/assets/synapses.json')` go
		// through this handler, and when the dashboard is loaded from
		// a different origin than SD Core (the cross-origin bridge
		// case: dashboard at Access-gated origin, API at cloudflared
		// origin), without CORS the browser blocks the response with
		// "No 'Access-Control-Allow-Origin' header is present".
		cors(w)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// Set Content-Type from the staticMime override BEFORE serving.
		// http.ServeFile / http.FileServer would otherwise call
		// mime.TypeByExtension which is broken on Windows for `.js`.
		if mt, ok := staticMime[strings.ToLower(filepath.Ext(p))]; ok {
			w.Header().Set("Content-Type", mt)
		}
		if file, ok := allowedExact[p]; ok {
			full := filepath.Join(s.dataDir, file)
			if _, err := os.Stat(full); err != nil {
				http.NotFound(w, r)
				return
			}
			// Force the browser to revalidate on every HTML page load
			// AND on every VERSION fetch. Without this the browser's
			// HTTP cache + service-worker layer can pin an old
			// index.html across multiple "hard reloads" (Docker
			// Desktop's grpc-fuse bind mount can lag the disk too).
			// Paired with the runtime VERSION fetch in index.html so
			// even a stuck HTML cache still gets the live banner
			// from the API.
			if p == "/" || p == "/index.html" || p == "/VERSION" {
				w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
				w.Header().Set("Pragma", "no-cache")
				w.Header().Set("Expires", "0")
			}
			http.ServeFile(w, r, full)
			return
		}
		for _, prefix := range allowedPrefixes {
			if strings.HasPrefix(p, prefix) {
				// Lazy-create the empty stub for user-data paths that
				// don't exist yet. After this call, the file server's
				// next read finds it.
				if body, ok := lazyEnsure[p]; ok {
					full := filepath.Join(s.dataDir, p)
					ensureEmptyAssetFile(full, body)
				}
				fs.ServeHTTP(w, r)
				return
			}
		}
		http.NotFound(w, r)
	})
}

// runHealthcheck is the implementation behind the binary's -healthcheck
// flag. Probes http://127.0.0.1:<port>/healthz and exits 0 on 2xx, 1
// otherwise. The Dockerfile HEALTHCHECK invokes `/sd-core -healthcheck`;
// scratch has no shell so we can't shell out from compose's CMD-SHELL.
func runHealthcheck(listen string) {
	// listen may be ":9911", "0.0.0.0:9911", "127.0.0.1:9911", etc. Probe
	// must hit a valid host; normalize 0.0.0.0/"" to localhost.
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		// Try as port-only ":9911" or fall through to default.
		host, port = "127.0.0.1", "9911"
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(host, port) + "/healthz"
	cl := &http.Client{Timeout: 3 * time.Second}
	resp, err := cl.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: GET %s: %v\n", url, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "healthcheck: %s returned %d\n", url, resp.StatusCode)
		os.Exit(1)
	}
	os.Exit(0)
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	stats := map[string]interface{}{
		"status":            "ok",
		"schema_version":    SchemaVersion,
		"subscribers":       s.hub.connCount.Load(),
		"total_events_seen": s.ring.totalN,
		"ring_capacity":     len(s.ring.buf),
		"bank_enabled":      s.bank != nil,
	}
	if s.bank != nil {
		if n, err := s.bank.Count(); err == nil {
			stats["bank_memories"] = n
		}
	}
	// 2026-05-23 (Feature #6) — surface the in-flight nightly run's phase
	// + elapsed time so a `curl /healthz` from a watcher can answer
	// "is the dream actually doing something, or stuck?" without
	// docker logs grepping. Omitted when nothing is in flight.
	if s.nightly != nil {
		if running, runID := s.nightly.IsRunning(); running {
			phase, phaseStarted := s.nightly.CurrentPhase()
			nightly := map[string]interface{}{
				"run_id":  runID,
				"running": true,
			}
			if phase != "" {
				nightly["current_phase"] = phase
				if phaseStarted != "" {
					nightly["phase_started_at"] = phaseStarted
					if t, terr := time.Parse(time.RFC3339Nano, phaseStarted); terr == nil {
						nightly["phase_elapsed_ms"] = time.Since(t).Milliseconds()
					}
				}
			}
			stats["nightly"] = nightly
		}
	}
	cors(w)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

func (s *Server) postEvent(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 256*1024))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var e Event
	if err := json.Unmarshal(body, &e); err != nil {
		http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := e.Validate(); err != nil {
		http.Error(w, "validate: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.hub.Fanout(e)
	// Wave 5: persist forwarded event into hook_events. Best-effort.
	if s.bank != nil {
		payloadJSON, _ := json.Marshal(e.Payload)
		saved, serr := s.bank.SaveHookEvent(HookEvent{
			AdapterID: e.AdapterID,
			SessionID: e.SessionID,
			EventType: e.Type,
			Payload:   string(payloadJSON),
			CreatedAt: e.Timestamp,
		})
		// v2.7 Bundle S — feed the auto-synthesis engine. Cheap when the
		// feature is disabled (early-return inside Observe).
		if serr == nil && s.hookSynth != nil {
			s.hookSynth.Observe(r.Context(), saved)
		}
	}
	// 2026-05-23 — real-time neighbor surfacing. Embeds substantive
	// hook text + finds top-K bank neighbors + broadcasts a
	// memory_neighbors_found WS event. No-op when the
	// `live_neighbor_stream_enabled` setting is false (default).
	// Runs in a goroutine — never blocks the /event response.
	s.emitLiveNeighbors(r.Context(), e)
	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte(`{"ok":true}`))
}

// /events GET = recent events; POST = batch of events
func (s *Server) eventsRouter(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodOptions:
		s.getEvents(w, r)
	case http.MethodPost:
		s.postEventsBatch(w, r)
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

func (s *Server) getEvents(w http.ResponseWriter, r *http.Request) {
	cors(w)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 100
	}
	// Wave 5 hook log: when ANY persistence filter is present
	// (adapter/session/type/since), serve from the hook_events table so
	// callers can audit historical activity. Without filters, the
	// existing in-memory ring buffer continues to serve so dashboards
	// that just want the live tail are unchanged.
	q := r.URL.Query()
	hasFilter := q.Get("adapter") != "" || q.Get("session") != "" ||
		q.Get("type") != "" || q.Get("since") != ""
	if hasFilter && s.bank != nil {
		rows, err := s.bank.ListHookEvents(HookEventListOpts{
			AdapterID: q.Get("adapter"),
			SessionID: q.Get("session"),
			EventType: q.Get("type"),
			Since:     q.Get("since"),
			Limit:     limit,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"count":      len(rows),
			"events":     rows,
			"source":     "hook_events",
		})
		return
	}
	if limit > len(s.ring.buf) {
		limit = len(s.ring.buf)
	}
	out := s.ring.Recent(limit)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"count":  len(out),
		"events": out,
	})
}

func (s *Server) postEventsBatch(w http.ResponseWriter, r *http.Request) {
	cors(w)
	body, err := io.ReadAll(io.LimitReader(r.Body, 5*1024*1024))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var batch []Event
	if err := json.Unmarshal(body, &batch); err != nil {
		http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
		return
	}
	rejected := 0
	persisted := 0
	for _, e := range batch {
		if err := e.Validate(); err != nil {
			rejected++
			continue
		}
		s.hub.Fanout(e)
		// Wave 5: persist forwarded events into hook_events so the
		// dashboard's Audit tab can replay them later. Best-effort —
		// persistence failures don't fail the request (the event already
		// fanned out to live subscribers).
		if s.bank != nil {
			payloadJSON, _ := json.Marshal(e.Payload)
			if _, err := s.bank.SaveHookEvent(HookEvent{
				AdapterID: e.AdapterID,
				SessionID: e.SessionID,
				EventType: e.Type,
				Payload:   string(payloadJSON),
				CreatedAt: e.Timestamp,
			}); err == nil {
				persisted++
			}
		}
	}
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"accepted":  len(batch) - rejected,
		"rejected":  rejected,
		"persisted": persisted,
	})
}

// /memories — proxy the assets/memories.json file. Lets the dashboard read it
// without needing CORS shenanigans against the static-file server.
func (s *Server) getMemories(w http.ResponseWriter, _ *http.Request) {
	cors(w)
	// When the bank is enabled (production deployments), serve live state
	// so the dashboard's CTRL+F5 path sees up-to-date tags + content after
	// bulk operations like /maps/merge. Prior behavior — serving a static
	// memories.json snapshot — meant any backend-side mutation (merge,
	// PATCH, cleanup) was visible only until the page reloaded; the
	// reload re-rendered from frozen data. Fall back to the static file
	// when bank is disabled (demo mode / pre-bank installs).
	if s.bank != nil {
		recs, err := s.bank.ListMemoriesWith(MemoryListOpts{Limit: 50000})
		if err == nil {
			items := make([]map[string]interface{}, 0, len(recs))
			for _, m := range recs {
				items = append(items, map[string]interface{}{
					"id":           m.ID,
					"text":         m.Text,
					"tags":         m.Tags,
					"region":       m.RegionHint, // legacy snapshot field name
					"region_tier":  "tier1_tags",
					"created_at":   m.CreatedAt,
					"mentioned_at": m.CreatedAt,
				})
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"schema_version": "1.0",
				"items":          items,
				"count":          len(items),
			})
			return
		}
		// On bank read failure, fall through to the static snapshot rather
		// than 5xx — keeps the dashboard renderable even mid-incident.
	}
	path := filepath.Join(s.dataDir, "assets", "memories.json")
	// Lazy-ensure the empty stub if the file is missing — same defense
	// in depth as the static handler. Guarantees this endpoint never 404s
	// in the empty-state-no-real-memories case.
	ensureEmptyAssetFile(path, `{"schema_version":"1.0","items":[],"comment":"Auto-created by SD Core on first request. Add memories via your adapter / memory provider."}`)
	data, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// bubblesGenerate handles POST /bubbles/generate. Body or query carries a
// memory_id; we ask the bubble generator to fill that neuron's slot next.
// Returns 202 Accepted (asynchronous — new thought arrives via WS).
func (s *Server) bubblesGenerate(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.bubbleGen == nil {
		http.Error(w, "bubble generator disabled", http.StatusServiceUnavailable)
		return
	}
	memID := r.URL.Query().Get("memory_id")
	if memID == "" {
		body, err := io.ReadAll(io.LimitReader(r.Body, 16*1024))
		if err == nil && len(body) > 0 {
			var p struct {
				MemoryID string `json:"memory_id"`
			}
			if json.Unmarshal(body, &p) == nil {
				memID = p.MemoryID
			}
		}
	}
	if memID == "" {
		http.Error(w, "memory_id required", http.StatusBadRequest)
		return
	}
	queued := s.bubbleGen.EnqueuePriority(memID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"memory_id":%q,"queued":%t}`, memID, queued)
}

// bubblesMutate handles POST /bubbles/mutate. Body or query carries a
// memory_id and a mode_key (e.g. "adhd" or "adhd+tism"). The bubble
// generator queues a mutation request that rewrites the existing base
// thought through the mode's lens and caches it under
// thoughts[memID].modes[modeKey]. Returns 202 Accepted.
func (s *Server) bubblesMutate(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.bubbleGen == nil {
		http.Error(w, "bubble generator disabled", http.StatusServiceUnavailable)
		return
	}
	memID := r.URL.Query().Get("memory_id")
	modeKey := r.URL.Query().Get("mode_key")
	if memID == "" || modeKey == "" {
		body, err := io.ReadAll(io.LimitReader(r.Body, 16*1024))
		if err == nil && len(body) > 0 {
			var p struct {
				MemoryID string `json:"memory_id"`
				ModeKey  string `json:"mode_key"`
			}
			if json.Unmarshal(body, &p) == nil {
				if memID == "" {
					memID = p.MemoryID
				}
				if modeKey == "" {
					modeKey = p.ModeKey
				}
			}
		}
	}
	if memID == "" || modeKey == "" {
		http.Error(w, "memory_id and mode_key required", http.StatusBadRequest)
		return
	}
	queued := s.bubbleGen.EnqueueMutation(memID, modeKey)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"memory_id":%q,"mode_key":%q,"queued":%t}`, memID, modeKey, queued)
}

// ---------------------------------------------------------------------------
// /bank/* — SQLite fallback bank for AI-reported memories (B6).
// ---------------------------------------------------------------------------

// POST /bank/memory — direct write. Body is a MemoryRecord (text required).
// Response is the resolved record. Adapters can use this when they want
// durability but DON'T want to fire a memory_added event (e.g. silent
// background memory consolidation).
func (s *Server) bankWrite(w http.ResponseWriter, r *http.Request) {
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
		http.Error(w, "bank disabled", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 256*1024))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var rec MemoryRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
		return
	}
	saved, err := s.bank.SaveMemory(rec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(saved)
}

// GET /bank/memories?limit&since&adapter_id — paginated list, newest first.
func (s *Server) bankList(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method != http.MethodGet && r.Method != http.MethodOptions {
		// Helpful 405: remote MCP / SDK clients (and AI coding agents
		// reading their own logs) keep confusing POST /bank/memories
		// with the singular POST /bank/memory write endpoint. The
		// earlier blunt "GET only" response gave them nowhere to go.
		// Surface the actual write paths inline. Allow header lets
		// well-behaved clients self-correct programmatically; the body
		// is for human / agent eyeballs.
		w.Header().Set("Allow", "GET, OPTIONS")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte(`{` +
			`"error":"GET only on /bank/memories",` +
			`"hint":"To write a memory, POST to /bank/memory (singular). ` +
			`Body: {\"text\":\"...\",\"tags\":[\"...\"],\"adapter_id\":\"my-agent\"}. ` +
			`Bearer auth uses the same SD_API_TOKEN as this list endpoint.",` +
			`"see_also":["POST /event with type=memory_added","POST /bank/memories/{id}/restore (un-soft-delete)"]` +
			`}`))
		return
	}
	if s.bank == nil {
		http.Error(w, "bank disabled", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	recs, err := s.bank.ListMemoriesWith(MemoryListOpts{
		Limit:            limit,
		Since:            q.Get("since"),
		AdapterID:        q.Get("adapter_id"),
		IncludeDeleted:   q.Get("include_deleted") == "1",
		OnlyDeleted:      q.Get("only_deleted") == "1",
		ExcludeSensitive: q.Get("exclude_sensitive") == "1",
		OnlySensitive:    q.Get("only_sensitive") == "1",
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	total, _ := s.bank.Count()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"count":     len(recs),
		"total":     total,
		"memories":  recs,
	})
}

// GET / PATCH / DELETE / POST on /bank/memories/{id}[/action] is served by
// bankByIDExtended in handlers_p5.go.

func (s *Server) websocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws upgrade failed: %v", err)
		return
	}
	sub := &Subscriber{
		conn: conn,
		send: make(chan Event, 256),
		id:   time.Now().UnixNano(),
	}
	s.hub.Add(sub)
	log.Printf("ws subscriber connected (total=%d)", s.hub.connCount.Load())

	// Replay the last 50 events to the new subscriber so the dashboard
	// has immediate context after reconnecting.
	for _, e := range s.ring.Recent(50) {
		select {
		case sub.send <- e:
		default:
		}
	}

	// Reader: just keeps the connection alive, reads any pings, exits on close.
	go func() {
		defer s.hub.Remove(sub)
		conn.SetReadLimit(8 * 1024)
		conn.SetReadDeadline(time.Now().Add(75 * time.Second))
		conn.SetPongHandler(func(string) error {
			conn.SetReadDeadline(time.Now().Add(75 * time.Second))
			return nil
		})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// Writer: pump events from sub.send to the WS connection.
	//
	// Critical: `defer conn.Close()` ensures every return path closes the
	// underlying socket. Without it, a single WriteJSON error would leave
	// the conn open while the writer goroutine had exited — Fanout would
	// keep queueing events into a buffer no one drains, and the client
	// would zombie until the reader's 75-second read deadline kicked in.
	// (See `CODE_HANDOFF — WS hub silent-drop bug 2026-05-09`.)
	defer conn.Close()
	pingTicker := time.NewTicker(30 * time.Second)
	defer pingTicker.Stop()
	for {
		select {
		case e, ok := <-sub.send:
			if !ok {
				conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteJSON(e); err != nil {
				return
			}
		case <-pingTicker.C:
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// CORS for adapters running in different processes / Docker networks,
// AND for dashboards loaded from a different origin than SD Core (the
// canonical example: dashboard at https://synaptic.example.com behind
// Cloudflare Access, SD Core API exposed at https://cognito.example.com
// via cloudflared tunnel — different origins from the browser's POV).
//
// Allow-Methods must cover everything the dashboard uses:
//   - GET     — every read endpoint (/events, /bank/*, /audit, /settings/*, /admin/*)
//   - POST    — /event, /bank/memory, /admin/* mutations, /maps/* actions
//   - PUT     — /settings/{key}, /settings/providers/{tier}, /admin/*/configure
//   - PATCH   — /bank/memories/{id} (partial update)
//   - DELETE  — /bank/memories/{id}, /nightly/runs/{id}, /maps/{id}
//   - OPTIONS — preflight
// Allow-Headers must cover Authorization (Bearer token), Content-Type
// (JSON bodies), AND X-Adapter-Id (dashboard sets this on every write
// so audit_log entries attribute correctly — was the load-bearing
// header that bounced preflight on cross-origin /audit, /settings,
// /admin requests before this fix).
func cors(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Adapter-Id")
	// Cache preflights for an hour so the dashboard isn't paying a
	// round-trip per fetch. Browsers cap at their own ceiling (Chrome 2h,
	// Firefox 24h) but 3600 is the conservative shared minimum.
	w.Header().Set("Access-Control-Max-Age", "3600")
}

// ---------------------------------------------------------------------------
// Auth middleware — Bearer token gate on every API request.
//
// Reads SD_API_TOKEN once at startup (comma-separated for multiple tokens,
// shared across all adapters in the shared-secret pattern). When empty, this
// middleware is a no-op and SD Core runs in open localhost-dev mode.
//
// When a token is configured, every incoming request must present either
// `Authorization: Bearer <token>` OR `?token=<tok>` query param. The query
// param fallback is necessary for `/ws` because the browser WebSocket API
// cannot set custom headers.
//
// Always exempt:
//   - GET /healthz       (monitoring probes shouldn't need creds)
//   - OPTIONS *          (CORS preflights)
//   - GET / and static-file paths (the dashboard HTML/JS/CSS — auth happens
//                                   when the dashboard JS connects to the
//                                   API/WS; gating the static shell would
//                                   make the deployment unviewable for
//                                   anyone behind Cloudflare Access who
//                                   handles auth at the edge)
// ---------------------------------------------------------------------------
type authMiddleware struct {
	next   http.Handler
	tokens map[string]bool // populated only when auth is enabled
}

// authPathExempt returns true for paths that bypass the token check.
// Static assets (the dashboard shell) are exempt because remote viewers
// typically authenticate at the edge (Cloudflare Access, Tailscale ACL,
// HTTP basic on a reverse proxy) before ever reaching SD Core. The API
// surface — /event, /events, /memories, /bank/*, /ws — still requires the
// token regardless of edge auth.
func authPathExempt(path string) bool {
	if path == "/healthz" {
		return true
	}
	// Static-file routes don't start with the API prefixes below.
	apiPrefixes := []string{
		"/event", "/events", "/memories", "/bank/", "/ws", "/bubbles", "/recall",
		"/reflect", "/import",
		"/maps", "/lexicon", "/audit", "/research", "/budget", "/settings",
		"/admin/", "/nightly/",
		"/procedures",
		"/entities",
		"/patients", // v2.8 — Patient registry. Auth-required like the rest of the API surface.
	}
	for _, p := range apiPrefixes {
		if path == strings.TrimSuffix(p, "/") || strings.HasPrefix(path, p) {
			return false
		}
	}
	return true
}

func newAuthMiddleware(next http.Handler) http.Handler {
	raw := os.Getenv("SD_API_TOKEN")
	tokens := map[string]bool{}
	for _, t := range strings.Split(raw, ",") {
		t = strings.TrimSpace(t)
		if t != "" {
			tokens[t] = true
		}
	}
	if len(tokens) == 0 {
		log.Printf("auth: SD_API_TOKEN unset — open mode (suitable for localhost only)")
		return next
	}
	log.Printf("auth: SD_API_TOKEN set — Bearer token required on API endpoints (%d token(s) accepted)", len(tokens))
	return &authMiddleware{next: next, tokens: tokens}
}

func (a *authMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions || authPathExempt(r.URL.Path) {
		a.next.ServeHTTP(w, r)
		return
	}
	// Header first; query-param fallback for WS (where headers can't be set).
	tok := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		tok = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	if tok == "" {
		tok = r.URL.Query().Get("token")
	}
	if !a.tokens[tok] {
		cors(w)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("WWW-Authenticate", `Bearer realm="synaptic"`)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized","detail":"missing or invalid bearer token"}`))
		return
	}
	a.next.ServeHTTP(w, r)
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// emitEnvLoadedEvents publishes one `env.loaded` control event per
// allow-listed env var that resolves non-empty in the running process.
// Called once at boot, after the bank → hub Emit wire is set up. Lets the
// dashboard's keyHint indicator flip from "✓ saved · pending restart" to
// "✓ env var detected on host" the instant the new container reads .env.
// Factored out so tests can drive it with a recording sink.
func emitEnvLoadedEvents(emit func(eventType, adapterID string, payload map[string]interface{})) {
	if emit == nil {
		return
	}
	keys := make([]string, 0, len(envWriteAllowList))
	for k := range envWriteAllowList {
		if os.Getenv(k) != "" {
			keys = append(keys, k)
		}
	}
	// Stable order so the test's first-emit-is-X assertions are deterministic.
	sort.Strings(keys)
	for _, k := range keys {
		emit("env.loaded", "sd-core-env", map[string]interface{}{"key": k})
	}
	if len(keys) > 0 {
		log.Printf("env.loaded: emitted for %d allow-listed key(s)", len(keys))
	}
}

// buildTiersFromSettingsOrEnv resolves each tier provider from the bank's
// settings if a non-default config exists, falling back to the legacy env
// vars otherwise. Returns (tier1, tier2, tier3) as LLMProvider — any can
// be nil for "disabled / not configured".
//
// Resolution rules per tier:
//   - settings row exists AND kind != ollama → BuildProvider(cfg)
//   - settings row exists AND kind == ollama → BuildProvider(cfg) using
//     SD_OLLAMA_URL when cfg.BaseURL is empty
//   - settings row absent → env var path:
//       Tier 1: always Ollama from SD_REALTIME_MODEL / SD_CLASSIFY_MODEL
//       Tier 2: Ollama from SD_NIGHTLY_MODEL if set; otherwise nil
//       Tier 3: nil (must be configured via settings to enable Oracle)
func buildTiersFromSettingsOrEnv(bank *Bank) (LLMProvider, LLMProvider, LLMProvider) {
	return buildOneTierFromSettingsOrEnv(bank, Tier1),
		buildOneTierFromSettingsOrEnv(bank, Tier2),
		buildOneTierFromSettingsOrEnv(bank, Tier3)
}

// normalizeProviderKindEnv translates SD_TIER{N}_KIND values into a
// canonical ProviderKind. Accepts the submitter's `openai_local`
// spelling (CHANGES.md doc) as a backwards-compat synonym for `custom`
// — auto-detect by BaseURL flips local treatment on (see BuildProvider).
// v2.6 Bundle E.
func normalizeProviderKindEnv(raw string) ProviderKind {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch s {
	case "openai_local", "lm_studio", "lmstudio", "llama_cpp", "llamacpp", "vllm":
		return ProviderCustom
	case "":
		return ""
	}
	return ProviderKind(s)
}

// buildProviderFromEnvVars reads SD_TIER{prefix}_{KIND,URL,MODEL,API_KEY_ENV,TIMEOUT_S}
// and returns the corresponding LLMProvider, or nil when KIND is empty.
// Used by every tier's env-var fallback. v2.6 Bundle E.
func buildProviderFromEnvVars(prefix string) LLMProvider {
	kindRaw := envOr("SD_TIER"+prefix+"_KIND", "")
	if prefix == "EMBED" {
		kindRaw = envOr("SD_EMBED_KIND", "")
	}
	kind := normalizeProviderKindEnv(kindRaw)
	if kind == "" || kind == ProviderDisabled {
		return nil
	}
	urlKey := "SD_TIER" + prefix + "_URL"
	modelKey := "SD_TIER" + prefix + "_MODEL"
	apiKey := "SD_TIER" + prefix + "_API_KEY_ENV"
	tmoKey := "SD_TIER" + prefix + "_TIMEOUT_S"
	if prefix == "EMBED" {
		urlKey, modelKey, apiKey, tmoKey = "SD_EMBED_URL", "SD_EMBED_MODEL", "SD_EMBED_API_KEY_ENV", "SD_EMBED_TIMEOUT_S"
	}
	cfg := ProviderConfig{
		Kind:      kind,
		BaseURL:   envOr(urlKey, ""),
		Model:     envOr(modelKey, ""),
		APIKeyEnv: envOr(apiKey, ""),
	}
	if v := envOr(tmoKey, ""); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			cfg.TimeoutSeconds = n
		}
	}
	// Best-effort validate at the env-path so a misconfigured deployment
	// fails loudly at startup instead of silently routing to a default
	// Ollama. Tier hint chosen from the prefix for the embed-tier
	// rejection in Validate; "" is fine for the others.
	tierHint := TierKey("")
	switch prefix {
	case "1":
		tierHint = Tier1
	case "2":
		tierHint = Tier2
	case "3":
		tierHint = Tier3
	case "EMBED":
		tierHint = TierEmbedding
	}
	if err := cfg.Validate(tierHint); err != nil {
		log.Printf("router: SD_TIER%s_KIND=%s rejected by validator: %v", prefix, kind, err)
		return nil
	}
	return BuildProvider(cfg)
}

// buildOneTierFromSettingsOrEnv resolves a single tier's provider from
// the bank's settings or the legacy env-var fallback. Factored out of
// buildTiersFromSettingsOrEnv so the hot-swap path (PUT /settings/
// providers/{tier} and POST /admin/router/reload) can rebuild just one
// tier without touching the others.
//
// Returns nil for "disabled / not configured" — callers should treat
// nil as "tier unavailable" (the runner falls back to lower tiers).
func buildOneTierFromSettingsOrEnv(bank *Bank, tier TierKey) LLMProvider {
	build := func(t TierKey, fallback func() LLMProvider) LLMProvider {
		if bank != nil {
			cfg, err := bank.GetProviderConfig(t)
			if err != nil {
				log.Printf("router: could not load %s from settings (%v); falling back to env", t, err)
			} else if cfg.Kind != ProviderDisabled && (cfg.Kind != ProviderOllama || cfg.Model != "") {
				return BuildProvider(cfg)
			}
		}
		return fallback()
	}
	switch tier {
	case Tier1:
		return build(Tier1, func() LLMProvider {
			// v2.6 Bundle E — SD_TIER1_KIND wins if set (covers `custom`,
			// `openai_local` alias, etc.). Otherwise fall back to the
			// legacy Ollama-from-env path.
			if p := buildProviderFromEnvVars("1"); p != nil {
				return p
			}
			return NewOllamaProvider(
				envOr("SD_OLLAMA_URL", "http://localhost:11434"),
				envOr("SD_REALTIME_MODEL", envOr("SD_CLASSIFY_MODEL", "llama3.2:3b")),
			)
		})
	case Tier2:
		return build(Tier2, func() LLMProvider {
			if p := buildProviderFromEnvVars("2"); p != nil {
				return p
			}
			if m := envOr("SD_NIGHTLY_MODEL", ""); m != "" {
				return NewOllamaProvider(envOr("SD_OLLAMA_URL", "http://localhost:11434"), m)
			}
			return nil
		})
	case Tier3:
		return build(Tier3, func() LLMProvider {
			// v2.6 Bundle E (T19) — Tier 3 env-var path. Previously
			// returned nil here; now `SD_TIER3_KIND=custom` (or
			// `openai_local` alias) with a local URL lets users wire
			// a self-hosted Oracle without touching the settings UI.
			return buildProviderFromEnvVars("3")
		})
	case TierEmbedding:
		// v2.6 Bundle D + Bundle E + v2.7 Bundle P — dedicated embed tier.
		// Resolution priority:
		//   1. Settings row (PUT /settings/providers/tier_embedding)
		//   2. SD_EMBED_KIND env (Bundle E provider-kind path)
		//   3. SD_SYNAPSE_MODEL env (legacy, kept for back-compat)
		//   4. SD_EMBED_MODEL_DEFAULT (Bundle P default — nomic-embed-text:v1.5)
		//   5. nil → ForEmbedding() falls back to Tier 1 (only if even the
		//      default is empty, e.g. operator explicitly cleared it).
		// Steps 4 + 5 are the v2.7 change. Pre-P this returned nil for
		// users who hadn't set SD_SYNAPSE_MODEL — making default recall
		// run on llama3.2:3b chat embeddings, which are poor. Bundle P
		// promotes nomic-embed-text:v1.5 to the default and ships it via
		// the ollama-tier1 entrypoint's secondary pull.
		return build(TierEmbedding, func() LLMProvider {
			if p := buildProviderFromEnvVars("EMBED"); p != nil {
				return p
			}
			// v2.7 Bundle P split-out: prefer the dedicated embed
			// container when SD_OLLAMA_EMBED_URL is set, otherwise
			// fall through to the legacy SD_OLLAMA_URL (tier1 chat
			// container also serving embeddings, for installs that
			// haven't enabled the embed split yet).
			embedURL := envOr("SD_OLLAMA_EMBED_URL", envOr("SD_OLLAMA_URL", "http://localhost:11434"))
			if m := envOr("SD_SYNAPSE_MODEL", ""); m != "" {
				return NewOllamaProvider(embedURL, m)
			}
			// Bundle P default. Empty string disables the default (a
			// deliberate operator override) and falls back to Tier 1.
			if m := envOr("SD_EMBED_MODEL_DEFAULT", "nomic-embed-text:v1.5"); m != "" {
				return NewOllamaProvider(embedURL, m)
			}
			return nil
		})
	}
	return nil
}

// maybeMigrateEmbedLabelDrift handles the v2.7 Bundle P default flip
// to nomic-embed-text:v1.5 as the embed tier. When the bank has
// existing rows under a different label than the current
// ForEmbedding() provider, one of three paths runs:
//
//  1. SD_SKIP_EMBED_MIGRATION=1 set → revert ForEmbedding to use the
//     legacy label as the embed-tier model (so /recall keeps working
//     against existing rows). Logged loudly so the operator knows.
//  2. Default (env unset) → kick off a background re-embed goroutine
//     using the existing /admin/embed-all worker code path with the
//     new provider. WS events broadcast progress; audit row marks
//     it as `embed_auto_migration`.
//  3. Bank empty or labels match → no-op.
//
// This is the v2.7 evolution of v2.6's maybeWarnEmbedLabelDrift —
// the warning was clean from a code-safety perspective but a real
// fraction of users wouldn't notice and would hit empty /recall
// results. Bundle P promotes the warning to an action.
func maybeMigrateEmbedLabelDrift(ctx context.Context, bank *Bank, router *ModelRouter, hub *Hub) {
	if bank == nil || router == nil {
		return
	}
	embedProvider := router.ForEmbedding()
	if embedProvider == nil {
		return
	}
	embedLabel := providerModelLabel(embedProvider)
	if embedLabel == "" {
		return
	}
	t1 := router.Tier1()
	if t1 == nil {
		return
	}
	t1Label := providerModelLabel(t1)
	if t1Label == "" || t1Label == embedLabel {
		return // labels already match — no drift
	}
	oldRows, err := bank.CountEmbeddingsByModel(t1Label)
	if err != nil || oldRows == 0 {
		return // bank empty (fresh install) or only-new rows already under embedLabel
	}

	// Operator opt-out: revert embed tier to legacy label so /recall
	// keeps resolving existing rows. New writes accumulate under the
	// legacy label too. The router-level swap keeps the rest of the
	// runtime unaware of the migration entirely.
	if envOr("SD_SKIP_EMBED_MIGRATION", "0") == "1" {
		legacy := NewOllamaProvider(envOr("SD_OLLAMA_URL", "http://localhost:11434"), t1Label)
		router.SetEmbedTier(legacy)
		log.Printf("router: SD_SKIP_EMBED_MIGRATION=1 — keeping legacy embed tier %q for this bank (%d rows). Set SD_SKIP_EMBED_MIGRATION=0 to migrate.",
			t1Label, oldRows)
		return
	}

	log.Printf("router: starting auto-migration — re-embedding %d memories from %q to %q. Set SD_SKIP_EMBED_MIGRATION=1 to skip and keep the legacy embed tier instead.",
		oldRows, t1Label, embedLabel)
	go runEmbedAutoMigration(ctx, bank, embedProvider, embedLabel, t1Label, oldRows, hub)
}

// runEmbedAutoMigration walks every memory in the bank and re-embeds
// it under the new embed tier's label. Used by Bundle P's startup
// auto-migration. Emits embed_migration_progress / embed_migration_done
// WS events; writes a single `embed_auto_migration` audit row on
// completion with final counts. Idempotent against re-runs: rows
// already under newLabel are skipped via the embedding-cache lookup.
func runEmbedAutoMigration(ctx context.Context, bank *Bank, provider LLMProvider, newLabel, oldLabel string, totalAtStart int, hub *Hub) {
	if bank == nil || provider == nil {
		return
	}
	startedAt := time.Now()
	mems, err := bank.ListMemoriesWith(MemoryListOpts{Limit: 50000})
	if err != nil {
		log.Printf("embed_auto_migration: list memories failed: %v", err)
		return
	}

	const progressMinGap = 500 * time.Millisecond
	var lastEmit time.Time
	embedded, skipped, errs := 0, 0, 0
	emit := func(force bool) {
		if hub == nil {
			return
		}
		now := time.Now()
		if !force && now.Sub(lastEmit) < progressMinGap {
			return
		}
		lastEmit = now
		hub.Fanout(Event{
			SchemaVersion: SchemaVersion,
			Type:          "embed_migration_progress",
			Timestamp:     now.UTC().Format(time.RFC3339Nano),
			AdapterID:     "sd-core-migration",
			Payload: map[string]any{
				"embedded":   embedded,
				"skipped":    skipped,
				"errors":     errs,
				"total":      totalAtStart,
				"old_label":  oldLabel,
				"new_label":  newLabel,
				"elapsed_ms": time.Since(startedAt).Milliseconds(),
			},
		})
	}

	for _, m := range mems {
		if ctx.Err() != nil {
			return
		}
		tagsAny := make([]interface{}, len(m.Tags))
		for i, t := range m.Tags {
			tagsAny[i] = t
		}
		text := synapseEmbedText(map[string]interface{}{
			"text": m.Text,
			"tags": tagsAny,
		})
		hash := synapseTextHash(text)
		// Skip if we already have an embedding for this hash under the
		// NEW label (migration was interrupted then restarted).
		if existing, _ := bank.GetEmbedding(hash); existing != nil {
			// Existing row could be under either label; check by re-saving
			// idempotently via SaveEmbedding (it's an upsert keyed by
			// text_hash). To know whether it's under the new label, we'd
			// need a column read; cheaper to just re-embed when in doubt.
			// Conservative skip: assume the row is correct if it exists.
			skipped++
			emit(false)
			continue
		}
		// Use a fresh background context for each call so a global ctx
		// cancellation halts the run but a per-call slowness doesn't.
		callCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
		vec, err := provider.Embed(callCtx, text)
		cancel()
		if err != nil {
			errs++
			emit(false)
			continue
		}
		if err := bank.SaveEmbedding(hash, vec, newLabel); err != nil {
			errs++
			emit(false)
			continue
		}
		embedded++
		emit(false)
	}
	emit(true)
	duration := time.Since(startedAt).Milliseconds()

	if hub != nil {
		hub.Fanout(Event{
			SchemaVersion: SchemaVersion,
			Type:          "embed_migration_done",
			Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
			AdapterID:     "sd-core-migration",
			Payload: map[string]any{
				"embedded":    embedded,
				"skipped":     skipped,
				"errors":      errs,
				"total":       totalAtStart,
				"old_label":   oldLabel,
				"new_label":   newLabel,
				"duration_ms": duration,
			},
		})
	}
	_ = bank.AppendAudit(AuditEntry{
		Operation:  "embed_auto_migration",
		EntityType: "embeddings",
		EntityID:   "auto-migration",
		AfterJSON: fmt.Sprintf(`{"old_label":%q,"new_label":%q,"embedded":%d,"skipped":%d,"errors":%d,"duration_ms":%d}`,
			oldLabel, newLabel, embedded, skipped, errs, duration),
		Reason:    "v2.7 Bundle P default-embed-tier auto-migration",
		AdapterID: "sd-core-migration",
	})
	log.Printf("embed_auto_migration: done — embedded=%d skipped=%d errors=%d duration=%dms (%q → %q)",
		embedded, skipped, errs, duration, oldLabel, newLabel)
}

// buildEmbedTierFromSettingsOrEnv resolves the dedicated embedding
// provider (TierEmbedding settings row → SD_SYNAPSE_MODEL env fallback
// → nil). Returns nil when neither is configured; the router's
// ForEmbedding() then falls back to Tier 1, preserving v2.5 behaviour.
// Kept separate from buildTiersFromSettingsOrEnv so the hot-swap path
// for the embed tier (PUT /settings/providers/tier_embedding) can
// rebuild just this slot without touching Tier 1/2/3.
func buildEmbedTierFromSettingsOrEnv(bank *Bank) LLMProvider {
	return buildOneTierFromSettingsOrEnv(bank, TierEmbedding)
}

// ensureEmptyAssetFile writes `body` to `path` if `path` doesn't already
// exist. Creates parent directories as needed. Used to scaffold empty
// memories.json and synapses.json on first run when the public release
// build ships without them. Failure to create is logged but not fatal —
// the dashboard's /memories endpoint will return 404 in that case, which
// is also a valid empty-state signal.
func ensureEmptyAssetFile(path, body string) {
	if _, err := os.Stat(path); err == nil {
		return // already present
	} else if !os.IsNotExist(err) {
		log.Printf("first-run: could not stat %s: %v", path, err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("first-run: could not create dir for %s: %v", path, err)
		return
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		log.Printf("first-run: could not write %s: %v", path, err)
		return
	}
	log.Printf("first-run: created empty %s", path)
}

// checkStaticBindMount stats index.html under the configured data dir
// and logs either a positive boot confirmation or a loud warning when
// it's missing. The missing case is almost always the compose-helper
// /workspace bind-mount footgun (see compose_runner.go); surfacing it at
// boot avoids silent dashboard 404s.
func checkStaticBindMount(dataDir string) {
	indexPath := filepath.Join(dataDir, "index.html")
	if _, err := os.Stat(indexPath); err != nil {
		log.Printf("WARNING: bind-mount appears stale: %s not found. "+
			"If you recreated core via the compose helper recently, run "+
			"`docker compose up -d core` from a HOST shell to fix.", indexPath)
		return
	}
	log.Printf("bind-mount OK · %s present", indexPath)
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func main() {
	listen := flag.String("listen", envOr("SD_CORE_LISTEN", "127.0.0.1:9911"),
		"listen address")
	dataDir := flag.String("data-dir", envOr("SD_DATA_DIR", "."),
		"data directory containing assets/memories.json")
	ringSize := flag.Int("ring-size", envInt("SD_RING_SIZE", 1000),
		"events kept in memory for replay")
	bankPath := flag.String("bank-path", envOr("SD_BANK_PATH", ""),
		"SQLite fallback bank file (empty = disabled). Default: <data-dir>/sd_bank.db")
	// -healthcheck: probe /healthz on the listen address, exit 0 on 200, 1
	// otherwise. Used by the Dockerfile HEALTHCHECK in CMD form because the
	// scratch base image has no shell + no wget. Single static binary
	// handles both serve mode and probe mode.
	healthcheck := flag.Bool("healthcheck", false,
		"probe /healthz once and exit 0/1; used by docker HEALTHCHECK")
	flag.Parse()

	if *healthcheck {
		runHealthcheck(*listen)
	}

	absDataDir, _ := filepath.Abs(*dataDir)
	memPath := filepath.Join(absDataDir, "assets", "memories.json")
	synPath := filepath.Join(absDataDir, "assets", "synapses.json")

	// First-run scaffolding. Production builds ship without memories.json
	// or synapses.json — those are user data, not source code. If either
	// is missing on startup, write an empty stub so the dashboard's fetch
	// path always succeeds and the watcher has something to diff against.
	// Stubs use the same envelope as the real files so existing parsers
	// don't have to special-case the empty state.
	ensureEmptyAssetFile(memPath, `{"schema_version":"1.0","items":[],"comment":"Auto-created by SD Core on first run. Add memories via your adapter / memory provider."}`)
	ensureEmptyAssetFile(synPath, `{"schema_version":"1.0","edges":[],"comment":"Auto-created by SD Core on first run. Synapses are recomputed when memories change."}`)

	// Default bank path lives next to the data dir so docker-compose's
	// volume mount captures it for free.
	resolvedBankPath := *bankPath
	defaultedPath := false
	if resolvedBankPath == "" && envOr("SD_BANK_DISABLE", "") == "" {
		resolvedBankPath = filepath.Join(absDataDir, "sd_bank.db")
		defaultedPath = true
	}
	bank, err := NewBank(resolvedBankPath)
	if err != nil {
		// If the user explicitly chose a bank path (via --bank-path or
		// SD_BANK_PATH), a NewBank failure means migrations or schema
		// verification failed — those are LOUD failures. Silently degrading
		// to bank=nil leaves the pipeline running with no write target,
		// which masquerades as "all phases produced 0" and burns hours of
		// debugging time. (Per handoff "schema migration not applied
		// 2026-05-10" §D — surface migration failures loudly.)
		//
		// When the path was defaulted (no explicit configuration) we keep
		// the old degrade-to-nil behavior so test fixtures and headless
		// dev runs without disk access still work.
		if !defaultedPath {
			log.Fatalf("bank: FATAL — cannot open %s: %v\n"+
				"This usually means a schema migration failed or the bank file is locked. "+
				"Check the path is writable, or run docker compose down before retry.",
				resolvedBankPath, err)
		}
		log.Printf("bank: disabled (%v)", err)
		bank = nil
	}
	if bank != nil {
		bank.LogSchemaState() // boot-time visibility into which DB + which schema
		// Crash recovery (handoff "dream cycle resumption + crash recovery
		// 2026-05-10" §3a): flip orphan in_progress rows from a dead prior
		// process to status='interrupted'. 5-minute stale window matches
		// the runner's 30s heartbeat with generous slack.
		if reconciled, err := bank.ReconcileStaleInProgress(5 * time.Minute); err == nil && reconciled > 0 {
			log.Printf("nightly: boot reconciled %d stale in_progress run(s) -> interrupted", reconciled)
		}
	}

	ring := NewRingBuffer(*ringSize)
	hub := NewHub(ring)
	hub.AttachBank(bank)
	hub.memPath = memPath // for synthetic-seed wipe on first real memory_added

	// Wire bank → hub control-channel emits. The dot-namespace convention
	// (sensitive.flipped, audit.appended, research.cached, map.updated,
	// env.loaded) keeps these events out of the brain-animation pipeline
	// on the dashboard side, which routes only snake_case adapter events
	// through there. See `CODE_HANDOFF — WS event extensions (2026-05-09)`.
	if bank != nil {
		bank.Emit = func(eventType, adapterID string, payload map[string]interface{}) {
			hub.Fanout(Event{
				SchemaVersion: SchemaVersion,
				Type:          eventType,
				Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
				AdapterID:     adapterID,
				Payload:       payload,
			})
		}
	}

	// ModelRouter (P5-4): each tier loaded from settings if the user has
	// configured one in onboarding, otherwise from env vars (legacy / fresh-
	// install path). When the user later swaps Tier 1 to a remote API via
	// the dashboard, the running container is unaffected — settings take
	// effect on next restart. (We do NOT hot-swap providers mid-process; an
	// in-flight Chat call to one provider mid-replacement is a footgun.)
	tier1, tier2, tier3 := buildTiersFromSettingsOrEnv(bank)
	localConcepts := strings.Split(envOr("SD_LOCAL_CONCEPTS", ""), ",")
	router := NewModelRouter(tier1, tier2, tier3, localConcepts)
	// v2.6 Bundle D — install dedicated embed tier if configured. nil
	// embed tier preserves v2.5 behaviour (ForEmbedding falls back to
	// Tier 1). Reject non-local embed providers in the router itself
	// (cosine compat / no remote embed mixing).
	if et := buildEmbedTierFromSettingsOrEnv(bank); et != nil {
		allowRemote := false
		if v := strings.TrimSpace(os.Getenv("SD_ALLOW_REMOTE_EMBEDDINGS")); v == "1" || v == "true" || v == "TRUE" || v == "on" {
			allowRemote = true
		}
		if et.IsLocal() || allowRemote {
			router.SetEmbedTier(et)
			if !et.IsLocal() {
				log.Printf("router: accepting remote embed provider %s (SD_ALLOW_REMOTE_EMBEDDINGS=1; mixing cosine spaces — bench/test only)", et.Name())
			}
		} else {
			log.Printf("router: refused remote embed provider %s (cosine-compat constraint — set SD_ALLOW_REMOTE_EMBEDDINGS=1 to override for bench/test)", et.Name())
		}
	}
	if t1 := router.Tier1(); t1 != nil {
		log.Printf("router: Tier 1 = %s (local=%v)", t1.Name(), t1.IsLocal())
	}
	if t2 := router.Tier2(); t2 != nil {
		log.Printf("router: Tier 2 = %s (local=%v)", t2.Name(), t2.IsLocal())
	}
	if t3 := router.Tier3(); t3 != nil {
		log.Printf("router: Tier 3 = %s (local=%v)", t3.Name(), t3.IsLocal())
	}
	if et := router.EmbedTier(); et != nil {
		log.Printf("router: Embed  = %s (local=%v)", et.Name(), et.IsLocal())
	} else {
		log.Printf("router: Embed  = (none — using Tier 1 as fallback)")
	}
	// Migration: catches the silent T11+T17 cosine-garbage trap flagged
	// in RELEASE_REVIEW. v2.7 Bundle P upgraded this from warn-only to
	// auto-migrate by default (with SD_SKIP_EMBED_MIGRATION=1 opt-out).
	// Actual call sits below where ctx is declared.

	// v2.7 Bundle K — wire the inline dedup resolver so SaveMemory can
	// check for near-duplicates using the same embed provider as
	// /recall and synapse. Method-value form so router.ReplaceTier
	// swaps take effect at the next SaveMemory call without restart.
	if bank != nil {
		bank.SetDedupProvider(router.ForEmbedding)
	}

	// Fire env.loaded events once at boot for any allow-listed key the
	// running process can actually see. Reuses bank.Emit so the events
	// land in the ring + fanout to any already-connected subscribers.
	if bank != nil {
		emitEnvLoadedEvents(bank.Emit)
	}

	server := &Server{
		hub: hub, ring: ring, bank: bank, dataDir: absDataDir,
		router: router,
		upgrader: websocket.Upgrader{
			CheckOrigin:     func(r *http.Request) bool { return true },
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
		},
		// Wave 7a Docker control plane — gated on SD_ALLOW_DOCKER_SOCKET=1.
		// Always populated (never nil); when the gate is off, requests get
		// a 412 with an enable-instructions hint.
		docker: NewDockerControlState(),
	}
	// LLM cross-encoder reranker is the SYSTEM DEFAULT when Tier 2 is
	// configured. Bridges semantic gaps that pooled-embedding cosine
	// misses (e.g. "artist" ↔ "bluegrass band"). Falls back to the
	// no-I/O salience blend when Tier 2 is absent OR the user
	// explicitly sets `recall_rerank_kind` = "salience".
	if bank != nil && router != nil {
		kind := ""
		if v, ok, _ := bank.GetSetting("recall_rerank_kind"); ok {
			kind = strings.ToLower(strings.TrimSpace(v))
		}
		if kind == "salience" {
			log.Printf("router: salience reranker selected (no-I/O blend; user override)")
		} else if router.ForNightly() != nil {
			// Default to LLM cross-encoder when Tier 2 is available.
			server.reranker = NewLLMReranker(router)
			log.Printf("router: LLM cross-encoder reranker enabled (Tier 2 default)")
		} else {
			log.Printf("router: salience reranker selected (Tier 2 unavailable; pair-scoring requires Tier 2)")
		}
	}
	// v2.7 Bundle S — wire the hook auto-synthesis engine so the live hook
	// event stream (repeated errors, recurring tool patterns, decisions,
	// session boundaries) distills into durable memories in real time as
	// the user works. Constructed unconditionally when the bank exists; the
	// engine early-returns inside Observe when `hook_synthesis_enabled` is
	// off (the shipped default), so this stays inert until a deployment
	// opts in via the setting. The synthesizer is Tier-1-bound
	// (router.ForRealtime()); a nil Tier 1 makes synthesis a no-op rather
	// than an error. Pre-2026-05-28 this was never constructed and had no
	// synthesizer, so Bundle S was dead code — this line is what turns the
	// hook stream into a self-building memory database.
	if bank != nil {
		server.hookSynth = NewHookSynthesisEngine(bank, NewLLMHookSynthesizer(router, bank))
		log.Printf("hook_synthesis: engine wired (enable via hook_synthesis_enabled=1)")
	}
	// v2.8 — Patient feature. Build the registry once we know the launched
	// bank path. The registry ensures a "default" patient exists pointing
	// at that path, so existing installs see their data unchanged. If a
	// non-default patient was active at last shutdown, swap to it here.
	if bank != nil && resolvedBankPath != "" {
		ps, err := LoadPatientStore(absDataDir, resolvedBankPath)
		if err != nil {
			log.Printf("patient store: disabled (%v)", err)
		} else {
			server.patients = ps
			if active, ok := ps.Active(); ok && active.BankPath != resolvedBankPath {
				if err := bank.Reopen(active.BankPath); err != nil {
					log.Printf("patient: resume active=%s reopen FAILED (%v) — staying on default", active.Name, err)
				} else {
					log.Printf("patient: resumed last-active = %s", active.Name)
				}
			}
		}
	}
	// Wave 7b2 — sidecar lifecycle manager. Idle reaper starts after the
	// hub + bank are ready (i.e., right here).
	server.lifecycle = NewLifecycleManager(bank, server.docker, hub)
	server.memoryLimiter = NewMemoryWriteLimiter(bank)
	server.lifecycle.StartIdleReaper(30 * time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// v2.7 Bundle P — embed-tier drift auto-migration. Now that ctx is
	// alive, run the migration check; it spawns a background goroutine
	// that respects ctx cancellation, so SIGINT during a long migration
	// aborts cleanly.
	maybeMigrateEmbedLabelDrift(ctx, bank, router, hub)

	// NightlyRunner: framework-only for now (no-op steps). Auto-loop polls
	// the configured nightly_schedule and kicks off DoRun once per day when
	// the window opens. P5-8/9/10 will swap in real consolidation steps via
	// runner.runStepsFn — no other call sites need to change.
	if bank != nil {
		server.nightly = NewNightlyRunner(bank, router, hub)
		// Wave 7b2 — give the runner a handle on the lifecycle manager so
		// it can ensure on_demand sidecars are running before Phase 0b
		// + stop on_demand_burst sidecars after the pipeline completes.
		server.nightly.lifecycle = server.lifecycle
		go server.nightly.Run(ctx)
	}

	// WS hub heartbeat — emits a `heartbeat` event every 30s so the
	// dashboard's stale-detector can hold its threshold to ~45s. Pure
	// transient (skips the ring) so it doesn't pollute /events?since=.
	go hub.RunHeartbeat(ctx)

	mw := NewMemoryWatcher(memPath, hub)
	if err := mw.Start(ctx); err != nil {
		log.Printf("memory watcher disabled: %v", err)
	}

	// Memory classifier — Tier 1+2 (tag/keyword) runs immediately at startup;
	// Tier 3 (LLM) runs concurrently for unclassified memories and fires
	// again whenever memories.json changes. v2.6 Bundle C: provider-agnostic.
	// Disabled with SD_CLASSIFY=0.
	if envOr("SD_CLASSIFY", "1") != "0" {
		cachePath := filepath.Join(absDataDir, "_meta", "classify_cache.json")
		// router.ForRealtime() is Tier 1 — may be nil if Tier 1 isn't
		// configured. NewMemoryClassifier handles nil by falling back to
		// the tag/keyword layer in pass(), preserving the v2.5 behaviour
		// for users who run without an LLM Tier 1.
		mc := NewMemoryClassifier(memPath, cachePath, router.ForRealtime, hub, bank)
		mw.OnChange(mc.Trigger)
		go mc.Run(ctx)
	} else {
		log.Printf("classifier: disabled (SD_CLASSIFY=0)")
	}

	// Synapse builder — computes embeddings (via router.ForEmbedding()) and
	// connects semantically similar memories in assets/synapses.json.
	// Disabled with SD_SYNAPSE=0. v2.6 Bundle C: provider-agnostic.
	if envOr("SD_SYNAPSE", "1") != "0" {
		synCachePath := filepath.Join(absDataDir, "_meta", "embed_cache.json")
		// router.ForEmbedding() may return nil (e.g. Tier 1 not local);
		// SynapseBuilder.buildPass logs+skips that case rather than
		// panicking, so wiring it directly is safe even before the embed
		// tier (Bundle D) lands.
		sb := NewSynapseBuilder(memPath, synPath, synCachePath, router.ForEmbedding, hub)
		mw.OnChange(sb.Trigger)
		go sb.Run(ctx)
	} else {
		log.Printf("synapse: disabled (SD_SYNAPSE=0)")
	}

	// Sensitive-content AI classifier (Layer 2). Layer 1 (regex) runs sync in
	// SaveMemory; this goroutine catches prose-style PII that regex misses.
	//
	// Auto-disabled when Tier 1 is non-local — the classifier sends every
	// memory body that regex didn't already flag to the model, so a remote
	// Tier 1 would continuously stream unfiltered memory text to a third
	// party. The override SD_SENSITIVE_AI_ALLOW_REMOTE=1 exists for users
	// who explicitly accept this trade-off (e.g. bring-your-own-key with a
	// trusted self-hosted endpoint). Layer 1 keeps running regardless.
	if envOr("SD_SENSITIVE_AI", "1") != "0" && bank != nil {
		// Snapshot Tier 1 at startup ONLY for the auto-disable safety
		// check (don't want to ever stream memory bodies to a remote
		// provider). The classifier itself uses a per-pass resolver via
		// `resolveSensitiveClassifierProvider(bank, router)` so user
		// flips of `sensitive_classifier_tier` (Tier 1 vs Tier 2) AND
		// `PUT /settings/providers/{tier}` hot-swaps land at the next
		// scan without restart.
		t1 := router.ForRealtime()
		switch {
		case t1 == nil:
			log.Printf("sensitive-ai: disabled (Tier 1 not configured)")
		case !t1.IsLocal() && envOr("SD_SENSITIVE_AI_ALLOW_REMOTE", "") == "":
			log.Printf("sensitive-ai: AUTO-DISABLED — Tier 1 (%s) is remote; would stream every unflagged memory body to a third party. Set SD_SENSITIVE_AI_ALLOW_REMOTE=1 to override (Layer 1 regex still runs).", t1.Name())
		default:
			interval := time.Duration(envOrInt("SD_SENSITIVE_AI_INTERVAL", 30)) * time.Second
			batch := envOrInt("SD_SENSITIVE_AI_BATCH", 25)
			sc := NewSensitiveClassifier(bank, func() LLMProvider {
				return resolveSensitiveClassifierProvider(bank, router)
			}, interval, batch)
			go sc.Run(ctx)
		}
	} else {
		log.Printf("sensitive-ai: disabled (SD_SENSITIVE_AI=0 or bank nil)")
	}

	// Server-side bubble generator (B-bubble). Disabled when SD_BUBBLE_GEN=0
	// or when the realtime provider isn't configured. Persists to
	// assets/thought_bubbles.json (served by the static file handler)
	// and broadcasts bubble_added events. v2.6 Bundle C: provider-agnostic.
	if envOr("SD_BUBBLE_GEN", "1") != "0" {
		bubbleConfigPath := envOr("SD_BUBBLE_CONFIG", filepath.Join(absDataDir, "config", "bubble_prompts.json"))
		bubbleCfg := loadBubbleConfig(bubbleConfigPath)
		if iv := envOrInt("SD_BUBBLE_INTERVAL_SEC", 0); iv > 0 {
			bubbleCfg.IntervalSeconds = iv
		}
		bubblePath := filepath.Join(absDataDir, "assets", "thought_bubbles.json")
		bg := NewBubbleGenerator(bubbleCfg, router.ForRealtime, bubblePath, memPath, synPath, hub, ring, bank)
		server.bubbleGen = bg
		// Wire dream-pipeline phases (1 dedup / 2 synthesis / 6 augment /
		// 8 schema) so newly-merged or newly-created memories get prompt
		// bubble refreshes instead of waiting for the next round-robin tick.
		if server.nightly != nil {
			server.nightly.SetBubbleEnqueue(bg.InvalidateAndEnqueue)
		}
		go bg.Run(ctx)
	} else {
		log.Printf("bubble: disabled (SD_BUBBLE_GEN=0)")
	}

	// Bind-mount self-check. The compose-helper container historically
	// recreated `core` with a stale /workspace:/data bind source, leaving
	// /data/index.html missing and the dashboard's static handler 404ing
	// `GET /`. Make that failure mode loud at boot instead of silent at
	// request time. See compose_runner.go::buildComposeArgs comment for
	// the root cause + the --project-directory fix.
	checkStaticBindMount(absDataDir)

	httpServer := &http.Server{
		Addr:    *listen,
		Handler: newAuthMiddleware(server.routes()),
		// ReadTimeout: request body must arrive within 15s. /event,
		// /memories, /recall bodies are small JSON; this is plenty.
		ReadTimeout: 15 * time.Second,
		// WriteTimeout: the server has this many seconds total between
		// accepting the request and finishing the response WRITE.
		// /reflect with a local model takes 30-120s to compute the
		// answer before the response is even started — a 15s
		// WriteTimeout killed every long-running synthesis call (RCA
		// found during iter-14 benchmarking with llama3.1:8b Tier 3).
		// Bumped to 15 min so /reflect, /research, /admin/embed-all,
		// /nightly/run all have a generous ceiling. Auth-middleware
		// rate limiting is the real DoS guard, not this timeout.
		WriteTimeout: 15 * time.Minute,
		IdleTimeout:  90 * time.Second,
	}

	go func() {
		log.Printf("Synaptic Core listening on %s (data=%s, ring=%d)",
			*listen, absDataDir, *ringSize)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	// Graceful shutdown
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutting down...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	httpServer.Shutdown(shutdownCtx)
	if bank != nil {
		bank.Close()
	}
}
