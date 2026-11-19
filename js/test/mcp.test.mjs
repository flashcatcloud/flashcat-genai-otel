// MCP client instrumentation (contract §4.3): Client.callTool emits a tools/call CLIENT span, injects traceparent into _meta, and records isError as tool_error.
// Verify CommonJS and the ESM fallback provided by register.
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { runScript, startCollector } from './helpers.mjs';

let c;
before(async () => {
  c = await startCollector();
});
after(async () => {
  await c.close();
});

function env(extra = {}) {
  return { FLASHCAT_ENDPOINT: c.url + '/v1/traces', FLASHCAT_API_KEY: 'k:s', ...extra };
}

const APP = (imports) => `
${imports}
const server = new McpServer({ name: 'orders', version: '1.0.0' });
server.tool('lookup_order', { phone: z.string() }, async ({ phone }, extra) => ({
  content: [{ type: 'text', text: JSON.stringify({ phone, status: 'shipped', traceparent: extra?._meta?.traceparent ?? null }) }],
}));
server.tool('broken', { id: z.string() }, async () => { throw new Error('no such order'); });
const [ct, st] = InMemoryTransport.createLinkedPair();
await server.connect(st);
const client = new Client({ name: 'kefu', version: '1.0.0' });
await client.connect(ct);
const params = { name: 'lookup_order', arguments: { phone: '13812345678' } };
const ok = await otel.agent('kefu', () => client.callTool(params))();
const bad = await client.callTool({ name: 'broken', arguments: { id: 'x' } });
await otel.flush();
console.log('RESULT:' + JSON.stringify({ text: ok.content[0].text, bad: bad.isError, paramsUntouched: !('_meta' in params), d: otel.diagnostics() }));
`;

function check(result, run) {
  const spans = c.spans().filter((s) => s.resource['test.run'] === run);
  const mcp = result.d.instrumentations.find((i) => i.lib === 'mcp');
  assert.deepEqual({ source: mcp.source, status: mcp.status }, { source: 'flashcat', status: 'installed' });
  const call = spans.find((s) => s.name === 'tools/call lookup_order');
  const agent = spans.find((s) => s.attrs['gen_ai.operation.name'] === 'invoke_agent');
  assert.ok(call, spans.map((s) => s.name).join(','));
  assert.equal(call.kind, 3, 'CLIENT');
  assert.equal(call.scope, 'flashcat.instrumentation.mcp');
  assert.equal(call.parent, agent.spanId);
  assert.equal(agent.attrs['flashcat.genai.root'], true);
  assert.equal(call.attrs['flashcat.genai.root'], false);
  assert.equal(call.attrs['mcp.method.name'], 'tools/call');
  assert.equal(call.attrs['gen_ai.operation.name'], 'execute_tool');
  assert.equal(call.attrs['gen_ai.tool.name'], 'lookup_order');
  assert.ok(call.attrs['gen_ai.tool.call.arguments'].includes('[REDACTED:phone]'), 'Tool arguments are masked');
  assert.ok(call.attrs['gen_ai.tool.call.result'].includes('shipped'));
  const tp = JSON.parse(result.text).traceparent;
  assert.ok(tp && tp.includes(call.traceId) && tp.includes(call.spanId), `Server received traceparent: ${tp}`);
  assert.equal(result.paramsUntouched, true, 'Caller-supplied params stay intact');
  assert.equal(result.bad, true);
  const bad = spans.find((s) => s.name === 'tools/call broken');
  assert.equal(bad.attrs['flashcat.genai.root'], true);
  assert.equal(bad.attrs['error.type'], 'tool_error');
  assert.equal(bad.status, 2, 'ERROR');
}

test('MCP client with CommonJS code initialization', async () => {
  const run = 'mcp-cjs';
  const code = `
const otel = require('@flashcat/otel');
otel.init({ serviceName: 'x', metrics: false, captureContent: 'span', resourceAttributes: { 'test.run': '${run}' } });
(async () => {
${APP(`const { McpServer } = require('@modelcontextprotocol/sdk/server/mcp.js');
const { Client } = require('@modelcontextprotocol/sdk/client/index.js');
const { InMemoryTransport } = require('@modelcontextprotocol/sdk/inMemory.js');
const { z } = require('zod');`)}
})();
`;
  const { result } = await runScript('mcp', code, { ext: 'cjs', env: env() });
  check(result, run);
});

test('MCP client with ESM and node --import @flashcat/otel/register', async () => {
  const run = 'mcp-esm';
  const code = APP(`import * as otel from '@flashcat/otel';
import { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js';
import { Client } from '@modelcontextprotocol/sdk/client/index.js';
import { InMemoryTransport } from '@modelcontextprotocol/sdk/inMemory.js';
import { z } from 'zod';`);
  const { result } = await runScript('mcp', code, {
    nodeArgs: ['--import', '@flashcat/otel/register'],
    env: env({ OTEL_SERVICE_NAME: 'x', OTEL_METRICS_EXPORTER: 'none', FLASHCAT_CAPTURE_CONTENT: 'span', OTEL_RESOURCE_ATTRIBUTES: `test.run=${run}` }),
  });
  check(result, run);
});

test('MCP client with ESM code initialization after a static import', async () => {
  const run = 'mcp-esm-init';
  const code = `import * as otel from '@flashcat/otel';
import { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js';
import { Client } from '@modelcontextprotocol/sdk/client/index.js';
import { InMemoryTransport } from '@modelcontextprotocol/sdk/inMemory.js';
import { z } from 'zod';
otel.init({ serviceName: 'x', metrics: false, captureContent: 'span', resourceAttributes: { 'test.run': '${run}' } });
${APP('')}`;
  const { result } = await runScript('mcp', code, { env: env() });
  check(result, run);
});
