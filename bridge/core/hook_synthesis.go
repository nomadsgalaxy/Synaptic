// hook_synthesis.go — v2.7 Bundle S: auto-synthesis from hook event
// stream.
//
// The plugin's `forward.js` posts every Claude Code tool call to
// /event. With the synthesis engine ON, we periodically scan recent
// hook events per (adapter_id, session_id) and condense them into
// memory rows that capture institutional knowledge — "what we tried,
// what worked, what broke, what we decided."
//
// Four trigger types fire synthesis:
//
//   - error          — repeated tool errors with similar signatures
//                      → "lesson learned" memory ("retried foo 3 times,
//                      always Bash exit 127 — missing dependency")
//   - pattern        — same N-step tool sequence recurs
//                      → "workflow pattern" memory
//   - decision       — explicit decision-tagged event from the agent
//                      → "decision X was made because Y"
//   - session_end    — adapter signals a session boundary
//                      → "what happened in this session" summary
//
// All synthesised memories use memory_type="synthesis" and
// source="hook_auto_synthesis" so the dashboard and lineage tooling can
// distinguish them from manually-saved memories. They carry the
// triggering event ids in SynthesisSourceIDs (well, the closest analog
// — we use the audit_log as the indirect link).
//
// Default behaviour: OFF. Opt-in via the `hook_synthesis_enabled`
// setting so existing users see no behaviour change.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Setting keys.
const (
	HookSynthesisEnabledKey        = "hook_synthesis_enabled"
	HookSynthesisErrorThresholdKey = "hook_synthesis_error_threshold" // N repeats before fire (default 3)
	HookSynthesisPatternWindowKey  = "hook_synthesis_pattern_window"  // last N events to scan (default 12)
	// HookSynthesisRichCaptureKey gates whether the synthesizer includes the
	// redacted tool detail (arg_summary / result_snippet) the rich-capture
	// hook forwards. Default OFF — when off, synthesis sees tool names only
	// (the conservative, lower-egress default). GUI-toggleable in the
	// dashboard Privacy tab.
	HookSynthesisRichCaptureKey = "hook_synthesis_rich_capture"
	// HookSynthesisPatternEnabledKey gates the "recurring tool sequence"
	// (pattern) trigger. Default OFF — in practice the pattern trigger fires
	// constantly during normal work (WebSearch>WebFetch, Bash>Read loops) and
	// produces vacuous "repeatedly used Bash" memories that only see tool
	// names, drowning out the high-value error / session_end memories. Off by
	// default; opt back in via the dashboard Privacy tab. error + session_end
	// triggers are unaffected.
	HookSynthesisPatternEnabledKey = "hook_synthesis_pattern_enabled"
	// HookSynthesisCheckpointEnabledKey gates the real-time checkpoint
	// trigger. Default ON — this is the fix for long active sessions
	// generating nothing until they end.
	HookSynthesisCheckpointEnabledKey = "hook_synthesis_checkpoint_enabled"
	// HookSynthesisCheckpointSalienceKey is the accumulated-salience
	// threshold that fires a checkpoint at a turn boundary; a long single
	// turn checkpoints mid-way at 3x this. Default 6 — a substantive
	// sub-task (a few edits + a build, or an error) crosses it; a trivial
	// turn (a question + a couple of reads) does not.
	HookSynthesisCheckpointSalienceKey = "hook_synthesis_checkpoint_salience"
)

// HookSynthesisTrigger names the kind of synthesis that fired. Stored
// on the saved memory's tags (e.g., "trigger:error").
type HookSynthesisTrigger string

const (
	TriggerError      HookSynthesisTrigger = "error"
	TriggerPattern    HookSynthesisTrigger = "pattern"
	TriggerDecision   HookSynthesisTrigger = "decision"
	TriggerSessionEnd HookSynthesisTrigger = "session_end"
	// TriggerCheckpoint is the real-time "encode this episode" trigger. It
	// fires DURING a session (not just at the end) when a salient sub-task
	// completes — at a user-prompt boundary once enough salience has
	// accumulated, or mid-turn when a long burst of work crosses a hard cap.
	// This is the encoding half of the two-stage model (real-time encoding at
	// salient event boundaries → offline dream consolidation), grounded in
	// Event Segmentation Theory + Generative Agents' salience-accumulation
	// reflection. It is what captures long active sessions as they happen.
	TriggerCheckpoint HookSynthesisTrigger = "checkpoint"
)

// HookSynthesizer is the Tier-1-bound primitive that turns a cluster of
// hook events into a single durable memory. Swap-in via Server.synth.
type HookSynthesizer interface {
	Synthesize(ctx context.Context, trigger HookSynthesisTrigger, events []HookEvent) (HookSynthesisResult, error)
}

// HookSynthesisResult is what a synthesiser returns. Text becomes the
// memory body; tags are merged with `["synthesis","trigger:<kind>"]`.
type HookSynthesisResult struct {
	Text string
	Tags []string
}

// HookSynthesisEngine ingests hook events and decides when to call out
// to the synthesiser. Thread-safe.
type HookSynthesisEngine struct {
	bank        *Bank
	synth       HookSynthesizer
	mu          sync.Mutex
	bySession   map[string]*sessionWindow
	maxSessions int // cap to keep memory bounded (LRU-ish on overflow)
}

// sessionWindow buffers the most-recent events for one session.
type sessionWindow struct {
	events []HookEvent // chronological
	// firedKeys records signatures we've already synthesised so a repeated
	// pattern doesn't spam the bank.
	firedKeys map[string]bool
	// salienceAccum is the running "worth remembering" weight accumulated
	// for the CURRENT episode (since the last checkpoint / turn boundary).
	// Drives the real-time checkpoint trigger.
	salienceAccum float64
	// checkpointAnchorTS is the created_at of the event that started the
	// current episode (the last checkpoint or last user-prompt boundary).
	// fire() pulls the episode's events as those with created_at > this.
	checkpointAnchorTS string
}

// NewHookSynthesisEngine constructs an engine. Bank may be nil (engine
// no-ops); synth may be nil (engine logs but doesn't persist).
func NewHookSynthesisEngine(bank *Bank, synth HookSynthesizer) *HookSynthesisEngine {
	return &HookSynthesisEngine{
		bank:        bank,
		synth:       synth,
		bySession:   map[string]*sessionWindow{},
		maxSessions: 256,
	}
}

func hookSynthesisEnabled(b *Bank) bool {
	if b == nil {
		return false
	}
	raw, ok, err := b.GetSetting(HookSynthesisEnabledKey)
	if err != nil || !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

func hookSynthesisErrorThreshold(b *Bank) int {
	if b == nil {
		return 3
	}
	raw, ok, err := b.GetSetting(HookSynthesisErrorThresholdKey)
	if err != nil || !ok {
		return 3
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 2 || n > 50 {
		return 3
	}
	return n
}

func hookSynthesisPatternWindow(b *Bank) int {
	if b == nil {
		return 12
	}
	raw, ok, err := b.GetSetting(HookSynthesisPatternWindowKey)
	if err != nil || !ok {
		return 12
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 4 || n > 200 {
		return 12
	}
	return n
}

// hookSynthesisPatternEnabled reads hook_synthesis_pattern_enabled,
// defaulting to FALSE. When false the recurring-tool-sequence trigger never
// fires — error + session_end still do. Off by default because the pattern
// trigger produces low-signal "repeatedly used X" noise during normal work.
func hookSynthesisPatternEnabled(b *Bank) bool {
	if b == nil {
		return false
	}
	raw, ok, err := b.GetSetting(HookSynthesisPatternEnabledKey)
	if err != nil || !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// hookSynthesisRichCapture reads the hook_synthesis_rich_capture setting,
// defaulting to false. When true, the synthesizer includes the redacted
// arg_summary / result_snippet detail (the file paths, commands, queries,
// result snippets the rich-capture hook forwards) in the synthesis prompt —
// i.e. that detail reaches Tier 1. Read live on each synthesis so a GUI
// toggle takes effect immediately.
func hookSynthesisRichCapture(b *Bank) bool {
	if b == nil {
		return false
	}
	raw, ok, err := b.GetSetting(HookSynthesisRichCaptureKey)
	if err != nil || !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// hookSynthesisCheckpointEnabled reads hook_synthesis_checkpoint_enabled,
// defaulting to TRUE (this is the real-time capture fix; on by default).
func hookSynthesisCheckpointEnabled(b *Bank) bool {
	if b == nil {
		return false
	}
	raw, ok, err := b.GetSetting(HookSynthesisCheckpointEnabledKey)
	if err != nil || !ok {
		return true // default ON
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// hookSynthesisCheckpointSalience reads the checkpoint salience threshold,
// defaulting to 6. Clamped to a sane range.
func hookSynthesisCheckpointSalience(b *Bank) float64 {
	if b == nil {
		return 6
	}
	raw, ok, err := b.GetSetting(HookSynthesisCheckpointSalienceKey)
	if err != nil || !ok {
		return 6
	}
	v, perr := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if perr != nil || v < 2 || v > 100 {
		return 6
	}
	return v
}

// salience scores one hook event's "worth remembering" weight, cheaply (no
// LLM call) — by event type and tool name. It gates the real-time
// checkpoint trigger so trivial turns (a question + a couple of reads) don't
// produce a memory while substantive ones (edits, builds, errors, decisions)
// do. Grounded in salience/surprise-gated encoding: errors (prediction
// violated) and user prompts (event boundaries) score highest.
func salience(ev HookEvent) float64 {
	switch ev.EventType {
	case "error", "tool_error":
		return 4.0 // prediction violated — high-value lesson signal
	case "prompt_received":
		return 2.5 // user intent + an event boundary
	case "response_complete":
		return 1.0 // the assistant said something substantive
	case "memory_added":
		return 0.5
	case "tool_call":
		switch extractToolName(ev) {
		case "Edit", "Write", "MultiEdit", "NotebookEdit":
			return 2.0 // a durable change was made
		case "Task":
			return 2.0 // spawned a unit of work / subagent
		case "Bash":
			return 1.0 // build/deploy/test or routine — medium
		case "Read", "Grep", "Glob", "LS":
			return 0.2 // routine exploration
		}
		if strings.HasPrefix(extractToolName(ev), "mcp__") {
			return 0.8
		}
		return 0.5
	}
	return 0.0 // tool_result, model_thinking, session_*, recall, bubbles, etc.
}

// Observe is the entrypoint: postEvent calls this after persisting an
// event. Cheap on the calling goroutine (just buffers + evaluates triggers);
// each fired trigger's synthesis runs on a DETACHED background goroutine (see
// the loop below) so a slow Tier-1 Chat can't stall the plugin's 5s-timeout
// hook — the /event handler returns 202 immediately.
func (e *HookSynthesisEngine) Observe(ctx context.Context, ev HookEvent) {
	if e == nil || !hookSynthesisEnabled(e.bank) {
		return
	}
	key := ev.AdapterID + "|" + ev.SessionID
	e.mu.Lock()
	w, ok := e.bySession[key]
	if !ok {
		// Cap session map size (best-effort eviction — drop arbitrary entry).
		if len(e.bySession) >= e.maxSessions {
			for k := range e.bySession {
				delete(e.bySession, k)
				break
			}
		}
		w = &sessionWindow{firedKeys: map[string]bool{}}
		e.bySession[key] = w
	}
	window := hookSynthesisPatternWindow(e.bank)
	w.events = append(w.events, ev)
	if len(w.events) > window {
		w.events = w.events[len(w.events)-window:]
	}
	triggers := evaluateTriggers(w, ev,
		hookSynthesisErrorThreshold(e.bank),
		hookSynthesisPatternEnabled(e.bank),
		hookSynthesisCheckpointEnabled(e.bank),
		hookSynthesisCheckpointSalience(e.bank),
	)
	e.mu.Unlock()

	// Fire synthesis ASYNCHRONOUSLY. The caller is the /event HTTP handler,
	// whose ctx is cancelled the moment it writes 202 — and the plugin's
	// forward.js hook has a 5s timeout. A blocking Tier 1 Chat call here
	// would stall the hook (or get its ctx cancelled mid-flight). Detach
	// onto a fresh background context with a generous bound so synthesis
	// completes after the event response has returned, exactly like
	// emitLiveNeighbors. (The pre-2026-05-28 inline call was the reason a
	// slow synth could wedge the hook.)
	for _, tr := range triggers {
		tr := tr
		go func() {
			fctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := e.fire(fctx, tr.trigger, tr.events, tr.signatureKey, key, tr.sinceTS); err != nil {
				log.Printf("hook_synthesis: fire %s: %v", tr.trigger, err)
			}
		}()
	}
}

type pendingTrigger struct {
	trigger      HookSynthesisTrigger
	events       []HookEvent
	signatureKey string // dedup so the same pattern doesn't re-fire
	// sinceTS, when set (checkpoint), tells fire() to distill the full
	// episode from hook_events with created_at > sinceTS rather than the
	// 12-event rolling window.
	sinceTS string
}

// evaluateTriggers inspects the session window after a new event lands.
// Returns the list of triggers that should fire NOW. Mutates w.firedKeys
// so callers don't re-fire on the same signature.
func evaluateTriggers(w *sessionWindow, latest HookEvent, errorThreshold int, patternEnabled bool, checkpointEnabled bool, checkpointSalience float64) []pendingTrigger {
	var out []pendingTrigger

	// session_end — adapter signals an explicit boundary.
	if latest.EventType == "session_end" {
		key := "session_end:" + latest.SessionID
		if !w.firedKeys[key] {
			w.firedKeys[key] = true
			out = append(out, pendingTrigger{
				trigger:      TriggerSessionEnd,
				events:       append([]HookEvent(nil), w.events...),
				signatureKey: key,
			})
		}
		return out
	}

	// checkpoint — the real-time "encode this episode" trigger. Accumulate a
	// cheap per-event salience score for the current episode; fire a
	// synthesis of the episode at a user-prompt BOUNDARY once enough has
	// accumulated, or MID-TURN once a long burst crosses a hard cap (3x). The
	// salience gate skips trivial turns (a question + a couple reads); the
	// hard cap keeps a very long single agent turn from going un-captured
	// until it ends. fire() distills the episode from hook_events since the
	// anchor (see sinceTS).
	if checkpointEnabled {
		if latest.EventType == "prompt_received" {
			// A new user prompt ends the previous episode and starts a new
			// one. Checkpoint the just-completed episode if it was substantive.
			if w.checkpointAnchorTS != "" && w.salienceAccum >= checkpointSalience {
				out = append(out, pendingTrigger{
					trigger:      TriggerCheckpoint,
					events:       append([]HookEvent(nil), w.events...),
					signatureKey: "checkpoint:" + w.checkpointAnchorTS,
					sinceTS:      w.checkpointAnchorTS,
				})
			}
			// Reset for the new episode; the prompt itself seeds its salience.
			w.salienceAccum = salience(latest)
			w.checkpointAnchorTS = latest.CreatedAt
		} else {
			if w.checkpointAnchorTS == "" {
				w.checkpointAnchorTS = latest.CreatedAt // start tracking
			}
			w.salienceAccum += salience(latest)
			// Hard cap — checkpoint mid-turn so a long agent turn is captured
			// in real time rather than only at the next user prompt.
			if w.salienceAccum >= 3*checkpointSalience {
				out = append(out, pendingTrigger{
					trigger:      TriggerCheckpoint,
					events:       append([]HookEvent(nil), w.events...),
					signatureKey: "checkpoint:" + w.checkpointAnchorTS + ":" + latest.CreatedAt,
					sinceTS:      w.checkpointAnchorTS,
				})
				w.salienceAccum = 0
				w.checkpointAnchorTS = latest.CreatedAt
			}
		}
	}

	// decision — explicit signal from the agent that something was
	// decided. The plugin emits these with event_type="decision".
	if latest.EventType == "decision" {
		// Use payload hash as signature so re-saving the same decision
		// doesn't multi-fire.
		sig := "decision:" + payloadSignature(latest.Payload)
		if !w.firedKeys[sig] {
			w.firedKeys[sig] = true
			out = append(out, pendingTrigger{
				trigger:      TriggerDecision,
				events:       []HookEvent{latest},
				signatureKey: sig,
			})
		}
	}

	// error — count consecutive same-shape errors at the tail of the
	// window. "Same shape" = errorSignature(payload) matches.
	if isErrorEvent(latest) {
		sig := errorSignature(latest.Payload)
		if sig != "" {
			count := 0
			cluster := []HookEvent{}
			for i := len(w.events) - 1; i >= 0; i-- {
				if isErrorEvent(w.events[i]) && errorSignature(w.events[i].Payload) == sig {
					count++
					cluster = append([]HookEvent{w.events[i]}, cluster...)
					continue
				}
				break
			}
			key := "error:" + sig
			if count >= errorThreshold && !w.firedKeys[key] {
				w.firedKeys[key] = true
				out = append(out, pendingTrigger{
					trigger:      TriggerError,
					events:       cluster,
					signatureKey: key,
				})
			}
		}
	}

	// pattern — the same 3-step tool sequence appears twice in the window.
	// OFF by default (patternEnabled): during normal work this fires
	// constantly on Bash>Read / WebSearch>WebFetch loops and produces
	// vacuous "repeatedly used X" memories (it only sees tool names), so it
	// drowns out the high-value error / session_end memories. Opt back in via
	// hook_synthesis_pattern_enabled.
	if patternEnabled {
		if seq, ok := repeatedToolSequence(w.events, 3); ok {
			key := "pattern:" + seq
			if !w.firedKeys[key] {
				w.firedKeys[key] = true
				out = append(out, pendingTrigger{
					trigger:      TriggerPattern,
					events:       append([]HookEvent(nil), w.events...),
					signatureKey: key,
				})
			}
		}
	}

	return out
}

// fire calls the synthesiser and persists the resulting memory.
func (e *HookSynthesisEngine) fire(ctx context.Context, tr HookSynthesisTrigger, events []HookEvent, sig, sessionKey, sinceTS string) error {
	if e.synth == nil {
		return errors.New("no synthesiser wired")
	}
	// For a session-end summary, distill the FULL session rather than just
	// the rolling window. The engine only buffers the last N events
	// (hook_synthesis_pattern_window, default 12), but a good "what
	// happened this session" memory needs the whole arc. Every event was
	// already persisted to hook_events before Observe ran, so pull them
	// back (newest-first → reverse to chronological).
	if tr == TriggerSessionEnd && e.bank != nil && len(events) > 0 {
		if sid := events[len(events)-1].SessionID; sid != "" {
			if full, ferr := e.bank.ListHookEvents(HookEventListOpts{SessionID: sid, Limit: 200}); ferr == nil && len(full) > len(events) {
				rev := make([]HookEvent, len(full))
				for i := range full {
					rev[len(full)-1-i] = full[i]
				}
				events = rev
			}
		}
	}
	// For a checkpoint, distill the FULL episode (everything since the anchor)
	// rather than the 12-event rolling window — a substantive sub-task can run
	// to dozens of events. Pull session events with created_at > sinceTS.
	if tr == TriggerCheckpoint && e.bank != nil && sinceTS != "" && len(events) > 0 {
		if sid := events[len(events)-1].SessionID; sid != "" {
			if full, ferr := e.bank.ListHookEvents(HookEventListOpts{SessionID: sid, Since: sinceTS, Limit: 200}); ferr == nil && len(full) > 0 {
				rev := make([]HookEvent, len(full))
				for i := range full {
					rev[len(full)-1-i] = full[i]
				}
				events = rev
			}
		}
	}
	res, err := e.synth.Synthesize(ctx, tr, events)
	if err != nil {
		return err
	}
	if strings.TrimSpace(res.Text) == "" {
		return nil // synthesiser passed; nothing to write
	}
	tags := []string{"synthesis", "trigger:" + string(tr)}
	tags = append(tags, res.Tags...)
	mem := MemoryRecord{
		Text:       res.Text,
		Tags:       tags,
		Source:     "hook_auto_synthesis",
		MemoryType: "synthesis",
	}
	if e.bank == nil {
		return errors.New("bank not enabled")
	}
	if _, err := e.bank.SaveMemory(mem); err != nil {
		return fmt.Errorf("save hook synthesis: %w", err)
	}
	return nil
}

// ── Helpers ─────────────────────────────────────────────────────────────

func isErrorEvent(ev HookEvent) bool {
	if ev.EventType == "tool_error" || ev.EventType == "error" {
		return true
	}
	// payload-encoded status:"error" or exit_code != 0 also count.
	if ev.Payload == "" {
		return false
	}
	var p map[string]interface{}
	if err := json.Unmarshal([]byte(ev.Payload), &p); err != nil {
		return false
	}
	if status, ok := p["status"].(string); ok && strings.EqualFold(status, "error") {
		return true
	}
	if ec, ok := p["exit_code"].(float64); ok && ec != 0 {
		return true
	}
	return false
}

// errorSignature normalises an error event into a stable key (tool name +
// first 64 chars of message). Same-shape errors collapse to the same key.
func errorSignature(payloadJSON string) string {
	if payloadJSON == "" {
		return ""
	}
	var p map[string]interface{}
	if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
		return ""
	}
	tool, _ := p["tool"].(string)
	if tool == "" {
		// forward.js PostToolUseFailure shape: where:"tool:Bash" (no bare
		// "tool" key). Without this branch EVERY error collapsed to the
		// signature "|", so unrelated errors clustered and the threshold
		// fired on any 3 errors rather than 3 of the SAME shape.
		if where, ok := p["where"].(string); ok {
			tool = strings.TrimPrefix(where, "tool:")
		}
	}
	msg, _ := p["message"].(string)
	if msg == "" {
		msg, _ = p["error"].(string)
	}
	if msg == "" {
		// forward.js carries the message under error_message.
		msg, _ = p["error_message"].(string)
	}
	// Trim to a short prefix so trivial whitespace / id variance doesn't
	// split the signature.
	if len(msg) > 64 {
		msg = msg[:64]
	}
	return strings.TrimSpace(tool) + "|" + strings.TrimSpace(msg)
}

// repeatedToolSequence scans the window for a length-N sequence of tool
// calls that appears at least twice. Returns the sequence as
// "tool1>tool2>tool3" so it's a stable key.
func repeatedToolSequence(events []HookEvent, n int) (string, bool) {
	if len(events) < 2*n {
		return "", false
	}
	tools := make([]string, len(events))
	for i, ev := range events {
		tools[i] = extractToolName(ev)
	}
	// Walk all windows of length n; record sigs; on a second sighting fire.
	seen := map[string]bool{}
	for i := 0; i+n <= len(events); i++ {
		// Skip windows containing an empty tool (= unparseable event).
		ok := true
		for j := 0; j < n; j++ {
			if tools[i+j] == "" {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		sig := strings.Join(tools[i:i+n], ">")
		if seen[sig] {
			return sig, true
		}
		seen[sig] = true
	}
	return "", false
}

func extractToolName(ev HookEvent) string {
	if ev.Payload == "" {
		return ""
	}
	var p map[string]interface{}
	if err := json.Unmarshal([]byte(ev.Payload), &p); err != nil {
		return ""
	}
	for _, key := range []string{"tool", "tool_name", "name"} {
		if v, ok := p[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func payloadSignature(payloadJSON string) string {
	if len(payloadJSON) > 80 {
		return payloadJSON[:80]
	}
	return payloadJSON
}
