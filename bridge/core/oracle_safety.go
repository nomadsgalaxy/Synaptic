// oracle_safety.go — HARD BARRIER between local memory and the public internet.
//
// ─────────────────────────────────────────────────────────────────────────
//
//  CRITICAL READ-FIRST FOR ANYONE TOUCHING TIER 3 / RESEARCH / ORACLE CODE:
//
//  Memories flagged `sensitive=true` are excluded from Tier 3 by default —
//  this is the standing privacy rule. v2.4.0b1 refined the rule to allow a
//  per-feature, per-call OPT-IN: callers may set OracleRequest.IncludeSensitive
//  = true to forward sensitive memories. When they do, they MUST surface
//  `included_sensitive_count` in their response AND record `included_sensitive:
//  true` in their audit log. Silent inclusion is forbidden.
//
//  LocalConcept filtering (SD_LOCAL_CONCEPTS — project codenames, internal
//  identifiers) is NOT opt-out-able. That filter always runs.
//
//  ENFORCEMENT MODEL:
//
//    1. The `sensitive` column on `memories` is the source of truth.
//       Auto-set when any tag matches SD_SENSITIVE_TAGS; sticky-on
//       (UPSERT uses MAX semantics — see SaveMemory).
//
//    2. The ONLY sanctioned way to obtain a Tier 3 LLMProvider plus a
//       payload to send is `ModelRouter.PrepareOracleCall`. It returns
//       a vetted `OracleSafeContent` whose `Memories` slice is filtered
//       per the rules above, whose `Prompt` has every SD_LOCAL_CONCEPTS
//       term redacted, and whose Topic has been checked against
//       SD_LOCAL_CONCEPTS.
//
//    3. If you find yourself accessing `router.Tier3` directly, STOP.
//       Use PrepareOracleCall. Direct field access exists only because
//       the field has to be assignable at construction time.
//
//    4. SD_DISABLE_ORACLE=1 is a global kill switch. PrepareOracleCall
//       returns ErrOracleDisabled regardless of how Tier 3 was wired.
//
//    5. Any new Tier 3 caller MUST write an audit_log entry of
//       operation="oracle_call" with after_json containing the
//       sanitized payload that was actually sent. When IncludeSensitive
//       was used, the audit entry's after_json MUST also carry
//       included_sensitive=true and sensitive_included=<count>.
//
//  Defense in depth: the package-level helper `assertNoSensitive` is
//  cheap; call it from any new outbound code path as belt-and-braces
//  ONLY on the default-path (non-opt-in) side.
//
// ─────────────────────────────────────────────────────────────────────────
package main

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Errors returned by PrepareOracleCall. Callers should branch on these
// (errors.Is) rather than string-match, so the user sees the right reason.
var (
	ErrOracleDisabled    = errors.New("oracle disabled (SD_DISABLE_ORACLE=1 or Tier 3 unconfigured)")
	ErrOracleTopicLocal  = errors.New("topic is in SD_LOCAL_CONCEPTS — local-only, never routed to Tier 3")
	ErrOracleAllRedacted = errors.New("all candidate memories were sensitive — nothing safe to send")
)

// defaultSensitiveTags is the seed list used when SD_SENSITIVE_TAGS is unset.
// Auto-flag is *additive only*: adding to this list will retroactively flag
// new writes, but old records keep whatever value they have until manually
// PATCHed. (We deliberately do NOT scan the whole table on startup; that
// would silently re-classify and could surprise users.)
var defaultSensitiveTags = []string{
	"private", "secret", "personal", "confidential", "sensitive", "nsfw",
	"password", "credential", "credentials", "api_key", "apikey", "token",
	"auth_token", "access_token", "private_key", "ssn", "medical", "health",
	"finance", "financial", "tax", "diary", "journal_private",
}

// sensitiveTagSet returns the runtime sensitive-tag set: SD_SENSITIVE_TAGS
// (comma-separated) if set, else defaultSensitiveTags. Tags are matched
// case-insensitively after trimming.
func sensitiveTagSet() map[string]bool {
	raw := os.Getenv("SD_SENSITIVE_TAGS")
	out := map[string]bool{}
	if raw == "" {
		for _, t := range defaultSensitiveTags {
			out[strings.ToLower(strings.TrimSpace(t))] = true
		}
		return out
	}
	for _, t := range strings.Split(raw, ",") {
		t = strings.ToLower(strings.TrimSpace(t))
		if t != "" {
			out[t] = true
		}
	}
	return out
}

// tagsContainSensitive reports whether any tag in `tags` matches the runtime
// sensitive-tag set. Used by SaveMemory and UpdateMemory to auto-promote.
func tagsContainSensitive(tags []string) bool {
	if len(tags) == 0 {
		return false
	}
	set := sensitiveTagSet()
	for _, t := range tags {
		if set[strings.ToLower(strings.TrimSpace(t))] {
			return true
		}
	}
	return false
}

// oracleDisabled checks the global kill switch.
func oracleDisabled() bool {
	v := strings.TrimSpace(os.Getenv("SD_DISABLE_ORACLE"))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}

// ─────────────────────────────────────────────────────────────────────────
// PrepareOracleCall
// ─────────────────────────────────────────────────────────────────────────

// OracleRequest is what callers MUST hand to ModelRouter.PrepareOracleCall
// before sending anything to Tier 3. The router decides what (if anything)
// is safe to forward.
type OracleRequest struct {
	// Topic is the search/research subject. If it appears verbatim in
	// SD_LOCAL_CONCEPTS, the entire request is rejected with
	// ErrOracleTopicLocal — no tier 3 call is made.
	Topic string
	// Memories are the candidate memory records that may be included as
	// context. The router filters out any record where Sensitive=true
	// UNLESS IncludeSensitive=true (per-call user opt-in, see below).
	// It always filters records whose tags overlap with SD_LOCAL_CONCEPTS;
	// LocalConcept filtering is a hard egress rule (project codename leak
	// prevention) that is NOT opt-out-able — it's not a privacy preference.
	Memories []MemoryRecord
	// Prompt is freeform extra prompt content (e.g., system instructions,
	// user-typed query). LocalConcepts terms are redacted before forwarding.
	Prompt string
	// Reason is forwarded to audit_log. Required.
	Reason string
	// IncludeSensitive (default false) implements the standing per-feature
	// opt-out rule. When true, sensitive memories ARE forwarded to Tier 3.
	// This MUST be set explicitly by the calling handler in response to a
	// user-supplied request body field or Config setting — never inferred,
	// never defaulted on. Callers that set this MUST surface
	// `included_sensitive_count` in their response and audit-log entry.
	// LocalConcept filtering still runs regardless.
	IncludeSensitive bool
}

// OracleSafeContent is the vetted result of PrepareOracleCall. The provider
// inside is the ONLY Tier 3 handle the caller should use, and it must only
// be called with the Memories and Prompt fields here — anything outside
// this struct may contain sensitive content.
type OracleSafeContent struct {
	Provider LLMProvider
	// Topic is the original topic with LocalConcepts redacted (defensive).
	Topic string
	// Prompt has had every SD_LOCAL_CONCEPTS term replaced with [REDACTED:term].
	Prompt string
	// Memories are guaranteed: no tag in SD_LOCAL_CONCEPTS, no text containing
	// a LocalConcept. Sensitive memories are present ONLY when the caller set
	// OracleRequest.IncludeSensitive=true (per-call user opt-in); otherwise
	// sensitive memories are excluded.
	Memories []MemoryRecord
	// SensitiveExcluded is the count of memories filtered for being sensitive.
	// Zero when IncludeSensitive=true was passed.
	SensitiveExcluded int
	// SensitiveIncluded is the count of sensitive memories that DID reach
	// Memories because the caller opted in. Always zero on the default path.
	// Callers MUST surface this in their response (`included_sensitive_count`)
	// and in their audit log so the user can see exactly what their opt-in
	// caused to leave the local boundary.
	SensitiveIncluded int
	// LocalConceptExcluded is the count filtered for tag-overlap or text-overlap
	// with LocalConcepts. Always enforced — not opt-out-able.
	LocalConceptExcluded int
	// Audit is a pre-built audit_log AFTER-JSON of exactly what's about to be
	// sent. Append it via Bank.AppendAudit before invoking Provider.Chat —
	// gives the user provenance even if Tier 3 errors mid-call.
	Audit AuditEntry
}

// PrepareOracleCall is the SINGLE sanctioned path to Tier 3. It validates
// the request against every guard (kill switch, topic block, sensitive-flag
// filter, local-concept tag filter, prompt redaction) and returns a
// content payload that is safe to forward.
//
// Returns:
//   - ErrOracleDisabled   when SD_DISABLE_ORACLE=1 or Tier 3 unconfigured.
//   - ErrOracleTopicLocal when req.Topic is in LocalConcepts.
//   - ErrOracleAllRedacted when EVERY candidate memory was filtered out and
//     the prompt is empty. (If you genuinely want Tier 3 to think about
//     a topic with no memory context, pass a non-empty Prompt and the call
//     proceeds.)
func (r *ModelRouter) PrepareOracleCall(req OracleRequest) (OracleSafeContent, error) {
	tier3 := r.Tier3()
	if tier3 == nil || oracleDisabled() {
		return OracleSafeContent{}, ErrOracleDisabled
	}
	if r.LocalConcepts[req.Topic] {
		return OracleSafeContent{}, fmt.Errorf("%w: topic=%q", ErrOracleTopicLocal, req.Topic)
	}

	safe := make([]MemoryRecord, 0, len(req.Memories))
	sensitiveSkip := 0
	sensitiveIncluded := 0
	localSkip := 0
	for _, m := range req.Memories {
		// LocalConcept filter — ALWAYS runs, never opt-out-able. This is the
		// hard egress rule (project codename / internal identifier leak
		// prevention), not a privacy preference.
		if tagsHaveLocalConcept(m.Tags, r.LocalConcepts) {
			localSkip++
			continue
		}
		// Belt-and-braces: never include text that matches a LocalConcept
		// even if the tag list is clean. We don't redact the body — we
		// drop the whole record, because partial-redaction of memory text
		// is too easy to bypass and creates false confidence.
		if textContainsLocalConcept(m.Text, r.LocalConcepts) ||
			textContainsLocalConcept(m.EnrichedText, r.LocalConcepts) {
			localSkip++
			continue
		}
		// Sensitive filter — opt-out-able per-call via IncludeSensitive.
		// When the caller hasn't opted in, drop the row. When they have,
		// keep it but COUNT it so the caller surfaces the disclosure in
		// their response and audit log.
		if m.Sensitive {
			if !req.IncludeSensitive {
				sensitiveSkip++
				continue
			}
			sensitiveIncluded++
		}
		safe = append(safe, m)
	}

	if len(safe) == 0 && strings.TrimSpace(req.Prompt) == "" {
		return OracleSafeContent{
			SensitiveExcluded:    sensitiveSkip,
			SensitiveIncluded:    sensitiveIncluded,
			LocalConceptExcluded: localSkip,
		}, ErrOracleAllRedacted
	}

	redactedPrompt := redactLocalConcepts(req.Prompt, r.LocalConcepts)
	redactedTopic := redactLocalConcepts(req.Topic, r.LocalConcepts)

	out := OracleSafeContent{
		Provider:             tier3,
		Topic:                redactedTopic,
		Prompt:               redactedPrompt,
		Memories:             safe,
		SensitiveExcluded:    sensitiveSkip,
		SensitiveIncluded:    sensitiveIncluded,
		LocalConceptExcluded: localSkip,
	}
	out.Audit = AuditEntry{
		Operation:  "oracle_call",
		EntityType: "research",
		EntityID:   redactedTopic,
		Reason:     req.Reason,
		AfterJSON: mustOracleAuditJSON(struct {
			Provider             string   `json:"provider"`
			Topic                string   `json:"topic"`
			Prompt               string   `json:"prompt"`
			MemoryIDsSent        []string `json:"memory_ids_sent"`
			SensitiveExcluded    int      `json:"sensitive_excluded"`
			SensitiveIncluded    int      `json:"sensitive_included"`
			IncludedSensitive    bool     `json:"included_sensitive"`
			LocalConceptExcluded int      `json:"local_concept_excluded"`
		}{
			Provider:             tier3.Name(),
			Topic:                redactedTopic,
			Prompt:               redactedPrompt,
			MemoryIDsSent:        memoryIDs(safe),
			SensitiveExcluded:    sensitiveSkip,
			SensitiveIncluded:    sensitiveIncluded,
			IncludedSensitive:    req.IncludeSensitive,
			LocalConceptExcluded: localSkip,
		}),
	}
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────────
// Helpers (unexported — these are not part of the public API)
// ─────────────────────────────────────────────────────────────────────────

func memoryIDs(ms []MemoryRecord) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

// tagsHaveLocalConcept reports whether any tag is in concepts. Comparison is
// case-sensitive because SD_LOCAL_CONCEPTS is user-curated and codenames may
// be case-significant ("Gravity" vs "gravity").
func tagsHaveLocalConcept(tags []string, concepts map[string]bool) bool {
	for _, t := range tags {
		if concepts[t] {
			return true
		}
	}
	return false
}

// textContainsLocalConcept reports whether any concept appears as a substring
// of text. Case-sensitive (project codenames preserve case).
func textContainsLocalConcept(text string, concepts map[string]bool) bool {
	if text == "" || len(concepts) == 0 {
		return false
	}
	// Iterate concepts in deterministic order so this function is stable
	// across runs (helps test determinism).
	keys := make([]string, 0, len(concepts))
	for c := range concepts {
		keys = append(keys, c)
	}
	sort.Strings(keys)
	for _, c := range keys {
		if c == "" {
			continue
		}
		if strings.Contains(text, c) {
			return true
		}
	}
	return false
}

// redactLocalConcepts replaces every concept occurrence in text with
// "[REDACTED:concept]". Empty concept set returns the input unchanged.
//
// Implementation: single-pass scan. At each position, the longest matching
// concept wins. Once a concept is emitted as "[REDACTED:concept]", scanning
// resumes AFTER the replacement, so we never recurse into our own marker
// (e.g., "GravityCAD" → "[REDACTED:GravityCAD]" without "Gravity" being
// re-redacted inside the marker).
func redactLocalConcepts(text string, concepts map[string]bool) string {
	if text == "" || len(concepts) == 0 {
		return text
	}
	keys := make([]string, 0, len(concepts))
	for c := range concepts {
		if c != "" {
			keys = append(keys, c)
		}
	}
	// Longest first so "GravityCAD" wins over "Gravity" at a shared start.
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })

	var out strings.Builder
	out.Grow(len(text))
	i := 0
	for i < len(text) {
		matched := ""
		for _, c := range keys {
			if i+len(c) <= len(text) && text[i:i+len(c)] == c {
				matched = c
				break // longest-first wins
			}
		}
		if matched != "" {
			out.WriteString("[REDACTED:")
			out.WriteString(matched)
			out.WriteString("]")
			i += len(matched)
			continue
		}
		out.WriteByte(text[i])
		i++
	}
	return out.String()
}

// assertNoSensitive is a defense-in-depth helper. Panic-on-violation by
// design: if reached, it indicates a calling-code bug (the caller bypassed
// PrepareOracleCall). Use only inside new Tier 3 send paths to detect
// regressions during development; remove or quiet for production builds
// once the path is well tested.
func assertNoSensitive(memories []MemoryRecord) error {
	for _, m := range memories {
		if m.Sensitive {
			return fmt.Errorf("INVARIANT VIOLATION: sensitive memory %s reached oracle send path", m.ID)
		}
	}
	return nil
}

// mustOracleAuditJSON marshals a struct to JSON for audit_log.after_json.
// Defined locally so this file does not need to import encoding/json
// directly through unrelated callers.
func mustOracleAuditJSON(v interface{}) string {
	return mustJSON(v) // mustJSON lives in handlers_p5.go
}
