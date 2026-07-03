# @synaptic/sdk (TypeScript SDK)

Tiny TypeScript / JavaScript client for emitting Synaptic activity events. **Zero runtime dependencies**, works on Node ≥18 and modern browsers (uses built-in `fetch`).

## Install

For now, install directly from this folder:

```bash
cd bridge/sdk/typescript
npm install
npm run build
# Then in your project: npm link / file:../path
```

Once published:

```bash
npm install @synaptic/sdk
```

## Use

```ts
import { connect } from '@synaptic/sdk';

const client = connect({ adapterId: 'my-agent', model: 'claude-opus-4-7' });

await client.emit('prompt_received',
  { text: 'hi', char_count: 2 }, { regionHint: 'wernicke_area' });
await client.emit('tool_call',
  { tool_name: 'bash' }, { regionHint: 'motor_cortex' });
await client.emit('memory_added',
  { memory_id: 'm-1', text: 'User said hi' });

await client.close(); // emits session_end
```

## API

### `connect(opts) → Client`

```ts
interface ConnectOptions {
  adapterId: string;
  model?: string;
  url?: string;             // default: SD_CORE_URL — preferred for remote
  host?: string;            // default: SD_CORE_HOST or 127.0.0.1
  port?: number;            // default: SD_CORE_PORT or 9911
  apiToken?: string;        // default: SD_API_TOKEN (Bearer header)
  sessionId?: string;       // default: random "sdk-<8hex>"
  autoSessionStart?: boolean; // default: true
}
```

Returns a `Client`. By default, immediately emits `session_start` with `payload.client = adapterId` and `payload.model = model`.

Environment overrides:

- `SD_CORE_URL` — full URL (e.g. `https://cognito.example.com`). Preferred for remote operation; takes precedence over host+port.
- `SD_CORE_HOST` (default `127.0.0.1`)
- `SD_CORE_PORT` (default `9911`)
- `SD_API_TOKEN` — Bearer token sent on every request when set. Required when SD Core was started with `SD_API_TOKEN`.

For wiring the SDK to a remote SD Core, see [`docs/REMOTE.md`](../../../docs/REMOTE.md).

### `client.emit(type, payload?, opts?) → Promise<void>`

Validates `type` against the v1.0 enum (throws `SchemaError` synchronously if unknown). POSTs to SD Core with a 1.5 s timeout. The returned promise resolves when the POST completes or fails silently — you can `await` it or fire-and-forget.

```ts
interface EmitOptions {
  regionHint?: string;
  sessionId?: string;
}
```

`emitEvent` is the long-form alias.

### `client.registerAdapterId(adapterId) → void`

Change the `adapter_id` used on subsequent emits.

### `client.close(reason?) → Promise<void>`

Emits `session_end` and waits for in-flight POSTs to settle.

### `client.sessionId`, `client.adapterId`

Read-only access.

## Examples

- [`examples/hello-brain.ts`](./examples/hello-brain.ts) — bare-minimum 5-event script
- [`examples/wrap-function.ts`](./examples/wrap-function.ts) — `traceTool()` higher-order function

## Verifying

```bash
# 1. SD Core up?
curl http://localhost:9911/healthz

# 2. Build + run
cd bridge/sdk/typescript
npm run build
node --experimental-strip-types examples/hello-brain.ts
# or with tsx: npx tsx examples/hello-brain.ts

# 3. SD Core saw the events?
curl 'http://localhost:9911/events?adapter_id=hello-brain' | jq .
```

## Drop rules

- **Schema-invalid types** throw `SchemaError` synchronously — your `await emit(...)` rejects.
- **Network failure / SD Core offline / 1.5 s timeout** is swallowed silently — the client is a telemetry path; we never want to break the host program.

## Browser usage

`fetch` is built in. CORS doesn't apply for `localhost → localhost` if you set SD Core to bind on `0.0.0.0` and serve the page from another origin, but the default is `127.0.0.1`-only — adjust SD Core if you need cross-origin emission.

## License

Pending — Open Community License.
