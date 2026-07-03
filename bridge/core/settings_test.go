package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────
// Generic settings CRUD
// ──────────────────────────────────────────────────────────────────────────

func TestSettings_CRUD(t *testing.T) {
	bank := newTestBank(t)

	// Empty initially.
	all, err := bank.ListSettings()
	if err != nil {
		t.Fatalf("ListSettings: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("expected empty settings, got %d rows", len(all))
	}

	// Set + get.
	if err := bank.SetSetting("theme", "dark"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	v, ok, err := bank.GetSetting("theme")
	if err != nil || !ok || v != "dark" {
		t.Errorf("GetSetting: %v %v %q", err, ok, v)
	}

	// Upsert.
	if err := bank.SetSetting("theme", "amber"); err != nil {
		t.Fatalf("SetSetting upsert: %v", err)
	}
	v, _, _ = bank.GetSetting("theme")
	if v != "amber" {
		t.Errorf("upsert should overwrite, got %q", v)
	}

	// Missing key.
	_, ok, err = bank.GetSetting("nope")
	if err != nil || ok {
		t.Errorf("missing key: err=%v ok=%v", err, ok)
	}

	// Delete.
	if err := bank.DeleteSetting("theme"); err != nil {
		t.Errorf("DeleteSetting: %v", err)
	}
	_, ok, _ = bank.GetSetting("theme")
	if ok {
		t.Errorf("expected key absent after delete")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// NightlySchedule.IsActive — the load-bearing helper NightlyRunner gates on
// ──────────────────────────────────────────────────────────────────────────

func TestNightlySchedule_IsActive(t *testing.T) {
	loc := time.UTC
	at := func(h, m int) time.Time {
		return time.Date(2026, 5, 8, h, m, 0, 0, loc)
	}

	// Pin every fixture to UTC. IsActive converts `now` into the schedule's
	// timezone, falling back to time.Local when Timezone is unset — so without
	// this the boundary assertions (built in UTC by at()) shift by the host's
	// local offset and fail on any non-UTC machine. Pinning the fixture, not
	// the production default, is the correct fix: on a desktop install
	// time.Local is the behavior a user expects for an un-zoned schedule.

	// Same-day window 02:00-06:00.
	day := NightlySchedule{Enabled: true, StartTime: "02:00", EndTime: "06:00", Timezone: "UTC"}
	if day.IsActive(at(1, 30)) {
		t.Errorf("01:30 should NOT be inside 02:00-06:00")
	}
	if !day.IsActive(at(2, 0)) {
		t.Errorf("02:00 should be inside (inclusive start)")
	}
	if !day.IsActive(at(5, 59)) {
		t.Errorf("05:59 should be inside")
	}
	if day.IsActive(at(6, 0)) {
		t.Errorf("06:00 should NOT be inside (exclusive end)")
	}
	if day.IsActive(at(14, 0)) {
		t.Errorf("14:00 should NOT be inside")
	}

	// Cross-midnight window 23:00-06:00.
	night := NightlySchedule{Enabled: true, StartTime: "23:00", EndTime: "06:00", Timezone: "UTC"}
	if !night.IsActive(at(23, 0)) {
		t.Errorf("23:00 should be inside cross-midnight window")
	}
	if !night.IsActive(at(23, 59)) {
		t.Errorf("23:59 should be inside")
	}
	if !night.IsActive(at(0, 0)) {
		t.Errorf("00:00 should be inside")
	}
	if !night.IsActive(at(5, 59)) {
		t.Errorf("05:59 should be inside")
	}
	if night.IsActive(at(6, 0)) {
		t.Errorf("06:00 should NOT be inside")
	}
	if night.IsActive(at(22, 0)) {
		t.Errorf("22:00 should NOT be inside")
	}

	// Disabled is always inactive.
	off := NightlySchedule{Enabled: false, StartTime: "02:00", EndTime: "06:00"}
	if off.IsActive(at(3, 0)) {
		t.Errorf("disabled schedule should never be active")
	}

	// Degenerate (start == end) is never active — explicit, not "always".
	zero := NightlySchedule{Enabled: true, StartTime: "03:00", EndTime: "03:00"}
	if zero.IsActive(at(3, 0)) {
		t.Errorf("start==end should never be active")
	}
}

func TestNightlySchedule_IsActiveTimezone(t *testing.T) {
	// Schedule in NY. When server clock says 06:30 UTC, NY is 02:30 (winter)
	// or 02:30 (summer DST adjusts the offset). Use a fixed Date in May where
	// US is on EDT (UTC-4) — so 06:30 UTC = 02:30 EDT, inside 02-06.
	utc630 := time.Date(2026, 5, 8, 6, 30, 0, 0, time.UTC)
	sched := NightlySchedule{
		Enabled:   true,
		StartTime: "02:00",
		EndTime:   "06:00",
		Timezone:  "America/New_York",
	}
	if !sched.IsActive(utc630) {
		t.Errorf("06:30 UTC = 02:30 EDT, should be inside 02-06 window")
	}
	// 09:00 UTC = 05:00 EDT — still inside.
	if !sched.IsActive(time.Date(2026, 5, 8, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("09:00 UTC = 05:00 EDT, should be inside")
	}
	// 14:00 UTC = 10:00 EDT — outside.
	if sched.IsActive(time.Date(2026, 5, 8, 14, 0, 0, 0, time.UTC)) {
		t.Errorf("14:00 UTC = 10:00 EDT, should be OUTSIDE")
	}
}

func TestNightlySchedule_Validate(t *testing.T) {
	bad := []NightlySchedule{
		{Enabled: true, StartTime: "25:00", EndTime: "06:00"},
		{Enabled: true, StartTime: "02:00", EndTime: "junk"},
		{Enabled: true, StartTime: "02:00", EndTime: "06:00", Timezone: "Mars/Olympus"},
		{Enabled: true, StartTime: "02:00", EndTime: "06:00", MinIdleMinutes: -1},
	}
	for i, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("[%d] expected validation error for %+v", i, s)
		}
	}
	// Disabled skips time validation entirely.
	off := NightlySchedule{Enabled: false, StartTime: "junk"}
	if err := off.Validate(); err != nil {
		t.Errorf("disabled schedule should not validate times, got %v", err)
	}
}

func TestNightlySchedule_NextActivation(t *testing.T) {
	loc := time.UTC
	from := time.Date(2026, 5, 8, 12, 0, 0, 0, loc) // noon UTC
	// Pin to UTC so NextActivationFrom computes the window in the same zone the
	// `from`/`at` times are built in — otherwise the host's local offset shifts
	// the result on non-UTC machines (see TestNightlySchedule_IsActive).
	sched := NightlySchedule{Enabled: true, StartTime: "23:00", EndTime: "06:00", Timezone: "UTC"}
	next := sched.NextActivationFrom(from)
	if next.Hour() != 23 || next.Day() != from.Day() {
		t.Errorf("from noon, next 23:00 should be same day, got %v", next)
	}
	// From 23:30, next 23:00 should be tomorrow.
	from2 := time.Date(2026, 5, 8, 23, 30, 0, 0, loc)
	next2 := sched.NextActivationFrom(from2)
	if next2.Day() != from.Day()+1 {
		t.Errorf("from 23:30, next 23:00 should roll to tomorrow, got %v", next2)
	}
}

func TestGetNightlySchedule_DefaultsWhenAbsent(t *testing.T) {
	bank := newTestBank(t)
	got, err := bank.GetNightlySchedule()
	if err != nil {
		t.Fatalf("GetNightlySchedule: %v", err)
	}
	if got.Configured {
		t.Errorf("default schedule should have configured=false (UI prompts)")
	}
	if !got.Enabled {
		t.Errorf("default schedule should be enabled (so a fresh deploy still runs nightly)")
	}
	if got.StartTime != "02:00" || got.EndTime != "06:00" {
		t.Errorf("unexpected default times: %+v", got)
	}
}

func TestSaveNightlySchedule_RoundTrip(t *testing.T) {
	bank := newTestBank(t)
	in := NightlySchedule{
		Enabled: true, StartTime: "23:00", EndTime: "07:30",
		Timezone: "UTC", Configured: true,
	}
	if err := bank.SaveNightlySchedule(in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := bank.GetNightlySchedule()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != in {
		t.Errorf("round-trip mismatch:\n  in=%+v\n got=%+v", in, got)
	}
}

func TestSaveNightlySchedule_RejectsBadInput(t *testing.T) {
	bank := newTestBank(t)
	bad := NightlySchedule{Enabled: true, StartTime: "junk", EndTime: "06:00"}
	if err := bank.SaveNightlySchedule(bad); err == nil {
		t.Errorf("expected validation error")
	}
	// Should not have written anything.
	v, ok, _ := bank.GetSetting(NightlyScheduleKey)
	if ok {
		t.Errorf("invalid save should not persist; got %q", v)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// HTTP
// ──────────────────────────────────────────────────────────────────────────

func TestHTTP_SettingsCRUD(t *testing.T) {
	bank := newTestBank(t)
	server := &Server{bank: bank}

	// PUT a generic key.
	body, _ := json.Marshal(map[string]string{"value": "amber"})
	req := httptest.NewRequest(http.MethodPut, "/settings/theme", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /settings/theme expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// GET it.
	req = httptest.NewRequest(http.MethodGet, "/settings/theme", nil)
	rec = httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings/theme expected 200, got %d", rec.Code)
	}
	var got SettingRow
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Value != "amber" {
		t.Errorf("expected value=amber, got %q", got.Value)
	}

	// LIST.
	req = httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec = httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /settings expected 200, got %d", rec.Code)
	}

	// DELETE.
	req = httptest.NewRequest(http.MethodDelete, "/settings/theme", nil)
	rec = httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("DELETE /settings/theme expected 204, got %d", rec.Code)
	}
}

func TestHTTP_NightlyScheduleTypedEndpoint(t *testing.T) {
	bank := newTestBank(t)
	server := &Server{bank: bank}

	// GET when nothing stored — returns defaults with configured=false.
	req := httptest.NewRequest(http.MethodGet, "/settings/nightly_schedule", nil)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET nightly_schedule expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got NightlySchedule
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Configured {
		t.Errorf("fresh GET should have configured=false")
	}
	if !got.Enabled {
		t.Errorf("fresh GET should be enabled by default")
	}

	// PUT a user-supplied schedule. The endpoint forces Configured=true.
	in := NightlySchedule{
		Enabled: true, StartTime: "23:00", EndTime: "07:00",
		Timezone: "UTC",
	}
	body, _ := json.Marshal(in)
	req = httptest.NewRequest(http.MethodPut, "/settings/nightly_schedule", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT nightly_schedule expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var saved NightlySchedule
	_ = json.Unmarshal(rec.Body.Bytes(), &saved)
	if !saved.Configured {
		t.Errorf("PUT should auto-set configured=true")
	}

	// Subsequent GET reflects the saved schedule.
	req = httptest.NewRequest(http.MethodGet, "/settings/nightly_schedule", nil)
	rec = httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	var post NightlySchedule
	_ = json.Unmarshal(rec.Body.Bytes(), &post)
	if !post.Configured || post.StartTime != "23:00" {
		t.Errorf("GET after PUT did not reflect change: %+v", post)
	}

	// PUT with bad time → 400.
	bad, _ := json.Marshal(NightlySchedule{Enabled: true, StartTime: "junk", EndTime: "06:00"})
	req = httptest.NewRequest(http.MethodPut, "/settings/nightly_schedule", bytes.NewReader(bad))
	rec = httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad PUT expected 400, got %d", rec.Code)
	}
}
