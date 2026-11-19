// Bundling (not_patched in contract §8): an unpatched library in use changes status from installed to not_patched.
// problems suggests how to externalize it in Next.js, webpack, and esbuild. Log one WARN and avoid false reports for normal imports.
// Simulate bundling by copying node_modules/openai outside node_modules and loading the vendored copy by relative path.
// Runtime hooks match package names and miss this copy just as they would miss bundled code.
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { runScript, startCollector, startFakeOpenAI } from './helpers.mjs';

let openai;
before(async () => {
  openai = await startFakeOpenAI();
});
after(async () => {
  await openai.close();
});

const inst = (d, lib) => d.instrumentations.find((i) => i.lib === lib);

// Copy openai under scripts/ outside node_modules and return the relative require path.
const VENDOR = `
const { cpSync, existsSync } = require('node:fs');
const { join } = require('node:path');
const vendor = join(__dirname, 'vendor-openai-' + process.pid);
if (!existsSync(vendor)) cpSync(join(__dirname, '..', 'node_modules', 'openai'), vendor, { recursive: true });
`;

// Route OpenAI requests through node:http to exercise http.client.request.start, as node-fetch does by default in openai 4.x.
const HTTP_FETCH = `
const http = require('node:http');
const httpFetch = (url, init = {}) =>
  new Promise((resolve, reject) => {
    // Match the Headers.raw() shape from node-fetch v2 (the Node.js default in openai 4.x): each header value is an array.
    const headers = {};
    for (const [k, v] of new Headers(init.headers ?? {}).entries()) headers[k] = [v];
    const req = http.request(String(url), { method: init.method ?? 'GET', headers }, (res) => {
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => resolve(new Response(Buffer.concat(chunks), { status: res.statusCode, headers: res.headers })));
    });
    req.on('error', reject);
    req.end(init.body ?? undefined);
  });
`;

const APP = (load, clientOpts = '') => `
const otel = require('@flashcat/otel');
${VENDOR}
${HTTP_FETCH}
const { OpenAI } = ${load};
(async () => {
  const client = new OpenAI({ baseURL: process.env.FAKE_OPENAI, apiKey: 'sk-test'${clientOpts} });
  const before = otel.diagnostics();
  const ask = otel.agent('kefu', async () => (await client.chat.completions.create({ model: 'gpt-4o', messages: [{ role: 'user', content: 'x' }] })).choices[0].message.content);
  const a1 = await ask();
  const a2 = await ask();
  await client.models.list().catch(() => {});
  await otel.flush();
  console.log('RESULT:' + JSON.stringify({ a1, a2, before: inst(before), after: inst(otel.diagnostics()), d: otel.diagnostics() }));
  function inst(d) { return d.instrumentations.find((i) => i.lib === 'openai'); }
})();
`;

async function run(load, clientOpts, nodeArgs = []) {
  const c = await startCollector();
  try {
    const r = await runScript('main', APP(load, clientOpts), {
      ext: 'cjs',
      nodeArgs,
      env: { FLASHCAT_ENDPOINT: c.url + '/v1/traces', FLASHCAT_API_KEY: 'k:s', FAKE_OPENAI: openai.url, OTEL_SERVICE_NAME: 'x', OTEL_METRICS_EXPORTER: 'none' },
    });
    return { ...r, spans: c.spans() };
  } finally {
    await c.close();
  }
}

for (const [name, clientOpts] of [
  ['global fetch (undici:request:create)', ''],
  ['node:http (http.client.request.start)', ', fetch: httpFetch'],
]) {
  test(`Bundled OpenAI (${name}) becomes not_patched, reports how to externalize it, and warns once`, async () => {
    const { result, stderr, spans } = await run(`require(vendor)`, clientOpts, ['--import', '@flashcat/otel/register']);
    assert.equal(result.a1, '您的订单已发货', 'Application output is unaffected');
    assert.equal(result.before.status, 'installed', 'The missing patch is detected only after a request');
    assert.equal(result.after.status, 'not_patched');
    assert.match(result.after.hint, /serverExternalPackages/);
    const problems = result.d.problems.filter((p) => p.includes('not_patched'));
    assert.equal(problems.length, 1, result.d.problems.join('\n'));
    const p = problems[0];
    assert.match(p, /^openai 正在发模型请求（OpenAI\/JS 6\.49\.0 → \/v1\/chat\/completions），但插桩补丁没有生效/);
    assert.match(p, /打包工具把 openai 打进了 bundle/);
    assert.match(p, /experimental: \{ serverComponentsExternalPackages: \['@flashcat\/otel', 'openai'\] \}/, 'Next.js 13–14 configuration');
    assert.match(p, /顶层 serverExternalPackages: \['@flashcat\/otel', 'openai'\]/, 'Next.js 15 configuration');
    assert.match(p, /webpack：用 externals/);
    assert.match(p, /esbuild：--external:openai/);
    const warns = stderr.split('\n').filter((l) => l.includes('WARN') && l.includes('not_patched'));
    assert.equal(warns.length, 1, 'Expected one WARN for two calls: ' + stderr);
    assert.equal(spans.filter((s) => s.attrs['gen_ai.operation.name'] === 'chat').length, 0, 'No model spans were produced');
    assert.equal(spans.filter((s) => s.name === 'invoke_agent kefu').length, 2, 'Semantic spans are still produced');
  });
}

test('OpenAI loaded with require produces model spans and stays installed with global fetch and node:http', async () => {
  for (const clientOpts of ['', ', fetch: httpFetch']) {
    const { result, stderr, spans } = await run(`require('openai')`, clientOpts, ['--import', '@flashcat/otel/register']);
    assert.equal(result.after.status, 'installed', clientOpts);
    assert.ok(!result.d.problems.some((p) => p.includes('not_patched')), result.d.problems.join('\n'));
    assert.ok(!stderr.includes('not_patched'), stderr);
    assert.equal(spans.filter((s) => s.attrs['gen_ai.operation.name'] === 'chat').length, 2);
  }
});

test('ESM patched via --import and import-in-the-middle avoids false reports', async () => {
  const c = await startCollector();
  try {
    const code = `
import * as otel from '@flashcat/otel';
import OpenAI from 'openai';
const client = new OpenAI({ baseURL: process.env.FAKE_OPENAI, apiKey: 'sk-test' });
await client.chat.completions.create({ model: 'gpt-4o', messages: [{ role: 'user', content: 'x' }] });
for await (const _ of await client.chat.completions.create({ model: 'gpt-4o', messages: [{ role: 'user', content: 'x' }], stream: true }));
await otel.flush();
console.log('RESULT:' + JSON.stringify(otel.diagnostics()));
`;
    const { result: d, stderr } = await runScript('main', code, {
      nodeArgs: ['--import', '@flashcat/otel/register'],
      env: { FLASHCAT_ENDPOINT: c.url + '/v1/traces', FLASHCAT_API_KEY: 'k:s', FAKE_OPENAI: openai.url, OTEL_SERVICE_NAME: 'x', OTEL_METRICS_EXPORTER: 'none' },
    });
    assert.equal(inst(d, 'openai').status, 'installed');
    assert.ok(!stderr.includes('not_patched'), stderr);
    assert.equal(c.spans().filter((s) => s.attrs['gen_ai.operation.name'] === 'chat').length, 2);
  } finally {
    await c.close();
  }
});
