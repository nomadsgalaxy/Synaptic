// handlers_admin_router_reload.go — POST /admin/router/reload
//
// Re-reads every Tier (1/2/3) from the bank's provider settings, rebuilds
// each provider, and atomically swaps them onto the live ModelRouter. PUT
// /settings/providers/{tier} already does this for a single tier on every
// save — this endpoint is the explicit "full re-sync from DB" fallback
// for recovery / manual override / scripted bulk changes.
//
// Response shape:
//
//	{
//	  "reloaded": ["tier1_provider", "tier2_provider", "tier3_provider"],
//	  "active": {
//	    "tier1_provider": {"name": "ollama:llama3.2:3b", "local": true},
//	    "tier2_provider": {"name": "airllm:meta-llama/Llama-3.1-70B-Instruct", "local": true},
//	    "tier3_provider": null
//	  }
//	}
package main

import (
	"log"
	"net/http"
)

func (s *Server) handleAdminRouterReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST", "OPTIONS")
		return
	}
	if s.router == nil {
		http.Error(w, "router not initialized", http.StatusServiceUnavailable)
		return
	}

	reloaded := []string{}
	active := map[string]interface{}{}

	for _, tier := range []TierKey{Tier1, Tier2, Tier3} {
		newProvider := buildOneTierFromSettingsOrEnv(s.bank, tier)
		old, ok := s.router.ReplaceTier(tier, newProvider)
		if !ok {
			continue
		}
		reloaded = append(reloaded, string(tier))
		oldName := "<none>"
		if old != nil {
			oldName = old.Name()
		}
		if newProvider == nil {
			log.Printf("router: %s reloaded %s → <unconfigured>", tier, oldName)
			active[string(tier)] = nil
		} else {
			log.Printf("router: %s reloaded %s → %s", tier, oldName, newProvider.Name())
			active[string(tier)] = map[string]interface{}{
				"name":  newProvider.Name(),
				"local": newProvider.IsLocal(),
			}
		}
	}

	s.auditWrite(AuditEntry{
		Operation:  "router_reload",
		EntityType: "setting",
		EntityID:   "providers",
		AfterJSON:  mustJSON(map[string]interface{}{"reloaded": reloaded}),
		Reason:     "router reloaded from DB via /admin/router/reload",
		AdapterID:  adapterIDFromRequest(r),
	})

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"reloaded": reloaded,
		"active":   active,
		"note":     "All tiers re-read from DB and atomically swapped on the live router.",
	})
}
