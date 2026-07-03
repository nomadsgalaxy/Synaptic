package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func mapOverridesServer(t *testing.T) *Server {
	t.Helper()
	bank := newTestBank(t)
	return &Server{bank: bank}
}

func putOverride(t *testing.T, server *Server, key string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/maps/overrides/"+key, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	return rec
}

func TestMapsOverrides_PutThenGet(t *testing.T) {
	server := mapOverridesServer(t)
	cat, alias := "project", "Event Tracker"
	rec := putOverride(t, server, "event-tracker", map[string]interface{}{
		"category": cat, "alias": alias, "archived": false,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// GET single
	req := httptest.NewRequest(http.MethodGet, "/maps/overrides/event-tracker", nil)
	rec2 := httptest.NewRecorder()
	server.routes().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET expected 200, got %d", rec2.Code)
	}
	var got MapOverride
	_ = json.Unmarshal(rec2.Body.Bytes(), &got)
	if got.Category != "project" {
		t.Errorf("category mismatch, got %q", got.Category)
	}
	if got.Alias != "Event Tracker" {
		t.Errorf("alias mismatch, got %q", got.Alias)
	}

	// GET list
	req = httptest.NewRequest(http.MethodGet, "/maps/overrides", nil)
	rec3 := httptest.NewRecorder()
	server.routes().ServeHTTP(rec3, req)
	if rec3.Code != http.StatusOK {
		t.Fatalf("GET list expected 200, got %d", rec3.Code)
	}
	var listResp map[string]interface{}
	_ = json.Unmarshal(rec3.Body.Bytes(), &listResp)
	overrides, _ := listResp["overrides"].(map[string]interface{})
	if _, ok := overrides["event-tracker"]; !ok {
		t.Errorf("list response missing event-tracker key, got %v", overrides)
	}
}

func TestMapsOverrides_Delete(t *testing.T) {
	server := mapOverridesServer(t)
	_ = putOverride(t, server, "trash-me", map[string]interface{}{"category": "topic"})

	req := httptest.NewRequest(http.MethodDelete, "/maps/overrides/trash-me", nil)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE expected 204, got %d", rec.Code)
	}

	// Verify gone
	req = httptest.NewRequest(http.MethodGet, "/maps/overrides/trash-me", nil)
	rec = httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET after delete expected 404, got %d", rec.Code)
	}
}

func TestMapsOverrides_PartialPatch(t *testing.T) {
	server := mapOverridesServer(t)
	// Set both fields.
	_ = putOverride(t, server, "k1", map[string]interface{}{
		"category": "entity", "alias": "Original", "archived": false,
	})
	// Partial PUT: only update category.
	cat := "topic"
	body := map[string]*string{"category": &cat}
	bodyJSON, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/maps/overrides/k1", bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT partial expected 200, got %d", rec.Code)
	}

	row, _ := server.bank.GetMapOverride("k1")
	if row.Category != "topic" {
		t.Errorf("category should have updated, got %q", row.Category)
	}
	if row.Alias != "Original" {
		t.Errorf("alias should be untouched (partial patch), got %q", row.Alias)
	}
}

func TestMapsOverrides_BadCategory(t *testing.T) {
	server := mapOverridesServer(t)
	rec := putOverride(t, server, "bad", map[string]interface{}{"category": "garbage"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad category should 400, got %d", rec.Code)
	}
}

func TestMapsOverrides_RoutePrecedence(t *testing.T) {
	server := mapOverridesServer(t)
	// /maps/overrides must dispatch to the overrides handler, not to the
	// generic /maps/{id} branch (which would 404 for "overrides" map id).
	req := httptest.NewRequest(http.MethodGet, "/maps/overrides", nil)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /maps/overrides should 200, got %d body=%s",
			rec.Code, rec.Body.String())
	}
}
