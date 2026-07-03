// sensitive_classifier.go — optional Tier 1/2 LLM sensitivity classifier
// (the SECOND pass after Tier 0's deterministic regex scan).
//
// LOCAL-ONLY: this classifier only ever uses the local Ollama Tier 1
// (llama3.2:3b) or Tier 2 (llama3.1:8b) providers — never the Tier 3 Oracle —
// so memory text is never sent off-box to be classified.
//
// Runs as a background goroutine started by main(). On a tick (default 30s),
// scans up to N bank memories where the AI scan has not yet run (or where
// the record was edited after the last scan), and asks the local LLM whether
// the content contains personal data, credentials, or other sensitive info.
// Positive classifications raise the sensitive flag (sticky-on).
//
// Tier 0 (sensitive_scan.go) catches the high-confidence regex/checksum
// patterns synchronously at write time. This LLM pass catches the prose cases
// regex misses: "spoke to Dr. Chen about my anxiety meds", "the prod password
// Steve gave me yesterday", etc.
//
// OPTIONAL: the LLM pass can be toggled off live via the
// `sensitive_llm_classifier_enabled` setting (default ON). When off, Tier 0
// alone runs. Defense in depth — even with this classifier disabled (toggle
// off, Ollama down, or SD_SENSITIVE_AI=0), Tier 0 plus the tag-based auto-flag
// plus PrepareOracleCall's hard egress filter still protect the bank.
//
// Env vars:
//   SD_SENSITIVE_AI            "1" enable (default), "0" disable
//   SD_SENSITIVE_AI_INTERVAL   tick seconds (default 30)
//   SD_SENSITIVE_AI_BATCH      max records per tick (default 25)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"
)

// SensitiveClassifier is a goroutine-driven Layer 2 classifier.
type SensitiveClassifier struct {
	bank *Bank
	// providerFn resolves the classifier provider on each pass. The
	// goroutine in main.go passes a closure over resolveSensitiveClassifier
	// Provider(bank, router) so the user can flip the sensitive_classifier_
	// tier setting (Tier 1 vs Tier 2) AND have provider hot-swaps from
	// PUT /settings/providers/{tier} land at the next pass without a
	// Core restart.
	providerFn func() LLMProvider
	interval   time.Duration
	batch      int
}

// NewSensitiveClassifier wires a classifier. providerFn should return
// the appropriate provider (Tier 1 from the router, or Tier 2 if the
// user opted into nightly-tier classification via settings). bank is
// required. Passing a plain provider (non-closure) is supported via
// `newSensitiveClassifierConst` — used by admin/test sites that want
// the provider snapshot frozen at construction.
func NewSensitiveClassifier(bank *Bank, providerFn func() LLMProvider, interval time.Duration, batch int) *SensitiveClassifier {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if batch <= 0 {
		batch = 25
	}
	return &SensitiveClassifier{
		bank:       bank,
		providerFn: providerFn,
		interval:   interval,
		batch:      batch,
	}
}

// newSensitiveClassifierConst is a shim for callers that hold a
// concrete provider (admin reclassify, tests) and don't need
// per-pass hot-swap behaviour. The classifier still uses the closure
// internally; this just hides the wrapping.
func newSensitiveClassifierConst(bank *Bank, provider LLMProvider, interval time.Duration, batch int) *SensitiveClassifier {
	return NewSensitiveClassifier(bank, func() LLMProvider { return provider }, interval, batch)
}

// provider snapshots the current provider. nil when unconfigured.
func (sc *SensitiveClassifier) provider() LLMProvider {
	if sc == nil || sc.providerFn == nil {
		return nil
	}
	return sc.providerFn()
}

// Run blocks until ctx is cancelled. Safe to invoke as `go sc.Run(ctx)`.
func (sc *SensitiveClassifier) Run(ctx context.Context) {
	if sc == nil || sc.bank == nil || sc.provider() == nil {
		log.Printf("sensitive-ai: disabled (missing bank or provider)")
		return
	}
	startupName := "(none)"
	if p := sc.provider(); p != nil {
		startupName = p.Name()
	}
	log.Printf("sensitive-ai: starting (interval=%s batch=%d provider=%s)",
		sc.interval, sc.batch, startupName)
	t := time.NewTicker(sc.interval)
	defer t.Stop()
	// Run a pass immediately so the first batch doesn't wait `interval`.
	sc.pass(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sc.pass(ctx)
		}
	}
}

// pass classifies up to `batch` un-scanned records.
func (sc *SensitiveClassifier) pass(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	// Optional second pass: when the LLM classifier is toggled off
	// (sensitive_llm_classifier_enabled=0), skip it — Tier 0 (the synchronous
	// regex scan in SaveMemory) plus the egress filter still protect the bank.
	// Checked per-pass so the toggle takes effect on the next tick, live.
	if !sensitiveLLMClassifierEnabled(sc.bank) {
		return
	}
	recs, err := sc.bank.ListUnscannedForSensitive(sc.batch)
	if err != nil {
		log.Printf("sensitive-ai: list unscanned: %v", err)
		return
	}
	if len(recs) == 0 {
		return
	}
	// Snapshot the provider for the duration of this pass — keeps the
	// audit-log "ai_classifier:<provider>" attribution consistent even if
	// the user PUT /settings/providers/{tier} mid-scan.
	passProvider := sc.provider()
	flagged := 0
	for _, rec := range recs {
		if ctx.Err() != nil {
			return
		}
		// Use enriched text if present (post-Tier-1 refined version) else raw.
		body := rec.EnrichedText
		if body == "" {
			body = rec.Text
		}
		isSensitive, justification, err := sc.classify(ctx, body)
		if err != nil {
			// Don't mark scanned on transient errors — try again next tick.
			log.Printf("sensitive-ai: classify %s: %v", rec.ID, err)
			continue
		}
		if err := sc.bank.MarkSensitiveScan(rec.ID, isSensitive); err != nil {
			log.Printf("sensitive-ai: mark %s: %v", rec.ID, err)
			continue
		}
		if isSensitive && !rec.Sensitive {
			flagged++
			// Reason carries the model's content-substance justification when
			// available — e.g. "ai_classifier:ollama:llama3.2:3b:contains an
			// API key reference". The dashboard's formatReason() picks the
			// 4th colon-separated field as the human-readable detail.
			providerLabel := "unknown"
			if passProvider != nil {
				providerLabel = passProvider.Name()
			}
			reason := "ai_classifier:" + providerLabel
			if justification != "" {
				reason += ":" + justification
			}
			// before/after snapshots: rec is the pre-flip state (sensitive=false);
			// after mirrors it with sensitive=true. Body redacted on both sides
			// per redactForAudit's policy for sensitive flips.
			before := rec
			after := rec
			after.Sensitive = true
			beforeRedacted := redactForAudit(before, "auto_flag_sensitive")
			afterRedacted := redactForAudit(after, "auto_flag_sensitive")
			bj, _ := json.Marshal(beforeRedacted)
			aj, _ := json.Marshal(afterRedacted)
			_ = sc.bank.AppendAudit(AuditEntry{
				Operation:  "auto_flag_sensitive",
				EntityType: "trace",
				EntityID:   rec.ID,
				BeforeJSON: string(bj),
				AfterJSON:  string(aj),
				Reason:     reason,
				AdapterID:  "sd-core-sensitive-ai",
			})
			log.Printf("sensitive-ai: auto-flagged %s as sensitive", rec.ID)
		}
	}
	if flagged > 0 {
		log.Printf("sensitive-ai: pass complete — %d/%d records newly flagged", flagged, len(recs))
	}
}

// classifyPrompt is the YES/NO + 1-line-justification template. We ask
// for two lines so the parse is trivial and we know exactly where to find
// the categorical reason. Models occasionally drift (extra preamble, code
// fences, etc.); the parser is forgiving — see classify() for the recovery
// rules.
//
// The "describe abstractly, do NOT echo verbatim" instruction is a soft
// guard against the model returning the actual sensitive substring as its
// justification. The hard guard is scrubSensitiveSubstrings() at parse
// time — if the model ignores the instruction, regex strips the verbatim
// payload before the audit row stores anything.
const classifyPrompt = `You are a privacy classifier. Default to NO. Only answer YES when the TEXT literally contains a real secret or private fact that would harm the user if posted publicly.

Reply with EXACTLY this format on TWO lines:
LINE 1: YES or NO
LINE 2: If YES, "<category>: <short reason ≤80 chars>" using one category from the taxonomy below. If NO, the literal string "n/a".

Do NOT echo the TEXT verbatim. Describe the category abstractly.

============================================================
CATEGORY TAXONOMY (use the exact tag in LINE 2):
============================================================

  credential — literal API keys, tokens, JWTs, passwords, private keys
  financial  — SSN, credit card #, bank account #, salary, debt tied to a
               real person
  contact    — real person's full name combined with their email or phone
  identity   — real person's full name combined with DOB, home address,
               or government ID (passport / driver's license)
  health     — specific medical, mental-health, or therapy facts about a
               named real person
  journal    — private confidence/journal content naming a real person
  other      — sensitive but doesn't fit the above buckets (use sparingly)

============================================================
THE ONLY THINGS THAT ARE SENSITIVE (answer YES):
============================================================

1. [credential] ACTUAL credential strings present in the text:
   - API key value: "sk-proj-abc123...", "ghp_xyz...", "AKIA...", "AIza..."
   - Password assignment: 'password = "letmein"', 'PG_PASSWORD: hunter2'
   - JWT, session id, OAuth refresh token, signed certificate body
   The literal credential string MUST be present in the TEXT. The mere
   mention of "we use an API key" or "the token expired" is NOT sensitive.

2. [contact / identity] Personally identifying combos involving a REAL
   identified person:
   - Full name (first + last) + email                            → contact
   - Full name + phone number                                    → contact
   - Full name + home address                                    → identity
   - Full name + government ID (SSN, passport, driver's license) → identity
   - Full name + date of birth + location                        → identity
   A first name alone is NEVER sensitive. A first name + project/company
   is NEVER sensitive (that's just dev context). Username/handle alone
   is NEVER sensitive.

3. [health] Specific health/medical/mental-health/therapy facts about a
   real identified person. Not "the user has anxiety as a topic", but
   "Jordan Reyes was diagnosed with X on date Y".

4. [financial] Specific financial details tied to a real identified person:
   account numbers, balances, debt amounts, salary figures.

5. [journal] Private journal-style content naming a real person + a
   specific confidence shared by them. Project status updates are NOT this.

============================================================
THINGS THAT ARE NEVER SENSITIVE (answer NO):
============================================================

- ANY discussion of authentication TECHNOLOGY or PROTOCOLS (OAuth, JWT,
  SAML, SSO, Bearer tokens, refresh tokens, password hashing, session
  management) without a literal credential value.
- Software architecture, design decisions, "how we implemented X".
- Project status notes, build commands, ports, COM/serial port numbers,
  configuration constants, file path conventions, debugging notes.
- "User prefers X" / "Jordan uses Y" / "[handle]'s workflow is Z" — these
  are developer preferences, not secrets.
- File paths even when they contain a Windows username (C:\Users\jreyes\…).
  Filesystem paths are not credentials.
- Mentions of company names, product names, project names, repository
  names, tool names, framework names.
- Bug reports, error messages, stack traces, build logs.
- Placeholder strings like "<your-token-here>", "${API_KEY}", "[REDACTED]".
- Travel notes, conference attendance, food preferences, hobbies, music
  taste — these are user-volunteered facts about themselves; the user
  CHOSE to put them in their own memory bank.
- Documentation, RFC references, library citations, public URLs.
- Synthesis summaries describing a project's architecture or features.

============================================================
EXAMPLES (study these — most NOs should look familiar):
============================================================

TEXT: "Acme Analytics Dashboard uses Grafana for analytics through Cloudflare JWT auth."
ANSWER: NO / n/a (architecture description; JWT mentioned abstractly)

TEXT: "Local OAuth callback is http://127.0.0.1:PORT/callback."
ANSWER: NO / n/a (callback URL pattern; no real credential)

TEXT: "Jordan prefers terse responses with file paths and line numbers."
ANSWER: NO / n/a (developer preference; first name only)

TEXT: "Device Fleet MCP uses HMAC handshake for initial node registration."
ANSWER: NO / n/a (security protocol design)

TEXT: "Jordan Reyes's contact emails: jreyes@example.com, j.reyes@acme-corp.com"
ANSWER: YES / contact: full name plus personal and work email addresses

TEXT: "TaskLogger admin authentication uses scrypt hashing and in-memory sessions."
ANSWER: NO / n/a (auth scheme architecture)

TEXT: "Jordan Alex Reyes, age 33, DOB April 10 1990, located in Springfield IL"
ANSWER: YES / identity: full name + date of birth + home location

TEXT: "Google OAuth requires http://127.0.0.1 as redirect URI for desktop apps."
ANSWER: NO / n/a (OAuth flow documentation; no real client secret)

TEXT: "Firmware calibration: K range 0.040–0.130 step 0.003, best at K=0.057."
ANSWER: NO / n/a (firmware engineering notes)

TEXT: "Build path: C:\Users\jreyes\OneDrive\Projects\MyFirmware\build"
ANSWER: NO / n/a (filesystem path; usernames in paths are not credentials)

TEXT: "Set OPENAI_API_KEY=sk-proj-1abc2def3ghi4 in the .env file before starting."
ANSWER: YES / credential: contains a literal OpenAI API key value

TEXT: "User prefers spicy food and avoids cilantro."
ANSWER: NO / n/a (food preference; user-volunteered self-fact)

TEXT: "User attended FOSDEM in February 2026."
ANSWER: NO / n/a (travel/conference attendance)

TEXT: "OAuth is an open standard for access delegation in cybersecurity."
ANSWER: NO / n/a (textbook definition of a protocol)

TEXT: "Jordan Reyes' Chase checking account 1234567890 balance is $8,402.17."
ANSWER: YES / financial: real person tied to bank account number and balance

TEXT: "Jordan Reyes was diagnosed with type 1 diabetes on 2024-06-12 by Dr. Chen."
ANSWER: YES / health: specific medical diagnosis tied to named real person

TEXT: "Jordan told me in confidence she's planning to leave Acme next quarter, please don't share."
ANSWER: YES / journal: private confidence shared by named real person

TEXT: "Average household debt in the US reached $103k in 2024 per Federal Reserve data."
ANSWER: NO / n/a (aggregate public statistic; no identified person)

TEXT: "Bank routing numbers are 9 digits and identify the bank, not an account holder."
ANSWER: NO / n/a (general financial-system trivia; no real account tied to a person)

============================================================
DECISION CHECKLIST (run through this before committing your answer):
============================================================

1. Does the TEXT contain a literal secret string (key, password, token)?
   - If yes → YES, category=credential
   - If no → continue
2. Does the TEXT contain a REAL person's full name (first + last) AND
   at least one of {email, phone, home address, government ID, DOB,
   medical fact, account number/salary, private confidence}?
   - If yes → YES, pick the matching category (contact / identity /
     health / financial / journal)
   - If no → continue
3. Is everything else just discussion / architecture / preferences /
   workflow / configuration / project context?
   - Then → NO

When ambiguous, ALWAYS answer NO. False positives degrade the bank far
more than missing the rare genuinely-sensitive memory (which the user
can manually flag from the trace detail panel).

TEXT:
` + "```\n%s\n```"

// aiJustificationMaxBytes caps how much justification text can land in the
// audit row. Longer than the model's prompt-imposed 80-char limit because
// (a) we accept short overruns, and (b) ASCII char ≠ byte for non-Latin
// content. 120 bytes is enough to be useful, short enough that an audit
// page rendering hundreds of rows stays snappy.
const aiJustificationMaxBytes = 120

// SensitiveClassifierPromptKey is the settings-table key where the user
// can override the default classifyPrompt. Setting it to empty (delete
// the row) reverts to the hardcoded default. The prompt MUST contain a
// single `%s` placeholder for the memory text; validation strips overrides
// that don't include the placeholder so we never send memories blind.
const SensitiveClassifierPromptKey = "sensitive_classifier_prompt"

// SensitiveClassifierTierKey selects which model tier the classifier
// calls. Values: "tier1" (default; llama3.2:3b — fast but more false
// positives on nuanced topics like "discussing OAuth abstractly") or
// "tier2" (llama3.1:8b — slower per memory but follows the prompt's
// "abstract discussion ≠ sensitive" carve-out much more reliably).
//
// Hot-swap policy:
//   - Bulk reclassify (/admin/sensitive/reclassify) reads this on every
//     call → switching tiers + clicking "Reprocess all" takes effect
//     immediately.
//   - The 30-second background classifier (sensitive_ai goroutine)
//     captures its provider at startup → tier changes apply on the
//     next core restart. This is a deliberate trade-off: bulk reclassify
//     is the "fix the bank" path, while the background classifier is
//     for "live-as-memories-arrive" tagging where rapid tier flapping
//     would produce inconsistent flag state.
const SensitiveClassifierTierKey = "sensitive_classifier_tier"

// SensitiveLLMClassifierEnabledKey is the live toggle for the OPTIONAL Tier 1/2
// LLM second pass. Default ON ("" / "1" / "true"). Set to "0"/"false" to run
// Tier 0 (deterministic regex) alone — Tier 0 still flags keys/cards/SSNs
// synchronously at write time, so this only disables the prose-level LLM pass.
// Checked per-pass + per bulk-reclassify call, so the toggle takes effect live.
const SensitiveLLMClassifierEnabledKey = "sensitive_llm_classifier_enabled"

// sensitiveLLMClassifierEnabled reports whether the optional LLM second pass
// should run. Defaults TRUE (unset/blank/unparseable → enabled) so existing
// installs keep their current behaviour; the user opts OUT for Tier-0-only.
func sensitiveLLMClassifierEnabled(bank *Bank) bool {
	if bank == nil {
		return true
	}
	val, ok, err := bank.GetSetting(SensitiveLLMClassifierEnabledKey)
	if err != nil || !ok {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(val)) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// promptCacheTTL determines how long classifyPrompt() caches a user
// override before re-reading from the bank. Short enough that toggling
// the prompt in Privacy config feels live; long enough that a 25-record
// batch doesn't hit the settings table 25 times.
const sensitivePromptCacheTTL = 10 * time.Second

// effectivePromptFor returns the prompt template currently in force —
// the user's override (loaded from the settings table) if present and
// valid, else the hardcoded default. Caches for sensitivePromptCacheTTL
// so a single batch only hits the bank once.
type promptCache struct {
	value     string
	loadedAt  time.Time
}

var sensitivePromptCache promptCache

// resolveSensitiveClassifierProvider picks Tier 2 or Tier 1 based on the
// user's stored preference. Falls back to Tier 1 (the historic default)
// when the setting is unset, unknown, or Tier 2 is unconfigured.
// Called by handlers that do ad-hoc classification (bulk reclassify);
// the background goroutine resolves once at startup.
//
// LOCAL-ONLY INVARIANT: this only ever returns Tier 1 (ForRealtime) or Tier 2
// — the local Ollama providers. It MUST NEVER return Tier 3 (router.ForOracle/
// the remote provider): classifying a memory for sensitivity must not send its
// text off-box. Do not add a Tier-3 branch here.
func resolveSensitiveClassifierProvider(bank *Bank, router *ModelRouter) LLMProvider {
	if router == nil {
		return nil
	}
	tier1 := router.ForRealtime()
	if bank == nil {
		return tier1
	}
	val, _, _ := bank.GetSetting(SensitiveClassifierTierKey)
	if val == "tier2" {
		t2 := router.Tier2()
		if t2 != nil {
			return t2
		}
		// User asked for Tier 2 but it isn't configured; fall back to
		// Tier 1 so the classifier still runs. Caller can surface a
		// warning if it wants to.
	}
	return tier1
}

// invalidateSensitivePromptCache forces the next classifier call to re-read
// the prompt setting from the bank. Called by the settings PUT handler so a
// user prompt change takes effect within the same second instead of waiting
// for the 10s TTL — same hot-swap pattern as provider config (PUT
// /settings/providers/{tier} triggers an immediate router reload).
func invalidateSensitivePromptCache() {
	sensitivePromptCache.value = ""
	sensitivePromptCache.loadedAt = time.Time{}
}

func (sc *SensitiveClassifier) effectivePrompt() string {
	if sc == nil || sc.bank == nil {
		return classifyPrompt
	}
	if time.Since(sensitivePromptCache.loadedAt) < sensitivePromptCacheTTL && sensitivePromptCache.value != "" {
		return sensitivePromptCache.value
	}
	val, ok, err := sc.bank.GetSetting(SensitiveClassifierPromptKey)
	if err != nil || !ok || strings.TrimSpace(val) == "" {
		sensitivePromptCache.value = classifyPrompt
		sensitivePromptCache.loadedAt = time.Now()
		return classifyPrompt
	}
	// Defensive: a prompt without the %s placeholder for memory text would
	// send empty content to the model and silently flag everything as YES
	// (or NO — depending on the model's default). Refuse the override and
	// stay on the safe default.
	if !strings.Contains(val, "%s") {
		log.Printf("sensitive-ai: user prompt missing %%s placeholder, falling back to default")
		sensitivePromptCache.value = classifyPrompt
		sensitivePromptCache.loadedAt = time.Now()
		return classifyPrompt
	}
	sensitivePromptCache.value = val
	sensitivePromptCache.loadedAt = time.Now()
	return val
}

// classify asks the LLM and returns (isSensitive, justification, error).
// Justification is "" when:
//   - the model said NO
//   - the model said YES but provided no second line
//   - the model returned malformed output (we still surface the YES/NO call)
//
// All justifications are scrubbed via scrubSensitiveSubstrings to ensure
// the verbatim sensitive payload never lands in the audit row, even if the
// model ignored the prompt's "describe abstractly" rule.
func (sc *SensitiveClassifier) classify(ctx context.Context, text string) (bool, string, error) {
	// Resolve the current provider once for this call (hot-swap-aware).
	p := sc.provider()
	if p == nil {
		return false, "", errors.New("sensitive-ai: no provider configured")
	}
	if len(text) > 2000 {
		text = text[:2000]
	}
	// 25s was fine for Tier 1 (3B) and the older terse prompt. The new
	// prompt is ~8KB (taxonomy + 17 examples + checklist) and Tier 2
	// (llama3.1:8b) on CPU regularly runs 30–60s for one classification.
	// 120s is a safe ceiling that still bounds runaway calls.
	cctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	// Budget bumped from 8 to 80 tokens to fit the two-line response. The
	// 80-char limit on LINE 2 is a soft guideline; budget gives the model
	// room to comply or slightly overrun.
	// Bank.ThinkOffPrefix() returns "[think:off]\n\n" when
	// settings.tier1_think_off is ON (default) — disables reasoning-trace
	// emission on reasoning-capable models so the YES/NO budget isn't
	// eaten by a hidden think trace. Users running reasoning models that
	// benefit from the trace can disable via Config → AI/API. No-op on
	// non-reasoning default models.
	prefix := ""
	if sc.bank != nil {
		prefix = sc.bank.ThinkOffPrefix()
	} else {
		prefix = "[think:off]\n\n"
	}
	out, err := p.Chat(cctx, []Message{
		{Role: "system", Content: prefix + "Reply with TWO lines exactly: LINE 1 is YES or NO; LINE 2 is a short justification (or n/a)."},
		{Role: "user", Content: formatPrompt(sc.effectivePrompt(), text)},
	}, 80)
	if err != nil {
		return false, "", err
	}
	isYes, justification := parseClassifyResponse(out)
	if !isYes {
		return false, "", nil
	}
	// Strip any verbatim sensitive substring the model may have echoed back.
	justification = scrubSensitiveSubstrings(justification)
	if len(justification) > aiJustificationMaxBytes {
		justification = justification[:aiJustificationMaxBytes]
	}
	return true, justification, nil
}

// parseClassifyResponse parses the model's two-line reply. Forgiving:
//
//   - Whitespace and blank lines are tolerated.
//   - "LINE 1: YES" / "1. YES" / "YES" all accepted on the first line.
//   - "LINE 2: ..." / "2. ..." / bare text accepted on the second line.
//   - "n/a" (any case) → empty justification.
//   - Malformed input → ("NO", "") so the caller treats the row as not
//     sensitive (fail-closed against hallucination).
func parseClassifyResponse(raw string) (bool, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false, ""
	}
	// Strip code-fence markers if the model wrapped its output.
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	parts := strings.SplitN(raw, "\n", 2)
	first := strings.TrimSpace(parts[0])
	first = trimLineLabel(first)
	upper := strings.ToUpper(first)
	// Take the first whitespace-delimited word so "YES, contains X" still
	// parses as YES.
	if i := strings.IndexAny(upper, " \t.,;:!?"); i >= 0 {
		upper = upper[:i]
	}
	isYes := upper == "YES" || upper == "Y"
	if !isYes {
		return false, ""
	}
	if len(parts) < 2 {
		return true, "" // YES with no justification line
	}
	detail := strings.TrimSpace(parts[1])
	detail = trimLineLabel(detail)
	if strings.EqualFold(detail, "n/a") || detail == "" {
		return true, ""
	}
	// Strip surrounding quotes the model sometimes adds.
	detail = strings.Trim(detail, `"' `)
	return true, detail
}

// trimLineLabel removes "LINE 1:", "LINE 2:", "1.", "2." prefixes so the
// downstream parse sees only the content.
func trimLineLabel(s string) string {
	for _, prefix := range []string{"LINE 1:", "LINE 2:", "Line 1:", "Line 2:", "line 1:", "line 2:", "1.", "2.", "1)", "2)"} {
		if strings.HasPrefix(s, prefix) {
			return strings.TrimSpace(s[len(prefix):])
		}
	}
	return s
}

// scrubSensitiveSubstrings replaces any verbatim sensitive substring in
// `text` with the categorical pattern name in brackets — e.g., a model
// reply "contains the key sk-proj-AbCdEf..." becomes "contains the key
// [openai_api_key]". Defends against the model ignoring the prompt's
// "describe abstractly" instruction.
func scrubSensitiveSubstrings(text string) string {
	if text == "" {
		return text
	}
	for _, p := range sensitivePatterns {
		text = p.rx.ReplaceAllStringFunc(text, func(match string) string {
			if p.check != nil && !p.check(match) {
				return match
			}
			return "[" + p.name + "]"
		})
	}
	return text
}

// formatPrompt fills a single %s into a template without importing fmt twice.
func formatPrompt(tpl, body string) string {
	return strings.Replace(tpl, "%s", body, 1)
}
