// handlers_admin_llm.go — Wave 8e LLM benchmark + nightly-duration estimator.
//
// Three endpoints under /admin/llm/*:
//
//   POST /admin/llm/benchmark/{tier}    — run a small Chat call against
//                                          the configured tier provider,
//                                          measure tokens/sec, persist the
//                                          (kind, model, compression) row.
//   GET  /admin/llm/benchmark            — list cached benchmark records.
//   GET  /admin/llm/estimate_nightly     — combine recent nightly stats +
//                                          the Tier 2 benchmark to render
//                                          "your next nightly will take ~Xh".
//
// Why the (kind, model, compression) tuple instead of per-tier rows:
// the same model on the same compression has the same rate regardless of
// which tier it's configured for. Caching by tuple means swapping Tier 2
// for the same Llama-3.1-70B configuration the user benchmarked at Tier
// 3 last week reuses that measurement.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// benchmarkPromptDefault is the fixed ~50-token input we run against every
// provider. Realistic shape (one-sentence summarisation of a short
// memory) so the rate reflects nightly-pipeline-like work, not a
// degenerate empty prompt or a long-context probe.
const benchmarkPromptDefault = `Summarize the following memory in one sentence: I went to the grocery store this morning and picked up bread, milk, and apples. The cashier was new and seemed nervous. I helped them locate the produce codes for the apples and we chatted briefly about the weather.`

// benchmarkMaxOutputTokens caps the response. 50 keeps even AirLLM's
// 1-3 tok/s rate finishing within ~30s; longer outputs would inflate
// the measured rate by amortising prompt-processing cost across more
// tokens but would also bias toward providers with cheap prompt-eval.
const benchmarkMaxOutputTokens = 50

// benchmarkCallTimeout matches the wave 8e nightly LLM-call ceiling.
// AirLLM-class providers can need 30+ seconds for 50 tokens.
const benchmarkCallTimeout = 30 * time.Minute

// recentBenchmarkWindow gates the auto-benchmark trigger: skip when a
// matching record is younger than this. 24h is loose enough that a
// daily compose-restart doesn't keep re-benchmarking the same provider.
const recentBenchmarkWindow = 24 * time.Hour

// adminLLMRouter dispatches /admin/llm/*. Registered in main.go.
func (s *Server) adminLLMRouter(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/llm")
	rest = strings.TrimPrefix(rest, "/")
	switch {
	case rest == "benchmark":
		s.handleAdminLLMBenchmarkList(w, r)
	case rest == "estimate_nightly":
		s.handleAdminLLMEstimateNightly(w, r)
	case strings.HasPrefix(rest, "benchmark/"):
		tierStr := strings.TrimPrefix(rest, "benchmark/")
		tierStr = strings.Trim(tierStr, "/")
		if tierStr == "" {
			http.Error(w, "missing tier in /admin/llm/benchmark/{tier}", http.StatusBadRequest)
			return
		}
		s.handleAdminLLMBenchmark(w, r, tierStr)
	default:
		http.NotFound(w, r)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// POST /admin/llm/benchmark/{tier}
// ──────────────────────────────────────────────────────────────────────────

// handleAdminLLMBenchmark runs the benchmark Chat call against the tier's
// configured provider + persists the result.
func (s *Server) handleAdminLLMBenchmark(w http.ResponseWriter, r *http.Request, tierStr string) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST", "OPTIONS")
		return
	}
	if s.bank == nil {
		http.Error(w, "bank disabled", http.StatusServiceUnavailable)
		return
	}
	tier, ok := parseTierKey(tierStr)
	if !ok {
		http.Error(w, "unknown tier; expected tier1|tier2|tier3|tier1_provider|tier2_provider|tier3_provider", http.StatusBadRequest)
		return
	}
	// Body is optional. When present it overrides the defaults; omit and
	// we benchmark with the canonical prompt + the tier's configured model.
	var body struct {
		Prompt string `json:"prompt"`
		Model  string `json:"model"`
	}
	if r.ContentLength > 0 {
		_ = readJSON(r, &body)
	}

	rec, err := s.runProviderBenchmark(r.Context(), tier, body.Prompt, body.Model)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if err := s.bank.UpsertProviderBenchmark(rec); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditWrite(AuditEntry{
		Operation:  "llm_benchmark",
		EntityType: "provider",
		EntityID:   string(tier),
		AfterJSON: mustJSON(map[string]any{
			"provider_kind":     rec.ProviderKind,
			"model":             rec.Model,
			"compression":       rec.Compression,
			"tokens_per_sec":    rec.TokensPerSec,
			"elapsed_ms":        rec.ElapsedMS,
			"prompt_tokens":     rec.PromptTokens,
			"completion_tokens": rec.CompletionTokens,
		}),
		Reason:    "POST /admin/llm/benchmark",
		AdapterID: adapterIDFromRequest(r),
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"tier":              string(tier),
		"provider_kind":     rec.ProviderKind,
		"model":             rec.Model,
		"compression":       rec.Compression,
		"elapsed_ms":        rec.ElapsedMS,
		"prompt_tokens":     rec.PromptTokens,
		"completion_tokens": rec.CompletionTokens,
		"tokens_per_sec":    rec.TokensPerSec,
		"sample_excerpt":    rec.SampleExcerpt,
		"benchmarked_at":    rec.BenchmarkedAt,
	})
}

// runProviderBenchmark is the workhorse: builds a temporary provider
// matching the tier config, runs one Chat call against it, measures.
// promptOverride + modelOverride are passed through from the request
// body when non-empty.
func (s *Server) runProviderBenchmark(ctx context.Context, tier TierKey, promptOverride, modelOverride string) (ProviderBenchmark, error) {
	cfg, err := s.bank.GetProviderConfig(tier)
	if err != nil {
		return ProviderBenchmark{}, fmt.Errorf("get provider config: %w", err)
	}
	if cfg.Kind == "" || cfg.Kind == ProviderDisabled {
		return ProviderBenchmark{}, fmt.Errorf("tier %s is disabled or unconfigured — set kind first via /settings/providers/%s", tier, tier)
	}
	if modelOverride != "" {
		cfg.Model = modelOverride
	}
	if cfg.Model == "" {
		return ProviderBenchmark{}, fmt.Errorf("tier %s has no configured model", tier)
	}
	provider := BuildProvider(cfg)
	if provider == nil {
		return ProviderBenchmark{}, fmt.Errorf("unable to build provider for kind=%s", cfg.Kind)
	}

	// Read compression — separate setting key for AirLLM, "" otherwise.
	compression := ""
	if cfg.Kind == ProviderAirLLM {
		if v, ok, _ := s.bank.GetSetting("airllm." + string(tier) + ".compression"); ok {
			compression = v
		}
	}

	prompt := promptOverride
	if prompt == "" {
		prompt = benchmarkPromptDefault
	}
	promptTokens := estimateTokens(prompt)

	callCtx, cancel := context.WithTimeout(ctx, benchmarkCallTimeout)
	defer cancel()
	start := time.Now()
	out, err := provider.Chat(callCtx, []Message{{Role: "user", Content: prompt}}, benchmarkMaxOutputTokens)
	elapsed := time.Since(start)
	if err != nil {
		return ProviderBenchmark{}, fmt.Errorf("provider chat: %w", err)
	}
	out = strings.TrimSpace(out)
	completionTokens := estimateTokens(out)

	// tokens_per_sec — guard against divide-by-zero on freakishly fast
	// responses. completion_tokens=0 also guarded — if the provider
	// returned nothing we don't have a meaningful rate.
	var rate float64
	elapsedSec := elapsed.Seconds()
	if elapsedSec > 0 && completionTokens > 0 {
		rate = float64(completionTokens) / elapsedSec
	}

	excerpt := out
	if len(excerpt) > 200 {
		excerpt = excerpt[:200]
	}

	return ProviderBenchmark{
		ProviderKind:     string(cfg.Kind),
		Model:            cfg.Model,
		Compression:      compression,
		ElapsedMS:        elapsed.Milliseconds(),
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TokensPerSec:     rate,
		SampleExcerpt:    excerpt,
		BenchmarkedAt:    time.Now().UTC().Format(time.RFC3339Nano),
	}, nil
}

// ──────────────────────────────────────────────────────────────────────────
// GET /admin/llm/benchmark
// ──────────────────────────────────────────────────────────────────────────

func (s *Server) handleAdminLLMBenchmarkList(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET", "OPTIONS")
		return
	}
	if s.bank == nil {
		http.Error(w, "bank disabled", http.StatusServiceUnavailable)
		return
	}
	rows, err := s.bank.ListProviderBenchmarks()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"records": rows,
	})
}

// ──────────────────────────────────────────────────────────────────────────
// GET /admin/llm/estimate_nightly
// ──────────────────────────────────────────────────────────────────────────

// NightlyEstimateBasis is the count of recent runs averaged for the
// estimate. 5 balances "recent enough to reflect current bank size"
// against "enough samples to smooth out one-off outliers".
const NightlyEstimateBasis = 5

// NightlyEstimatePhase is one row in the per-phase breakdown response.
type NightlyEstimatePhase struct {
	Phase    string  `json:"phase"`
	Tokens   int     `json:"tokens"`
	Seconds  float64 `json:"seconds"`
	SharePct float64 `json:"share_pct"`
}

// handleAdminLLMEstimateNightly serves GET /admin/llm/estimate_nightly.
//
// Walks the last N nightly runs, averages their per-phase token spend,
// divides by the Tier 2 provider's cached benchmark rate, and returns a
// human-readable total + breakdown. When no benchmark exists for the
// configured Tier 2 provider, the response carries an actionable
// `caveats` entry pointing at /admin/llm/benchmark/tier2_provider.
func (s *Server) handleAdminLLMEstimateNightly(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET", "OPTIONS")
		return
	}
	if s.bank == nil {
		http.Error(w, "bank disabled", http.StatusServiceUnavailable)
		return
	}

	// Resolve the Tier 2 provider config + cached benchmark rate.
	tier2cfg, _ := s.bank.GetProviderConfig(Tier2)
	compression := ""
	if tier2cfg.Kind == ProviderAirLLM {
		if v, ok, _ := s.bank.GetSetting("airllm." + string(Tier2) + ".compression"); ok {
			compression = v
		}
	}
	var tokensPerSec float64
	var benchAt string
	if tier2cfg.Kind != "" && tier2cfg.Kind != ProviderDisabled && tier2cfg.Model != "" {
		if rec, ok, _ := s.bank.GetProviderBenchmark(string(tier2cfg.Kind), tier2cfg.Model, compression); ok {
			tokensPerSec = rec.TokensPerSec
			benchAt = rec.BenchmarkedAt
		}
	}

	// Walk recent nightly runs + accumulate per-phase token spend.
	runs, _ := s.bank.ListNightlyRuns(NightlyRunListOpts{Limit: NightlyEstimateBasis})
	used := 0
	var tokensPhase0b int
	var tokensAugment int
	var tokensTotal int
	for _, r := range runs {
		if r.StatsJSON == "" {
			continue
		}
		stats, ok := parseStatsJSON(r.StatsJSON)
		if !ok {
			continue
		}
		used++
		tokensTotal += stats.TokensTotal
		if stats.TokensTotal == 0 {
			// Fall back to in/out sum when older runs only populated those.
			tokensTotal += stats.TokensIn + stats.TokensOut
		}
		if stats.DeepEncoding != nil {
			tokensPhase0b += stats.DeepEncoding.TokensIn + stats.DeepEncoding.TokensOut
		}
		tokensAugment += stats.AugmentedTokens
	}

	caveats := []string{}
	if used == 0 {
		caveats = append(caveats, "No completed nightly runs yet. Trigger one (POST /nightly/trigger) so we have data to estimate against.")
	}
	if tokensPerSec == 0 {
		caveats = append(caveats,
			"No benchmark cached for the configured Tier 2 provider. Run POST /admin/llm/benchmark/tier2_provider first.")
	} else {
		caveats = append(caveats,
			fmt.Sprintf("Estimate based on last %d nightly runs and a benchmarked rate of %.2f tok/s. Real wall-clock will vary with concurrency + model load time.", used, tokensPerSec))
	}

	// Per-run averages.
	var avgTotal, avgPhase0b, avgAugment int
	if used > 0 {
		avgTotal = tokensTotal / used
		avgPhase0b = tokensPhase0b / used
		avgAugment = tokensAugment / used
	}
	avgOther := avgTotal - avgPhase0b - avgAugment
	if avgOther < 0 {
		avgOther = 0
	}

	perPhase := []NightlyEstimatePhase{}
	estimatedTotalSeconds := 0.0
	if tokensPerSec > 0 && used > 0 {
		mk := func(phase string, tokens int) NightlyEstimatePhase {
			sec := float64(tokens) / tokensPerSec
			share := 0.0
			if avgTotal > 0 {
				share = (float64(tokens) / float64(avgTotal)) * 100.0
			}
			return NightlyEstimatePhase{
				Phase:    phase,
				Tokens:   tokens,
				Seconds:  sec,
				SharePct: share,
			}
		}
		if avgPhase0b > 0 {
			perPhase = append(perPhase, mk("0b_deep_encode", avgPhase0b))
		}
		if avgAugment > 0 {
			perPhase = append(perPhase, mk("6_augment", avgAugment))
		}
		if avgOther > 0 {
			perPhase = append(perPhase, mk("other_phases", avgOther))
		}
		estimatedTotalSeconds = float64(avgTotal) / tokensPerSec
	}

	resp := map[string]any{
		"tier2_provider":          string(tier2cfg.Kind),
		"tier2_model":             tier2cfg.Model,
		"compression":             compression,
		"tokens_per_sec":          nullIfZero(tokensPerSec),
		"benchmarked_at":          benchAt,
		"based_on_runs":           used,
		"avg_tokens_per_run":      avgTotal,
		"estimated_total_seconds": estimatedTotalSeconds,
		"estimated_human":         humaniseDuration(estimatedTotalSeconds),
		"per_phase":               perPhase,
		"caveats":                 caveats,
	}
	writeJSON(w, http.StatusOK, resp)
}

// parseStatsJSON unmarshals the persisted stats_json string into the
// NightlyStats shape. Returns ok=false on parse failure so we can skip
// pathological rows without aborting the whole estimate.
func parseStatsJSON(raw string) (NightlyStats, bool) {
	var stats NightlyStats
	if err := json.Unmarshal([]byte(raw), &stats); err != nil {
		return stats, false
	}
	return stats, true
}

// nullIfZero is a tiny helper: returns nil for a 0 rate so the JSON
// response carries `null` rather than `0` — explicit "no measurement"
// signal for the frontend.
func nullIfZero(v float64) any {
	if v == 0 {
		return nil
	}
	return v
}

// humaniseDuration renders seconds as "4h 12min" / "23 min" / "1d 2h".
// Returns "" when v <= 0 so the frontend can render its own dash.
func humaniseDuration(secs float64) string {
	if secs <= 0 {
		return ""
	}
	total := int(secs + 0.5)
	days := total / 86400
	hours := (total % 86400) / 3600
	minutes := (total % 3600) / 60
	switch {
	case days > 0 && hours > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case days > 0:
		return fmt.Sprintf("%dd", days)
	case hours > 0 && minutes > 0:
		return fmt.Sprintf("%dh %dmin", hours, minutes)
	case hours > 0:
		return fmt.Sprintf("%dh", hours)
	case minutes > 0:
		return fmt.Sprintf("%dmin", minutes)
	default:
		return fmt.Sprintf("%ds", total)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Auto-benchmark trigger
// ──────────────────────────────────────────────────────────────────────────

// maybeAutoBenchmark fires a goroutine that benchmarks (kind, model,
// compression) iff no cached record younger than recentBenchmarkWindow
// exists. Caller is fire-and-forget; errors land in the audit log so the
// user can see why the auto-trigger didn't update the cache.
//
// Wired from handleAdminAirLLMConfigureTier (and other "set active
// provider" paths in the future) so the Dream Journal's estimate is
// ready by the time the user navigates there.
func (s *Server) maybeAutoBenchmark(tier TierKey, kind, model, compression string) {
	if s == nil || s.bank == nil || model == "" {
		return
	}
	// Skip when a recent record already exists.
	if rec, ok, _ := s.bank.GetProviderBenchmark(kind, model, compression); ok {
		if t, err := time.Parse(time.RFC3339Nano, rec.BenchmarkedAt); err == nil {
			if time.Since(t) < recentBenchmarkWindow {
				return
			}
		}
	}
	go func() {
		// Background ctx — the HTTP request's ctx is already gone by the
		// time this fires. The benchmarkCallTimeout inside
		// runProviderBenchmark caps the call.
		ctx := context.Background()
		rec, err := s.runProviderBenchmark(ctx, tier, "", model)
		if err != nil {
			s.auditWrite(AuditEntry{
				Operation:  "llm_benchmark_auto_failed",
				EntityType: "provider",
				EntityID:   string(tier),
				AfterJSON: mustJSON(map[string]any{
					"provider_kind": kind,
					"model":         model,
					"compression":   compression,
					"error":         err.Error(),
				}),
				Reason:    "auto-benchmark on configure_tier failed",
				AdapterID: "sd-core-llm",
			})
			return
		}
		if err := s.bank.UpsertProviderBenchmark(rec); err != nil {
			return
		}
		s.auditWrite(AuditEntry{
			Operation:  "llm_benchmark_auto",
			EntityType: "provider",
			EntityID:   string(tier),
			AfterJSON: mustJSON(map[string]any{
				"provider_kind":  rec.ProviderKind,
				"model":          rec.Model,
				"compression":    rec.Compression,
				"tokens_per_sec": rec.TokensPerSec,
				"elapsed_ms":     rec.ElapsedMS,
			}),
			Reason:    "auto-benchmark on configure_tier",
			AdapterID: "sd-core-llm",
		})
	}()
}

