// Configuration precedence, validation, ConfigError, idempotence, the disable switch, and diagnostics shape.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { runScript, startCollector } from './helpers.mjs';

const PRINT_INIT = (opts = '') => `
import * as otel from '@flashcat/otel';
const d = otel.init(${opts});
console.log('RESULT:' + JSON.stringify(d));
`;

test('Configuration precedence is explicit arguments, then FLASHCAT_*, OTEL_*, and defaults', async () => {
  const env = {
    FLASHCAT_SERVICE_NAME: 'from-flashcat',
    OTEL_SERVICE_NAME: 'from-otel',
    FLASHCAT_ENVIRONMENT: 'staging',
    OTEL_RESOURCE_ATTRIBUTES: 'deployment.environment.name=otel-env,service.name=from-res',
    FLASHCAT_ENDPOINT: 'http://127.0.0.1:1/flashcat/v1/traces',
    OTEL_EXPORTER_OTLP_TRACES_ENDPOINT: 'http://127.0.0.1:1/otel/v1/traces',
    FLASHCAT_API_KEY: 'k:s',
  };
  // Explicit arguments take precedence.
  let { result } = await runScript('bare', PRINT_INIT(`{ serviceName: 'explicit', environment: 'dev', endpoint: 'http://127.0.0.1:1/explicit/v1/traces', metrics: false }`), { env });
  assert.equal(result.service_name, 'explicit');
  assert.equal(result.environment, 'dev');
  assert.equal(result.endpoint, 'http://127.0.0.1:1/explicit/v1/traces');
  // FLASHCAT_* comes next.
  ({ result } = await runScript('bare', PRINT_INIT('{ metrics: false }'), { env }));
  assert.equal(result.service_name, 'from-flashcat');
  assert.equal(result.environment, 'staging');
  assert.equal(result.endpoint, 'http://127.0.0.1:1/flashcat/v1/traces');
  // OTEL_* follows.
  const otelOnly = { ...env };
  delete otelOnly.FLASHCAT_SERVICE_NAME;
  delete otelOnly.FLASHCAT_ENVIRONMENT;
  delete otelOnly.FLASHCAT_ENDPOINT;
  ({ result } = await runScript('bare', PRINT_INIT('{ metrics: false }'), { env: otelOnly }));
  assert.equal(result.service_name, 'from-otel');
  assert.equal(result.environment, 'otel-env');
  assert.equal(result.endpoint, 'http://127.0.0.1:1/otel/v1/traces');
  // Use service.name from OTEL_RESOURCE_ATTRIBUTES, append /v1/traces to OTEL_EXPORTER_OTLP_ENDPOINT, and default the environment to prod.
  ({ result } = await runScript('bare', PRINT_INIT('{ metrics: false }'), {
    env: { OTEL_RESOURCE_ATTRIBUTES: 'service.name=from-res', OTEL_EXPORTER_OTLP_ENDPOINT: 'http://127.0.0.1:1/base/', FLASHCAT_API_KEY: 'k:s' },
  }));
  assert.equal(result.service_name, 'from-res');
  assert.equal(result.environment, 'prod');
  assert.equal(result.endpoint, 'http://127.0.0.1:1/base/v1/traces');
});

test('FLASHCAT_API_KEY takes precedence over Authorization from OTEL_EXPORTER_OTLP_HEADERS', async () => {
  const c = await startCollector();
  try {
    const code = `
import * as otel from '@flashcat/otel';
otel.init({ serviceName: 'x', metrics: false });
otel.trace('job', () => 1);
await otel.flush();
console.log('RESULT:' + JSON.stringify(otel.diagnostics()));
`;
    await runScript('bare', code, {
      env: { FLASHCAT_ENDPOINT: c.url + '/v1/traces', OTEL_EXPORTER_OTLP_HEADERS: 'Authorization=Basic%20abc%3D%3D' },
    });
    assert.equal(c.requests[0].headers.authorization, 'Basic abc==');
  } finally {
    await c.close();
  }
});

test('Invalid endpoints, missing service names, and malformed credentials disable export and report problems without throwing', async () => {
  let { result } = await runScript('bare', PRINT_INIT(`{ serviceName: 'x' }`), {
    env: { FLASHCAT_ENDPOINT: 'ingest.example.com', FLASHCAT_API_KEY: 'k:s' },
  });
  assert.ok(result.problems.some((p) => p.includes('完整 URL')), result.problems.join('\n'));
  assert.equal(result.provider.exporter_mode, 'none');
  assert.equal(result.enabled, false);

  ({ result } = await runScript('bare', PRINT_INIT(`{ serviceName: 'x' }`), {
    env: { FLASHCAT_ENDPOINT: 'http://127.0.0.1:4318', FLASHCAT_API_KEY: 'k:s' },
  }));
  assert.ok(result.problems.some((p) => p.includes('缺少路径')), result.problems.join('\n'));
  assert.equal(result.provider.exporter_mode, 'none');

  ({ result } = await runScript('bare', PRINT_INIT(), {
    env: { FLASHCAT_ENDPOINT: 'http://127.0.0.1:1/v1/traces', FLASHCAT_API_KEY: 'k:s' },
  }));
  assert.ok(result.problems.some((p) => p.includes('service_name')), result.problems.join('\n'));
  assert.equal(result.provider.exporter_mode, 'none');

  ({ result } = await runScript('bare', PRINT_INIT(`{ serviceName: 'unknown_service:node' }`), {
    env: { FLASHCAT_ENDPOINT: 'http://127.0.0.1:1/v1/traces', FLASHCAT_API_KEY: 'k:s' },
  }));
  assert.ok(result.problems.some((p) => p.includes('unknown_service')));

  ({ result } = await runScript('bare', PRINT_INIT(`{ serviceName: 'x' }`), {
    env: { FLASHCAT_ENDPOINT: 'http://127.0.0.1:1/v1/traces', FLASHCAT_API_KEY: 'base64-without-colon' },
  }));
  assert.ok(result.problems.some((p) => p.includes('keyID:secret')), result.problems.join('\n'));
  assert.equal(result.provider.exporter_mode, 'none');

  // A missing endpoint goes in notes, as in Python.
  ({ result } = await runScript('bare', PRINT_INIT(`{ serviceName: 'x' }`), { env: { FLASHCAT_API_KEY: 'k:s' } }));
  assert.ok(result.notes.some((p) => p.includes('缺少 endpoint')), result.notes.join('\n'));
  assert.equal(result.provider.exporter_mode, 'none');
});

test('Invalid explicit capture settings throw ConfigError, event modes report problems, and invalid environment values do not throw', async () => {
  const code = `
import * as otel from '@flashcat/otel';
const out = {};
for (const v of [true, 'yes', 1]) {
  try { otel.init({ serviceName: 'x', captureContent: v }); out[String(v)] = 'no-throw'; }
  catch (e) { out[String(v)] = e instanceof otel.ConfigError ? 'ConfigError:' + e.message : 'other:' + e; }
}
try { otel.init({ serviceName: 'x', exporter: 'bogus' }); out.exporter = 'no-throw'; } catch (e) { out.exporter = e.name; }
try { otel.init({ serviceName: 'x', masking: { extraPatterns: [{ name: 'bad', pattern: '(', replacement: 'x' }] } }); out.masking = 'no-throw'; } catch (e) { out.masking = e.name; }
// ConfigError leaves the SDK uninitialized so corrected options can be retried.
const d = otel.init({ serviceName: 'x', captureContent: 'event', metrics: false });
out.eventProblems = d.problems;
out.upstream = d.content.upstream;
console.log('RESULT:' + JSON.stringify(out));
`;
  const { result } = await runScript('bare', code);
  assert.match(result.true, /^ConfigError:/);
  assert.match(result.yes, /^ConfigError:/);
  assert.match(result['1'], /^ConfigError:/);
  assert.equal(result.exporter, 'ConfigError');
  assert.equal(result.masking, 'ConfigError');
  assert.ok(result.eventProblems.some((p) => p.includes('event')), result.eventProblems.join('\n'));
  assert.equal(result.upstream, 'NO_CONTENT');

  const { result: r2 } = await runScript('bare', PRINT_INIT(`{ serviceName: 'x', metrics: false }`), {
    env: { FLASHCAT_CAPTURE_CONTENT: 'yes-please' },
  });
  assert.ok(r2.problems.some((p) => p.includes('FLASHCAT_CAPTURE_CONTENT')));
  assert.equal(r2.content.upstream, 'NO_CONTENT');

  // In JS, OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=true is an alias for span.
  const { result: r3 } = await runScript('bare', PRINT_INIT(`{ serviceName: 'x', metrics: false }`), {
    env: { OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT: 'true' },
  });
  assert.equal(r3.content.upstream, 'SPAN_ONLY');
  // Explicit false overrides environment settings.
  const { result: r4 } = await runScript('bare', PRINT_INIT(`{ serviceName: 'x', metrics: false, captureContent: false }`), {
    env: { FLASHCAT_CAPTURE_CONTENT: 'span' },
  });
  assert.equal(r4.content.upstream, 'NO_CONTENT');
});

test('The content capture switch configures each source and preserves user-set environment variables', async () => {
  const code = `
import * as otel from '@flashcat/otel';
const d = otel.init({ serviceName: 'x', metrics: false, captureContent: 'span' });
const e = process.env;
console.log('RESULT:' + JSON.stringify({ content: d.content, upstream: e.OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT,
  anthropic: e.ANTHROPIC_OPEN_TELEMETRY_TRACES_CONTENT_MODE, hideIn: e.OPENINFERENCE_HIDE_INPUTS, hideOut: e.OPENINFERENCE_HIDE_OUTPUTS }));
`;
  const { result } = await runScript('bare', code, { env: { OPENINFERENCE_HIDE_OUTPUTS: 'true' } });
  assert.equal(result.upstream, 'true', 'Upstream JS instrumentation accepts only true or false');
  assert.equal(result.anthropic, 'content');
  assert.equal(result.hideIn, 'false');
  assert.equal(result.hideOut, 'true', 'Preserve the user-set value');
  assert.equal(result.content.anthropic, 'content');
  assert.equal(result.content.openinference, 'best_effort:hide=false');
});

test('A second init returns the original diagnostics without side effects', async () => {
  const code = `
import * as otel from '@flashcat/otel';
import { trace } from '@opentelemetry/api';
const d1 = otel.init({ serviceName: 'first', metrics: false });
const p1 = trace.getTracerProvider().getDelegate();
const d2 = otel.init({ serviceName: 'second', exporter: 'none' });
const p2 = trace.getTracerProvider().getDelegate();
console.log('RESULT:' + JSON.stringify({ s1: d1.service_name, s2: d2.service_name, same: p1 === p2, m2: d2.provider.exporter_mode }));
`;
  const { result, stderr } = await runScript('bare', code, {
    env: { FLASHCAT_ENDPOINT: 'http://127.0.0.1:1/v1/traces', FLASHCAT_API_KEY: 'k:s' },
  });
  assert.deepEqual(result, { s1: 'first', s2: 'first', same: true, m2: 'add' });
  assert.match(stderr, /init 已调用过/);
});

test('FLASHCAT_DISABLED and OTEL_SDK_DISABLED make init return only diagnostics', async () => {
  for (const k of ['FLASHCAT_DISABLED', 'OTEL_SDK_DISABLED']) {
    const code = `
import * as otel from '@flashcat/otel';
import { trace } from '@opentelemetry/api';
const d = otel.init({ serviceName: 'x' });
const name = trace.getTracerProvider().getDelegate().constructor.name;
console.log('RESULT:' + JSON.stringify({ d, name, env: process.env.OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT ?? null }));
`;
    const { result } = await runScript('bare', code, {
      env: { [k]: 'true', FLASHCAT_ENDPOINT: 'http://127.0.0.1:1/v1/traces', FLASHCAT_API_KEY: 'k:s' },
    });
    assert.equal(result.d.enabled, false);
    assert.ok(result.d.notes.some((n) => n.includes('FLASHCAT_DISABLED')));
    assert.equal(result.name, 'NoopTracerProvider');
    assert.equal(result.env, null, 'No environment variables were written');
    assert.deepEqual(result.d.instrumentations, []);
  }
});

test('Diagnostics field names match contract §8', async () => {
  const code = `
import * as otel from '@flashcat/otel';
const before = otel.diagnostics();
const d = otel.init({ serviceName: 'kefu-agent', metrics: false });
console.log('RESULT:' + JSON.stringify({ before, d, again: otel.diagnostics() }));
`;
  const { result } = await runScript('bare', code, {
    env: { FLASHCAT_ENDPOINT: 'http://127.0.0.1:1/v1/traces', FLASHCAT_API_KEY: 'k:s' },
  });
  assert.ok(result.before.problems.includes('init 尚未调用'));
  const keys = ['sdk', 'enabled', 'service_name', 'environment', 'endpoint', 'problems', 'notes', 'provider', 'content', 'instrumentations', 'export', 'semantics'];
  for (const d of [result.d, result.again]) {
    assert.deepEqual(Object.keys(d).sort(), [...keys].sort());
    assert.deepEqual(d.sdk, { name: 'flashcat-otel', version: '0.1.0', language: 'nodejs' });
    assert.deepEqual(Object.keys(d.provider).sort(), ['exporter_mode', 'metrics', 'situation']);
    assert.deepEqual(Object.keys(d.semantics).sort(), ['masked_spans', 'nested_inference_spans', 'orphan_inference_spans', 'truncated_spans']);
    for (const k of ['ok', 'network', 'dropped_queue_full', 'last_error']) assert.ok(k in d.export, k);
    assert.equal(d.provider.metrics, 'disabled');
    const libs = d.instrumentations.map((i) => i.lib);
    assert.deepEqual(libs, ['openai', 'anthropic', 'openai-agents', 'langchain', 'ai', 'bedrock', 'google-genai', 'mcp']);
    for (const i of d.instrumentations) {
      assert.equal(i.status, 'not_present');
      assert.ok(['upstream', 'native', 'openinference', 'flashcat'].includes(i.source));
    }
  }
});

test('FLASHCAT_SHUTDOWN_TIMEOUT accepts fractional seconds, defaults to 5 seconds on invalid input, and yields to shutdownTimeoutMs', async () => {
  // Read the deadline from the globalThis state shared by ESM and CJS because it is absent from diagnostics.
  const code = (opts) => `
import * as otel from '@flashcat/otel';
const d = otel.init(${opts});
console.log('RESULT:' + JSON.stringify({ problems: d.problems, ms: globalThis[Symbol.for('flashcat.otel.state')].shutdownTimeoutMs }));
`;
  const run = async (value, opts = `{ serviceName: 'x', metrics: false }`) =>
    (await runScript('bare', code(opts), { env: value === undefined ? {} : { FLASHCAT_SHUTDOWN_TIMEOUT: value } })).result;
  assert.deepEqual(await run(undefined), { problems: [], ms: 5000 });
  assert.deepEqual(await run('2.5'), { problems: [], ms: 2500 });
  assert.deepEqual(await run('0.2'), { problems: [], ms: 200 });
  for (const bad of ['abc', '0', '-1', '5s']) {
    const r = await run(bad);
    assert.equal(r.ms, 5000, bad);
    assert.equal(r.problems.length, 1, bad);
    assert.match(r.problems[0], new RegExp(`FLASHCAT_SHUTDOWN_TIMEOUT 必须是正数.*'${bad}'.*默认 5 秒`));
  }
  assert.deepEqual(await run('2', `{ serviceName: 'x', metrics: false, shutdownTimeoutMs: 1234 }`), { problems: [], ms: 1234 });
});
