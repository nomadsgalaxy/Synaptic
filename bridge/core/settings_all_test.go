// settings_all_test.go — wave 6 Part 1 of the server-side prefs work.
// Verifies GET /settings/all groups settings by namespace + carries the
// typed bundles inline.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSettingsAll_GroupsByNamespace(t *testing.T) {
	bank := newTestBank(t)
	bank.SetSetting("ui.theme", "dark")
	bank.SetSetting("ui.brain_name", "ORACLE")
	bank.SetSetting("ui.bubbles_hidden", "true")
	bank.SetSetting("notes.last_export", "2026-05-10")

	srv := newTestServer(t, bank)
	req := httptest.NewRequest(http.MethodGet, "/settings/all", nil)
	w := httptest.NewRecorder()
	srv.settingsAll(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Raw            map[string]map[string]string `json:"raw"`
		DreamPipeline  json.RawMessage              `json:"dream_pipeline"`
		Augment        json.RawMessage              `json:"augment"`
		Providers      json.RawMessage              `json:"providers"`
		APIKeyEnvSet   json.RawMessage              `json:"api_key_env_set"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	ui := resp.Raw["ui"]
	if ui["theme"] != "dark" {
		t.Errorf("ui.theme: want dark, got %q", ui["theme"])
	}
	if ui["brain_name"] != "ORACLE" {
		t.Errorf("ui.brain_name: want ORACLE, got %q", ui["brain_name"])
	}
	if ui["bubbles_hidden"] != "true" {
		t.Errorf("ui.bubbles_hidden: want \"true\", got %q", ui["bubbles_hidden"])
	}
	if resp.Raw["notes"]["last_export"] != "2026-05-10" {
		t.Errorf("notes.last_export missing")
	}
	if len(resp.DreamPipeline) == 0 || string(resp.DreamPipeline) == "null" {
		t.Errorf("dream_pipeline bundle missing")
	}
	if len(resp.Providers) == 0 {
		t.Errorf("providers bundle missing")
	}
}

func TestSettingsAll_RootNamespaceForUndottedKeys(t *testing.T) {
	bank := newTestBank(t)
	bank.SetSetting("legacy_key", "value")
	srv := newTestServer(t, bank)
	req := httptest.NewRequest(http.MethodGet, "/settings/all", nil)
	w := httptest.NewRecorder()
	srv.settingsAll(w, req)
	var resp struct {
		Raw map[string]map[string]string `json:"raw"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Raw["_root"]["legacy_key"] != "value" {
		t.Errorf("undotted key should land in _root, got %+v", resp.Raw["_root"])
	}
}

func TestSettingsAll_MethodGuard(t *testing.T) {
	bank := newTestBank(t)
	srv := newTestServer(t, bank)
	req := httptest.NewRequest(http.MethodPost, "/settings/all", nil)
	w := httptest.NewRecorder()
	srv.settingsAll(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST want 405, got %d", w.Code)
	}
}

func TestSettingsRoot_RoutesAllBeforeKeyCatchall(t *testing.T) {
	// Regression: /settings/all must dispatch to settingsAll, not be looked
	// up as a literal key with key="all".
	bank := newTestBank(t)
	srv := newTestServer(t, bank)
	req := httptest.NewRequest(http.MethodGet, "/settings/all", nil)
	w := httptest.NewRecorder()
	srv.settingsRoot(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if _, ok := resp["raw"]; !ok {
		t.Errorf("response missing 'raw' key — route may have fallen through to settingsByKey")
	}
}
