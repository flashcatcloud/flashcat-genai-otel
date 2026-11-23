// Export counters (contract §3) distinguish 401, 429, 503, and network errors. Only the transport retries, and only the final result is counted.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { refusedUrl, runScript, startCollector } from './helpers.mjs';

const APP = `
import * as otel from '@flashcat/genai-otel';
otel.init({ serviceName: 'x', metrics: false });
otel.trace('job', () => 1);
otel.workflow('wf', () => 1)();
const ok = await otel.flush();
console.log('RESULT:' + JSON.stringify({ ok, diag: otel.diagnostics() }));
`;

for (const [status, bucket] of [
  [401, 'http_401'],
  [429, 'http_429'],
  [503, 'http_5xx'],
  [404, 'http_404'],
]) {
  test(`${status} increments export.${bucket} once and includes the status in last_error`, async () => {
    const c = await startCollector();
    c.status = status;
    try {
      const t0 = Date.now();
      const { result } = await runScript('bare', APP, {
        env: { FLASHCAT_ENDPOINT: c.url + '/v1/traces', FLASHCAT_API_KEY: 'k:bad' },
      });
      const d = result.diag;
      assert.equal(d.export[bucket], 1, JSON.stringify(d.export));
      assert.equal(d.export.ok, 0);
      assert.match(d.export.last_error, new RegExp(`HTTP ${status}`));
      assert.match(d.export.last_error, /fake collector/, 'last_error contains the first 300 bytes of the response body');
      assert.equal(c.requests.filter((r) => r.path === '/v1/traces').length, [429, 503].includes(status) ? 3 : 1, 'The transport retries only retryable status codes, at most twice');
      assert.ok(Date.now() - t0 < 8000);
    } finally {
      await c.close();
    }
  });
}

test('Network errors increment export.network', async () => {
  const url = await refusedUrl();
  const { result } = await runScript('bare', APP, {
    env: { FLASHCAT_ENDPOINT: url + '/v1/traces', FLASHCAT_API_KEY: 'k:s' },
  });
  assert.ok(result.diag.export.network >= 1, JSON.stringify(result.diag.export));
  assert.match(result.diag.export.last_error, /ECONNREFUSED/);
});
