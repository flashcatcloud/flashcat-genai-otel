// Test fixtures: a mock OTLP collector that decodes gzip and protobuf, mock model servers, and scripts run in separate child processes.
// Each scenario gets its own process to isolate global OTel state.
import { spawn } from 'node:child_process';
import { mkdirSync, writeFileSync, existsSync, readFileSync, readdirSync } from 'node:fs';
import { createServer } from 'node:http';
import { createServer as createNetServer } from 'node:net';
import { createServer as createHttpsServer } from 'node:https';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { gunzipSync } from 'node:zlib';
import { randomUUID } from 'node:crypto';

export const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
export const APPS = join(ROOT, 'test', '.work', 'apps');

export function appDir(name) {
  const d = join(APPS, name);
  if (!existsSync(join(d, 'node_modules', '@flashcat', 'genai-otel'))) {
    throw new Error(`测试应用 ${name} 未准备好，先运行 npm run test:setup`);
  }
  // Ensure the app tests the current build by comparing it with js/dist. Entry points reference chunks by content-based names.
  const installed = join(d, 'node_modules', '@flashcat', 'genai-otel', 'dist');
  const built = join(ROOT, 'dist');
  const same =
    readdirSync(installed).sort().join() === readdirSync(built).sort().join() &&
    readdirSync(built).every((f) => readFileSync(join(installed, f)).equals(readFileSync(join(built, f))));
  if (!same) throw new Error(`测试应用 ${name} 里的 @flashcat/genai-otel 不是当前构建，先运行 npm run test:setup`);
  return d;
}

// ---------------- Minimal protobuf decoding for ExportTraceServiceRequest ----------------

function readVarint(buf, pos) {
  let result = 0n;
  let shift = 0n;
  for (;;) {
    const b = buf[pos++];
    result |= BigInt(b & 0x7f) << shift;
    if ((b & 0x80) === 0) break;
    shift += 7n;
  }
  return [result, pos];
}

/** Decode to { fieldNo: [{wt, v}] }. v is a Buffer for wt=2, BigInt for wt=0, and Buffer for wt=1/5. */
function fields(buf) {
  const out = {};
  let pos = 0;
  while (pos < buf.length) {
    let key;
    [key, pos] = readVarint(buf, pos);
    const no = Number(key >> 3n);
    const wt = Number(key & 7n);
    let v;
    if (wt === 0) [v, pos] = readVarint(buf, pos);
    else if (wt === 1) {
      v = buf.subarray(pos, pos + 8);
      pos += 8;
    } else if (wt === 5) {
      v = buf.subarray(pos, pos + 4);
      pos += 4;
    } else if (wt === 2) {
      let len;
      [len, pos] = readVarint(buf, pos);
      v = buf.subarray(pos, pos + Number(len));
      pos += Number(len);
    } else throw new Error(`unsupported wire type ${wt}`);
    (out[no] ??= []).push({ wt, v });
  }
  return out;
}

const str = (f) => (f ? f[0].v.toString('utf8') : '');
const all = (f) => (f ?? []).map((x) => x.v);

function anyValue(buf) {
  const f = fields(buf);
  if (f[1]) return f[1][0].v.toString('utf8');
  if (f[2]) return f[2][0].v === 1n;
  if (f[3]) return Number(BigInt.asIntN(64, f[3][0].v));
  if (f[4]) return f[4][0].v.readDoubleLE(0);
  if (f[5]) return all(fields(f[5][0].v)[1]).map(anyValue);
  if (f[6]) return kvs(all(fields(f[6][0].v)[1]));
  if (f[7]) return f[7][0].v;
  return null;
}

function kvs(list) {
  const out = {};
  for (const b of list) {
    const f = fields(b);
    out[str(f[1])] = f[2] ? anyValue(f[2][0].v) : null;
  }
  return out;
}

export function decodeTraces(body) {
  const spans = [];
  for (const rsBuf of all(fields(body)[1])) {
    const rs = fields(rsBuf);
    const resource = rs[1] ? kvs(all(fields(rs[1][0].v)[1])) : {};
    for (const ssBuf of all(rs[2])) {
      const ss = fields(ssBuf);
      const scopeF = ss[1] ? fields(ss[1][0].v) : {};
      const scope = str(scopeF[1]);
      const scopeVersion = str(scopeF[2]);
      for (const spBuf of all(ss[2])) {
        const sp = fields(spBuf);
        const status = sp[15] ? fields(sp[15][0].v) : {};
        spans.push({
          name: str(sp[5]),
          scope,
          scopeVersion,
          resource,
          traceId: sp[1] ? sp[1][0].v.toString('hex') : '',
          spanId: sp[2] ? sp[2][0].v.toString('hex') : '',
          parent: sp[4] ? sp[4][0].v.toString('hex') : '',
          kind: sp[6] ? Number(sp[6][0].v) : 0,
          attrs: kvs(all(sp[9])),
          events: all(sp[11]).map((e) => {
            const ef = fields(e);
            return { name: str(ef[2]), attrs: kvs(all(ef[3])) };
          }),
          status: status[3] ? Number(status[3][0].v) : 0,
          statusMessage: str(status[2]),
        });
      }
    }
  }
  return spans;
}

// ---------------- Mock collector ----------------

export async function startCollector({ tls } = {}) {
  const c = {
    requests: [],
    status: 200,
    spans() {
      const out = [];
      for (const r of this.requests) {
        if (r.path !== '/v1/traces' || r.status !== 200) continue;
        out.push(...decodeTraces(r.body));
      }
      return out;
    },
  };
  const handler = (req, res) => {
    const chunks = [];
    req.on('data', (d) => chunks.push(d));
    req.on('end', () => {
      let body = Buffer.concat(chunks);
      const gz = req.headers['content-encoding'] === 'gzip';
      if (gz) body = gunzipSync(body);
      c.requests.push({ path: req.url, headers: req.headers, body, status: c.status, gzip: gz });
      res.writeHead(c.status, { 'Content-Type': 'application/x-protobuf' });
      res.end(c.status === 200 ? '' : `status ${c.status} from fake collector`);
    });
  };
  const srv = tls ? createHttpsServer(tls, handler) : createServer(handler);
  await new Promise((r) => srv.listen(0, '127.0.0.1', r));
  c.url = `${tls ? 'https' : 'http'}://127.0.0.1:${srv.address().port}`;
  c.close = () => new Promise((r) => srv.close(r));
  return c;
}

/** Blackhole endpoint for contract §9. Accept connections without reading or replying until close(). */
export async function startBlackhole() {
  const sockets = new Set();
  let accepted = 0;
  const srv = createNetServer((sock) => {
    accepted++;
    sockets.add(sock);
    sock.on('error', () => {});
    sock.on('close', () => sockets.delete(sock));
  });
  await new Promise((r) => srv.listen(0, '127.0.0.1', r));
  return {
    url: `http://127.0.0.1:${srv.address().port}`,
    /** Total number of accepted connections. */
    accepted: () => accepted,
    close: () =>
      new Promise((r) => {
        for (const s of sockets) s.destroy();
        srv.close(r);
      }),
  };
}

/** Return a currently unused port that refuses connections. */
export async function refusedUrl() {
  const srv = createNetServer();
  await new Promise((r) => srv.listen(0, '127.0.0.1', r));
  const port = srv.address().port;
  await new Promise((r) => srv.close(r));
  return `http://127.0.0.1:${port}`;
}

// ---------------- Mock model servers ----------------

/** Mock OpenAI chat.completions (including streaming), responses, and embeddings. */
export async function startFakeOpenAI() {
  const calls = [];
  const srv = createServer((req, res) => {
    const chunks = [];
    req.on('data', (d) => chunks.push(d));
    req.on('end', () => {
      let body = {};
      try {
        body = JSON.parse(Buffer.concat(chunks).toString('utf8') || '{}');
      } catch {}
      calls.push({ path: req.url, body });
      if (req.url.endsWith('/chat/completions')) {
        if (body.stream) {
          res.writeHead(200, { 'Content-Type': 'text/event-stream' });
          const base = { id: 'chatcmpl-s', object: 'chat.completion.chunk', created: 1761620000, model: 'gpt-4o-2024-08-06' };
          // Send a role-only chunk first, then the first content chunk 30 ms later. TTFT excludes the role-only chunk. Usage includes cached tokens.
          res.write(`data: ${JSON.stringify({ ...base, choices: [{ index: 0, delta: { role: 'assistant' }, finish_reason: null }] })}\n\n`);
          setTimeout(() => {
            res.write(`data: ${JSON.stringify({ ...base, choices: [{ index: 0, delta: { content: '已' }, finish_reason: null }] })}\n\n`);
            res.write(`data: ${JSON.stringify({ ...base, choices: [{ index: 0, delta: { content: '发货' }, finish_reason: 'stop' }] })}\n\n`);
            res.write(
              `data: ${JSON.stringify({ ...base, choices: [], usage: { prompt_tokens: 20, completion_tokens: 2, total_tokens: 22, prompt_tokens_details: { cached_tokens: 8 } } })}\n\n`,
            );
            res.end('data: [DONE]\n\n');
          }, 30);
          return;
        }
        const payload = {
          id: 'chatcmpl-test',
          object: 'chat.completion',
          created: 1761620000,
          model: 'gpt-4o-2024-08-06',
          choices: [{ index: 0, finish_reason: 'stop', message: { role: 'assistant', content: '您的订单已发货' } }],
          usage: { prompt_tokens: 120, completion_tokens: 12, total_tokens: 132, prompt_tokens_details: { cached_tokens: 64 } },
        };
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify(payload));
        return;
      }
      if (req.url.endsWith('/responses')) {
        const payload = {
          id: 'resp_test',
          object: 'response',
          created_at: 1761620000,
          status: 'completed',
          model: 'gpt-4o-2024-08-06',
          output: [
            {
              type: 'message',
              id: 'msg_test',
              status: 'completed',
              role: 'assistant',
              content: [{ type: 'output_text', text: '您的订单已发货', annotations: [] }],
            },
          ],
          usage: { input_tokens: 50, output_tokens: 6, total_tokens: 56, input_tokens_details: { cached_tokens: 16 }, output_tokens_details: { reasoning_tokens: 0 } },
          parallel_tool_calls: true,
          tool_choice: 'auto',
          tools: [],
        };
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify(payload));
        return;
      }
      res.writeHead(404, { 'Content-Type': 'application/json' });
      res.end('{"error":{"message":"not found"}}');
    });
  });
  await new Promise((r) => srv.listen(0, '127.0.0.1', r));
  return { url: `http://127.0.0.1:${srv.address().port}/v1`, calls, close: () => new Promise((r) => srv.close(r)) };
}

/** Mock Anthropic /v1/messages endpoint. */
export async function startFakeAnthropic() {
  const calls = [];
  const srv = createServer((req, res) => {
    const chunks = [];
    req.on('data', (d) => chunks.push(d));
    req.on('end', () => {
      calls.push({ path: req.url, headers: req.headers });
      let body = {};
      try {
        body = JSON.parse(Buffer.concat(chunks).toString('utf8') || '{}');
      } catch {
        // Ignore parse errors.
      }
      // claude-cache-1 includes cache usage without changing the other fixtures.
      const usage = body.model === 'claude-cache-1'
        ? { input_tokens: 40, output_tokens: 7, cache_read_input_tokens: 100, cache_creation_input_tokens: 20 }
        : { input_tokens: 40, output_tokens: 7 };
      const model = body.model === 'claude-cache-1' ? 'claude-cache-1-20261101' : 'claude-test-1';
      if (body.stream) {
        const ev = (type, data) => `event: ${type}\ndata: ${JSON.stringify({ type, ...data })}\n\n`;
        res.writeHead(200, { 'Content-Type': 'text/event-stream', 'request-id': 'req_test' });
        res.end(
          ev('message_start', { message: { id: 'msg_stream', type: 'message', role: 'assistant', model, content: [], stop_reason: null, usage: { ...usage, output_tokens: 1 } } }) +
            ev('content_block_start', { index: 0, content_block: { type: 'text', text: '' } }) +
            ev('content_block_delta', { index: 0, delta: { type: 'text_delta', text: '您的订单已发货' } }) +
            ev('content_block_stop', { index: 0 }) +
            ev('message_delta', { delta: { stop_reason: 'end_turn', stop_sequence: null }, usage: { output_tokens: usage.output_tokens } }) +
            ev('message_stop', {}),
        );
        return;
      }
      const payload = {
        id: 'msg_test',
        type: 'message',
        role: 'assistant',
        model,
        content: [{ type: 'text', text: '您的订单已发货' }],
        stop_reason: 'end_turn',
        stop_sequence: null,
        usage,
      };
      res.writeHead(200, { 'Content-Type': 'application/json', 'request-id': 'req_test' });
      res.end(JSON.stringify(payload));
    });
  });
  await new Promise((r) => srv.listen(0, '127.0.0.1', r));
  return { url: `http://127.0.0.1:${srv.address().port}`, calls, close: () => new Promise((r) => srv.close(r)) };
}

// ---------------- Child processes ----------------

/**
 * Write and run a Node.js script in the test app directory. Return the JSON after the last RESULT: marker in stdout.
 * Remove inherited OTEL_*, FLASHCAT_*, and proxy variables, then apply the caller's environment.
 */
export async function runScript(app, code, { env = {}, ext = 'mjs', nodeArgs = [], timeoutMs = 60_000, expectExit = 0, signal } = {}) {
  const dir = appDir(app);
  const scripts = join(dir, 'scripts');
  mkdirSync(scripts, { recursive: true });
  const file = join(scripts, `t-${randomUUID()}.${ext}`);
  writeFileSync(file, code);
  const fullEnv = {};
  for (const [k, v] of Object.entries(process.env)) {
    if (k.startsWith('OTEL_') || k.startsWith('FLASHCAT_') || k.startsWith('ANTHROPIC_') || k.startsWith('OPENINFERENCE_') || k.startsWith('OPENAI_') || /proxy/i.test(k)) continue;
    if (k === 'NODE_OPTIONS' || k.startsWith('NODE_TEST')) continue;
    fullEnv[k] = v;
  }
  // Disable the Agents SDK's own trace export to keep requests local.
  fullEnv.OPENAI_AGENTS_DISABLE_TRACING = '1';
  Object.assign(fullEnv, env);
  const child = spawn(process.execPath, [...nodeArgs, file], { cwd: dir, env: fullEnv });
  let stdout = '';
  let stderr = '';
  child.stdout.on('data', (d) => (stdout += d));
  child.stderr.on('data', (d) => (stderr += d));
  if (signal) {
    // Wait for the script to print READY before sending the signal.
    const t = setInterval(() => {
      if (stdout.includes('READY')) {
        clearInterval(t);
        child.kill(signal);
      }
    }, 20);
    child.on('exit', () => clearInterval(t));
  }
  const timer = setTimeout(() => child.kill('SIGKILL'), timeoutMs);
  const [code_, sig] = await new Promise((r) => child.on('exit', (c, s) => r([c, s])));
  clearTimeout(timer);
  const ok = typeof expectExit === 'function' ? expectExit(code_, sig) : code_ === expectExit;
  if (!ok) {
    throw new Error(`script exit=${code_} signal=${sig}\nSTDOUT:\n${stdout}\nSTDERR:\n${stderr}`);
  }
  const last = stdout.split('\n').filter((l) => l.startsWith('RESULT:')).pop();
  const result = last ? JSON.parse(last.slice('RESULT:'.length)) : {};
  return { result, stdout, stderr, exitCode: code_, signal: sig };
}

export function basic(key) {
  return 'Basic ' + Buffer.from(key).toString('base64');
}

/** Wait for a matching span at the collector when automatic flush runs asynchronously. */
export async function waitFor(fn, timeoutMs = 5000) {
  const t0 = Date.now();
  for (;;) {
    const v = fn();
    if (v) return v;
    if (Date.now() - t0 > timeoutMs) return v;
    await new Promise((r) => setTimeout(r, 50));
  }
}
