// handlers_memory_coverage.go — GET /memories/coverage
//
// Live (current-instant) memory coverage counts read directly from the
// bank, not from a nightly-run stats snapshot. The Dream Journal's
// deep-encoding card uses this on render + on backfill completion so
// the ambient embedding / deep-encoding progress bars always reflect
// right-now state instead of the snapshot baked into runs[0].stats.
//
// Sub-path of /memories/* (not /admin/*) on purpose:
//   - Cloudflare Access policies in remote-tunnel deployments commonly
//     gate /admin/* with OAuth/SSO redirects, blocking SD Core's bearer
//     auth before the request ever reaches the handler (see v2.1.6b1's
//     tunnel-routing note).
//   - Historical cloudflared ingress regexes ship with `memories` in the
//     prefix list, so /memories/coverage routes without any tunnel
//     regex update on the user's end.
// SD Core's own bearer-token auth is sufficient (read-only diagnostic).
//
// Response shape:
//
//	{
//	  "live": 3666,
//	  "embedded": 3645,
//	  "unembedded": 21,
//	  "deep_encoded": 8,
//	  "dirty": 3658,
//	  "dirty_by_reason": {"never_encoded": 2733, "model_upgrade": 867, ...}
//	}
package main

import (
	"net/http"
)

func (s *Server) handleMemoryCoverage(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET", "OPTIONS")
		return
	}
	if s.bank == nil {
		http.Error(w, "bank disabled", http.StatusServiceUnavailable)
		return
	}
	live, embedded, err := s.bank.CountEmbeddedMemories()
	if err != nil {
		http.Error(w, "count embedded: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_, encoded, err := s.bank.CountDeepEncoded()
	if err != nil {
		http.Error(w, "count deep-encoded: "+err.Error(), http.StatusInternalServerError)
		return
	}
	dirty, byReason, err := s.bank.CountDirtyMemories()
	if err != nil {
		http.Error(w, "count dirty: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"live":            live,
		"embedded":        embedded,
		"unembedded":      live - embedded,
		"deep_encoded":    encoded,
		"dirty":           dirty,
		"dirty_by_reason": byReason,
	})
}
