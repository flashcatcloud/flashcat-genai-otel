// Masking and truncation (contract §6): run all spec/fixtures/masking.json cases through the full export pipeline and compare the default rules exactly.
// L1 preserves gen_ai.usage.input_tokens. Oversized values are truncated at a valid UTF-8 boundary and marked.
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { ROOT, runScript, startCollector } from './helpers.mjs';

const fixture = JSON.parse(readFileSync(join(ROOT, '..', 'spec', 'fixtures', 'masking.json'), 'utf8'));

let c;
before(async () => {
  c = await startCollector();
});
after(async () => {
  await c.close();
});

test('The source matches the fixture exactly for default rules, key regex, content keys, and limits', () => {
  const src = readFileSync(join(ROOT, 'src', 'processors.ts'), 'utf8');
  const lit = (s) => `'${s.replace(/\\/g, '\\\\')}'`;
  for (const p of fixture.content_patterns) {
    assert.ok(src.includes(`name: '${p.name}'`), p.name);
    assert.ok(src.includes(lit(p.regex)), `Regex ${p.name} differs from the fixture`);
    assert.ok(src.includes(`'${p.replacement}'`), p.replacement);
  }
  // Verify rule order.
  const idx = fixture.content_patterns.map((p) => src.indexOf(`name: '${p.name}'`));
  assert.deepEqual([...idx].sort((a, b) => a - b), idx);
  const keySrc = /SENSITIVE_KEY_SOURCE =\s*\n?\s*'([^']*)' \+\s*\n?\s*'([^']*)'/.exec(src);
  assert.equal(keySrc[1] + keySrc[2], fixture.sensitive_key_regex);
  for (const k of fixture.content_keys) assert.ok(src.includes(`'${k}'`), k);
  assert.equal(fixture.scan_limit_bytes, 256 * 1024);
  assert.equal(fixture.truncate_bytes, 512 * 1024);
  assert.equal(fixture.truncated_attr, 'flashcat.content_truncated');
});

test('All masking.json key_cases and content_cases pass through init, span creation, masking, and protobuf export', async () => {
  const run = 'mask-fixture';
  const code = `
import * as otel from '@flashcat/genai-otel';
import { readFileSync } from 'node:fs';
const fx = JSON.parse(readFileSync(${JSON.stringify(join(ROOT, '..', 'spec', 'fixtures', 'masking.json'))}, 'utf8'));
otel.init({ serviceName: 'x', metrics: false, captureContent: 'span', resourceAttributes: { 'test.run': '${run}' } });
fx.key_cases.forEach((kc, i) => otel.trace('key-' + i, () => {}, { [kc.key]: 'secret-value' }));
fx.content_cases.forEach((cc, i) => otel.trace('content-' + i, () => {}, Object.fromEntries(fx.content_keys.map((k) => [k, cc.input]))));
await otel.flush();
console.log('RESULT:{}');
`;
  await runScript('bare', code, { env: { FLASHCAT_ENDPOINT: c.url + '/v1/traces', FLASHCAT_API_KEY: 'k:s' } });
  const spans = c.spans().filter((s) => s.resource['test.run'] === run);
  const by = Object.fromEntries(spans.map((s) => [s.name, s]));
  fixture.key_cases.forEach((kc, i) => {
    const v = by['key-' + i].attrs[kc.key];
    assert.equal(v, kc.masked ? fixture.replacement_l1 : 'secret-value', `key ${kc.key}`);
  });
  fixture.content_cases.forEach((cc, i) => {
    for (const k of fixture.content_keys) {
      assert.equal(by['content-' + i].attrs[k], cc.output, `content case ${i} on ${k}: ${cc.input}`);
    }
  });
});

test('Capture off skips L2, L1 preserves counters, and L3 marks truncation at a valid character boundary', async () => {
  const run = 'mask-l1l3';
  const code = `
import * as otel from '@flashcat/genai-otel';
otel.init({ serviceName: 'x', metrics: false, masking: { truncateBytes: 10 }, resourceAttributes: { 'test.run': '${run}' } });
otel.trace('s', () => {}, {
  'http.request.header.authorization': 'Bearer abc',
  'gen_ai.usage.input_tokens': 5,
  'gen_ai.usage.output_tokens': '7',
  'big': '中文中文中文',
  'gen_ai.input.messages': '13812345678',
});
await otel.flush();
console.log('RESULT:' + JSON.stringify(otel.diagnostics().semantics));
`;
  const { result } = await runScript('bare', code, { env: { FLASHCAT_ENDPOINT: c.url + '/v1/traces', FLASHCAT_API_KEY: 'k:s' } });
  const s = c.spans().find((x) => x.resource['test.run'] === run);
  assert.equal(s.attrs['http.request.header.authorization'], '[REDACTED]');
  assert.equal(s.attrs['gen_ai.usage.input_tokens'], 5);
  assert.equal(s.attrs['gen_ai.usage.output_tokens'], '7', 'Counter keys are preserved even with string values');
  assert.equal(s.attrs.big, '中文中', 'Truncate 18 bytes to 9 within the 10-byte limit, preserving complete characters');
  assert.equal(s.attrs['gen_ai.input.messages'], '1381234567', 'With capture off, skip L2 content masking while retaining L3 truncation');
  assert.equal(s.attrs['flashcat.content_truncated'], true);
  assert.deepEqual(result, { orphan_inference_spans: 0, nested_inference_spans: 0, masked_spans: 1, truncated_spans: 1 });
});

test('Truncation defaults to 512 KiB and additional custom rules apply', async () => {
  const run = 'mask-big';
  const code = `
import * as otel from '@flashcat/genai-otel';
otel.init({ serviceName: 'x', metrics: false, captureContent: 'span',
  masking: { extraPatterns: [{ name: 'order', pattern: /ORD-\\d{6}/, replacement: '[REDACTED:order]' }] },
  resourceAttributes: { 'test.run': '${run}' } });
otel.trace('s', () => {}, { 'x.big': 'é'.repeat(300 * 1024), 'gen_ai.tool.call.arguments': '单号 ORD-123456' });
await otel.flush();
console.log('RESULT:{}');
`;
  await runScript('bare', code, { env: { FLASHCAT_ENDPOINT: c.url + '/v1/traces', FLASHCAT_API_KEY: 'k:s' } });
  const s = c.spans().find((x) => x.resource['test.run'] === run);
  assert.equal(Buffer.byteLength(s.attrs['x.big']), 512 * 1024);
  assert.equal(s.attrs['flashcat.content_truncated'], true);
  assert.equal(s.attrs['gen_ai.tool.call.arguments'], '单号 [REDACTED:order]');
});

test('The linear email matcher agrees with the regex on 400 random strings and handles long input without blocking the event loop', async () => {
  const run = 'mask-random';
  // Bias the random strings toward characters that exercise boundary conditions.
  const alphabet = ['a', 'b', 'Z', '1', '9', '.', '.', '-', '_', '%', '+', '@', '@', ' ', '中', '\\', 'é', 'c', 'om'];
  let seed = 12345;
  const rnd = () => ((seed = (seed * 1103515245 + 12345) & 0x7fffffff) / 0x7fffffff);
  const cases = [];
  for (let i = 0; i < 400; i++) {
    let s = '';
    const n = 1 + Math.floor(rnd() * 40);
    for (let j = 0; j < n; j++) s += alphabet[Math.floor(rnd() * alphabet.length)];
    cases.push(s);
  }
  cases.push('x@y.com123@z.org', 'a@b.cc.dd@e.ff', 'a@b.c', 'a@.co', '@a.co', 'a.@b.co', 'aa@bb..cc', 'a@b-c.d1.ef');
  const email = fixture.content_patterns.find((p) => p.name === 'email');
  const rx = new RegExp(email.regex, 'gu');
  const expected = cases.map((s) => s.replace(rx, email.replacement));
  const big = 'A'.repeat(256 * 1024 - 10) + '@nodomain';
  const code = `
import * as otel from '@flashcat/genai-otel';
otel.init({ serviceName: 'x', metrics: false, captureContent: 'span', resourceAttributes: { 'test.run': '${run}' } });
const cases = ${JSON.stringify(cases)};
cases.forEach((s, i) => otel.trace('r-' + i, () => {}, { 'gen_ai.tool.call.result': s }));
const t0 = performance.now();
otel.trace('big', () => {}, { 'gen_ai.tool.call.result': 'A'.repeat(256 * 1024 - 10) + '@nodomain' });
const ms = performance.now() - t0;
await otel.flush();
console.log('RESULT:' + JSON.stringify({ ms }));
`;
  const { result } = await runScript('bare', code, { env: { FLASHCAT_ENDPOINT: c.url + '/v1/traces', FLASHCAT_API_KEY: 'k:s' } });
  const spans = c.spans().filter((s) => s.resource['test.run'] === run);
  const by = Object.fromEntries(spans.map((s) => [s.name, s]));
  cases.forEach((s, i) => {
    // The other three rules require more digits than these random strings contain, so this only compares the email rule.
    assert.equal(by['r-' + i].attrs['gen_ai.tool.call.result'], expected[i], JSON.stringify(s));
  });
  assert.equal(by.big.attrs['gen_ai.tool.call.result'], big);
  assert.ok(result.ms < 500, `A 256 KiB string with no matches took ${result.ms} ms`);
});
