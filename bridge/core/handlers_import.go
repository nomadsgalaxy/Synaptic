// handlers_import.go — POST /import (v2.5.0b1+ feature #22).
//
// One-way bulk import of memories from external memory systems and from
// generic JSON dumps. The endpoint accepts a JSON payload describing the
// source format and a memories array; SD Core normalizes each item to a
// MemoryRecord and writes it through the bank.
//
// Contract:
//
//   POST /import
//     Body: {
//       "format":    "synaptic"|"format_a"|"format_b"|"format_c"|"generic",
//                                    // legacy keys are still accepted for
//                                    // back-compat (see mapper switch below)
//       "memories":  [...],          // source-shape records
//       "options": {                 // all optional
//         "tag_prefix":      string, // tag prefix applied to every memory
//                                    // ("imported:<format>") so they're
//                                    // recognizable in the UI
//         "adapter_id":      string, // override adapter_id on every record
//                                    // (default: "import-{format}")
//         "preserve_ids":    bool,   // keep source ids verbatim instead of
//                                    // letting the bank assign UUIDs. Risk:
//                                    // collisions with existing memories.
//         "mark_sensitive":  bool    // force-flag every imported memory as
//                                    // sensitive (use when re-importing
//                                    // private data dumps).
//       }
//     }
//     200: {
//       imported:        int,
//       skipped:         int,
//       errors:          []{index, message},
//       duration_ms:     int,
//       format:          string,
//       total_submitted: int
//     }
//     400: malformed payload, unknown format, or empty memories array
//     413: payload exceeds size limit (8 MiB)
//     500: bank write failure
//
// Privacy:
//   - The endpoint is auth-gated by the bearer-token middleware. Imports are
//     a write path; respects MemoryWriteRateLimited.
//   - mark_sensitive options exists so re-importing personal dumps doesn't
//     need a post-hoc bulk sensitive-flag pass. The user explicitly chooses
//     whether to mark.
//   - No memories are sent to Tier 3 during import. Embeddings + classifier
//     run on the normal ingestion path after the bank write commits.
//
// Format-specific notes:
//
//   synaptic — passthrough; the memories array is expected to be a list of
//              MemoryRecord JSON shapes. Use this for round-tripping between
//              SD Core installations (paired with /memories?include_deleted=1
//              + dump).
//
//   external-format-a — retain-style payload shape. Maps:
//                 text                       → text
//                 tags                       → tags
//                 region                     → region_hint
//                 metadata.adapter_id        → adapter_id (when set)
//                 metadata.session_id        → session_id (when set)
//                 created_at                 → preserved as `imported_at` tag
//
//   external-format-b — categorized memory shape. Maps:
//                 memory                     → text
//                 categories                 → tags
//                 metadata.user_id           → adapter_id
//                 created_at                 → preserved as `imported_at` tag
//
//   external-format-c — archival-memory shape. Maps:
//                 content                    → text
//                 labels                     → tags
//                 source_id                  → session_id
//                 created_at                 → preserved as `imported_at` tag
//
//   generic   — Loose mapping. Required: `text` (or `content` or `body` or
//               `summary` as fallbacks). Optional: `tags`, `region`,
//               `adapter_id`, `session_id`, `created_at`, `source`. Useful
//               for one-off CLI dumps.
//
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// importRequest is the wire-level request body.
type importRequest struct {
	Format   string            `json:"format"`
	Memories []json.RawMessage `json:"memories"`
	Options  importOptions     `json:"options"`
}

type importOptions struct {
	TagPrefix      string `json:"tag_prefix"`
	AdapterID      string `json:"adapter_id"`
	PreserveIDs    bool   `json:"preserve_ids"`
	MarkSensitive  bool   `json:"mark_sensitive"`
}

type importError struct {
	Index   int    `json:"index"`
	Message string `json:"message"`
}

type importResponse struct {
	Imported        int           `json:"imported"`
	Skipped         int           `json:"skipped"`
	Errors          []importError `json:"errors,omitempty"`
	DurationMs      int64         `json:"duration_ms"`
	Format          string        `json:"format"`
	TotalSubmitted  int           `json:"total_submitted"`
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST", "OPTIONS")
		return
	}
	if s.bank == nil {
		http.Error(w, "bank disabled — import requires SD_BANK_PATH", http.StatusServiceUnavailable)
		return
	}

	// 8 MiB cap. Imports are usually a few hundred memories; if the user
	// has 10K+ memories they should chunk the upload client-side.
	body, err := io.ReadAll(io.LimitReader(r.Body, 8*1024*1024+1))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > 8*1024*1024 {
		http.Error(w, "payload exceeds 8 MiB limit — chunk client-side", http.StatusRequestEntityTooLarge)
		return
	}

	var req importRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Format == "" {
		http.Error(w, `"format" required (see /import docs for supported format IDs)`, http.StatusBadRequest)
		return
	}
	// Functional keys retained for back-compat (callers in the wild use these).
	allowed := map[string]bool{"synaptic": true, "hindsight": true, "mem0": true, "letta": true, "generic": true}
	if !allowed[req.Format] {
		http.Error(w, "unknown format: "+req.Format, http.StatusBadRequest)
		return
	}
	if len(req.Memories) == 0 {
		http.Error(w, `"memories" array required and must be non-empty`, http.StatusBadRequest)
		return
	}

	start := time.Now()
	adapterID := req.Options.AdapterID
	if adapterID == "" {
		adapterID = "import-" + req.Format
	}

	resp := importResponse{
		Format:         req.Format,
		TotalSubmitted: len(req.Memories),
	}

	for idx, raw := range req.Memories {
		rec, mapErr := normalizeImportRow(req.Format, raw, req.Options, adapterID)
		if mapErr != nil {
			resp.Skipped++
			resp.Errors = append(resp.Errors, importError{Index: idx, Message: mapErr.Error()})
			continue
		}
		if rec.Text == "" {
			resp.Skipped++
			resp.Errors = append(resp.Errors, importError{Index: idx, Message: "empty text after mapping"})
			continue
		}
		if _, err := s.bank.SaveMemory(rec); err != nil {
			resp.Skipped++
			resp.Errors = append(resp.Errors, importError{Index: idx, Message: "bank.SaveMemory: " + err.Error()})
			continue
		}
		resp.Imported++
	}

	resp.DurationMs = time.Since(start).Milliseconds()
	s.auditWrite(AuditEntry{
		Operation:  "memory_bulk_import",
		EntityType: "memory",
		EntityID:   "",
		AfterJSON: fmt.Sprintf(`{"format":%q,"imported":%d,"skipped":%d,"total":%d}`,
			req.Format, resp.Imported, resp.Skipped, resp.TotalSubmitted),
		AdapterID: adapterIDFromRequest(r),
	})
	writeJSON(w, http.StatusOK, resp)
}

// normalizeImportRow maps a single source-shape JSON row to MemoryRecord per
// the format. Returns an error when the row can't be parsed at all; returns
// a MemoryRecord with empty Text when no text field could be found (caller
// surfaces the "empty text" skip).
func normalizeImportRow(format string, raw json.RawMessage, opts importOptions, adapterID string) (MemoryRecord, error) {
	switch format {
	case "synaptic":
		var rec MemoryRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return MemoryRecord{}, fmt.Errorf("parse synaptic shape: %w", err)
		}
		if !opts.PreserveIDs {
			rec.ID = ""
		}
		applyImportOptions(&rec, opts, adapterID)
		return rec, nil

	case "hindsight":
		var src struct {
			ID       string                 `json:"id"`
			Text     string                 `json:"text"`
			Tags     []string               `json:"tags"`
			Region   string                 `json:"region"`
			Created  string                 `json:"created_at"`
			Metadata map[string]interface{} `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &src); err != nil {
			return MemoryRecord{}, fmt.Errorf("parse hindsight shape: %w", err)
		}
		rec := MemoryRecord{
			Text:       src.Text,
			Tags:       src.Tags,
			RegionHint: src.Region,
			Source:     "import:hindsight",
		}
		if opts.PreserveIDs {
			rec.ID = src.ID
		}
		if v, ok := src.Metadata["adapter_id"].(string); ok && v != "" {
			rec.AdapterID = v
		}
		if v, ok := src.Metadata["session_id"].(string); ok && v != "" {
			rec.SessionID = v
		}
		if src.Created != "" {
			rec.Tags = append(rec.Tags, "imported_at:"+src.Created)
		}
		applyImportOptions(&rec, opts, adapterID)
		return rec, nil

	case "mem0":
		var src struct {
			ID         string                 `json:"id"`
			Memory     string                 `json:"memory"`
			Categories []string               `json:"categories"`
			Created    string                 `json:"created_at"`
			Metadata   map[string]interface{} `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &src); err != nil {
			return MemoryRecord{}, fmt.Errorf("parse mem0 shape: %w", err)
		}
		rec := MemoryRecord{
			Text:   src.Memory,
			Tags:   src.Categories,
			Source: "import:mem0",
		}
		if opts.PreserveIDs {
			rec.ID = src.ID
		}
		if v, ok := src.Metadata["user_id"].(string); ok && v != "" {
			rec.AdapterID = v
		}
		if src.Created != "" {
			rec.Tags = append(rec.Tags, "imported_at:"+src.Created)
		}
		applyImportOptions(&rec, opts, adapterID)
		return rec, nil

	case "letta":
		var src struct {
			ID       string   `json:"id"`
			Content  string   `json:"content"`
			Labels   []string `json:"labels"`
			SourceID string   `json:"source_id"`
			Created  string   `json:"created_at"`
		}
		if err := json.Unmarshal(raw, &src); err != nil {
			return MemoryRecord{}, fmt.Errorf("parse letta shape: %w", err)
		}
		rec := MemoryRecord{
			Text:      src.Content,
			Tags:      src.Labels,
			SessionID: src.SourceID,
			Source:    "import:letta",
		}
		if opts.PreserveIDs {
			rec.ID = src.ID
		}
		if src.Created != "" {
			rec.Tags = append(rec.Tags, "imported_at:"+src.Created)
		}
		applyImportOptions(&rec, opts, adapterID)
		return rec, nil

	case "generic":
		// Be forgiving: read text from any of several candidate keys, tags
		// from any of tags/categories/labels, etc. Anything not recognized
		// is dropped silently.
		var m map[string]interface{}
		if err := json.Unmarshal(raw, &m); err != nil {
			return MemoryRecord{}, fmt.Errorf("parse generic shape: %w", err)
		}
		rec := MemoryRecord{Source: "import:generic"}
		for _, k := range []string{"text", "content", "body", "summary", "memory"} {
			if v, ok := m[k].(string); ok && v != "" {
				rec.Text = v
				break
			}
		}
		for _, k := range []string{"tags", "categories", "labels"} {
			if arr, ok := m[k].([]interface{}); ok {
				for _, v := range arr {
					if s, ok := v.(string); ok && s != "" {
						rec.Tags = append(rec.Tags, s)
					}
				}
				break
			}
		}
		if v, ok := m["region"].(string); ok {
			rec.RegionHint = v
		}
		if v, ok := m["region_hint"].(string); ok && rec.RegionHint == "" {
			rec.RegionHint = v
		}
		if v, ok := m["adapter_id"].(string); ok {
			rec.AdapterID = v
		}
		if v, ok := m["session_id"].(string); ok {
			rec.SessionID = v
		}
		if opts.PreserveIDs {
			if v, ok := m["id"].(string); ok {
				rec.ID = v
			}
		}
		if v, ok := m["created_at"].(string); ok && v != "" {
			rec.Tags = append(rec.Tags, "imported_at:"+v)
		}
		applyImportOptions(&rec, opts, adapterID)
		return rec, nil
	}

	return MemoryRecord{}, fmt.Errorf("unknown format: %s", format)
}

// applyImportOptions stamps adapter_id, tag prefix, and sensitive flag onto
// the normalized record. Idempotent — caller may invoke multiple times.
func applyImportOptions(rec *MemoryRecord, opts importOptions, defaultAdapterID string) {
	if rec.AdapterID == "" {
		rec.AdapterID = defaultAdapterID
	}
	if opts.MarkSensitive {
		rec.Sensitive = true
	}
	if opts.TagPrefix != "" {
		// Prepend so it's easy to spot in the dashboard. Skip if the prefix
		// is already present (re-importing should be idempotent on tags).
		found := false
		for _, t := range rec.Tags {
			if strings.EqualFold(t, opts.TagPrefix) {
				found = true
				break
			}
		}
		if !found {
			rec.Tags = append([]string{opts.TagPrefix}, rec.Tags...)
		}
	}
}
