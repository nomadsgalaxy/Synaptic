#!/usr/bin/env node
// =============================================================================
// Synaptic — Universal MCP Adapter
//
// A stdio MCP server. Any MCP-aware AI client (Claude Desktop, Cursor, Cline,
// Continue, Gemini CLI, Claude Code, etc.) can connect this server and use
// its two tools to push live activity into the Synaptic dashboard:
//
//   report_event(type, payload?, region_hint?, session_id?)
//     → POSTs a v1.0 schema event to SD Core at http://localhost:9911/event.
//
//   report_memory_save(text, tags?, region_hint?, memory_id?, session_id?)
//     → Convenience wrapper that emits a `memory_added` event. Persistence
//       to a SQLite fallback bank is B6 (not yet implemented).
//
// The server emits its own `session_start` on launch and `session_end` on
// shutdown, so a connected client always shows up as a live agent in the HUD
// for the duration of the connection.
//
// All logging goes to stderr — stdout is reserved for the MCP JSON-RPC frame.
// =============================================================================

import { Server } from '@modelcontextprotocol/sdk/server/index.js';
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import {
  CallToolRequestSchema,
  ListToolsRequestSchema,
} from '@modelcontextprotocol/sdk/types.js';
import http  from 'node:http';
import https from 'node:https';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import { URL } from 'node:url';
import { randomUUID } from 'node:crypto';

// -----------------------------------------------------------------------------
// Config (config.json → env vars → built-in defaults, in that priority order)
// -----------------------------------------------------------------------------
// Edit config.json (same directory as this file) to point at a remote SD Core
// or add an API token. Env vars set by the MCP client take precedence.
const __dirname = dirname(fileURLToPath(import.meta.url));
try {
  const _cfg = JSON.parse(readFileSync(join(__dirname, 'config.json'), 'utf8'));
  for (const [k, v] of Object.entries(_cfg)) {
    if (v && !process.env[k]) process.env[k] = String(v);
  }
} catch (_) {}

// Remote-mode preferred: SD_CORE_URL = "https://cognito.example.com" with
// SD_API_TOKEN set to the matching SD Core's bearer token.
// Backwards-compat: SD_CORE_HOST + SD_CORE_PORT for localhost-only setups.
const SD_CORE_URL  = process.env.SD_CORE_URL || '';
const SD_CORE_HOST = process.env.SD_CORE_HOST || '127.0.0.1';
const SD_CORE_PORT = parseInt(process.env.SD_CORE_PORT || '9911', 10);
const SD_API_TOKEN = process.env.SD_API_TOKEN || '';
const ADAPTER_ID   = process.env.SD_ADAPTER_ID   || 'mcp-adapter';
const CLIENT_NAME  = process.env.SD_CLIENT       || 'mcp-client';
const MODEL_NAME   = process.env.SD_MODEL        || 'unknown';
const SERVER_VERSION = '0.1.0';
const SESSION_ID = `mcp-${randomUUID().slice(0, 8)}`;
const REQUEST_TIMEOUT_MS = 1500;

function resolveTarget() {
  if (SD_CORE_URL) {
    const u = new URL(SD_CORE_URL);
    return {
      protocol: u.protocol,
      host: u.hostname,
      port: u.port ? parseInt(u.port, 10) : (u.protocol === 'https:' ? 443 : 80),
      pathPrefix: u.pathname.replace(/\/$/, ''),
      transport: u.protocol === 'https:' ? https : http,
      label: `${u.protocol}//${u.hostname}${u.port ? ':' + u.port : ''}`,
    };
  }
  return {
    protocol: 'http:',
    host: SD_CORE_HOST,
    port: SD_CORE_PORT,
    pathPrefix: '',
    transport: http,
    label: `${SD_CORE_HOST}:${SD_CORE_PORT}`,
  };
}
const TARGET = resolveTarget();

// Tag → brain region (mirrors classifier.go + §13.2 of the Memory Authoring Guide)
const TAG_REGION = {
  agent:'prefrontal_cortex', subagent:'prefrontal_cortex', workflow:'prefrontal_cortex',
  orchestration:'prefrontal_cortex', planning:'prefrontal_cortex',
  'design-decision':'prefrontal_cortex', decision:'prefrontal_cortex', 'ai-cascade':'prefrontal_cortex',
  feedback:'frontal_lobe', directive:'frontal_lobe', 'user-profile':'frontal_lobe',
  personality:'frontal_lobe', preferences:'frontal_lobe', feature:'frontal_lobe',
  purpose:'frontal_lobe', overview:'frontal_lobe', project:'frontal_lobe',
  output:'broca_area', response:'broca_area', writing:'broca_area',
  summary:'broca_area', documentation:'broca_area', docs:'broca_area',
  prompt:'wernicke_area', intent:'wernicke_area', parsing:'wernicke_area', comprehension:'wernicke_area',
  screenshot:'visual_cortex', 'chrome-mcp':'visual_cortex', ui:'visual_cortex',
  ux:'visual_cortex', visual:'visual_cortex', image:'visual_cortex', web:'visual_cortex',
  browser:'visual_cortex', react:'visual_cortex', html:'visual_cortex', css:'visual_cortex',
  '3d':'visual_cortex', cad:'visual_cortex',
  'memory_type:fact':'temporal_lobe_left', 'memory_type:rule':'temporal_lobe_left',
  'memory_type:procedure':'temporal_lobe_left', reference:'temporal_lobe_left',
  knowledge:'temporal_lobe_left', music:'temporal_lobe_left', audio:'temporal_lobe_left',
  consolidated:'hippocampus', 'mental-model':'hippocampus', bank:'hippocampus',
  hindsight:'hippocampus', memory:'hippocampus',
  architecture:'parietal_lobe', 'tech-stack':'parietal_lobe', 'repo-layout':'parietal_lobe',
  code:'parietal_lobe', 'system-design':'parietal_lobe', math:'parietal_lobe',
  logic:'parietal_lobe', data:'parietal_lobe', config:'parietal_lobe', schema:'parietal_lobe',
  tool:'motor_cortex', mcp:'motor_cortex', bash:'motor_cortex', exec:'motor_cortex',
  action:'motor_cortex', deployment:'motor_cortex', operations:'motor_cortex',
  setup:'motor_cortex', github:'motor_cortex', cli:'motor_cortex',
  cloudflare:'motor_cortex', docker:'motor_cortex',
  testing:'cerebellum', ci:'cerebellum', test:'cerebellum', coordination:'cerebellum', auth:'cerebellum',
  error:'amygdala', gotcha:'amygdala', incident:'amygdala', warning:'amygdala',
  bug:'amygdala', bugfix:'amygdala',
  slack:'corpus_callosum', 'discord-bridge':'corpus_callosum', bridge:'corpus_callosum',
  'inter-agent':'corpus_callosum', mitmproxy:'corpus_callosum',
  observability:'corpus_callosum', 'ai-tooling':'corpus_callosum',
  creative:'brain_stem', generation:'brain_stem', imagination:'brain_stem',
  art:'brain_stem', dream:'brain_stem',
};
function regionForTags(tags) {
  if (!Array.isArray(tags)) return 'frontal_lobe';
  for (const t of tags) { const r = TAG_REGION[t]; if (r) return r; }
  return 'frontal_lobe';
}

// Allowed event types per docs/event-schema.md v1.0
const VALID_TYPES = new Set([
  'session_start',
  'session_end',
  'prompt_received',
  'model_thinking',
  'response_streaming',
  'response_complete',
  'tool_call',
  'tool_result',
  'memory_recall',
  'memory_added',
  'memory_updated',
  'memory_deleted',
  'subagent_spawn',
  'subagent_complete',
  'error',
]);

// -----------------------------------------------------------------------------
// Logging (stderr only — stdout is reserved for MCP frames)
// -----------------------------------------------------------------------------
function log(...args) {
  process.stderr.write('[synaptic-mcp] ' + args.join(' ') + '\n');
}

// -----------------------------------------------------------------------------
// HTTP POST to SD Core (fire-and-forget; never throws)
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
        res.on('data', () => {});
        res.on('end', () => resolve({ ok: res.statusCode < 400, status: res.statusCode }));
      }
    );
    req.on('timeout', () => { req.destroy(); resolve({ ok: false, error: 'timeout' }); });
    req.on('error', (err) => resolve({ ok: false, error: err.message }));
    req.write(body);
    req.end();
  });
}

// Generic HTTP request to SD Core. Used by the bank/maps/research/audit/
// budget tools. Returns {ok, status, data, error} where `data` is the
// parsed JSON body (or raw text if not JSON). Never throws — failures
// land in the caller's response so the AI gets a useful error message.
function sdRequest({ method, path, body, timeoutMs }) {
  return new Promise((resolve) => {
    const headers = { 'Accept': 'application/json' };
    let bodyBuf = null;
    if (body !== undefined && body !== null) {
      bodyBuf = Buffer.from(JSON.stringify(body), 'utf8');
      headers['Content-Type']   = 'application/json';
      headers['Content-Length'] = bodyBuf.length;
    }
    if (SD_API_TOKEN) headers['Authorization'] = `Bearer ${SD_API_TOKEN}`;
    headers['X-Adapter-Id'] = ADAPTER_ID;
    const req = TARGET.transport.request(
      {
        host: TARGET.host,
        port: TARGET.port,
        path: `${TARGET.pathPrefix}${path}`,
        method: method || 'GET',
        headers,
        timeout: timeoutMs || REQUEST_TIMEOUT_MS,
      },
      (res) => {
        const chunks = [];
        res.on('data', (c) => chunks.push(c));
        res.on('end', () => {
          const raw = Buffer.concat(chunks).toString('utf8');
          let data = raw;
          if ((res.headers['content-type'] || '').includes('application/json') && raw) {
            try { data = JSON.parse(raw); } catch (_) { /* keep raw */ }
          }
          resolve({
            ok: res.statusCode < 400,
            status: res.statusCode,
            data,
          });
        });
      }
    );
    req.on('timeout', () => { req.destroy(); resolve({ ok: false, status: 0, error: 'timeout' }); });
    req.on('error', (err) => resolve({ ok: false, status: 0, error: err.message }));
    if (bodyBuf) req.write(bodyBuf);
    req.end();
  });
}

function buildEvent({ type, payload, region_hint, session_id }) {
  const finalPayload = { ...(payload || {}) };
  if (region_hint && !finalPayload.region_hint) finalPayload.region_hint = region_hint;
  return {
    schema_version: '1.0',
    type,
    timestamp: new Date().toISOString(),
    adapter_id: ADAPTER_ID,
    session_id: session_id || SESSION_ID,
    payload: finalPayload,
  };
}

// -----------------------------------------------------------------------------
// MCP Server
// -----------------------------------------------------------------------------
const server = new Server(
  {
    name: 'synaptic-mcp',
    version: SERVER_VERSION,
  },
  {
    capabilities: {
      tools: {},
    },
  }
);

const TOOLS = [
  {
    name: 'report_event',
    description:
      'Report a Synaptic activity event so it pulses the brain dashboard in real time. ' +
      'Use this whenever the AI begins reasoning, calls a tool, completes a response, recalls a memory, ' +
      'or hits an error. The dashboard maps event types to anatomical regions (e.g. tool_call → motor cortex, ' +
      'prompt_received → wernicke). Calling this is fire-and-forget; if SD Core is offline, the call returns ' +
      'a non-error message and the dashboard simply stays quiet.',
    inputSchema: {
      type: 'object',
      required: ['type'],
      properties: {
        type: {
          type: 'string',
          enum: Array.from(VALID_TYPES),
          description: 'Event type per the Synaptic v1.0 event schema.',
        },
        payload: {
          type: 'object',
          description:
            'Event-type-specific fields. For tool_call: { tool_name, tool_call_id, args }. ' +
            'For prompt_received: { text, char_count }. For response_streaming: { delta, tokens }. ' +
            'See docs/event-schema.md for the full list.',
        },
        region_hint: {
          type: 'string',
          description:
            'Optional anatomical region key from assets/anatomy.json (e.g. "frontal_lobe", ' +
            '"motor_cortex", "hippocampus"). Overrides the dashboard\'s region inference.',
        },
        session_id: {
          type: 'string',
          description:
            'Optional session id. Defaults to the MCP server\'s own session id, which groups ' +
            'all events from this client connection together in the HUD.',
        },
      },
    },
  },
  {
    name: 'report_memory_save',
    description:
      'Notify Synaptic that a memory was saved to an EXTERNAL memory ' +
      'system. Emits a memory_added event so a neuron pulses and a ripple spreads through the dashboard. ' +
      'NOTE: emitting this event ALSO writes the memory into the Synaptic bank (the backend persists ' +
      'memory_added events through SaveMemory). If you want to write into Synaptic\'s own bank as the ' +
      'authoritative record, prefer the explicit `sd_create_memory` tool — same wire-level effect, ' +
      'clearer intent. Use this one when Synaptic is shadowing a primary memory system you control.\n\n' +
      'TAGS MATTER. Each memory should carry 3–5 tags drawn from at least three of these buckets so ' +
      'the dashboard can route the neuron to the right brain region AND form synaptic connections ' +
      'with related memories:\n' +
      '  • Region: feature, bugfix, architecture, tool, error, docs, ui, code, testing, deployment\n' +
      '  • Topic: oauth, voxel-layout, memory-consolidation, etc. (specific subject of the memory)\n' +
      '  • Project: project:<name>\n' +
      '  • Identity: user:<name>, repo:<name>\n' +
      'A memory needs ≥2 shared tags with another memory to form a synapse. Empty tags or single-use ' +
      'tags produce orphan neurons. Full guide: docs/MEMORY_AUTHORING_GUIDE.md',
    inputSchema: {
      type: 'object',
      required: ['text'],
      properties: {
        text: {
          type: 'string',
          description: 'The memory text. 1–3 specific sentences — concrete identifiers (file paths, function names, version numbers) beat vague descriptions.',
        },
        tags: {
          type: 'array',
          items: { type: 'string' },
          description: 'Required for good visualization. 3–5 tags covering region/topic/project (and optionally identity). Examples: ["feature","react","ui","deployment","project:event-tracker"] or ["bugfix","oauth","auth","project:gravity"]. See docs/MEMORY_AUTHORING_GUIDE.md.',
        },
        region_hint: {
          type: 'string',
          description: 'Optional anatomical region key. Usually leave unset — the classifier routes from tags. Override only when you know better than the tag patterns. Valid keys: prefrontal_cortex, frontal_lobe, parietal_lobe, motor_cortex, visual_cortex, temporal_lobe_left, temporal_lobe_right, hippocampus, amygdala, cerebellum, brain_stem, broca_area, wernicke_area, corpus_callosum.',
        },
        memory_id: {
          type: 'string',
          description: 'Stable id from the underlying memory system (helps the dashboard dedupe).',
        },
        session_id: {
          type: 'string',
          description: 'Optional session id; defaults to the MCP server session.',
        },
      },
    },
  },

  // ---------------------------------------------------------------------------
  // sd_create_memory — write a new memory into the Synaptic bank
  // ---------------------------------------------------------------------------
  {
    name: 'sd_create_memory',
    description:
      'Create a new memory in the Synaptic bank. The classifier auto-routes by tags (anatomy region), ' +
      'the synapse builder embeds the text and links it to similar memories within the next nightly ' +
      'pass, and the dashboard pulses a fresh neuron immediately. Use this for anything you want to ' +
      'persist into Synaptic as the authoritative store (vs `report_memory_save`, which is for ' +
      'shadowing an EXTERNAL memory system).\n\n' +
      'TAGS MATTER. 3–5 tags drawn from at least three buckets:\n' +
      '  • Region: feature, bugfix, architecture, tool, error, docs, ui, code, testing, deployment\n' +
      '  • Topic: oauth, voxel-layout, memory-consolidation, etc.\n' +
      '  • Project: project:<name>\n' +
      '  • Identity: user:<name>, repo:<name>\n' +
      'A memory needs ≥2 shared tags with another to form a synapse. Single-use tags produce orphan ' +
      'neurons. The first sentence of `text` is what shows in list views — front-load the identifier ' +
      '(file path, function name, decision title) before the rationale.',
    inputSchema: {
      type: 'object',
      required: ['text'],
      properties: {
        text: {
          type: 'string',
          description: '1–3 specific sentences. Front-load identifiers (paths, names, versions). The first sentence becomes the dashboard summary.',
        },
        tags: {
          type: 'array',
          items: { type: 'string' },
          description: '3–5 tags spanning region/topic/project (and optionally identity). Examples: ["bugfix","oauth","auth","project:gravity"], ["feature","react","ui","project:event-tracker"].',
        },
        region_hint: {
          type: 'string',
          description: 'Optional region override (prefrontal_cortex, frontal_lobe, parietal_lobe, motor_cortex, visual_cortex, temporal_lobe_left, temporal_lobe_right, hippocampus, amygdala, cerebellum, brain_stem, broca_area, wernicke_area, corpus_callosum). Usually leave unset and let the classifier route from tags.',
        },
        id: {
          type: 'string',
          description: 'Optional caller-supplied id. Useful when migrating from another memory system and you want to preserve cross-system identity for dedupe.',
        },
        sensitive: {
          type: 'boolean',
          description: 'If true, mark the memory sensitive on creation. Sensitive memories are excluded from any external research / Tier 3 Oracle / web fetch. Sticky-on policy applies — backend regex/AI may also auto-flag.',
        },
        session_id: {
          type: 'string',
          description: 'Optional session id; defaults to the MCP server\'s session.',
        },
      },
    },
  },

  // ---------------------------------------------------------------------------
  // sd_pin_memory — high-confidence, user-curated memory (vs auto-extracted)
  // ---------------------------------------------------------------------------
  {
    name: 'sd_pin_memory',
    description:
      'Pin a high-confidence, user-curated memory into Synaptic. Like sd_create_memory, but adds the ' +
      '`pinned` tag so dedup/decay phases NEVER prune it and Phase 0b deep-encodes it on the next pass ' +
      'at top priority (above tmr_user). Use this when the user says "remember that …" or makes an ' +
      'explicit factual claim worth treating as ground truth. Auto-extracted memories should use ' +
      '`sd_create_memory`; pinned ones use this.\n\n' +
      'Tags inherit the same rules as sd_create_memory — 3–5 spanning region/topic/project. The ' +
      '`pinned` tag is appended automatically; do not include it in your `tags` input.',
    inputSchema: {
      type: 'object',
      required: ['text'],
      properties: {
        text: {
          type: 'string',
          description: '1–3 specific sentences front-loading the identifier (path/name/decision). First sentence becomes the dashboard summary.',
        },
        tags: {
          type: 'array',
          items: { type: 'string' },
          description: '3–5 tags spanning region/topic/project. `pinned` is added automatically.',
        },
        region_hint: {
          type: 'string',
          description: 'Optional region override. Usually leave unset and let the classifier route.',
        },
        id: { type: 'string', description: 'Optional caller-supplied id (UUID-style).' },
        sensitive: { type: 'boolean', description: 'Mark sensitive on creation. Sticky-on.' },
        session_id: { type: 'string', description: 'Optional session id; defaults to the MCP session.' },
      },
    },
  },

  // ---------------------------------------------------------------------------
  // sd_recall — semantic search over the user's memory bank
  // ---------------------------------------------------------------------------
  {
    name: 'sd_recall',
    description:
      'Semantic search the Synaptic memory bank. Returns memories most relevant to the query, ' +
      'ranked by embedding similarity. Use this BEFORE answering when the user references past ' +
      'work, decisions, projects, or anything that might already be recorded — recalled memories ' +
      'beat speculation. Honors the egress chokepoint: sensitive memories are returned to local ' +
      'callers but filtered for any downstream Tier 3 / Oracle calls.',
    inputSchema: {
      type: 'object',
      required: ['query'],
      properties: {
        query: { type: 'string', description: 'Natural-language search. Phrase as you would think it, not as keywords.' },
        limit: { type: 'integer', description: 'Max results (default 8, cap 50).', minimum: 1, maximum: 50 },
        region: { type: 'string', description: 'Optional anatomical region filter (e.g. "hippocampus", "frontal_lobe").' },
        tags: { type: 'array', items: { type: 'string' }, description: 'Optional tag filter — results must contain ALL listed tags.' },
      },
    },
  },

  // ---------------------------------------------------------------------------
  // sd_recall_maps — map-level recall (higher-level abstractions)
  // ---------------------------------------------------------------------------
  {
    name: 'sd_recall_maps',
    description:
      'Recall MemoryMaps — the higher-level abstractions in the user\'s knowledge graph — rather than ' +
      'individual memories. Maps represent projects, concepts, people, technologies. Use this when the ' +
      'user asks "Can you recall the project X" or "what do I have about Y as a topic." Ranks by hybrid ' +
      'of three signals: direct name match (strongest — "recall the project Synaptic-Disorder" returns ' +
      'that map first even if cosine is fuzzy), schema-text cosine (matches conceptual queries), and ' +
      'aggregate member similarity (finds maps whose individual memories are relevant even when the ' +
      'schema isn\'t). Returns map metadata + top member excerpts per map. Use BEFORE sd_recall when ' +
      'the user is asking about a topic or named entity rather than a specific experience. To dig ' +
      'deeper into a map\'s underlying memories without a follow-up tool call, set ' +
      'include_full_members=true — each result then carries full MemoryRecord rows in `top_members`.',
    inputSchema: {
      type: 'object',
      required: ['query'],
      properties: {
        query: { type: 'string', description: 'Natural-language query. "Recall the project X" / "what maps do I have about Y."' },
        limit: { type: 'integer', description: 'Max maps returned (default 10, cap 50).', minimum: 1, maximum: 50 },
        mode:  { type: 'string', enum: ['hybrid', 'name', 'schema', 'members'], description: 'Scoring strategy. hybrid (default) blends all three signals; name forces strict name match; schema is cosine on the map schema_text; members aggregates per-member cosine.' },
        type:  { type: 'string', description: 'Optional: restrict to a map type (project|concept|person|tech).' },
        include_proposed: { type: 'boolean', description: 'Include proposed (un-accepted) maps in results. Default false.' },
        include_full_members: { type: 'boolean', description: 'When true, hydrate full MemoryRecord rows for the top members of each map (in addition to 160-char excerpts). Use when you need to act on the actual memories, not just preview the map. Default false.' },
        members_limit: { type: 'integer', description: 'Number of full member records per map when include_full_members=true. Default 5, capped 1..20.', minimum: 1, maximum: 20 },
      },
    },
  },

  // ---------------------------------------------------------------------------
  // sd_reflect — Tier 3 synthesis over local recall results (no web egress)
  // ---------------------------------------------------------------------------
  {
    name: 'sd_reflect',
    description:
      'Reflect on memories matching a query — Tier 3 synthesis over recall results. Internally runs ' +
      '`sd_recall` then asks Tier 3 to write a short synthesis prose paragraph weaving the recalled ' +
      'memories together (themes, contradictions, decisions, open threads). Returns synthesis + ' +
      'source_memory_ids + memories_excluded_by_privacy_filter (count). Tier 3 IS an egress path: ' +
      'sensitive memories are EXCLUDED by default. Set include_sensitive=true to opt into forwarding ' +
      'them on a per-call basis (response always surfaces included_sensitive_count when used). ' +
      'Use when the user asks "what do I know about X?" or "summarize my thinking on Y" — answers ' +
      'grounded in their own past work beat a generic LLM response.',
    inputSchema: {
      type: 'object',
      required: ['query'],
      properties: {
        query:  { type: 'string', description: 'Natural-language question to reflect on (max 1000 chars).' },
        limit:  { type: 'integer', description: 'Max recalled memories used as ground truth (cap 30).', minimum: 1, maximum: 30 },
        tags:   { type: 'array', items: { type: 'string' }, description: 'Optional tag filter on the recalled set.' },
        tags_match: { type: 'string', enum: ['any', 'all', 'any_strict', 'all_strict'], description: 'How to combine the tag filter; default "any" (case-insensitive).' },
        budget: { type: 'string', enum: ['low', 'mid', 'high'], description: 'Recall + token budget bundle. low=top5/512, mid=top15/1024 (default), high=top30/2048.' },
        include_sensitive: { type: 'boolean', description: 'Per-call opt-in. When true, sensitive-flagged memories ARE forwarded to Tier 3. Default false. LocalConcept filtering still runs.' },
        save_as_memory: { type: 'boolean', description: 'v2.7 Bundle R — when true, persist the synthesis as a memory with memory_type="reflection" so future recall calls can surface it. Default false. Response includes memory_id when set.' },
      },
    },
  },

  // ---------------------------------------------------------------------------
  // sd_propose_map — R13 redesign: ask Synaptic to suggest a new MemoryMap
  // ---------------------------------------------------------------------------
  {
    name: 'sd_propose_map',
    description:
      'Propose a new MemoryMap (thematic grouping) from a set of memories. The proposal lands with ' +
      'status="proposed" — the user reviews + accepts/edits/dismisses via the dashboard. Use when the ' +
      'user asks "make a map of all my Project X memories" or you notice a cluster of related memories ' +
      'that deserves explicit grouping. NEVER writes the synthesis directly into the memories table — ' +
      'lives in the maps surface as a UI-distinct artifact. Tier 2 (Nightly) provider must be configured. ' +
      'Sensitive memories are excluded from the synthesis prompt by default; set include_sensitive=true ' +
      'to opt in per-call.',
    inputSchema: {
      type: 'object',
      required: ['source'],
      properties: {
        source: { type: 'string', enum: ['tag', 'query', 'recall_id', 'auto'], description: 'Trigger flavor — tag (one tag), query (semantic), recall_id (explicit memory_ids), auto (system-triggered).' },
        tag:    { type: 'string', description: 'Required when source=tag.' },
        query:  { type: 'string', description: 'Required when source=query — natural-language theme.' },
        memory_ids: { type: 'array', items: { type: 'string' }, description: 'Required when source=recall_id|auto — explicit member set.' },
        name:   { type: 'string', description: 'Optional user-supplied name. If omitted, LLM proposes one.' },
        type:   { type: 'string', enum: ['project', 'person', 'technology', 'organization', 'concept'], description: 'Default "concept".' },
        limit:  { type: 'integer', description: 'Cap on member memories pulled (default 20, max 100).', minimum: 1, maximum: 100 },
        include_sensitive: { type: 'boolean', description: 'Per-call opt-in to forward sensitive memories to the Tier 2 synthesis. Default false.' },
      },
    },
  },

  // ---------------------------------------------------------------------------
  // sd_research — Tier 3 Oracle web research (cached + budget-tracked)
  // ---------------------------------------------------------------------------
  {
    name: 'sd_research',
    description:
      'Trigger a Tier 3 Oracle web-research query through Synaptic. Sensitive memories are filtered ' +
      'from the prompt before egress (the dashboard shows the user how many were excluded). Result is ' +
      'cached for 7 days by default — repeated queries return cache_hit:true with no additional token ' +
      'spend. Use sparingly: real Oracle calls cost real tokens. For known-cached topics, prefer sd_recall ' +
      'against the local bank first.',
    inputSchema: {
      type: 'object',
      required: ['query'],
      properties: {
        query: { type: 'string', description: 'Research question (max 1000 chars).' },
        ttl_hours: { type: 'integer', description: 'Override cache TTL (default 168h = 7 days).', minimum: 1 },
      },
    },
  },

  // ---------------------------------------------------------------------------
  // sd_list_memories — paginated browse with filters
  // ---------------------------------------------------------------------------
  {
    name: 'sd_list_memories',
    description:
      'List memories from the bank with optional filters. Useful for "show me everything tagged X" ' +
      'or "what recent memories are flagged sensitive". Returns id/summary/tags/region/timestamps + ' +
      'lifecycle flags (light_encoded, nightly_consolidated, oracle_augmented). Use sd_get_memory for ' +
      'full text + enriched_text.',
    inputSchema: {
      type: 'object',
      properties: {
        limit:            { type: 'integer', description: 'Max results (default 20, cap 200).', minimum: 1, maximum: 200 },
        only_sensitive:   { type: 'boolean', description: 'Return only sensitive-flagged memories.' },
        exclude_sensitive:{ type: 'boolean', description: 'Hide sensitive-flagged memories.' },
        only_deleted:     { type: 'boolean', description: 'Return only soft-deleted memories (Trash).' },
        include_deleted:  { type: 'boolean', description: 'Include soft-deleted memories alongside live ones.' },
      },
    },
  },

  // ---------------------------------------------------------------------------
  // sd_get_memory — full record with lifecycle + enriched_text
  // ---------------------------------------------------------------------------
  {
    name: 'sd_get_memory',
    description:
      'Fetch a single memory by id. Returns full text, tags, region, all lifecycle flags ' +
      '(light_encoded, nightly_consolidated, enriched_text, oracle_augmented, source, merged_from, ' +
      'sensitive, sensitive_checked_at, deleted_at). Use after sd_recall when you need the complete ' +
      'record beyond the search-result summary.',
    inputSchema: {
      type: 'object',
      required: ['id'],
      properties: {
        id: { type: 'string', description: 'Memory id (from sd_recall results or sd_list_memories).' },
      },
    },
  },

  // ---------------------------------------------------------------------------
  // sd_update_memory — edit text/tags/sensitive flag
  // ---------------------------------------------------------------------------
  {
    name: 'sd_update_memory',
    description:
      'PATCH a memory. Use to flip the sensitive flag (for excluding from external research), correct ' +
      'tags, or edit text. Sensitive flag has sticky-on policy: setting `sensitive: false` may be ignored ' +
      'by the backend if the content still matches a privacy-trigger pattern (the response will show the ' +
      'resolved value). Editing text or tags auto-marks the memory dirty for re-enrichment on the next ' +
      'dream cycle (Phase 0b will refresh its summary, tags, and region in light of the broader bank). ' +
      'Sensitive-flag-only edits do NOT trigger re-enrichment. The full audit history is recorded automatically.',
    inputSchema: {
      type: 'object',
      required: ['id'],
      properties: {
        id:        { type: 'string', description: 'Memory id.' },
        text:      { type: 'string', description: 'New text (replaces existing).' },
        tags:      { type: 'array', items: { type: 'string' }, description: 'New tag set (replaces existing).' },
        sensitive: { type: 'boolean', description: 'Set/clear the sensitive flag (sticky-on may override clears).' },
      },
    },
  },

  // ---------------------------------------------------------------------------
  // sd_delete_memory — soft (default) or hard delete
  // ---------------------------------------------------------------------------
  {
    name: 'sd_delete_memory',
    description:
      'Delete a memory. Default is SOFT — sets deleted_at and excludes from default queries, but the ' +
      'row stays in the DB and can be restored via sd_restore_memory. Pass `hard: true` for permanent ' +
      'deletion (irrecoverable). Use soft delete unless you have a specific reason to hard-delete ' +
      '(privacy obligation, accidental high-volume bulk import you want to truly remove).',
    inputSchema: {
      type: 'object',
      required: ['id'],
      properties: {
        id:   { type: 'string',  description: 'Memory id.' },
        hard: { type: 'boolean', description: 'If true, hard-delete (irrecoverable). Default soft.' },
      },
    },
  },

  // ---------------------------------------------------------------------------
  // sd_restore_memory — undo a soft delete
  // ---------------------------------------------------------------------------
  {
    name: 'sd_restore_memory',
    description:
      'Restore a soft-deleted memory (clear deleted_at). Only works for soft-deleted rows; hard-deleted ' +
      'rows are gone and return 404. Use sd_list_memories with `only_deleted: true` to browse the Trash ' +
      'and find candidates for restore.',
    inputSchema: {
      type: 'object',
      required: ['id'],
      properties: {
        id: { type: 'string', description: 'Memory id of a soft-deleted memory.' },
      },
    },
  },

  // ---------------------------------------------------------------------------
  // sd_save_procedure — store a durable directive (v2.7 Bundle O)
  // ---------------------------------------------------------------------------
  {
    name: 'sd_save_procedure',
    description:
      'Save a procedural memory — a durable directive Synaptic auto-injects into future inference prompts ' +
      '("always prefer pnpm over npm in this repo", "Slack messages over 800 chars need a thread"). Stored ' +
      'as memory_type="procedure" so it shows up in /recall like other memories AND in the dedicated ' +
      '/procedures endpoint. Active procedures (not deleted, not dormant) become a bullet list at the top ' +
      'of the system prompt for /reflect and any handler that opts in via BuildSystemPromptWithProcedures. ' +
      'Use when the user says "always do X" or the agent identifies a habit worth crystallising.',
    inputSchema: {
      type: 'object',
      required: ['text'],
      properties: {
        text:  { type: 'string', description: 'The procedure text — phrase as an imperative directive.' },
        scope: { type: 'string', description: 'Optional scope tag (e.g., "auth"). Lets a future caller pull a subset via /procedures?scope=...' },
        tags:  { type: 'array', items: { type: 'string' }, description: 'Additional tags. "procedure" is auto-added.' },
      },
    },
  },

  // ---------------------------------------------------------------------------
  // sd_forget — soft-delete with reason (v2.7 Bundle M)
  // ---------------------------------------------------------------------------
  {
    name: 'sd_forget',
    description:
      'Soft-delete a memory with an explicit reason recorded in the audit log. Equivalent to sd_delete_memory ' +
      'but the reason field surfaces in `sd_get_audit` and the Trash UI ("forgotten because: X") so future-you ' +
      'understands the intent. Restorable via sd_restore_memory. Use this instead of sd_delete_memory when an ' +
      'agent is autonomously pruning — leaves a breadcrumb the user can review.',
    inputSchema: {
      type: 'object',
      required: ['id', 'reason'],
      properties: {
        id:     { type: 'string', description: 'Memory id to forget.' },
        reason: { type: 'string', description: 'Human-readable rationale (e.g., "superseded by newer guidance", "wrong fact, retracted").' },
      },
    },
  },

  // ---------------------------------------------------------------------------
  // sd_supersede — replace memory A with memory B (R10 bi-temporal edge)
  // ---------------------------------------------------------------------------
  {
    name: 'sd_supersede',
    description:
      'Mark memory A as superseded by memory B. The original row stays in the bank (audit trail preserved) ' +
      'but default /recall hides it — callers get the current-truth view by default and can opt-in to history ' +
      'via include_superseded. Use when the world changed and the old fact is no longer correct, rather than ' +
      'editing the original (which destroys the timeline). Optional valid_from sets when the new fact became ' +
      'true (defaults to now).',
    inputSchema: {
      type: 'object',
      required: ['superseding_id', 'superseded_id'],
      properties: {
        superseding_id: { type: 'string', description: 'Memory id that contains the new/correct fact.' },
        superseded_id:  { type: 'string', description: 'Memory id of the old/replaced fact.' },
        valid_from:     { type: 'string', description: 'ISO 8601 timestamp when the new fact became true (default: now).' },
        reason:         { type: 'string', description: 'Why the supersession happened (e.g., "user moved to a new address").' },
      },
    },
  },

  // ---------------------------------------------------------------------------
  // sd_set_dormant — put a memory to sleep (or wake it up)
  // ---------------------------------------------------------------------------
  {
    name: 'sd_set_dormant',
    description:
      'Manually put a memory into dormancy (sleep) or wake it back up. Dormant memories stay in the bank ' +
      'but are hidden from default /recall — surface them via include_dormant=true. Use this when a memory ' +
      'is still potentially relevant but feels stale or noisy in the current context. Wake it later with ' +
      '`dormant: false` when conditions change. The nightly dormancy pass writes the same column with ' +
      'reason="ttl" when an expires_at fires.',
    inputSchema: {
      type: 'object',
      required: ['id', 'dormant'],
      properties: {
        id:      { type: 'string',  description: 'Memory id.' },
        dormant: { type: 'boolean', description: 'true = put to sleep; false = wake up.' },
      },
    },
  },

  // ---------------------------------------------------------------------------
  // sd_set_ttl — set or clear the optional expiry timestamp
  // ---------------------------------------------------------------------------
  {
    name: 'sd_set_ttl',
    description:
      'Set or clear a memory\'s optional TTL. When the timestamp passes, the nightly dormancy pass moves the ' +
      'memory into dormancy (it does NOT delete the row — dormant memories stay queryable via ' +
      'include_dormant=true). Pass an empty string to clear an existing TTL. Sensitive memories REJECT a TTL ' +
      'set (the call returns 400) — sticky-on policy keeps sensitive rows reachable for audit. Use to age out ' +
      'memories of transient relevance ("remember this for the next 30 days").',
    inputSchema: {
      type: 'object',
      required: ['id', 'expires_at'],
      properties: {
        id:         { type: 'string', description: 'Memory id.' },
        expires_at: { type: 'string', description: 'ISO 8601 timestamp, or empty string to clear.' },
      },
    },
  },

  // ---------------------------------------------------------------------------
  // sd_get_audit — write-op log (filterable)
  // ---------------------------------------------------------------------------
  {
    name: 'sd_get_audit',
    description:
      'Fetch the audit log of write operations against the bank. Each entry has action, entity_type, ' +
      'entity_id, before_json/after_json (redacted for sensitive values), adapter_id, created_at. Useful ' +
      'for "what changed since X?" or debugging.',
    inputSchema: {
      type: 'object',
      properties: {
        limit:       { type: 'integer', description: 'Max entries (default 20, cap 500).', minimum: 1, maximum: 500 },
        entity_type: { type: 'string', description: 'Filter by type (memory, map, lexicon, association, etc.).' },
        action:      { type: 'string', description: 'Filter by operation (insert, update, delete, etc.).' },
        since:       { type: 'string', description: 'ISO 8601 timestamp; only return entries created at-or-after.' },
      },
    },
  },

  // ---------------------------------------------------------------------------
  // sd_get_budget — token spend over a window
  // ---------------------------------------------------------------------------
  {
    name: 'sd_get_budget',
    description:
      'Read the token budget for the last N days. Returns total_tokens / total_in / total_out, optional ' +
      'cap_daily / cap_monthly, and a per-day breakdown. Use to check spend before an expensive Oracle ' +
      'research call, or to surface the user\'s monthly burn.',
    inputSchema: {
      type: 'object',
      properties: {
        days: { type: 'integer', description: 'Window size in days (default 30, cap 365).', minimum: 1, maximum: 365 },
      },
    },
  },

  // ---------------------------------------------------------------------------
  // sd_dream_run — trigger a nightly consolidation cycle on demand
  // ---------------------------------------------------------------------------
  {
    name: 'sd_dream_run',
    description:
      'Trigger an immediate nightly consolidation cycle (POST /nightly/run). The pipeline runs all ' +
      'shipped phases: Phase 0a/0b encoding + enrichment, Phase 1 dedup, Phase 2 synthesis, Phase 3 ' +
      'lexicon rebuild, Phase 4 maps, Phase 5 decay, Phase 7 cross-region, Phase 8 schema, Phase 9 ' +
      'reinforcement, Phase 10 replay (if enabled), Phase 11 narrative, Phase 12 dream entry. Returns ' +
      'the run id; the run executes asynchronously and surfaces results in /nightly/runs and via the ' +
      'dashboard\'s Dream Journal. Single-instance: returns 409 if a run is already in progress.',
    inputSchema: {
      type: 'object',
      properties: {},
    },
  },

  // ---------------------------------------------------------------------------
  // sd_dream_status — inspect recent dream cycles
  // ---------------------------------------------------------------------------
  {
    name: 'sd_dream_status',
    description:
      'Read the most recent N dream cycle records (GET /nightly/runs). Each row includes id, ' +
      'started_at/finished_at, status (in_progress | completed | partial | failed), full stats v3 ' +
      '(encoded, merged, consolidated, lexicon_pairs, maps_updated, deep_encoding.{coverage_percent, ' +
      'queue, remaining}, embedding_coverage, etc.), narrative, dream_entry, and any error. Use to ' +
      'verify the pipeline is making progress, debug stuck phases, or reference past runs for ' +
      'context-setting prompts.',
    inputSchema: {
      type: 'object',
      properties: {
        limit:  { type: 'integer', description: 'Max rows (default 10, cap 50).', minimum: 1, maximum: 50 },
        status: { type: 'string',  description: 'Filter by status (e.g. "failed", "partial", "completed").' },
      },
    },
  },

  // ---------------------------------------------------------------------------
  // sd_deep_encode_all — backfill Phase 0b deep enrichment in one shot
  // ---------------------------------------------------------------------------
  {
    name: 'sd_deep_encode_all',
    description:
      'Synchronously backfill Phase 0b deep encoding across the entire bank (POST /admin/deep-encode-all). ' +
      'Tier 2 (Nightly) re-summarizes, retags, and refines region for every memory. Long-running: ~70 ' +
      'minutes for 3000 memories at sequential concurrency. Returns {queued, enriched, skipped_existing, ' +
      'errors[], tokens_total, duration_ms}. Returns 424 when Tier 2 is unconfigured. Use this once after ' +
      'a fresh import to seed the bank, or after a Tier 2 model upgrade to bring all memories current.',
    inputSchema: {
      type: 'object',
      properties: {
        skip_existing:      { type: 'boolean', description: 'Skip memories already deep-encoded (default true).' },
        include_dirty_only: { type: 'boolean', description: 'Only re-enrich memories already marked dirty (default false).' },
        token_budget:       { type: 'integer', description: 'Hard cap on total tokens (default 1000000).' },
        max_concurrent:     { type: 'integer', description: 'Per-memory worker count (default 1).', minimum: 1, maximum: 8 },
      },
    },
  },

  // ---------------------------------------------------------------------------
  // sd_embed_all — backfill Tier 1 embeddings (prerequisite for Phase 1 / 7)
  // ---------------------------------------------------------------------------
  {
    name: 'sd_embed_all',
    description:
      'Synchronously backfill Tier 1 embeddings across the entire bank (POST /admin/embed-all). Phases ' +
      '1 (dedup), 2 (synthesis), 7 (cross-region), 9 (reinforcement) all silently skip when ' +
      'embedding_coverage is below threshold. Run this after a fresh import OR if the Dream Journal ' +
      'progress card shows an embedding-coverage warning. Returns 424 when Tier 1 is unconfigured.',
    inputSchema: {
      type: 'object',
      properties: {
        skip_existing:  { type: 'boolean', description: 'Skip memories whose embedding hash is already cached (default true).' },
        max_concurrent: { type: 'integer', description: 'Reserved for future async; ignored today (default 1).' },
      },
    },
  },

  // ---------------------------------------------------------------------------
  // sd_lexicon_rebuild — rebuild tag co-occurrence pairs from current state
  // ---------------------------------------------------------------------------
  {
    name: 'sd_lexicon_rebuild',
    description:
      'Force a full rebuild of the tag co-occurrence pairs table (POST /lexicon/rebuild). Phase 3 ' +
      'rebuilds this every nightly cycle, but a manual trigger is useful after bulk tag merges or to ' +
      'verify the lexicon view shows current data. Async: returns a run_id; progress streams via ' +
      'lexicon_rebuild_progress / lexicon_rebuild_done WS events.',
    inputSchema: {
      type: 'object',
      properties: {
        scope: { type: 'string', description: 'Currently only "all" is meaningful (default).' },
      },
    },
  },

  // ---------------------------------------------------------------------------
  // sd_get_dream_settings / sd_set_dream_settings — pipeline tuning
  // ---------------------------------------------------------------------------
  {
    name: 'sd_get_dream_settings',
    description:
      'Read the dream pipeline configuration (GET /settings/dream_pipeline). Returns 19 settings ' +
      'including the master toggles (deep_enrich_enabled, replay_enabled), per-phase caps and ' +
      'thresholds. Use to inspect the user\'s current tuning before suggesting changes.',
    inputSchema: { type: 'object', properties: {} },
  },

  {
    name: 'sd_set_dream_settings',
    description:
      'Update the dream pipeline configuration (PUT /settings/dream_pipeline). Pass any subset of ' +
      'settings to update; omitted fields keep their current value. Common adjustments: enabling ' +
      'replay_enabled for creative recombination, raising deep_enrich_max_per_run to drain the dirty ' +
      'queue faster, lowering decay_age_days for younger banks. Most users should not touch the ' +
      'thresholds without reading the corresponding research papers cited in CITATIONS.md.',
    inputSchema: {
      type: 'object',
      properties: {
        deep_enrich_enabled:           { type: 'boolean' },
        deep_enrich_max_per_run:       { type: 'integer', minimum: 0 },
        deep_enrich_token_budget_per_run: { type: 'integer', minimum: 0 },
        deep_enrich_concurrency:       { type: 'integer', minimum: 1, maximum: 8 },
        replay_enabled:                { type: 'boolean' },
        dedup_threshold:               { type: 'number',  minimum: 0, maximum: 1 },
        synthesis_max_per_run:         { type: 'integer', minimum: 0 },
        synthesis_cluster_min:         { type: 'integer', minimum: 2 },
        synthesis_cluster_sim:         { type: 'number',  minimum: 0, maximum: 1 },
        decay_age_days:                { type: 'integer', minimum: 0 },
        decay_recall_days:             { type: 'integer', minimum: 0 },
        decay_max_per_run:             { type: 'integer', minimum: 0 },
        map_min_memories:              { type: 'integer', minimum: 1 },
        cross_region_threshold:        { type: 'number',  minimum: 0, maximum: 1 },
        cross_region_max_per_run:      { type: 'integer', minimum: 0 },
        schema_max_per_run:            { type: 'integer', minimum: 0 },
        schema_lookback_days:          { type: 'integer', minimum: 1 },
        schema_cluster_sim:            { type: 'number',  minimum: 0, maximum: 1 },
        reinforcement_decay_factor:    { type: 'number',  minimum: 0, maximum: 1 },
      },
    },
  },
];

server.setRequestHandler(ListToolsRequestSchema, async () => ({ tools: TOOLS }));

server.setRequestHandler(CallToolRequestSchema, async (request) => {
  const { name, arguments: args = {} } = request.params;

  if (name === 'report_event') {
    const type = args.type;
    if (!type || !VALID_TYPES.has(type)) {
      return {
        isError: true,
        content: [{
          type: 'text',
          text: `Unknown event type: ${type}. Valid types: ${[...VALID_TYPES].join(', ')}`,
        }],
      };
    }
    const event = buildEvent({
      type,
      payload: args.payload,
      region_hint: args.region_hint,
      session_id: args.session_id,
    });
    const res = await postEvent(event);
    return {
      content: [{
        type: 'text',
        text: res.ok
          ? `Event ${type} delivered to SD Core.`
          : `Event ${type} not delivered (SD Core unreachable: ${res.error || res.status}). Dashboard stayed quiet; this is non-fatal.`,
      }],
    };
  }

  if (name === 'report_memory_save') {
    const text = args.text;
    if (typeof text !== 'string' || !text.length) {
      return {
        isError: true,
        content: [{ type: 'text', text: 'report_memory_save requires a non-empty `text` field.' }],
      };
    }
    const event = buildEvent({
      type: 'memory_added',
      payload: {
        memory_id: args.memory_id || `mcp-${randomUUID().slice(0, 8)}`,
        text,
        tags: Array.isArray(args.tags) ? args.tags.slice(0, 16) : [],
      },
      region_hint: args.region_hint || regionForTags(Array.isArray(args.tags) ? args.tags : []),
      session_id: args.session_id,
    });
    const res = await postEvent(event);
    return {
      content: [{
        type: 'text',
        text: res.ok
          ? `Memory ripple sent to dashboard.`
          : `SD Core unreachable (${res.error || res.status}). The save was reported but the dashboard didn't see it.`,
      }],
    };
  }

  // ---------------------------------------------------------------------------
  // Generic helper: format an SD Core request result for MCP text response.
  // Returns a CallToolResponse-shaped object. Handles ok/empty/error uniformly.
  // ---------------------------------------------------------------------------
  const respond = (res, friendlyName) => {
    if (res.ok) {
      const body = typeof res.data === 'string' ? res.data : JSON.stringify(res.data, null, 2);
      return { content: [{ type: 'text', text: body }] };
    }
    const httpHint = res.status === 401 ? ' (auth required — set SD_API_TOKEN)'
                  : res.status === 404 ? ' (endpoint missing — older SD Core?)'
                  : res.status === 405 ? ' (route collision — check path)'
                  : res.status === 429 ? ' (rate-limited or budget cap reached)'
                  : '';
    const errMsg = res.error || (typeof res.data === 'string' ? res.data : JSON.stringify(res.data || {}));
    return {
      isError: true,
      content: [{
        type: 'text',
        text: `${friendlyName} failed (HTTP ${res.status})${httpHint}: ${String(errMsg).slice(0, 400)}`,
      }],
    };
  };

  if (name === 'sd_create_memory') {
    const text = args.text;
    if (typeof text !== 'string' || !text.trim()) {
      return { isError: true, content: [{ type: 'text', text: 'sd_create_memory requires a non-empty `text`.' }] };
    }
    const tags = Array.isArray(args.tags) ? args.tags.slice(0, 16) : [];
    // Backend persists memory_added events through SaveMemory, so this
    // doubles as the dashboard pulse + bank write. Mirror the existing
    // report_memory_save shape but with explicit creation semantics.
    const event = buildEvent({
      type: 'memory_added',
      payload: {
        memory_id: (typeof args.id === 'string' && args.id) ? args.id : `mcp-${randomUUID().slice(0, 8)}`,
        text: text.trim(),
        tags,
        ...(typeof args.sensitive === 'boolean' ? { sensitive: args.sensitive } : {}),
      },
      region_hint: (typeof args.region_hint === 'string' && args.region_hint) ? args.region_hint : regionForTags(tags),
      session_id: args.session_id,
    });
    const res = await postEvent(event);
    if (res.ok) {
      return {
        content: [{
          type: 'text',
          text: `Memory created (id=${event.payload.memory_id}, region=${event.payload.region_hint || 'auto'}, tags=${tags.length}). Synapse builder will embed + link on the next pass; classifier may auto-route the region.`,
        }],
      };
    }
    return {
      isError: true,
      content: [{
        type: 'text',
        text: `sd_create_memory failed (HTTP ${res.status}): ${res.error || 'unknown'}. The memory was NOT persisted.`,
      }],
    };
  }

  if (name === 'sd_pin_memory') {
    const text = args.text;
    if (typeof text !== 'string' || !text.trim()) {
      return { isError: true, content: [{ type: 'text', text: 'sd_pin_memory requires a non-empty `text`.' }] };
    }
    // Inherit sd_create_memory's payload shape but append the `pinned` tag
    // and a `pinned: true` payload flag so the backend can prioritise it
    // in Phase 0b deep_encode (above tmr_user) and exclude it from
    // dedup/decay merges. Tag-based marker is the source of truth for
    // existing query paths; the payload flag is a hint for new ones.
    const userTags = Array.isArray(args.tags) ? args.tags.slice(0, 15) : [];
    const tags = userTags.includes('pinned') ? userTags : [...userTags, 'pinned'];
    const event = buildEvent({
      type: 'memory_added',
      payload: {
        memory_id: (typeof args.id === 'string' && args.id) ? args.id : `pin-${randomUUID().slice(0, 8)}`,
        text: text.trim(),
        tags,
        pinned: true,
        ...(typeof args.sensitive === 'boolean' ? { sensitive: args.sensitive } : {}),
      },
      region_hint: (typeof args.region_hint === 'string' && args.region_hint) ? args.region_hint : regionForTags(tags),
      session_id: args.session_id,
    });
    const res = await postEvent(event);
    if (res.ok) {
      return {
        content: [{
          type: 'text',
          text: `Memory pinned (id=${event.payload.memory_id}, region=${event.payload.region_hint || 'auto'}, tags=${tags.length}). Pinned memories are protected from dedup/decay and deep-encoded on the next nightly pass at top priority.`,
        }],
      };
    }
    return {
      isError: true,
      content: [{ type: 'text', text: `sd_pin_memory failed (HTTP ${res.status}): ${res.error || 'unknown'}.` }],
    };
  }

  if (name === 'sd_recall') {
    const query = args.query;
    if (typeof query !== 'string' || !query.trim()) {
      return { isError: true, content: [{ type: 'text', text: 'sd_recall requires a non-empty `query`.' }] };
    }
    const body = { query: query.trim() };
    if (Number.isInteger(args.limit))   body.limit  = args.limit;
    if (typeof args.region === 'string' && args.region) body.region = args.region;
    if (Array.isArray(args.tags))       body.tags   = args.tags.slice(0, 16);
    // Recall does query embedding + BM25 + cross-encoder rerank over the whole
    // bank — measured 2.7s warm / ~11s cold on a 5k-memory bank, far over the
    // 1500ms default, so it ALWAYS timed out. Give it generous headroom (every
    // other semantic endpoint already overrides the default).
    return respond(await sdRequest({ method: 'POST', path: '/recall', body, timeoutMs: 30_000 }), 'sd_recall');
  }

  if (name === 'sd_recall_maps') {
    const query = args.query;
    if (typeof query !== 'string' || !query.trim()) {
      return { isError: true, content: [{ type: 'text', text: 'sd_recall_maps requires a non-empty `query`.' }] };
    }
    const body = { query: query.trim() };
    if (Number.isInteger(args.limit))   body.limit = Math.min(args.limit, 50);
    if (typeof args.mode === 'string' && ['hybrid','name','schema','members'].includes(args.mode)) body.mode = args.mode;
    if (typeof args.type === 'string' && args.type) body.type = args.type;
    if (args.include_proposed === true) body.include_proposed = true;
    if (args.include_full_members === true) body.include_full_members = true;
    if (Number.isInteger(args.members_limit)) body.members_limit = Math.max(1, Math.min(20, args.members_limit));
    // Members mode walks every memory's embedding — allow up to 60s for
    // banks with thousands of memories on a cold cache.
    return respond(await sdRequest({ method: 'POST', path: '/recall/maps', body, timeoutMs: 60_000 }), 'sd_recall_maps');
  }

  if (name === 'sd_propose_map') {
    const source = args.source;
    if (!['tag', 'query', 'recall_id', 'auto'].includes(source)) {
      return { isError: true, content: [{ type: 'text', text: 'sd_propose_map requires source one of: tag|query|recall_id|auto.' }] };
    }
    const body = { source };
    if (typeof args.tag === 'string')   body.tag   = args.tag.trim();
    if (typeof args.query === 'string') body.query = args.query.trim();
    if (Array.isArray(args.memory_ids)) body.memory_ids = args.memory_ids.slice(0, 100);
    if (typeof args.name === 'string')  body.name  = args.name.trim();
    if (typeof args.type === 'string')  body.type  = args.type;
    if (Number.isInteger(args.limit))   body.limit = Math.min(args.limit, 100);
    if (args.include_sensitive === true) body.include_sensitive = true;
    // Synthesis on Tier 2 — can take a minute on AirLLM 70B; allow 120s.
    return respond(await sdRequest({ method: 'POST', path: '/maps/propose', body, timeoutMs: 120_000 }), 'sd_propose_map');
  }

  if (name === 'sd_research') {
    const query = args.query;
    if (typeof query !== 'string' || !query.trim()) {
      return { isError: true, content: [{ type: 'text', text: 'sd_research requires a non-empty `query`.' }] };
    }
    if (query.length > 1000) {
      return { isError: true, content: [{ type: 'text', text: `query too long (${query.length} chars · max 1000).` }] };
    }
    const body = { query: query.trim() };
    if (Number.isInteger(args.ttl_hours)) body.ttl_hours = args.ttl_hours;
    return respond(await sdRequest({ method: 'POST', path: '/research', body, timeoutMs: 60_000 }), 'sd_research');
  }

  if (name === 'sd_reflect') {
    const query = args.query;
    if (typeof query !== 'string' || !query.trim()) {
      return { isError: true, content: [{ type: 'text', text: 'sd_reflect requires a non-empty `query`.' }] };
    }
    if (query.length > 1000) {
      return { isError: true, content: [{ type: 'text', text: `query too long (${query.length} chars · max 1000).` }] };
    }
    const body = { query: query.trim() };
    if (Number.isInteger(args.limit))  body.limit  = Math.min(args.limit, 30);
    if (Array.isArray(args.tags))      body.tags   = args.tags.slice(0, 16);
    if (typeof args.tags_match === 'string') body.tags_match = args.tags_match;
    if (typeof args.budget === 'string') body.budget = args.budget;
    if (args.include_sensitive === true) body.include_sensitive = true;
    if (args.save_as_memory === true)    body.save_as_memory    = true;
    // Tier 3 synthesis can be slow — match sd_research's 60s ceiling.
    return respond(await sdRequest({ method: 'POST', path: '/reflect', body, timeoutMs: 60_000 }), 'sd_reflect');
  }

  if (name === 'sd_list_memories') {
    const qs = new URLSearchParams();
    if (Number.isInteger(args.limit))     qs.set('limit',             String(args.limit));
    if (args.only_sensitive   === true)   qs.set('only_sensitive',    '1');
    if (args.exclude_sensitive === true)  qs.set('exclude_sensitive', '1');
    if (args.only_deleted     === true)   qs.set('only_deleted',      '1');
    if (args.include_deleted  === true)   qs.set('include_deleted',   '1');
    const path = '/bank/memories' + (qs.toString() ? '?' + qs.toString() : '');
    return respond(await sdRequest({ method: 'GET', path }), 'sd_list_memories');
  }

  if (name === 'sd_get_memory') {
    const id = args.id;
    if (typeof id !== 'string' || !id) {
      return { isError: true, content: [{ type: 'text', text: 'sd_get_memory requires an `id`.' }] };
    }
    return respond(await sdRequest({ method: 'GET', path: `/bank/memories/${encodeURIComponent(id)}` }), 'sd_get_memory');
  }

  if (name === 'sd_update_memory') {
    const id = args.id;
    if (typeof id !== 'string' || !id) {
      return { isError: true, content: [{ type: 'text', text: 'sd_update_memory requires an `id`.' }] };
    }
    const body = {};
    if (typeof args.text === 'string')       body.text      = args.text;
    if (Array.isArray(args.tags))            body.tags      = args.tags.slice(0, 32);
    if (typeof args.sensitive === 'boolean') body.sensitive = args.sensitive;
    if (Object.keys(body).length === 0) {
      return { isError: true, content: [{ type: 'text', text: 'sd_update_memory needs at least one of: text, tags, sensitive.' }] };
    }
    return respond(await sdRequest({ method: 'PATCH', path: `/bank/memories/${encodeURIComponent(id)}`, body }), 'sd_update_memory');
  }

  if (name === 'sd_delete_memory') {
    const id = args.id;
    if (typeof id !== 'string' || !id) {
      return { isError: true, content: [{ type: 'text', text: 'sd_delete_memory requires an `id`.' }] };
    }
    const path = `/bank/memories/${encodeURIComponent(id)}` + (args.hard === true ? '?hard=1' : '');
    const res  = await sdRequest({ method: 'DELETE', path });
    if (res.ok) {
      const mode = args.hard === true ? 'hard-deleted (irrecoverable)' : 'soft-deleted (restorable via sd_restore_memory)';
      return { content: [{ type: 'text', text: `Memory ${id} ${mode}.` }] };
    }
    return respond(res, 'sd_delete_memory');
  }

  if (name === 'sd_restore_memory') {
    const id = args.id;
    if (typeof id !== 'string' || !id) {
      return { isError: true, content: [{ type: 'text', text: 'sd_restore_memory requires an `id`.' }] };
    }
    const res = await sdRequest({ method: 'POST', path: `/bank/memories/${encodeURIComponent(id)}/restore` });
    if (res.ok) {
      return { content: [{ type: 'text', text: `Memory ${id} restored.` }] };
    }
    return respond(res, 'sd_restore_memory');
  }

  // ── v2.7 Bundle O — procedural memory ──────────────────────────────────

  if (name === 'sd_save_procedure') {
    const text = typeof args.text === 'string' ? args.text.trim() : '';
    if (!text) {
      return { isError: true, content: [{ type: 'text', text: 'sd_save_procedure requires a non-empty `text`.' }] };
    }
    const tags = Array.isArray(args.tags) ? args.tags.slice(0, 16) : [];
    // Always include the canonical "procedure" tag so /recall users can
    // pull procedural memories with a tag filter even without hitting
    // the dedicated /procedures endpoint.
    if (!tags.some((t) => typeof t === 'string' && t.toLowerCase() === 'procedure')) {
      tags.unshift('procedure');
    }
    if (typeof args.scope === 'string' && args.scope.trim()) {
      const scopeTag = args.scope.trim();
      if (!tags.some((t) => t.toLowerCase() === scopeTag.toLowerCase())) {
        tags.push(scopeTag);
      }
    }
    const event = buildEvent({
      type: 'memory_added',
      payload: {
        memory_id:   `mcp-proc-${randomUUID().slice(0, 8)}`,
        text,
        tags,
        memory_type: 'procedure',
      },
      region_hint: regionForTags(tags),
      session_id:  args.session_id,
    });
    const res = await postEvent(event);
    if (res.ok) {
      return {
        content: [{
          type: 'text',
          text: `Procedure saved (id=${event.payload.memory_id}, tags=${tags.length}). It will auto-inject into the system prompt for /reflect and other consumer calls.`,
        }],
      };
    }
    return {
      isError: true,
      content: [{ type: 'text', text: `sd_save_procedure failed (HTTP ${res.status}): ${res.error || 'unknown'}.` }],
    };
  }

  // ── v2.7 Bundle M — agent-callable mutations ────────────────────────────

  if (name === 'sd_forget') {
    const id = args.id;
    const reason = typeof args.reason === 'string' ? args.reason.trim() : '';
    if (typeof id !== 'string' || !id) {
      return { isError: true, content: [{ type: 'text', text: 'sd_forget requires an `id`.' }] };
    }
    if (!reason) {
      return { isError: true, content: [{ type: 'text', text: 'sd_forget requires a non-empty `reason` — the audit row needs context.' }] };
    }
    // Reason rides on the soft-delete via a query param the backend records
    // in audit.before_json under "deleted_reason". This mirrors the existing
    // soft-delete shape — no schema change required.
    const path = `/bank/memories/${encodeURIComponent(id)}?reason=${encodeURIComponent(reason)}`;
    const res = await sdRequest({ method: 'DELETE', path });
    if (res.ok) {
      return { content: [{ type: 'text', text: `Memory ${id} forgotten (soft-deleted, restorable). Reason: ${reason}` }] };
    }
    return respond(res, 'sd_forget');
  }

  if (name === 'sd_supersede') {
    const sup = args.superseding_id;
    const old = args.superseded_id;
    if (typeof sup !== 'string' || !sup) {
      return { isError: true, content: [{ type: 'text', text: 'sd_supersede requires `superseding_id`.' }] };
    }
    if (typeof old !== 'string' || !old) {
      return { isError: true, content: [{ type: 'text', text: 'sd_supersede requires `superseded_id`.' }] };
    }
    if (sup === old) {
      return { isError: true, content: [{ type: 'text', text: 'sd_supersede: a memory cannot supersede itself.' }] };
    }
    const body = { superseded_id: old };
    if (typeof args.valid_from === 'string' && args.valid_from) body.valid_from = args.valid_from;
    if (typeof args.reason === 'string' && args.reason)         body.reason     = args.reason;
    return respond(
      await sdRequest({ method: 'POST', path: `/bank/memories/${encodeURIComponent(sup)}/supersede`, body }),
      'sd_supersede',
    );
  }

  if (name === 'sd_set_dormant') {
    const id = args.id;
    if (typeof id !== 'string' || !id) {
      return { isError: true, content: [{ type: 'text', text: 'sd_set_dormant requires an `id`.' }] };
    }
    if (typeof args.dormant !== 'boolean') {
      return { isError: true, content: [{ type: 'text', text: 'sd_set_dormant requires `dormant` (boolean).' }] };
    }
    return respond(
      await sdRequest({
        method: 'PATCH',
        path:   `/bank/memories/${encodeURIComponent(id)}`,
        body:   { dormant: args.dormant },
      }),
      'sd_set_dormant',
    );
  }

  if (name === 'sd_set_ttl') {
    const id = args.id;
    if (typeof id !== 'string' || !id) {
      return { isError: true, content: [{ type: 'text', text: 'sd_set_ttl requires an `id`.' }] };
    }
    // Empty string is a valid value (clears the TTL); only reject non-string types.
    if (typeof args.expires_at !== 'string') {
      return { isError: true, content: [{ type: 'text', text: 'sd_set_ttl requires `expires_at` (ISO 8601 string, or empty string to clear).' }] };
    }
    return respond(
      await sdRequest({
        method: 'PATCH',
        path:   `/bank/memories/${encodeURIComponent(id)}`,
        body:   { expires_at: args.expires_at },
      }),
      'sd_set_ttl',
    );
  }

  if (name === 'sd_get_audit') {
    const qs = new URLSearchParams();
    if (Number.isInteger(args.limit)) qs.set('limit', String(args.limit));
    if (typeof args.entity_type === 'string' && args.entity_type) qs.set('entity_type', args.entity_type);
    if (typeof args.action      === 'string' && args.action)      qs.set('action',      args.action);
    if (typeof args.since       === 'string' && args.since)       qs.set('since',       args.since);
    const path = '/audit' + (qs.toString() ? '?' + qs.toString() : '');
    return respond(await sdRequest({ method: 'GET', path }), 'sd_get_audit');
  }

  if (name === 'sd_get_budget') {
    const days = Number.isInteger(args.days) ? args.days : 30;
    return respond(await sdRequest({ method: 'GET', path: `/budget?days=${days}` }), 'sd_get_budget');
  }

  // ---------------------------------------------------------------------------
  // Phase 0b dream pipeline tools (sd_dream_run / sd_dream_status,
  // sd_deep_encode_all / sd_embed_all, sd_lexicon_rebuild,
  // sd_get_dream_settings / sd_set_dream_settings)
  // ---------------------------------------------------------------------------
  if (name === 'sd_dream_run') {
    return respond(await sdRequest({ method: 'POST', path: '/nightly/run', body: {} }), 'sd_dream_run');
  }

  if (name === 'sd_dream_status') {
    const qs = new URLSearchParams();
    qs.set('limit', String(Number.isInteger(args.limit) ? Math.min(args.limit, 50) : 10));
    if (typeof args.status === 'string' && args.status) qs.set('status', args.status);
    return respond(await sdRequest({ method: 'GET', path: '/nightly/runs?' + qs.toString() }), 'sd_dream_status');
  }

  if (name === 'sd_deep_encode_all') {
    const body = {
      skip_existing:      args.skip_existing      !== false,
      include_dirty_only: !!args.include_dirty_only,
      max_concurrent:     Number.isInteger(args.max_concurrent) ? args.max_concurrent : 1,
      token_budget:       Number.isInteger(args.token_budget) ? args.token_budget : 1000000,
    };
    return respond(
      await sdRequest({ method: 'POST', path: '/admin/deep-encode-all', body, timeoutMs: 5_400_000 }),
      'sd_deep_encode_all',
    );
  }

  if (name === 'sd_embed_all') {
    const body = {
      skip_existing:  args.skip_existing  !== false,
      max_concurrent: Number.isInteger(args.max_concurrent) ? args.max_concurrent : 1,
    };
    return respond(
      await sdRequest({ method: 'POST', path: '/admin/embed-all', body, timeoutMs: 1_800_000 }),
      'sd_embed_all',
    );
  }

  if (name === 'sd_lexicon_rebuild') {
    const body = { scope: args.scope || 'all' };
    // Full lexicon-graph rebuild over the whole bank — an expensive admin op
    // that can run minutes; the 1500ms default would always time out.
    return respond(await sdRequest({ method: 'POST', path: '/lexicon/rebuild', body, timeoutMs: 600_000 }), 'sd_lexicon_rebuild');
  }

  if (name === 'sd_get_dream_settings') {
    return respond(await sdRequest({ method: 'GET', path: '/settings/dream_pipeline' }), 'sd_get_dream_settings');
  }

  if (name === 'sd_set_dream_settings') {
    // Pass through any subset of the documented keys; backend ignores unknowns.
    return respond(
      await sdRequest({ method: 'PUT', path: '/settings/dream_pipeline', body: args }),
      'sd_set_dream_settings',
    );
  }

  return {
    isError: true,
    content: [{ type: 'text', text: `Unknown tool: ${name}` }],
  };
});

// -----------------------------------------------------------------------------
// Lifecycle: announce session_start, hook shutdown for session_end
// -----------------------------------------------------------------------------
async function announceSessionStart() {
  await postEvent(buildEvent({
    type: 'session_start',
    payload: {
      client: CLIENT_NAME,
      model: MODEL_NAME,
      adapter_version: SERVER_VERSION,
    },
  }));
}

let endingSent = false;
async function announceSessionEnd(reason) {
  if (endingSent) return;
  endingSent = true;
  // Use a tighter timeout on shutdown so we don't hang the parent process.
  await postEvent(buildEvent({
    type: 'session_end',
    payload: { reason: reason || 'unknown' },
  }));
}

for (const sig of ['SIGINT', 'SIGTERM', 'SIGHUP']) {
  process.on(sig, async () => {
    await announceSessionEnd(sig);
    process.exit(0);
  });
}
process.on('beforeExit', () => announceSessionEnd('beforeExit'));

// -----------------------------------------------------------------------------
// Boot
// -----------------------------------------------------------------------------
const transport = new StdioServerTransport();
await server.connect(transport);
log(`connected — adapter=${ADAPTER_ID} session=${SESSION_ID} → ${TARGET.label}${SD_API_TOKEN ? ' (auth: bearer)' : ''}`);
await announceSessionStart();
