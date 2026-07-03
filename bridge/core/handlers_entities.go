// handlers_entities.go — v2.7 Bundle N HTTP surface for the entity
// registry. The bank methods (UpsertEntity, AddAlias, ResolveAlias,
// ExpandQueryViaAliases) shipped with Bundle N but had no REST entry
// point until now — agents could only populate the registry via the
// MCP tools or direct bank-level access.
//
// Endpoints:
//
//   GET    /entities                          list (?limit=)
//   POST   /entities                          upsert by canonical_name
//                                             body: {canonical_name, kind?}
//   GET    /entities/{id}                     fetch one
//   DELETE /entities/{id}                     remove (cascades aliases)
//   GET    /entities/{id}/aliases             list aliases for an entity
//   POST   /entities/{id}/aliases             add an alias
//                                             body: {alias, source?}
//   DELETE /entities/{id}/aliases/{alias}     remove one alias
//   GET    /entities/resolve?alias=...        canonical names for an alias
//   GET    /entities/expand?query=...         alias-expanded canonical names
//                                             (what /recall does internally)
package main

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

func (s *Server) handleEntities(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if s.bank == nil {
		http.Error(w, "bank disabled", http.StatusServiceUnavailable)
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, "/entities")
	rest = strings.TrimPrefix(rest, "/")

	// /entities/resolve?alias=...
	if rest == "resolve" {
		s.handleEntityResolve(w, r)
		return
	}
	// /entities/expand?query=...
	if rest == "expand" {
		s.handleEntityExpand(w, r)
		return
	}

	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			s.handleEntityList(w, r)
		case http.MethodPost:
			s.handleEntityUpsert(w, r)
		default:
			methodNotAllowed(w, "GET", "POST", "OPTIONS")
		}
		return
	}

	// /entities/{id}[/aliases[/{alias}]]
	parts := strings.SplitN(rest, "/", 3)
	id := parts[0]
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			s.handleEntityGet(w, r, id)
		case http.MethodDelete:
			s.handleEntityDelete(w, r, id)
		default:
			methodNotAllowed(w, "GET", "DELETE", "OPTIONS")
		}
		return
	}

	if parts[1] == "aliases" {
		if len(parts) == 2 {
			switch r.Method {
			case http.MethodGet:
				s.handleAliasList(w, r, id)
			case http.MethodPost:
				s.handleAliasAdd(w, r, id)
			default:
				methodNotAllowed(w, "GET", "POST", "OPTIONS")
			}
			return
		}
		// /entities/{id}/aliases/{alias}
		alias := parts[2]
		if r.Method == http.MethodDelete {
			s.handleAliasRemove(w, r, id, alias)
			return
		}
		methodNotAllowed(w, "DELETE", "OPTIONS")
		return
	}

	http.Error(w, "unknown sub-resource", http.StatusNotFound)
}

func (s *Server) handleEntityList(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	rows, err := s.bank.ListEntities(limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(rows), "entities": rows})
}

func (s *Server) handleEntityUpsert(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID            string `json:"id"`
		CanonicalName string `json:"canonical_name"`
		Kind          string `json:"kind"`
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
		return
	}
	got, err := s.bank.UpsertEntity(Entity{
		ID:            body.ID,
		CanonicalName: body.CanonicalName,
		Kind:          body.Kind,
	})
	if err != nil {
		// canonical_name validation errors → 400
		if strings.Contains(err.Error(), "required") {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditWrite(AuditEntry{
		Operation: "entity_upsert", EntityType: "entity", EntityID: got.ID,
		AdapterID: adapterIDFromRequest(r),
	})
	writeJSON(w, http.StatusOK, got)
}

func (s *Server) handleEntityGet(w http.ResponseWriter, r *http.Request, id string) {
	got, err := s.bank.GetEntity(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (s *Server) handleEntityDelete(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.bank.DeleteEntity(id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditWrite(AuditEntry{
		Operation: "entity_delete", EntityType: "entity", EntityID: id,
		AdapterID: adapterIDFromRequest(r),
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAliasList(w http.ResponseWriter, r *http.Request, id string) {
	rows, err := s.bank.ListAliases(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(rows), "aliases": rows})
}

func (s *Server) handleAliasAdd(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Alias  string `json:"alias"`
		Source string `json:"source"`
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.bank.AddAlias(id, body.Alias, body.Source); err != nil {
		if strings.Contains(err.Error(), "empty") {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditWrite(AuditEntry{
		Operation: "entity_alias_add", EntityType: "entity_alias", EntityID: id,
		Reason:    body.Alias,
		AdapterID: adapterIDFromRequest(r),
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAliasRemove(w http.ResponseWriter, r *http.Request, id, alias string) {
	if err := s.bank.RemoveAlias(id, alias); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditWrite(AuditEntry{
		Operation: "entity_alias_remove", EntityType: "entity_alias", EntityID: id,
		Reason:    alias,
		AdapterID: adapterIDFromRequest(r),
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleEntityResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET", "OPTIONS")
		return
	}
	alias := r.URL.Query().Get("alias")
	canonicals, err := s.bank.ResolveAlias(alias)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"alias":      alias,
		"canonicals": canonicals,
		"count":      len(canonicals),
	})
}

func (s *Server) handleEntityExpand(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET", "OPTIONS")
		return
	}
	q := r.URL.Query().Get("query")
	expansions, err := s.bank.ExpandQueryViaAliases(q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"query":      q,
		"expansions": expansions,
		"count":      len(expansions),
	})
}

// readJSON is provided by handlers_p5.go — no local definition here.
