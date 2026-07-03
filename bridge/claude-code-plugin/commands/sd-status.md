---
description: One-shot Synaptic health check — token spend + recent activity + bank size
allowed-tools: mcp__synaptic__sd_get_budget, mcp__synaptic__sd_get_audit, mcp__synaptic__sd_list_memories
---

# /sd-status — Synaptic health check

Run these three tools in parallel and synthesize the results into a tight one-screen status report:

1. `mcp__synaptic__sd_get_budget` (days=30) — token spend + cap headroom
2. `mcp__synaptic__sd_get_audit` (limit=10) — last 10 write operations
3. `mcp__synaptic__sd_list_memories` (limit=1) — just to surface the total `count` field

Report format:

```
SYNAPTIC STATUS · YYYY-MM-DD HH:MM
─────────────────────────────────
Bank      <count> memories
Budget    <total_tokens>/<cap_monthly> tokens (<pct>% of monthly cap)
          <total_in> in / <total_out> out · last 30 days
Recent    <action> · <entity_type> · <relative-time-ago>
          <action> · <entity_type> · <relative-time-ago>
          ... (top 5)
```

If `cap_monthly` is unset, omit the `/<cap>` and `<pct>%` parts. If any tool errors, report the failure inline rather than guessing.
