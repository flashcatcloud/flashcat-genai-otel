// Scenario A end to end: the mock endpoint receives protobuf with Basic auth and gzip, resource attributes are set, and export.ok increases.
// Upstream OpenAI 6 instrumentation emits a chat span under the agent with session attributes. Check content capture in both ESM and CJS.
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { startCollector, startFakeOpenAI, runScript, basic } from './helpers.mjs';

let collector;
let openai;
before(async () => {
  openai = await startFakeOpenAI();
});
after(async () => {
  await openai.close();
});

const BODY = (content, question) => `
otel.init({ serviceName: 'kefu-agent', captureContent: ${content} });
const client = new OpenAI({ baseURL: process.env.FAKE_OPENAI, apiKey: 'sk-test' });

const lookupOrder = otel.tool('lookup_order', (orderId) => ({ order_id: orderId, status: 'shipped' }));
const answer = otel.agent('kefu', async (q) => {
  lookupOrder('A-1');
  const r = await client.chat.completions.create({ model: 'gpt-4o', messages: [{ role: 'user', content: q }] });
  return r.choices[0].message.content;
});

const out = await otel.session({ conversationId: 'c-123', userId: 'u-9', tags: { team: 'kf' } }, () => answer(${JSON.stringify(question)}));
await otel.flush();
console.log('RESULT:' + JSON.stringify({ out, diag: otel.diagnostics() }));
`;

// Call init before require('openai'), as in the Python test.
const CJS_APP = (content, question) => `
const otel = require('@flashcat/genai-otel');
(async () => {
${BODY(content, question).replace("otel.init(", "otel.init(").replace("const client =", "const { OpenAI } = require('openai');\nconst client =")}
})();
`;

function checkEndToEnd(c, result) {
  const { diag, out } = result;
  assert.equal(out, '您的订单已发货');
  assert.equal(diag.sdk.language, 'nodejs');
  assert.equal(diag.provider.situation, 'A');
  assert.equal(diag.provider.exporter_mode, 'add');
  assert.equal(diag.provider.metrics, 'add', 'The metrics endpoint is derived from the traces endpoint');
  const statuses = Object.fromEntries(diag.instrumentations.map((i) => [i.lib, i]));
  assert.equal(statuses.openai.status, 'installed');
  assert.equal(statuses.openai.source, 'upstream');
  assert.equal(statuses.openai.lib_version, '6.49.0');
  assert.ok(diag.export.ok >= 3, JSON.stringify(diag.export));

  const traceReqs = c.requests.filter((r) => r.path === '/v1/traces');
  assert.ok(traceReqs.length >= 1);
  assert.equal(traceReqs[0].headers.authorization, basic('fck_test:s3cret'));
  assert.equal(traceReqs[0].headers['content-type'], 'application/x-protobuf');
  assert.ok(traceReqs[0].gzip, 'The request body is gzipped');

  const spans = c.spans();
  const byOp = Object.fromEntries(spans.map((s) => [s.attrs['gen_ai.operation.name'], s]));
  const chat = byOp.chat;
  const agentSpan = byOp.invoke_agent;
  const toolSpan = byOp.execute_tool;
  assert.ok(chat && agentSpan && toolSpan, spans.map((s) => s.name).join(','));
  assert.equal(chat.resource['service.name'], 'kefu-agent');
  assert.equal(chat.resource['deployment.environment.name'], 'prod');
  assert.equal(chat.resource['telemetry.distro.name'], 'flashcat-genai-otel');
  assert.equal(chat.resource['telemetry.distro.version'], '0.1.0');
  assert.equal(chat.resource['telemetry.sdk.language'], 'nodejs');
  assert.equal(chat.attrs['gen_ai.conversation.id'], 'c-123');
  assert.equal(chat.attrs['user.id'], 'u-9');
  assert.equal(chat.attrs['flashcat.tag.team'], 'kf');
  assert.equal(chat.attrs['gen_ai.usage.input_tokens'], 120);
  assert.equal(chat.parent, agentSpan.spanId);
  assert.equal(toolSpan.parent, agentSpan.spanId);
  assert.equal(chat.traceId, agentSpan.traceId);
  assert.equal(agentSpan.name, 'invoke_agent kefu');
  assert.equal(agentSpan.scope, 'flashcat.genai');
  assert.equal(agentSpan.scopeVersion, '0.1.0');
  assert.equal(agentSpan.kind, 1, 'INTERNAL');
  assert.equal(agentSpan.attrs['gen_ai.agent.name'], 'kefu');
  assert.equal(agentSpan.attrs['gen_ai.conversation.id'], 'c-123');
  assert.equal(toolSpan.attrs['gen_ai.tool.name'], 'lookup_order');
  assert.equal(toolSpan.attrs['gen_ai.tool.type'], 'function');
  return { chat, agentSpan, toolSpan, spans };
}

const ENV = (c) => ({
  FLASHCAT_ENDPOINT: c.url + '/v1/traces',
  FLASHCAT_API_KEY: 'fck_test:s3cret',
  FAKE_OPENAI: openai.url,
});

test('Scenario A end to end with CommonJS code initialization and default content capture', async () => {
  const c = await startCollector();
  try {
    const { result } = await runScript('main', CJS_APP('false', '我的订单到哪了'), { ext: 'cjs', env: ENV(c) });
    const { chat, agentSpan, toolSpan } = checkEndToEnd(c, result);
    assert.equal(chat.attrs['gen_ai.input.messages'], undefined, 'Content capture is off by default');
    assert.equal(agentSpan.attrs['gen_ai.input.messages'], undefined);
    assert.equal(toolSpan.attrs['gen_ai.tool.call.arguments'], undefined);
    assert.ok(c.requests.some((r) => r.path === '/v1/metrics'), 'Flush also sends metrics to /v1/metrics');
  } finally {
    await c.close();
  }
});

test('Scenario A end to end with CommonJS, span content capture, and masking', async () => {
  const c = await startCollector();
  try {
    const { result } = await runScript('main', CJS_APP("'span'", '手机13812345678，邮箱a@b.com'), { ext: 'cjs', env: ENV(c) });
    const { chat, agentSpan, toolSpan } = checkEndToEnd(c, result);
    // instrumentation-openai 0.23 emits chat content as log events. The SDK copies them to span attributes and masks them as usual.
    const chatIn = JSON.parse(chat.attrs['gen_ai.input.messages']);
    assert.equal(chatIn.at(-1).role, 'user');
    assert.ok(chatIn.at(-1).parts[0].content.includes('[REDACTED:phone]'));
    assert.ok(!chat.attrs['gen_ai.input.messages'].includes('13812345678'));
    const chatOut = JSON.parse(chat.attrs['gen_ai.output.messages']);
    assert.equal(chatOut[0].role, 'assistant');
    assert.equal(chatOut[0].parts[0].content, '您的订单已发货');
    assert.equal(chatOut[0].finish_reason, 'stop');
    assert.equal(result.diag.content.openai, 'log_events_bridged');
    assert.ok(!result.diag.notes.some((n) => n.includes('日志事件')));
    assert.equal(result.diag.content.upstream, 'SPAN_ONLY');
    assert.ok(toolSpan.attrs['gen_ai.tool.call.arguments'].includes('A-1'));
    assert.ok(toolSpan.attrs['gen_ai.tool.call.result'].includes('shipped'));
    const agentIn = JSON.parse(agentSpan.attrs['gen_ai.input.messages']);
    assert.equal(agentIn[0].role, 'user');
    assert.equal(agentIn[0].parts[0].type, 'text');
    assert.ok(agentIn[0].parts[0].content.includes('[REDACTED:phone]'));
    assert.ok(agentIn[0].parts[0].content.includes('[REDACTED:email]'));
    assert.ok(!agentSpan.attrs['gen_ai.input.messages'].includes('13812345678'));
    const agentOut = JSON.parse(agentSpan.attrs['gen_ai.output.messages']);
    assert.equal(agentOut[0].role, 'assistant');
    assert.equal(agentOut[0].parts[0].content, '您的订单已发货');
    assert.equal(result.diag.semantics.masked_spans, 2, 'The agent arguments and chat input contain a phone number and email address');
  } finally {
    await c.close();
  }
});

test('Scenario A end to end with ESM, node --import @flashcat/genai-otel/register, and environment-only configuration', async () => {
  const c = await startCollector();
  try {
    const code = `
import * as otel from '@flashcat/genai-otel';
import OpenAI from 'openai';
const client = new OpenAI({ baseURL: process.env.FAKE_OPENAI, apiKey: 'sk-test' });
const lookupOrder = otel.tool('lookup_order', (orderId) => ({ order_id: orderId }));
const answer = otel.agent('kefu', async (q) => {
  lookupOrder('A-1');
  const r = await client.chat.completions.create({ model: 'gpt-4o', messages: [{ role: 'user', content: q }] });
  return r.choices[0].message.content;
});
const out = await otel.session({ conversationId: 'c-123', userId: 'u-9', tags: { team: 'kf' } }, () => answer('我的订单到哪了'));
await otel.flush();
console.log('RESULT:' + JSON.stringify({ out, diag: otel.diagnostics() }));
`;
    const { result, stderr } = await runScript('main', code, {
      nodeArgs: ['--import', '@flashcat/genai-otel/register'],
      env: { ...ENV(c), OTEL_SERVICE_NAME: 'kefu-agent' },
    });
    checkEndToEnd(c, result);
    assert.match(stderr, /\[flashcat-genai-otel\] INFO /, 'init logs a one-line summary');
  } finally {
    await c.close();
  }
});

test('ESM registration via --import and CommonJS require share state', async () => {
  const c = await startCollector();
  try {
    const code = `
const otel = require('@flashcat/genai-otel');
const { OpenAI } = require('openai');
(async () => {
  const again = otel.init({ serviceName: 'other' });   // register already initialized the SDK. init returns the original diagnostics.
  const client = new OpenAI({ baseURL: process.env.FAKE_OPENAI, apiKey: 'sk-test' });
  const answer = otel.agent('kefu', async (q) => (await client.chat.completions.create({ model: 'gpt-4o', messages: [{ role: 'user', content: q }] })).choices[0].message.content);
  await otel.session({ conversationId: 'c-123', userId: 'u-9', tags: { team: 'kf' } }, () => answer('x'));
  await otel.flush();
  console.log('RESULT:' + JSON.stringify({ again, diag: otel.diagnostics() }));
})();
`;
    const { result } = await runScript('main', code, {
      ext: 'cjs',
      nodeArgs: ['--import', '@flashcat/genai-otel/register'],
      env: { ...ENV(c), OTEL_SERVICE_NAME: 'kefu-agent' },
    });
    assert.equal(result.again.service_name, 'kefu-agent');
    assert.ok(!result.diag.problems.includes('init 尚未调用'));
    assert.equal(result.diag.provider.situation, 'A');
    assert.ok(result.diag.export.ok >= 2);
    const spans = c.spans();
    const chat = spans.find((s) => s.attrs['gen_ai.operation.name'] === 'chat');
    const ag = spans.find((s) => s.attrs['gen_ai.operation.name'] === 'invoke_agent');
    assert.equal(chat.parent, ag.spanId);
    assert.equal(chat.attrs['gen_ai.conversation.id'], 'c-123');
  } finally {
    await c.close();
  }
});

test('A streaming chat in an async generator agent emits a child model span', async () => {
  const c = await startCollector();
  try {
    const code = `
import * as otel from '@flashcat/genai-otel';
import OpenAI from 'openai';
const client = new OpenAI({ baseURL: process.env.FAKE_OPENAI, apiKey: 'sk-test' });
const streamAnswer = otel.agent('streamer', async function* (q) {
  const stream = await client.chat.completions.create({ model: 'gpt-4o', stream: true, stream_options: { include_usage: true }, messages: [{ role: 'user', content: q }] });
  for await (const chunk of stream) {
    const t = chunk.choices[0]?.delta?.content;
    if (t) yield t;
  }
});
let text = '';
await otel.session({ conversationId: 'c-s' }, async () => { for await (const t of streamAnswer('x')) text += t; });
await otel.flush();
console.log('RESULT:' + JSON.stringify({ text }));
`;
    const { result } = await runScript('main', code, {
      nodeArgs: ['--import', '@flashcat/genai-otel/register'],
      env: { ...ENV(c), OTEL_SERVICE_NAME: 'kefu-agent' },
    });
    assert.equal(result.text, '已发货');
    const spans = c.spans();
    const chat = spans.find((s) => s.attrs['gen_ai.operation.name'] === 'chat');
    const ag = spans.find((s) => s.attrs['gen_ai.operation.name'] === 'invoke_agent');
    assert.ok(chat && ag);
    assert.equal(chat.parent, ag.spanId);
    assert.equal(chat.attrs['gen_ai.conversation.id'], 'c-s');
  } finally {
    await c.close();
  }
});

test('Upstream OpenAI instrumentation for openai <7 adds cache tokens, streaming TTFT, and request.stream with one model span per call and unchanged results', async () => {
  const c = await startCollector();
  try {
    const code = `
import * as otel from '@flashcat/genai-otel';
import OpenAI from 'openai';
const client = new OpenAI({ baseURL: process.env.FAKE_OPENAI, apiKey: 'sk-test' });
const messages = [{ role: 'user', content: 'x' }];
const { data, response } = await client.chat.completions.create({ model: 'gpt-4o', messages }).withResponse();
let text = '';
for await (const chunk of await client.chat.completions.create({ model: 'gpt-4o', messages, stream: true, stream_options: { include_usage: true } })) {
  text += chunk.choices[0]?.delta?.content ?? '';
}
const r = await client.responses.create({ model: 'gpt-4o', input: 'x' });
await otel.flush();
console.log('RESULT:' + JSON.stringify({ content: data.choices[0].message.content, status: response.status, text, out: r.output_text }));
`;
    const { result } = await runScript('main', code, {
      nodeArgs: ['--import', '@flashcat/genai-otel/register'],
      env: { ...ENV(c), OTEL_SERVICE_NAME: 'kefu-agent', OTEL_METRICS_EXPORTER: 'none' },
    });
    assert.deepEqual(result, { content: '您的订单已发货', status: 200, text: '已发货', out: '您的订单已发货' });
    const chats = c.spans().filter((s) => s.attrs['gen_ai.operation.name'] === 'chat');
    assert.equal(chats.length, 3, 'Exactly one model span per call');
    assert.ok(chats.every((s) => s.scope === '@opentelemetry/instrumentation-openai'));
    const [plain, streamed, resp] = ['chatcmpl-test', 'chatcmpl-s', 'resp_test'].map((id) => chats.find((s) => s.attrs['gen_ai.response.id'] === id));
    assert.equal(plain.attrs['gen_ai.usage.cache_read.input_tokens'], 64);
    assert.equal(plain.attrs['gen_ai.request.stream'], false);
    assert.equal(plain.attrs['gen_ai.response.time_to_first_chunk'], undefined, 'Non-streaming spans omit TTFT');
    assert.equal(streamed.attrs['gen_ai.usage.cache_read.input_tokens'], 8, 'Streaming usage comes from the final chunk that carries it');
    assert.equal(streamed.attrs['gen_ai.request.stream'], true);
    const ttft = streamed.attrs['gen_ai.response.time_to_first_chunk'];
    assert.ok(typeof ttft === 'number' && ttft >= 0.025 && ttft < 2, `Time to first token: ${ttft} s (role-only chunks are excluded, and content arrives 30 ms later)`);
    assert.equal(resp.attrs['gen_ai.usage.cache_read.input_tokens'], 16, 'Responses uses input_tokens_details.cached_tokens');
    assert.equal(resp.attrs['gen_ai.request.stream'], false);
  } finally {
    await c.close();
  }
});

test('Zero-code OpenAI <7 captures log-event content on spans for non-streaming chat, streaming chat, and Responses', async () => {
  const c = await startCollector();
  try {
    const code = `
import * as otel from '@flashcat/genai-otel';
import OpenAI from 'openai';
const client = new OpenAI({ baseURL: process.env.FAKE_OPENAI, apiKey: 'sk-test' });
const messages = [{ role: 'system', content: '你是客服' }, { role: 'user', content: '我的订单' }];
await client.chat.completions.create({ model: 'gpt-4o', messages });
for await (const _ of await client.chat.completions.create({ model: 'gpt-4o', messages, stream: true, stream_options: { include_usage: true } }));
await client.responses.create({ model: 'gpt-4o', input: '查订单' });
await otel.flush();
console.log('RESULT:' + JSON.stringify(otel.diagnostics().content));
`;
    const { result } = await runScript('main', code, {
      nodeArgs: ['--import', '@flashcat/genai-otel/register'],
      env: { ...ENV(c), OTEL_SERVICE_NAME: 'kefu-agent', OTEL_METRICS_EXPORTER: 'none', FLASHCAT_CAPTURE_CONTENT: 'span' },
    });
    assert.equal(result.openai, 'log_events_bridged');
    const chats = c.spans().filter((s) => s.attrs['gen_ai.operation.name'] === 'chat');
    const [plain, streamed, resp] = ['chatcmpl-test', 'chatcmpl-s', 'resp_test'].map((id) => chats.find((s) => s.attrs['gen_ai.response.id'] === id));
    for (const s of [plain, streamed]) {
      const input = JSON.parse(s.attrs['gen_ai.input.messages']);
      assert.deepEqual(input.map((m) => [m.role, m.parts[0].content]), [['system', '你是客服'], ['user', '我的订单']]);
    }
    assert.equal(JSON.parse(plain.attrs['gen_ai.output.messages'])[0].parts[0].content, '您的订单已发货');
    assert.equal(JSON.parse(streamed.attrs['gen_ai.output.messages'])[0].parts[0].content, '已发货', 'Streaming output is assembled and written to the span');
    assert.ok(resp.attrs['gen_ai.input.messages'].includes('查订单'), resp.attrs['gen_ai.input.messages']);
    assert.ok(resp.attrs['gen_ai.output.messages'].includes('您的订单已发货'), resp.attrs['gen_ai.output.messages']);
  } finally {
    await c.close();
  }
});
