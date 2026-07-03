// handlers_procedures.go — v2.7 Bundle O: HTTP surface for procedural
// memories.
//
// Procedures are stored as ordinary memories with memory_type=
// "procedure" — callers create them via the existing POST /event +
// /bank/memories paths (or via sd_save_procedure / sd_create_memory on
// the MCP side). This file adds the read endpoint the dashboard uses
// to list "what does the agent think it should always do?".
//
//   GET  /procedures              → list active (non-deleted, non-dormant)
//                                   procedural memories
//   GET  /procedures?scope=auth   → filter by tag (case-insensitive)
//   GET  /procedures/prefix       → return the rendered system-prompt
//                                   prefix the inference path injects
//                                   (handy for the "preview procedures"
//                                   debug card)
package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

func (s *Server) handleProcedures(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if s.bank == nil {
		http.Error(w, "bank disabled", http.StatusServiceUnavailable)
		return
	}

	// /procedures/prefix returns the prompt fragment rather than the raw
	// memory list. Lets the dashboard render exactly what the agent will
	// see in its system prompt.
	if strings.HasSuffix(r.URL.Path, "/prefix") {
		scope := r.URL.Query().Get("scope")
		prefix, err := s.bank.ProcedurePromptPrefix(scope, 0)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"prefix":     prefix,
			"prefix_len": len(prefix),
		})
		return
	}

	scope := r.URL.Query().Get("scope")
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	procs, err := s.bank.ListActiveProcedures(scope, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"count":      len(procs),
		"scope":      scope,
		"procedures": procs,
	})
}
