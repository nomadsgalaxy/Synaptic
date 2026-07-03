# Hermes Agent passive-observer plugin

Pipes Hermes Agent hook events into Synaptic so the brain dashboard
lights up while Hermes works — region pulses for tool calls, neuron
firing for LLM responses, memory_added / memory_recall when the user
also runs MuninnDB. Same shape as the sibling `claude-code-plugin/`
and `gemini-cli-hooks/` directories.

**Opt-in.** No core Go binary changes. This directory is not advertised
in onboarding; install it manually per the steps below.

## What it does

`forward.js` reads a Hermes hook event JSON on stdin, gets the event
name on argv[2], and translates it into one or more Synaptic events
posted to `/event`. The mapping:

| Hermes event       | Synaptic event(s)                                          |
|--------------------|------------------------------------------------------------|
| `on_session_start` | `session_start`                                            |
| `on_session_end`   | `session_end`                                              |
| `pre_tool_call`    | `model_thinking` + `tool_call` (with `region_hint`)        |
| `post_tool_call`   | `tool_result` — or `memory_added` / `memory_recall` when the tool is `mcp__zmem__muninn_remember` / `muninn_recall` |
| `pre_llm_call`     | `prompt_received`                                          |
| `post_llm_call`    | `response_streaming` ×3 + `response_complete`              |
| `subagent_stop`    | `subagent_complete`                                        |

Region hints follow the same map the Claude Code plugin uses, just
with Hermes' snake_case tool spellings (`terminal`, `read_file`,
`web_fetch`, etc.). See `forward.js → TOOL_REGION` to extend.

## Install

1. Ensure Node.js is on PATH (`node --version` ≥ 18 — uses built-in
   `http`/`https`, no npm install needed).

2. Set SD Core location (host env vars, used at hook-fire time):

   ```bash
   export SD_CORE_URL=http://localhost:9911           # or http://your-host:9911
   export SD_API_TOKEN=<your token, if SD_API_TOKEN is set in Core>
   ```

3. Add the hook block to `~/.hermes/config.yaml`:

   ```yaml
   hooks:
     on_session_start:
       - command: node /path/to/synaptic/bridge/hermes-plugin/forward.js on_session_start
         timeout: 3
     on_session_end:
       - command: node /path/to/synaptic/bridge/hermes-plugin/forward.js on_session_end
         timeout: 3
     pre_tool_call:
       - command: node /path/to/synaptic/bridge/hermes-plugin/forward.js pre_tool_call
         timeout: 3
     post_tool_call:
       - command: node /path/to/synaptic/bridge/hermes-plugin/forward.js post_tool_call
         timeout: 3
     pre_llm_call:
       - command: node /path/to/synaptic/bridge/hermes-plugin/forward.js pre_llm_call
         timeout: 3
     post_llm_call:
       - command: node /path/to/synaptic/bridge/hermes-plugin/forward.js post_llm_call
         timeout: 3
     subagent_stop:
       - command: node /path/to/synaptic/bridge/hermes-plugin/forward.js subagent_stop
         timeout: 3
   hooks_auto_accept: true
   ```

   `hooks_auto_accept: true` keeps Hermes from prompting for consent on
   every new session. Safe here because the hook only reads stdin and
   POSTs to localhost (or whatever `SD_CORE_URL` points at).

4. Restart Hermes. The next session you start should emit a
   `session_start` event into SD Core's `/events` stream within a
   second; confirm with:

   ```bash
   curl -sH "Authorization: Bearer $SD_API_TOKEN" \
     "http://localhost:9911/events?adapter_id=hermes-hooks&limit=5"
   ```

## Notes

- **Exit behaviour:** the script ALWAYS exits 0. SD Core unreachable,
  malformed payload, JSON parse error — all silently dropped. Hermes is
  never blocked.

- **Hard-cap on POST timeout:** 2.5 s per hook. A hung SD Core can't
  slow Hermes down.

- **No npm dependencies.** Only Node built-ins (`http`, `https`, `url`).
  Works on any Node 18+ install without `npm install`.

- **Auto-detect Muninn (zmem) memory ops.** When a tool call name is
  `mcp__zmem__muninn_remember` or `muninn_remember` (and recall
  variants), the post_tool_call hook emits a `memory_added` /
  `memory_recall` event instead of generic `tool_result`. Brain region
  is derived from any `tags` in the args via the Muninn type-mapping
  documented in CHANGES.md item 5.

- **Hermes payload shape:** uses `args` (not `tool_input` like Claude
  Code). The script tries both keys so the plugin tolerates
  cross-version drift.

- **Region map:** extend `TOOL_REGION` in `forward.js` for any custom
  tools you've added to Hermes. Unmapped tools default to
  `motor_cortex`; MCP-namespaced tools (`mcp__*`) default to
  `corpus_callosum`.

## Relationship to the other plugins

`bridge/` ships three passive-observer plugins:

| Directory                | For                | Status                                                          |
|--------------------------|--------------------|-----------------------------------------------------------------|
| `claude-code-plugin/`    | Claude Code CLI    | First-party, documented in onboarding                           |
| `gemini-cli-hooks/`      | Google Gemini CLI  | Opt-in sibling; same shape as Hermes                            |
| `hermes-plugin/`         | Hermes Agent       | Opt-in sibling; same shape as Gemini                            |

All three forward to the same `/event` endpoint with different
`adapter_id` values so the dashboard's activity filter can split them
out.
