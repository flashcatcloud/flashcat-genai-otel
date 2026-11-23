// The catalog selects instrumentation by installed version. Cover conflicts, preloaded libraries, missing packages, and disabled entries.
// Point all model SDKs at local test servers.
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { runScript, startCollector, startFakeAnthropic, startFakeOpenAI } from './helpers.mjs';

let openai;
let anthropic;
before(async () => {
  openai = await startFakeOpenAI();
  anthropic = await startFakeAnthropic();
});
after(async () => {
  await openai.close();
  await anthropic.close();
});

const inst = (d, lib) => d.instrumentations.find((i) => i.lib === lib);

async function withCollector(fn) {
  const c = await startCollector();
  try {
    return await fn(c, { FLASHCAT_ENDPOINT: c.url + '/v1/traces', FLASHCAT_API_KEY: 'k:s', FAKE_OPENAI: openai.url, FAKE_ANTHROPIC: anthropic.url });
  } finally {
    await c.close();
  }
}

test('Anthropic 1.8.0 uses native instrumentation in metadata_only mode and emits a child of the agent with session attributes', () =>
  withCollector(async (c, env) => {
    const code = `
import * as otel from '@flashcat/genai-otel';
const d = otel.init({ serviceName: 'x', metrics: false });
const { default: Anthropic } = await import('@anthropic-ai/sdk');
const client = new Anthropic({ baseURL: process.env.FAKE_ANTHROPIC, apiKey: 'sk-ant-test' });   // Construct the client after init.
const answer = otel.agent('kefu', async (q) => (await client.messages.create({ model: 'claude-test-1', max_tokens: 64, messages: [{ role: 'user', content: q }] })).content[0].text);
const out = await otel.session({ conversationId: 'c-an', userId: 'u-an' }, () => answer('手机13812345678'));
await otel.flush();
console.log('RESULT:' + JSON.stringify({ out, d }));
`;
    const { result } = await runScript('main', code, { env });
    assert.equal(result.out, '您的订单已发货');
    assert.deepEqual(inst(result.d, 'anthropic'), { lib: 'anthropic', source: 'native', status: 'installed', lib_version: '1.8.0' });
    assert.equal(result.d.content.anthropic, 'metadata_only');
    const spans = c.spans();
    const ag = spans.find((s) => s.attrs['gen_ai.operation.name'] === 'invoke_agent');
    const sdkSpan = spans.find((s) => s.scope !== 'flashcat.genai');
    assert.ok(sdkSpan, spans.map((s) => s.scope + ':' + s.name).join(','));
    assert.equal(sdkSpan.parent, ag.spanId);
    assert.equal(sdkSpan.attrs['gen_ai.conversation.id'], 'c-an', 'Every span in the session scope gets session attributes even without gen_ai.operation.name at start');
    assert.equal(sdkSpan.attrs['user.id'], 'u-an');
    assert.equal(sdkSpan.attrs['gen_ai.usage.input_tokens'], 40);
    assert.ok(!JSON.stringify(sdkSpan.attrs).includes('13812345678'), 'metadata_only omits content');
  }));

test('Anthropic 0.133.0 uses upstream instrumentation and emits a child of the agent with usage added by the SDK and no content', () =>
  withCollector(async (c, env) => {
    const code = `
const otel = require('@flashcat/genai-otel');
const d = otel.init({ serviceName: 'x', metrics: false, captureContent: 'span' });
const { Anthropic } = require('@anthropic-ai/sdk');
(async () => {
  const client = new Anthropic({ baseURL: process.env.FAKE_ANTHROPIC, apiKey: 'sk-ant-test' });
  const answer = otel.agent('kefu', async (q) => (await client.messages.create({ model: 'claude-test-1', max_tokens: 64, messages: [{ role: 'user', content: q }] })).content[0].text);
  const out = await otel.session({ conversationId: 'c-old' }, () => answer('hi'));
  await otel.flush();
  console.log('RESULT:' + JSON.stringify({ out, d }));
})();
`;
    const { result } = await runScript('anth', code, { ext: 'cjs', env });
    assert.equal(result.out, '您的订单已发货');
    assert.deepEqual(inst(result.d, 'anthropic'), { lib: 'anthropic', source: 'upstream', status: 'installed', lib_version: '0.133.0' });
    assert.ok(result.d.notes.some((n) => n.includes('instrumentation-anthropic')), 'With capture on, diagnostics explain that this source omits content');
    const spans = c.spans();
    const ag = spans.find((s) => s.attrs['gen_ai.operation.name'] === 'invoke_agent');
    const chat = spans.find((s) => s.scope === '@opentelemetry/instrumentation-anthropic');
    assert.ok(chat, spans.map((s) => s.scope + ':' + s.name).join(','));
    assert.equal(chat.attrs['gen_ai.operation.name'], 'chat');
    assert.equal(chat.parent, ag.spanId);
    assert.equal(chat.attrs['gen_ai.conversation.id'], 'c-old');
    assert.equal(chat.attrs['gen_ai.usage.input_tokens'], 40);
    assert.equal(chat.attrs['gen_ai.usage.output_tokens'], 7);
    assert.equal(chat.attrs['gen_ai.response.model'], 'claude-test-1');
    assert.equal(chat.attrs['gen_ai.response.id'], 'msg_test');
    assert.deepEqual(chat.attrs['gen_ai.response.finish_reasons'], ['end_turn']);
    assert.equal(chat.attrs['gen_ai.usage.cache_read.input_tokens'], undefined, 'Omit cache usage when there are no cache hits');
    assert.equal(chat.attrs['gen_ai.input.messages'], undefined, 'This source omits content');
  }));

test('Anthropic 0.133.0 records usage for cache hits, streaming, messages.stream(), and early cancellation', () =>
  withCollector(async (c, env) => {
    const code = `
const otel = require('@flashcat/genai-otel');
otel.init({ serviceName: 'x', metrics: false });
const { Anthropic } = require('@anthropic-ai/sdk');
(async () => {
  const client = new Anthropic({ baseURL: process.env.FAKE_ANTHROPIC, apiKey: 'sk-ant-test' });
  const req = (model, extra = {}) => ({ model, max_tokens: 64, messages: [{ role: 'user', content: 'hi' }], ...extra });
  const out = {};
  await otel.trace('cache', () => client.messages.create(req('claude-cache-1')));
  await otel.trace('stream', async () => {
    let text = '';
    for await (const ev of await client.messages.create(req('claude-cache-1', { stream: true }))) {
      if (ev.type === 'content_block_delta') text += ev.delta.text;
    }
    out.streamText = text;
  });
  await otel.trace('helper', async () => {
    const final = await client.messages.stream(req('claude-test-1')).finalMessage();
    out.helperText = final.content[0].text;
  });
  await otel.trace('early', async () => {
    for await (const ev of await client.messages.create(req('claude-test-1', { stream: true }))) {
      if (ev.type === 'message_start') break;
    }
  });
  await otel.flush();
  console.log('RESULT:' + JSON.stringify(out));
})();
`;
    const { result } = await runScript('anth', code, { ext: 'cjs', env });
    assert.deepEqual(result, { streamText: '您的订单已发货', helperText: '您的订单已发货' });
    const spans = c.spans();
    const chatUnder = (root) => {
      const r = spans.find((s) => s.name === root);
      const found = spans.filter((s) => s.parent === r.spanId && s.scope === '@opentelemetry/instrumentation-anthropic');
      assert.equal(found.length, 1, root + ': ' + spans.map((s) => s.scope + ':' + s.name).join(','));
      return found[0].attrs;
    };
    const cache = chatUnder('cache');
    assert.equal(cache['gen_ai.usage.input_tokens'], 160, 'input_tokens includes cached input: 40 + 100 + 20');
    assert.equal(cache['gen_ai.usage.cache_read.input_tokens'], 100);
    assert.equal(cache['gen_ai.usage.cache_creation.input_tokens'], 20);
    assert.equal(cache['gen_ai.usage.output_tokens'], 7);
    assert.equal(cache['gen_ai.response.model'], 'claude-cache-1-20261101');
    const stream = chatUnder('stream');
    assert.equal(stream['gen_ai.usage.input_tokens'], 160);
    assert.equal(stream['gen_ai.usage.output_tokens'], 7, 'Cumulative message_delta usage replaces the value of 1 from message_start');
    assert.deepEqual(stream['gen_ai.response.finish_reasons'], ['end_turn']);
    assert.equal(stream['gen_ai.response.id'], 'msg_stream');
    const helper = chatUnder('helper');
    assert.equal(helper['gen_ai.usage.input_tokens'], 40);
    assert.equal(helper['gen_ai.usage.output_tokens'], 7);
    const early = chatUnder('early');
    assert.equal(early['gen_ai.usage.input_tokens'], 40, 'Input usage from message_start is recorded before early cancellation');
  }));

for (const mode of ['cjs', 'esm']) {
  test(`openai 7.55.0 uses OpenInference (${mode === 'cjs' ? 'CommonJS' : 'ESM with the register manuallyInstrument fallback'})`, () =>
    withCollector(async (c, env) => {
      const body = `
const client = new OpenAI({ baseURL: process.env.FAKE_OPENAI, apiKey: 'sk-test' });
const answer = otel.agent('kefu', async (q) => (await client.chat.completions.create({ model: 'gpt-4o', messages: [{ role: 'user', content: q }] })).choices[0].message.content);
const out = await otel.session({ conversationId: 'c-oi' }, () => answer('手机13812345678'));
await otel.flush();
console.log('RESULT:' + JSON.stringify({ out, d: otel.diagnostics() }));
`;
      const code =
        mode === 'cjs'
          ? `const otel = require('@flashcat/genai-otel');\notel.init({ serviceName: 'x', metrics: false });\nconst { OpenAI } = require('openai');\n(async () => {${body}})();`
          : `import * as otel from '@flashcat/genai-otel';\nimport OpenAI from 'openai';\n${body}`;
      const { result } = await runScript('oi', code, {
        ext: mode === 'cjs' ? 'cjs' : 'mjs',
        nodeArgs: mode === 'esm' ? ['--import', '@flashcat/genai-otel/register'] : [],
        env: { ...env, OTEL_SERVICE_NAME: 'x' },
      });
      assert.equal(result.out, '您的订单已发货');
      assert.deepEqual(inst(result.d, 'openai'), { lib: 'openai', source: 'openinference', status: 'installed', lib_version: '7.55.0' });
      const spans = c.spans();
      const ag = spans.find((s) => s.attrs['gen_ai.operation.name'] === 'invoke_agent');
      const llm = spans.find((s) => s.attrs['openinference.span.kind'] === 'LLM');
      assert.ok(llm, spans.map((s) => s.scope + ':' + s.name).join(','));
      assert.equal(llm.scope, '@arizeai/openinference-instrumentation-openai');
      assert.equal(llm.parent, ag.spanId);
      assert.equal(ag.attrs['flashcat.genai.root'], true);
      assert.equal(llm.attrs['flashcat.genai.root'], false);
      assert.equal(llm.attrs['gen_ai.conversation.id'], 'c-oi');
      assert.equal(llm.attrs['llm.token_count.prompt'], 120);
      assert.equal(llm.attrs['input.value'], '__REDACTED__', 'OPENINFERENCE_HIDE_INPUTS=true is honored');
    }));
}

for (const mode of ['cjs', 'esm']) {
  test(`OpenAI 7 streams (${mode}) record TTFT on the single OpenInference span and use its cache token key`, () =>
    withCollector(async (c, env) => {
      const body = `
const client = new OpenAI({ baseURL: process.env.FAKE_OPENAI, apiKey: 'sk-test' });
let text = '';
for await (const chunk of await client.chat.completions.create({ model: 'gpt-4o', messages: [{ role: 'user', content: 'x' }], stream: true, stream_options: { include_usage: true } })) {
  text += chunk.choices[0]?.delta?.content ?? '';
}
const plain = await client.chat.completions.create({ model: 'gpt-4o', messages: [{ role: 'user', content: 'x' }] });
await otel.shutdown();
console.log('RESULT:' + JSON.stringify({ text, plain: plain.choices[0].message.content }));
`;
      const code =
        mode === 'cjs'
          ? `const otel = require('@flashcat/genai-otel');\notel.init({ serviceName: 'x', metrics: false });\nconst { OpenAI } = require('openai');\n(async () => {${body}})();`
          : `import * as otel from '@flashcat/genai-otel';\nimport OpenAI from 'openai';\n${body}`;
      const { result } = await runScript('oi', code, {
        ext: mode === 'cjs' ? 'cjs' : 'mjs',
        nodeArgs: mode === 'esm' ? ['--import', '@flashcat/genai-otel/register'] : [],
        env: { ...env, OTEL_SERVICE_NAME: 'x', OTEL_METRICS_EXPORTER: 'none' },
      });
      assert.deepEqual(result, { text: '已发货', plain: '您的订单已发货' });
      const llm = c.spans().filter((s) => s.attrs['openinference.span.kind'] === 'LLM');
      assert.equal(llm.length, 2, llm.map((s) => s.name).join(','));
      // The mock server reports prompt_tokens=20 for streaming and 120 for non-streaming.
      const [streamed, plain] = [20, 120].map((n) => llm.find((s) => s.attrs['llm.token_count.prompt'] === n));
      assert.ok(streamed && plain, JSON.stringify(llm.map((s) => s.attrs)));
      const ttft = streamed.attrs['gen_ai.response.time_to_first_chunk'];
      assert.ok(typeof ttft === 'number' && ttft >= 0.025 && ttft < 2, `Time to first token: ${ttft} s`);
      assert.equal(streamed.attrs['llm.token_count.prompt_details.cache_read'], 8);
      assert.equal(streamed.attrs['gen_ai.usage.cache_read.input_tokens'], undefined, 'OpenInference already records cache tokens under its own key');
      assert.equal(plain.attrs['gen_ai.response.time_to_first_chunk'], undefined);
      assert.equal(plain.attrs['llm.token_count.prompt_details.cache_read'], 64);
    }));
}

const NOT_INSTALLED_PROBLEM =
  '检测到 openai 7.55.0，但没装对应的插桩包，这部分调用不会产生 span；请执行 npm install @arizeai/openinference-instrumentation-openai';

test('OpenAI 7 without OpenInference is instrumentation_not_installed, reports the install command, and warns once (contract §8)', async () => {
  const { result, stderr } = await runScript(
    'o7bare',
    `import * as otel from '@flashcat/genai-otel';\nconsole.log('RESULT:' + JSON.stringify(otel.init({ serviceName: 'x', exporter: 'none', metrics: false })));\notel.init();`,
  );
  assert.deepEqual(inst(result, 'openai'), {
    lib: 'openai',
    source: 'openinference',
    status: 'instrumentation_not_installed',
    lib_version: '7.55.0',
    hint: 'npm install @arizeai/openinference-instrumentation-openai',
  });
  assert.deepEqual(result.problems, [NOT_INSTALLED_PROBLEM]);
  const warns = stderr.split('\n').filter((l) => l.startsWith('[flashcat-genai-otel] WARN') && l.includes('openinference-instrumentation-openai'));
  assert.deepEqual(warns, [`[flashcat-genai-otel] WARN ${NOT_INSTALLED_PROBLEM}`], stderr);
  assert.match(stderr, /INFO .*"problems":\["检测到 openai 7\.55\.0/, 'The startup summary contains a nonempty problems array');
});

test('Zero-code --import reports the install command and warns once across ESM registration and the CJS application', async () => {
  const { result, stderr } = await runScript(
    'o7bare',
    `const otel = require('@flashcat/genai-otel');\notel.init();\nconsole.log('RESULT:' + JSON.stringify(otel.diagnostics()));`,
    { ext: 'cjs', nodeArgs: ['--import', '@flashcat/genai-otel/register'], env: { OTEL_SERVICE_NAME: 'x', OTEL_METRICS_EXPORTER: 'none' } },
  );
  assert.ok(result.problems.includes(NOT_INSTALLED_PROBLEM), result.problems.join('\n'));
  assert.equal(stderr.split('\n').filter((l) => l.includes('WARN') && l.includes('openinference-instrumentation-openai')).length, 1, stderr);
});

test('OpenAI Agents 0.21.0 uses upstream instrumentation and applies session attributes to agent runs (ESM via --import)', () =>
  withCollector(async (c, env) => {
    const code = `
// Intercept the Agents SDK's trace export to api.openai.com so the test stays local.
const realFetch = globalThis.fetch; const blocked = [];
globalThis.fetch = (u, o) => { const url = String(u?.url ?? u); if (!url.startsWith('http://127.0.0.1')) { blocked.push(url); return Promise.resolve(new Response('{}')); } return realFetch(u, o); };
const otel = await import('@flashcat/genai-otel');
const { Agent, run } = await import('@openai/agents');
const agent = new Agent({ name: 'kefu', instructions: '你是客服', model: 'gpt-4o' });
const r = await otel.session({ conversationId: 'c-ag' }, () => run(agent, '我的订单到哪了'));
await otel.flush();
console.log('RESULT:' + JSON.stringify({ out: r.finalOutput, d: otel.diagnostics() }));
`;
    const { result } = await runScript('main', code, {
      nodeArgs: ['--import', '@flashcat/genai-otel/register'],
      env: { ...env, OTEL_SERVICE_NAME: 'x', OPENAI_BASE_URL: openai.url, OPENAI_API_KEY: 'sk-test', OPENAI_AGENTS_DISABLE_TRACING: '0' },
    });
    assert.equal(result.out, '您的订单已发货');
    assert.deepEqual(inst(result.d, 'openai-agents'), { lib: 'openai-agents', source: 'upstream', status: 'installed', lib_version: '0.21.0' });
    const spans = c.spans().filter((s) => s.scope === '@opentelemetry/instrumentation-openai-agents');
    const ag = spans.find((s) => s.name === 'invoke_agent kefu');
    assert.ok(ag, c.spans().map((s) => s.scope + ':' + s.name).join(','));
    assert.equal(ag.attrs['gen_ai.agent.name'], 'kefu');
    assert.equal(ag.attrs['gen_ai.conversation.id'], 'c-ag');
  }));

for (const content of [false, 'span']) {
  test(`AI SDK 7 registers a native OpenTelemetry integration: ${content === false ? 'capture off forces recordInputs and recordOutputs to false' : 'capture on records and masks content'}`, () =>
    withCollector(async (c, env) => {
      const code = `
import * as otel from '@flashcat/genai-otel';
const d = otel.init({ serviceName: 'x', metrics: false, captureContent: ${JSON.stringify(content)} });
const { generateText } = await import('ai');
const { MockLanguageModelV4 } = await import('ai/test');
const model = new MockLanguageModelV4({ doGenerate: async () => ({
  content: [{ type: 'text', text: '订单已发货，邮箱 a@b.com' }],
  finishReason: { unified: 'stop', raw: 'stop' },
  usage: { inputTokens: { total: 10, noCache: 10, cacheRead: 0, cacheWrite: 0 }, outputTokens: { total: 5, text: 5, reasoning: 0 } },
  warnings: [],
}) });
const r = await otel.session({ conversationId: 'c-ai' }, () => otel.agent('kefu', () => generateText({ model, prompt: '我的手机13812345678' }))());
await otel.flush();
console.log('RESULT:' + JSON.stringify({ text: r.text, d }));
`;
      const { result } = await runScript('main', code, { env });
      assert.equal(result.text, '订单已发货，邮箱 a@b.com');
      assert.deepEqual(inst(result.d, 'ai'), { lib: 'ai', source: 'native', status: 'installed', lib_version: '7.0.170' });
      const spans = c.spans();
      const ag = spans.find((s) => s.scope === 'flashcat.genai');
      const aiSpans = spans.filter((s) => s.scope === 'gen_ai');
      assert.ok(aiSpans.length >= 1, spans.map((s) => s.scope + ':' + s.name).join(','));
      const root = aiSpans.find((s) => s.parent === ag.spanId);
      assert.ok(root, 'The AI SDK span is a child of the agent');
      for (const s of aiSpans) assert.equal(s.attrs['gen_ai.conversation.id'], 'c-ai');
      const all = JSON.stringify(aiSpans.map((s) => s.attrs));
      assert.ok(!all.includes('13812345678'), 'The phone number is absent');
      if (content === false) {
        assert.ok(!all.includes('订单已发货'), 'Capture off omits output content');
        for (const s of aiSpans) {
          assert.equal(s.attrs['gen_ai.input.messages'], undefined);
          assert.equal(s.attrs['gen_ai.output.messages'], undefined);
        }
      } else {
        assert.ok(aiSpans.some((s) => s.attrs['gen_ai.input.messages']?.includes('[REDACTED:phone]')), all);
        assert.ok(aiSpans.some((s) => s.attrs['gen_ai.output.messages']?.includes('[REDACTED:email]')), all);
      }
    }));
}

test('LangChain (@langchain/core 1.4.0) emits OpenInference spans in CommonJS', () =>
  withCollector(async (c, env) => {
    const code = `
const otel = require('@flashcat/genai-otel');
const d = otel.init({ serviceName: 'x', metrics: false });
const { FakeListChatModel } = require('@langchain/core/utils/testing');
(async () => {
  const model = new FakeListChatModel({ responses: ['您的订单已发货'] });
  const out = await otel.session({ conversationId: 'c-lc' }, () => otel.agent('kefu', async (q) => (await model.invoke(q)).content)('hi'));
  await otel.flush();
  console.log('RESULT:' + JSON.stringify({ out, d }));
})();
`;
    const { result } = await runScript('oi', code, { ext: 'cjs', env });
    assert.equal(result.out, '您的订单已发货');
    assert.deepEqual(inst(result.d, 'langchain'), { lib: 'langchain', source: 'openinference', status: 'installed', lib_version: '1.4.0' });
    const spans = c.spans();
    const ag = spans.find((s) => s.scope === 'flashcat.genai');
    const lc = spans.find((s) => s.scope.includes('openinference-instrumentation-langchain'));
    assert.ok(lc, spans.map((s) => s.scope + ':' + s.name).join(','));
    assert.equal(lc.parent, ag.spanId);
    assert.equal(lc.attrs['gen_ai.conversation.id'], 'c-lc');
  }));

test('A foreign shimmer wrapper reports skipped_conflict by default, and prefer=flashcat installs instrumentation anyway', async () => {
  const pre = `
const otel = require('@flashcat/genai-otel');
const { OpenAI } = require('openai');
const proto = OpenAI.Chat.Completions.prototype;
const orig = proto.create;
const wrapped = function (...a) { return orig.apply(this, a); };
Object.defineProperty(wrapped, '__wrapped', { value: true });
proto.create = wrapped;
`;
  let { result } = await runScript('main', pre + `console.log('RESULT:' + JSON.stringify(otel.init({ serviceName: 'x', exporter: 'none', metrics: false })));`, { ext: 'cjs' });
  assert.equal(inst(result, 'openai').status, 'skipped_conflict');
  assert.deepEqual(inst(result, 'openai').foreign, ['OpenAI.Chat.Completions.prototype.create.__wrapped']);
  ({ result } = await runScript('main', pre + `console.log('RESULT:' + JSON.stringify(otel.init({ serviceName: 'x', exporter: 'none', metrics: false, prefer: 'flashcat' })));`, { ext: 'cjs' }));
  assert.equal(inst(result, 'openai').status, 'loaded_before_init');
});

test('A CommonJS target required before init reports loaded_before_init and suggests --import', async () => {
  const code = `
const { OpenAI } = require('openai');
const otel = require('@flashcat/genai-otel');
console.log('RESULT:' + JSON.stringify(otel.init({ serviceName: 'x', exporter: 'none', metrics: false })));
`;
  const { result } = await runScript('main', code, { ext: 'cjs' });
  const i = inst(result, 'openai');
  assert.equal(i.status, 'loaded_before_init');
  assert.match(i.hint, /--import @flashcat\/genai-otel\/register/);
});

test('instrumentations / disabledInstrumentations / OTEL_NODE_DISABLED_INSTRUMENTATIONS', async () => {
  let { result } = await runScript('main', `import * as otel from '@flashcat/genai-otel';\nconsole.log('RESULT:' + JSON.stringify(otel.init({ serviceName: 'x', exporter: 'none', metrics: false, disabledInstrumentations: ['openai'] })));`, {
    env: { OTEL_NODE_DISABLED_INSTRUMENTATIONS: 'anthropic' },
  });
  assert.equal(inst(result, 'openai').status, 'disabled');
  assert.equal(inst(result, 'anthropic').status, 'disabled');
  assert.equal(inst(result, 'ai').status, 'installed');
  assert.equal(inst(result, 'bedrock').status, 'not_present');
  ({ result } = await runScript('main', `import * as otel from '@flashcat/genai-otel';\nconsole.log('RESULT:' + JSON.stringify(otel.init({ serviceName: 'x', exporter: 'none', metrics: false, instrumentations: ['openai'] })));`));
  assert.deepEqual(result.instrumentations.map((i) => i.lib), ['openai']);
});

test('LangChain in ESM uses the register manuallyInstrument fallback', () =>
  withCollector(async (c, env) => {
    const code = `
import * as otel from '@flashcat/genai-otel';
import { FakeListChatModel } from '@langchain/core/utils/testing';
const model = new FakeListChatModel({ responses: ['您的订单已发货'] });
const out = await otel.session({ conversationId: 'c-lc-esm' }, () => otel.agent('kefu', async (q) => (await model.invoke(q)).content)('hi'));
await otel.flush();
console.log('RESULT:' + JSON.stringify({ out, d: otel.diagnostics() }));
`;
    const { result } = await runScript('oi', code, { nodeArgs: ['--import', '@flashcat/genai-otel/register'], env: { ...env, OTEL_SERVICE_NAME: 'x' } });
    assert.equal(result.out, '您的订单已发货');
    assert.equal(inst(result.d, 'langchain').status, 'installed');
    const spans = c.spans();
    const ag = spans.find((s) => s.scope === 'flashcat.genai');
    const lc = spans.find((s) => s.scope.includes('openinference-instrumentation-langchain'));
    assert.ok(lc, spans.map((s) => s.scope + ':' + s.name).join(','));
    assert.equal(lc.parent, ag.spanId);
    assert.equal(lc.attrs['gen_ai.conversation.id'], 'c-lc-esm');
  }));

test('Bedrock ConverseCommand emits a chat span (client 3.1167.0 and instrumentation 0.80.0)', () =>
  withCollector(async (c, env) => {
    // Mock the Bedrock Runtime Converse endpoint.
    const { createServer } = await import('node:http');
    const srv = createServer((req, res) => {
      req.resume();
      req.on('end', () => {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({
          output: { message: { role: 'assistant', content: [{ text: '您的订单已发货' }] } },
          stopReason: 'end_turn',
          usage: { inputTokens: 33, outputTokens: 7, totalTokens: 40 },
          metrics: { latencyMs: 5 },
        }));
      });
    });
    await new Promise((r) => srv.listen(0, '127.0.0.1', r));
    try {
      const code = `
const otel = require('@flashcat/genai-otel');
const d = otel.init({ serviceName: 'x', metrics: false });
const { BedrockRuntimeClient, ConverseCommand } = require('@aws-sdk/client-bedrock-runtime');
const { NodeHttpHandler } = require('@smithy/node-http-handler');   // The mock server speaks HTTP/1.1 only. The Bedrock Runtime client defaults to HTTP/2.
(async () => {
  const client = new BedrockRuntimeClient({ region: 'us-east-1', endpoint: process.env.FAKE_BEDROCK, requestHandler: new NodeHttpHandler(),
    credentials: { accessKeyId: 'AKIDTEST', secretAccessKey: 'secret' } });
  const answer = otel.agent('kefu', async (q) => (await client.send(new ConverseCommand({ modelId: 'anthropic.claude-test', messages: [{ role: 'user', content: [{ text: q }] }] }))).output.message.content[0].text);
  const out = await otel.session({ conversationId: 'c-br' }, () => answer('hi'));
  await otel.flush();
  console.log('RESULT:' + JSON.stringify({ out, d }));
})();
`;
      const { result } = await runScript('bedrock', code, { ext: 'cjs', env: { ...env, FAKE_BEDROCK: `http://127.0.0.1:${srv.address().port}` } });
      assert.equal(result.out, '您的订单已发货');
      assert.equal(inst(result.d, 'bedrock').status, 'installed');
      assert.equal(inst(result.d, 'bedrock').source, 'upstream');
      const spans = c.spans();
      const ag = spans.find((s) => s.scope === 'flashcat.genai');
      const chat = spans.find((s) => s.attrs['gen_ai.operation.name'] === 'chat');
      assert.ok(chat, spans.map((s) => s.scope + ':' + s.name).join(','));
      assert.equal(chat.parent, ag.spanId);
      assert.equal(chat.attrs['gen_ai.usage.input_tokens'], 33);
      assert.equal(chat.attrs['gen_ai.conversation.id'], 'c-br');
    } finally {
      await new Promise((r) => srv.close(r));
    }
  }));

test('Bedrock reports not_present when the client is absent', async () => {
  const { result } = await runScript('main', `import * as otel from '@flashcat/genai-otel';\nconsole.log('RESULT:' + JSON.stringify(otel.init({ serviceName: 'x', exporter: 'none', metrics: false })));`);
  assert.equal(inst(result, 'bedrock').status, 'not_present');
  assert.equal(inst(result, 'bedrock').source, 'upstream');
});
