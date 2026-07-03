#!/usr/bin/env node
const http = require('http');
const https = require('https');
const url = require('url');
const fs = require('fs');

// Read from stdin
let input = '';
process.stdin.on('data', chunk => { input += chunk; });
process.stdin.on('end', () => {
  try {
    const payload = JSON.parse(input);
    const eventName = process.argv[2];
    
    // Translate Gemini hook to SD event
    const sdEvent = translate(eventName, payload);
    if (sdEvent) {
      const events = Array.isArray(sdEvent) ? sdEvent : [sdEvent];
      events.forEach(postEvent);
    }
  } catch (e) {
    // Silent fail
  }
  // Gemini CLI expects a JSON response on stdout
  console.log(JSON.stringify({ decision: 'allow' }));
});

const SD_CORE_URL = process.env.SD_CORE_URL || 'http://localhost:9911';
const SD_API_TOKEN = process.env.SD_API_TOKEN || '';
const ADAPTER_ID = 'gemini-cli-hooks';

function translate(geminiEvent, payload) {
  const base = {
    schema_version: '1.0',
    timestamp: new Date().toISOString(),
    adapter_id: ADAPTER_ID,
    session_id: payload.session_id,
  };

  switch (geminiEvent) {
    case 'SessionStart':
      return {
        ...base,
        type: 'session_start',
        payload: { client: 'Gemini CLI', cwd: payload.cwd }
      };
    case 'BeforeTool':
      return [
        { ...base, type: 'model_thinking', payload: { region_hint: 'frontal_lobe' } },
        { ...base, type: 'tool_call', payload: { 
            tool_name: payload.tool_name, 
            arg_keys: payload.tool_input ? Object.keys(payload.tool_input) : [] 
          } 
        }
      ];
    case 'AfterTool':
      return {
        ...base,
        type: 'tool_result',
        payload: { tool_name: payload.tool_name, ok: true }
      };
    default:
      return null;
  }
}

function postEvent(event) {
  const body = JSON.stringify(event);
  const u = new url.URL(SD_CORE_URL);
  const transport = u.protocol === 'https:' ? https : http;
  
  const req = transport.request({
    hostname: u.hostname,
    port: u.port || (u.protocol === 'https:' ? 443 : 80),
    path: (u.pathname === '/' ? '' : u.pathname) + '/event',
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'Content-Length': Buffer.byteLength(body),
      'Authorization': `Bearer ${SD_API_TOKEN}`
    }
  });
  req.on('error', () => {});
  req.write(body);
  req.end();
}
