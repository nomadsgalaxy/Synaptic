// hook_events_test.go — Wave 5 hook log persistence + /events filter
// fallthrough.
package main

import (
	"strings"
	"testing"
	"time"
)

func TestSaveHookEvent_RequiresEventType(t *testing.T) {
	bank := newTestBank(t)
	_, err := bank.SaveHookEvent(HookEvent{AdapterID: "cc", Payload: "{}"})
	if err == nil {
		t.Errorf("expected error when event_type missing")
	}
}

func TestSaveHookEvent_AutoFillsIDAndTimestamp(t *testing.T) {
	bank := newTestBank(t)
	saved, err := bank.SaveHookEvent(HookEvent{
		AdapterID: "cc", EventType: "tool_call", Payload: `{"tool":"Read"}`,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if saved.ID == "" {
		t.Errorf("expected auto-generated ID")
	}
	if saved.CreatedAt == "" {
		t.Errorf("expected auto-generated CreatedAt")
	}
}

func TestSaveHookEvent_TruncatesOversizedPayload(t *testing.T) {
	bank := newTestBank(t)
	big := strings.Repeat("x", MaxHookEventPayloadBytes*2)
	saved, err := bank.SaveHookEvent(HookEvent{
		EventType: "huge", Payload: big,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(saved.Payload) > MaxHookEventPayloadBytes {
		t.Errorf("payload not truncated: got %d bytes", len(saved.Payload))
	}
	if !strings.HasSuffix(saved.Payload, "<truncated>") {
		t.Errorf("expected truncation marker at end, got tail=%q", saved.Payload[len(saved.Payload)-20:])
	}
}

func TestListHookEvents_FiltersByAdapterSessionType(t *testing.T) {
	bank := newTestBank(t)
	bank.SaveHookEvent(HookEvent{AdapterID: "cc", SessionID: "s1", EventType: "tool_call"})
	bank.SaveHookEvent(HookEvent{AdapterID: "cc", SessionID: "s2", EventType: "tool_call"})
	bank.SaveHookEvent(HookEvent{AdapterID: "mcp", SessionID: "s1", EventType: "memory_added"})

	gotCC, _ := bank.ListHookEvents(HookEventListOpts{AdapterID: "cc"})
	if len(gotCC) != 2 {
		t.Errorf("adapter=cc want 2, got %d", len(gotCC))
	}
	gotS1, _ := bank.ListHookEvents(HookEventListOpts{SessionID: "s1"})
	if len(gotS1) != 2 {
		t.Errorf("session=s1 want 2, got %d", len(gotS1))
	}
	gotType, _ := bank.ListHookEvents(HookEventListOpts{EventType: "memory_added"})
	if len(gotType) != 1 {
		t.Errorf("type=memory_added want 1, got %d", len(gotType))
	}
}

func TestListHookEvents_OrdersNewestFirst(t *testing.T) {
	bank := newTestBank(t)
	bank.SaveHookEvent(HookEvent{EventType: "a", CreatedAt: "2026-01-01T00:00:00Z"})
	bank.SaveHookEvent(HookEvent{EventType: "b", CreatedAt: "2026-05-01T00:00:00Z"})
	bank.SaveHookEvent(HookEvent{EventType: "c", CreatedAt: "2026-03-01T00:00:00Z"})

	got, _ := bank.ListHookEvents(HookEventListOpts{})
	if len(got) != 3 {
		t.Fatalf("want 3, got %d", len(got))
	}
	if got[0].EventType != "b" || got[1].EventType != "c" || got[2].EventType != "a" {
		t.Errorf("ordering: want b,c,a got %s,%s,%s", got[0].EventType, got[1].EventType, got[2].EventType)
	}
}

func TestListHookEvents_SinceFiltersCorrectly(t *testing.T) {
	bank := newTestBank(t)
	bank.SaveHookEvent(HookEvent{EventType: "old", CreatedAt: "2026-01-01T00:00:00Z"})
	bank.SaveHookEvent(HookEvent{EventType: "new", CreatedAt: "2026-05-01T00:00:00Z"})

	got, _ := bank.ListHookEvents(HookEventListOpts{Since: "2026-04-01T00:00:00Z"})
	if len(got) != 1 || got[0].EventType != "new" {
		t.Errorf("since filter: want [new], got %+v", got)
	}
}

func TestPruneHookEvents_DropsOldRows(t *testing.T) {
	bank := newTestBank(t)
	bank.SaveHookEvent(HookEvent{EventType: "old", CreatedAt: "2026-01-01T00:00:00Z"})
	bank.SaveHookEvent(HookEvent{EventType: "keep", CreatedAt: "2026-05-01T00:00:00Z"})

	cutoff, _ := time.Parse(time.RFC3339, "2026-03-01T00:00:00Z")
	dropped, err := bank.PruneHookEvents(cutoff)
	if err != nil {
		t.Fatalf("prune err: %v", err)
	}
	if dropped != 1 {
		t.Errorf("want 1 dropped, got %d", dropped)
	}
	left, _ := bank.ListHookEvents(HookEventListOpts{})
	if len(left) != 1 || left[0].EventType != "keep" {
		t.Errorf("after prune: want [keep], got %+v", left)
	}
}
