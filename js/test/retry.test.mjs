// Bounded retries at exit (contract §3 and §9): verify attempts, backoff, deadline, and cancellation through both public export paths.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { gunzipSync } from 'node:zlib';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { buildSync } from 'esbuild';
import { ROOT, decodeTraces, runScript, waitFor } from './helpers.mjs';

// Build an internal entry point only for transport unit tests. Integration tests use the package installed by test:setup.
const outdir = join(ROOT, 'test', '.work', 'retry');
buildSync({
  entryPoints: [join(ROOT, 'src', 'export.ts'), join(ROOT, 'src', 'deadline.ts'), join(ROOT, 'src', 'state.ts')],
  outdir, bundle: true, splitting: true, format: 'esm', platform: 'node', packages: 'external',
  outExtension: { '.js': '.mjs' },
});
const { GzipHttpTransport } = await import(pathToFileURL(join(outdir, 'export.mjs')));
const { openDeadline, isDeadlineError } = await import(pathToFileURL(join(outdir, 'deadline.mjs')));
const { state } = await import(pathToFileURL(join(outdir, 'state.mjs')));

async function receiver({ failures = 0, status = 503, reset = false, retryAfter, responseDelay = 0 } = {}) {
  const requests = [];
  const perPath = new Map();
  const server = createServer((req, res) => {
    const chunks = [];
    req.on('data', (chunk) => chunks.push(chunk));
    req.on('end', () => {
      const attempt = (perPath.get(req.url) ?? 0) + 1;
      perPath.set(req.url, attempt);
      const fail = attempt <= failures;
      const request = { path: req.url, at: performance.now(), body: gunzipSync(Buffer.concat(chunks)), fail };
      requests.push(request);
      const respond = () => {
        if (fail && reset) return req.socket.destroy();
        res.writeHead(fail ? status : 200, retryAfter ? { 'Retry-After': retryAfter } : {});
        res.end();
      };
      if (responseDelay) setTimeout(respond, responseDelay);
      else respond();
    });
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  return {
    requests,
    url: `http://127.0.0.1:${server.address().port}/v1/traces`,
    close: () => new Promise((resolve) => {
      server.closeAllConnections();
      server.close(resolve);
    }),
  };
}

async function send(options, timeout = 3000) {
  const sink = await receiver(options);
  const transport = new GzipHttpTransport(sink.url, {});
  const dl = openDeadline(timeout);
  const started = performance.now();
  try {
    const result = await transport.send(Buffer.from('last batch'), 10_000);
    return { result, requests: sink.requests, ms: performance.now() - started };
  } finally {
    dl.close();
    transport.shutdown();
    await sink.close();
    assert.equal(state().inflight.size, 0, 'All requests and backoff waits have been unregistered');
  }
}

for (const status of [429, 502, 503, 504]) {
  test(`Bounded export retries ${status} at most twice and succeeds after 200 and 400 ms backoffs`, async () => {
    const { result, requests } = await send({ failures: 2, status });
    assert.equal(result.status, 'success');
    assert.equal(requests.length, 3);
    assert.ok(requests[1].at - requests[0].at >= 190, 'The first backoff is 200 ms');
    assert.ok(requests[2].at - requests[1].at >= 390, 'The second backoff is 400 ms');
    assert.ok(requests.every((r) => r.body.equals(requests[0].body)), 'Retries send the same batch');
  });
}

test('A reset connection delivers after two retries at exit', async () => {
  const { result, requests } = await send({ failures: 2, reset: true });
  assert.equal(result.status, 'success');
  assert.equal(requests.length, 3);
});

test('Three consecutive failures exhaust the retry budget with no fourth request', async () => {
  const { result, requests, ms } = await send({ failures: 3 });
  assert.equal(result.status, 'failure');
  assert.equal(result.error.code, 503);
  assert.equal(requests.length, 3);
  assert.ok(ms >= 590 && ms < 1500, `${ms} ms`);
});

for (const status of [400, 401, 403, 404, 413, 500, 501, 505]) {
  test(`Non-retryable HTTP ${status} sends only one request`, async () => {
    const { result, requests } = await send({ failures: 3, status });
    assert.equal(result.error.code, status);
    assert.equal(requests.length, 1);
  });
}

test('Less than 200 ms for backoff preserves the original failure and skips the retry', async () => {
  const { result, requests, ms } = await send({ failures: 2 }, 150);
  assert.equal(requests.length, 1);
  assert.equal(result.error.code, 503);
  assert.ok(ms < 150, `${ms} ms`);
});

test('Enough time for backoff alone still skips the retry if no request fits', async () => {
  const { result, requests, ms } = await send({ failures: 2, responseDelay: 250 }, 650);
  assert.equal(result.error.code, 503);
  assert.equal(requests.length, 1, 'About 400 ms remains, less than the 200 ms backoff plus the previous 250 ms request');
  assert.ok(ms < 600, `${ms} ms`);
});

for (const [label, retryAfter] of [
  ['seconds', '10'],
  ['HTTP date', new Date(Date.now() + 3600_000).toUTCString()],
  ['extremely large seconds value', '9'.repeat(310)],
]) {
  test(`Retry-After (${label}) exceeding the remaining budget skips retries immediately`, async () => {
    const { result, requests, ms } = await send({ failures: 2, retryAfter }, 1000);
    assert.equal(requests.length, 1);
    assert.equal(result.error.code, 503);
    assert.ok(ms < 500, `${ms} ms`);
  });
}

test('On natural exit, backoff keeps the process alive through both retries so traces and metrics arrive', async () => {
  const sink = await receiver({ failures: 2, reset: true });
  try {
    await runScript('bare', `
import * as otel from '@flashcat/otel';
import { metrics } from '@opentelemetry/api';
otel.init({ serviceName: 'retry' });
metrics.getMeter('retry').createCounter('retry_counter').add(7);
otel.trace('last-batch', () => {});
`, { env: { FLASHCAT_ENDPOINT: sink.url, FLASHCAT_API_KEY: 'exitcheck:secret' } });
    for (const path of ['/v1/traces', '/v1/metrics']) {
      const requests = sink.requests.filter((r) => r.path === path);
      assert.equal(requests.length, 3, path);
      assert.equal(requests.filter((r) => !r.fail).length, 1, path);
    }
  } finally {
    await sink.close();
  }
});

test('Explicit flush(3000) delivers traces and metrics after bounded retries', async () => {
  const sink = await receiver({ failures: 2 });
  try {
    const { result } = await runScript('bare', `
import * as otel from '@flashcat/otel';
import { metrics } from '@opentelemetry/api';
otel.init({ serviceName: 'retry' });
metrics.getMeter('retry').createCounter('retry_counter').add(7);
otel.trace('last-batch', () => {});
const ok = await otel.flush(3000);
console.log('RESULT:' + JSON.stringify({ ok, stats: otel.diagnostics().export }));
`, { env: { FLASHCAT_ENDPOINT: sink.url, FLASHCAT_API_KEY: 'exitcheck:secret' } });
    assert.equal(result.ok, true);
    assert.equal(result.stats.ok, 1);
    for (const path of ['/v1/traces', '/v1/metrics']) {
      const requests = sink.requests.filter((r) => r.path === path);
      assert.equal(requests.length, 3, path);
      assert.equal(requests.filter((r) => !r.fail).length, 1, path);
    }
  } finally {
    await sink.close();
  }
});

test('An in-budget Retry-After waits at least the one second requested by the server', async () => {
  const { result, requests } = await send({ failures: 1, retryAfter: '1' });
  assert.equal(result.status, 'success');
  assert.equal(requests.length, 2);
  assert.ok(requests[1].at - requests[0].at >= 990);
});

test('Steady-state HTTP and network failures are never retried', async () => {
  for (const reset of [false, true]) {
    const sink = await receiver({ failures: 3, reset });
    const transport = new GzipHttpTransport(sink.url, {});
    try {
      assert.equal((await transport.send(Buffer.from('batch'), 1000)).status, 'failure');
      assert.equal(sink.requests.length, 1);
    } finally {
      transport.shutdown();
      await sink.close();
    }
  }
});

test('An earlier deadline during backoff cancels the wait and prevents a background retry', async () => {
  const sink = await receiver({ failures: 3 });
  const transport = new GzipHttpTransport(sink.url, {});
  const dl = openDeadline(3000);
  let earlier;
  try {
    const pending = transport.send(Buffer.from('batch'), 10_000);
    assert.ok(await waitFor(() => sink.requests.length === 1));
    earlier = openDeadline(30);
    const result = await pending;
    assert.ok(isDeadlineError(result.error));
    await new Promise((resolve) => setTimeout(resolve, 250));
    assert.equal(sink.requests.length, 1);
    assert.equal(state().inflight.size, 0);
  } finally {
    earlier?.close();
    dl.close();
    transport.shutdown();
    await sink.close();
  }
});

test('Request timeout uses the shorter of the export timeout and remaining budget, then aborts the wait for a response', async () => {
  for (const [timeout, exportTimeout] of [[80, 10_000], [1000, 80]]) {
    const sink = await receiver({ responseDelay: 200 });
    const transport = new GzipHttpTransport(sink.url, {});
    const dl = openDeadline(timeout);
    try {
      const started = performance.now();
      const result = await transport.send(Buffer.from('batch'), exportTimeout);
      assert.equal(result.status, 'failure');
      assert.ok(performance.now() - started < 1000);
      if (timeout === 80) assert.ok(isDeadlineError(result.error));
      else assert.match(result.error.message, /timed out/);
    } finally {
      dl.close();
      transport.shutdown();
      await sink.close();
    }
  }
});

for (const reset of [false, true]) {
  for (const failures of [1, 2, 3]) {
    test(`Explicit shutdown(5000) with ${failures} failures each for traces and metrics (${reset ? 'reset' : '503'})`, async () => {
      const sink = await receiver({ failures, reset });
      try {
        const { result } = await runScript('bare', `
import * as otel from '@flashcat/otel';
import { metrics } from '@opentelemetry/api';
otel.init({ serviceName: 'retry' });
metrics.getMeter('retry').createCounter('retry_counter').add(7);
otel.trace('last-batch', () => {});
const ok = await otel.shutdown(5000);
console.log('RESULT:' + JSON.stringify({ ok, stats: otel.diagnostics().export }));
`, { env: { FLASHCAT_ENDPOINT: sink.url, FLASHCAT_API_KEY: 'exitcheck:secret' } });
        for (const path of ['/v1/traces', '/v1/metrics']) {
          const requests = sink.requests.filter((r) => r.path === path);
          assert.equal(requests.length, Math.min(failures + 1, 3), path);
          assert.equal(requests.filter((r) => !r.fail).length, failures < 3 ? 1 : 0, path);
          assert.ok(requests.every((r) => r.body.equals(requests[0].body)), `${path} payload is unchanged`);
        }
        if (failures < 3) {
          assert.equal(result.ok, true);
          assert.equal(result.stats.ok, 1);
          assert.equal(result.stats.network, 0);
          assert.equal(result.stats.http_5xx ?? 0, 0);
          const traces = sink.requests.find((r) => r.path === '/v1/traces' && !r.fail);
          assert.equal(decodeTraces(traces.body)[0].name, 'last-batch');
          assert.ok(sink.requests.find((r) => r.path === '/v1/metrics' && !r.fail).body.includes('retry_counter'));
        } else {
          assert.equal(result.stats.ok, 0);
          assert.equal(result.stats[reset ? 'network' : 'http_5xx'], 1, 'Count only the final failure');
        }
      } finally {
        await sink.close();
      }
    });
  }
}
