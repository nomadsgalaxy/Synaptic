// ollama_proxy.go — Wave 7c1 proxy client for Ollama's /api/tags + /api/pull.
//
// Each per-tier Ollama container exposes the same HTTP API on its
// internal port 11434. From inside the SD Core container we reach a
// sibling Ollama via docker's internal DNS (e.g.
// `http://synaptic-ollama-tier2:11434`).
//
// Endpoints used:
//
//   GET  /api/tags     — list pulled models
//   POST /api/pull     — pull a model (chunked JSON progress stream)
//   GET  /api/version  — daemon version (small + cheap; surfaces in the UI)
//
// Anything else stays out of scope for this wave.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OllamaProxyClient talks to a single named Ollama container.
type OllamaProxyClient struct {
	baseURL string
	http    *http.Client
}

// NewOllamaProxyClient builds a client targeting baseURL (e.g.
// `http://synaptic-ollama-tier2:11434`). Short request timeout — the
// pull endpoint streams + uses its own no-timeout client.
func NewOllamaProxyClient(baseURL string) *OllamaProxyClient {
	return &OllamaProxyClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// OllamaModel is the trimmed projection of one /api/tags entry.
type OllamaModel struct {
	Name       string `json:"name"`
	Size       int64  `json:"size"`        // bytes
	SizeMB     int64  `json:"size_mb"`     // derived
	ModifiedAt string `json:"modified_at"`
	Digest     string `json:"digest,omitempty"`
}

// ListModels calls GET /api/tags. Returns the list of pulled models +
// the underlying Ollama version (best-effort).
func (c *OllamaProxyClient) ListModels(ctx context.Context) ([]OllamaModel, string, error) {
	if c == nil {
		return nil, "", errors.New("ollama proxy client nil")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/tags", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, "", fmt.Errorf("ollama /api/tags: %s: %s", resp.Status, body)
	}
	var raw struct {
		Models []struct {
			Name       string `json:"name"`
			ModifiedAt string `json:"modified_at"`
			Size       int64  `json:"size"`
			Digest     string `json:"digest"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, "", err
	}
	out := make([]OllamaModel, len(raw.Models))
	for i, m := range raw.Models {
		out[i] = OllamaModel{
			Name:       m.Name,
			Size:       m.Size,
			SizeMB:     m.Size / (1024 * 1024),
			ModifiedAt: m.ModifiedAt,
			Digest:     m.Digest,
		}
	}
	// Version: best-effort separate call.
	version := c.fetchVersion(ctx)
	return out, version, nil
}

// fetchVersion calls /api/version (best-effort; returns "" on error).
func (c *OllamaProxyClient) fetchVersion(ctx context.Context) string {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/version", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var v struct {
		Version string `json:"version"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return v.Version
}

// OllamaPullStatus is one line of the chunked pull progress stream.
// The Ollama daemon emits JSON-per-line; we forward each line up to the
// SSE handler.
type OllamaPullStatus struct {
	Status    string `json:"status"`
	Digest    string `json:"digest,omitempty"`
	Total     int64  `json:"total,omitempty"`
	Completed int64  `json:"completed,omitempty"`
	Error     string `json:"error,omitempty"`
}

// StreamPull calls POST /api/pull and invokes onLine for each parsed
// status chunk. Returns when the stream ends or ctx is cancelled.
// Closes the connection on context cancellation (the caller's context
// owns the lifetime — typically the SSE handler).
func (c *OllamaProxyClient) StreamPull(ctx context.Context, modelName string, onLine func(OllamaPullStatus)) error {
	if c == nil {
		return errors.New("ollama proxy client nil")
	}
	body, _ := json.Marshal(map[string]any{
		"name":   modelName,
		"stream": true,
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/pull", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Pull can take minutes for a 5-8GB model — disable the per-request
	// timeout. Caller's ctx governs cancellation.
	noTimeoutClient := &http.Client{Transport: c.http.Transport}
	resp, err := noTimeoutClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ollama /api/pull: %s: %s", resp.Status, raw)
	}
	scanner := bufio.NewScanner(resp.Body)
	// Pull lines can be longer than the default 64KB buffer when an early
	// status carries a long error message; raise the cap.
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<20)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var s OllamaPullStatus
		if jerr := json.Unmarshal([]byte(line), &s); jerr != nil {
			// Forward as a status-only line so the UI sees what happened.
			onLine(OllamaPullStatus{Status: "(parse error) " + line})
			continue
		}
		onLine(s)
		if s.Error != "" {
			return fmt.Errorf("ollama pull error: %s", s.Error)
		}
	}
	return scanner.Err()
}

// ollamaBaseURLForContainer maps a compose service name (e.g. "ollama-tier2")
// to the per-network DNS URL we use to reach it. Compose adds the
// container_name as a network alias, so http://<container_name>:11434
// resolves inside the synaptic_default network.
func ollamaBaseURLForContainer(containerName string) string {
	return "http://" + containerName + ":11434"
}

// DeleteModel calls Ollama's `DELETE /api/delete` with the model name in
// the request body. Returns nil on success. The Ollama daemon doesn't
// report bytes freed in the response — callers (the /delete_model
// handler) compute that by diff-ing ListModels before+after.
//
// Note: Ollama's DELETE accepts JSON body even though that's unusual
// for DELETE methods. The shape matches the documented API:
//
//	DELETE /api/delete
//	Content-Type: application/json
//	{"name": "llama3.1:8b"}
func (c *OllamaProxyClient) DeleteModel(ctx context.Context, modelName string) error {
	if c == nil {
		return errors.New("ollama proxy client nil")
	}
	body, _ := json.Marshal(map[string]any{"name": modelName})
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete,
		c.baseURL+"/api/delete", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("ollama /api/delete: model %q not found", modelName)
	}
	return fmt.Errorf("ollama /api/delete: %s: %s", resp.Status, raw)
}
