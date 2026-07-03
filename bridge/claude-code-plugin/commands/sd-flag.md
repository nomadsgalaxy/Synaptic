---
description: Mark a Synaptic memory as sensitive (excluded from external research / Oracle / web fetch)
allowed-tools: mcp__synaptic__sd_recall, mcp__synaptic__sd_update_memory
---

# /sd-flag — Mark a memory sensitive

Workflow:

1. If `$ARGUMENTS` looks like a memory id (UUID format), skip step 2 and use it directly.
2. Otherwise, call `mcp__synaptic__sd_recall` with `$ARGUMENTS` as the query, limit=5. Show the user the matches and ask which id to flag (one number 1–5).
3. Call `mcp__synaptic__sd_update_memory` with `id: <chosen>` and `sensitive: true`.
4. Confirm the result. Note: setting `sensitive: false` is sticky — backend may keep it on if content still triggers a privacy regex.

Sensitive memories are filtered from any Tier 3 / Oracle / web-research call so their content stays inside the local brain.
