// Minimal Synaptic TypeScript SDK example.
//
// Run with SD Core listening on localhost:9911. Compile with `npm run build`
// from bridge/sdk/typescript/ first, or use ts-node / tsx.

import { connect } from '@synaptic/sdk';

async function main() {
  const client = connect({ adapterId: 'hello-brain', model: 'claude-opus-4-7' });
  console.log(`connected — session_id=${client.sessionId}`);

  await client.emit('prompt_received',
    { text: "what's the weather", char_count: 18 },
    { regionHint: 'wernicke_area' });

  await client.emit('tool_call',
    { tool_name: 'bash', tool_call_id: 'tc-1' },
    { regionHint: 'motor_cortex' });

  await client.emit('tool_result',
    { tool_name: 'bash', tool_call_id: 'tc-1', ok: true },
    { regionHint: 'cerebellum' });

  await client.emit('memory_added',
    { memory_id: 'm-7c2', text: 'User asked about the weather' },
    { regionHint: 'hippocampus' });

  await client.emit('response_complete',
    { total_tokens: 142, stop_reason: 'end_turn' },
    { regionHint: 'broca_area' });

  await client.close();
  console.log('done');
}

main().catch(console.error);
