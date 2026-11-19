// Cover @google/genai calls, streaming, and embeddings, with cache-inclusive input usage, reasoning-inclusive output usage, content masking, CommonJS, and ESM register.
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { runScript, startCollector } from './helpers.mjs';

let c;
let gemini;
before(async () => {
  c = await startCollector();
  gemini = await startFakeGemini();
});
after(async () => {
  await c.close();
  await gemini.close();
});

const USAGE = { promptTokenCount: 30, candidatesTokenCount: 5, cachedContentTokenCount: 10, thoughtsTokenCount: 3, totalTokenCount: 38 };

function startFakeGemini() {
  const srv = createServer((req, res) => {
    let body = '';
    req.on('data', (d) => (body += d));
    req.on('end', () => {
      if (req.url.includes(':batchEmbedContents')) {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ embeddings: [{ values: [0.1, 0.2] }] }));
        return;
      }
      if (req.url.includes(':embedContent')) {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ embedding: { values: [0.1, 0.2] } }));
        return;
      }
      if (req.url.includes(':streamGenerateContent')) {
        const chunk = (o) => `data: ${JSON.stringify(o)}\r\n\r\n`;
        res.writeHead(200, { 'Content-Type': 'text/event-stream' });
        res.write(chunk({ candidates: [{ index: 0, content: { role: 'model', parts: [{ text: '您的订单' }] } }], modelVersion: 'gemini-test-001', responseId: 'resp-s' }));
        setTimeout(() => {
          res.end(
            chunk({
              candidates: [{ index: 0, content: { role: 'model', parts: [{ text: '已发货' }] }, finishReason: 'STOP' }],
              usageMetadata: USAGE,
              modelVersion: 'gemini-test-001',
              responseId: 'resp-s',
            }),
          );
        }, 20);
        return;
      }
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(
        JSON.stringify({
          candidates: [{ index: 0, content: { role: 'model', parts: [{ text: '您的订单已发货' }] }, finishReason: 'STOP' }],
          usageMetadata: USAGE,
          modelVersion: 'gemini-test-001',
          responseId: 'resp-g',
        }),
      );
    });
  });
  return new Promise((r) => srv.listen(0, '127.0.0.1', () => r({ url: `http://127.0.0.1:${srv.address().port}`, close: () => new Promise((x) => srv.close(x)) })));
}

function env(extra = {}) {
  return { FLASHCAT_ENDPOINT: c.url + '/v1/traces', FLASHCAT_API_KEY: 'k:s', FAKE_GEMINI: gemini.url, ...extra };
}

const BODY = `
const ai = new GoogleGenAI({ apiKey: 'test-key', httpOptions: { baseUrl: process.env.FAKE_GEMINI } });
const out = {};
const r = await otel.agent('kefu', () => ai.models.generateContent({
  model: 'gemini-test',
  contents: '手机13812345678的订单到哪了',
  config: { systemInstruction: '你是客服' },
}))();
out.text = r.text;
let streamed = '';
for await (const chunk of await ai.models.generateContentStream({ model: 'gemini-test', contents: [{ role: 'user', parts: [{ text: '查订单' }] }] })) {
  streamed += chunk.text ?? '';
}
out.streamed = streamed;
const e = await ai.models.embedContent({ model: 'text-embedding-004', contents: 'hello' });
out.dims = e.embeddings?.[0]?.values?.length ?? e.embedding?.values?.length;
await otel.flush();
console.log('RESULT:' + JSON.stringify({ ...out, d: otel.diagnostics() }));
`;

function check(result, run) {
  assert.deepEqual({ text: result.text, streamed: result.streamed, dims: result.dims }, { text: '您的订单已发货', streamed: '您的订单已发货', dims: 2 });
  const item = result.d.instrumentations.find((i) => i.lib === 'google-genai');
  assert.deepEqual({ source: item.source, status: item.status }, { source: 'flashcat', status: 'installed' });
  const spans = c.spans().filter((s) => s.resource['test.run'] === run);
  const gen = spans.filter((s) => s.attrs['gen_ai.operation.name'] === 'generate_content');
  assert.equal(gen.length, 2, spans.map((s) => s.name).join(','));
  const [plain, stream] = ['resp-g', 'resp-s'].map((id) => gen.find((s) => s.attrs['gen_ai.response.id'] === id));
  const agent = spans.find((s) => s.attrs['gen_ai.operation.name'] === 'invoke_agent');
  for (const s of [plain, stream]) {
    assert.equal(s.name, 'generate_content gemini-test');
    assert.equal(s.kind, 3, 'CLIENT');
    assert.equal(s.scope, 'flashcat.instrumentation.google-genai');
    assert.equal(s.attrs['gen_ai.provider.name'], 'gcp.gemini');
    assert.equal(s.attrs['gen_ai.request.model'], 'gemini-test');
    assert.equal(s.attrs['gen_ai.response.model'], 'gemini-test-001');
    assert.equal(s.attrs['gen_ai.usage.input_tokens'], 30, 'promptTokenCount already includes cached tokens');
    assert.equal(s.attrs['gen_ai.usage.output_tokens'], 8, 'candidates 5 + thoughts 3');
    assert.equal(s.attrs['gen_ai.usage.reasoning.output_tokens'], 3);
    assert.equal(s.attrs['gen_ai.usage.cache_read.input_tokens'], 10);
    assert.deepEqual(s.attrs['gen_ai.response.finish_reasons'], ['STOP']);
  }
  assert.equal(plain.parent, agent.spanId);
  const input = JSON.parse(plain.attrs['gen_ai.input.messages']);
  assert.deepEqual(input.map((m) => m.role), ['system', 'user']);
  assert.ok(input[1].parts[0].content.includes('[REDACTED:phone]'), 'Input is masked');
  assert.equal(JSON.parse(plain.attrs['gen_ai.output.messages'])[0].parts[0].content, '您的订单已发货');
  assert.equal(stream.attrs['gen_ai.request.stream'], true);
  assert.ok(stream.attrs['gen_ai.response.time_to_first_chunk'] >= 0, 'Streaming records time to first token');
  assert.equal(JSON.parse(stream.attrs['gen_ai.output.messages'])[0].parts[0].content, '您的订单已发货', 'Streaming text is assembled and written to the span');
  const emb = spans.find((s) => s.attrs['gen_ai.operation.name'] === 'embeddings');
  assert.equal(emb.name, 'embeddings text-embedding-004');
  assert.equal(emb.attrs['gen_ai.provider.name'], 'gcp.gemini');
}

test('Google GenAI with CommonJS code initialization', async () => {
  const run = 'genai-cjs';
  const code = `
const otel = require('@flashcat/otel');
otel.init({ serviceName: 'x', metrics: false, captureContent: 'span', resourceAttributes: { 'test.run': '${run}' } });
const { GoogleGenAI } = require('@google/genai');
(async () => {
${BODY}
})();
`;
  const { result } = await runScript('genai', code, { ext: 'cjs', env: env() });
  check(result, run);
});

test('Google GenAI with ESM and node --import @flashcat/otel/register', async () => {
  const run = 'genai-esm';
  const code = `import * as otel from '@flashcat/otel';
import { GoogleGenAI } from '@google/genai';
${BODY}`;
  const { result } = await runScript('genai', code, {
    nodeArgs: ['--import', '@flashcat/otel/register'],
    env: env({ OTEL_SERVICE_NAME: 'x', OTEL_METRICS_EXPORTER: 'none', FLASHCAT_CAPTURE_CONTENT: 'span', OTEL_RESOURCE_ATTRIBUTES: `test.run=${run}` }),
  });
  check(result, run);
});

test('Google GenAI still records usage with message capture off', async () => {
  const run = 'genai-off';
  const code = `
const otel = require('@flashcat/otel');
otel.init({ serviceName: 'x', metrics: false, resourceAttributes: { 'test.run': '${run}' } });
const { GoogleGenAI } = require('@google/genai');
(async () => {
  const ai = new GoogleGenAI({ apiKey: 'test-key', httpOptions: { baseUrl: process.env.FAKE_GEMINI } });
  await ai.models.generateContent({ model: 'gemini-test', contents: 'secret' });
  await otel.flush();
  console.log('RESULT:{}');
})();
`;
  await runScript('genai', code, { ext: 'cjs', env: env() });
  const s = c.spans().find((x) => x.resource['test.run'] === run && x.attrs['gen_ai.operation.name'] === 'generate_content');
  assert.equal(s.attrs['gen_ai.input.messages'], undefined);
  assert.equal(s.attrs['gen_ai.output.messages'], undefined);
  assert.equal(s.attrs['gen_ai.usage.input_tokens'], 30);
});
