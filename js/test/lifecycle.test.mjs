// Lifecycle (contract §9): flush and shut down automatically on exit, preserve default SIGTERM exit, and respect user signal handlers.
// The shutdown deadline bounds exit time with an unreachable endpoint. Count spans dropped at the deadline in dropped_timeout.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { refusedUrl, runScript, startBlackhole, startCollector, waitFor } from './helpers.mjs';

test('Natural script exit flushes automatically in beforeExit without an explicit flush call', async () => {
  const c = await startCollector();
  try {
    const code = `
import * as otel from '@flashcat/otel';
otel.init({ serviceName: 'x', metrics: false });
otel.trace('auto-flushed', () => {});
`;
    const t0 = Date.now();
    await runScript('bare', code, { env: { FLASHCAT_ENDPOINT: c.url + '/v1/traces', FLASHCAT_API_KEY: 'k:s' } });
    assert.ok(Date.now() - t0 < 4000, 'The process skipped the five-second batch timer');
    assert.ok(c.spans().some((s) => s.name === 'auto-flushed'));
  } finally {
    await c.close();
  }
});

test('SIGTERM flushes and preserves the default exit behavior without a user listener', async () => {
  const c = await startCollector();
  try {
    const code = `
import * as otel from '@flashcat/otel';
otel.init({ serviceName: 'x', metrics: false });
otel.trace('before-sigterm', () => {});
setInterval(() => {}, 1000);   // Keep the process running.
console.log('READY');
`;
    const r = await runScript('bare', code, {
      env: { FLASHCAT_ENDPOINT: c.url + '/v1/traces', FLASHCAT_API_KEY: 'k:s' },
      signal: 'SIGTERM',
      expectExit: (code, sig) => sig === 'SIGTERM',
      timeoutMs: 10_000,
    });
    assert.equal(r.signal, 'SIGTERM', 'SIGTERM still terminates the process by default');
    assert.ok(c.spans().some((s) => s.name === 'before-sigterm'), 'The SDK flushed before exit');
  } finally {
    await c.close();
  }
});

test('With a user SIGTERM listener, the SDK flushes and lets the listener control exit', async () => {
  const c = await startCollector();
  try {
    const code = `
import * as otel from '@flashcat/otel';
otel.init({ serviceName: 'x', metrics: false });
otel.trace('user-handler', () => {});
const t = setInterval(() => {}, 1000);
process.on('SIGTERM', () => {
  console.log('USER_HANDLER');
  setTimeout(() => { clearInterval(t); process.exitCode = 7; }, 300);
});
console.log('READY');
`;
    const r = await runScript('bare', code, {
      env: { FLASHCAT_ENDPOINT: c.url + '/v1/traces', FLASHCAT_API_KEY: 'k:s' },
      signal: 'SIGTERM',
      expectExit: 7,
      timeoutMs: 10_000,
    });
    assert.match(r.stdout, /USER_HANDLER/);
    assert.ok(await waitFor(() => c.spans().some((s) => s.name === 'user-handler')));
  } finally {
    await c.close();
  }
});

// ---- Shutdown deadline (contract §3 and §9): bound exit time for unreachable endpoints and count deadline drops ----

// Generate traces and metrics so both pipelines hit the unreachable endpoint and share the same deadline.
const EXIT_APP = (tail) => `
import * as otel from '@flashcat/otel';
import { metrics } from '@opentelemetry/api';
otel.init({ serviceName: 'x' });
metrics.getMeter('t').createCounter('flashcat_test_counter').add(1);
otel.trace('a', () => {});
otel.trace('b', () => {});
const t0 = performance.now();
${tail}
`;

test('Natural exit against a blackhole flushes and shuts down in beforeExit and counts drops at the one-second deadline', async () => {
  const bh = await startBlackhole();
  try {
    const code = EXIT_APP(`process.on('exit', () => console.log('RESULT:' + JSON.stringify({ ms: performance.now() - t0, d: otel.diagnostics() })));`);
    const { result } = await runScript('bare', code, {
      env: { FLASHCAT_ENDPOINT: bh.url + '/v1/traces', FLASHCAT_API_KEY: 'k:s', FLASHCAT_SHUTDOWN_TIMEOUT: '1' },
      timeoutMs: 20_000,
    });
    assert.ok(result.ms >= 900 && result.ms < 2000, `Application completion to process exit took ${result.ms} ms with a one-second deadline`);
    assert.equal(result.d.export.dropped_timeout, 2, JSON.stringify(result.d.export));
    assert.equal(result.d.export.network, 0, 'Deadline drops do not count as network errors');
    assert.equal(result.d.export.ok, 0);
    assert.match(result.d.export.last_error, /时限已到/);
    assert.ok(bh.accepted() >= 2, `Both traces and metrics reached the blackhole (${bh.accepted()} connections)`);
  } finally {
    await bh.close();
  }
});

test('Explicit shutdown(800) overrides FLASHCAT_SHUTDOWN_TIMEOUT, returns false at its deadline, and lets the process exit promptly', async () => {
  const bh = await startBlackhole();
  try {
    const code = EXIT_APP(`
const ok = await otel.shutdown(800);
const t1 = performance.now();
const again = await otel.shutdown(5000);   // Return the original shutdown result.
process.on('exit', () => console.log('RESULT:' + JSON.stringify({ ok, again, shutdownMs: t1 - t0, exitMs: performance.now() - t1, d: otel.diagnostics() })));
`);
    const { result } = await runScript('bare', code, {
      env: { FLASHCAT_ENDPOINT: bh.url + '/v1/traces', FLASHCAT_API_KEY: 'k:s', FLASHCAT_SHUTDOWN_TIMEOUT: '30' },
      timeoutMs: 20_000,
    });
    assert.equal(result.ok, false);
    assert.equal(result.again, false);
    assert.ok(result.shutdownMs >= 700 && result.shutdownMs < 1500, `shutdown took ${result.shutdownMs} ms`);
    assert.ok(result.exitMs < 500, `Process exited ${result.exitMs} ms after shutdown returned, with no pending requests or timers`);
    assert.equal(result.d.export.dropped_timeout, 2, JSON.stringify(result.d.export));
  } finally {
    await bh.close();
  }
});

test('Connection refusals retry for about 0.6 seconds at exit and count as network errors', async () => {
  const url = await refusedUrl();
  const code = EXIT_APP(`process.on('exit', () => console.log('RESULT:' + JSON.stringify({ ms: performance.now() - t0, d: otel.diagnostics() })));`);
  const { result } = await runScript('bare', code, { env: { FLASHCAT_ENDPOINT: url + '/v1/traces', FLASHCAT_API_KEY: 'k:s' } });
  assert.ok(result.ms >= 590 && result.ms < 1500, `Application completion to process exit took ${result.ms} ms`);
  assert.ok(result.d.export.network >= 1, JSON.stringify(result.d.export));
  assert.equal(result.d.export.dropped_timeout, 0);
  assert.match(result.d.export.last_error, /ECONNREFUSED/);
});

test('Against a blackhole, flush(500) aborts the request at the deadline and returns false so the process can exit', async () => {
  const bh = await startBlackhole();
  try {
    const code = `
import * as otel from '@flashcat/otel';
otel.init({ serviceName: 'x', metrics: false });
otel.trace('a', () => {});
const t0 = performance.now();
const ok = await otel.flush(500);
const t1 = performance.now();
process.on('exit', () => console.log('RESULT:' + JSON.stringify({ ok, flushMs: t1 - t0, exitMs: performance.now() - t1, d: otel.diagnostics() })));
`;
    const { result } = await runScript('bare', code, {
      env: { FLASHCAT_ENDPOINT: bh.url + '/v1/traces', FLASHCAT_API_KEY: 'k:s' },
      timeoutMs: 20_000,
    });
    assert.equal(result.ok, false);
    assert.ok(result.flushMs >= 400 && result.flushMs < 1200, `flush took ${result.flushMs} ms`);
    assert.ok(result.exitMs < 500, `Process exited ${result.exitMs} ms after flush returned`);
    assert.equal(result.d.export.dropped_timeout, 1, JSON.stringify(result.d.export));
  } finally {
    await bh.close();
  }
});

test('A single shutdown delivers all spans and metrics to a healthy endpoint', async () => {
  const c = await startCollector();
  try {
    const code = EXIT_APP(`
const ok = await otel.shutdown();
console.log('RESULT:' + JSON.stringify({ ok, d: otel.diagnostics() }));
`);
    const { result } = await runScript('bare', code, { env: { FLASHCAT_ENDPOINT: c.url + '/v1/traces', FLASHCAT_API_KEY: 'k:s' } });
    assert.equal(result.ok, true);
    assert.equal(result.d.export.ok, 2);
    assert.equal(result.d.export.dropped_timeout, 0);
    assert.deepEqual(c.spans().map((s) => s.name).sort(), ['a', 'b']);
    const m = c.requests.filter((r) => r.path === '/v1/metrics');
    assert.equal(m.length, 1, 'Metrics export exactly once during shutdown');
    assert.ok(m[0].gzip && m[0].body.includes('flashcat_test_counter'), 'Metrics are sent as gzipped protobuf');
  } finally {
    await c.close();
  }
});

test('SIGTERM against a blackhole during Kubernetes graceful termination honors FLASHCAT_SHUTDOWN_TIMEOUT and preserves default exit', async () => {
  const bh = await startBlackhole();
  try {
    const code = `
import * as otel from '@flashcat/otel';
otel.init({ serviceName: 'x' });
otel.trace('before-sigterm', () => {});
setInterval(() => {}, 1000);   // Keep the process running.
console.log('READY');
`;
    const t0 = Date.now();
    const r = await runScript('bare', code, {
      env: { FLASHCAT_ENDPOINT: bh.url + '/v1/traces', FLASHCAT_API_KEY: 'k:s', FLASHCAT_SHUTDOWN_TIMEOUT: '1' },
      signal: 'SIGTERM',
      expectExit: (code, sig) => sig === 'SIGTERM',
      timeoutMs: 20_000,
    });
    const ms = Date.now() - t0;
    assert.equal(r.signal, 'SIGTERM');
    assert.ok(ms < 4000, `Startup to exit took ${ms} ms with a one-second deadline. The five-second default would exceed the limit`);
    assert.ok(bh.accepted() >= 1, 'Export was attempted');
  } finally {
    await bh.close();
  }
});

test('The shutdown deadline aborts preexisting requests, counts their spans in dropped_timeout, and lets the process exit', async () => {
  const bh = await startBlackhole();
  try {
    const code = `
import * as otel from '@flashcat/otel';
otel.init({ serviceName: 'x', metrics: false });
otel.trace('a', () => {});
void otel.flush(10000);                       // This request is already in flight against the blackhole with a 10-second export timeout.
await new Promise((r) => setTimeout(r, 150));
const t0 = performance.now();
const ok = await otel.shutdown(800);
const t1 = performance.now();
process.on('exit', () => console.log('RESULT:' + JSON.stringify({ ok, shutdownMs: t1 - t0, exitMs: performance.now() - t1, d: otel.diagnostics() })));
`;
    for (let i = 0; i < 3; i++) {
      const { result } = await runScript('bare', code, {
        env: { FLASHCAT_ENDPOINT: bh.url + '/v1/traces', FLASHCAT_API_KEY: 'k:s' },
        timeoutMs: 20_000,
      });
      assert.equal(result.ok, false, JSON.stringify(result));
      assert.ok(result.shutdownMs < 1500, `shutdown took ${result.shutdownMs} ms`);
      assert.ok(result.exitMs < 500, `Attempt ${i + 1}: process exited ${result.exitMs} ms after shutdown returned`);
      assert.equal(result.d.export.dropped_timeout, 1, JSON.stringify(result.d.export));
      assert.equal(result.d.export.network, 0);
    }
  } finally {
    await bh.close();
  }
});
