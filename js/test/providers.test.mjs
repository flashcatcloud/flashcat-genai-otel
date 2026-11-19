// OTel JS provider scenarios (contract §2.4). Scenario B cannot append a processor, so verify the fallback, diagnostics, and spanProcessor() with an existing NodeSDK.
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { mkdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { appDir, runScript, startCollector, startFakeOpenAI } from './helpers.mjs';

let openai;
before(async () => {
  openai = await startFakeOpenAI();
});
after(async () => {
  await openai.close();
});

const SDK_ENV = { OTEL_METRICS_EXPORTER: 'none', OTEL_LOGS_EXPORTER: 'none' };
const B_PROBLEM =
  "进程里已有 TracerProvider，OTel JS 不支持事后追加处理器。请把 `otel.spanProcessor()` 加进 NodeSDK 的 `spanProcessors`，并用 `init({ exporter: 'reuse' })`";

test('Scenario B keeps instrumentation, falls back to none, and suggests spanProcessor() when an existing NodeSDK does not export to Flashcat', async () => {
  const c = await startCollector();
  try {
    const code = `
import { NodeSDK } from '@opentelemetry/sdk-node';
import { SimpleSpanProcessor, InMemorySpanExporter } from '@opentelemetry/sdk-trace';
import * as otel from '@flashcat/otel';
const mem = new InMemorySpanExporter();
new NodeSDK({ serviceName: 'order-svc', spanProcessors: [new SimpleSpanProcessor({ exporter: mem })] }).start();
const d = otel.init({ serviceName: 'order-svc' });
otel.agent('a', () => 1)();
await otel.flush();
console.log('RESULT:' + JSON.stringify({ d, mem: mem.getFinishedSpans().map((s) => s.name) }));
`;
    const { result } = await runScript('main', code, {
      env: {
        ...SDK_ENV,
        FLASHCAT_ENDPOINT: c.url + '/v1/traces',
        FLASHCAT_API_KEY: 'k:s',
        OTEL_EXPORTER_OTLP_TRACES_ENDPOINT: 'http://other-vendor.example.com:4318/v1/traces',
      },
    });
    const d = result.d;
    assert.equal(d.provider.situation, 'B');
    assert.equal(d.provider.exporter_mode, 'none');
    assert.ok(d.problems.includes(B_PROBLEM), d.problems.join('\n'));
    assert.equal(d.enabled, false);
    assert.equal(d.service_name, 'order-svc');
    assert.equal(d.instrumentations.find((i) => i.lib === 'openai').status, 'installed', 'Instrumentation is still installed');
    assert.deepEqual(result.mem, ['invoke_agent a'], 'The application SDK received the semantic span');
    assert.equal(c.requests.length, 0, 'No requests were sent to Flashcat');
  } finally {
    await c.close();
  }
});

// Follow the README pattern. readme.test.mjs runs the actual snippet verbatim.
export const README_INSTRUMENTATION = `// instrumentation.mjs
import { register } from 'node:module';
import { NodeSDK } from '@opentelemetry/sdk-node';
import * as otel from '@flashcat/otel';

register('@opentelemetry/instrumentation/hook.mjs', import.meta.resolve('@flashcat/otel'));   // Required for ESM applications.

const sdk = new NodeSDK({
  serviceName: 'kefu-agent',
  spanProcessors: [otel.spanProcessor()],
});
sdk.start();
otel.init({ exporter: 'reuse' });
`;

test('README setup with otel.spanProcessor() in NodeSDK, reuse mode, and node --import ./instrumentation.mjs app.mjs', async () => {
  const c = await startCollector();
  try {
    const dir = appDir('main');
    const instr = join(dir, 'scripts', `instrumentation-${Date.now()}.mjs`);
    mkdirSync(join(dir, 'scripts'), { recursive: true });
    writeFileSync(instr, README_INSTRUMENTATION);
    const code = `
import * as otel from '@flashcat/otel';
import OpenAI from 'openai';
const client = new OpenAI({ baseURL: process.env.FAKE_OPENAI, apiKey: 'sk-test' });
const answer = otel.agent('kefu', async (q) => (await client.chat.completions.create({ model: 'gpt-4o', messages: [{ role: 'user', content: q }] })).choices[0].message.content);
await otel.session({ conversationId: 'c-9', userId: 'u-9' }, () => answer('x'));
otel.trace('job', () => {}, { 'db.password': 'hunter2' });
await otel.flush();
console.log('RESULT:' + JSON.stringify(otel.diagnostics()));
`;
    const { result: d } = await runScript('main', code, {
      nodeArgs: ['--import', instr],
      env: { ...SDK_ENV, FLASHCAT_ENDPOINT: c.url + '/v1/traces', FLASHCAT_API_KEY: 'fck_test:s3cret', FAKE_OPENAI: openai.url },
    });
    assert.equal(d.provider.situation, 'B');
    assert.equal(d.provider.exporter_mode, 'reuse');
    assert.deepEqual(d.problems, []);
    assert.equal(d.service_name, 'kefu-agent');
    assert.equal(d.instrumentations.find((i) => i.lib === 'openai').status, 'installed');
    assert.ok(d.export.ok >= 3, JSON.stringify(d.export));
    const spans = c.spans();
    const chat = spans.find((s) => s.attrs['gen_ai.operation.name'] === 'chat');
    const ag = spans.find((s) => s.attrs['gen_ai.operation.name'] === 'invoke_agent');
    const job = spans.find((s) => s.name === 'job');
    assert.ok(chat, 'OpenAI is patched in the ESM application');
    assert.equal(chat.parent, ag.spanId);
    assert.equal(chat.attrs['gen_ai.conversation.id'], 'c-9', 'spanProcessor() adds session attributes');
    assert.equal(chat.resource['service.name'], 'kefu-agent');
    assert.equal(job.attrs['db.password'], '[REDACTED]', 'spanProcessor() applies masking');
    assert.equal(c.requests[0].headers.authorization, 'Basic ' + Buffer.from('fck_test:s3cret').toString('base64'));
  } finally {
    await c.close();
  }
});

test('The same setup works in CommonJS with --require instrumentation.cjs', async () => {
  const c = await startCollector();
  try {
    const dir = appDir('main');
    const instr = join(dir, 'scripts', `instrumentation-${Date.now()}.cjs`);
    writeFileSync(instr, `
const { NodeSDK } = require('@opentelemetry/sdk-node');
const otel = require('@flashcat/otel');
new NodeSDK({ serviceName: 'kefu-agent', spanProcessors: [otel.spanProcessor()] }).start();
otel.init({ exporter: 'reuse' });
`);
    const code = `
const otel = require('@flashcat/otel');
const { OpenAI } = require('openai');
(async () => {
  const client = new OpenAI({ baseURL: process.env.FAKE_OPENAI, apiKey: 'sk-test' });
  const answer = otel.agent('kefu', async (q) => (await client.chat.completions.create({ model: 'gpt-4o', messages: [{ role: 'user', content: q }] })).choices[0].message.content);
  await otel.session({ conversationId: 'c-9' }, () => answer('x'));
  await otel.flush();
  console.log('RESULT:' + JSON.stringify(otel.diagnostics()));
})();
`;
    const { result: d } = await runScript('main', code, {
      ext: 'cjs',
      nodeArgs: ['--require', instr],
      env: { ...SDK_ENV, FLASHCAT_ENDPOINT: c.url + '/v1/traces', FLASHCAT_API_KEY: 'k:s', FAKE_OPENAI: openai.url },
    });
    assert.deepEqual(d.problems, []);
    const chat = c.spans().find((s) => s.attrs['gen_ai.operation.name'] === 'chat');
    assert.equal(chat.attrs['gen_ai.conversation.id'], 'c-9');
  } finally {
    await c.close();
  }
});

test('Without an endpoint, spanProcessor() only processes sessions and reports the reason in problems without throwing', async () => {
  const code = `
import { NodeSDK } from '@opentelemetry/sdk-node';
import * as otel from '@flashcat/otel';
const p = otel.spanProcessor();
new NodeSDK({ serviceName: 'x', spanProcessors: [p] }).start();
const d = otel.init({ exporter: 'reuse' });
console.log('RESULT:' + JSON.stringify(d));
`;
  const { result } = await runScript('main', code, { env: SDK_ENV });
  assert.ok(result.problems.some((p) => p.includes('spanProcessor()') && p.includes('缺少 endpoint')), result.problems.join('\n'));
});

test('Scenario C reuses an existing provider that already exports to Flashcat', async () => {
  const c = await startCollector();
  try {
    const code = `
import { NodeSDK } from '@opentelemetry/sdk-node';
import * as otel from '@flashcat/otel';
new NodeSDK({ serviceName: 'order-svc' }).start();
const d = otel.init({ serviceName: 'order-svc', metrics: false });
console.log('RESULT:' + JSON.stringify(d));
`;
    const { result } = await runScript('main', code, {
      env: {
        ...SDK_ENV,
        FLASHCAT_ENDPOINT: c.url + '/v1/traces',
        FLASHCAT_API_KEY: 'k:s',
        OTEL_EXPORTER_OTLP_TRACES_ENDPOINT: c.url + '/v1/traces',
      },
    });
    assert.equal(result.provider.situation, 'C');
    assert.equal(result.provider.exporter_mode, 'reuse');
    assert.equal(result.enabled, true);
    assert.ok(result.notes.some((n) => n.includes('未新增 exporter')));
  } finally {
    await c.close();
  }
});

test('shutdown closes only SDK-owned providers', async () => {
  const code = `
import { NodeSDK } from '@opentelemetry/sdk-node';
import { SimpleSpanProcessor, InMemorySpanExporter } from '@opentelemetry/sdk-trace';
import * as otel from '@flashcat/otel';
const mem = new InMemorySpanExporter();
new NodeSDK({ serviceName: 'x', spanProcessors: [new SimpleSpanProcessor({ exporter: mem })] }).start();
otel.init({ serviceName: 'x', exporter: 'none' });
await otel.shutdown();
otel.trace('after-shutdown', () => {});
await otel.flush();
console.log('RESULT:' + JSON.stringify(mem.getFinishedSpans().map((s) => s.name)));
`;
  const { result } = await runScript('main', code, { env: SDK_ENV });
  assert.deepEqual(result, ['after-shutdown'], 'The application provider still works');
});
