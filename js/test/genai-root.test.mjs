// AI root marker (contract §5.1): test span-start and export behavior using the packaged SDK.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { runScript, startCollector } from './helpers.mjs';

const ROOT = 'flashcat.genai.root';

async function withCollector(fn) {
  const c = await startCollector();
  try {
    await fn(c, { FLASHCAT_ENDPOINT: c.url + '/v1/traces', FLASHCAT_API_KEY: 'k:s' });
  } finally {
    await c.close();
  }
}

test('The HTTP child agent is the AI root. Nested semantic spans and model calls behind ordinary spans are descendants', () =>
  withCollector(async (c, env) => {
    await runScript('bare', `
import { SpanKind, trace } from '@opentelemetry/api';
import * as otel from '@flashcat/otel';
otel.init({ serviceName: 'x', metrics: false });
const ask = otel.llm('direct', async () => {});
const askThroughCache = otel.llm('through-cache', async () => {});
const embed = otel.embedding('m', async () => {});
const retrieve = otel.retrieval('kb', async () => []);
const workflow = otel.workflow('fulfill', async () => {
  await Promise.resolve();
  await Promise.all([embed(), retrieve()]);
});
const tool = otel.tool('lookup', async () => {
  await Promise.resolve();
  await workflow();
});
const agent = otel.agent('kefu', async () => {
  await Promise.resolve();
  await Promise.all([ask(), tool(), otel.trace('cache', async () => {
    await Promise.resolve();
    await askThroughCache();
  })]);
});
await otel.session({ conversationId: 'c-1' }, () => trace.getTracer('biz').startActiveSpan(
  'POST /chat', { kind: SpanKind.SERVER, attributes: { 'http.request.method': 'POST' } }, async (http) => {
    await agent();
    http.end();
  },
));
await otel.flush();
`, { env });
    const spans = new Map(c.spans().map((s) => [s.name, s]));
    const http = spans.get('POST /chat');
    const agent = spans.get('invoke_agent kefu');
    assert.equal(http.attrs['gen_ai.conversation.id'], 'c-1');
    assert.equal(http.attrs[ROOT], undefined);
    assert.equal(agent.parent, http.spanId);
    assert.equal(agent.attrs[ROOT], true);
    const cache = spans.get('cache');
    assert.equal(cache.attrs[ROOT], undefined);
    for (const [name, parent] of [
      ['chat direct', agent],
      ['execute_tool lookup', agent],
      ['invoke_workflow fulfill', spans.get('execute_tool lookup')],
      ['embeddings m', spans.get('invoke_workflow fulfill')],
      ['retrieval kb', spans.get('invoke_workflow fulfill')],
      ['chat through-cache', cache],
    ]) {
      assert.equal(spans.get(name).parent, parent.spanId, name);
      assert.equal(spans.get(name).attrs[ROOT], false, name);
    }
  }));

test('Export marks spans that gain GenAI attributes late, and child spans read current parent attributes at start', () =>
  withCollector(async (c, env) => {
    const { result } = await runScript('bare', `
import { trace } from '@opentelemetry/api';
import * as otel from '@flashcat/otel';
otel.init({ serviceName: 'x', metrics: false });
const tr = trace.getTracer('biz');
otel.trace('late root', (span) => span.setAttribute('gen_ai.operation.name', 'chat'));
otel.agent('outer', () => otel.trace('late child', (span) => {
  span.setAttribute('llm.request.type', 'chat');
  span.setAttribute('db.password', 'secret');
}))();
otel.trace('parent late', (span) => {
  span.setAttribute('openinference.span.kind', 'AGENT');
  tr.startSpan('child of late', { attributes: { 'gen_ai.operation.name': 'chat' } }).end();
});
await otel.flush();
console.log('RESULT:' + JSON.stringify(otel.diagnostics().semantics));
`, { env });
    const attrs = Object.fromEntries(c.spans().map((s) => [s.name, s.attrs]));
    assert.equal(attrs['late root'][ROOT], true);
    assert.equal(attrs['invoke_agent outer'][ROOT], true);
    assert.equal(attrs['late child'][ROOT], false);
    assert.equal(attrs['late child']['db.password'], '[REDACTED]');
    assert.equal(attrs['parent late'][ROOT], true);
    assert.equal(attrs['child of late'][ROOT], false);
    assert.equal(result.masked_spans, 1, 'Setting the marker alone does not count as masking');
    assert.equal(result.truncated_spans, 0);
  }));

test('GenAI classification matches the Flashcat backend, including Vercel ai. keys and the session ID exception', () =>
  withCollector(async (c, env) => {
    await runScript('bare', `
import * as otel from '@flashcat/otel';
otel.init({ serviceName: 'x', metrics: false });
for (const [name, attrs] of Object.entries({
  session: { 'gen_ai.conversation.id': 'c-1' },
  ai: { 'ai.model.id': 'm' },
  operation: { 'operation.name': 'HTTP request' },
  vercel: { 'operation.name': 'ai.generateText', 'ai.model.id': 'm' },
  operationId: { 'ai.operationId': 'ai.generateText' },
  flashcat: { 'flashcat.span.kind': 'agent' },
  genai: { 'gen_ai.usage.input_tokens': 1 },
  llm: { 'llm.token_count.prompt': 1 },
  openinference: { 'openinference.project.name': 'x' },
  traceloop: { 'traceloop.entity.name': 'x' },
  langfuse: { 'langfuse.observation.name': 'x' },
})) {
  otel.trace(name, () => {}, attrs);
}
await otel.flush();
`, { env });
    const roots = Object.fromEntries(c.spans().map((s) => [s.name, s.attrs[ROOT]]));
    assert.deepEqual(roots, {
      session: undefined, ai: undefined, operation: undefined, vercel: true, operationId: true,
      flashcat: true, genai: true, llm: true, openinference: true, traceloop: true, langfuse: true,
    });
  }));

test('Remote parents restored from traceparent do not propagate GenAI ancestry', () =>
  withCollector(async (c, env) => {
    await runScript('bare', `
import { context, propagation, ROOT_CONTEXT, trace } from '@opentelemetry/api';
import * as otel from '@flashcat/otel';
otel.init({ serviceName: 'x', metrics: false });
otel.agent('upstream', () => otel.trace('forwarder', () => {
  const carrier = {};
  propagation.inject(context.active(), carrier);
  const incoming = propagation.extract(ROOT_CONTEXT, carrier);
  const tr = trace.getTracer('biz');
  tr.startSpan('downstream', { attributes: { 'gen_ai.operation.name': 'chat' } }, incoming).end();
}))();
await otel.flush();
`, { env });
    const spans = c.spans();
    const downstream = spans.find((s) => s.name === 'downstream');
    const forwarder = spans.find((s) => s.name === 'forwarder');
    assert.equal(downstream.traceId, forwarder.traceId);
    assert.equal(downstream.parent, forwarder.spanId);
    assert.equal(downstream.attrs[ROOT], true);
  }));

test('A reused exporter receives only span-start markers, and OpenInference spans without attributes are identified by scope', () =>
  withCollector(async (c, env) => {
    const { result } = await runScript('main', `
import { context, trace } from '@opentelemetry/api';
import { NodeSDK } from '@opentelemetry/sdk-node';
import { InMemorySpanExporter, SimpleSpanProcessor } from '@opentelemetry/sdk-trace';
import * as otel from '@flashcat/otel';
const mem = new InMemorySpanExporter();
const sdk = new NodeSDK({ serviceName: 'x', spanProcessors: [
  otel.spanProcessor(), new SimpleSpanProcessor({ exporter: mem }),
] });
sdk.start();
const d = otel.init({ exporter: 'reuse', metrics: false });
const langchain = trace.getTracer('@arizeai/openinference-instrumentation-langchain');
const openai = trace.getTracer('@arizeai/openinference-instrumentation-openai');
const roots = {};
otel.trace('http', () => langchain.startActiveSpan('chain', (chain) => {
  roots.chain = chain.attributes['${ROOT}'];
  const parent = trace.setSpanContext(context.active(), chain.spanContext());
  openai.startActiveSpan('model', {}, parent, (model) => {
    roots.model = model.attributes['${ROOT}'];
    model.setAttribute('openinference.span.kind', 'LLM');
    model.end();
  });
  otel.trace('late child', (span) => span.setAttribute('gen_ai.operation.name', 'chat'));
  chain.setAttribute('openinference.span.kind', 'CHAIN');
  chain.end();
}));
await otel.flush();
const exported = Object.fromEntries(mem.getFinishedSpans().map((s) => [s.name, s.attributes['${ROOT}'] ?? null]));
console.log('RESULT:' + JSON.stringify({ mode: d.provider.exporter_mode, roots, exported }));
`, { env: { ...env, OTEL_METRICS_EXPORTER: 'none', OTEL_LOGS_EXPORTER: 'none' } });
    assert.equal(result.mode, 'reuse');
    assert.deepEqual(result.roots, { chain: true, model: false });
    assert.deepEqual(result.exported, { http: null, chain: true, model: false, 'late child': null });
    const roots = Object.fromEntries(c.spans().map((s) => [s.name, s.attrs[ROOT]]));
    assert.deepEqual(roots, { http: undefined, chain: true, model: false, 'late child': false });
  }));

test('An OpenInference LangChain RunnableSequence is an AI root, and its children retain that ancestry when reconstructing local parent context', () =>
  withCollector(async (c, env) => {
    await runScript('oi', `
const otel = require('@flashcat/otel');
otel.init({ serviceName: 'x', metrics: false });
const { RunnableLambda, RunnableSequence } = require('@langchain/core/runnables');
const { FakeListChatModel } = require('@langchain/core/utils/testing');
const identity = RunnableLambda.from((q) => q);
const model = new FakeListChatModel({ responses: ['ok'] });
const chain = RunnableSequence.from([identity, model]);
(async () => {
  await otel.trace('POST /langchain', () => chain.invoke('hi'));
  await otel.flush();
})();
`, { env, ext: 'cjs' });
    const spans = c.spans();
    const http = spans.find((s) => s.name === 'POST /langchain');
    const chain = spans.find((s) => s.name === 'RunnableSequence');
    assert.equal(chain.scope, '@arizeai/openinference-instrumentation-langchain');
    assert.equal(chain.parent, http.spanId);
    assert.equal(chain.attrs[ROOT], true);
    const children = spans.filter((s) => s.parent === chain.spanId);
    assert.ok(children.some((s) => s.attrs['openinference.span.kind'] === 'LLM'));
    for (const child of children) assert.equal(child.attrs[ROOT], false, child.name);
  }));

test('Ancestry tracking retains the most recent 65,536 spans with a GenAI ancestor', async () => {
  const { result } = await runScript('bare', `
import { ROOT_CONTEXT, trace } from '@opentelemetry/api';
import * as otel from '@flashcat/otel';
otel.init({ serviceName: 'x', exporter: 'none', metrics: false });
const tr = trace.getTracer('biz');
const attrs = { 'gen_ai.operation.name': 'chat' };
const agent = tr.startSpan('agent', { attributes: attrs });
const agentCtx = trace.setSpan(ROOT_CONTEXT, agent);
const startChild = () => tr.startSpan('plain', {}, agentCtx);
const oldest = startChild();
const next = startChild();
for (let i = 2; i < 65536; i++) startChild().end();
tr.startSpan('apm').end();
tr.startSpan('another root', { attributes: attrs }).end();
function rootUnder(parent) {
  const span = tr.startSpan('llm', { attributes: attrs }, trace.setSpan(ROOT_CONTEXT, parent));
  span.end();
  return span.attributes['${ROOT}'];
}
const atCapacity = rootUnder(oldest); // Adding an entry evicts oldest.
const afterEviction = rootUnder(oldest);
const retained = rootUnder(next);
oldest.end();
next.end();
agent.end();
console.log('RESULT:' + JSON.stringify({ atCapacity, afterEviction, retained }));
`);
  assert.deepEqual(result, { atCapacity: false, afterEviction: true, retained: false });
});
