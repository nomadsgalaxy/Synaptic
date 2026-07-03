// Pattern: wrap any async function so its calls light up the dashboard.
// Drop this in front of your tool handlers / agent steps.

import { connect, type Client } from '@synaptic/sdk';

function traceTool<T extends (...args: any[]) => Promise<any>>(
  client: Client,
  fn: T,
  options: { region?: string; name?: string } = {}
): T {
  const region = options.region ?? 'motor_cortex';
  const name = options.name ?? fn.name ?? 'anonymous';
  return (async (...args: any[]) => {
    const id = `tc-${Math.random().toString(16).slice(2, 10)}`;
    await client.emit('tool_call',
      { tool_name: name, tool_call_id: id },
      { regionHint: region });
    try {
      const out = await fn(...args);
      await client.emit('tool_result',
        { tool_name: name, tool_call_id: id, ok: true },
        { regionHint: 'cerebellum' });
      return out;
    } catch (e: any) {
      await client.emit('error',
        { where: name, error_message: String(e?.message ?? e).slice(0, 200) },
        { regionHint: 'amygdala' });
      throw e;
    }
  }) as T;
}

async function main() {
  const client = connect({ adapterId: 'wrap-example', model: 'claude-opus-4-7' });

  const lookup = traceTool(
    client,
    async (symbol: string) => {
      await new Promise(r => setTimeout(r, 100));
      return `price of ${symbol}: $42`;
    },
    { name: 'lookup', region: 'parietal_lobe' }
  );

  const webSearch = traceTool(
    client,
    async (query: string) => {
      await new Promise(r => setTimeout(r, 200));
      return `first result for: ${query}`;
    },
    { name: 'web_search', region: 'temporal_lobe_left' }
  );

  console.log(await lookup('AAPL'));
  console.log(await webSearch('synaptic disorder dashboard'));

  await client.close();
}

main().catch(console.error);
