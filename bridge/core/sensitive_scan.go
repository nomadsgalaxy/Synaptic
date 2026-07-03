// sensitive_scan.go — Tier 0 sensitivity classifier (deterministic, no LLM).
//
// Tier 0 is the always-on FIRST pass: pure regex + checksum (Luhn, IBAN
// mod-97), no model, no network, sub-millisecond. It runs synchronously
// inside Bank.SaveMemory and Bank.UpdateMemory whenever the text content
// changes, so a key/card/SSN is flagged the instant it's written — even if
// the Tier 1/2 LLM classifier is disabled or offline. It catches the
// high-confidence patterns: real API keys, JWTs, private-key blocks,
// government/financial IDs (SSN, IBAN), credit cards (Luhn-checked), bearer
// tokens, connection-string credentials, and explicit password assignments.
//
// The optional Tier 1/2 LLM classifier (sensitive_classifier.go) is the
// SECOND pass — local-only (Ollama Tier 1/2, never the Tier 3 Oracle) — for
// the harder cases regex can't catch without massive false-positive rates:
// names embedded in prose, casual mentions of medical conditions, etc. It can
// be toggled off (sensitive_llm_classifier_enabled=0) to run Tier 0 alone.
//
// Design contract:
//   - This pass is FAST. Hundreds of microseconds for typical input.
//   - This pass NEVER lowers sensitive — only raises. Sticky-on.
//   - Every match returns the *category name* (not the matched substring),
//     so we can audit-log "auto-flagged: api_key, ssn" without ever writing
//     the secret itself into the audit log. (The user's secret stays on
//     the original record; the audit only mentions categories.)
//   - When a sensitive credit card or SSN is detected, we still keep the
//     full text in `text` (immutable raw storage). The protection is at
//     the egress side — PrepareOracleCall filters the whole record. We
//     do not redact in-place because that breaks the raw-text invariant.
package main

import (
	"regexp"
	"strings"
)

// sensitivePattern pairs a category name with a regex.
type sensitivePattern struct {
	name  string
	rx    *regexp.Regexp
	check func(string) bool // optional secondary check (e.g., Luhn)
}

// All patterns are case-insensitive unless the category requires the original
// casing (e.g., AWS access keys are upper-case, GitHub PATs are mixed).
var sensitivePatterns = []sensitivePattern{
	// ── Cloud / API keys ────────────────────────────────────────────────
	{name: "openai_api_key", rx: regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{20,}\b`)},
	{name: "anthropic_api_key", rx: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_\-]{20,}\b`)},
	{name: "github_token", rx: regexp.MustCompile(`\b(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}\b`)},
	{name: "google_api_key", rx: regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{30,}\b`)},
	{name: "aws_access_key", rx: regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{name: "aws_session_token", rx: regexp.MustCompile(`\b(ASIA|AROA)[0-9A-Z]{16}\b`)},
	{name: "slack_token", rx: regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`)},
	{name: "stripe_key", rx: regexp.MustCompile(`\b(sk|rk|pk)_(live|test)_[A-Za-z0-9]{20,}\b`)},
	{name: "twilio_sid", rx: regexp.MustCompile(`\bAC[0-9a-f]{32}\b`)},
	{name: "sendgrid_key", rx: regexp.MustCompile(`\bSG\.[A-Za-z0-9_\-]{16,}\.[A-Za-z0-9_\-]{16,}\b`)},

	// ── Tokens / sessions ───────────────────────────────────────────────
	{
		name: "jwt",
		rx:   regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`),
	},
	{
		name: "bearer_token",
		rx:   regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._\-~+/=]{16,}`),
	},

	// ── Private keys ────────────────────────────────────────────────────
	{
		name: "pem_private_key",
		rx:   regexp.MustCompile(`-----BEGIN (RSA |EC |OPENSSH |DSA |PGP |ENCRYPTED |PRIVATE KEY)`),
	},
	{
		name: "ssh_private_key_id",
		rx:   regexp.MustCompile(`-----BEGIN OPENSSH PRIVATE KEY-----`),
	},

	// ── Explicit secret assignments ─────────────────────────────────────
	// Only triggers on common code/config patterns (`password = "x"`, `api_key: y`).
	// Conservative on bareword "password" mentions to keep false-positives down.
	{
		name: "password_assignment",
		rx:   regexp.MustCompile(`(?i)(?:^|\W)(?:password|passwd|pwd|passphrase)\s*[:=]\s*["']?[^\s"',;]{4,}`),
	},
	{
		name: "secret_assignment",
		rx:   regexp.MustCompile(`(?i)(?:^|\W)(?:api[_-]?key|secret|access[_-]?token|auth[_-]?token|client[_-]?secret)\s*[:=]\s*["']?[^\s"',;]{8,}`),
	},

	// ── Government / financial IDs ──────────────────────────────────────
	{
		name: "ssn",
		// 3-2-4 with hyphens or spaces. Reject obvious all-zero / 666 /
		// 9XX area-number placeholders via the check callback (RE2 has no
		// lookahead).
		rx: regexp.MustCompile(`\b\d{3}[- ]\d{2}[- ]\d{4}\b`),
		check: func(s string) bool {
			if len(s) < 11 {
				return false
			}
			a := s[:3]
			if a == "000" || a == "666" {
				return false
			}
			if a[0] == '9' {
				return false
			}
			// Middle group of 00 or last 0000 are also invalid SSNs.
			if s[4:6] == "00" || s[7:11] == "0000" {
				return false
			}
			return true
		},
	},
	{
		name: "credit_card",
		rx:   regexp.MustCompile(`\b(?:\d[ -]?){13,19}\b`),
		check: func(s string) bool {
			digits := stripNonDigits(s)
			if len(digits) < 13 || len(digits) > 19 {
				return false
			}
			return luhnValid(digits)
		},
	},

	// ── More cloud / vendor API tokens (distinctive prefixes → near-zero
	// false positives, no secondary check needed). ────────────────────────
	{name: "gitlab_token", rx: regexp.MustCompile(`\bglpat-[A-Za-z0-9_\-]{20,}\b`)},
	{name: "npm_token", rx: regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`)},
	{name: "huggingface_token", rx: regexp.MustCompile(`\bhf_[A-Za-z0-9]{30,}\b`)},
	{name: "digitalocean_token", rx: regexp.MustCompile(`\bdop_v1_[a-f0-9]{64}\b`)},
	{name: "mailgun_key", rx: regexp.MustCompile(`\bkey-[0-9a-f]{32}\b`)},
	{name: "gcp_service_account", rx: regexp.MustCompile(`\b[a-z0-9][a-z0-9\-]{4,}@[a-z0-9\-]+\.iam\.gserviceaccount\.com\b`)},
	{
		// Azure storage / Service Bus connection-string key (context-gated on
		// AccountKey=/SharedAccessKey= so a bare base64 blob doesn't trip it).
		name: "azure_storage_key",
		rx:   regexp.MustCompile(`(?i)\b(?:AccountKey|SharedAccessKey)\s*=\s*[A-Za-z0-9+/]{40,}={0,2}`),
	},

	// ── Credentials embedded in connection strings / URLs ──────────────────
	// scheme://user:password@host — catches DB/broker passwords that the
	// secret_assignment pattern misses. The `:[^@\s]+@` requires a non-empty
	// password segment, so `https://example.com` (no creds) won't match.
	{
		name: "connection_string_credentials",
		rx:   regexp.MustCompile(`(?i)\b(?:postgres|postgresql|mysql|mongodb(?:\+srv)?|redis|amqp|amqps|https?|ftp|ssh)://[^:/\s]+:[^@/\s]+@`),
	},

	// ── More government / financial IDs ────────────────────────────────────
	{
		// IBAN: 2-letter country + 2 check digits + up to 30 alnum, validated
		// with the ISO 7064 mod-97 checksum to kill random uppercase matches.
		name:  "iban",
		rx:    regexp.MustCompile(`\b[A-Z]{2}\d{2}[A-Z0-9]{11,30}\b`),
		check: func(s string) bool { return ibanValid(s) },
	},
	{
		// Passport / driver-license — ONLY when explicitly labelled, to avoid
		// matching any 6-9 char alnum token. Conservative by design.
		name: "passport_or_license",
		rx:   regexp.MustCompile(`(?i)\b(?:passport|driver'?s?\s*licen[sc]e|\bDL)\b\s*(?:no\.?|number|#|:)?\s*[A-Z0-9]{5,12}\b`),
	},

	// ── PII (lower confidence — these are likely true positives but match
	// frequently in normal text. Listed last so the named match is recorded
	// but the AI classifier is the higher-quality arbiter for long prose).
	{
		name: "email",
		rx:   regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`),
	},
	{
		name: "phone",
		// NANP + international common formats; somewhat conservative.
		rx: regexp.MustCompile(`(?:(?:\+\d{1,3}[ \-.])?\(?\d{3}\)?[ \-.]?\d{3}[ \-.]?\d{4})`),
	},
}

// scanSensitive returns a sorted, de-duplicated list of category names that
// matched in `text`. Empty list means nothing matched. The categories are
// safe to log (they describe *what* was found, not the secret itself).
func scanSensitive(text string) []string {
	if text == "" {
		return nil
	}
	hits := map[string]bool{}
	for _, p := range sensitivePatterns {
		matches := p.rx.FindAllString(text, -1)
		if len(matches) == 0 {
			continue
		}
		if p.check == nil {
			hits[p.name] = true
			continue
		}
		// Secondary check (e.g., Luhn on credit cards).
		for _, m := range matches {
			if p.check(m) {
				hits[p.name] = true
				break
			}
		}
	}
	if len(hits) == 0 {
		return nil
	}
	out := make([]string, 0, len(hits))
	for h := range hits {
		out = append(out, h)
	}
	sortStrings(out)
	return out
}

// sortStrings is a tiny replacement for sort.Strings to keep this file's
// import surface minimal (oracle_safety.go already imports sort).
func sortStrings(s []string) {
	// Insertion sort — list is always tiny (<10 elements).
	for i := 1; i < len(s); i++ {
		j := i
		for j > 0 && s[j-1] > s[j] {
			s[j-1], s[j] = s[j], s[j-1]
			j--
		}
	}
}

func stripNonDigits(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// luhnValid implements the Luhn checksum used by all major credit cards.
func luhnValid(digits string) bool {
	if len(digits) == 0 {
		return false
	}
	sum := 0
	alt := false
	for i := len(digits) - 1; i >= 0; i-- {
		n := int(digits[i] - '0')
		if alt {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		alt = !alt
	}
	return sum%10 == 0
}

// ibanValid checks an IBAN with the ISO 7064 mod-97 algorithm: move the first
// four chars to the end, map letters A-Z → 10-35, then the whole number mod 97
// must equal 1. Computed digit-by-digit to avoid big-int overflow.
func ibanValid(s string) bool {
	s = strings.ToUpper(strings.ReplaceAll(s, " ", ""))
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	// Rearrange: first 4 chars to the end.
	rearr := s[4:] + s[:4]
	rem := 0
	for _, r := range rearr {
		var d int
		switch {
		case r >= '0' && r <= '9':
			d = int(r - '0')
		case r >= 'A' && r <= 'Z':
			d = int(r-'A') + 10
		default:
			return false
		}
		// Fold each mapped value (1 or 2 digits) into the running remainder.
		if d >= 10 {
			rem = (rem*100 + d) % 97
		} else {
			rem = (rem*10 + d) % 97
		}
	}
	return rem == 1
}
