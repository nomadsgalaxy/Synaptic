// bundle_s_guards_test.go — guards for v2.7 Bundle S (auto-synthesis
// from the hook event stream).
//
// The engine observes events the plugin's `forward.js` pushes via
// /event. Four trigger types fire synthesis: error / pattern / decision
// / session_end. Default behaviour is OFF (preserves v2.6).
//
// Coverage:
//   - Default OFF: no synthesis even when an error storm hits
//   - Enabled + 3 consecutive same-shape errors fires a "lesson" memory
//     with memory_type=synthesis and source=hook_auto_synthesis
//   - Enabled + session_end event fires a session summary
//   - Enabled + decision event fires a decision memory
//   - Pattern trigger fires when a 3-tool sequence repeats
//   - Same trigger doesn't fire twice for the same signature (de-dup)
//   - Settings: threshold + window helpers honour overrides
package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubSynth captures invocations + returns deterministic text.
//
// Observe() fires synthesis on a detached goroutine (see hook_synthesis.go —
// the 2026-05-28 change that stopped a slow synth wedging the 5s-timeout hook),
// so Synthesize runs concurrently with the test's assertions. The mutex makes
// the call slice race-free; tests must use waitForCalls (not read calls
// directly) to await the async fire.
type stubSynth struct {
	mu    sync.Mutex
	calls []HookSynthesisTrigger
}

func (s *stubSynth) Synthesize(_ context.Context, tr HookSynthesisTrigger, _ []HookEvent) (HookSynthesisResult, error) {
	s.mu.Lock()
	s.calls = append(s.calls, tr)
	s.mu.Unlock()
	return HookSynthesisResult{
		Text: "auto-synth: " + string(tr),
		Tags: []string{"auto"},
	}, nil
}

// snapshot returns a race-free copy of the recorded trigger calls.
func (s *stubSynth) snapshot() []HookSynthesisTrigger {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]HookSynthesisTrigger(nil), s.calls...)
}

// count returns how many triggers have fired so far (race-free).
func (s *stubSynth) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// waitForCalls blocks until the stub has recorded at least n trigger fires or
// the timeout elapses, then returns the final snapshot. Synthesis is async, so
// an immediate read would see zero; this polls the engine to completion.
func waitForCalls(t *testing.T, s *stubSynth, n int, timeout time.Duration) []HookSynthesisTrigger {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.count() >= n {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	return s.snapshot()
}

// settleNoCalls waits a short grace period and asserts NOTHING fired — used by
// the disabled / below-threshold cases where an async fire would otherwise race
// past a bare immediate check.
func settleNoCalls(t *testing.T, s *stubSynth) []HookSynthesisTrigger {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	return s.snapshot()
}

// helper: feed a hook event through the engine.
func observe(eng *HookSynthesisEngine, evType, sessionID, payload string) {
	eng.Observe(context.Background(), HookEvent{
		ID:        "ev-" + sessionID + "-" + evType,
		AdapterID: "cc",
		SessionID: sessionID,
		EventType: evType,
		Payload:   payload,
	})
}

// ── Setting helpers ────────────────────────────────────────────────────

func TestBundleS_HookSynthesisDefaultsOff(t *testing.T) {
	bank := newTestBank(t)
	if hookSynthesisEnabled(bank) {
		t.Errorf("hook synthesis must default OFF")
	}
	if hookSynthesisEnabled(nil) {
		t.Errorf("nil bank should default OFF")
	}
}

func TestBundleS_HookSynthesisHonoursOnSetting(t *testing.T) {
	bank := newTestBank(t)
	for _, v := range []string{"1", "true", "on", "yes"} {
		bank.SetSetting(HookSynthesisEnabledKey, v)
		if !hookSynthesisEnabled(bank) {
			t.Errorf("value %q should enable", v)
		}
	}
}

func TestBundleS_HookSynthesisThresholdHelper(t *testing.T) {
	bank := newTestBank(t)
	if got := hookSynthesisErrorThreshold(bank); got != 3 {
		t.Errorf("default threshold should be 3, got %d", got)
	}
	bank.SetSetting(HookSynthesisErrorThresholdKey, "5")
	if got := hookSynthesisErrorThreshold(bank); got != 5 {
		t.Errorf("override should win, got %d", got)
	}
	for _, bad := range []string{"0", "1", "9999", "abc"} {
		bank.SetSetting(HookSynthesisErrorThresholdKey, bad)
		if got := hookSynthesisErrorThreshold(bank); got != 3 {
			t.Errorf("bad %q should fall back to 3, got %d", bad, got)
		}
	}
}

// ── Default-OFF gate ────────────────────────────────────────────────────

func TestBundleS_DisabledDoesNotFire(t *testing.T) {
	bank := newTestBank(t)
	synth := &stubSynth{}
	eng := NewHookSynthesisEngine(bank, synth)

	// Pour 5 same-shape errors at it — nothing should fire when disabled.
	payload := `{"tool":"Bash","status":"error","message":"command not found"}`
	for i := 0; i < 5; i++ {
		observe(eng, "tool_call", "s1", payload)
	}
	if calls := settleNoCalls(t, synth); len(calls) != 0 {
		t.Errorf("disabled engine fired %d times, want 0", len(calls))
	}
}

// ── Error trigger ───────────────────────────────────────────────────────

func TestBundleS_ErrorTriggerFires(t *testing.T) {
	bank := newTestBank(t)
	bank.SetSetting(HookSynthesisEnabledKey, "1")
	synth := &stubSynth{}
	eng := NewHookSynthesisEngine(bank, synth)

	payload := `{"tool":"Bash","status":"error","message":"command not found"}`
	// First two should buffer; third (threshold default 3) fires.
	observe(eng, "tool_call", "s1", payload)
	observe(eng, "tool_call", "s1", payload)
	if calls := settleNoCalls(t, synth); len(calls) != 0 {
		t.Errorf("expected 0 calls before threshold, got %d", len(calls))
	}
	observe(eng, "tool_call", "s1", payload)
	// Synthesis fires async — wait for the detached goroutine.
	calls := waitForCalls(t, synth, 1, 2*time.Second)
	if len(calls) != 1 || calls[0] != TriggerError {
		t.Errorf("expected one error trigger, got %v", calls)
	}

	// Verify a memory landed with the right metadata. fire() persists the
	// memory inside the same goroutine after Synthesize returns, so poll.
	var mems []MemoryRecord
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mems, _ = bank.ListMemoriesWith(MemoryListOpts{Limit: 50})
		if len(mems) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	found := false
	for _, m := range mems {
		if m.Source == "hook_auto_synthesis" && m.MemoryType == "synthesis" {
			found = true
			hasTrigger := false
			for _, tag := range m.Tags {
				if tag == "trigger:error" {
					hasTrigger = true
				}
			}
			if !hasTrigger {
				t.Errorf("expected 'trigger:error' tag, got %v", m.Tags)
			}
		}
	}
	if !found {
		t.Errorf("expected a hook_auto_synthesis memory, got %d total", len(mems))
	}
}

func TestBundleS_ErrorTriggerDoesntDoubleFireSameSignature(t *testing.T) {
	bank := newTestBank(t)
	bank.SetSetting(HookSynthesisEnabledKey, "1")
	synth := &stubSynth{}
	eng := NewHookSynthesisEngine(bank, synth)

	payload := `{"tool":"Bash","status":"error","message":"oom"}`
	for i := 0; i < 6; i++ {
		observe(eng, "tool_call", "s1", payload)
	}
	// Even with 6 errors, the same-signature key fires once. Wait for the
	// first async fire, then settle to confirm no further fires arrive.
	waitForCalls(t, synth, 1, 2*time.Second)
	calls := settleNoCalls(t, synth)
	errorCount := 0
	for _, tr := range calls {
		if tr == TriggerError {
			errorCount++
		}
	}
	if errorCount != 1 {
		t.Errorf("expected 1 error fire (dedup), got %d (calls=%v)", errorCount, calls)
	}
}

// ── session_end trigger ─────────────────────────────────────────────────

func TestBundleS_SessionEndFires(t *testing.T) {
	bank := newTestBank(t)
	bank.SetSetting(HookSynthesisEnabledKey, "1")
	synth := &stubSynth{}
	eng := NewHookSynthesisEngine(bank, synth)

	// Pour a few normal events first.
	observe(eng, "tool_call", "s1", `{"tool":"Read"}`)
	observe(eng, "tool_call", "s1", `{"tool":"Edit"}`)
	observe(eng, "session_end", "s1", `{}`)

	calls := waitForCalls(t, synth, 1, 2*time.Second)
	if len(calls) != 1 || calls[0] != TriggerSessionEnd {
		t.Errorf("expected session_end trigger, got %v", calls)
	}
}

// ── decision trigger ───────────────────────────────────────────────────

func TestBundleS_DecisionFires(t *testing.T) {
	bank := newTestBank(t)
	bank.SetSetting(HookSynthesisEnabledKey, "1")
	synth := &stubSynth{}
	eng := NewHookSynthesisEngine(bank, synth)

	observe(eng, "decision", "s1", `{"text":"switch to nomic-embed for v2.7"}`)
	calls := waitForCalls(t, synth, 1, 2*time.Second)
	if len(calls) != 1 || calls[0] != TriggerDecision {
		t.Errorf("expected decision trigger, got %v", calls)
	}
}

// ── pattern trigger ─────────────────────────────────────────────────────

func TestBundleS_PatternTriggerFires(t *testing.T) {
	bank := newTestBank(t)
	bank.SetSetting(HookSynthesisEnabledKey, "1")
	// The pattern trigger is OFF by default (it fires constantly on Bash>Read
	// loops and produces vacuous "repeatedly used X" memories — see
	// hookSynthesisPatternEnabled). This test specifically exercises it, so
	// opt in.
	bank.SetSetting(HookSynthesisPatternEnabledKey, "1")
	synth := &stubSynth{}
	eng := NewHookSynthesisEngine(bank, synth)

	// Repeat the tool sequence read→grep→edit twice in a row.
	seq := []string{"Read", "Grep", "Edit"}
	for repeat := 0; repeat < 2; repeat++ {
		for _, tool := range seq {
			observe(eng, "tool_call", "s1", `{"tool":"`+tool+`"}`)
		}
	}
	// Synthesis fires async — wait for the pattern fire, then settle so any
	// extra fires are counted too.
	waitForCalls(t, synth, 1, 2*time.Second)
	calls := settleNoCalls(t, synth)
	patternCount := 0
	for _, tr := range calls {
		if tr == TriggerPattern {
			patternCount++
		}
	}
	if patternCount < 1 {
		t.Errorf("expected at least one pattern trigger after sequence repeat, got %v", calls)
	}
}

// ── Per-session isolation ──────────────────────────────────────────────

func TestBundleS_SessionsAreIsolated(t *testing.T) {
	// Errors from session A and B should be tracked separately — 3 errors
	// in A fires, 3 in B fires, but neither's count should leak into the
	// other.
	bank := newTestBank(t)
	bank.SetSetting(HookSynthesisEnabledKey, "1")
	synth := &stubSynth{}
	eng := NewHookSynthesisEngine(bank, synth)

	payload := `{"tool":"Bash","status":"error","message":"x"}`
	// Two errors in each — neither should fire (threshold is 3).
	observe(eng, "tool_call", "A", payload)
	observe(eng, "tool_call", "A", payload)
	observe(eng, "tool_call", "B", payload)
	observe(eng, "tool_call", "B", payload)
	if calls := settleNoCalls(t, synth); len(calls) != 0 {
		t.Errorf("2 errors per session shouldn't fire (threshold 3), got %v", calls)
	}

	// One more in A fires; B stays quiet. Wait for the async fire, then settle.
	observe(eng, "tool_call", "A", payload)
	waitForCalls(t, synth, 1, 2*time.Second)
	calls := settleNoCalls(t, synth)
	errorCount := 0
	for _, tr := range calls {
		if tr == TriggerError {
			errorCount++
		}
	}
	if errorCount != 1 {
		t.Errorf("expected 1 error fire after A hits 3, got %d (calls=%v)", errorCount, calls)
	}
}

// ── Helper-level coverage ──────────────────────────────────────────────

func TestBundleS_IsErrorEvent(t *testing.T) {
	cases := []struct {
		desc    string
		ev      HookEvent
		wantErr bool
	}{
		{"explicit tool_error type", HookEvent{EventType: "tool_error"}, true},
		{"status:error payload", HookEvent{EventType: "tool_call", Payload: `{"status":"error"}`}, true},
		{"non-zero exit_code", HookEvent{EventType: "tool_call", Payload: `{"exit_code":1}`}, true},
		{"normal tool call", HookEvent{EventType: "tool_call", Payload: `{"tool":"Read"}`}, false},
		{"empty payload", HookEvent{EventType: "tool_call"}, false},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			if got := isErrorEvent(c.ev); got != c.wantErr {
				t.Errorf("isErrorEvent(%+v): got %v, want %v", c.ev, got, c.wantErr)
			}
		})
	}
}

func TestBundleS_ErrorSignatureGroupsSameShapeErrors(t *testing.T) {
	p1 := `{"tool":"Bash","message":"command not found: xyz"}`
	p2 := `{"tool":"Bash","message":"command not found: abc"}` // different tail, same head
	p3 := `{"tool":"Read","message":"file not found"}`
	if errorSignature(p1) == "" {
		t.Errorf("signature should be non-empty for %s", p1)
	}
	if errorSignature(p1) == errorSignature(p3) {
		t.Errorf("different tool/msg should NOT collide")
	}
	// Same tool, same first 64 chars of message → same sig.
	if !strings.HasPrefix(errorSignature(p1), "Bash|command not found:") {
		t.Errorf("expected Bash|command not found:..., got %q", errorSignature(p1))
	}
	if !strings.HasPrefix(errorSignature(p2), "Bash|command not found:") {
		t.Errorf("expected Bash|command not found:..., got %q", errorSignature(p2))
	}
}
