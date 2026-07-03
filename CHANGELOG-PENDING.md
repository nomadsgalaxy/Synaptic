# Pending changelog

Edit as you ship work; on next `tools/build_release.py --zip`,
this file gets promoted to `synaptic-<version>-NOTES.md` next to
the ZIP and reset to this stub.

---

## What changed since v3.1.0b2

- **Privacy: formalized the Tier 0 sensitivity classifier + made the LLM pass
  optional.** Tier 0 is the always-on, deterministic FIRST pass (regex +
  Luhn/IBAN-mod-97 checksums, no model, no network, sub-millisecond) that runs
  synchronously in every `SaveMemory`/`UpdateMemory` — so keys/cards/SSNs are
  flagged the instant they're written, even if the LLM is offline. Extended its
  pattern set: GitLab/npm/HuggingFace/DigitalOcean/Mailgun tokens, GCP
  service-account emails, Azure storage keys, **connection-string credentials**
  (`scheme://user:pass@`), and **IBAN** (mod-97 validated) — all with
  false-positive guards (verified live: a `postgres://…` credential flagged
  sensitive on save with no LLM). The optional Tier 1/2 LLM second pass (prose
  cases regex can't catch) is now toggleable live via
  `sensitive_llm_classifier_enabled` (default ON; off → Tier 0 alone) and is
  hard-guaranteed **local-only** — it can only ever use Ollama Tier 1/2, never
  the Tier 3 Oracle, so memory text is never sent off-box to be classified.

- **Fixed: consolidation data-integrity bug — 52% of the bank was
  "live-but-merged."** A bulk restore (`POST /…/restore` looped ~2,601× in
  30s on 2026-05-23) un-tombstoned dedup losers, but `UndeleteMemory`
  cleared `deleted_at` while leaving `deleted_reason` set — producing 2,601
  live rows still marked `"merged into <id>"`. Compounded by an earlier
  degenerate embedding model that let one survivor absorb 150–248
  topically-unrelated losers (e.g. 248 design memories "merged into" an
  OAuth error). Fixes:
  - `UndeleteMemory` now clears `deleted_reason` atomically with
    `deleted_at` (enforces the invariant: no live row carries a deletion
    reason).
  - Nightly Phase 1 dedup gained a Jaccard tag-overlap guard
    (`dedup_tag_overlap_pct`, default 50) mirroring the inline path, so
    cross-topic merges are structurally impossible even under a degenerate
    embedding model.
  - One-off repair endpoint (`POST /admin/repair/orphaned-merges`,
    dry-run-defaulted) cleared the stale `deleted_reason` on all 2,601
    orphans. No memories were lost — losers retained full text.
  - Embedding model-keying (vectors keyed on `text_hash` alone, so a model
    swap silently cross-wires) identified and deferred with a migration
    plan — too risky to ship alongside the urgent fixes.

- **Fixed: recall leaked soft-deleted memories.** `AllEmbeddingsForMemories`
  (the cosine candidate query) had no `deleted_at` filter, so tombstoned
  memories with cached embeddings were scored and returned on the cosine
  path (BM25 already filtered, but RRF re-unioned them). Added
  `WHERE deleted_at = ''` — index-backed, fixes a coverage-math under-report
  as a bonus.

- **Fixed: `sd_recall` MCP timeout** (continued from b2) — adapter `/recall`
  + `/lexicon/rebuild` were on the 1500ms default while recall takes
  2.7–11s; given 30s / 600s timeouts.
