---
description: Trigger a Tier 3 Oracle web-research query through Synaptic (cached, budget-tracked, sensitive memories filtered)
allowed-tools: mcp__synaptic__sd_research, mcp__synaptic__sd_get_budget
---

# /sd-research — Oracle research via Synaptic

Run a Tier 3 Oracle web-research query through `mcp__synaptic__sd_research`:

**Query:** $ARGUMENTS

Before running the actual research, briefly check `mcp__synaptic__sd_get_budget` (days=30) and warn the user if `total_tokens` is within 10% of `cap_monthly` — Oracle calls are real spend.

After the research returns, summarize the response in 3-5 bullets and surface:
- `cache_hit` (true means no new tokens spent)
- `sensitive_excluded` count if non-zero (memories that didn't reach the Oracle prompt)
- `tokens_total` actual spend on this query

If the source citations are present, list them. If `cache_hit:true`, mention the original `cached_at` timestamp.
