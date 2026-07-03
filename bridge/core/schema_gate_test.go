// schema_gate_test.go — guards for the Phase-8 schema vacuousness gate
// (isVacuousSchema / classifySchema in nightly_pipeline.go).
//
// Background: nightly "dream" Phase 8 asks a weak Tier-2 model to synthesize a
// "schema" memory from a cluster of summaries. The model habitually emits
// vacuous process prose ("the project utilized Bash commands to enhance
// functionality") and ignores the SKIP instruction. The gate is
// positive-anchor-first: keep a schema only if it names something concrete and
// lookup-able (file, path, identifier, issue ref, sha, or known product noun),
// after dropping the SKIP sentinel and a filler-phrase denylist.
//
// These cases pin three failure classes the reviewers cared about:
//   - vacuous prose must DROP (the real post-fix bank samples),
//   - false positives must DROP (brand-noun filler, anchor-stuffing),
//   - false negatives must KEEP (genuinely-concrete schemas, including
//     abstract-but-useful decision schemas that name a real artifact).
package main

import "testing"

func TestIsVacuousSchema(t *testing.T) {
	cases := []struct {
		name       string
		text       string
		wantDrop   bool
		wantReason schemaDropReason
	}{
		// ── SKIP sentinel ────────────────────────────────────────────────
		{"skip_bare", "SKIP", true, schemaDropSkip},
		{"skip_punct", "skip.", true, schemaDropSkip},
		{"skip_prefix", "SKIP - nothing concrete in common", true, schemaDropSkip},
		{"empty", "   ", true, schemaDropSkip},

		// ── Real vacuous bank samples (must DROP) ────────────────────────
		{
			"vacuous_bash_read_grep",
			"The project consistently utilized Bash commands, particularly Read and Grep, to enhance document structure and functionality.",
			true, schemaDropNoAnchor,
		},
		{
			"vacuous_config_settings",
			"The project involved extensive use of Bash commands for editing configuration files, enhancing functionality, and managing system settings.",
			true, schemaDropFiller, // "enhancing functionality" / "managing system settings"
		},
		{
			"vacuous_modular_iterative",
			"The summaries reflect a structured approach to software development characterized by modular design and iterative improvement.",
			true, schemaDropFiller,
		},

		// ── Brand-noun false positives (must DROP) ───────────────────────
		// camelHump brand nouns must NOT count as concrete anchors.
		{"brand_typescript", "The summaries describe a TypeScript project with consistent tooling.", true, schemaDropNoAnchor},
		{"brand_javascript", "These memories revolve around a JavaScript codebase and its general structure.", true, schemaDropNoAnchor},
		{"brand_postgresql", "A recurring theme is the PostgreSQL database backing the application.", true, schemaDropNoAnchor},
		{"brand_websockets_graphql", "The work centers on WebSockets and GraphQL for client communication.", true, schemaDropNoAnchor},
		{"brand_macos_oauth", "The project targets macOS and uses OAuth for authentication broadly.", true, schemaDropNoAnchor},

		// ── Anchor-stuffing false positive (must DROP) ───────────────────
		// A filename token glued onto pure process filler still dies — the
		// filler denylist fires before the anchor scan.
		{
			"stuffed_filename_filler",
			"The cluster used forward.js to improve functionality across the project.",
			true, schemaDropFiller,
		},

		// ── Genuinely concrete schemas (must KEEP) ───────────────────────
		{
			"good_forward_js",
			"These summaries all revolve around the forward.js hook bridge — three add MCP op->brain-pulse mappings; one fixes errorSignature's where/error_message read.",
			false, schemaKeep,
		},
		{
			"good_schema_max_per_run",
			"The cluster centers on the SchemaMaxPerRun cap in nightly_pipeline.go: replay clusters share the same budget so a flood of replay clusters can't bypass it (#52).",
			false, schemaKeep,
		},
		{
			"good_cloudflared_product",
			"Recurring decision: the tunnel ingress regex in the system profile cloudflared config must mirror every new SD Core path family before remote smoketests pass.",
			false, schemaKeep, // "cloudflared" product noun
		},
		{
			"good_path_anchor",
			"Two summaries fix the same bug where memory writes hit the memories endpoint instead of /bank/memories, dropping success on map merge.",
			false, schemaKeep, // "/bank/memories" path
		},

		// ── Abstract-but-useful DECISION schemas that name an artifact ───
		// (false-negative class the efficacy reviewer flagged) — KEEP.
		{
			"good_decision_caching",
			"The team repeatedly chose caching over recomputation in recall_cross_encoder.go to keep the hot path fast.",
			false, schemaKeep, // snake_case file/identifier anchor
		},
		{
			"good_decision_anchor_over_blocklist",
			"A recurring design stance: favor positive anchors over blocklists in isVacuousSchema, since the weak model just rephrases filler.",
			false, schemaKeep, // "isVacuousSchema" camelHump
		},
		{
			"good_product_ollama",
			"Across nights the Tier-2 model defaults to ollama for synthesis when no remote provider is configured.",
			false, schemaKeep, // "ollama" product noun
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotReason := classifySchema(tc.text)
			gotDrop := gotReason != schemaKeep
			if gotDrop != tc.wantDrop {
				t.Fatalf("isVacuousSchema=%v, want %v (reason=%q)\n  text: %s",
					gotDrop, tc.wantDrop, gotReason, tc.text)
			}
			if gotReason != tc.wantReason {
				t.Errorf("reason=%q, want %q\n  text: %s", gotReason, tc.wantReason, tc.text)
			}
			// isVacuousSchema must agree with classifySchema.
			if isVacuousSchema(tc.text) != gotDrop {
				t.Errorf("isVacuousSchema disagrees with classifySchema for %q", tc.name)
			}
		})
	}
}
