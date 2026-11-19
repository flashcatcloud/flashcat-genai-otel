// TypeScript ESM (.mts) and CJS (.cts) consumers can resolve declarations for @flashcat/otel and @flashcat/otel/register.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { appDir, ROOT } from './helpers.mjs';

const CONSUMER = `
import * as otel from '@flashcat/otel';
import type { InitOptions, Diagnostics, MaskingConfig } from '@flashcat/otel';

const masking: MaskingConfig = { extraPatterns: [{ name: 'order', pattern: /ORD-\\\\d+/, replacement: '[REDACTED:order]' }] };
const opts: InitOptions = { serviceName: 'kefu-agent', captureContent: false, masking, exporter: 'auto' };
const d: Diagnostics = otel.init(opts);
const s: string | null = d.service_name;
const answer = otel.agent('kefu', async (q: string): Promise<string> => q, { description: 'x' });
const p: Promise<string> = answer('hi');
const lookup = otel.tool('lookup', (id: number) => ({ id }));
const r: { id: number } = lookup(1);
const w = otel.workflow('wf', () => 1);
const ask = otel.llm('qwen-max', async (q: string): Promise<string> => q, { provider: 'dashscope' });
const ap: Promise<string> = ask('hi');
const emb = otel.embedding('text-embedding-v3', (t: string[]) => [[0.1]]);
const ev: number[][] = emb(['a']);
const search = otel.retrieval('kb', (q: string) => [{ id: 'd1', content: q }]);
const usage: otel.Usage = { inputTokens: 1, outputTokens: 2, cacheReadInputTokens: 0, cacheWriteInputTokens: 0 };
const ann: otel.AnnotateOptions = { input: [{ role: 'user', content: 'hi' }], output: 'ok', model: 'm', usage, finishReasons: ['stop'], tags: { t: 1 } };
otel.annotate(ann);
// @ts-expect-error usage fields require numbers
otel.annotate({ usage: { inputTokens: '1' } });
const n: number = otel.trace('job', (span) => { span.setAttribute('k', 'v'); return 1; }, { a: 1 });
const v: Promise<string> = otel.session({ conversationId: 'c', userId: 'u', tags: { t: 1 } }, () => answer('x'));
const sp = otel.spanProcessor({ endpoint: 'https://ingest.example.com/v1/traces', apiKey: 'k:s' });
sp.onEnd;
const f: Promise<boolean> = otel.flush(1000);
const sd: Promise<boolean> = otel.shutdown();
const e = new otel.ConfigError('x');
// @ts-expect-error captureContent does not accept true
otel.init({ captureContent: true });
void [s, p, r, w, n, v, f, sd, e];
`;

test('TypeScript consumers in nodenext mode type-check both .mts and .cts', () => {
  const dir = join(appDir('bare'), 'types-check');
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, 'consumer.mts'), CONSUMER + "\nimport '@flashcat/otel/register';\n");
  writeFileSync(join(dir, 'consumer.cts'), CONSUMER);
  writeFileSync(
    join(dir, 'tsconfig.json'),
    JSON.stringify({
      compilerOptions: { module: 'nodenext', moduleResolution: 'nodenext', target: 'es2022', strict: true, noEmit: true, skipLibCheck: false, types: [] },
      files: ['consumer.mts', 'consumer.cts'],
    }),
  );
  const tsc = join(ROOT, 'node_modules', 'typescript', 'bin', 'tsc');
  let out = '';
  try {
    out = execFileSync(process.execPath, [tsc, '-p', join(dir, 'tsconfig.json')], { cwd: dir, encoding: 'utf8' });
  } catch (e) {
    assert.fail(`tsc failed:\n${e.stdout}\n${e.stderr}`);
  }
  assert.equal(out.trim(), '');
});
