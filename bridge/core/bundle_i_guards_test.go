//go:build longmemeval
// +build longmemeval

// bundle_i_guards_test.go — guards for v2.7 Bundle I (LongMemEval
// harness scaffolding).
//
// QUARANTINED behind the `longmemeval` build tag (2026-06-03): the
// non-test LongMemEval harness implementation (LongMemEvalTurn,
// LongMemEvalRow, QASystem, scoreAnswer, RunLongMemEval,
// LoadLongMemEvalJSONL) was removed from the working tree, leaving this
// file referencing undefined symbols and breaking the default `go test`
// for the whole package. Tagging it out restores the package suite.
// Restore the harness file (or run `go test -tags longmemeval`) to
// re-enable these guards. Only this file referenced the missing symbols;
// the sibling bundle_i_*_test.go files test other features and stay in
// the default build.
//
// We don't ship the real LongMemEval dataset (multi-GB, third-party
// licensed), so these tests exercise the harness with synthetic rows.
// The integration test `TestLongMemEval_RealDataset` lives under build
// tag `longmemeval` and is run by the user against the upstream
// tarball — see longmemeval_harness.go for instructions.
//
// Coverage:
//   - LoadLongMemEvalJSONL parses well-formed input
//   - LoadLongMemEvalJSONL rejects rows missing question or gold_answer
//   - scoreAnswer credits exact, alias, and substring matches
//   - scoreAnswer is case-insensitive
//   - scoreAnswer rejects empty predictions
//   - RunLongMemEval drives a QASystem stub end-to-end and aggregates
//     accuracy correctly
//   - RunLongMemEval calls Reset/SeedHistory/Ask in the right order
//   - RunLongMemEval fails fast on a system error
package main

import (
	"errors"
	"strings"
	"testing"
)

// ── scoreAnswer ─────────────────────────────────────────────────────────

func TestBundleI_ScoreAnswer_ExactMatch(t *testing.T) {
	ok, kind := scoreAnswer("Paris", "Paris", nil)
	if !ok || kind != "exact" {
		t.Errorf("exact match should be exact, got (%v, %q)", ok, kind)
	}
}

func TestBundleI_ScoreAnswer_CaseInsensitive(t *testing.T) {
	ok, _ := scoreAnswer("PARIS", "paris", nil)
	if !ok {
		t.Errorf("case-insensitive exact match should win")
	}
}

func TestBundleI_ScoreAnswer_AliasMatch(t *testing.T) {
	ok, kind := scoreAnswer("Anthropic", "anthropic", []string{"Anthropic PBC", "Anthropic Inc"})
	// "anthropic" == "anthropic" → exact (alias check is for when pred
	// matches an acceptable variant verbatim).
	if !ok || kind != "exact" {
		t.Errorf("expected exact for canonical match, got (%v, %q)", ok, kind)
	}
	ok, kind = scoreAnswer("anthropic pbc", "Anthropic", []string{"Anthropic PBC"})
	if !ok || kind != "alias" {
		t.Errorf("expected alias for variant match, got (%v, %q)", ok, kind)
	}
}

func TestBundleI_ScoreAnswer_SubstringMatch(t *testing.T) {
	// LLMs tend to wrap answers in chatter — substring catches it.
	ok, kind := scoreAnswer("The answer is Paris, capital of France.", "Paris", nil)
	if !ok || kind != "substring" {
		t.Errorf("expected substring credit, got (%v, %q)", ok, kind)
	}
}

func TestBundleI_ScoreAnswer_EmptyFails(t *testing.T) {
	if ok, _ := scoreAnswer("", "Paris", nil); ok {
		t.Errorf("empty prediction should not score")
	}
	if ok, _ := scoreAnswer("   ", "Paris", nil); ok {
		t.Errorf("whitespace-only prediction should not score")
	}
}

func TestBundleI_ScoreAnswer_NoMatchReturnsNone(t *testing.T) {
	ok, kind := scoreAnswer("Berlin", "Paris", nil)
	if ok {
		t.Errorf("wrong answer should not score")
	}
	if kind != "none" {
		t.Errorf("expected match_kind=none on miss, got %q", kind)
	}
}

// ── JSONL loader ────────────────────────────────────────────────────────

func TestBundleI_LoadJSONL_ParsesValidRows(t *testing.T) {
	src := `{"id":"r1","question":"Q1?","gold_answer":"A1","history":[{"role":"user","content":"hi"}]}
{"id":"r2","question":"Q2?","gold_answer":"A2"}`
	rows, err := LoadLongMemEvalJSONL(strings.NewReader(src))
	if err != nil {
		t.Fatalf("LoadJSONL: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("expected 2 rows, got %d", len(rows))
	}
	if rows[0].Question != "Q1?" || rows[1].GoldAnswer != "A2" {
		t.Errorf("unexpected row contents: %+v", rows)
	}
}

func TestBundleI_LoadJSONL_RejectsMissingFields(t *testing.T) {
	src := `{"id":"r1","question":"Q1?"}` // missing gold_answer
	if _, err := LoadLongMemEvalJSONL(strings.NewReader(src)); err == nil {
		t.Errorf("expected error on row missing gold_answer")
	}
}

func TestBundleI_LoadJSONL_RejectsEmptyInput(t *testing.T) {
	if _, err := LoadLongMemEvalJSONL(strings.NewReader("")); err == nil {
		t.Errorf("expected error on empty input")
	}
}

// ── RunLongMemEval orchestration ────────────────────────────────────────

// fakeQASystem records call order so the test can verify
// Reset→SeedHistory→Ask sequencing.
type fakeQASystem struct {
	seeded    map[string][]LongMemEvalTurn
	calls     []string                                // "reset"|"seed:r1"|"ask:r1"
	answers   map[string]string                       // rowID → prediction
	errOnAsk  string                                  // rowID that should make Ask error
}

func newFakeQASystem(answers map[string]string) *fakeQASystem {
	return &fakeQASystem{
		seeded:  map[string][]LongMemEvalTurn{},
		answers: answers,
	}
}

func (f *fakeQASystem) Reset() error {
	f.calls = append(f.calls, "reset")
	f.seeded = map[string][]LongMemEvalTurn{}
	return nil
}

func (f *fakeQASystem) SeedHistory(rowID string, history []LongMemEvalTurn) error {
	f.calls = append(f.calls, "seed:"+rowID)
	f.seeded[rowID] = history
	return nil
}

func (f *fakeQASystem) Ask(rowID, q string) (string, error) {
	f.calls = append(f.calls, "ask:"+rowID)
	if rowID == f.errOnAsk {
		return "", errors.New("simulated provider failure")
	}
	return f.answers[rowID], nil
}

func TestBundleI_RunLongMemEval_AggregatesAccuracy(t *testing.T) {
	rows := []LongMemEvalRow{
		{ID: "r1", Question: "?", GoldAnswer: "alpha"},
		{ID: "r2", Question: "?", GoldAnswer: "beta"},
		{ID: "r3", Question: "?", GoldAnswer: "gamma"},
		{ID: "r4", Question: "?", GoldAnswer: "delta"},
	}
	sys := newFakeQASystem(map[string]string{
		"r1": "alpha",                 // exact
		"r2": "the answer is beta",    // substring
		"r3": "WRONG",                 // miss
		"r4": "delta",                 // exact
	})
	score, err := RunLongMemEval(rows, sys)
	if err != nil {
		t.Fatalf("RunLongMemEval: %v", err)
	}
	if score.Total != 4 {
		t.Errorf("expected total=4, got %d", score.Total)
	}
	if score.Correct != 3 {
		t.Errorf("expected correct=3, got %d", score.Correct)
	}
	if score.Accuracy < 0.74 || score.Accuracy > 0.76 {
		t.Errorf("expected accuracy ~0.75, got %v", score.Accuracy)
	}
}

func TestBundleI_RunLongMemEval_CallsResetSeedAskInOrder(t *testing.T) {
	rows := []LongMemEvalRow{
		{ID: "r1", Question: "?", GoldAnswer: "a"},
		{ID: "r2", Question: "?", GoldAnswer: "b"},
	}
	sys := newFakeQASystem(map[string]string{"r1": "a", "r2": "b"})
	if _, err := RunLongMemEval(rows, sys); err != nil {
		t.Fatalf("RunLongMemEval: %v", err)
	}
	want := []string{"reset", "seed:r1", "ask:r1", "reset", "seed:r2", "ask:r2"}
	if len(sys.calls) != len(want) {
		t.Fatalf("call count: want %d, got %d (%v)", len(want), len(sys.calls), sys.calls)
	}
	for i, w := range want {
		if sys.calls[i] != w {
			t.Errorf("call %d: want %q, got %q", i, w, sys.calls[i])
		}
	}
}

func TestBundleI_RunLongMemEval_FailsFastOnAskError(t *testing.T) {
	rows := []LongMemEvalRow{
		{ID: "r1", Question: "?", GoldAnswer: "a"},
		{ID: "r2", Question: "?", GoldAnswer: "b"},
		{ID: "r3", Question: "?", GoldAnswer: "c"},
	}
	sys := newFakeQASystem(map[string]string{"r1": "a", "r3": "c"})
	sys.errOnAsk = "r2"
	score, err := RunLongMemEval(rows, sys)
	if err == nil {
		t.Errorf("expected fail-fast error on r2, got nil")
	}
	// We scored r1 (correct) before hitting the error.
	if score.Correct != 1 {
		t.Errorf("expected 1 correct row before error, got %d", score.Correct)
	}
}
