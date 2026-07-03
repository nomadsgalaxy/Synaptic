// Mirrors docs/event-schema.md v1.0. Keep in sync if the schema changes.
export const EVENT_TYPES = [
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
] as const;

export type EventType = (typeof EVENT_TYPES)[number];

const EVENT_TYPE_SET = new Set<string>(EVENT_TYPES);

export class SchemaError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'SchemaError';
  }
}

export function validateType(eventType: string): asserts eventType is EventType {
  if (!EVENT_TYPE_SET.has(eventType)) {
    throw new SchemaError(
      `Unknown event type "${eventType}". Valid: ${EVENT_TYPES.join(', ')}`
    );
  }
}

export interface Envelope {
  schema_version: '1.0';
  type: EventType;
  timestamp: string;
  adapter_id: string;
  session_id: string;
  payload: Record<string, unknown>;
}
