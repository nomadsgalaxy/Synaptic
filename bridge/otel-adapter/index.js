#!/usr/bin/env node
// =============================================================================
// Synaptic — OTel Adapter
//
// An OTLP/HTTP receiver that translates OpenTelemetry traces into Synaptic
// Disorder events. Listens on :4318/v1/traces (the OTLP/HTTP standard port)
// and accepts both `application/json` and `application/x-protobuf` bodies,
// matching what mainstream instrumentations emit by default.
//
// Supported instrumentations (verified attribute conventions):
//   - opentelemetry-instrumentation-anthropic   (GenAI semconv)
//   - opentelemetry-instrumentation-openai-v2   (GenAI semconv)
//   - openinference-instrumentation-{langchain,llama_index,autogen,crewai,...}
//   - any framework that emits GenAI / OpenInference attributes
//
// Span → SD event mapping (one summary event per span at receive time):
//   - root span                → session_start (deduced model)
//   - LLM-call span (gen_ai.*) → model_thinking + response_complete
//                                 + response_streaming for any per-chunk events
//   - TOOL span                → tool_call + tool_result
//   - RETRIEVER span           → memory_recall
//   - EMBEDDING span           → model_thinking (frontal_lobe)
//   - AGENT span (non-root)    → subagent_spawn + subagent_complete
//   - status=ERROR             → error (in addition to whatever above)
//
// All forwarding is fire-and-forget against SD Core on :9911/event. If SD
// Core is offline, we still ack the OTel exporter with 200 OK so we don't
// trigger backpressure / retries on the user's app.
// =============================================================================

import http  from 'node:http';
import https from 'node:https';
import { fileURLToPath, URL } from 'node:url';
import { dirname, resolve } from 'node:path';
import { randomUUID } from 'node:crypto';
import protobuf from 'protobufjs';

// -----------------------------------------------------------------------------
// Config
// -----------------------------------------------------------------------------
// Remote-mode preferred: SD_CORE_URL = "https://cognito.example.com" with
// SD_API_TOKEN set. Backwards-compat HOST + PORT pair still works.
const SD_CORE_URL  = process.env.SD_CORE_URL || '';
const SD_CORE_HOST = process.env.SD_CORE_HOST || '127.0.0.1';
const SD_CORE_PORT = parseInt(process.env.SD_CORE_PORT || '9911', 10);
const SD_API_TOKEN = process.env.SD_API_TOKEN || '';
const LISTEN_HOST  = process.env.OTEL_LISTEN_HOST  || '127.0.0.1';
const LISTEN_PORT  = parseInt(process.env.OTEL_LISTEN_PORT || '4318', 10);
const ADAPTER_ID   = process.env.SD_ADAPTER_ID || 'otel-adapter';
const REQ_TIMEOUT_MS = 1500;

function resolveTarget() {
  if (SD_CORE_URL) {
    const u = new URL(SD_CORE_URL);
    return {
      host: u.hostname,
      port: u.port ? parseInt(u.port, 10) : (u.protocol === 'https:' ? 443 : 80),
      pathPrefix: u.pathname.replace(/\/$/, ''),
      transport: u.protocol === 'https:' ? https : http,
      label: `${u.protocol}//${u.hostname}${u.port ? ':' + u.port : ''}`,
    };
  }
  return {
    host: SD_CORE_HOST,
    port: SD_CORE_PORT,
    pathPrefix: '',
    transport: http,
    label: `${SD_CORE_HOST}:${SD_CORE_PORT}`,
  };
}
const TARGET = resolveTarget();

// -----------------------------------------------------------------------------
// Load vendored OTLP traces protos
// -----------------------------------------------------------------------------
const __dirname = dirname(fileURLToPath(import.meta.url));
const protoRoot = new protobuf.Root();
protoRoot.resolvePath = (origin, target) => resolve(__dirname, 'proto', target);
await protoRoot.load(
  'opentelemetry/proto/collector/trace/v1/trace_service.proto',
  { keepCase: false }
);
const ExportRequest = protoRoot.lookupType(
  'opentelemetry.proto.collector.trace.v1.ExportTraceServiceRequest'
);
const ExportResponse = protoRoot.lookupType(
  'opentelemetry.proto.collector.trace.v1.ExportTraceServiceResponse'
);

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------
function log(...args) {
  process.stderr.write('[synaptic-otel] ' + args.join(' ') + '\n');
}

// AnyValue → JS primitive
function anyValueToJs(v) {
  if (!v) return null;
  if (v.stringValue !== undefined && v.stringValue !== null) return v.stringValue;
  if (v.boolValue !== undefined && v.boolValue !== null) return v.boolValue;
  if (v.intValue !== undefined && v.intValue !== null) {
    // protobufjs Long → number when safe
    return typeof v.intValue === 'object' && 'toNumber' in v.intValue
      ? v.intValue.toNumber()
      : Number(v.intValue);
  }
  if (v.doubleValue !== undefined && v.doubleValue !== null) return v.doubleValue;
  if (v.bytesValue) return Buffer.from(v.bytesValue).toString('base64');
  if (v.arrayValue) return (v.arrayValue.values || []).map(anyValueToJs);
  if (v.kvlistValue) {
    const o = {};
    for (const kv of v.kvlistValue.values || []) o[kv.key] = anyValueToJs(kv.value);
    return o;
  }
  return null;
}

function attrsToObj(attrs) {
  const out = {};
  for (const kv of attrs || []) out[kv.key] = anyValueToJs(kv.value);
  return out;
}

function bytesToHex(b) {
  if (!b) return '';
  if (typeof b === 'string') {
    // JSON OTLP sends base64
    try { return Buffer.from(b, 'base64').toString('hex'); } catch { return b; }
  }
  return Buffer.from(b).toString('hex');
}

// -----------------------------------------------------------------------------
// SD Core forwarder (fire-and-forget, no throw)
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
        timeout: REQ_TIMEOUT_MS,
      },
      (res) => {
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

function buildEvent(type, sessionId, payload) {
  return {
    schema_version: '1.0',
    type,
    timestamp: new Date().toISOString(),
    adapter_id: ADAPTER_ID,
    session_id: sessionId,
    payload: payload || {},
  };
}

// -----------------------------------------------------------------------------
// Span attribute extractors (handles GenAI + OpenInference conventions)
// -----------------------------------------------------------------------------
function modelOf(attrs) {
  return attrs['gen_ai.request.model']
      || attrs['gen_ai.response.model']
      || attrs['llm.model_name']
      || attrs['llm.invocation_parameters.model']
      || null;
}

function clientOf(attrs, resourceAttrs) {
  return attrs['gen_ai.system']
      || resourceAttrs['service.name']
      || 'otel';
}

function tokensOf(attrs) {
  const totalA = attrs['gen_ai.usage.input_tokens'];
  const totalB = attrs['gen_ai.usage.output_tokens'];
  const totalC = attrs['gen_ai.usage.total_tokens'];
  const oiPrompt = attrs['llm.token_count.prompt'];
  const oiComp   = attrs['llm.token_count.completion'];
  const oiTotal  = attrs['llm.token_count.total'];
  const sum = (a, b) => (a || 0) + (b || 0);
  return {
    prompt:     totalA ?? oiPrompt ?? null,
    completion: totalB ?? oiComp   ?? null,
    total:      totalC ?? oiTotal  ?? sum(totalA ?? oiPrompt, totalB ?? oiComp) ?? null,
  };
}

function spanKindOf(attrs) {
  // Prefer OpenInference convention; fall back to heuristics on span name
  const oi = attrs['openinference.span.kind'];
  if (oi) return String(oi).toUpperCase();
  if (attrs['gen_ai.system']) return 'LLM';
  if (attrs['tool.name'] || attrs['gen_ai.tool.name']) return 'TOOL';
  return null;
}

function regionForKind(kind, toolName) {
  switch (kind) {
    case 'LLM':       return 'broca_area';
    case 'TOOL':      return toolName === 'web_search' ? 'temporal_lobe_left' : 'motor_cortex';
    case 'RETRIEVER': return 'hippocampus';
    case 'EMBEDDING': return 'frontal_lobe';
    case 'AGENT':     return 'prefrontal_cortex';
    case 'CHAIN':     return 'corpus_callosum';
    case 'RERANKER':  return 'parietal_lobe';
    case 'GUARDRAIL': return 'amygdala';
    default:          return null;
  }
}

// -----------------------------------------------------------------------------
// Span → SD events (returns array; emitter loops + posts)
// -----------------------------------------------------------------------------
function translateSpan(span, resourceAttrs) {
  const events = [];
  const attrs = attrsToObj(span.attributes);
  const kind = spanKindOf(attrs);
  const traceId = bytesToHex(span.traceId);
  const sessionId = traceId ? `otel-${traceId.slice(0, 16)}` : `otel-${randomUUID().slice(0, 8)}`;
  const isRoot = !span.parentSpanId || (typeof span.parentSpanId === 'string' ? !span.parentSpanId.length : !span.parentSpanId.length);

  const model = modelOf(attrs);
  const client = clientOf(attrs, resourceAttrs);

  // 1) Root span → session_start (one per trace)
  if (isRoot) {
    events.push(buildEvent('session_start', sessionId, {
      client,
      model: model || 'unknown',
      span_name: span.name,
      adapter_kind: kind || 'ROOT',
    }));
  }

  // 2) Per-kind primary event(s)
  const toolName = attrs['tool.name'] || attrs['gen_ai.tool.name'] || null;
  switch (kind) {
    case 'LLM': {
      events.push(buildEvent('model_thinking', sessionId, {
        model: model || 'unknown',
        region_hint: 'frontal_lobe',
      }));
      // Per-chunk streaming events recorded as span.events
      for (const ev of span.events || []) {
        const evName = ev.name || '';
        if (
          evName.startsWith('gen_ai.choice') ||
          evName.startsWith('gen_ai.content') ||
          evName.includes('streaming')
        ) {
          const evAttrs = attrsToObj(ev.attributes);
          events.push(buildEvent('response_streaming', sessionId, {
            tokens: evAttrs['gen_ai.usage.output_tokens'] || evAttrs['llm.token_count.completion'] || 1,
            delta: (evAttrs['gen_ai.completion'] || evAttrs['gen_ai.choice.message.content'] || '').toString().slice(0, 80),
            region_hint: 'broca_area',
          }));
        }
      }
      const tk = tokensOf(attrs);
      events.push(buildEvent('response_complete', sessionId, {
        model: model || 'unknown',
        total_tokens: tk.total,
        prompt_tokens: tk.prompt,
        completion_tokens: tk.completion,
        stop_reason: attrs['gen_ai.response.finish_reasons']
                  || attrs['llm.invocation_parameters.stop'],
        region_hint: 'broca_area',
      }));
      break;
    }
    case 'TOOL': {
      const region = regionForKind('TOOL', toolName);
      events.push(buildEvent('tool_call', sessionId, {
        tool_name: toolName || span.name,
        tool_call_id: attrs['tool.call_id'] || null,
        region_hint: region,
      }));
      events.push(buildEvent('tool_result', sessionId, {
        tool_name: toolName || span.name,
        tool_call_id: attrs['tool.call_id'] || null,
        ok: !(span.status && span.status.code === 2),
        region_hint: 'cerebellum',
      }));
      break;
    }
    case 'RETRIEVER': {
      events.push(buildEvent('memory_recall', sessionId, {
        score: attrs['retrieval.score'] || null,
        memory_id: attrs['retrieval.document.id'] || null,
        region_hint: 'hippocampus',
      }));
      break;
    }
    case 'EMBEDDING': {
      events.push(buildEvent('model_thinking', sessionId, {
        model: model || attrs['embedding.model_name'] || 'unknown',
        region_hint: 'frontal_lobe',
      }));
      break;
    }
    case 'AGENT': {
      if (!isRoot) {
        events.push(buildEvent('subagent_spawn', sessionId, {
          subagent_id: span.spanId ? bytesToHex(span.spanId).slice(0, 12) : 'unknown',
          purpose: span.name,
          region_hint: 'prefrontal_cortex',
        }));
        events.push(buildEvent('subagent_complete', sessionId, {
          subagent_id: span.spanId ? bytesToHex(span.spanId).slice(0, 12) : 'unknown',
          ok: !(span.status && span.status.code === 2),
          region_hint: 'prefrontal_cortex',
        }));
      }
      break;
    }
    default:
      // Unknown / generic span — only forward if it has GenAI signals already handled above,
      // otherwise we silently skip to avoid noise.
      break;
  }

  // 3) Status=ERROR → error event
  if (span.status && span.status.code === 2) {
    events.push(buildEvent('error', sessionId, {
      where: span.name || 'otel-span',
      error_message: (span.status.message || 'span ended in error').slice(0, 200),
      region_hint: 'amygdala',
    }));
  }

  return events;
}

// -----------------------------------------------------------------------------
// Decode + dispatch
// -----------------------------------------------------------------------------
function decodeBody(contentType, buf) {
  const ct = (contentType || '').toLowerCase();
  if (ct.includes('json')) {
    return JSON.parse(buf.toString('utf8'));
  }
  if (ct.includes('protobuf')) {
    return ExportRequest.toObject(ExportRequest.decode(buf), {
      defaults: false, longs: Number, bytes: Buffer, arrays: true,
    });
  }
  // Default to JSON if unspecified
  return JSON.parse(buf.toString('utf8'));
}

async function handleTraces(req, res) {
  const chunks = [];
  let total = 0;
  for await (const c of req) {
    chunks.push(c);
    total += c.length;
    if (total > 16 * 1024 * 1024) { // 16 MB cap
      res.statusCode = 413;
      res.end('payload too large');
      return;
    }
  }
  const buf = Buffer.concat(chunks);
  let parsed;
  try {
    parsed = decodeBody(req.headers['content-type'], buf);
  } catch (e) {
    log('decode failed:', e.message);
    res.statusCode = 400;
    res.end('decode error');
    return;
  }

  let spanCount = 0;
  let emittedCount = 0;
  for (const rs of parsed.resourceSpans || parsed.resource_spans || []) {
    const resAttrs = attrsToObj(rs.resource?.attributes);
    for (const ss of rs.scopeSpans || rs.scope_spans || []) {
      for (const span of ss.spans || []) {
        spanCount++;
        // Normalize JSON keys to camelCase if they came in snake_case
        if (span.parent_span_id !== undefined && span.parentSpanId === undefined) {
          span.parentSpanId = span.parent_span_id;
        }
        if (span.trace_id !== undefined && span.traceId === undefined) {
          span.traceId = span.trace_id;
        }
        if (span.span_id !== undefined && span.spanId === undefined) {
          span.spanId = span.span_id;
        }
        const events = translateSpan(span, resAttrs);
        for (const evt of events) {
          emittedCount++;
          postEvent(evt);
        }
      }
    }
  }

  // Always succeed back to the OTel exporter so we don't trigger retries.
  // Reply with the OTLP success protobuf or empty JSON depending on request.
  res.statusCode = 200;
  const acceptsProto = (req.headers['content-type'] || '').toLowerCase().includes('protobuf');
  if (acceptsProto) {
    res.setHeader('content-type', 'application/x-protobuf');
    res.end(Buffer.from(ExportResponse.encode(ExportResponse.create({})).finish()));
  } else {
    res.setHeader('content-type', 'application/json');
    res.end('{}');
  }
  log(`spans=${spanCount} → events=${emittedCount}`);
}

// -----------------------------------------------------------------------------
// HTTP server
// -----------------------------------------------------------------------------
const server = http.createServer(async (req, res) => {
  if (req.method === 'POST' && (req.url === '/v1/traces' || req.url.startsWith('/v1/traces?'))) {
    return handleTraces(req, res);
  }
  if (req.method === 'GET' && req.url === '/healthz') {
    res.statusCode = 200; res.end('ok'); return;
  }
  res.statusCode = 404;
  res.end('not found');
});

server.listen(LISTEN_PORT, LISTEN_HOST, () => {
  log(`OTLP/HTTP receiver listening on http://${LISTEN_HOST}:${LISTEN_PORT}`);
  log(`forwarding to SD Core at ${TARGET.label}${SD_API_TOKEN ? ' (auth: bearer)' : ''}`);
});

for (const sig of ['SIGINT', 'SIGTERM']) {
  process.on(sig, () => { log('shutting down'); server.close(() => process.exit(0)); });
}
