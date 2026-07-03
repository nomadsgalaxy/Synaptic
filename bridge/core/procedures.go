// procedures.go — v2.7 Bundle O: procedural memories.
//
// A "procedure" is a memory the agent saves to teach future runs
// something about how to behave — "always quote file paths with
// spaces", "prefer pnpm over npm in this repo", "Slack messages over
// 800 chars need a thread". They're stored as ordinary memories with
// memory_type="procedure" and an optional "scope" tag that lets a
// caller pull a subset (e.g., only procedures tagged "scope:auth").
//
// At inference time, callers that build a system prompt can ask the
// bank to materialise the active procedures as a prefix. The
// reflection synthesiser (Bundle R) is the first wired consumer;
// other code paths can opt in via BuildSystemPromptWithProcedures().
//
// Procedures inherit the rest of the memory machinery: they go through
// SaveMemory (so the sensitive-flag scan applies, dirty bit is set,
// and so on), they can be put dormant via sd_set_dormant, and they
// surface in /recall by default. The only special handling is the
// prompt-prefix construction below.
package main

import (
	"errors"
	"strings"
)

// ProcedureMemoryType is the canonical value for the memory_type
// column. Saving a memory with this type marks it as a procedure;
// ListActiveProcedures filters on it.
const ProcedureMemoryType = "procedure"

// ListActiveProcedures returns up to `limit` non-deleted, non-dormant
// procedural memories, newest first. `scope` is an optional filter:
// when non-empty, only procedures whose tag set contains the literal
// token (matched case-insensitively) are returned. Empty scope = all.
func (b *Bank) ListActiveProcedures(scope string, limit int) ([]MemoryRecord, error) {
	if b == nil {
		return nil, errors.New("bank not enabled")
	}
	if limit <= 0 {
		limit = 32
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := b.db.Query(`
SELECT `+memoryColumns+`
FROM memories
WHERE memory_type = ?
  AND deleted_at = ''
  AND dormant_at = ''
ORDER BY updated_at DESC
LIMIT ?`, ProcedureMemoryType, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	scopeLower := strings.ToLower(strings.TrimSpace(scope))
	out := []MemoryRecord{}
	for rows.Next() {
		rec, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		if scopeLower != "" {
			if !memoryMatchesScope(rec, scopeLower) {
				continue
			}
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// memoryMatchesScope returns true when the memory's tag set contains
// the scope token (case-insensitively). Tag-level scoping rather than
// region_hint scoping so a single procedure can apply across regions.
func memoryMatchesScope(rec MemoryRecord, scopeLower string) bool {
	for _, t := range rec.Tags {
		if strings.ToLower(strings.TrimSpace(t)) == scopeLower {
			return true
		}
	}
	return false
}

// ProcedurePromptPrefix renders the active procedures as a single
// system-prompt fragment. Returns an empty string when no procedures
// apply — callers can unconditionally prepend the result. Each
// procedure is rendered as a bullet so the model sees a clean checklist.
//
// Example output (scope="" with two stored procedures):
//
//   Active procedures (apply to every reply unless explicitly overridden):
//   - prefer pnpm over npm in this repo
//   - quote file paths that contain spaces
//
// Cap on emitted bytes keeps a runaway procedure store from drowning
// out the actual user prompt. The cap is the lesser of `byteCap` (when
// >0) or 4 KB.
func (b *Bank) ProcedurePromptPrefix(scope string, byteCap int) (string, error) {
	procs, err := b.ListActiveProcedures(scope, 32)
	if err != nil {
		return "", err
	}
	if len(procs) == 0 {
		return "", nil
	}
	cap := byteCap
	if cap <= 0 || cap > 4096 {
		cap = 4096
	}
	var b2 strings.Builder
	b2.WriteString("Active procedures (apply to every reply unless explicitly overridden):\n")
	for _, p := range procs {
		// Prefer enriched_text when set — that's the cleaned-up version
		// the dream pipeline wrote. Falls back to raw text.
		body := p.EnrichedText
		if strings.TrimSpace(body) == "" {
			body = p.Text
		}
		body = strings.TrimSpace(body)
		if body == "" {
			continue
		}
		// One procedure per line, trimmed to a single line.
		body = strings.ReplaceAll(body, "\n", " ")
		line := "- " + body + "\n"
		if b2.Len()+len(line) > cap {
			// Stop before exceeding the cap; the remaining procedures are
			// dropped silently. Future work: rank by salience and keep the
			// top N when truncated. For now FIFO-newest-first.
			break
		}
		b2.WriteString(line)
	}
	out := b2.String()
	// If only the heading made it in (no body lines fit), return empty.
	if strings.Count(out, "\n") < 2 {
		return "", nil
	}
	return out + "\n", nil
}

// BuildSystemPromptWithProcedures prepends the procedure prefix to the
// caller's base system prompt. Cheap (no-op when no procedures). Use at
// any LLM call site where the user might want their procedures to
// govern model behaviour.
func BuildSystemPromptWithProcedures(b *Bank, scope, basePrompt string) string {
	if b == nil {
		return basePrompt
	}
	prefix, err := b.ProcedurePromptPrefix(scope, 0)
	if err != nil || prefix == "" {
		return basePrompt
	}
	return prefix + basePrompt
}
