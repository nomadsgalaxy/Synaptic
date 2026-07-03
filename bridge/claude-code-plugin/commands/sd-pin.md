---
description: Pin a high-confidence, user-curated memory into Synaptic (protected from dedup/decay, deep-encoded at top priority)
allowed-tools: mcp__synaptic__sd_pin_memory
---

# /sd-pin — Pin a memory as ground-truth

Use the `mcp__synaptic__sd_pin_memory` tool to persist the user's statement as a **pinned** memory — these are protected from dedup + decay and get deep-encoded ahead of every other dirty memory on the next nightly pass.

**Memory content:** $ARGUMENTS

Tag selection guidance:
- Pick 3–5 tags spanning region (`feature`, `decision`, `bugfix`, `architecture`, `tool`), topic (`oauth`, `synaptic`, etc.), and project (`project:<name>`).
- Don't include `pinned` — it's added automatically.
- Use `user:<name>` if the statement is about a specific person.

After pinning, briefly confirm what was saved (id, tag list, region).

If `$ARGUMENTS` is empty, ask the user what they want to pin and what tags fit.
