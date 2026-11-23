// Test HTTPS transport against a mock endpoint with a self-signed certificate trusted via NODE_EXTRA_CA_CERTS.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { ROOT, basic, runScript, startCollector } from './helpers.mjs';

function selfSigned() {
  const dir = join(ROOT, 'test', '.work', 'certs');
  mkdirSync(dir, { recursive: true });
  const key = join(dir, 'key.pem');
  const cert = join(dir, 'cert.pem');
  execFileSync('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', key, '-out', cert, '-days', '1', '-subj', '/CN=127.0.0.1', '-addext', 'subjectAltName=IP:127.0.0.1'], { stdio: 'ignore' });
  return { key: readFileSync(key), cert: readFileSync(cert), certPath: cert };
}

test('HTTPS sends protobuf with gzip and Basic auth, and an untrusted certificate counts as a network error', async () => {
  const { key, cert, certPath } = selfSigned();
  const c = await startCollector({ tls: { key, cert } });
  try {
    assert.ok(c.url.startsWith('https://'));
    const code = `
import * as otel from '@flashcat/genai-otel';
otel.init({ serviceName: 'x', metrics: false });
otel.trace('over-https', () => {});
await otel.flush();
console.log('RESULT:' + JSON.stringify(otel.diagnostics().export));
`;
    const env = { FLASHCAT_ENDPOINT: c.url + '/v1/traces', FLASHCAT_API_KEY: 'k:s' };
    const { result } = await runScript('bare', code, { env: { ...env, NODE_EXTRA_CA_CERTS: certPath } });
    assert.equal(result.ok, 1, JSON.stringify(result));
    assert.ok(c.spans().some((s) => s.name === 'over-https'));
    assert.ok(c.requests[0].gzip);
    assert.equal(c.requests[0].headers.authorization, basic('k:s'));

    const { result: r2 } = await runScript('bare', code, { env });
    assert.equal(r2.ok, 0);
    assert.equal(r2.network, 1);
    assert.match(r2.last_error, /self-signed|certificate/i);
  } finally {
    await c.close();
  }
});
