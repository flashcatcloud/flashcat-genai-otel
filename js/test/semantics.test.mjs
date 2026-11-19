// Semantic API span names and attributes, sync and async return types, and ERROR status with the original exception rethrown.
// Preserve this, arguments, and results. Capture can be disabled. Session attributes stay in scope, preserve existing IDs, and omit baggage by default.
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

function spansOf(traceIdSet) {
  return c.spans().filter((s) => traceIdSet.has(s.attrs['test.run']) || traceIdSet.has(s.resource['test.run']));
}

test('Four span types have the expected names, kinds, and start attributes and omit content when capture is off', async () => {
  const run = 'sem-names';
  const code = `
import * as otel from '@flashcat/otel';
otel.init({ serviceName: 'x', metrics: false, resourceAttributes: { 'test.run': '${run}' } });
const a = otel.agent('kefu', (q) => 'a:' + q, { description: '客服' });
const t = otel.tool('search_kb', (q) => ['doc'], { description: '查知识库' });
const w = otel.workflow('ticket_flow', () => 1);
a('hi'); t('q'); w();
otel.trace('batch_job', (span) => { span.setAttribute('k', 'v'); }, { 'job.id': 'j-1' });
await otel.flush();
console.log('RESULT:{}');
`;
  await runScript('bare', code, { env: env() });
  const spans = spansOf(new Set([run]));
  const by = Object.fromEntries(spans.map((s) => [s.name, s]));
  assert.deepEqual(Object.keys(by).sort(), ['batch_job', 'execute_tool search_kb', 'invoke_agent kefu', 'invoke_workflow ticket_flow']);
  for (const s of spans) {
    assert.equal(s.kind, 1, 'INTERNAL');
    assert.equal(s.scope, 'flashcat.otel');
  }
  assert.deepEqual(by['invoke_agent kefu'].attrs, {
    'gen_ai.operation.name': 'invoke_agent',
    'gen_ai.agent.name': 'kefu',
    'gen_ai.agent.description': '客服',
    'flashcat.genai.root': true,
  });
  assert.deepEqual(by['execute_tool search_kb'].attrs, {
    'gen_ai.operation.name': 'execute_tool',
    'gen_ai.tool.name': 'search_kb',
    'gen_ai.tool.type': 'function',
    'gen_ai.tool.description': '查知识库',
    'flashcat.genai.root': true,
  });
  assert.deepEqual(by['invoke_workflow ticket_flow'].attrs, {
    'gen_ai.operation.name': 'invoke_workflow',
    'gen_ai.workflow.name': 'ticket_flow',
    'flashcat.genai.root': true,
  });
  assert.deepEqual(by.batch_job.attrs, { 'job.id': 'j-1', k: 'v' });
});

test('Wrappers preserve this, arguments, return values, name, length, and exceptions across sync calls, promises, async iterators, and generators', async () => {
  const run = 'sem-shapes';
  const code = `
import * as otel from '@flashcat/otel';
otel.init({ serviceName: 'x', metrics: false, captureContent: 'span', resourceAttributes: { 'test.run': '${run}' } });
class OrderError extends Error {}
const res = {};
const obj = { base: 40, add: otel.tool('add', function add(a, b = 1) { return this.base + a + b; }) };
res.thisOk = obj.add(1, 1);
res.name = obj.add.name; res.length = obj.add.length;

const boomErr = new OrderError('no such order');
const boom = otel.tool('boom', () => { throw boomErr; });
try { boom(); } catch (e) { res.sameSync = e === boomErr; }

const aboom = otel.agent('aboom', async () => { throw boomErr; });
try { await aboom(); } catch (e) { res.sameAsync = e === boomErr; }

const aio = otel.agent('aio', async (x) => x * 2);
res.async = await aio(21);

const gen = otel.tool('gen', function* () { yield 1; yield 2; yield 3; });
res.gen = [...gen()];
const g2 = gen(); g2.next(); g2.return(); // Close early.

const agen = otel.workflow('agen', async function* () { yield 'a'; yield 'b'; });
res.agen = []; for await (const v of agen()) res.agen.push(v);
const early = otel.workflow('agen_early', async function* () { yield 'a'; yield 'b'; });
for await (const v of early()) { break; }
const agenErr = otel.workflow('agen_err', async function* () { yield 'a'; throw boomErr; });
try { for await (const v of agenErr()) {} } catch (e) { res.sameAgen = e === boomErr; }

// Return thenables with extra APIs unchanged, such as a model SDK APIPromise.
class APIPromiseLike { constructor(v) { this.p = Promise.resolve(v); } then(a, b) { return this.p.then(a, b); } withResponse() { return 'raw'; } }
const thenable = new APIPromiseLike('v');
const passthrough = otel.tool('thenable', () => thenable);
const got = passthrough();
res.sameThenable = got === thenable; res.withResponse = got.withResponse(); res.thenableValue = await got;

// Return streams with only [Symbol.asyncIterator] unchanged and end the span when iteration ends.
class StreamLike { constructor() { this.controller = 'ctl'; } async *[Symbol.asyncIterator]() { yield 's1'; yield 's2'; } }
const sobj = new StreamLike();
const streamFn = otel.agent('stream_like', async () => sobj);
const gotStream = await streamFn();
res.sameStream = gotStream === sobj; res.stream = []; for await (const v of gotStream) res.stream.push(v);

await otel.flush();
console.log('RESULT:' + JSON.stringify(res));
`;
  const { result } = await runScript('bare', code, { env: env() });
  assert.deepEqual(result, {
    thisOk: 42,
    name: 'add',
    length: 1,
    sameSync: true,
    sameAsync: true,
    async: 42,
    gen: [1, 2, 3],
    agen: ['a', 'b'],
    sameAgen: true,
    sameThenable: true,
    withResponse: 'raw',
    thenableValue: 'v',
    sameStream: true,
    stream: ['s1', 's2'],
  });
  const spans = spansOf(new Set([run]));
  const name = (s) => s.attrs['gen_ai.tool.name'] ?? s.attrs['gen_ai.agent.name'] ?? s.attrs['gen_ai.workflow.name'];
  const all = (n) => spans.filter((s) => name(s) === n);
  const one = (n) => {
    const l = all(n);
    assert.equal(l.length >= 1, true, `Missing span ${n}`);
    return l[0];
  };
  assert.equal(one('boom').status, 2, 'ERROR');
  assert.equal(one('boom').attrs['error.type'], 'OrderError');
  assert.equal(one('boom').events[0].name, 'exception');
  assert.equal(one('boom').statusMessage, 'no such order');
  assert.equal(one('aboom').status, 2);
  assert.equal(one('aio').attrs['gen_ai.output.messages'], JSON.stringify([{ role: 'assistant', parts: [{ type: 'text', content: '42' }] }]));
  assert.equal(one('aio').attrs['gen_ai.input.messages'], JSON.stringify([{ role: 'user', parts: [{ type: 'text', content: '[21]' }] }]));
  assert.equal(one('add').attrs['gen_ai.tool.call.arguments'], '[1,1]');
  assert.equal(one('add').attrs['gen_ai.tool.call.result'], '42');
  assert.equal(all('gen').length, 2, 'Generator spans end on exhaustion and early return');
  assert.ok(all('gen').some((s) => s.attrs['gen_ai.tool.call.result'] === '[1,2,3]'));
  assert.equal(one('agen').attrs['gen_ai.output.messages'], JSON.stringify([{ role: 'assistant', parts: [{ type: 'text', content: '["a","b"]' }] }]));
  assert.equal(one('agen_early').status, 0, 'Early return leaves the status non-error');
  assert.equal(one('agen_err').status, 2);
  assert.equal(one('thenable').attrs['gen_ai.tool.call.result'], 'v', 'Preserve strings verbatim, as in Python');
  assert.equal(one('stream_like').attrs['gen_ai.output.messages'], JSON.stringify([{ role: 'assistant', parts: [{ type: 'text', content: '["s1","s2"]' }] }]));
});

test('Functions wrapped before init work with no-op spans and start emitting spans after init', async () => {
  const run = 'sem-preinit';
  const code = `
import * as otel from '@flashcat/otel';
const f = otel.agent('early', (x) => x + 1);
const r1 = f(1);
otel.init({ serviceName: 'x', metrics: false, resourceAttributes: { 'test.run': '${run}' } });
const r2 = f(2);
await otel.flush();
console.log('RESULT:' + JSON.stringify([r1, r2]));
`;
  const { result } = await runScript('bare', code, { env: env() });
  assert.deepEqual(result, [2, 3]);
  assert.equal(spansOf(new Set([run])).filter((s) => s.attrs['gen_ai.agent.name'] === 'early').length, 1);
});

test('Serialization caps content at 32 KiB with …[truncated] and falls back to String() on failure', async () => {
  const run = 'sem-serialize';
  const code = `
import * as otel from '@flashcat/otel';
otel.init({ serviceName: 'x', metrics: false, captureContent: 'span', resourceAttributes: { 'test.run': '${run}' } });
otel.tool('big', (s) => 1)('x'.repeat(40000));
const cyc = {}; cyc.self = cyc;
otel.tool('cyc', (o) => 10n)(cyc);
await otel.flush();
console.log('RESULT:{}');
`;
  await runScript('bare', code, { env: env() });
  const spans = spansOf(new Set([run]));
  const big = spans.find((s) => s.attrs['gen_ai.tool.name'] === 'big');
  assert.equal(big.attrs['gen_ai.tool.call.arguments'].length, 32 * 1024 + '…[truncated]'.length);
  assert.ok(big.attrs['gen_ai.tool.call.arguments'].endsWith('…[truncated]'));
  const cyc = spans.find((s) => s.attrs['gen_ai.tool.name'] === 'cyc');
  assert.equal(cyc.attrs['gen_ai.tool.call.arguments'], '[object Object]');
  assert.equal(cyc.attrs['gen_ai.tool.call.result'], '10');
});

test('Sessions scope three attribute types, preserve existing conversation IDs, omit baggage by default, support parent inheritance, and count orphan inference spans', async () => {
  const run = 'sem-session';
  const code = `
import * as otel from '@flashcat/otel';
import { trace, propagation, context } from '@opentelemetry/api';
otel.init({ serviceName: 'x', metrics: false, resourceAttributes: { 'test.run': '${run}' } });
const tracer = trace.getTracer('third-party');
const res = {};
await otel.session({ conversationId: 'c-1', userId: 'u-1', tags: { tenant: 'a', n: 1 } }, async () => {
  tracer.startSpan('raw-inside').end();                                    // Include spans from any source.
  tracer.startSpan('has-conv', { attributes: { 'gen_ai.conversation.id': 'own' } }).end();
  res.baggage = propagation.getBaggage(context.active()) ?? null;
  await otel.session({ tags: { step: 's2' } }, async () => { tracer.startSpan('nested').end(); });
});
tracer.startSpan('raw-outside').end();
otel.session({ conversationId: 'c-2', userId: 'u-2', propagate: true }, () => {
  const b = propagation.getBaggage(context.active());
  res.propagated = { conv: b?.getEntry('gen_ai.conversation.id')?.value, user: b?.getEntry('user.id')?.value };
});
// Start the child outside the session context to test inheritance from the parent's session attributes.
const parent = tracer.startSpan('parent', { attributes: { 'gen_ai.conversation.id': 'p-1', 'user.id': 'pu', 'flashcat.tag.t': 'x' } });
context.with(trace.setSpan(context.active(), parent), () => tracer.startSpan('child').end());
parent.end();
// Orphan inference span.
tracer.startSpan('chat gpt-4o', { attributes: { 'gen_ai.operation.name': 'chat' } }).end();
await otel.flush();
console.log('RESULT:' + JSON.stringify({ res, diag: otel.diagnostics() }));
`;
  const { result } = await runScript('bare', code, { env: env() });
  const spans = spansOf(new Set([run]));
  const by = Object.fromEntries(spans.map((s) => [s.name, s]));
  assert.equal(by['raw-inside'].attrs['gen_ai.conversation.id'], 'c-1');
  assert.equal(by['raw-inside'].attrs['user.id'], 'u-1');
  assert.equal(by['raw-inside'].attrs['flashcat.tag.tenant'], 'a');
  assert.equal(by['raw-inside'].attrs['flashcat.tag.n'], '1');
  assert.equal(by['has-conv'].attrs['gen_ai.conversation.id'], 'own', 'Preserve the existing conversation ID');
  assert.equal(by.nested.attrs['flashcat.tag.tenant'], 'a');
  assert.equal(by.nested.attrs['flashcat.tag.step'], 's2');
  assert.equal(by.nested.attrs['gen_ai.conversation.id'], 'c-1');
  assert.equal(by['raw-outside'].attrs['gen_ai.conversation.id'], undefined, 'Spans outside the session scope omit the conversation ID');
  assert.equal(by['raw-outside'].attrs['user.id'], undefined);
  assert.equal(result.res.baggage, null, 'Baggage is off by default');
  assert.deepEqual(result.res.propagated, { conv: 'c-2', user: 'u-2' });
  assert.equal(by.child.attrs['gen_ai.conversation.id'], 'p-1');
  assert.equal(by.child.attrs['user.id'], 'pu');
  assert.equal(by.child.attrs['flashcat.tag.t'], 'x');
  assert.equal(result.diag.semantics.orphan_inference_spans, 1);
});

test('A tracer failure runs the original function and preserves its return value or exception', async () => {
  const code = `
import { trace } from '@opentelemetry/api';
trace.setGlobalTracerProvider({ getTracer() { throw new Error('broken tracer'); } });
const otel = await import('@flashcat/otel');
const d = otel.init({ serviceName: 'x', exporter: 'none', metrics: false });
const res = {};
res.sync = otel.agent('a', (x) => x + 1)(1);
res.async = await otel.tool('t', async (x) => x * 2)(21);
const err = new TypeError('biz');
try { otel.workflow('w', () => { throw err; })(); } catch (e) { res.same = e === err; }
res.trace = otel.trace('job', (span) => typeof span.setAttribute);
res.session = otel.session({ conversationId: 'c' }, () => 'ok');
res.situation = d.provider.situation;
console.log('RESULT:' + JSON.stringify(res));
`;
  const { result } = await runScript('bare', code);
  assert.deepEqual(result, { sync: 2, async: 42, same: true, trace: 'function', session: 'ok', situation: 'B' });
});
