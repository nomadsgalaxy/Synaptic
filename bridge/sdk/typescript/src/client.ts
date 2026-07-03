import { type Envelope, type EventType, validateType } from './schema.js';

function nowIso(): string {
  return new Date().toISOString();
}

function shortId(): string {
  // 8 hex chars; collisions across sessions are not load-bearing.
  return Math.random().toString(16).slice(2, 10).padStart(8, '0');
}

export interface ConnectOptions {
  adapterId: string;
  model?: string;
  /**
   * Full SD Core URL (e.g. "https://cognito.example.com"). Preferred for
   * remote operation. When set, takes precedence over host+port.
   */
  url?: string;
  host?: string;
  port?: number;
  /**
   * Bearer token sent as `Authorization: Bearer <token>` on every request.
   * Required when SD Core was started with SD_API_TOKEN set.
   */
  apiToken?: string;
  sessionId?: string;
  /** Auto-emit session_start on connect. Default true. */
  autoSessionStart?: boolean;
}

export interface EmitOptions {
  regionHint?: string;
  sessionId?: string;
}

export class Client {
  readonly sessionId: string;
  private _adapterId: string;
  private readonly model?: string;
  private readonly endpoint: string;
  private readonly apiToken?: string;
  private closed = false;
  // Track in-flight emits so close() can flush gracefully.
  private inflight = new Set<Promise<void>>();

  constructor(opts: ConnectOptions) {
    this._adapterId = opts.adapterId;
    this.model = opts.model;
    if (opts.url) {
      this.endpoint = opts.url.replace(/\/$/, '') + '/event';
    } else {
      const host = opts.host ?? '127.0.0.1';
      const port = opts.port ?? 9911;
      this.endpoint = `http://${host}:${port}/event`;
    }
    this.apiToken = opts.apiToken;
    this.sessionId = opts.sessionId ?? `sdk-${shortId()}`;
  }

  get adapterId(): string {
    return this._adapterId;
  }

  /** Change the adapter_id used on subsequent emits. */
  registerAdapterId(adapterId: string): void {
    this._adapterId = adapterId;
  }

  /**
   * Emit an event. Fire-and-forget by default — returns a promise that
   * resolves when the POST completes (or fails silently). Awaiting is
   * optional; the call never throws on network errors.
   */
  emitEvent(
    eventType: string,
    payload?: Record<string, unknown>,
    opts: EmitOptions = {}
  ): Promise<void> {
    if (this.closed) return Promise.resolve();
    validateType(eventType);
    const envelope = this.buildEnvelope(eventType as EventType, payload, opts);
    const p = this.post(envelope);
    this.inflight.add(p);
    p.finally(() => this.inflight.delete(p));
    return p;
  }

  /** Short alias matching the Python API. */
  emit(
    eventType: string,
    payload?: Record<string, unknown>,
    opts: EmitOptions = {}
  ): Promise<void> {
    return this.emitEvent(eventType, payload, opts);
  }

  /** Emit session_end and wait for in-flight POSTs to settle. */
  async close(reason: string = 'client_close'): Promise<void> {
    if (this.closed) return;
    this.closed = true;
    // Re-open the gate just for this final emit.
    this.closed = false;
    await this.emitEvent('session_end', { reason });
    this.closed = true;
    await Promise.allSettled([...this.inflight]);
  }

  // ------------------------------------------------------------------
  // Internals
  // ------------------------------------------------------------------
  private buildEnvelope(
    eventType: EventType,
    payload: Record<string, unknown> | undefined,
    opts: EmitOptions
  ): Envelope {
    const merged: Record<string, unknown> = { ...(payload ?? {}) };
    if (opts.regionHint && merged.region_hint == null) {
      merged.region_hint = opts.regionHint;
    }
    if (eventType === 'session_start' && this.model && merged.model == null) {
      merged.model = this.model;
    }
    return {
      schema_version: '1.0',
      type: eventType,
      timestamp: nowIso(),
      adapter_id: this._adapterId,
      session_id: opts.sessionId ?? this.sessionId,
      payload: merged,
    };
  }

  private async post(envelope: Envelope): Promise<void> {
    const controller = new AbortController();
    // Slightly longer than localhost-only since remote (CF Tunnel) adds
    // tunnel + TLS handshake overhead.
    const timer = setTimeout(() => controller.abort(), 2500);
    const headers: Record<string, string> = { 'content-type': 'application/json' };
    if (this.apiToken) headers['authorization'] = `Bearer ${this.apiToken}`;
    try {
      await fetch(this.endpoint, {
        method: 'POST',
        headers,
        body: JSON.stringify(envelope),
        signal: controller.signal,
        // Avoid reuse pitfalls in some Node fetch impls
        keepalive: false,
      });
    } catch {
      // SD Core offline / aborted — drop event silently.
    } finally {
      clearTimeout(timer);
    }
  }
}

/**
 * Connect to SD Core and (by default) emit ``session_start``.
 *
 * Environment variables override defaults:
 *   - SD_CORE_URL  (e.g. https://cognito.example.com — preferred for remote)
 *   - SD_CORE_HOST (default 127.0.0.1)
 *   - SD_CORE_PORT (default 9911)
 *   - SD_API_TOKEN — Bearer token sent on every request when set.
 */
export function connect(opts: ConnectOptions): Client {
  const env = typeof process !== 'undefined' ? process.env : undefined;
  const url   = opts.url   ?? env?.SD_CORE_URL   ?? undefined;
  const host  = opts.host  ?? env?.SD_CORE_HOST  ?? '127.0.0.1';
  const portRaw =
    opts.port ??
    (env?.SD_CORE_PORT ? Number(env.SD_CORE_PORT) : undefined) ??
    9911;
  const apiToken = opts.apiToken ?? env?.SD_API_TOKEN ?? undefined;
  const client = new Client({ ...opts, url, host, port: portRaw, apiToken });
  if (opts.autoSessionStart !== false) {
    void client.emitEvent('session_start', {
      client: opts.adapterId,
      model: opts.model ?? 'unknown',
    });
  }
  return client;
}
