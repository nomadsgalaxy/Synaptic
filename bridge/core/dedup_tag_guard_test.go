// dedup_tag_guard_test.go — invariants for the dedup-integrity hardening
// (dedup-integrity-team, 2026-06-14).
//
// Covers two guarantees:
//
//  1. Phase 1 dedup will NOT merge two topically-unrelated memories that
//     merely share a functional region group. This is the structural fix
//     for the over-broad-matching incident where a degenerate embedding
//     model pushed cosine over threshold for unrelated pairs; the
//     tag-overlap (Jaccard) guard blocks them even when the vectors lie.
//
//  2. Tombstone / resurrection invariant: a merged loser carries BOTH
//     deleted_at AND deleted_reason, and after UndeleteMemory the row is
//     live again with deleted_reason cleared — so no LIVE row ever carries
//     a stale deleted_reason.
package main

import "testing"

// seedDedupEmbedding stores a vector under the exact text_hash key that
// loadEmbeddedMemories → AllEmbeddingsForMemories recomputes for a memory,
// so Phase 1 dedup can pair it. The model label is cosmetic: GetEmbedding
// keys on text_hash alone. It also stamps deep_encoded_at, because
// loadEmbeddedMemories gates on deep-encoded rows (requireDeepEncoded=true).
func seedDedupEmbedding(t *testing.T, bank *Bank, rec MemoryRecord, vec []float32) {
	t.Helper()
	tagsAny := make([]interface{}, len(rec.Tags))
	for i, tg := range rec.Tags {
		tagsAny[i] = tg
	}
	embedText := synapseEmbedText(map[string]interface{}{
		"text": rec.Text,
		"tags": tagsAny,
	})
	if err := bank.SaveEmbedding(synapseTextHash(embedText), vec, "llama3.2:3b"); err != nil {
		t.Fatalf("SaveEmbedding: %v", err)
	}
	// Phase 1 only considers deep-encoded memories.
	if _, err := bank.db.Exec(
		`UPDATE memories SET deep_encoded_at = ? WHERE id = ?`,
		"2026-05-01T00:00:00Z", rec.ID,
	); err != nil {
		t.Fatalf("stamp deep_encoded_at: %v", err)
	}
}

// TestPhase1Dedup_TagGuardBlocksCrossTopicMerge is the core regression for
// the incident: two unrelated memories in the same region group with
// IDENTICAL (degenerate) embedding vectors — cosine 1.0, far above
// threshold — must NOT merge, because their tag sets don't overlap.
func TestPhase1Dedup_TagGuardBlocksCrossTopicMerge(t *testing.T) {
	bank := newTestBank(t)

	// Same functional region group (frontal_lobe → "executive"), totally
	// different topics, ZERO tag overlap.
	oauth, err := bank.SaveMemory(MemoryRecord{
		Text:       "Error redirect_uri_mismatch indicates a wrong loopback URL was registered.",
		Tags:       []string{"oauth", "auth", "credentials"},
		RegionHint: "frontal_lobe",
	})
	if err != nil {
		t.Fatalf("SaveMemory oauth: %v", err)
	}
	gravity, err := bank.SaveMemory(MemoryRecord{
		Text:       "Gravity uses GitHub auth via octokit with personal access tokens.",
		Tags:       []string{"gravity", "ci", "packaging"},
		RegionHint: "frontal_lobe",
	})
	if err != nil {
		t.Fatalf("SaveMemory gravity: %v", err)
	}

	// Degenerate/identical vectors → cosine == 1.0 (the failure condition).
	vec := []float32{0.5, 0.5, 0.5, 0.5}
	seedDedupEmbedding(t, bank, oauth, vec)
	seedDedupEmbedding(t, bank, gravity, vec)

	pc := newPipelineContextForTest(t, bank)
	if err := pc.runPhase1Dedup(); err != nil {
		t.Fatalf("runPhase1Dedup: %v", err)
	}

	// Neither memory may have been merged away.
	for _, id := range []string{oauth.ID, gravity.ID} {
		got, gerr := bank.GetMemory(id)
		if gerr != nil {
			t.Fatalf("GetMemory %s: %v", id, gerr)
		}
		if got.DeletedAt != "" {
			t.Errorf("cross-topic merge happened: %s soft-deleted (reason=%q) despite zero tag overlap",
				id, got.DeletedReason)
		}
	}
	if pc.stats.Pruned != 0 {
		t.Errorf("expected 0 pruned, got %d", pc.stats.Pruned)
	}
}

// TestPhase1Dedup_TagGuardAllowsTrueDuplicate confirms the guard does NOT
// over-block: two memories with high tag overlap AND high cosine still
// merge, and the resulting tombstone is well-formed and reversible.
func TestPhase1Dedup_TagGuardAllowsTrueDuplicate(t *testing.T) {
	bank := newTestBank(t)

	a, err := bank.SaveMemory(MemoryRecord{
		Text:       "System re-derives lateralization on boot from memories.json; no migration needed.",
		Tags:       []string{"anatomy", "neuroscience", "configurable"},
		RegionHint: "frontal_lobe",
	})
	if err != nil {
		t.Fatalf("SaveMemory a: %v", err)
	}
	b, err := bank.SaveMemory(MemoryRecord{
		Text:       "Lateralization rules update safely on boot without data migration.",
		Tags:       []string{"anatomy", "neuroscience", "configurable"}, // identical tags → 100% overlap
		RegionHint: "frontal_lobe",
	})
	if err != nil {
		t.Fatalf("SaveMemory b: %v", err)
	}

	vec := []float32{0.5, 0.5, 0.5, 0.5} // cosine 1.0, above threshold
	seedDedupEmbedding(t, bank, a, vec)
	seedDedupEmbedding(t, bank, b, vec)

	pc := newPipelineContextForTest(t, bank)
	if err := pc.runPhase1Dedup(); err != nil {
		t.Fatalf("runPhase1Dedup: %v", err)
	}

	if pc.stats.Pruned != 1 {
		t.Fatalf("expected exactly 1 prune for a true duplicate, got %d", pc.stats.Pruned)
	}

	// Exactly one of the pair is the soft-deleted loser; assert the
	// tombstone invariant on it, then assert UndeleteMemory reverses it.
	var loserID, survivorID string
	for _, id := range []string{a.ID, b.ID} {
		got, _ := bank.GetMemory(id)
		if got.DeletedAt != "" {
			loserID = id
		} else {
			survivorID = id
		}
	}
	if loserID == "" || survivorID == "" {
		t.Fatalf("expected one survivor + one loser, got survivor=%q loser=%q", survivorID, loserID)
	}

	// (a) Tombstone invariant: loser has BOTH deleted_at AND deleted_reason.
	loser, _ := bank.GetMemory(loserID)
	if loser.DeletedAt == "" {
		t.Errorf("loser %s missing deleted_at", loserID)
	}
	if loser.DeletedReason == "" {
		t.Errorf("loser %s missing deleted_reason", loserID)
	}
	if want := "merged into " + survivorID; loser.DeletedReason != want {
		t.Errorf("loser deleted_reason = %q, want %q", loser.DeletedReason, want)
	}

	// (b) Resurrection invariant: UndeleteMemory makes the row live again
	// AND clears deleted_reason — no LIVE row may carry a deleted_reason.
	if err := bank.UndeleteMemory(loserID); err != nil {
		t.Fatalf("UndeleteMemory: %v", err)
	}
	revived, _ := bank.GetMemory(loserID)
	if revived.DeletedAt != "" {
		t.Errorf("after undelete, deleted_at should be cleared, got %q", revived.DeletedAt)
	}
	if revived.DeletedReason != "" {
		t.Errorf("after undelete, LIVE row still carries deleted_reason=%q", revived.DeletedReason)
	}
}
