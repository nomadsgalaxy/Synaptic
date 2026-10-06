#!/usr/bin/env node
// forward.js — Hermes Agent passive observer → Synaptic SD Core.
//
// Hermes Agent (agent/shell_hooks.py) ships a shell-hook system that is
// structurally identical to Claude Code's: each hook event is serialised
// as JSON on stdin and passed to a shell command, plus the event name on
// argv[2]. This script translates Hermes hook events into Synaptic
// events and POSTs them to /event.
//
// Wiring (in ~/.hermes/config.yaml, see README.md for the full block):
//   hooks:
//     on_session_start: [{ command: node /path/to/forward.js on_session_start, timeout: 3 }]
//     on_session_end:   [...]
//     pre_tool_call:    [...]
//     post_tool_call:   [...]
//     pre_llm_call:     [...]
//     post_llm_call:    [...]
//     subagent_stop:    [...]
//   hooks_auto_accept: true
//
// Exit behaviour: ALWAYS exit 0 unconditionally. An unreachable SD Core
// or a malformed payload is a silent drop — Hermes itself is unaffected.

const http = require('http');
const https = require('https');
const url = require('url');

let input = '';
process.stdin.on('data', chunk => { input += chunk; });
process.stdin.on('end', () => {
  let events = [];
  try {
    const payload = input.length > 0 ? JSON.parse(input) : {};
    const eventName = process.argv[2];
    const translated = translate(eventName, payload);
    if (translated) {
      events = Array.isArray(translated) ? translated : [translated];
    }
  } catch (_) {
    // Silent fail — never block Hermes on this.
  }
  // Wait for all HTTP requests to complete before exiting so the
  // events actually reach SD Core. (process.exit(0) here used to
  // kill the process before the async requests could finish.)
  if (events.length > 0) {
    Promise.all(events.map(postEvent)).finally(() => process.exit(0));
  } else {
    process.exit(0);
  }
});

// ── Config (env-overridable) ─────────────────────────────────────────
const SD_CORE_URL = process.env.SD_CORE_URL || 'http://localhost:9911';
const SD_API_TOKEN = process.env.SD_API_TOKEN || '';
const ADAPTER_ID = 'hermes-hooks';

// ── Tool name → brain region mapping ─────────────────────────────────
// Hermes ships snake_case tool names by convention (`terminal`,
// `read_file`, etc.). Mirror the claude-code-plugin's TOOL_REGION map
// but with the Hermes spellings. Anything not pinned here falls into
// motor_cortex via regionForTool.
const TOOL_REGION = {
  // Filesystem
  read_file:       'visual_cortex',
  read:            'visual_cortex',
  list_files:      'parietal_lobe',
  glob:            'parietal_lobe',
  grep:            'parietal_lobe',
  write_file:      'broca_area',
  write:           'broca_area',
  edit_file:       'motor_cortex',
  edit:            'motor_cortex',
  multi_edit:      'motor_cortex',
  // Shell / system
  terminal:        'motor_cortex',
  bash:            'motor_cortex',
  shell:           'motor_cortex',
  exec:            'motor_cortex',
  // Web
  web_fetch:       'temporal_lobe_left',
  web_search:      'temporal_lobe_left',
  fetch:           'temporal_lobe_left',
  search:          'temporal_lobe_left',
  // Planning / orchestration
  task:            'prefrontal_cortex',
  todo_write:      'prefrontal_cortex',
  plan:            'prefrontal_cortex',
  // Notebooks
  notebook_edit:   'motor_cortex',
};

function regionForTool(toolName) {
  if (!toolName) return null;
  const lower = String(toolName).toLowerCase();
  if (TOOL_REGION[lower]) return TOOL_REGION[lower];
  // MCP tools land in corpus_callosum (cross-hemisphere coordinator)
  // by convention with the claude-code-plugin.
  if (lower.startsWith('mcp__')) return 'corpus_callosum';
  return 'motor_cortex';
}

// ── Muninn (zmem) memory operations: detect and map ──────────────────
// The submitter runs Hermes alongside MuninnDB. When a tool call hits
// mcp__zmem__muninn_remember / muninn_recall, surface it as a Synaptic
// memory event rather than a generic tool_call, with a region hint
// derived from any tags in the args.
function muninnRegionFromArgs(args) {
  const tags = (args && Array.isArray(args.tags)) ? args.tags : [];
  // Same convention as the claude-code-plugin's tag→region mapping.
  for (const tag of tags) {
    const t = String(tag).toLowerCase();
    if (t === 'user' || t === 'identity')      return 'frontal_lobe';
    if (t === 'feedback' || t === 'emotion')   return 'amygdala';
    if (t === 'project' || t === 'planning')   return 'prefrontal_cortex';
    if (t === 'reference' || t === 'doc')      return 'corpus_callosum';
  }
  return 'hippocampus';
}

// ── Hermes event → SD Core event(s) ──────────────────────────────────
function translate(hermesEvent, payload) {
  const ts = new Date().toISOString();
  const sessionId = payload.session_id || payload.session || undefined;
  const base = {
    schema_version: '1.0',
    timestamp: ts,
    adapter_id: ADAPTER_ID,
    session_id: sessionId,
  };

  // Hermes uses `args` (not `tool_input`) for tool-call arguments.
  // Tool name lives under `tool` or `tool_name` depending on Hermes
  // version — we try both.
  const toolName = payload.tool_name || payload.tool;
  const args = payload.args || payload.tool_input || {};

  switch (hermesEvent) {
    case 'on_session_start':
      return {
        ...base,
        type: 'session_start',
        payload: {
          client: 'Hermes Agent',
          cwd: payload.cwd || payload.workdir,
        },
      };

    case 'on_session_end':
      return {
        ...base,
        type: 'session_end',
        payload: {
          turns: payload.turns,
          duration_ms: payload.duration_ms,
        },
      };

    case 'pre_tool_call':
      return [
        // Indicate the agent is reasoning about which tool to invoke.
        { ...base, type: 'model_thinking', payload: { region_hint: 'frontal_lobe', tool_name: toolName } },
        // The tool call itself.
        {
          ...base,
          type: 'tool_call',
          payload: {
            tool_name: toolName,
            arg_keys: Object.keys(args),
            region_hint: regionForTool(toolName),
            tool_call_id: payload.tool_call_id || payload.id,
          },
        },
      ];

    case 'post_tool_call': {
      // Muninn (zmem) detect — surface as memory_added / memory_recall
      // rather than generic tool_result. Falls back to tool_result for
      // anything else.
      const lower = String(toolName || '').toLowerCase();
      if (lower === 'mcp__zmem__muninn_remember' || lower === 'muninn_remember') {
        return {
          ...base,
          type: 'memory_added',
          payload: {
            tool_name: toolName,
            region_hint: muninnRegionFromArgs(args),
            ok: payload.ok !== false,
            source: 'muninn',
          },
        };
      }
      if (lower === 'mcp__zmem__muninn_recall' || lower === 'muninn_recall') {
        return {
          ...base,
          type: 'memory_recall',
          payload: {
            tool_name: toolName,
            region_hint: muninnRegionFromArgs(args),
            ok: payload.ok !== false,
            source: 'muninn',
          },
        };
      }
      return {
        ...base,
        type: 'tool_result',
        payload: {
          tool_name: toolName,
          region_hint: regionForTool(toolName),
          ok: payload.ok !== false,
          duration_ms: payload.duration_ms,
          tool_call_id: payload.tool_call_id || payload.id,
        },
      };
    }

    case 'pre_llm_call':
      return {
        ...base,
        type: 'prompt_received',
        payload: {
          model: payload.model,
          token_estimate: payload.token_estimate,
        },
      };

    case 'post_llm_call':
      // Mirror the claude-code-plugin: simulate a brief stream of
      // response_streaming events followed by a completion. The
      // dashboard's neuron firing animation expects multiple pulses to
      // look natural; three is the minimum that reads as motion rather
      // than a single blink.
      return [
        { ...base, type: 'response_streaming', payload: { phase: 'start' } },
        { ...base, type: 'response_streaming', payload: { phase: 'mid' } },
        { ...base, type: 'response_streaming', payload: { phase: 'end' } },
        {
          ...base,
          type: 'response_complete',
          payload: {
            model: payload.model,
            tokens_in: payload.tokens_in,
            tokens_out: payload.tokens_out,
            duration_ms: payload.duration_ms,
          },
        },
      ];

    case 'subagent_stop':
      return {
        ...base,
        type: 'subagent_complete',
        payload: {
          subagent_name: payload.subagent_name || payload.name,
          duration_ms: payload.duration_ms,
        },
      };

    default:
      return null;
  }
}

// ── POST helper ──────────────────────────────────────────────────────
function postEvent(event) {
  return new Promise((resolve) => {
    let body;
    try { body = JSON.stringify(event); } catch (_) { return resolve(); }
    const u = new url.URL(SD_CORE_URL);
    const transport = u.protocol === 'https:' ? https : http;
    const headers = {
      'Content-Type': 'application/json',
      'Content-Length': Buffer.byteLength(body),
    };
    if (SD_API_TOKEN) {
      headers['Authorization'] = 'Bearer ' + SD_API_TOKEN;
    }
    const req = transport.request({
      hostname: u.hostname,
      port: u.port || (u.protocol === 'https:' ? 443 : 80),
      path: (u.pathname === '/' ? '' : u.pathname) + '/event',
      method: 'POST',
      headers,
      timeout: 2500, // hard cap so a hung Core doesn't slow Hermes
    });
    req.on('response', () => { try { req.destroy(); } catch (_) {} resolve(); });
    req.on('error', () => resolve()); // silent drop
    req.on('timeout', () => { try { req.destroy(); } catch (_) {} resolve(); });
    req.write(body);
    req.end();
  });
}
