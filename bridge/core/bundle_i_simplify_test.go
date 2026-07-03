// bundle_i_simplify_test.go — guards for the query-simplification
// lesson learned from the Bundle I smoke benchmark.
//
// Before: query "What does TTL do in Bundle L?" produced an embedding
// clustered near profile-identity memories; the topically-relevant
// memory ("Bundle L pivot: TTL fires dormancy…") didn't surface in
// the top 100 results. BM25 RRF didn't compensate because FTS5 was
// ranking documents containing the question-word stop-set above the
// answer. Score on this row: 0/1.
//
// After: simplifyQueryForEmbedding strips the question-word
// scaffolding before the query reaches the embedder AND the BM25
// path. Both signals now agree on the keyword content. Score on the
// same row: 1/1 (took the smoke benchmark from 4/5 to 5/5).
//
// These tests pin the helper's behaviour so the next refactor can't
// silently regress the benchmark.
package main

import (
	"strings"
	"testing"
)

func TestBundleI_Simplify_StripsInterrogatives(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"What does TTL do in Bundle L?", "TTL Bundle L"},
		{"How do I rebuild the embed container?", "rebuild embed container"},
		{"Where is the v2.7 release script?", "v2.7 release script"},
		{"Who is AC?", "AC"},
		{"What is the pour-over ratio?", "pour-over ratio"},
		// Already keyword-only: passes through unchanged
		{"OAuth refresh tokens", "OAuth refresh tokens"},
		// Trailing punctuation on content tokens is preserved as part
		// of the token if it's mid-word (e.g., 'v1.5') but stripped at
		// the very end.
		{"What runs nomic-embed-text:v1.5?", "runs nomic-embed-text:v1.5"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := simplifyQueryForEmbedding(c.in)
			if got != c.want {
				t.Errorf("simplifyQueryForEmbedding(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestBundleI_Simplify_PreservesSingleLetterTokens(t *testing.T) {
	// "Bundle L" is a meaningful token pair — the single 'L' must
	// survive the stop-list. Previously a more aggressive stop-list
	// would have dropped it, breaking the original benchmark miss.
	got := simplifyQueryForEmbedding("What does Bundle L mean?")
	if !strings.Contains(got, "L") {
		t.Errorf("expected single-letter 'L' to survive, got %q", got)
	}
}

func TestBundleI_Simplify_AllStopWordsFallsBackToRaw(t *testing.T) {
	// A query that's nothing BUT stop-words should not produce empty
	// input to the embedder (would 4xx the call). Fall back to the
	// raw query in that pathological case.
	raw := "what does it do?"
	got := simplifyQueryForEmbedding(raw)
	if got != raw {
		t.Errorf("all-stop-words should fall back to raw query; got %q", got)
	}
}

func TestBundleI_QuerySimplify_DefaultsOn(t *testing.T) {
	bank := newTestBank(t)
	if !querySimplifyEnabled(bank) {
		t.Errorf("query simplification should default ON")
	}
	if !querySimplifyEnabled(nil) {
		t.Errorf("nil bank should default ON too")
	}
}

func TestBundleI_QuerySimplify_HonoursOffSetting(t *testing.T) {
	bank := newTestBank(t)
	for _, val := range []string{"0", "false", "off", "no", "FALSE"} {
		bank.SetSetting(QuerySimplifyEnabledKey, val)
		if querySimplifyEnabled(bank) {
			t.Errorf("value %q should disable simplification", val)
		}
	}
	bank.SetSetting(QuerySimplifyEnabledKey, "1")
	if !querySimplifyEnabled(bank) {
		t.Errorf(`"1" should re-enable`)
	}
}
