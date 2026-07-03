// ollama_proxy_test.go — wave 7c1 unit tests for the Ollama proxy
// client + the routing additions for /models + /pull_model.
//
// Live-Ollama paths get covered by the rebuild-and-verify step; here
// we run an offline httptest.Server that mimics the Ollama HTTP shape
// (one happy path + one error path) and verify the parser.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// ──────────────────────────────────────────────────────────────────────────
// OllamaProxyClient
// ──────────────────────────────────────────────────────────────────────────

func TestOllamaProxy_ListModels_HappyPath(t *testing.T) {
	body := `{
		"models": [
			{"name":"llama3.2:3b","modified_at":"2026-05-01T00:00:00Z","size":2000000000,"digest":"abc"},
			{"name":"nomic-embed-text","modified_at":"2026-05-01T00:00:00Z","size":250000000,"digest":"def"}
		]
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(body))
		case "/api/version":
			w.Write([]byte(`{"version":"0.5.4"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cli := NewOllamaProxyClient(srv.URL)
	models, version, err := cli.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if version != "0.5.4" {
		t.Errorf("version: want 0.5.4 got %q", version)
	}
	if len(models) != 2 {
		t.Fatalf("want 2 models, got %d", len(models))
	}
	if models[0].Name != "llama3.2:3b" || models[0].SizeMB != 2000000000/(1024*1024) {
		t.Errorf("model 0: %+v", models[0])
	}
}

func TestOllamaProxy_ListModels_NotFoundError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("nope"))
	}))
	defer srv.Close()
	cli := NewOllamaProxyClient(srv.URL)
	_, _, err := cli.ListModels(context.Background())
	if err == nil {
		t.Errorf("expected error on 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention status; got %v", err)
	}
}

func TestOllamaProxy_StreamPull_EmitsStatusLines(t *testing.T) {
	// Ollama pulls emit newline-delimited JSON status objects.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pull" {
			http.NotFound(w, r)
			return
		}
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "application/x-ndjson")
		lines := []string{
			`{"status":"pulling manifest"}`,
			`{"status":"downloading","digest":"sha256:aaa","total":1000,"completed":500}`,
			`{"status":"downloading","digest":"sha256:aaa","total":1000,"completed":1000}`,
			`{"status":"success"}`,
		}
		for _, l := range lines {
			w.Write([]byte(l + "\n"))
			flusher.Flush()
		}
	}))
	defer srv.Close()

	cli := NewOllamaProxyClient(srv.URL)
	var got []OllamaPullStatus
	err := cli.StreamPull(context.Background(), "llama3.2:3b", func(s OllamaPullStatus) {
		got = append(got, s)
	})
	if err != nil {
		t.Fatalf("StreamPull: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("want 4 status lines, got %d: %+v", len(got), got)
	}
	if got[0].Status != "pulling manifest" {
		t.Errorf("line 0: %+v", got[0])
	}
	if got[1].Completed != 500 || got[1].Total != 1000 {
		t.Errorf("line 1 progress: %+v", got[1])
	}
	if got[3].Status != "success" {
		t.Errorf("line 3: %+v", got[3])
	}
}

func TestOllamaProxy_StreamPull_PropagatesErrorLine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Write([]byte(`{"status":"pulling manifest"}` + "\n"))
		w.Write([]byte(`{"error":"manifest not found"}` + "\n"))
	}))
	defer srv.Close()
	cli := NewOllamaProxyClient(srv.URL)
	err := cli.StreamPull(context.Background(), "bogus:tag", func(s OllamaPullStatus) {})
	if err == nil {
		t.Errorf("expected error when stream carries error field")
	}
	if !strings.Contains(err.Error(), "manifest not found") {
		t.Errorf("error should propagate ollama message; got %v", err)
	}
}

func TestOllamaBaseURLForContainer_ShapesCorrectly(t *testing.T) {
	if got := ollamaBaseURLForContainer("synaptic-ollama-tier2"); got != "http://synaptic-ollama-tier2:11434" {
		t.Errorf("got %q", got)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Router additions (offline — gate-off paths)
// ──────────────────────────────────────────────────────────────────────────

func TestAdminDockerRouter_AcceptsModelsAndPullModel(t *testing.T) {
	srv := newTestServer(t, nil)
	srv.docker = &DockerControlState{
		client:         NewDockerClient("/tmp/nope.sock"),
		allowlistRegex: regexp.MustCompile("^synaptic-"),
		composeProject: "synaptic",
		socketAllowed:  false, // gate off short-circuits before docker calls
	}
	cases := []struct {
		path   string
		method string
	}{
		{"/admin/docker/services/ollama-tier2/models", http.MethodGet},
		{"/admin/docker/services/ollama-tier2/pull_model", http.MethodPost},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.adminDockerRouter(w, req)
		if w.Code != http.StatusPreconditionFailed {
			t.Errorf("%s %s: want 412 (gate off), got %d body=%s", c.method, c.path, w.Code, w.Body.String())
		}
	}
}

func TestAdminDockerRouter_UnknownActionListsNew(t *testing.T) {
	srv := newTestServer(t, nil)
	srv.docker = NewDockerControlState()
	req := httptest.NewRequest(http.MethodPost, "/admin/docker/services/core/yeet", nil)
	w := httptest.NewRecorder()
	srv.adminDockerRouter(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
	body := w.Body.String()
	for _, action := range []string{"models", "pull_model"} {
		if !strings.Contains(body, action) {
			t.Errorf("404 body should mention legal action %q; got %s", action, body)
		}
	}
}

// silence unused-import warnings under different build modes
var _ = json.Marshal
