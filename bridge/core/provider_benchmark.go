// provider_benchmark.go — Wave 8e provider benchmark cache.
//
// Stores the most-recent tokens-per-sec measurement for every
// (provider_kind, model, compression) tuple SD Core has benchmarked.
// Used by the Dream Journal's "your next nightly will take ~Xh"
// estimate, where the multiplier is dominated by Tier 2's throughput
// (AirLLM-class providers run 1-3 tok/s on consumer CPUs — the
// difference between 10-min and 4-hour nightlies).
//
// One row per tuple, REPLACE on conflict — we only care about the
// current rate, not the history of measurements. Long history would
// add bytes without supporting any estimator that uses it.
//
// compression is "" for providers that don't have a compression flag
// (Ollama, OpenAI, Anthropic). AirLLM stores 4bit/8bit/none.
package main

import (
	"errors"
	"time"
)

// ProviderBenchmark is one cached benchmark measurement. The composite
// (ProviderKind, Model, Compression) uniquely identifies a tuple; tuples
// have at most one row in the table.
type ProviderBenchmark struct {
	ProviderKind     string  `json:"provider_kind"`
	Model            string  `json:"model"`
	Compression      string  `json:"compression"`
	ElapsedMS        int64   `json:"elapsed_ms"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TokensPerSec     float64 `json:"tokens_per_sec"`
	SampleExcerpt    string  `json:"sample_excerpt,omitempty"`
	BenchmarkedAt    string  `json:"benchmarked_at"`
}

// UpsertProviderBenchmark replaces the row for (provider_kind, model,
// compression) with the supplied record. The provider tuple keys the
// table; re-benchmarking overwrites the prior measurement.
func (b *Bank) UpsertProviderBenchmark(rec ProviderBenchmark) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	if rec.ProviderKind == "" || rec.Model == "" {
		return errors.New("provider_kind and model are required")
	}
	if rec.BenchmarkedAt == "" {
		rec.BenchmarkedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_, err := b.db.Exec(`
INSERT INTO provider_benchmark (
    provider_kind, model, compression,
    elapsed_ms, prompt_tokens, completion_tokens, tokens_per_sec,
    sample_excerpt, benchmarked_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(provider_kind, model, compression) DO UPDATE SET
    elapsed_ms        = excluded.elapsed_ms,
    prompt_tokens     = excluded.prompt_tokens,
    completion_tokens = excluded.completion_tokens,
    tokens_per_sec    = excluded.tokens_per_sec,
    sample_excerpt    = excluded.sample_excerpt,
    benchmarked_at    = excluded.benchmarked_at`,
		rec.ProviderKind, rec.Model, rec.Compression,
		rec.ElapsedMS, rec.PromptTokens, rec.CompletionTokens, rec.TokensPerSec,
		rec.SampleExcerpt, rec.BenchmarkedAt,
	)
	return err
}

// GetProviderBenchmark returns the cached record for the exact
// (kind, model, compression) tuple. ok=false (no error) when no row.
func (b *Bank) GetProviderBenchmark(kind, model, compression string) (ProviderBenchmark, bool, error) {
	var rec ProviderBenchmark
	if b == nil {
		return rec, false, errors.New("bank not enabled")
	}
	err := b.db.QueryRow(`
SELECT provider_kind, model, compression,
       elapsed_ms, prompt_tokens, completion_tokens, tokens_per_sec,
       sample_excerpt, benchmarked_at
FROM provider_benchmark
WHERE provider_kind = ? AND model = ? AND compression = ?`,
		kind, model, compression,
	).Scan(&rec.ProviderKind, &rec.Model, &rec.Compression,
		&rec.ElapsedMS, &rec.PromptTokens, &rec.CompletionTokens, &rec.TokensPerSec,
		&rec.SampleExcerpt, &rec.BenchmarkedAt)
	if err != nil {
		// sql.ErrNoRows surfaces as Scan error; normalise to (zero, false, nil).
		if err.Error() == "sql: no rows in result set" {
			return rec, false, nil
		}
		return rec, false, err
	}
	return rec, true, nil
}

// ListProviderBenchmarks returns every cached benchmark row, newest first.
// Used by GET /admin/llm/benchmark + the auto-benchmark "do I need to run
// one?" gate (skip when the matching row is <24h old).
func (b *Bank) ListProviderBenchmarks() ([]ProviderBenchmark, error) {
	if b == nil {
		return nil, errors.New("bank not enabled")
	}
	rows, err := b.db.Query(`
SELECT provider_kind, model, compression,
       elapsed_ms, prompt_tokens, completion_tokens, tokens_per_sec,
       sample_excerpt, benchmarked_at
FROM provider_benchmark
ORDER BY benchmarked_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProviderBenchmark{}
	for rows.Next() {
		var rec ProviderBenchmark
		if err := rows.Scan(&rec.ProviderKind, &rec.Model, &rec.Compression,
			&rec.ElapsedMS, &rec.PromptTokens, &rec.CompletionTokens, &rec.TokensPerSec,
			&rec.SampleExcerpt, &rec.BenchmarkedAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}
