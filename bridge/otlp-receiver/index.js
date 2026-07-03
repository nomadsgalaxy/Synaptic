#!/usr/bin/env node
// =============================================================================
// Synaptic — OTLP/HTTP Receiver (B5)
//
// Accepts OpenTelemetry trace spans (OTLP/HTTP JSON) from OpenInference-
// instrumented AI frameworks and translates each span into an SD brain event.
//
// Supported frameworks (via openinference-instrumentation-*):
//   LangChain · LlamaIndex · AutoGen · CrewAI · DSPy · OpenAI · Anthropic
//
// Endpoint: POST http://localhost:4318/v1/traces   (OTLP/HTTP JSON)
//
// Quick start:
//   export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
//   node bridge/otlp-receiver/index.js
//
// OpenInference span kind → SD event mapping:
//   LLM       → prompt_received + response_complete (tokens, model name)
//   CHAIN     → subagent_complete  (prefrontal_cortex)
//   AGENT     → subagent_complete  (prefrontal_cortex)
//   TOOL      → tool_call + tool_result  (motor_cortex / cerebellum)
//   RETRIEVER → memory_recall  (hippocampus)
//   EMBEDDING → memory_added   (hippocampus)
//   GUARDRAIL → error if triggered  (amygdala)
//
// Design rules:
//   - Always ACK the framework immediately (never block).
//   - Fire-and-forget to SD Core; silent on failure.
//   - No third-party deps — Node built-ins only.
//   - Same config.json / env-var pattern as the other SD bridges.
// =============================================================================

import http  from 'node:http';
import https from 'node:https';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import { URL as NodeURL } from 'node:url';
import { randomUUID } from 'node:crypto';

// -----------------------------------------------------------------------------
// Config (config.json → env vars → built-in defaults, in that priority order)
// -----------------------------------------------------------------------------
const __dirname = dirname(fileURLToPath(import.meta.url));
try {
  const _cfg = JSON.parse(readFileSync(join(__dirname, 'config.json'), 'utf8'));
  for (const [k, v] of Object.entries(_cfg)) {
    if (v && !process.env[k]) process.env[k] = String(v);
  }
} catch (_) {}

const SD_CORE_URL        = process.env.SD_CORE_URL  || '';
const SD_CORE_HOST       = process.env.SD_CORE_HOST || '127.0.0.1';
const SD_CORE_PORT       = parseInt(process.env.SD_CORE_PORT  || '9911', 10);
const SD_API_TOKEN       = process.env.SD_API_TOKEN || '';
const ADAPTER_ID         = process.env.SD_ADAPTER_ID || 'otlp-receiver';
const OTLP_PORT          = parseInt(process.env.OTLP_PORT || '4318', 10);
const REQUEST_TIMEOUT_MS = 1500;

function resolveTarget() {
  if (SD_CORE_URL) {
    const u = new NodeURL(SD_CORE_URL);
    return {
      host: u.hostname,
      port: u.port ? parseInt(u.port, 10) : (u.protocol === 'https:' ? 443 : 80),
      pathPrefix: u.pathname.replace(/\/$/, ''),
      transport: u.protocol === 'https:' ? https : http,
      label: `${u.protocol}//${u.hostname}${u.port ? ':' + u.port : ''}`,
    };
  }
  return {
    host: SD_CORE_HOST, port: SD_CORE_PORT,
    pathPrefix: '', transport: http,
    label: `${SD_CORE_HOST}:${SD_CORE_PORT}`,
  };
}
const TARGET = resolveTarget();

// -----------------------------------------------------------------------------
// Post to SD Core (fire-and-forget)
// -----------------------------------------------------------------------------
function postEvent(event) {
  return new Promise((resolve) => {
    const body = Buffer.from(JSON.stringify(event), 'utf8');
    const headers = { 'Content-Type': 'application/json', 'Content-Length': body.length };
    if (SD_API_TOKEN) headers['Authorization'] = `Bearer ${SD_API_TOKEN}`;
    const req = TARGET.transport.request(
      { host: TARGET.host, port: TARGET.port, path: `${TARGET.pathPrefix}/event`,
        method: 'POST', headers, timeout: REQUEST_TIMEOUT_MS },
      (res) => { res.on('data', () => {}); res.on('end', () => resolve(true)); }
    );
    req.on('timeout', () => { req.destroy(); resolve(false); });
    req.on('error',   () => resolve(false));
    req.write(body);
    req.end();
  });
}

// -----------------------------------------------------------------------------
// OTLP attribute helpers
// -----------------------------------------------------------------------------
// OTLP JSON encodes int64 as strings; values come as {stringValue|intValue|...}
function getAttr(attributes, key) {
  const a = (attributes || []).find(a => a.key === key);
  if (!a) return null;
  const v = a.value || {};
  return v.stringValue ?? v.intValue ?? v.doubleValue ?? v.boolValue ?? null;
}

// OTLP nanosecond timestamps are int64 strings (too large for JS Number safely)
function nanoToISO(ns) {
  if (!ns) return new Date().toISOString();
  try { return new Date(Number(BigInt(ns) / 1_000_000n)).toISOString(); }
  catch { return new Date().toISOString(); }
}

// -----------------------------------------------------------------------------
// OpenInference span → SD events
// -----------------------------------------------------------------------------
function spanToEvents(span, resourceAttrs) {
  const attrs   = span.attributes || [];
  const kind    = (getAttr(attrs, 'openinference.span.kind') || '').toUpperCase();
  // Prefer session.id tag; fall back to trace id so all spans in a trace group together
  const session = getAttr(attrs, 'session.id')
                || getAttr(attrs, 'llm.session_id')
                || `otlp-${(span.traceId || '').slice(0, 8) || randomUUID().slice(0, 8)}`;
  const model   = getAttr(attrs, 'llm.model_name')
                || getAttr(resourceAttrs, 'service.name')
                || 'unknown';
  const ts      = nanoToISO(span.endTimeUnixNano || span.startTimeUnixNano);
  const ok      = (span.status?.code ?? 0) !== 2; // OTEL STATUS_CODE_ERROR = 2

  const base = { schema_version: '1.0', adapter_id: ADAPTER_ID, session_id: session, timestamp: ts };

  switch (kind) {
    case 'LLM': {
      const events = [];
      const inputText  = getAttr(attrs, 'input.value') || '';
      const totalTok   = getAttr(attrs, 'llm.token_count.total');
      const promptTok  = getAttr(attrs, 'llm.token_count.prompt');
      const completTok = getAttr(attrs, 'llm.token_count.completion');
      if (inputText) {
        events.push({ ...base, type: 'prompt_received',
          payload: { text: inputText.slice(0, 200), char_count: inputText.length, region_hint: 'wernicke_area' } });
      }
      events.push({ ...base, type: 'response_complete',
        payload: {
          model,
          ...(totalTok   != null && { total_tokens:       totalTok   }),
          ...(promptTok  != null && { prompt_tokens:      promptTok  }),
          ...(completTok != null && { completion_tokens:  completTok }),
          region_hint: 'broca_area',
        } });
      return events;
    }

    case 'CHAIN':
      return [{ ...base, type: 'subagent_complete',
        payload: { subagent_id: span.name || 'chain', ok, region_hint: 'prefrontal_cortex' } }];

    case 'AGENT':
      return [{ ...base, type: 'subagent_complete',
        payload: { subagent_id: span.name || 'agent', ok, region_hint: 'prefrontal_cortex' } }];

    case 'TOOL': {
      const toolName = getAttr(attrs, 'tool.name') || span.name || 'unknown';
      return [
        { ...base, type: 'tool_call',   payload: { tool_name: toolName, region_hint: 'motor_cortex' } },
        { ...base, type: 'tool_result', payload: { tool_name: toolName, ok, region_hint: 'cerebellum' } },
      ];
    }

    case 'RETRIEVER':
      return [{ ...base, type: 'memory_recall',
        payload: { source: 'retriever', region_hint: 'hippocampus' } }];

    case 'EMBEDDING':
      return [{ ...base, type: 'memory_added',
        payload: { source: 'embedding', region_hint: 'hippocampus' } }];

    case 'GUARDRAIL':
      return ok ? [] : [{ ...base, type: 'error',
        payload: { where: 'guardrail', error_message: span.name || 'guardrail triggered', region_hint: 'amygdala' } }];

    default:
      return []; // unknown / unset kind — skip rather than produce noise
  }
}

// -----------------------------------------------------------------------------
// Parse OTLP JSON payload → flat array of SD events
// -----------------------------------------------------------------------------
function otlpToEvents(body) {
  const events = [];
  for (const rs of body.resourceSpans || []) {
    const resourceAttrs = rs.resource?.attributes || [];
    // OTLP 1.0 uses scopeSpans; older exporters use instrumentationLibrarySpans
    for (const ss of rs.scopeSpans || rs.instrumentationLibrarySpans || []) {
      for (const span of ss.spans || []) {
        events.push(...spanToEvents(span, resourceAttrs));
      }
    }
  }
  return events;
}

// -----------------------------------------------------------------------------
// HTTP server
// -----------------------------------------------------------------------------
const server = http.createServer((req, res) => {
  if (req.method === 'GET' && req.url === '/healthz') {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ ok: true, adapter: ADAPTER_ID, sd_core: TARGET.label }));
    return;
  }

  if (req.method === 'POST' && req.url === '/v1/traces') {
    const chunks = [];
    req.on('data', c => chunks.push(c));
    req.on('end', async () => {
      // ACK immediately — never hold up the framework
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end('{}');
      try {
        const body   = JSON.parse(Buffer.concat(chunks).toString('utf8'));
        const events = otlpToEvents(body);
        await Promise.all(events.map(postEvent));
      } catch (_) {}
    });
    req.on('error', () => { if (!res.headersSent) { res.writeHead(400); res.end(); } });
    return;
  }

  res.writeHead(404);
  res.end();
});

server.listen(OTLP_PORT, () => {
  process.stderr.write(
    `[synaptic-otlp] listening on :${OTLP_PORT} → SD Core ${TARGET.label}${SD_API_TOKEN ? ' (auth: bearer)' : ''}\n`
  );
});
