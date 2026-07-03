// bundle_i_session_expand_test.go — guards for the session co-
// retrieval lesson learned from the LongMemEval Oracle baseline.
//
// Symptom: multi-session category scored 4/12 (33%) on the 50-row
// baseline. Questions like "How many days did I spend on camping
// trips?" need MULTIPLE turns from the same conversation summed up,
// but the top-K /recall pulled only the highest-cosine turn. The
// synthesizer answered with the single trip it saw, missing the
// rest.
//
// Ship: after fusion + dormancy + supersede, promote up to 3
// memories per top-3 session into the candidate set. Genuine user
// value: a question about a conversation gets the full conversation
// as context, not one isolated turn.
//
// Setting `recall_session_expand_enabled` (default ON).
package main

import (
	"testing"
)

func TestBundleI_SessionExpand_DefaultsOn(t *testing.T) {
	bank := newTestBank(t)
	if !sessionExpandEnabled(bank) {
		t.Errorf("session expand should default ON")
	}
	if !sessionExpandEnabled(nil) {
		t.Errorf("nil bank should default ON too")
	}
}

func TestBundleI_SessionExpand_HonoursOff(t *testing.T) {
	bank := newTestBank(t)
	for _, v := range []string{"0", "false", "off", "no"} {
		bank.SetSetting(SessionExpandEnabledKey, v)
		if sessionExpandEnabled(bank) {
			t.Errorf("value %q should disable session expand", v)
		}
	}
}

// ── MemoriesInSession ───────────────────────────────────────────────

func TestBundleI_MemoriesInSession_ChronologicalOrder(t *testing.T) {
	bank := newTestBank(t)
	// Three turns in one session, plus one turn in another (noise).
	t1, _ := bank.SaveMemory(MemoryRecord{
		Text: "turn 1", SessionID: "sess-A",
		CreatedAt: "2024-01-01T10:00:00Z",
	})
	t2, _ := bank.SaveMemory(MemoryRecord{
		Text: "turn 2", SessionID: "sess-A",
		CreatedAt: "2024-01-01T10:05:00Z",
	})
	t3, _ := bank.SaveMemory(MemoryRecord{
		Text: "turn 3", SessionID: "sess-A",
		CreatedAt: "2024-01-01T10:10:00Z",
	})
	bank.SaveMemory(MemoryRecord{
		Text: "noise", SessionID: "sess-B",
		CreatedAt: "2024-01-01T10:07:00Z",
	})

	got, err := bank.MemoriesInSession("sess-A", 10)
	if err != nil {
		t.Fatalf("MemoriesInSession: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(got))
	}
	want := []string{t1.ID, t2.ID, t3.ID}
	for i, w := range want {
		if got[i].ID != w {
			t.Errorf("turn %d: got %s, want %s", i, got[i].ID, w)
		}
	}
}

func TestBundleI_MemoriesInSession_ExcludesDeletedAndDormant(t *testing.T) {
	bank := newTestBank(t)
	live, _ := bank.SaveMemory(MemoryRecord{
		Text: "live", SessionID: "sess-X",
		CreatedAt: "2024-01-01T10:00:00Z",
	})
	deleted, _ := bank.SaveMemory(MemoryRecord{
		Text: "soft-deleted", SessionID: "sess-X",
		CreatedAt: "2024-01-01T10:01:00Z",
	})
	dormant, _ := bank.SaveMemory(MemoryRecord{
		Text: "dormant", SessionID: "sess-X",
		CreatedAt: "2024-01-01T10:02:00Z",
	})
	bank.SoftDeleteMemory(deleted.ID)
	bank.UpdateMemory(dormant.ID, MemoryUpdate{Dormant: ptrBool(true)})

	got, _ := bank.MemoriesInSession("sess-X", 10)
	if len(got) != 1 || got[0].ID != live.ID {
		t.Errorf("expected only the live memory, got %v", got)
	}
}

func TestBundleI_MemoriesInSession_BlankSessionReturnsEmpty(t *testing.T) {
	bank := newTestBank(t)
	bank.SaveMemory(MemoryRecord{Text: "x", SessionID: "sess-Y",
		CreatedAt: "2024-01-01T00:00:00Z"})
	got, _ := bank.MemoriesInSession("   ", 10)
	if len(got) != 0 {
		t.Errorf("blank session id should return nothing, got %v", got)
	}
}

func TestBundleI_MemoriesInSession_LimitClampsToReasonable(t *testing.T) {
	bank := newTestBank(t)
	for i := 0; i < 5; i++ {
		bank.SaveMemory(MemoryRecord{
			Text:      "turn",
			SessionID: "sess-Z",
			CreatedAt: "2024-01-01T10:0" + string(rune('0'+i)) + ":00Z",
		})
	}
	// Limit 0 should clamp to default (20), so all 5 turns come back.
	got, _ := bank.MemoriesInSession("sess-Z", 0)
	if len(got) != 5 {
		t.Errorf("expected all 5 turns with limit=0, got %d", len(got))
	}
	// Excessive limit clamps too.
	got, _ = bank.MemoriesInSession("sess-Z", 1000)
	if len(got) != 5 {
		t.Errorf("excessive limit should clamp; expected 5, got %d", len(got))
	}
}
