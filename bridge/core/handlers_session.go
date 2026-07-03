// handlers_session.go — Wave 7c2 session/dirty endpoints.
//
// Two endpoints:
//
//   GET  /session/dirty           — the section→last-dirtied-at snapshot
//   POST /session/clear/{section} — best-effort ack that the client has
//                                   refreshed its local copy of {section}
//
// Design choice (deviates from the handoff's literal spec): we don't
// maintain per-subscriber dirty maps server-side. Instead, the server
// reports a single global "last dirtied" timestamp per section + the
// client tracks its own "last fetched" timestamps client-side. The
// client decides what's stale by comparing the two.
//
// Why this design rather than per-subscriber bitmaps:
//
//   1. No subscriber-to-HTTP-request mapping needed (HTTP requests don't
//      naturally identify a WS connection without a session-id cookie
//      we don't currently have).
//   2. Multi-tab clients on the same browser each track independently —
//      desired behavior (each tab's "what have I seen" is its own state).
//   3. Survives WS reconnects without state sync.
//   4. Memory cost is O(num_sections), not O(num_subscribers * num_sections).
//
// POST /session/clear/{section} is preserved for handoff compliance but
// is a structured no-op (acknowledges + returns ok) — the client owns
// its own "last fetched" state.
package main

import (
	"net/http"
	"strings"
	"time"
)

// lazyFetchEnabled reports whether the wave-7c2 cursor-aware list
// endpoints should include `next_cursor` + `section_clean_at` fields in
// their responses. Default false — rollout is reversible. When false,
// list endpoints accept `since_cursor` as an alias for the existing
// `since` filter but the response shape stays exactly as it was pre-7c2.
func lazyFetchEnabled() bool {
	return envOr("SD_LAZY_FETCH_ENABLED", "0") == "1"
}

// pickSinceCursor returns the cursor from the request, accepting either
// the wave-7c2 `since_cursor` query param or the legacy `since` for
// continuity with older clients. Empty when neither is set.
func pickSinceCursor(r *http.Request) string {
	q := r.URL.Query()
	if v := q.Get("since_cursor"); v != "" {
		return v
	}
	return q.Get("since")
}

// handleSessionDirty serves GET /session/dirty. Returns the global
// section-state snapshot. Sections never dirtied are reported as null.
//
// Response shape:
//
//   {
//     "audit": "2026-05-11T00:00:00Z",
//     "maps":  null,
//     ...
//   }
//
// The frontend compares each section's value against its own last-fetched
// timestamp; when server's > client's, that tab gets a stale badge.
func (s *Server) handleSessionDirty(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET", "OPTIONS")
		return
	}
	if s.hub == nil {
		http.Error(w, "hub not configured", http.StatusServiceUnavailable)
		return
	}
	state := s.hub.SectionState()
	// JSON-friendly shape: zero-value time → null, otherwise RFC3339Nano.
	out := map[string]any{}
	for section, t := range state {
		if t.IsZero() {
			out[section] = nil
		} else {
			out[section] = t.Format(time.RFC3339Nano)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSessionClear serves POST /session/clear/{section}. Per the
// design above, this is a no-op — clients own their last-fetched state.
// Kept for handoff-compliance + future per-subscriber state if needed.
//
// Returns 400 when {section} isn't in the known set so a frontend typo
// surfaces immediately.
func (s *Server) handleSessionClear(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST", "OPTIONS")
		return
	}
	section := strings.TrimPrefix(r.URL.Path, "/session/clear/")
	section = strings.Trim(section, "/")
	if section == "" {
		http.Error(w, "missing section name", http.StatusBadRequest)
		return
	}
	known := false
	for _, n := range knownSections {
		if n == section {
			known = true
			break
		}
	}
	if !known {
		http.Error(w, "unknown section name", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"section":    section,
		"cleared_at": time.Now().UTC().Format(time.RFC3339Nano),
		"note":       "Acknowledged. Clients own their last-fetched state; this endpoint is informational.",
	})
}
