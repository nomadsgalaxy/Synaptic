/**
 * Synaptic TypeScript SDK.
 *
 *   import { connect } from '@synaptic/sdk';
 *
 *   const client = connect({ adapterId: 'my-agent', model: 'claude-opus-4-7' });
 *   await client.emit('tool_call', { tool_name: 'bash' }, { regionHint: 'motor_cortex' });
 *   await client.close();
 *
 * The client never blocks: every emit returns a promise that resolves once
 * the POST completes or times out. If SD Core is offline, the event is
 * silently dropped.
 */
export { Client, connect } from './client.js';
export type { ConnectOptions, EmitOptions } from './client.js';
export {
  EVENT_TYPES,
  validateType,
  SchemaError,
} from './schema.js';
export type { EventType, Envelope } from './schema.js';
