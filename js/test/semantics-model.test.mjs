// Manual model spans (llm, embedding, and retrieval) and annotate (contract §4, §4.1, and §4.2).
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

function env() {
  return { FLASHCAT_ENDPOINT: c.url + '/v1/traces', FLASHCAT_API_KEY: 'k:s' };
}

function spansOf(run) {
  return c.spans().filter((s) => s.resource['test.run'] === run);
}

function byOp(spans) {
  return Object.fromEntries(spans.map((s) => [s.attrs['gen_ai.operation.name'] ?? s.name, s]));
}

test('llm, embedding, and retrieval span names, kinds, and attributes, annotate fields, and retrieval content masking', async () => {
  const run = 'model-spans';
  const code = `
import * as otel from '@flashcat/otel';
otel.init({ serviceName: 'x', metrics: false, captureContent: 'span', resourceAttributes: { 'test.run': '${run}' } });
const ask = otel.llm('qwen-max', async (prompt) => {
  otel.annotate({
    input: [{ role: 'system', content: '你是客服' }, { role: 'user', content: prompt }],
    output: '已为您查询',
    model: 'qwen-max-0919',
    usage: { inputTokens: 30, outputTokens: 5, cacheReadInputTokens: 10, cacheWriteInputTokens: 2, bogus: 1 },
    finishReasons: ['stop'],
    tags: { route: 'gateway' },
  });
  return { raw: 'annotate 写过 output，返回值不覆盖' };
}, { provider: 'dashscope' });
const embed = otel.embedding('text-embedding-v3', (texts) => {
  otel.annotate({ usage: { inputTokens: 8, outputTokens: 99 }, model: 'text-embedding-v3' });
  return [[0.1, 0.2]];
}, { provider: 'dashscope' });
const search = otel.retrieval('kb-orders', (query) => [{ id: 'd1', content: '订单联系人 13812345678', score: 0.9 }, '第二段文本']);
const handle = otel.agent('kefu', async (q) => { embed([q]); search(q); return ask(q); });
await handle('我的订单到哪了');
await otel.flush();
console.log('RESULT:' + JSON.stringify(otel.diagnostics().semantics));
`;
  const { result } = await runScript('bare', code, { env: env() });
  const ops = byOp(spansOf(run));
  const { chat, embeddings: emb, retrieval: ret, invoke_agent: agent } = ops;

  assert.equal(chat.name, 'chat qwen-max');
  assert.equal(chat.kind, 3, 'CLIENT');
  assert.equal(chat.attrs['gen_ai.request.model'], 'qwen-max');
  assert.equal(chat.attrs['gen_ai.provider.name'], 'dashscope');
  assert.equal(chat.attrs['gen_ai.response.model'], 'qwen-max-0919');
  assert.equal(chat.attrs['gen_ai.usage.input_tokens'], 30);
  assert.equal(chat.attrs['gen_ai.usage.output_tokens'], 5);
  assert.equal(chat.attrs['gen_ai.usage.cache_read.input_tokens'], 10);
  assert.equal(chat.attrs['gen_ai.usage.cache_write.input_tokens'], 2);
  assert.deepEqual(chat.attrs['gen_ai.response.finish_reasons'], ['stop']);
  assert.equal(chat.attrs['flashcat.tag.route'], 'gateway');
  const msgs = JSON.parse(chat.attrs['gen_ai.input.messages']);
  assert.deepEqual(msgs.map((m) => m.role), ['system', 'user']);
  assert.equal(msgs[1].parts[0].content, '我的订单到哪了');
  const out = JSON.parse(chat.attrs['gen_ai.output.messages']);
  assert.equal(out[0].role, 'assistant');
  assert.equal(out[0].parts[0].content, '已为您查询');
  assert.equal(chat.parent, agent.spanId);

  assert.equal(emb.name, 'embeddings text-embedding-v3');
  assert.equal(emb.kind, 3);
  assert.equal(emb.attrs['gen_ai.usage.input_tokens'], 8);
  assert.equal(emb.attrs['gen_ai.usage.output_tokens'], undefined, 'Embedding records only input tokens');
  assert.equal(emb.attrs['gen_ai.input.messages'], undefined, 'Embedding omits content');

  assert.equal(ret.name, 'retrieval kb-orders');
  assert.equal(ret.attrs['gen_ai.data_source.id'], 'kb-orders');
  assert.ok(ret.attrs['gen_ai.retrieval.query.text'].includes('我的订单到哪了'));
  const docs = JSON.parse(ret.attrs['gen_ai.retrieval.documents']);
  assert.equal(docs[0].id, 'd1');
  assert.deepEqual(docs[1], { content: '第二段文本' });
  assert.ok(!ret.attrs['gen_ai.retrieval.documents'].includes('13812345678'), 'Retrieved documents are masked too');
  assert.ok(ret.attrs['gen_ai.retrieval.documents'].includes('[REDACTED:phone]'));

  assert.equal(result.nested_inference_spans, 0);
});

test('annotate respects the content capture switch and is a no-op outside a semantic span, including inside trace', async () => {
  const run = 'annotate-scope';
  const code = `
import * as otel from '@flashcat/otel';
otel.init({ serviceName: 'x', metrics: false, resourceAttributes: { 'test.run': '${run}' } });
otel.annotate({ output: '不在语义 span 里', tags: { a: 'b' } });
const ask = otel.llm('m1', (p) => { otel.annotate({ input: 'secret prompt', output: 'secret answer', usage: { inputTokens: 3 } }); return 'x'; });
const toolFn = otel.tool('t1', (x) => { otel.annotate({ output: 'ignored: content off' }); return x; });
otel.trace('job', () => { otel.annotate({ tags: { plain: '1' } }); ask('p'); toolFn(1); });
await otel.flush();
console.log('RESULT:{"ok":true}');
`;
  const { result } = await runScript('bare', code, { env: env() });
  assert.deepEqual(result, { ok: true });
  const ops = byOp(spansOf(run));
  assert.equal(ops.chat.attrs['gen_ai.usage.input_tokens'], 3);
  assert.equal(ops.chat.attrs['gen_ai.input.messages'], undefined);
  assert.equal(ops.chat.attrs['gen_ai.output.messages'], undefined);
  assert.equal(ops.execute_tool.attrs['gen_ai.tool.call.result'], undefined);
  assert.equal(ops.job.attrs['flashcat.tag.plain'], undefined);
});

test('Annotated output takes precedence, non-array retrieval results become one document, and oversized lists remain valid JSON', async () => {
  const run = 'annotate-output';
  const code = `
import * as otel from '@flashcat/otel';
otel.init({ serviceName: 'x', metrics: false, captureContent: 'span', resourceAttributes: { 'test.run': '${run}' } });
otel.tool('lookup', (id) => { otel.annotate({ output: { status: 'shipped' } }); return 'raw'; })('A-1');
otel.retrieval('kb', (q) => '只有一段')('q');
otel.retrieval('big', (q) => Array.from({ length: 50 }, (_, i) => ({ id: 'd' + i, content: 'x'.repeat(2000) })))('q');
await otel.flush();
console.log('RESULT:{}');
`;
  await runScript('bare', code, { env: env() });
  const spans = spansOf(run);
  const tool = spans.find((s) => s.name === 'execute_tool lookup');
  assert.deepEqual(JSON.parse(tool.attrs['gen_ai.tool.call.result']), { status: 'shipped' });
  const kb = spans.find((s) => s.name === 'retrieval kb');
  assert.deepEqual(JSON.parse(kb.attrs['gen_ai.retrieval.documents']), [{ content: '只有一段' }]);
  const big = spans.find((s) => s.name === 'retrieval big');
  const docs = JSON.parse(big.attrs['gen_ai.retrieval.documents']);
  assert.ok(docs.length < 50);
  assert.deepEqual(docs.at(-1), { content: '…[truncated]' });
});

test('An instrumented model call inside a manual llm span increments nested_inference_spans and reports one problem', async () => {
  const run = 'nested';
  const code = `
import { trace } from '@opentelemetry/api';
import * as otel from '@flashcat/otel';
otel.init({ serviceName: 'x', metrics: false, resourceAttributes: { 'test.run': '${run}' } });
const tracer = trace.getTracer('@opentelemetry/instrumentation-openai');
const ask = otel.llm('gpt-4o', async () => {
  tracer.startSpan('chat gpt-4o', { attributes: { 'gen_ai.operation.name': 'chat' } }).end();
  tracer.startSpan('ChatCompletion', { attributes: { 'openinference.span.kind': 'LLM' } }).end();
});
await ask();
// Inference spans outside llm are excluded.
tracer.startSpan('chat outside', { attributes: { 'gen_ai.operation.name': 'chat' } }).end();
await otel.flush();
console.log('RESULT:' + JSON.stringify(otel.diagnostics()));
`;
  const { result } = await runScript('bare', code, { env: env() });
  assert.equal(result.semantics.nested_inference_spans, 2);
  assert.equal(result.problems.filter((p) => p.includes('重复计数')).length, 1);
});
