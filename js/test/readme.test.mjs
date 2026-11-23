// Run README code blocks marked <!-- snippet:xxx --> verbatim because the onboarding page copies them directly.
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { appDir, ROOT, runScript, startCollector, startFakeOpenAI } from './helpers.mjs';

const README = readFileSync(join(ROOT, 'README.md'), 'utf8');

export function snippet(name) {
  const re = new RegExp(`<!-- snippet:${name} -->\\s*\\n\`\`\`[a-z]*\\n([\\s\\S]*?)\\n\`\`\``);
  const m = re.exec(README);
  if (!m) throw new Error(`README 里没有 snippet:${name}`);
  return m[1];
}

/** Parse a Bash snippet into environment variables from export lines and arguments from the node line. */
function parseBash(text) {
  const env = {};
  let argv;
  for (const raw of text.split('\n')) {
    const line = raw.replace(/\s+#.*$/, '').trim();
    if (!line) continue;
    const ex = /^export\s+([A-Z_]+)=(.*)$/.exec(line);
    if (ex) env[ex[1]] = ex[2];
    else if (line.startsWith('node ')) argv = line.split(/\s+/).slice(1);
  }
  return { env, argv };
}

let openai;
before(async () => {
  openai = await startFakeOpenAI();
});
after(async () => {
  await openai.close();
});

const CHAT_APP_CJS = `
const otel = require('@flashcat/genai-otel');
const { OpenAI } = require('openai');
(async () => {
  const client = new OpenAI({ baseURL: process.env.FAKE_OPENAI, apiKey: 'sk-test' });
  const answer = otel.agent('kefu', async (q) => (await client.chat.completions.create({ model: 'gpt-4o', messages: [{ role: 'user', content: q }] })).choices[0].message.content);
  await otel.session({ conversationId: 'c-readme' }, () => answer('x'));
  await otel.flush();
  console.log('RESULT:' + JSON.stringify(otel.diagnostics()));
})();
`;

const CHAT_APP_ESM = `
import * as otel from '@flashcat/genai-otel';
import OpenAI from 'openai';
const client = new OpenAI({ baseURL: process.env.FAKE_OPENAI, apiKey: 'sk-test' });
const answer = otel.agent('kefu', async (q) => (await client.chat.completions.create({ model: 'gpt-4o', messages: [{ role: 'user', content: q }] })).choices[0].message.content);
await otel.session({ conversationId: 'c-readme' }, () => answer('x'));
await otel.flush();
console.log('RESULT:' + JSON.stringify(otel.diagnostics()));
`;

function assertChatUnderAgent(c, d) {
  assert.deepEqual(d.problems, []);
  const spans = c.spans();
  const chat = spans.find((s) => s.attrs['gen_ai.operation.name'] === 'chat');
  const ag = spans.find((s) => s.attrs['gen_ai.operation.name'] === 'invoke_agent');
  assert.ok(chat && ag, spans.map((s) => s.name).join(','));
  assert.equal(chat.parent, ag.spanId);
  assert.equal(chat.attrs['gen_ai.conversation.id'], 'c-readme');
  assert.equal(chat.resource['service.name'], 'kefu-agent');
}

test('README zero-code startup uses environment variables and node --import @flashcat/genai-otel/register app.js with a CommonJS app', async () => {
  const c = await startCollector();
  try {
    const { env, argv } = parseBash(snippet('zero-code'));
    assert.deepEqual(argv, ['--import', '@flashcat/genai-otel/register', 'app.js']);
    env.FLASHCAT_ENDPOINT = c.url + '/v1/traces'; // Use the local mock collector.
    const { result } = await runScript('main', CHAT_APP_CJS, { ext: 'js', nodeArgs: argv.slice(0, -1), env: { ...env, FAKE_OPENAI: openai.url } });
    assertChatUnderAgent(c, result);
    assert.equal(c.requests[0].headers.authorization, 'Basic ' + Buffer.from('keyID:secret').toString('base64'));
  } finally {
    await c.close();
  }
});

test('README code initialization snippet for CommonJS', async () => {
  const c = await startCollector();
  try {
    const code = `${snippet('init-cjs')}
(async () => {
  const client = new OpenAI({ baseURL: process.env.FAKE_OPENAI, apiKey: 'sk-test' });
  const answer = otel.agent('kefu', async (q) => (await client.chat.completions.create({ model: 'gpt-4o', messages: [{ role: 'user', content: q }] })).choices[0].message.content);
  await otel.session({ conversationId: 'c-readme' }, () => answer('x'));
  await otel.flush();
  console.log('RESULT:' + JSON.stringify(otel.diagnostics()));
})();
`;
    const { result } = await runScript('main', code, {
      ext: 'cjs',
      env: { FLASHCAT_ENDPOINT: c.url + '/v1/traces', FLASHCAT_API_KEY: 'k:s', FAKE_OPENAI: openai.url },
    });
    assertChatUnderAgent(c, result);
  } finally {
    await c.close();
  }
});

test('README code initialization snippet for ESM using instrumentation.mjs and node --import ./instrumentation.mjs app.mjs', async () => {
  const c = await startCollector();
  try {
    const dir = appDir('main');
    const instr = join(dir, 'scripts', `instrumentation-esm-${Date.now()}.mjs`);
    mkdirSync(join(dir, 'scripts'), { recursive: true });
    writeFileSync(instr, snippet('init-esm'));
    const { argv } = parseBash(snippet('init-esm-run'));
    assert.deepEqual(argv, ['--import', './instrumentation.mjs', 'app.mjs']);
    const { result } = await runScript('main', CHAT_APP_ESM, {
      nodeArgs: ['--import', instr],
      env: { FLASHCAT_ENDPOINT: c.url + '/v1/traces', FLASHCAT_API_KEY: 'k:s', FAKE_OPENAI: openai.url },
    });
    assertChatUnderAgent(c, result);
  } finally {
    await c.close();
  }
});

test('README semantic API snippet', async () => {
  const c = await startCollector();
  try {
    await runScript('main', snippet('semantic'), {
      nodeArgs: ['--import', '@flashcat/genai-otel/register'],
      env: { OTEL_SERVICE_NAME: 'kefu-agent', FLASHCAT_ENDPOINT: c.url + '/v1/traces', FLASHCAT_API_KEY: 'k:s' },
    });
    const spans = c.spans();
    const ag = spans.find((s) => s.name === 'invoke_agent kefu');
    const tl = spans.find((s) => s.name === 'execute_tool search_kb');
    assert.ok(ag && tl);
    assert.equal(tl.parent, ag.spanId);
    for (const s of [ag, tl]) {
      assert.equal(s.attrs['gen_ai.conversation.id'], 'c-123');
      assert.equal(s.attrs['user.id'], 'u-9');
      assert.equal(s.attrs['flashcat.tag.tenant'], 'a');
    }
  } finally {
    await c.close();
  }
});

test('README snippet for existing OpenTelemetry applications using NodeSDK, spanProcessor(), and reuse mode', async () => {
  const c = await startCollector();
  try {
    const dir = appDir('main');
    const instr = join(dir, 'scripts', `instrumentation-otel-${Date.now()}.mjs`);
    writeFileSync(instr, snippet('existing-otel'));
    const { result } = await runScript('main', CHAT_APP_ESM, {
      nodeArgs: ['--import', instr],
      env: { OTEL_METRICS_EXPORTER: 'none', OTEL_LOGS_EXPORTER: 'none', FLASHCAT_ENDPOINT: c.url + '/v1/traces', FLASHCAT_API_KEY: 'k:s', FAKE_OPENAI: openai.url },
    });
    assertChatUnderAgent(c, result);
    assert.equal(result.provider.situation, 'B');
    assert.equal(result.provider.exporter_mode, 'reuse');
  } finally {
    await c.close();
  }
});
