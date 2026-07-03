// hook_synthesizer_llm.go — concrete HookSynthesizer (v2.7 Bundle S).
//
// This is the "brain" the HookSynthesisEngine calls when a trigger fires.
// It turns a cluster of hook events (errors, recurring tool sequences,
// decisions, or a whole session) into one durable memory using the Tier 1
// (realtime) provider. Without it the engine has the machinery to detect
// triggers but no way to condense events into prose — so hook
// auto-synthesis is inert. (Pre-2026-05-28 the engine was never even
// constructed AND had no synthesizer wired; this file + the main.go
// wiring close that gap.)
//
// Tier choice: ForRealtime() (Tier 1). Synthesis is a cheap summarization
// task that should run fast and NOT contend with the nightly Tier 2
// pipeline. When Tier 1 is unconfigured the synthesizer returns an empty
// result (no error) so the engine simply writes nothing.
//
// Privacy: the rendered events include the first ~200 chars of user
// prompts and up to ~300 chars of assistant turns (the same text the
// dashboard already receives via response_complete). The synthesized
// memory is written through Bank.SaveMemory, so it passes the normal
// sensitive-classifier + dedup path like any other memory.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// LLMHookSynthesizer implements HookSynthesizer via a Tier 1 chat call.
type LLMHookSynthesizer struct {
	router *ModelRouter
	bank   *Bank // read live for the hook_synthesis_rich_capture gate
}

// NewLLMHookSynthesizer constructs the synthesizer. router/bank may be nil
// (Synthesize then no-ops / treats rich-capture as off).
func NewLLMHookSynthesizer(router *ModelRouter, bank *Bank) *LLMHookSynthesizer {
	return &LLMHookSynthesizer{router: router, bank: bank}
}

// Synthesize condenses an event cluster into a memory. Returns an empty
// result (no error) when there is nothing worth saving or Tier 1 is
// unconfigured — the engine treats empty Text as "skip, don't persist".
func (s *LLMHookSynthesizer) Synthesize(ctx context.Context, trigger HookSynthesisTrigger, events []HookEvent) (HookSynthesisResult, error) {
	if s == nil || s.router == nil || len(events) == 0 {
		return HookSynthesisResult{}, nil
	}
	provider := s.router.ForRealtime() // Tier 1
	if provider == nil {
		return HookSynthesisResult{}, nil // Tier 1 not configured — no-op
	}
	// Rich-capture gate read LIVE so the dashboard toggle takes effect on
	// the next synthesis without a restart. Off → tool names only.
	prompt := buildHookSynthesisPrompt(trigger, events, hookSynthesisRichCapture(s.bank))
	if prompt == "" {
		return HookSynthesisResult{}, nil
	}
	// Bound the call. The engine already fires us in a detached goroutine,
	// but a per-call timeout keeps cost + latency predictable and means a
	// wedged Tier 1 can't leak a goroutine indefinitely.
	cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	out, err := provider.Chat(cctx, []Message{{Role: "user", Content: prompt}}, 400)
	if err != nil {
		return HookSynthesisResult{}, fmt.Errorf("hook synth chat: %w", err)
	}
	return parseHookSynthesisOutput(out), nil
}

// buildHookSynthesisPrompt frames the per-trigger ask and renders the
// event cluster. The LLM is told to return JSON {text, tags}, and to skip
// (empty text) when there's nothing durable to capture — so trivial
// clusters don't spam the bank.
func buildHookSynthesisPrompt(trigger HookSynthesisTrigger, events []HookEvent, rich bool) string {
	var b strings.Builder
	switch trigger {
	case TriggerError:
		b.WriteString(`A tool error repeated several times in this coding session. Write ONE durable "lesson learned" memory (1-3 sentences): what was being attempted, the error, and the most likely cause or fix. Be specific and reusable so a future agent avoids the same mistake. Return empty text if the errors are transient or unactionable.`)
	case TriggerPattern:
		b.WriteString(`The same short sequence of tool calls recurred in this coding session. Write ONE concise "workflow pattern" memory (1-2 sentences) naming the recurring procedure and what it accomplishes. Return empty text if it is trivial (e.g. plain read-then-edit).`)
	case TriggerDecision:
		b.WriteString(`An explicit decision was logged. Capture it as ONE durable memory in the form "Decision: <what> = <chosen> because <why>". Keep it factual.`)
	case TriggerSessionEnd:
		b.WriteString(`Summarize this coding session into ONE durable memory (2-4 sentences): what was worked on, key decisions made, what broke and how it was resolved, and any state a future session would need to continue. Factual and concise — no filler, no praise. Return empty text only if literally nothing of substance happened.`)
	case TriggerCheckpoint:
		b.WriteString(`These events are one substantive sub-task that just completed mid-session. Write ONE durable "progress" memory (1-3 sentences) capturing what was actually accomplished: what was changed/built/fixed/decided, with specific names where shown (files, commands, components). Factual and concise — no filler, no "the user asked". Return empty text if nothing durable happened (pure exploration with no outcome).`)
	default:
		b.WriteString(`Condense these events into ONE concise, durable memory. Return empty text if nothing is worth saving.`)
	}
	b.WriteString("\n\nRespond ONLY as JSON: {\"text\": \"<the memory, or empty string to skip>\", \"tags\": [\"2-5 short lowercase topical tags\"]}\n\nEVENTS (chronological):\n")
	// Render the cluster compactly. Cap to the last 40 events so a long
	// session can't blow the token budget; recent events carry the most
	// summary-relevant signal.
	n := len(events)
	start := 0
	if n > 40 {
		start = n - 40
	}
	wrote := 0
	for i := start; i < n; i++ {
		line := renderHookEvent(events[i], rich)
		if line == "" {
			continue
		}
		b.WriteString("  - ")
		b.WriteString(line)
		b.WriteString("\n")
		wrote++
	}
	if wrote == 0 {
		return "" // nothing renderable — skip the call entirely
	}
	return b.String()
}

// renderHookEvent collapses one event into a single compact line for the
// synthesis prompt. Extracts the conversational text the plugin already
// forwards (user prompt prefix, assistant turn) plus tool/error structure.
func renderHookEvent(ev HookEvent, rich bool) string {
	var p map[string]interface{}
	if ev.Payload != "" {
		_ = json.Unmarshal([]byte(ev.Payload), &p)
	}
	gs := func(k string) string {
		if p == nil {
			return ""
		}
		if v, ok := p[k].(string); ok {
			return v
		}
		return ""
	}
	switch ev.EventType {
	case "prompt_received":
		if t := gs("text"); t != "" {
			return "USER: " + hookTrunc(t, 200)
		}
		return "USER prompt"
	case "tool_call":
		// arg_summary (the file/command/query/URL) is already redacted +
		// clipped plugin-side. Included ONLY when rich capture is on — this
		// is what turns "used Bash" into "ran docker compose build core".
		s := "tool_call: " + gs("tool_name")
		if rich {
			if a := gs("arg_summary"); a != "" {
				s += " — " + a
			}
		}
		return s
	case "tool_result":
		s := "tool_ok: " + gs("tool_name")
		if rich {
			if r := gs("result_snippet"); r != "" {
				s += " → " + r
			}
		}
		return s
	case "error":
		return "ERROR " + gs("where") + " :: " + hookTrunc(gs("error_message"), 140)
	case "response_complete":
		if t := gs("text"); t != "" {
			return "ASSISTANT: " + hookTrunc(t, 300)
		}
		return "ASSISTANT response"
	case "memory_added":
		return "memory saved"
	case "memory_recall":
		return "memory recall"
	case "session_start":
		return "session start"
	case "session_end":
		if r := gs("reason"); r != "" {
			return "session end (" + r + ")"
		}
		return "session end"
	default:
		return ev.EventType
	}
}

// parseHookSynthesisOutput accepts the LLM's JSON {text, tags}, tolerating
// code fences and prose wrappers. Falls back to treating the whole output
// as the memory text when JSON parsing fails.
func parseHookSynthesisOutput(out string) HookSynthesisResult {
	out = strings.TrimSpace(out)
	if out == "" {
		return HookSynthesisResult{}
	}
	out = hookStripFences(out)
	var parsed struct {
		Text string   `json:"text"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err == nil {
		return HookSynthesisResult{Text: strings.TrimSpace(parsed.Text), Tags: hookCleanTags(parsed.Tags)}
	}
	// Forgiving fallback: maybe the model wrapped JSON in prose. Try to
	// extract the first {...} block.
	if i := strings.IndexByte(out, '{'); i >= 0 {
		if j := strings.LastIndexByte(out, '}'); j > i {
			if err := json.Unmarshal([]byte(out[i:j+1]), &parsed); err == nil {
				return HookSynthesisResult{Text: strings.TrimSpace(parsed.Text), Tags: hookCleanTags(parsed.Tags)}
			}
		}
	}
	// Last resort: the raw text is the memory.
	return HookSynthesisResult{Text: out}
}

// hookTrunc shortens s to at most n runes, appending an ellipsis when cut.
func hookTrunc(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// hookStripFences removes a leading ```json / ``` fence and trailing ```.
func hookStripFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if nl := strings.IndexByte(s, '\n'); nl >= 0 {
			s = s[nl+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	return strings.TrimSpace(s)
}

// hookCleanTags normalises LLM-suggested tags: lowercase, trimmed,
// de-duplicated, capped at 5, dropping empties and anything too long.
func hookCleanTags(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, t := range in {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || len(t) > 40 || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if len(out) >= 5 {
			break
		}
	}
	return out
}
