#!/usr/bin/env node
// =============================================================================
// Synaptic — Claude Code hook forwarder
//
// Reads a Claude Code hook payload from stdin (one JSON object), translates it
// into a Synaptic event (per docs/event-schema.md v1.0), and POSTs it
// to SD Core at http://localhost:9911/event.
//
// Invocation:
//   node forward.js <HookEventName>
//
// Where <HookEventName> is one of (the real Claude Code hook events):
//   SessionStart | SessionEnd | UserPromptSubmit |
//   PreToolUse   | PostToolUse | SubagentStop | Stop
// (There is NO PostToolUseFailure event — tool failures arrive on
//  PostToolUse and are detected there.)
//
// Design rules:
//   - NEVER block Claude Code. We exit 0 unconditionally.
//   - Network call is best-effort; if SD Core is down, we silently drop.
//   - No third-party deps; uses only Node's built-in `http`.
//   - Total timeout < 1.5s so the hook adds negligible latency.
// =============================================================================

const http  = require('http');
const https = require('https');
const url   = require('url');
const fs    = require('fs');
const path  = require('path');
const os    = require('os');

// -----------------------------------------------------------------------------
// Config (config.json → env vars → built-in defaults, in that priority order)
// -----------------------------------------------------------------------------
// Edit hooks/config.json to point at a remote SD Core or add an API token.
// Env vars set in the hooks.json env block take precedence over config.json.
try {
  const _cfg = JSON.parse(fs.readFileSync(path.join(__dirname, 'config.json'), 'utf8'));
  for (const [k, v] of Object.entries(_cfg)) {
    if (v && !process.env[k]) process.env[k] = String(v);
  }
} catch (_) {}

// Remote-mode preferred path: SD_CORE_URL = "https://cognito.example.com".
// Backwards-compatible HOST + PORT pair still works for localhost-only setups.
// SD_API_TOKEN is a Bearer token the matching SD Core was started with;
// when set, every POST adds Authorization: Bearer <token>.
const SD_CORE_URL  = process.env.SD_CORE_URL || '';
const SD_CORE_HOST = process.env.SD_CORE_HOST || '127.0.0.1';
const SD_CORE_PORT = parseInt(process.env.SD_CORE_PORT || '9911', 10);
const SD_API_TOKEN = process.env.SD_API_TOKEN || '';
const ADAPTER_ID   = process.env.SD_ADAPTER_ID   || 'claude-code-hooks';
// Claude Code doesn't currently expose the active model in hook payloads.
// Users who want a meaningful HUD model line can set this env var; otherwise
// we report the adapter id ("claude-code") as a placeholder.
const MODEL_NAME   = process.env.SD_MODEL || process.env.CLAUDE_CODE_MODEL || 'claude-code';
const REQUEST_TIMEOUT_MS = 1500;
// Rich capture (opt-in, default OFF) — forward the SUBSTANCE of each tool
// call (the file path / command / query / URL) and a short result snippet,
// so the hook-synthesis engine can write memories that say WHAT was done,
// not just which tool ran. Everything forwarded passes through
// redactSecrets() first.
//
// The control is the SERVER setting `hook_synthesis_rich_capture` (GUI-
// toggleable in the dashboard Privacy tab). The hook can't block on a GET
// per event, so it reads a short-lived local cache file synchronously for
// the gate decision, and refreshes that cache after the event POST — awaited
// so the write actually lands before this short-lived process exits, but only
// doing network work when the cache is stale (~1 GET per 5-min window). The
// SD_RICH_CAPTURE env var is a hard override (1/0) for
// power users / testing. Default (no env, no/false cache) = OFF.
const SD_RICH_CAPTURE_ENV = process.env.SD_RICH_CAPTURE || '';        // '' | '1' | '0' | ...
const RICH_CACHE_DIR  = path.join(os.tmpdir(), 'synaptic-hooks');
const RICH_CACHE_FILE = path.join(RICH_CACHE_DIR, 'rich_capture.json');
const RICH_CACHE_TTL_MS = 300000; // 5 min — caps the GET to ~1 per window

// readRichCaptureCacheSync — 0-wait local read; returns true/false or null
// (missing/stale/unreadable).
function readRichCaptureCacheSync() {
  try {
    const c = JSON.parse(fs.readFileSync(RICH_CACHE_FILE, 'utf8'));
    if (c && typeof c.value === 'boolean' && c.refreshedAt &&
        (Date.now() - c.refreshedAt) < RICH_CACHE_TTL_MS) {
      return c.value;
    }
  } catch (_) {}
  return null;
}

// shouldCaptureRich — synchronous gate. Env override wins; else the cached
// server setting; else OFF.
function shouldCaptureRich() {
  if (/^(1|true|on|yes)$/i.test(SD_RICH_CAPTURE_ENV)) return true;
  if (/^(0|false|off|no)$/i.test(SD_RICH_CAPTURE_ENV)) return false;
  return readRichCaptureCacheSync() === true;
}

// maybeRefreshRichCaptureCache — GET the server setting when the cache is
// missing/stale and persist it. Returns a Promise that resolves once the
// write lands (or on timeout/error/fresh-cache). MUST be awaited: a bare
// fire-and-forget call is killed by the process.exit() at the end of the
// hook before its async response callback can run, so the cache would never
// be written. Resolves on every path; swallows errors; never rejects.
function maybeRefreshRichCaptureCache() {
  return new Promise((resolve) => {
    try {
      // Only refresh when stale — keeps this to ~1 GET per TTL window.
      const c = (() => { try { return JSON.parse(fs.readFileSync(RICH_CACHE_FILE, 'utf8')); } catch (_) { return null; } })();
      if (c && c.refreshedAt && (Date.now() - c.refreshedAt) < RICH_CACHE_TTL_MS) return resolve();
      const headers = { 'Accept': 'application/json' };
      if (SD_API_TOKEN) headers['Authorization'] = `Bearer ${SD_API_TOKEN}`;
      const req = TARGET.transport.request({
        host: TARGET.host, port: TARGET.port,
        path: `${TARGET.pathPrefix}/settings/hook_synthesis_rich_capture`,
        method: 'GET', headers, timeout: 1500,
      }, (res) => {
        const chunks = [];
        res.on('data', (d) => chunks.push(d));
        res.on('end', () => {
          try {
            // 404 (unset key) → default false.
            let value = false;
            if (res.statusCode >= 200 && res.statusCode < 300) {
              const p = JSON.parse(Buffer.concat(chunks).toString('utf8') || '{}');
              const v = String(p.value != null ? p.value : (p.enabled != null ? p.enabled : ''));
              value = /^(1|true|on|yes)$/i.test(v);
            }
            try { fs.mkdirSync(RICH_CACHE_DIR, { recursive: true }); } catch (_) {}
            fs.writeFileSync(RICH_CACHE_FILE, JSON.stringify({ value, refreshedAt: Date.now() }), 'utf8');
          } catch (_) {}
          resolve();
        });
      });
      req.on('timeout', () => { req.destroy(); resolve(); });
      req.on('error', () => resolve());
      req.end();
    } catch (_) { resolve(); }
  });
}

// Resolve {host, port, protocol, pathPrefix, transport} from either SD_CORE_URL
// or the legacy host/port pair. SD_CORE_URL wins when present.
function resolveTarget() {
  if (SD_CORE_URL) {
    const u = new url.URL(SD_CORE_URL);
    return {
      protocol: u.protocol,
      host: u.hostname,
      port: u.port ? parseInt(u.port, 10) : (u.protocol === 'https:' ? 443 : 80),
      pathPrefix: u.pathname.replace(/\/$/, ''),
      transport: u.protocol === 'https:' ? https : http,
    };
  }
  return {
    protocol: 'http:',
    host: SD_CORE_HOST,
    port: SD_CORE_PORT,
    pathPrefix: '',
    transport: http,
  };
}
const TARGET = resolveTarget();

// -----------------------------------------------------------------------------
// Memory tag → brain region mapping (mirrors classifier.go + §13.2 of guide)
// -----------------------------------------------------------------------------
const TAG_REGION = {
  // Prefrontal Cortex
  agent:'prefrontal_cortex', subagent:'prefrontal_cortex', workflow:'prefrontal_cortex',
  orchestration:'prefrontal_cortex', planning:'prefrontal_cortex',
  'design-decision':'prefrontal_cortex', decision:'prefrontal_cortex', 'ai-cascade':'prefrontal_cortex',
  // Frontal Lobe
  feedback:'frontal_lobe', directive:'frontal_lobe', 'user-profile':'frontal_lobe',
  personality:'frontal_lobe', preferences:'frontal_lobe', feature:'frontal_lobe',
  purpose:'frontal_lobe', overview:'frontal_lobe', project:'frontal_lobe',
  // Broca's Area
  output:'broca_area', response:'broca_area', writing:'broca_area',
  summary:'broca_area', documentation:'broca_area', docs:'broca_area',
  // Wernicke's Area
  prompt:'wernicke_area', intent:'wernicke_area', parsing:'wernicke_area', comprehension:'wernicke_area',
  // Visual Cortex
  screenshot:'visual_cortex', 'chrome-mcp':'visual_cortex', ui:'visual_cortex',
  ux:'visual_cortex', visual:'visual_cortex', image:'visual_cortex', web:'visual_cortex',
  browser:'visual_cortex', react:'visual_cortex', html:'visual_cortex', css:'visual_cortex',
  '3d':'visual_cortex', cad:'visual_cortex',
  // Temporal Lobe
  'memory_type:fact':'temporal_lobe_left', 'memory_type:rule':'temporal_lobe_left',
  'memory_type:procedure':'temporal_lobe_left', reference:'temporal_lobe_left',
  knowledge:'temporal_lobe_left', music:'temporal_lobe_left', audio:'temporal_lobe_left',
  // Hippocampus
  consolidated:'hippocampus', 'mental-model':'hippocampus', bank:'hippocampus',
  hindsight:'hippocampus', memory:'hippocampus',
  // Parietal Lobe
  architecture:'parietal_lobe', 'tech-stack':'parietal_lobe', 'repo-layout':'parietal_lobe',
  code:'parietal_lobe', 'system-design':'parietal_lobe', math:'parietal_lobe',
  logic:'parietal_lobe', data:'parietal_lobe', config:'parietal_lobe', schema:'parietal_lobe',
  // Motor Cortex
  tool:'motor_cortex', mcp:'motor_cortex', bash:'motor_cortex', exec:'motor_cortex',
  action:'motor_cortex', deployment:'motor_cortex', operations:'motor_cortex',
  setup:'motor_cortex', github:'motor_cortex', cli:'motor_cortex',
  cloudflare:'motor_cortex', docker:'motor_cortex',
  // Cerebellum
  testing:'cerebellum', ci:'cerebellum', test:'cerebellum', coordination:'cerebellum', auth:'cerebellum',
  // Amygdala
  error:'amygdala', gotcha:'amygdala', incident:'amygdala', warning:'amygdala',
  bug:'amygdala', bugfix:'amygdala',
  // Corpus Callosum
  slack:'corpus_callosum', 'discord-bridge':'corpus_callosum', bridge:'corpus_callosum',
  'inter-agent':'corpus_callosum', mitmproxy:'corpus_callosum',
  observability:'corpus_callosum', 'ai-tooling':'corpus_callosum',
  // Brain Stem
  creative:'brain_stem', generation:'brain_stem', imagination:'brain_stem',
  art:'brain_stem', dream:'brain_stem',
};
function regionForTags(tags) {
  if (!Array.isArray(tags)) return 'frontal_lobe';
  for (const t of tags) {
    const r = TAG_REGION[t];
    if (r) return r;
  }
  return 'frontal_lobe';
}

// -----------------------------------------------------------------------------
// Tool name → anatomical region hint
// -----------------------------------------------------------------------------
// Coarse mapping; SD Core's classifier or the dashboard's regionForEvent()
// fallback handles anything we don't pin here.
const TOOL_REGION = {
  Bash:        'motor_cortex',
  Read:        'visual_cortex',
  Write:       'broca_area',
  Edit:        'motor_cortex',
  MultiEdit:   'motor_cortex',
  Glob:        'parietal_lobe',
  Grep:        'parietal_lobe',
  WebFetch:    'temporal_lobe_left',
  WebSearch:   'temporal_lobe_left',
  Task:        'prefrontal_cortex',
  TodoWrite:   'prefrontal_cortex',
  NotebookEdit:'motor_cortex',
};
function regionForTool(toolName) {
  if (!toolName) return null;
  if (TOOL_REGION[toolName]) return TOOL_REGION[toolName];
  if (toolName.startsWith('mcp__')) return 'corpus_callosum';
  return 'motor_cortex';
}

// -----------------------------------------------------------------------------
// Rich capture (SD_RICH_CAPTURE) — sanitized tool detail
// -----------------------------------------------------------------------------
// redactSecrets scrubs credential-shaped substrings before any tool arg or
// result is forwarded. It would otherwise reach SD Core AND Tier 1 (OpenAI)
// during synthesis — and this very session's Bash history is full of
// `Authorization: Bearer <token>` and the 48-hex SD_API_TOKEN. Policy:
// OVER-redact rather than risk a single leak. Each pattern is global.
const _SECRET_PATTERNS = [
  /-----BEGIN[\s\S]*?-----/g,                                            // PEM / key block headers (+ bodies)
  /eyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}/g,         // JWTs
  /\bAKIA[0-9A-Z]{16}\b/g,                                               // AWS access key id
  /\b(?:gh[pousr]|github_pat)_[A-Za-z0-9_]{20,}\b/g,                     // GitHub tokens
  /\bsk-[A-Za-z0-9_-]{16,}\b/g,                                          // OpenAI-style keys
  /\bxox[baprs]-[A-Za-z0-9-]{10,}\b/g,                                   // Slack tokens
  /[Bb]earer\s+[A-Za-z0-9._~+/=-]{8,}/g,                                 // Bearer headers
  /(?:authorization|api[_-]?key|secret|token|password|passwd|pwd|access[_-]?key|client[_-]?secret|auth[_-]?token|private[_-]?key)["']?\s*[:=]\s*["']?[^\s"',;)&]+/gi, // key=val secrets
  /\b[a-z][a-z0-9+.-]*:\/\/[^\s/:@]+:[^\s/@]+@/gi,                       // user:pass@host in URLs/DSNs
  /\b[0-9a-fA-F]{24,}\b/g,                                               // long hex (SD_API_TOKEN is 48 hex; tokens/hashes)
  /\b[A-Za-z0-9+/]{40,}={0,2}\b/g,                                       // long base64-ish blobs
];
function redactSecrets(s) {
  if (s == null) return '';
  let out = String(s);
  for (const re of _SECRET_PATTERNS) out = out.replace(re, '‹redacted›');
  return out;
}
// clip = redact + collapse whitespace + truncate.
function clip(s, n) {
  s = redactSecrets(String(s == null ? '' : s)).replace(/\s+/g, ' ').trim();
  // Truncate on CODE POINTS, not UTF-16 units: a bare s.slice(0,n) can split a
  // surrogate pair, leaving a lone surrogate that becomes U+FFFD ('�') when the
  // event is later UTF-8 encoded (Buffer.from(JSON.stringify(...))). Array.from
  // iterates code points, so we never clip mid-character.
  const r = Array.from(s);
  return r.length > n ? r.slice(0, n).join('') + '…' : s;
}
// stripQuery drops a URL's query string (tokens often ride there) + fragment.
function stripQuery(u) {
  try { const x = new url.URL(String(u)); return clip(x.origin + x.pathname, 160); }
  catch (_) { return clip(u, 120); }
}

// richArgSummary — the substantive arg per tool, redacted + clipped. Returns
// '' when there's nothing safe/useful to add (the caller then omits the
// field and the existing arg_keys hint stands). NEVER returns file CONTENTS
// (old_string/new_string/content) — only paths.
function richArgSummary(toolName, ti) {
  if (!ti || typeof ti !== 'object') return '';
  const t = toolName || '';
  if (t === 'Bash')      return clip(ti.command, 240);
  if (t === 'WebSearch') return clip(ti.query, 200);
  if (t === 'WebFetch')  return stripQuery(ti.url) + (ti.prompt ? ' :: ' + clip(ti.prompt, 120) : '');
  if (t === 'Grep')      return clip((ti.pattern || '') + (ti.path ? ' in ' + ti.path : ''), 200);
  if (t === 'Glob')      return clip(ti.pattern, 160);
  if (t === 'Read' || t === 'Write' || t === 'Edit' || t === 'MultiEdit' || t === 'NotebookEdit')
                         return clip(ti.file_path || ti.notebook_path, 200); // PATH ONLY — never content
  if (t === 'Task')      return clip((ti.subagent_type ? '[' + ti.subagent_type + '] ' : '') + (ti.description || ''), 160);
  if (t === 'TodoWrite') return Array.isArray(ti.todos) ? (ti.todos.length + ' todos') : '';
  if (t === 'AskUserQuestion') {
    const qs = Array.isArray(ti.questions) ? ti.questions : [];
    if (!qs.length) return '';
    const first = qs[0] && (qs[0].question || qs[0].header) ? (qs[0].question || qs[0].header) : '';
    return clip(first + (qs.length > 1 ? ' (+' + (qs.length - 1) + ' more)' : ''), 200);
  }
  if (t.startsWith('mcp__')) {
    const parts = [];
    for (const [k, v] of Object.entries(ti)) {
      if (typeof v === 'string' || typeof v === 'number') parts.push(k + '=' + clip(v, 60));
      if (parts.length >= 4) break;
    }
    return clip(parts.join(' '), 200);
  }
  return ''; // unknown tool — fall back to the arg_keys already in the payload
}

// richResultSummary — short, redacted status/snippet from a tool result.
// NEVER forwards Read file contents (PII); hard-clips Bash output (could be
// an env/secret dump) after redaction.
function richResultSummary(toolName, tr) {
  if (tr == null) return '';
  const t = toolName || '';
  if (t === 'Read') {
    const s = typeof tr === 'string' ? tr : (tr && tr.content ? String(tr.content) : '');
    return s ? ('read ~' + s.length + ' chars') : 'read'; // size hint only — no content
  }
  let s = '';
  if (typeof tr === 'string') s = tr;
  else if (tr && typeof tr === 'object') s = String(tr.stdout || tr.output || tr.content || tr.result || JSON.stringify(tr));
  else s = String(tr);
  return clip(s, t === 'Bash' ? 160 : 200);
}

// -----------------------------------------------------------------------------
// Hook event → Synaptic event
// -----------------------------------------------------------------------------
function translate(hookEventName, hookPayload) {
  const ts = new Date().toISOString();
  const sessionId = hookPayload.session_id || undefined;

  const base = {
    schema_version: '1.0',
    timestamp: ts,
    adapter_id: ADAPTER_ID,
    ...(sessionId ? { session_id: sessionId } : {}),
  };

  switch (hookEventName) {
    case 'SessionStart':
      return {
        ...base,
        type: 'session_start',
        payload: {
          client: 'Claude Code',
          model: MODEL_NAME,
          cwd: hookPayload.cwd,
        },
      };

    case 'SessionEnd':
      return {
        ...base,
        type: 'session_end',
        payload: { reason: hookPayload.reason || 'unknown' },
      };

    case 'UserPromptSubmit': {
      const text = hookPayload.prompt || hookPayload.message || '';
      return {
        ...base,
        type: 'prompt_received',
        payload: {
          text: text.slice(0, 200),
          char_count: text.length,
          region_hint: 'wernicke_area',
        },
      };
    }

    case 'PreToolUse': {
      const toolName = hookPayload.tool_name || 'unknown';
      const toolUseId = hookPayload.tool_use_id || null;
      // Emit model_thinking (frontal_lobe) + tool_call (tool-specific region) in parallel.
      return [
        {
          ...base,
          type: 'model_thinking',
          payload: { model: MODEL_NAME, region_hint: 'frontal_lobe' },
        },
        {
          ...base,
          type: 'tool_call',
          payload: {
            tool_name: toolName,
            tool_call_id: toolUseId,
            // Don't ship tool_input verbatim — could be huge / contain PII.
            // We forward keys only as a structural hint.
            arg_keys: hookPayload.tool_input
              ? Object.keys(hookPayload.tool_input).slice(0, 8)
              : [],
            // Rich capture: add the substantive arg (path/command/query/URL),
            // redacted + clipped, so synthesis knows WHAT was done. Gated by
            // the server setting (cached) + SD_RICH_CAPTURE env override.
            ...(shouldCaptureRich()
              ? (() => { const a = richArgSummary(toolName, hookPayload.tool_input); return a ? { arg_summary: a } : {}; })()
              : {}),
            region_hint: regionForTool(toolName),
          },
        },
      ];
    }

    case 'PostToolUse': {
      const toolName = hookPayload.tool_name || 'unknown';
      // ── Failure detection ─────────────────────────────────────────────
      // Claude Code has NO 'PostToolUseFailure' hook event — tool failures
      // arrive HERE, on PostToolUse, with the result under `tool_response`.
      // The old handler hardcoded ok:true and never inspected it, so no
      // type:'error' event was ever emitted → the error-synthesis trigger
      // could never accumulate (0 "lesson learned" memories). Probe the
      // result for an explicit failure signal and, when present, emit an
      // error in the shape hook_synthesis.go errorSignature() parses
      // (where:'tool:<name>' + error_message). Signals are deliberately
      // specific (is_error===true / success===false / non-zero exit_code) to
      // avoid false positives on successful results.
      const _tr = hookPayload.tool_response;
      // Claude Code renders a FAILED tool result as a plain string that starts
      // with "Error:" (e.g. Bash → "Error: Exit code 1\n<stderr>"); structured
      // / MCP tools may instead pass an object with is_error/success/exit_code.
      // Cover both, plus a top-level is_error, so failures across tool kinds
      // are caught. (Verified against this session's transcript: bare `cat`
      // miss → tool_response = "Error: Exit code 1\ncat: ... No such file".)
      const _trErrStr = (typeof _tr === 'string') && _tr.trim().slice(0, 6) === 'Error:';
      const _failed =
        hookPayload.is_error === true ||
        (typeof hookPayload.tool_error === 'string' && hookPayload.tool_error.length > 0) ||
        _trErrStr ||
        (_tr && typeof _tr === 'object' &&
          (_tr.is_error === true || _tr.success === false ||
           (typeof _tr.exit_code === 'number' && _tr.exit_code !== 0)));
      if (_failed) {
        let _msg = (typeof hookPayload.tool_error === 'string') ? hookPayload.tool_error : '';
        if (!_msg && typeof _tr === 'string') _msg = _tr;
        if (!_msg && _tr && typeof _tr === 'object') {
          _msg = _tr.error || _tr.message ||
                 (typeof _tr.stderr === 'string' ? _tr.stderr : '') ||
                 (typeof _tr.content === 'string' ? _tr.content : '');
        }
        return {
          ...base,
          type: 'error',
          payload: {
            where: `tool:${toolName}`,
            tool_call_id: hookPayload.tool_use_id || null,
            error_message: clip(_msg, 200),
            region_hint: 'amygdala',
          },
        };
      }
      // Hindsight retain → memory saved → hippocampus memory_added pulse.
      if (toolName === 'mcp__hindsight__retain') {
        return {
          ...base,
          type: 'memory_added',
          payload: { source: 'hindsight', region_hint: 'hippocampus' },
        };
      }
      // Other Hindsight ops (recall, reflect, create_mental_model, …) → memory_recall.
      if (toolName.startsWith('mcp__hindsight__')) {
        return {
          ...base,
          type: 'memory_recall',
          payload: { source: 'hindsight', region_hint: 'hippocampus' },
        };
      }
      // SD memory save → memory_added; derive region from the tags passed
      // to the tool. Accept both the current `synaptic` slug and the legacy
      // `synaptic-disorder` slug from pre-2.1.0b1 plugin installs.
      if (toolName === 'mcp__synaptic__report_memory_save' ||
          toolName === 'mcp__synaptic-disorder__report_memory_save') {
        const tags = hookPayload.tool_input && hookPayload.tool_input.tags;
        return {
          ...base,
          type: 'memory_added',
          payload: { region_hint: regionForTags(tags) },
        };
      }
      // Synaptic / Syn pipeline ops via MCP. We surface these as dream/admin
      // pulses so the dashboard can show "manual nightly run kicked off" etc.
      // Match both legacy `synaptic-disorder` and future `synaptic` slugs.
      if (/^mcp__(synaptic-disorder|synaptic)__sd_/.test(toolName)) {
        const op = toolName.replace(/^mcp__(synaptic-disorder|synaptic)__/, '');
        if (op === 'sd_dream_run') {
          return { ...base, type: 'dream_run_started', payload: { source: 'mcp', region_hint: 'brain_stem' } };
        }
        if (op === 'sd_dream_status') {
          return { ...base, type: 'dream_run_query', payload: { source: 'mcp', region_hint: 'brain_stem' } };
        }
        if (op === 'sd_deep_encode_all') {
          return { ...base, type: 'deep_encode_started', payload: { source: 'mcp', region_hint: 'hippocampus' } };
        }
        if (op === 'sd_embed_all') {
          return { ...base, type: 'embed_backfill_started', payload: { source: 'mcp', region_hint: 'hippocampus' } };
        }
        if (op === 'sd_lexicon_rebuild') {
          return { ...base, type: 'lexicon_rebuild_started', payload: { source: 'mcp', region_hint: 'parietal_lobe' } };
        }
        if (op === 'sd_get_dream_settings' || op === 'sd_set_dream_settings') {
          return { ...base, type: 'settings_touched', payload: { key: 'dream_pipeline', region_hint: 'frontal_lobe' } };
        }
        if (op === 'sd_research') {
          return { ...base, type: 'research_query', payload: { source: 'mcp', region_hint: 'temporal_lobe_left' } };
        }
        if (op === 'sd_reflect') {
          return { ...base, type: 'memory_recall', payload: { source: 'mcp', subtype: 'reflect', region_hint: 'prefrontal_cortex' } };
        }
        if (op === 'sd_recall' || op === 'sd_list_memories' || op === 'sd_get_memory' || op === 'sd_get_audit') {
          return { ...base, type: 'memory_recall', payload: { source: 'mcp', region_hint: 'hippocampus' } };
        }
        if (op === 'sd_create_memory' || op === 'sd_update_memory' || op === 'sd_restore_memory') {
          const tags = hookPayload.tool_input && hookPayload.tool_input.tags;
          return { ...base, type: 'memory_added', payload: { source: 'mcp', region_hint: regionForTags(tags) } };
        }
        if (op === 'sd_delete_memory') {
          return { ...base, type: 'memory_deleted', payload: { source: 'mcp', region_hint: 'amygdala' } };
        }
      }
      return {
        ...base,
        type: 'tool_result',
        payload: {
          tool_name: toolName,
          tool_call_id: hookPayload.tool_use_id || null,
          ok: true,
          duration_ms: hookPayload.duration_ms,
          // Rich capture: add a short redacted result snippet/status so
          // synthesis knows the outcome (Read never forwards contents).
          ...(shouldCaptureRich()
            ? (() => { const r = richResultSummary(toolName, hookPayload.tool_response); return r ? { result_snippet: r } : {}; })()
            : {}),
          region_hint: 'cerebellum',
        },
      };
    }

    // NOTE: there is no 'PostToolUseFailure' case — Claude Code has no such
    // hook event. Tool failures are detected inside 'PostToolUse' above.

    case 'SubagentStop':
      return {
        ...base,
        type: 'subagent_complete',
        payload: {
          subagent_id: hookPayload.agent_id || hookPayload.subagent_id || 'unknown',
          ok: true,
          region_hint: 'prefrontal_cortex',
        },
      };

    case 'Stop': {
      // 2026-05-23 — read the actual assistant turn from the session's
      // JSONL transcript (Claude Code's hook payload exposes
      // `transcript_path`). Hooks don't surface model output directly, so
      // the transcript file is the only way to capture what Claude said.
      // Falls back to the prior placeholder pulses if the file is missing
      // or unreadable — never blocks the hook.
      const assistantTurn = readLatestAssistantTurn(hookPayload.transcript_path);
      if (assistantTurn && assistantTurn.text) {
        // Cap at 4000 chars so a 50-page response doesn't blow out a single
        // event. The full text lives in the JSONL for any offline distillation
        // pass (Vir-style); the event carries enough for live neighbor search
        // + dashboard preview.
        const text = assistantTurn.text.length > 4000
          ? assistantTurn.text.slice(0, 4000) + ' …[truncated]'
          : assistantTurn.text;
        return {
          ...base,
          type: 'response_complete',
          payload: {
            text,
            char_count: assistantTurn.text.length,
            // estimateTokens-style approximation; saves an inference call.
            tokens: Math.ceil(assistantTurn.text.length / 4),
            model: assistantTurn.model || MODEL_NAME,
            stop_reason: assistantTurn.stop_reason || null,
            region_hint: 'broca_area',
          },
        };
      }
      // Fallback: original placeholder shape so the dashboard's
      // broca_area pulse still happens when the transcript isn't readable.
      const streamingPulses = [1, 2, 3].map(() => ({
        ...base,
        type: 'response_streaming',
        payload: { tokens: Math.floor(Math.random() * 40) + 10, region_hint: 'broca_area' },
      }));
      return [...streamingPulses, { ...base, type: 'response_complete', payload: { region_hint: 'broca_area' } }];
    }

    default:
      return null;
  }
}

// -----------------------------------------------------------------------------
// HTTP POST (fire-and-forget, short timeout, no throw)
// -----------------------------------------------------------------------------
function postEvent(event) {
  return new Promise((resolve) => {
    const body = Buffer.from(JSON.stringify(event), 'utf8');
    const headers = {
      'Content-Type':   'application/json',
      'Content-Length': body.length,
    };
    if (SD_API_TOKEN) headers['Authorization'] = `Bearer ${SD_API_TOKEN}`;
    const req = TARGET.transport.request(
      {
        host: TARGET.host,
        port: TARGET.port,
        path: `${TARGET.pathPrefix}/event`,
        method: 'POST',
        headers,
        timeout: REQUEST_TIMEOUT_MS,
      },
      (res) => {
        // Drain the response so the socket can close cleanly.
        res.on('data', () => {});
        res.on('end', () => resolve(true));
      }
    );
    req.on('timeout', () => { req.destroy(); resolve(false); });
    req.on('error', () => resolve(false));
    req.write(body);
    req.end();
  });
}

// -----------------------------------------------------------------------------
// readLatestAssistantTurn — read the most recent assistant turn from a
// Claude Code session transcript JSONL. Claude Code writes every turn
// (user prompt, assistant response, tool call, tool result) to
// ~/.claude/projects/<encoded-path>/<session-id>.jsonl as a stream of
// JSON-per-line records. Hook payloads expose the absolute transcript_path
// on Stop/SubagentStop events.
//
// This is the ONLY way to capture the assistant's actual prose response —
// hooks don't surface model output, only tool calls + session boundaries.
// (Vir does the same thing in batch mode every 3 hours; we do it live on
// every Stop event.)
//
// Returns { text, model, stop_reason } when the latest assistant turn is
// found, or null on any error (missing file / unparseable lines / no
// assistant turn yet). Reads up to 2 MB of tail — long sessions interleave
// huge thinking + tool_use rows so a 64 KB tail can hold as few as 25 lines
// (observed: 221 MB transcript, 64 KB ≈ 25 lines, all tool_use → no text
// block ever found → empty payload). 2 MB covers ~800 lines which reliably
// reaches the previous user-visible response even in heavy agent chains.
// -----------------------------------------------------------------------------
function readLatestAssistantTurn(transcriptPath) {
  if (!transcriptPath || typeof transcriptPath !== 'string') return null;
  try {
    if (!fs.existsSync(transcriptPath)) return null;
    const stat = fs.statSync(transcriptPath);
    if (!stat || stat.size === 0) return null;
    const readSize = Math.min(stat.size, 2 * 1024 * 1024);
    const fd = fs.openSync(transcriptPath, 'r');
    const buf = Buffer.alloc(readSize);
    fs.readSync(fd, buf, 0, readSize, stat.size - readSize);
    fs.closeSync(fd);
    const tail = buf.toString('utf8');
    // First newline boundary so we don't try to parse a partial JSON.
    const lines = tail.split('\n').filter(l => l.trim());
    // Walk backwards to the most recent assistant turn.
    for (let i = lines.length - 1; i >= 0; i--) {
      let row;
      try { row = JSON.parse(lines[i]); } catch { continue; }
      // Claude Code transcript record shapes vary slightly by version.
      // The common pattern: { type: 'assistant', message: { content: [...] } }
      // OR { role: 'assistant', content: [...] }.
      const isAssistant =
        row.type === 'assistant' ||
        row.role === 'assistant' ||
        (row.message && row.message.role === 'assistant');
      if (!isAssistant) continue;
      const content = (row.message && row.message.content) || row.content || [];
      if (!Array.isArray(content)) continue;
      // Concatenate all text blocks (skip tool_use blocks — those are
      // captured separately via PreToolUse hooks). Thinking blocks are
      // skipped too; the dashboard already has model_thinking pulses.
      const textParts = [];
      for (const block of content) {
        if (block && typeof block === 'object' && block.type === 'text' && typeof block.text === 'string') {
          textParts.push(block.text);
        }
      }
      const text = textParts.join('\n').trim();
      if (!text) continue;
      return {
        text,
        model: (row.message && row.message.model) || row.model || null,
        stop_reason: (row.message && row.message.stop_reason) || row.stop_reason || null,
      };
    }
    return null;
  } catch (_) {
    return null;
  }
}

// -----------------------------------------------------------------------------
// recallAndInject — for UserPromptSubmit, semantic-search the bank for memories
// relevant to the user's prompt and write the top matches to stdout as
// Claude Code's `hookSpecificOutput.additionalContext`. The injected text
// is prepended to the user's prompt before the model sees it, so the agent
// boots each turn with relevant Synaptic context — no manual /recall needed.
//
// Opt-in via SD_RECALL_INJECT=1 (default off). Keeps the hook's default
// behaviour identical for users who haven't asked for in-flight injection.
// Caps the injected block at 2000 chars so long memories don't blow out
// the context window.
//
// Best-effort: failures are swallowed and the hook returns no injection
// rather than blocking the user's prompt.
// -----------------------------------------------------------------------------
function recallAndInject(prompt) {
  if (!process.env.SD_RECALL_INJECT || process.env.SD_RECALL_INJECT === '0') {
    return Promise.resolve(null);
  }
  const q = (prompt || '').trim();
  if (q.length < 8) return Promise.resolve(null); // too short to be a meaningful query
  const limit = parseInt(process.env.SD_RECALL_INJECT_LIMIT || '5', 10);
  const body = JSON.stringify({ query: q.slice(0, 500), limit });
  const headers = { 'Content-Type': 'application/json', 'X-Adapter-Id': ADAPTER_ID };
  if (SD_API_TOKEN) headers['Authorization'] = 'Bearer ' + SD_API_TOKEN;
  return new Promise((resolve) => {
    const req = TARGET.transport.request(
      {
        host: TARGET.host, port: TARGET.port,
        path: `${TARGET.pathPrefix}/recall`, method: 'POST',
        headers, timeout: REQUEST_TIMEOUT_MS,
      },
      (res) => {
        const chunks = [];
        res.on('data', (c) => chunks.push(c));
        res.on('end', () => {
          try {
            const parsed = JSON.parse(Buffer.concat(chunks).toString('utf8'));
            const items = parsed.results || parsed.memories || parsed.items || [];
            if (!Array.isArray(items) || items.length === 0) return resolve(null);
            const lines = [];
            let total = 0;
            for (const m of items.slice(0, limit)) {
              const txt = (m.text || m.enriched_text || '').slice(0, 220);
              const tags = (m.tags || []).slice(0, 5).join(', ');
              const line = `- [${m.region_hint || 'unknown'}] ${txt}${tags ? '  ·  ' + tags : ''}`;
              if (total + line.length > 2000) break;
              lines.push(line);
              total += line.length;
            }
            if (lines.length === 0) return resolve(null);
            resolve(
              `<synaptic-recall>\nThe following memories may be relevant to the user's prompt — use them as context but do not cite them unless directly helpful.\n${lines.join('\n')}\n</synaptic-recall>`
            );
          } catch (_) { resolve(null); }
        });
      }
    );
    req.on('timeout', () => { req.destroy(); resolve(null); });
    req.on('error', () => resolve(null));
    req.write(body);
    req.end();
  });
}

// -----------------------------------------------------------------------------
// stdin → JSON (with safe fallback)
// -----------------------------------------------------------------------------
function readStdin() {
  return new Promise((resolve) => {
    const chunks = [];
    let done = false;
    const finish = () => {
      if (done) return;
      done = true;
      const raw = Buffer.concat(chunks).toString('utf8').trim();
      if (!raw) return resolve({});
      try { resolve(JSON.parse(raw)); }
      catch { resolve({ _raw: raw.slice(0, 500) }); }
    };
    // If stdin is a TTY (manual invocation), give up after 100ms.
    if (process.stdin.isTTY) {
      setTimeout(finish, 100);
    } else {
      setTimeout(finish, REQUEST_TIMEOUT_MS); // hard cap
    }
    process.stdin.on('data', (c) => chunks.push(c));
    process.stdin.on('end', finish);
    process.stdin.on('error', finish);
  });
}

// -----------------------------------------------------------------------------
// Main
// -----------------------------------------------------------------------------
(async () => {
  try {
    const eventName = process.argv[2] || 'Unknown';
    const hookPayload = await readStdin();
    const sdResult = translate(eventName, hookPayload);
    // Fire-and-forget: post the event in parallel with any recall injection.
    // We don't await the post when injection is the main output path because
    // the post is best-effort and the user-facing context block matters more.
    const postPromise = sdResult
      ? Promise.all((Array.isArray(sdResult) ? sdResult : [sdResult]).map(postEvent))
      : Promise.resolve();
    // Per-turn recall injection (feature #2) — only on UserPromptSubmit,
    // only when SD_RECALL_INJECT=1. Inject Synaptic context into the
    // agent's next turn via hookSpecificOutput.additionalContext so the
    // model sees relevant prior memories without an explicit /sd-recall.
    if (eventName === 'UserPromptSubmit') {
      const promptText = hookPayload.prompt || hookPayload.message || '';
      const injection = await recallAndInject(promptText);
      if (injection) {
        process.stdout.write(JSON.stringify({
          hookSpecificOutput: {
            hookEventName: 'UserPromptSubmit',
            additionalContext: injection,
          },
        }));
      }
    }
    await postPromise;
    // Refresh the rich-capture setting cache for the NEXT hook invocation.
    // Awaited (NOT fire-and-forget): the process.exit(0) below would otherwise
    // kill this short-lived process before the GET's response callback writes
    // the cache, which is exactly why rich capture silently never armed. Only
    // does network work when the cache is stale (~1 GET per 5-min window) and
    // is bounded by the GET timeout; errors resolve silently.
    await maybeRefreshRichCaptureCache();
  } catch (_) {
    // Swallow everything — we must never break the user's Claude Code session.
  } finally {
    process.exit(0);
  }
})();
