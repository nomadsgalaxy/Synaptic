// handlers_admin_budget.go — GET /admin/budget/caps
//
// Returns the budget caps configured via SD_TIER{N}_API_{VENDOR}_BUDGET
// envs alongside the current period spend for each (tier, vendor)
// combination. The dashboard renders this as a "monthly $20 → $4.32
// used (22%)" status line per provider.
//
// Spend is in TOKENS (not dollars) — Synaptic doesn't ship a price
// table because rates change per-vendor / per-model and we don't want
// to silently mislead. The UI can apply its own estimation if desired.
// Token counts come from the same `token_budget_lines` table the
// existing /budget endpoint uses, but filtered by the budget cap's
// period window (not the fixed 30-day default).
package main

import (
	"net/http"
	"time"
)

type budgetCapResponse struct {
	SchemaVersion string             `json:"schema_version"`
	GeneratedAt   string             `json:"generated_at"`
	Caps          []budgetCapEntry   `json:"caps"`
}

type budgetCapEntry struct {
	Tier         string     `json:"tier"`
	Vendor       string     `json:"vendor"`
	BudgetEnv    string     `json:"budget_env"`
	KeyEnv       string     `json:"key_env"`
	KeyCount     int        `json:"key_count"`
	HasKey       bool       `json:"has_key"`
	Spec         BudgetSpec `json:"spec"`
	WindowStart  string     `json:"window_start"`     // YYYY-MM-DD inclusive
	WindowEnd    string     `json:"window_end"`       // YYYY-MM-DD inclusive
	TokensIn     int        `json:"tokens_in"`        // sum across the window
	TokensOut    int        `json:"tokens_out"`
	TokensTotal  int        `json:"tokens_total"`
}

func (s *Server) handleAdminBudgetCaps(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	now := time.Now().UTC()
	today := now.Format("2006-01-02")
	caps := envBudgetKeys()
	out := make([]budgetCapEntry, 0, len(caps))
	for _, c := range caps {
		entry := budgetCapEntry{
			Tier:        c.Tier,
			Vendor:      c.Vendor,
			BudgetEnv:   c.BudgetEnv,
			KeyEnv:      c.KeyEnv,
			KeyCount:    c.KeyCount,
			HasKey:      c.HasKey,
			Spec:        c.Spec,
			WindowStart: budgetWindowStart(c.Spec, now),
			WindowEnd:   today,
		}
		// Aggregate token spend in the cap's period for this tier+vendor.
		// (Future: filter by model too if multi-model rotation lands.)
		if s.bank != nil && entry.WindowStart != "" {
			row := s.bank.db.QueryRow(
				`SELECT COALESCE(SUM(tokens_in), 0), COALESCE(SUM(tokens_out), 0)
				 FROM token_budget_lines
				 WHERE tier = ? AND provider = ? AND date >= ? AND date <= ?`,
				c.Tier, c.Vendor, entry.WindowStart, today,
			)
			_ = row.Scan(&entry.TokensIn, &entry.TokensOut)
			entry.TokensTotal = entry.TokensIn + entry.TokensOut
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, budgetCapResponse{
		SchemaVersion: "1.0",
		GeneratedAt:   now.Format(time.RFC3339),
		Caps:          out,
	})
}
