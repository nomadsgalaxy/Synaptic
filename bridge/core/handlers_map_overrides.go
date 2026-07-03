// handlers_map_overrides.go — server-side map-overrides store.
//
// Replaces the dashboard's per-browser `localStorage['sd:atlas:map-overrides']`
// blob so multi-device users share Project / Entity / Topic categorisations
// across sessions. Frontend migration plan (per the handoff): on first GET
// returning the new shape, dashboard uploads its localStorage entries via
// PUTs and then clears local state.
//
// Endpoints:
//
//   GET    /maps/overrides              list all overrides
//   PUT    /maps/overrides/{map_key}    upsert one
//   DELETE /maps/overrides/{map_key}    clear one
//
// `map_key` is opaque on the backend — the frontend passes lowercased tag
// names or `cluster:<id>` strings. Stored COLLATE NOCASE so case differences
// don't bifurcate state. URL-encoded on the wire (the dashboard already
// `encodeURIComponent`s these).
package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// MapOverride is one row in the map_overrides table.
type MapOverride struct {
	MapKey    string `json:"map_key"`
	Category  string `json:"category,omitempty"` // project|entity|topic|"" (empty = no category)
	Alias     string `json:"alias,omitempty"`
	Archived  bool   `json:"archived"`
	UpdatedAt string `json:"updated_at"`
	UpdatedBy string `json:"updated_by,omitempty"`
}

// validMapCategories is the closed set of categories users can assign to
// a map. project/entity/topic are the original taxonomy from the maps
// overrides handoff; concept/technology were added 2026-05-09 to mark
// publicly-knowable maps that the research-augmentation pipeline is
// allowed to fill in via Tier 3 (per the augmentation handoff's strict
// category-gate rule). Everything else (project/entity/personal) is
// HARD-skipped by the augmentation engine.
var validMapCategories = map[string]bool{
	"":           true, // null / "no category" — explicit allowed
	"project":    true,
	"entity":     true,
	"topic":      true,
	"concept":    true, // augmentable
	"technology": true, // augmentable
}

// augmentableMapCategories are the categories the research-augmentation
// engine treats as opt-in. The user must EXPLICITLY tag a map as one of
// these for it to ever be a candidate. project/entity/personal stay
// off-limits regardless of thinness.
var augmentableMapCategories = map[string]bool{
	"topic":      true,
	"concept":    true,
	"technology": true,
}

// SaveMapOverride upserts a row by map_key. Empty category means "no
// category" — kept distinct from "row absent" so the dashboard can clear
// just the category without removing the alias / archived bits.
func (b *Bank) SaveMapOverride(o MapOverride) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	if o.MapKey == "" {
		return errors.New("map_key is required")
	}
	if !validMapCategories[o.Category] {
		return errors.New("category must be one of: project, entity, topic, or empty")
	}
	o.UpdatedAt = nowUTC()
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_, err := b.db.Exec(`
INSERT INTO map_overrides (map_key, category, alias, archived, updated_at, updated_by)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(map_key) DO UPDATE SET
    category   = excluded.category,
    alias      = excluded.alias,
    archived   = excluded.archived,
    updated_at = excluded.updated_at,
    updated_by = excluded.updated_by`,
		o.MapKey, o.Category, o.Alias, boolToInt(o.Archived), o.UpdatedAt, o.UpdatedBy,
	)
	return err
}

// GetMapOverride returns one row by map_key. sql.ErrNoRows when absent.
func (b *Bank) GetMapOverride(key string) (MapOverride, error) {
	if b == nil {
		return MapOverride{}, errors.New("bank not enabled")
	}
	var o MapOverride
	var arch int
	err := b.db.QueryRow(
		`SELECT map_key, category, alias, archived, updated_at, updated_by
		 FROM map_overrides WHERE map_key = ?`, key,
	).Scan(&o.MapKey, &o.Category, &o.Alias, &arch, &o.UpdatedAt, &o.UpdatedBy)
	o.Archived = arch != 0
	return o, err
}

// ListMapOverrides returns all rows. Suitable for the small expected size
// (~hundreds at most). The frontend caches and reads this once per Maps-
// tab open.
func (b *Bank) ListMapOverrides() ([]MapOverride, error) {
	if b == nil {
		return nil, errors.New("bank not enabled")
	}
	rows, err := b.db.Query(
		`SELECT map_key, category, alias, archived, updated_at, updated_by
		 FROM map_overrides ORDER BY map_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MapOverride{}
	for rows.Next() {
		var o MapOverride
		var arch int
		if err := rows.Scan(&o.MapKey, &o.Category, &o.Alias, &arch, &o.UpdatedAt, &o.UpdatedBy); err != nil {
			return nil, err
		}
		o.Archived = arch != 0
		out = append(out, o)
	}
	return out, rows.Err()
}

// DeleteMapOverride removes one row. sql.ErrNoRows when nothing matched.
func (b *Bank) DeleteMapOverride(key string) error {
	if b == nil {
		return errors.New("bank not enabled")
	}
	b.wmu.Lock()
	defer b.wmu.Unlock()
	res, err := b.db.Exec(`DELETE FROM map_overrides WHERE map_key = ?`, key)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ──────────────────────────────────────────────────────────────────────────
// HTTP
// ──────────────────────────────────────────────────────────────────────────

// mapsOverridesRoot dispatches /maps/overrides[/{key}].
//
// CRITICAL: this handler is called from `mapsByID`'s sub-resource branch,
// so it slots into the existing dispatcher BEFORE the generic /maps/{id}
// handler would interpret "overrides" as a map id. (Same route-precedence
// trap as /admin/env_write etc.)
func (s *Server) mapsOverridesRoot(w http.ResponseWriter, r *http.Request, tail []string) {
	if s.bank == nil {
		http.Error(w, "bank disabled", http.StatusServiceUnavailable)
		return
	}
	switch len(tail) {
	case 0:
		// /maps/overrides — collection ops
		switch r.Method {
		case http.MethodGet:
			out, err := s.bank.ListMapOverrides()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			// Shape per the handoff: { overrides: { <map_key>: {...} } } —
			// keyed object, not an array, so the dashboard's lookup is O(1).
			indexed := make(map[string]MapOverride, len(out))
			for _, o := range out {
				indexed[o.MapKey] = o
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"count":     len(out),
				"overrides": indexed,
			})
			// Set the "server is now the source of truth" hint so the
			// frontend knows to migrate its localStorage on first GET.
			w.Header().Set("X-Maps-Persistence", "server")
		default:
			methodNotAllowed(w, "GET", "OPTIONS")
		}
	case 1:
		key, _ := url.PathUnescape(tail[0])
		if key == "" {
			http.Error(w, "map_key is required", http.StatusBadRequest)
			return
		}
		switch r.Method {
		case http.MethodGet:
			row, err := s.bank.GetMapOverride(key)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, http.StatusOK, row)
		case http.MethodPut:
			var body struct {
				Category *string `json:"category"`
				Alias    *string `json:"alias"`
				Archived *bool   `json:"archived"`
			}
			if err := readJSON(r, &body); err != nil {
				http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
				return
			}
			// Read existing so PUT semantics are partial-patch (passing
			// `null` for a field leaves it unchanged). Frontend treats
			// PUT like a setter for the full row but tolerates partial
			// inputs gracefully.
			existing, err := s.bank.GetMapOverride(key)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			next := existing
			next.MapKey = key
			if body.Category != nil {
				next.Category = strings.ToLower(strings.TrimSpace(*body.Category))
			}
			if body.Alias != nil {
				next.Alias = *body.Alias
			}
			if body.Archived != nil {
				next.Archived = *body.Archived
			}
			next.UpdatedBy = adapterIDFromRequest(r)
			if err := s.bank.SaveMapOverride(next); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			s.auditWrite(AuditEntry{
				Operation: "upsert", EntityType: "map_override", EntityID: key,
				BeforeJSON: mustJSON(existing), AfterJSON: mustJSON(next),
				AdapterID: adapterIDFromRequest(r),
			})
			row, _ := s.bank.GetMapOverride(key)
			writeJSON(w, http.StatusOK, row)
		case http.MethodDelete:
			before, _ := s.bank.GetMapOverride(key)
			if err := s.bank.DeleteMapOverride(key); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			s.auditWrite(AuditEntry{
				Operation: "delete", EntityType: "map_override", EntityID: key,
				BeforeJSON: mustJSON(before),
				AdapterID:  adapterIDFromRequest(r),
			})
			w.WriteHeader(http.StatusNoContent)
		default:
			methodNotAllowed(w, "GET", "PUT", "DELETE", "OPTIONS")
		}
	default:
		http.Error(w, "bad path", http.StatusBadRequest)
	}
	_ = json.Marshal // keep encoding/json import live for mustJSON via handlers_p5
}
