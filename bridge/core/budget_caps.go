// budget_caps.go — parses the SD_TIER{N}_API_{VENDOR}_BUDGET envelope
// and exposes a structured view of configured per-tier+vendor spend
// caps + current spend.
//
// Format: <Period><Amount>
//   Period: M=monthly, W=weekly, D=daily, T=total (lifetime)
//   Amount: integer or decimal in USD (no symbol, no thousands sep)
//
// Examples:
//   M20    → $20/month
//   M50.0  → $50/month (decimal accepted)
//   W5     → $5/week
//   D2     → $2/day
//   T100   → $100 lifetime
//
// Parsing is forgiving: case-insensitive period, leading whitespace
// trimmed, surrounding quotes stripped. Returns zero-value when the
// input is empty or unparseable so the caller can render "no cap"
// rather than blocking on a configuration error.
package main

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// BudgetSpec is the parsed shape of an SD_TIER*_API_*_BUDGET env value.
type BudgetSpec struct {
	Period  string  `json:"period"` // "monthly" | "weekly" | "daily" | "total" | ""
	Amount  float64 `json:"amount"` // dollars
	Raw     string  `json:"raw"`    // original env value, for debugging
	Display string  `json:"display"` // pre-formatted "$20/month" string
}

// IsZero is true when no usable cap was configured.
func (b BudgetSpec) IsZero() bool { return b.Period == "" || b.Amount <= 0 }

var budgetSpecPattern = regexp.MustCompile(`^([MWDTmwdt])([0-9]+(?:\.[0-9]+)?)$`)

// parseBudgetSpec converts an "M20" / "W5" / "D2" / "T100" envelope
// string into a BudgetSpec. Returns zero-value spec for unparseable
// or empty input — never errors.
func parseBudgetSpec(raw string) BudgetSpec {
	s := strings.TrimSpace(raw)
	s = strings.Trim(s, "\"'")
	if s == "" {
		return BudgetSpec{}
	}
	m := budgetSpecPattern.FindStringSubmatch(s)
	if m == nil {
		return BudgetSpec{Raw: raw}
	}
	period := ""
	switch strings.ToLower(m[1]) {
	case "m":
		period = "monthly"
	case "w":
		period = "weekly"
	case "d":
		period = "daily"
	case "t":
		period = "total"
	}
	amt, err := strconv.ParseFloat(m[2], 64)
	if err != nil || amt <= 0 {
		return BudgetSpec{Raw: raw}
	}
	display := fmt.Sprintf("$%g", amt)
	switch period {
	case "monthly":
		display += "/month"
	case "weekly":
		display += "/week"
	case "daily":
		display += "/day"
	case "total":
		display += " lifetime"
	}
	return BudgetSpec{Period: period, Amount: amt, Raw: raw, Display: display}
}

// budgetWindowStart returns the inclusive start date (YYYY-MM-DD) for
// the spec's period, anchored to the supplied "now" timestamp. For
// "total", returns the unix epoch date so callers can sum from the
// beginning of time.
func budgetWindowStart(spec BudgetSpec, now time.Time) string {
	switch spec.Period {
	case "monthly":
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).Format("2006-01-02")
	case "weekly":
		// Weekly window = last 7 days inclusive of today (Sun-rolling).
		return now.AddDate(0, 0, -6).Format("2006-01-02")
	case "daily":
		return now.Format("2006-01-02")
	case "total":
		return "1970-01-01"
	}
	return ""
}

// envBudgetKeys returns the set of SD_TIER*_API_*_BUDGET env var names
// currently set, with their parsed specs and the matching key env var
// name (so the caller can decide whether the matching key is also set).
func envBudgetKeys() []ConfiguredBudget {
	out := []ConfiguredBudget{}
	// Iterate through all env vars matching the pattern. We look for
	// SD_TIER{1,2,3}_API_{VENDOR}_BUDGET where VENDOR is a token.
	pattern := regexp.MustCompile(`^SD_TIER([1-3])_API_([A-Z]+)_BUDGET$`)
	for _, env := range os.Environ() {
		eq := strings.IndexByte(env, '=')
		if eq < 0 {
			continue
		}
		name := env[:eq]
		val := env[eq+1:]
		m := pattern.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		spec := parseBudgetSpec(val)
		if spec.IsZero() {
			continue
		}
		tier := "tier" + m[1] + "_provider"
		vendor := strings.ToLower(m[2])
		keyEnv := fmt.Sprintf("SD_TIER%s_API_%s_KEY", m[1], m[2])
		keys := resolveAPIKeys(keyEnv)
		out = append(out, ConfiguredBudget{
			Tier:           tier,
			Vendor:         vendor,
			BudgetEnv:      name,
			KeyEnv:         keyEnv,
			Spec:           spec,
			KeyCount:       len(keys),
			HasKey:         len(keys) > 0,
		})
	}
	return out
}

// ConfiguredBudget pairs a parsed budget spec with the tier/vendor it
// applies to and the key-count for that vendor.
type ConfiguredBudget struct {
	Tier      string     `json:"tier"`       // tier1_provider | tier2_provider | tier3_provider
	Vendor    string     `json:"vendor"`     // openai | claude | mistral | ...
	BudgetEnv string     `json:"budget_env"` // env var name we parsed
	KeyEnv    string     `json:"key_env"`    // env var name for the matching key(s)
	Spec      BudgetSpec `json:"spec"`
	KeyCount  int        `json:"key_count"`  // number of keys configured for this tier+vendor
	HasKey    bool       `json:"has_key"`    // KeyCount > 0
}

// resolveAPIKeys reads the named env var and splits it into individual
// keys. Supports two formats:
//   1. Comma-separated:  SD_TIER3_API_OPENAI_KEY="sk-1,sk-2,sk-3"
//   2. Numeric suffixes: SD_TIER3_API_OPENAI_KEY_1, _2, _3 (ALSO checked
//      independent of the bare name)
// Comma values inside a single env var win when both shapes are set.
//
// Returns an empty slice when no key is found. Used by both the budget
// admin surface (key_count) and the provider's auth path (rotation).
func resolveAPIKeys(envName string) []string {
	out := []string{}
	if envName == "" {
		return out
	}
	bare := strings.TrimSpace(os.Getenv(envName))
	if bare != "" {
		for _, k := range strings.Split(bare, ",") {
			k = strings.TrimSpace(k)
			if k != "" {
				out = append(out, k)
			}
		}
	}
	// Also check numeric-suffixed forms: ENVNAME_1, _2, _3, ...
	// Stop at first gap so we don't iterate to infinity.
	for i := 1; i <= 32; i++ {
		v := strings.TrimSpace(os.Getenv(fmt.Sprintf("%s_%d", envName, i)))
		if v == "" {
			if i > 1 || bare != "" {
				break // gap reached; stop scanning
			}
			continue
		}
		out = append(out, v)
	}
	return out
}
